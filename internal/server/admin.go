package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/SnowballSH/modelgate/internal/accounting"
	"github.com/SnowballSH/modelgate/internal/keys"
	"github.com/SnowballSH/modelgate/internal/models"
	"github.com/SnowballSH/modelgate/internal/store"
)

type AdminHandler struct {
	store          *store.Store
	acct           *accounting.Accountant
	table          *models.Table
	metrics        *Metrics
	identityHeader string
	budgetUSD      float64
	now            func() time.Time
	keyRand        io.Reader
	static         http.Handler
}

func NewAdminHandler(s *store.Store, acct *accounting.Accountant, table *models.Table, m *Metrics, identityHeader string, budgetUSD float64, now func() time.Time, keyRand io.Reader, static http.Handler) http.Handler {
	return &AdminHandler{
		store:          s,
		acct:           acct,
		table:          table,
		metrics:        m,
		identityHeader: identityHeader,
		budgetUSD:      budgetUSD,
		now:            now,
		keyRand:        keyRand,
		static:         static,
	}
}

type adminKeyJSON struct {
	ID            string   `json:"id"`
	Prefix        string   `json:"prefix"`
	Label         string   `json:"label"`
	Models        []string `json:"models"`
	QuotaUSD      *float64 `json:"quota_usd"`
	ExpiresAt     *string  `json:"expires_at"`
	RevokedAt     *string  `json:"revoked_at"`
	RevokedBy     *string  `json:"revoked_by"`
	LastUsedAt    *string  `json:"last_used_at"`
	CreatedAt     string   `json:"created_at"`
	CreatedBy     string   `json:"created_by"`
	MonthSpendUSD float64  `json:"month_spend_usd"`
}

func rfc3339Ptr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	v := t.UTC().Format(time.RFC3339)
	return &v
}

func (h *AdminHandler) keyJSON(ctx context.Context, k store.KeyRecord) adminKeyJSON {
	spend, _ := h.store.MonthSpendByKey(ctx, accounting.Month(h.now()), k.ID)
	return adminKeyJSON{
		ID:            k.ID,
		Prefix:        k.Prefix,
		Label:         k.Label,
		Models:        k.Models,
		QuotaUSD:      k.QuotaUSD,
		ExpiresAt:     rfc3339Ptr(k.ExpiresAt),
		RevokedAt:     rfc3339Ptr(k.RevokedAt),
		RevokedBy:     k.RevokedBy,
		LastUsedAt:    rfc3339Ptr(k.LastUsedAt),
		CreatedAt:     k.CreatedAt.UTC().Format(time.RFC3339),
		CreatedBy:     k.CreatedBy,
		MonthSpendUSD: spend,
	}
}

func (h *AdminHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	identity := r.Header.Get(h.identityHeader)
	if identity == "" {
		writeErrorStatus(w, http.StatusUnauthorized, "authentication_error", "missing_identity", "missing identity header")
		return
	}

	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && path == "/api/keys":
		h.listKeys(w, r)
	case r.Method == http.MethodPost && path == "/api/keys":
		h.createKey(w, r, identity)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/keys/") && strings.HasSuffix(path, "/revoke"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/keys/"), "/revoke")
		h.revokeKey(w, r, id, identity)
	case r.Method == http.MethodGet && path == "/api/usage":
		h.usage(w, r)
	case strings.HasPrefix(path, "/api/"):
		writeNotFound(w, "unknown route")
	case h.static != nil:
		h.static.ServeHTTP(w, r)
	default:
		writeNotFound(w, "unknown route")
	}
}

func (h *AdminHandler) listKeys(w http.ResponseWriter, r *http.Request) {
	records, err := h.store.ListKeys(r.Context())
	if err != nil {
		writeErrorStatus(w, http.StatusInternalServerError, "api_error", "api_error", "failed to list keys")
		return
	}
	out := make([]adminKeyJSON, len(records))
	for i, k := range records {
		out[i] = h.keyJSON(r.Context(), k)
	}
	writeJSONStatus(w, http.StatusOK, map[string]any{"keys": out})
}

type createKeyRequest struct {
	Label     string   `json:"label"`
	Models    []string `json:"models"`
	QuotaUSD  *float64 `json:"quota_usd"`
	ExpiresAt *string  `json:"expires_at"`
}

func (h *AdminHandler) createKey(w http.ResponseWriter, r *http.Request, identity string) {
	var req createKeyRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, "invalid_request_error", "invalid_request_error", "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.Label) == "" {
		writeErrorStatus(w, http.StatusBadRequest, "invalid_request_error", "invalid_request_error", "label is required")
		return
	}
	for _, id := range req.Models {
		if _, ok := h.table.Resolve(id); !ok {
			writeErrorStatus(w, http.StatusBadRequest, "invalid_request_error", "unknown_model", "unknown model: "+id)
			return
		}
	}
	if req.QuotaUSD != nil && *req.QuotaUSD <= 0 {
		writeErrorStatus(w, http.StatusBadRequest, "invalid_request_error", "invalid_request_error", "quota_usd must be positive")
		return
	}
	now := h.now()
	var expiresAt *time.Time
	if req.ExpiresAt != nil {
		t, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			writeErrorStatus(w, http.StatusBadRequest, "invalid_request_error", "invalid_request_error", "expires_at must be RFC3339")
			return
		}
		if !t.After(now) {
			writeErrorStatus(w, http.StatusBadRequest, "invalid_request_error", "invalid_request_error", "expires_at must be in the future")
			return
		}
		expiresAt = &t
	}

	gen, err := keys.Generate(h.keyRand)
	if err != nil {
		writeErrorStatus(w, http.StatusInternalServerError, "api_error", "api_error", "failed to generate key")
		return
	}
	record := store.KeyRecord{
		ID:           gen.ID,
		Prefix:       gen.Prefix,
		SecretSHA256: gen.SecretSHA256[:],
		Label:        req.Label,
		Models:       req.Models,
		QuotaUSD:     req.QuotaUSD,
		ExpiresAt:    expiresAt,
		CreatedAt:    now,
		CreatedBy:    identity,
	}
	if err := h.store.InsertKey(r.Context(), record); err != nil {
		writeErrorStatus(w, http.StatusInternalServerError, "api_error", "api_error", "failed to store key")
		return
	}
	h.refreshKeyCount(r.Context())
	slog.Info("key created", "key_id", gen.ID, "label", req.Label, "identity", identity)
	writeJSONStatus(w, http.StatusCreated, map[string]any{
		"key":      h.keyJSON(r.Context(), record),
		"full_key": gen.Full,
	})
}

func (h *AdminHandler) revokeKey(w http.ResponseWriter, r *http.Request, id, identity string) {
	_, found, err := h.store.KeyByID(r.Context(), id)
	if err != nil {
		writeErrorStatus(w, http.StatusInternalServerError, "api_error", "api_error", "failed to look up key")
		return
	}
	if !found {
		writeNotFound(w, "unknown key")
		return
	}
	if err := h.store.RevokeKey(r.Context(), id, h.now(), identity); err != nil {
		writeErrorStatus(w, http.StatusInternalServerError, "api_error", "api_error", "failed to revoke key")
		return
	}
	slog.Info("key revoked", "key_id", id, "identity", identity)
	key, _, err := h.store.KeyByID(r.Context(), id)
	if err != nil {
		writeErrorStatus(w, http.StatusInternalServerError, "api_error", "api_error", "failed to look up key")
		return
	}
	writeJSONStatus(w, http.StatusOK, map[string]any{"key": h.keyJSON(r.Context(), key)})
}

func (h *AdminHandler) usage(w http.ResponseWriter, r *http.Request) {
	now := h.now()
	month := accounting.Month(now)
	spend, err := h.store.MonthSpend(r.Context(), month)
	if err != nil {
		writeErrorStatus(w, http.StatusInternalServerError, "api_error", "api_error", "failed to read spend")
		return
	}
	records, err := h.store.ListKeys(r.Context())
	if err != nil {
		writeErrorStatus(w, http.StatusInternalServerError, "api_error", "api_error", "failed to list keys")
		return
	}
	type keyUsage struct {
		ID       string   `json:"id"`
		Label    string   `json:"label"`
		SpendUSD float64  `json:"spend_usd"`
		QuotaUSD *float64 `json:"quota_usd"`
	}
	usages := make([]keyUsage, len(records))
	for i, k := range records {
		keySpend, err := h.store.MonthSpendByKey(r.Context(), month, k.ID)
		if err != nil {
			writeErrorStatus(w, http.StatusInternalServerError, "api_error", "api_error", "failed to read key spend")
			return
		}
		usages[i] = keyUsage{ID: k.ID, Label: k.Label, SpendUSD: keySpend, QuotaUSD: k.QuotaUSD}
	}
	writeJSONStatus(w, http.StatusOK, map[string]any{
		"month":      month,
		"budget_usd": h.budgetUSD,
		"spend_usd":  spend,
		"keys":       usages,
	})
}

func (h *AdminHandler) refreshKeyCount(ctx context.Context) {
	if records, err := h.store.ListKeys(ctx); err == nil {
		h.metrics.SetKeyCount(float64(len(records)))
	}
}
