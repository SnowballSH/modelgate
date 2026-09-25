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
	"os"
	"slices"
	"time"

	"github.com/SnowballSH/modelgate/internal/accounting"
	"github.com/SnowballSH/modelgate/internal/anthro"
	"github.com/SnowballSH/modelgate/internal/models"
	"github.com/SnowballSH/modelgate/internal/oai"
	"github.com/SnowballSH/modelgate/internal/oairesp"
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
	maxOutputTokens  int
	maxBodyBytes     int64
	bodyReadTimeout  time.Duration
	requestDeadline  time.Duration
	version          string
	now              func() time.Time
}

type PublicConfig struct {
	DefaultMaxTokens int
	MaxOutputTokens  int
	MaxBodyBytes     int64
	BodyReadTimeout  time.Duration
	RequestDeadline  time.Duration
	Version          string
}

const (
	defaultBodyReadTimeout = 30 * time.Second
	bookingTimeout         = 10 * time.Second
)

func NewPublicHandler(g *Guards, table *models.Table, acct *accounting.Accountant, s *store.Store, up Upstreams, m *Metrics, cfg PublicConfig, now func() time.Time) http.Handler {
	if cfg.BodyReadTimeout <= 0 {
		cfg.BodyReadTimeout = defaultBodyReadTimeout
	}
	return &PublicHandler{
		guards:           g,
		table:            table,
		acct:             acct,
		store:            s,
		up:               up,
		metrics:          m,
		defaultMaxTokens: cfg.DefaultMaxTokens,
		maxOutputTokens:  cfg.MaxOutputTokens,
		maxBodyBytes:     cfg.MaxBodyBytes,
		bodyReadTimeout:  cfg.BodyReadTimeout,
		requestDeadline:  cfg.RequestDeadline,
		version:          cfg.Version,
		now:              now,
	}
}

func (h *PublicHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r = withRequestID(w, r)
	liftBodyDeadline := h.armBodyDeadline(w)
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		h.handleModels(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		h.handleChat(w, r, liftBodyDeadline)
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

// requestRecord is what the per-request log line reports: values resolved
// against the model table and the accepted reasoning_effort vocabulary,
// never a raw request string, the key, the prompt or any body.
type requestRecord struct {
	KeyID           string
	Model           string
	Upstream        string
	ReasoningEffort string
	Stream          bool
}

// callUpstream runs one provider call inside the in-flight gauge and the
// circuit breaker, reporting every failing outcome itself. It returns false
// when the caller must not go on to write a successful response.
func (h *PublicHandler) callUpstream(breaker *provider.Breaker, observe func(string), writeFailure func(code string), call func() error) bool {
	h.metrics.IncInFlight()
	err := call()
	h.metrics.DecInFlight()
	breaker.Record(err)
	switch {
	case err == nil:
		return true
	case errors.Is(err, provider.ErrClientAborted):
		observe("client_aborted")
	default:
		code := h.recordProviderError(err)
		observe(code)
		writeFailure(code)
	}
	return false
}

func writeProviderError(w http.ResponseWriter) func(code string) {
	return func(code string) {
		writeError(w, code, messageForProviderCode(code))
	}
}

func (h *PublicHandler) handleChat(w http.ResponseWriter, r *http.Request, liftBodyDeadline func()) {
	start := time.Now()
	record := requestRecord{Model: "unknown"}
	observe := func(outcome string) {
		elapsed := time.Since(start)
		h.metrics.ObserveRequest(outcome, record.Model, elapsed.Seconds())
		slog.Info("request", "request_id", requestIDFrom(r.Context()), "key_id", record.KeyID,
			"model", record.Model, "upstream", record.Upstream, "reasoning_effort", record.ReasoningEffort,
			"stream", record.Stream, "status", outcome, "duration_ms", elapsed.Milliseconds(),
			"version", h.version)
	}
	fail := func(code, message string) {
		observe(code)
		writeError(w, code, message)
	}

	key, ok, err := h.guards.Authenticate(r.Context(), r.Header.Get("Authorization"))
	switch {
	case err != nil:
		fail(CodeInternal, messageForCode(CodeInternal))
		return
	case !ok:
		fail(CodeInvalidAPIKey, messageForCode(CodeInvalidAPIKey))
		return
	}
	record.KeyID = key.ID

	body, code := h.readBody(w, r, liftBodyDeadline)
	if code != "" {
		fail(code, messageForBodyCode(code))
		return
	}
	var req oai.ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		fail(CodeInvalidRequest, "invalid JSON body")
		return
	}
	demand, err := worstCaseDemand(req, len(body), h.defaultMaxTokens, h.maxOutputTokens)
	if err != nil {
		fail(CodeInvalidRequest, err.Error())
		return
	}
	adm, refusal := h.guards.Admit(r.Context(), key, req.Model, demand)
	if refusal != nil {
		observe(refusal.Code)
		writeRefusal(w, *refusal, messageForCode(refusal.Code))
		return
	}
	defer adm.Release()
	effort, _ := translate.KnownEffort(req.ReasoningEffort)
	record = requestRecord{
		KeyID:           adm.Key.ID,
		Model:           req.Model,
		Upstream:        adm.Model.UpstreamAPI,
		ReasoningEffort: effort,
		Stream:          req.Stream,
	}
	if violation := limitViolation(req.Model, adm.Model.Limits, req, effort); violation != "" {
		fail(CodeInvalidRequest, violation)
		return
	}

	breaker := h.up.breakerFor(adm.Model.Provider)
	if !breaker.Allow() {
		fail(CodeProviderUnavailable, "provider circuit open")
		return
	}

	// A non-streaming call keeps running when the caller hangs up: the
	// provider bills it either way, and only its answer says how much.
	upstreamCtx := r.Context()
	if !req.Stream {
		upstreamCtx = context.WithoutCancel(upstreamCtx)
	}
	ctx, cancel := context.WithTimeout(upstreamCtx, h.requestDeadline)
	defer cancel()
	call := admittedCall{w: w, r: r, req: req, adm: adm, breaker: breaker, observe: observe, promptBytes: len(body), hidesOutput: mayHideOutput(adm.Model, effort)}

	if adm.Model.Provider == models.ProviderOpenAI {
		if adm.Model.UpstreamAPI == models.UpstreamResponses {
			h.chatResponses(ctx, call, fail)
			return
		}
		h.chatOpenAI(ctx, call)
		return
	}

	areq, err := translate.ToAnthropic(req, adm.Model.ProviderModel, h.defaultMaxTokens)
	if err != nil {
		fail(CodeInvalidRequest, err.Error())
		return
	}

	if req.Stream {
		h.streamChat(ctx, call, areq)
		return
	}

	var aresp anthro.MessagesResponse
	if !h.callUpstream(breaker, observe, writeProviderError(w), func() (err error) {
		aresp, err = h.up.Anthropic.Messages(ctx, areq)
		return err
	}) {
		return
	}

	h.recordUsage(r.Context(), adm, req.Model, translate.ToStoreUsage(aresp.Usage))
	if h.callerGone(r, observe) {
		return
	}
	id := "chatcmpl-" + randomHex16()
	resp := translate.FromAnthropic(aresp, req.Model, h.now().Unix(), id)
	observe("success")
	writeJSONStatus(w, http.StatusOK, resp)
}

// admittedCall carries one admitted request through a provider path.
type admittedCall struct {
	w           http.ResponseWriter
	r           *http.Request
	req         oai.ChatRequest
	adm         Admission
	breaker     *provider.Breaker
	observe     func(string)
	promptBytes int
	hidesOutput bool
}

// meter starts the tally for a stream whose upstream request caps output
// at outputCap tokens.
func (c admittedCall) meter(outputCap int) streamMeter {
	m := streamMeter{promptBytes: c.promptBytes}
	if c.hidesOutput {
		m.hiddenOutputCap = int64(outputCap)
	}
	return m
}

// armBodyDeadline bounds how long the request body may take to arrive,
// from before authentication: net/http drains an unread body before
// answering, so without a deadline a stalled sender holds even a refusal,
// and the connection, open indefinitely. It returns the function that lifts
// the deadline once the body has been read in full, so the deadline cannot
// cut short the response that follows.
func (h *PublicHandler) armBodyDeadline(w http.ResponseWriter) (lift func()) {
	rc := http.NewResponseController(w)
	if rc.SetReadDeadline(time.Now().Add(h.bodyReadTimeout)) != nil {
		return func() {}
	}
	return func() { _ = rc.SetReadDeadline(time.Time{}) }
}

// readBody reads the request body within the armed deadline and the size
// cap. Only a complete read lifts the deadline; after a failed one it stays
// to bound the drain.
func (h *PublicHandler) readBody(w http.ResponseWriter, r *http.Request, liftDeadline func()) ([]byte, string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBodyBytes))
	var tooLarge *http.MaxBytesError
	switch {
	case err == nil:
		liftDeadline()
		return body, ""
	case errors.As(err, &tooLarge):
		return nil, CodeRequestTooLarge
	case errors.Is(err, os.ErrDeadlineExceeded):
		return nil, CodeRequestTimeout
	default:
		return nil, CodeInvalidRequest
	}
}

func messageForBodyCode(code string) string {
	if code == CodeInvalidRequest {
		return "failed to read request body"
	}
	return messageForCode(code)
}

func (h *PublicHandler) callerGone(r *http.Request, observe func(string)) bool {
	if r.Context().Err() == nil {
		return false
	}
	observe("client_aborted")
	return true
}

// chatOpenAI forwards the request nearly verbatim: the public wire format
// is already OpenAI's, so only the model name, the usage capture, and the
// error surface need modelgate's treatment.
func (h *PublicHandler) chatOpenAI(ctx context.Context, call admittedCall) {
	w, r, req, adm := call.w, call.r, call.req, call.adm
	up := req
	up.Model = adm.Model.ProviderModel
	if up.MaxTokens == nil && up.MaxCompletionTokens == nil {
		cap := h.defaultMaxTokens
		up.MaxCompletionTokens = &cap
	}

	if req.Stream {
		h.streamOpenAI(ctx, call, up)
		return
	}

	var resp oai.ChatResponse
	if !h.callUpstream(call.breaker, call.observe, writeProviderError(w), func() (err error) {
		resp, err = h.up.OpenAI.Chat(ctx, up)
		return err
	}) {
		return
	}

	h.recordUsage(r.Context(), adm, req.Model, storeUsageFromOAI(resp.Usage))
	if h.callerGone(r, call.observe) {
		return
	}
	resp.Model = req.Model
	call.observe("success")
	writeJSONStatus(w, http.StatusOK, resp)
}

func (h *PublicHandler) streamOpenAI(ctx context.Context, call admittedCall, up oai.ChatRequest) {
	req := call.req
	sw := newSSEWriter(call.w)
	// Usage is always requested upstream so aborted streams can be billed;
	// the usage-only chunk reaches the client only when it asked for it.
	up.StreamOptions = &oai.StreamOptions{IncludeUsage: true}
	clientWantsUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage

	var usage oai.Usage
	meter := call.meter(openAIOutputCap(up))
	ok := h.callUpstream(call.breaker, call.observe, sw.fail, func() error {
		return h.up.OpenAI.ChatStream(ctx, up, func(chunk oai.ChatChunk) error {
			meter.started = true
			if chunk.Usage != nil {
				usage = *chunk.Usage
				meter.finalUsage = true
				if len(chunk.Choices) == 0 && !clientWantsUsage {
					return nil
				}
			}
			meter.count(chunk)
			chunk.Model = req.Model
			if err := sw.chunk(chunk); err != nil {
				return fmt.Errorf("%w: %v", provider.ErrClientAborted, err)
			}
			return nil
		})
	})
	defer func() {
		h.bookStreamUsage(call.r.Context(), call.adm, req.Model, meter, storeUsageFromOAI(usage))
	}()
	if !ok {
		return
	}
	sw.done()
	call.observe("success")
}

// openAIOutputCap is the most output a Chat Completions request can bill:
// its larger cap, once per choice.
func openAIOutputCap(req oai.ChatRequest) int {
	var perChoice int
	for _, limit := range []*int{req.MaxCompletionTokens, req.MaxTokens} {
		if limit != nil {
			perChoice = max(perChoice, *limit)
		}
	}
	if req.N != nil {
		return perChoice * *req.N
	}
	return perChoice
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

func (h *PublicHandler) streamChat(ctx context.Context, call admittedCall, areq anthro.MessagesRequest) {
	req := call.req
	id := "chatcmpl-" + randomHex16()
	st := translate.NewStreamTranslator(req.Model, h.now().Unix(), id)
	sw := newSSEWriter(call.w)

	meter := call.meter(areq.MaxTokens)
	ok := h.callUpstream(call.breaker, call.observe, sw.fail, func() error {
		return h.up.Anthropic.MessagesStream(ctx, areq, func(ev anthro.StreamEvent) error {
			meter.started = true
			if ev.Type == "message_delta" && ev.Usage != nil {
				meter.finalUsage = true
			}
			chunks, err := st.Next(ev)
			if err != nil {
				return fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
			}
			for _, chunk := range chunks {
				meter.count(chunk)
				if err := sw.chunk(chunk); err != nil {
					return fmt.Errorf("%w: %v", provider.ErrClientAborted, err)
				}
			}
			return nil
		})
	})
	defer func() {
		h.bookStreamUsage(call.r.Context(), call.adm, req.Model, meter, translate.ToStoreUsage(st.Usage()))
	}()
	if !ok {
		return
	}

	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		u := translate.OAIUsage(st.Usage())
		if !h.streamUsageChunk(sw, id, req.Model, u, call.observe) {
			return
		}
	}
	sw.done()
	call.observe("success")
}

// chatResponses serves a model the table routes through the Responses API,
// translating in both directions around the same guards the other upstreams
// run under.
func (h *PublicHandler) chatResponses(ctx context.Context, call admittedCall, fail func(code, message string)) {
	w, r, req, adm := call.w, call.r, call.req, call.adm
	rreq, err := translate.ToResponses(req, adm.Model.ProviderModel, h.defaultMaxTokens)
	if err != nil {
		fail(CodeInvalidRequest, err.Error())
		return
	}

	if req.Stream {
		h.streamResponses(ctx, call, rreq)
		return
	}

	var resp oairesp.Response
	if !h.callUpstream(call.breaker, call.observe, writeProviderError(w), func() (err error) {
		resp, err = h.up.OpenAI.Responses(ctx, rreq)
		return err
	}) {
		return
	}

	h.recordUsage(r.Context(), adm, req.Model, translate.ResponsesStoreUsage(resp.Usage))
	if h.callerGone(r, call.observe) {
		return
	}

	// A 200 reporting a failed run would otherwise translate to an empty but
	// successful answer, sending the agent back to retry work the provider
	// has already finished and billed.
	if resp.Status == "failed" || resp.Error != nil {
		fail(CodeProviderUnavailable, messageForProviderCode(CodeProviderUnavailable))
		return
	}

	id := "chatcmpl-" + randomHex16()
	call.observe("success")
	writeJSONStatus(w, http.StatusOK, translate.FromResponses(resp, req.Model, h.now().Unix(), id))
}

func (h *PublicHandler) streamResponses(ctx context.Context, call admittedCall, rreq oairesp.Request) {
	req := call.req
	id := "chatcmpl-" + randomHex16()
	st := translate.NewResponsesStreamTranslator(req.Model, h.now().Unix(), id)
	sw := newSSEWriter(call.w)

	outputCap := h.maxOutputTokens
	if rreq.MaxOutputTokens != nil {
		outputCap = *rreq.MaxOutputTokens
	}
	meter := call.meter(outputCap)
	ok := h.callUpstream(call.breaker, call.observe, sw.fail, func() error {
		return h.up.OpenAI.ResponsesStream(ctx, rreq, func(ev oairesp.StreamEvent) error {
			meter.started = true
			if isTerminalResponsesEvent(ev) && ev.Response != nil && ev.Response.Usage != nil {
				meter.finalUsage = true
			}
			chunks, err := st.Next(ev)
			if err != nil {
				return fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
			}
			for _, chunk := range chunks {
				meter.count(chunk)
				if err := sw.chunk(chunk); err != nil {
					return fmt.Errorf("%w: %v", provider.ErrClientAborted, err)
				}
			}
			return nil
		})
	})
	defer func() {
		h.bookStreamUsage(call.r.Context(), call.adm, req.Model, meter, translate.ResponsesStoreUsage(st.Usage()))
	}()
	if !ok {
		return
	}

	// The Responses stream carries no usage chunk of its own; the client that
	// asked for one gets it synthesized from the terminal event.
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		if !h.streamUsageChunk(sw, id, req.Model, translate.ResponsesOAIUsage(st.Usage()), call.observe) {
			return
		}
	}
	sw.done()
	call.observe("success")
}

func isTerminalResponsesEvent(ev oairesp.StreamEvent) bool {
	switch ev.Type {
	case "response.completed", "response.incomplete", "response.failed":
		return true
	}
	return false
}

func (h *PublicHandler) streamUsageChunk(sw *sseWriter, id, publicModel string, usage oai.Usage, observe func(string)) bool {
	err := sw.chunk(oai.ChatChunk{
		ID: id, Object: "chat.completion.chunk", Created: h.now().Unix(),
		Model: publicModel, Choices: []oai.ChunkChoice{},
		Usage: &usage,
	})
	if err != nil {
		observe("client_aborted")
		return false
	}
	return true
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
func (sw *sseWriter) fail(code string) {
	if !sw.wrote {
		writeError(sw.w, code, messageForProviderCode(code))
		return
	}
	payload, _ := json.Marshal(wireError(code, messageForProviderCode(code)))
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
	case CodeCapReserved:
		return "the remaining monthly quota or budget is held by requests in flight; retry shortly"
	case CodeRequestTooLarge:
		return "request body too large"
	case CodeRequestTimeout:
		return "request body not received in time"

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
