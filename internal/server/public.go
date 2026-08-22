package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/SnowballSH/modelgate/internal/accounting"
	"github.com/SnowballSH/modelgate/internal/anthro"
	"github.com/SnowballSH/modelgate/internal/keys"
	"github.com/SnowballSH/modelgate/internal/models"
	"github.com/SnowballSH/modelgate/internal/oai"
	"github.com/SnowballSH/modelgate/internal/provider"
	"github.com/SnowballSH/modelgate/internal/store"
	"github.com/SnowballSH/modelgate/internal/translate"
)

type PublicHandler struct {
	guards           *Guards
	table            *models.Table
	acct             *accounting.Accountant
	store            *store.Store
	client           *provider.Client
	breaker          *provider.Breaker
	metrics          *Metrics
	defaultMaxTokens int
	maxBodyBytes     int64
	requestDeadline  time.Duration
	now              func() time.Time
}

func NewPublicHandler(g *Guards, table *models.Table, acct *accounting.Accountant, s *store.Store, client *provider.Client, breaker *provider.Breaker, m *Metrics, defaultMaxTokens int, maxBodyBytes int64, requestDeadline time.Duration, now func() time.Time) http.Handler {
	return &PublicHandler{
		guards:           g,
		table:            table,
		acct:             acct,
		store:            s,
		client:           client,
		breaker:          breaker,
		metrics:          m,
		defaultMaxTokens: defaultMaxTokens,
		maxBodyBytes:     maxBodyBytes,
		requestDeadline:  requestDeadline,
		now:              now,
	}
}

func (h *PublicHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		h.handleModels(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		h.handleChat(w, r)
	default:
		writeNotFound(w, "unknown route")
	}
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErrorStatus(w http.ResponseWriter, status int, errType, code, message string) {
	writeJSONStatus(w, status, oai.ErrorBody{Error: oai.ErrorDetail{
		Message: message,
		Type:    errType,
		Code:    code,
	}})
}

func writeNotFound(w http.ResponseWriter, message string) {
	writeErrorStatus(w, http.StatusNotFound, "invalid_request_error", "not_found", message)
}

func (h *PublicHandler) handleModels(w http.ResponseWriter, r *http.Request) {
	key, ok := h.authenticate(r)
	if !ok {
		WriteError(w, CodeInvalidAPIKey, "invalid API key")
		return
	}
	resp := oai.ModelsResponse{Object: "list", Data: []oai.ModelInfo{}}
	for _, id := range h.table.IDs() {
		if key.Models != nil && !slices.Contains(key.Models, id) {
			continue
		}
		resp.Data = append(resp.Data, oai.ModelInfo{ID: id, Object: "model", OwnedBy: "modelgate"})
	}
	writeJSONStatus(w, http.StatusOK, resp)
}

func (h *PublicHandler) authenticate(r *http.Request) (store.KeyRecord, bool) {
	id, secret, ok := keys.ParseBearer(r.Header.Get("Authorization"))
	if !ok {
		return store.KeyRecord{}, false
	}
	key, found, err := h.store.KeyByID(r.Context(), id)
	if err != nil || !found || len(key.SecretSHA256) != sha256.Size {
		return store.KeyRecord{}, false
	}
	if !keys.Verify(secret, [sha256.Size]byte(key.SecretSHA256)) {
		return store.KeyRecord{}, false
	}
	now := h.now()
	if key.RevokedAt != nil || (key.ExpiresAt != nil && now.After(*key.ExpiresAt)) {
		return store.KeyRecord{}, false
	}
	return key, true
}

func (h *PublicHandler) handleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	model := ""
	observe := func(outcome string) {
		h.metrics.ObserveRequest(outcome, model, time.Since(start).Seconds())
	}
	fail := func(code, message string) {
		observe(code)
		WriteError(w, code, message)
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, h.maxBodyBytes+1))
	if err != nil {
		fail(CodeInvalidRequest, "failed to read request body")
		return
	}
	var req oai.ChatRequest
	if int64(len(body)) <= h.maxBodyBytes {
		if err := json.Unmarshal(body, &req); err != nil {
			fail(CodeInvalidRequest, "invalid JSON body")
			return
		}
	}
	model = req.Model

	adm, code, ok := h.guards.Admit(r.Context(), r.Header.Get("Authorization"), int64(len(body)), req.Model)
	if !ok {
		fail(code, messageForCode(code))
		return
	}
	defer adm.Release()

	if !h.breaker.Allow() {
		h.metrics.SetBreakerOpen(true)
		fail(CodeProviderUnavailable, "provider circuit open")
		return
	}
	h.metrics.SetBreakerOpen(false)

	areq, err := translate.ToAnthropic(req, adm.Model.ProviderModel, h.defaultMaxTokens)
	if err != nil {
		fail(CodeInvalidRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.requestDeadline)
	defer cancel()

	if req.Stream {
		h.streamChat(ctx, w, r, req, areq, adm, observe)
		return
	}

	h.metrics.IncInFlight()
	aresp, err := h.client.Messages(ctx, areq)
	h.metrics.DecInFlight()
	h.breaker.Record(err)
	if err != nil {
		code := h.recordProviderError(err)
		fail(code, "upstream provider error")
		return
	}

	id := "chatcmpl-" + randomHex16()
	resp := translate.FromAnthropic(aresp, req.Model, h.now().Unix(), id)
	h.recordUsage(r.Context(), adm, req.Model, aresp.Usage)
	observe("success")
	writeJSONStatus(w, http.StatusOK, resp)
}

func (h *PublicHandler) streamChat(ctx context.Context, w http.ResponseWriter, r *http.Request, req oai.ChatRequest, areq anthro.MessagesRequest, adm Admission, observe func(string)) {
	id := "chatcmpl-" + randomHex16()
	st := translate.NewStreamTranslator(req.Model, h.now().Unix(), id)
	flusher, _ := w.(http.Flusher)
	wrote := false

	writeChunk := func(chunk oai.ChatChunk) error {
		if !wrote {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			wrote = true
		}
		data, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	h.metrics.IncInFlight()
	err := h.client.MessagesStream(ctx, areq, func(ev anthro.StreamEvent) error {
		chunks, err := st.Next(ev)
		if err != nil {
			return err
		}
		for _, chunk := range chunks {
			if err := writeChunk(chunk); err != nil {
				return err
			}
		}
		return nil
	})
	h.metrics.DecInFlight()
	h.breaker.Record(err)

	if err != nil {
		code := h.recordProviderError(err)
		observe(code)
		if !wrote {
			WriteError(w, code, "upstream provider error")
			return
		}
		payload, _ := json.Marshal(oai.ErrorBody{Error: oai.ErrorDetail{
			Message: "upstream provider error",
			Type:    errorTypeForStatus(StatusForCode(code)),
			Code:    code,
		}})
		fmt.Fprintf(w, "data: %s\n\n", payload)
		if flusher != nil {
			flusher.Flush()
		}
		return
	}

	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	h.recordUsage(r.Context(), adm, req.Model, st.Usage())
	observe("success")
}

func (h *PublicHandler) recordProviderError(err error) string {
	switch {
	case errors.Is(err, provider.ErrAuth):
		h.metrics.ProviderError("auth")
		return CodeProviderAuthError
	case errors.Is(err, provider.ErrRateLimited):
		h.metrics.ProviderError("rate_limited")
		return CodeRateLimited
	case errors.Is(err, provider.ErrTimeout):
		h.metrics.ProviderError("timeout")
		return CodeTimeout
	default:
		h.metrics.ProviderError("unavailable")
		return CodeProviderUnavailable
	}
}

func (h *PublicHandler) recordUsage(ctx context.Context, adm Admission, publicModel string, aUsage anthro.Usage) {
	usage := translate.ToStoreUsage(aUsage)
	now := h.now()
	if err := h.acct.Record(ctx, now, adm.Key.ID, publicModel, usage, adm.Model.Pricing); err == nil {
		if spend, err := h.store.MonthSpend(ctx, accounting.Month(now)); err == nil {
			h.metrics.SetMonthSpend(spend)
		}
	}
	_ = h.store.TouchLastUsed(ctx, adm.Key.ID, now)
	h.metrics.AddTokens(publicModel, usage)
}

func messageForCode(code string) string {
	switch code {
	case CodeInvalidAPIKey:
		return "invalid API key"
	case CodeModelNotFound:
		return "model not found or not allowed for this key"
	case CodeRateLimited:
		return "rate limit exceeded"
	case CodeQuotaExhausted:
		return "monthly quota exhausted for this key"
	case CodeBudgetExhausted:
		return "monthly budget exhausted"
	case CodeRequestTooLarge:
		return "request body too large"
	default:
		return "request rejected"
	}
}

func randomHex16() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(buf)
}
