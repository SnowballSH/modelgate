package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/SnowballSH/modelgate/internal/accounting"
	"github.com/SnowballSH/modelgate/internal/anthro"
	"github.com/SnowballSH/modelgate/internal/models"
	"github.com/SnowballSH/modelgate/internal/oai"
	"github.com/SnowballSH/modelgate/internal/provider"
	"github.com/SnowballSH/modelgate/internal/store"
	"github.com/SnowballSH/modelgate/internal/translate"
)

// Upstreams holds one client and one circuit breaker per configured
// provider; a provider absent from the model table stays nil.
type Upstreams struct {
	Anthropic        *provider.Client
	AnthropicBreaker *provider.Breaker
	OpenAI           *provider.OpenAIClient
	OpenAIBreaker    *provider.Breaker
}

func (u Upstreams) breakerFor(providerName string) *provider.Breaker {
	if providerName == models.ProviderOpenAI {
		return u.OpenAIBreaker
	}
	return u.AnthropicBreaker
}

type PublicHandler struct {
	guards           *Guards
	table            *models.Table
	acct             *accounting.Accountant
	store            *store.Store
	up               Upstreams
	metrics          *Metrics
	defaultMaxTokens int
	maxBodyBytes     int64
	requestDeadline  time.Duration
	now              func() time.Time
}

type PublicConfig struct {
	DefaultMaxTokens int
	MaxBodyBytes     int64
	RequestDeadline  time.Duration
}

func NewPublicHandler(g *Guards, table *models.Table, acct *accounting.Accountant, s *store.Store, up Upstreams, m *Metrics, cfg PublicConfig, now func() time.Time) http.Handler {
	return &PublicHandler{
		guards:           g,
		table:            table,
		acct:             acct,
		store:            s,
		up:               up,
		metrics:          m,
		defaultMaxTokens: cfg.DefaultMaxTokens,
		maxBodyBytes:     cfg.MaxBodyBytes,
		requestDeadline:  cfg.RequestDeadline,
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

func (h *PublicHandler) handleModels(w http.ResponseWriter, r *http.Request) {
	key, ok, err := h.guards.Authenticate(r.Context(), r.Header.Get("Authorization"))
	if err != nil || !ok {
		writeError(w, CodeInvalidAPIKey, "invalid API key")
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

func (h *PublicHandler) handleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	model := "unknown"
	observe := func(outcome string) {
		h.metrics.ObserveRequest(outcome, model, time.Since(start).Seconds())
	}
	fail := func(code, message string) {
		observe(code)
		writeError(w, code, message)
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
	adm, code, ok := h.guards.Admit(r.Context(), r.Header.Get("Authorization"), int64(len(body)), req.Model)
	if !ok {
		fail(code, messageForCode(code))
		return
	}
	defer adm.Release()
	model = req.Model

	breaker := h.up.breakerFor(adm.Model.Provider)
	if !breaker.Allow() {
		h.metrics.SetBreakerOpen(adm.Model.Provider, true)
		fail(CodeProviderUnavailable, "provider circuit open")
		return
	}
	h.metrics.SetBreakerOpen(adm.Model.Provider, false)

	ctx, cancel := context.WithTimeout(r.Context(), h.requestDeadline)
	defer cancel()

	if adm.Model.Provider == models.ProviderOpenAI {
		h.chatOpenAI(ctx, w, r, req, adm, breaker, observe, fail)
		return
	}

	areq, err := translate.ToAnthropic(req, adm.Model.ProviderModel, h.defaultMaxTokens)
	if err != nil {
		fail(CodeInvalidRequest, err.Error())
		return
	}

	if req.Stream {
		h.streamChat(ctx, w, r, req, areq, adm, breaker, observe)
		return
	}

	h.metrics.IncInFlight()
	aresp, err := h.up.Anthropic.Messages(ctx, areq)
	h.metrics.DecInFlight()
	breaker.Record(err)
	if errors.Is(err, provider.ErrClientAborted) {
		observe("client_aborted")
		return
	}
	if err != nil {
		code := h.recordProviderError(err)
		fail(code, messageForProviderCode(code))
		return
	}

	id := "chatcmpl-" + randomHex16()
	resp := translate.FromAnthropic(aresp, req.Model, h.now().Unix(), id)
	h.recordUsage(r.Context(), adm, req.Model, translate.ToStoreUsage(aresp.Usage))
	observe("success")
	writeJSONStatus(w, http.StatusOK, resp)
}

// chatOpenAI forwards the request nearly verbatim: the public wire format
// is already OpenAI's, so only the model name, the usage capture, and the
// error surface need modelgate's treatment.
func (h *PublicHandler) chatOpenAI(ctx context.Context, w http.ResponseWriter, r *http.Request, req oai.ChatRequest, adm Admission, breaker *provider.Breaker, observe func(string), fail func(code, message string)) {
	up := req
	up.Model = adm.Model.ProviderModel
	if up.MaxTokens == nil && up.MaxCompletionTokens == nil {
		cap := h.defaultMaxTokens
		up.MaxCompletionTokens = &cap
	}

	if req.Stream {
		h.streamOpenAI(ctx, w, r, req, up, adm, breaker, observe)
		return
	}

	h.metrics.IncInFlight()
	resp, err := h.up.OpenAI.Chat(ctx, up)
	h.metrics.DecInFlight()
	breaker.Record(err)
	if errors.Is(err, provider.ErrClientAborted) {
		observe("client_aborted")
		return
	}
	if err != nil {
		code := h.recordProviderError(err)
		fail(code, messageForProviderCode(code))
		return
	}

	resp.Model = req.Model
	h.recordUsage(r.Context(), adm, req.Model, storeUsageFromOAI(resp.Usage))
	observe("success")
	writeJSONStatus(w, http.StatusOK, resp)
}

func (h *PublicHandler) streamOpenAI(ctx context.Context, w http.ResponseWriter, r *http.Request, req, up oai.ChatRequest, adm Admission, breaker *provider.Breaker, observe func(string)) {
	sw := newSSEWriter(w)
	// Usage is always requested upstream so aborted streams can be billed;
	// the usage-only chunk reaches the client only when it asked for it.
	up.StreamOptions = &oai.StreamOptions{IncludeUsage: true}
	clientWantsUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage

	var usage oai.Usage
	var contentChars int
	h.metrics.IncInFlight()
	err := h.up.OpenAI.ChatStream(ctx, up, func(chunk oai.ChatChunk) error {
		if chunk.Usage != nil {
			usage = *chunk.Usage
			if len(chunk.Choices) == 0 && !clientWantsUsage {
				return nil
			}
		}
		for _, c := range chunk.Choices {
			contentChars += len(c.Delta.Content)
		}
		chunk.Model = req.Model
		if err := sw.chunk(chunk); err != nil {
			return fmt.Errorf("%w: %v", provider.ErrClientAborted, err)
		}
		return nil
	})
	h.metrics.DecInFlight()
	breaker.Record(err)

	// OpenAI reports usage only in the stream's final chunk, so an aborted
	// stream would otherwise bill nothing while the provider bills
	// everything generated. Booking a character-count estimate keeps the
	// quota and budget honest; the log marks it as an estimate.
	defer func() {
		u := storeUsageFromOAI(usage)
		if !u.HasTokens() && contentChars > 0 {
			u = store.Usage{OutputTokens: int64((contentChars + 3) / 4)}
			slog.Warn("stream ended before usage arrived; booking estimated output tokens",
				"key_id", adm.Key.ID, "model", req.Model, "estimated_output_tokens", u.OutputTokens)
		}
		if u.HasTokens() {
			h.recordUsage(r.Context(), adm, req.Model, u)
		}
	}()

	if errors.Is(err, provider.ErrClientAborted) {
		observe("client_aborted")
		return
	}
	if err != nil {
		code := h.recordProviderError(err)
		observe(code)
		sw.fail(w, code)
		return
	}
	sw.done()
	observe("success")
}

// storeUsageFromOAI clamps the cached count into [0, PromptTokens]: a
// nonconforming upstream must never produce negative input tokens, which
// would corrupt spend accounting and panic the token counters.
func storeUsageFromOAI(u oai.Usage) store.Usage {
	var cached int64
	if u.PromptTokensDetails != nil {
		cached = min(max(u.PromptTokensDetails.CachedTokens, 0), u.PromptTokens)
	}
	return store.Usage{
		InputTokens:     u.PromptTokens - cached,
		OutputTokens:    u.CompletionTokens,
		CacheReadTokens: cached,
	}
}

func (h *PublicHandler) streamChat(ctx context.Context, w http.ResponseWriter, r *http.Request, req oai.ChatRequest, areq anthro.MessagesRequest, adm Admission, breaker *provider.Breaker, observe func(string)) {
	id := "chatcmpl-" + randomHex16()
	st := translate.NewStreamTranslator(req.Model, h.now().Unix(), id)
	sw := newSSEWriter(w)

	h.metrics.IncInFlight()
	err := h.up.Anthropic.MessagesStream(ctx, areq, func(ev anthro.StreamEvent) error {
		chunks, err := st.Next(ev)
		if err != nil {
			return fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
		}
		for _, chunk := range chunks {
			if err := sw.chunk(chunk); err != nil {
				return fmt.Errorf("%w: %v", provider.ErrClientAborted, err)
			}
		}
		return nil
	})
	h.metrics.DecInFlight()
	breaker.Record(err)

	// The provider bills every token the translator saw, aborted stream or
	// not, so spend is booked on every exit path.
	defer func() {
		u := translate.ToStoreUsage(st.Usage())
		if u.HasTokens() {
			h.recordUsage(r.Context(), adm, req.Model, u)
		}
	}()

	if errors.Is(err, provider.ErrClientAborted) {
		observe("client_aborted")
		return
	}
	if err != nil {
		code := h.recordProviderError(err)
		observe(code)
		sw.fail(w, code)
		return
	}

	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		u := translate.OAIUsage(st.Usage())
		if err := sw.chunk(oai.ChatChunk{
			ID: id, Object: "chat.completion.chunk", Created: h.now().Unix(),
			Model: req.Model, Choices: []oai.ChunkChoice{},
			Usage: &u,
		}); err != nil {
			observe("client_aborted")
			return
		}
	}
	sw.done()
	observe("success")
}

// sseWriter frames chat chunks as server-sent events, deferring the
// response headers until the first write so a pre-stream failure can
// still answer with a plain JSON error.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	wrote   bool
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	flusher, _ := w.(http.Flusher)
	return &sseWriter{w: w, flusher: flusher}
}

func (sw *sseWriter) raw(payload []byte) error {
	if !sw.wrote {
		sw.w.Header().Set("Content-Type", "text/event-stream")
		sw.w.Header().Set("Cache-Control", "no-cache")
		sw.w.WriteHeader(http.StatusOK)
		sw.wrote = true
	}
	if _, err := fmt.Fprintf(sw.w, "data: %s\n\n", payload); err != nil {
		return err
	}
	if sw.flusher != nil {
		sw.flusher.Flush()
	}
	return nil
}

func (sw *sseWriter) chunk(chunk oai.ChatChunk) error {
	data, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	return sw.raw(data)
}

// fail answers a mid-stream error inside the SSE body, or as a plain JSON
// error when nothing has streamed yet.
func (sw *sseWriter) fail(w http.ResponseWriter, code string) {
	if !sw.wrote {
		writeError(w, code, messageForProviderCode(code))
		return
	}
	payload, _ := json.Marshal(errorBody(errorTypeForStatus(statusForCode(code)), code, messageForProviderCode(code)))
	_ = sw.raw(payload)
}

func (sw *sseWriter) done() {
	_ = sw.raw([]byte("[DONE]"))
}

func messageForProviderCode(code string) string {
	if code == CodeInvalidRequest {
		return "the provider rejected the translated request"
	}
	return "upstream provider error"
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
	case errors.Is(err, provider.ErrInvalidRequest):
		h.metrics.ProviderError("rejected")
		return CodeInvalidRequest
	default:
		h.metrics.ProviderError("unavailable")
		return CodeProviderUnavailable
	}
}

// recordUsage runs on a context detached from the request: spend the
// provider billed must be booked even when the caller is already gone.
func (h *PublicHandler) recordUsage(ctx context.Context, adm Admission, publicModel string, usage store.Usage) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	now := h.now()
	if err := h.acct.Record(ctx, now, adm.Key.ID, publicModel, usage, adm.Model.Pricing); err != nil {
		slog.Error("usage record failed", "key_id", adm.Key.ID, "model", publicModel, "err", err)
	} else if spend, err := h.store.MonthSpend(ctx, accounting.Month(now)); err == nil {
		h.metrics.SetMonthSpend(spend)
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
