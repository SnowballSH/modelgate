package server

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SnowballSH/modelgate/internal/accounting"
	"github.com/SnowballSH/modelgate/internal/anthro"
	"github.com/SnowballSH/modelgate/internal/keys"
	"github.com/SnowballSH/modelgate/internal/models"
	"github.com/SnowballSH/modelgate/internal/oai"
	"github.com/SnowballSH/modelgate/internal/oairesp"
	"github.com/SnowballSH/modelgate/internal/provider"
	"github.com/SnowballSH/modelgate/internal/store"
)

const testModelJSON = `{"models":{"claude-sonnet-5":{
	"provider_model":"claude-sonnet-5-20260115",
	"input_usd_per_mtok":3,
	"output_usd_per_mtok":15,
	"cache_read_usd_per_mtok":0.30,
	"cache_write_usd_per_mtok":3.75}}}`

func testTable(t *testing.T) *models.Table {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(testModelJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	table, err := models.LoadTable(path)
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func insertTestKey(t *testing.T, s *store.Store, allowed []string) (string, keys.Generated) {
	t.Helper()
	gen, err := keys.Generate(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	err = s.InsertKey(t.Context(), store.KeyRecord{
		ID:           gen.ID,
		Prefix:       gen.Prefix,
		SecretSHA256: gen.SecretSHA256[:],
		Label:        "test",
		Models:       allowed,
		CreatedAt:    time.Now().UTC(),
		CreatedBy:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + gen.Full, gen
}

type publicEnv struct {
	handler http.Handler
	store   *store.Store
	acct    *accounting.Accountant
	metrics *Metrics
	now     time.Time
}

type publicEnvOptions struct {
	maxBodyBytes    int64
	budgetUSD       float64
	rpm             int
	bodyReadTimeout time.Duration
}

func newPublicEnv(t *testing.T, providerHandler http.HandlerFunc, maxBodyBytes int64) *publicEnv {
	t.Helper()
	return newPublicEnvWith(t, providerHandler, publicEnvOptions{maxBodyBytes: maxBodyBytes, budgetUSD: 100, rpm: 1000})
}

func newPublicEnvWith(t *testing.T, providerHandler http.HandlerFunc, opts publicEnvOptions) *publicEnv {
	t.Helper()
	upstream := httptest.NewServer(providerHandler)
	t.Cleanup(upstream.Close)

	s := testStore(t)
	table := testTable(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return now }
	acct := accounting.New(s, opts.budgetUSD)
	guards := NewGuards(s, acct, table, opts.rpm, 8, nowFn)
	client := provider.NewClient(upstream.URL, "test-upstream-key", upstream.Client())
	breaker := provider.NewBreaker(100, time.Minute, nowFn)
	m := NewMetrics(opts.budgetUSD)
	m.TrackMonthSpend(s, nowFn)
	m.TrackBreaker(models.ProviderAnthropic, breaker)
	handler := NewPublicHandler(guards, table, acct, s, Upstreams{Anthropic: client, AnthropicBreaker: breaker}, m, PublicConfig{DefaultMaxTokens: 4096, MaxOutputTokens: 128_000, MaxBodyBytes: opts.maxBodyBytes, BodyReadTimeout: opts.bodyReadTimeout, RequestDeadline: 5 * time.Second, Version: "test-build"}, nowFn)
	return &publicEnv{handler: handler, store: s, acct: acct, metrics: m, now: now}
}

func insertQuotaKey(t *testing.T, env *publicEnv, quotaUSD float64) string {
	t.Helper()
	gen, err := keys.Generate(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	err = env.store.InsertKey(t.Context(), store.KeyRecord{
		ID:           gen.ID,
		Prefix:       gen.Prefix,
		SecretSHA256: gen.SecretSHA256[:],
		Label:        "quota",
		QuotaUSD:     &quotaUSD,
		CreatedAt:    env.now,
		CreatedBy:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + gen.Full
}

func chatBody(stream bool) string {
	return fmt.Sprintf(`{"model":"claude-sonnet-5","stream":%t,"messages":[{"role":"user","content":"hi"}]}`, stream)
}

func doPublic(env *publicEnv, method, path, auth, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	return rec
}

func decodeErrorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body oai.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return body.Error.Code
}

func fullResponseHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(anthro.MessagesResponse{
			ID:         "msg_1",
			Type:       "message",
			Role:       "assistant",
			Model:      "claude-sonnet-5-20260115",
			Content:    []anthro.ContentBlock{{Type: "text", Text: "hello there"}},
			StopReason: "end_turn",
			Usage: anthro.Usage{
				InputTokens:              100,
				OutputTokens:             50,
				CacheReadInputTokens:     10,
				CacheCreationInputTokens: 5,
			},
		})
	}
}

func TestChatNonStreamSuccess(t *testing.T) {
	env := newPublicEnv(t, fullResponseHandler(), 1<<20)
	auth, gen := insertTestKey(t, env.store, nil)

	rec := doPublic(env, http.MethodPost, "/v1/chat/completions", auth, chatBody(false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp oai.ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %d", len(resp.Choices))
	}
	choice := resp.Choices[0]
	if choice.Message.Content == nil || *choice.Message.Content != "hello there" {
		t.Errorf("content = %v", choice.Message.Content)
	}
	if choice.FinishReason != "stop" {
		t.Errorf("finish_reason = %q", choice.FinishReason)
	}
	if resp.Usage.PromptTokens != 115 || resp.Usage.CompletionTokens != 50 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if resp.Model != "claude-sonnet-5" {
		t.Errorf("model = %q", resp.Model)
	}

	wantCost := 100*3/1e6 + 50*15/1e6 + 10*0.30/1e6 + 5*3.75/1e6
	spend, err := env.store.MonthSpend(t.Context(), accounting.Month(env.now))
	if err != nil {
		t.Fatal(err)
	}
	if spend <= 0 || math.Abs(spend-wantCost) > 1e-12 {
		t.Errorf("month spend = %v, want %v", spend, wantCost)
	}

	key, found, err := env.store.KeyByID(t.Context(), gen.ID)
	if err != nil || !found {
		t.Fatalf("key lookup: %v %v", found, err)
	}
	if key.LastUsedAt == nil {
		t.Error("LastUsedAt not touched")
	}
}

func TestChatStreamSuccess(t *testing.T) {
	events := []string{
		`{"type":"message_start","message":{"type":"message","role":"assistant","usage":{"input_tokens":100,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"type":"message_delta","stop_reason":"end_turn"},"usage":{"output_tokens":42}}`,
		`{"type":"message_stop"}`,
	}
	env := newPublicEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range events {
			fmt.Fprintf(w, "data: %s\n\n", ev)
		}
	}, 1<<20)
	auth, _ := insertTestKey(t, env.store, nil)

	rec := doPublic(env, http.MethodPost, "/v1/chat/completions", auth, chatBody(true))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q", ct)
	}

	var contents []string
	var finish string
	sawDone := false
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if payload == "[DONE]" {
			sawDone = true
			continue
		}
		if sawDone {
			t.Fatalf("data after [DONE]: %s", payload)
		}
		var chunk oai.ChatChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("bad chunk %q: %v", payload, err)
		}
		if len(chunk.Choices) != 1 {
			t.Fatalf("chunk choices = %d", len(chunk.Choices))
		}
		if c := chunk.Choices[0].Delta.Content; c != "" {
			contents = append(contents, c)
		}
		if fr := chunk.Choices[0].FinishReason; fr != nil {
			finish = *fr
		}
	}
	if strings.Join(contents, "") != "Hello world" {
		t.Errorf("streamed content = %q", strings.Join(contents, ""))
	}
	if finish != "stop" {
		t.Errorf("finish_reason = %q", finish)
	}
	if !sawDone {
		t.Error("missing [DONE]")
	}

	wantCost := 100*3/1e6 + 42*15/1e6
	spend, err := env.store.MonthSpend(t.Context(), accounting.Month(env.now))
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(spend-wantCost) > 1e-12 {
		t.Errorf("month spend = %v, want %v (recorded more or less than once?)", spend, wantCost)
	}
}

func TestChatGuardFailures(t *testing.T) {
	env := newPublicEnv(t, fullResponseHandler(), 1<<20)
	auth, _ := insertTestKey(t, env.store, nil)

	rec := doPublic(env, http.MethodPost, "/v1/chat/completions", "", chatBody(false))
	if rec.Code != http.StatusUnauthorized || decodeErrorCode(t, rec) != CodeInvalidAPIKey {
		t.Errorf("no auth: status %d code %s", rec.Code, decodeErrorCode(t, rec))
	}

	unknown := `{"model":"gpt-9","messages":[{"role":"user","content":"hi"}]}`
	rec = doPublic(env, http.MethodPost, "/v1/chat/completions", auth, unknown)
	if rec.Code != http.StatusNotFound || decodeErrorCode(t, rec) != CodeModelNotFound {
		t.Errorf("unknown model: status %d code %s", rec.Code, decodeErrorCode(t, rec))
	}

	small := newPublicEnv(t, fullResponseHandler(), 16)
	auth2, _ := insertTestKey(t, small.store, nil)
	rec = doPublic(small, http.MethodPost, "/v1/chat/completions", auth2, chatBody(false))
	if rec.Code != http.StatusRequestEntityTooLarge || decodeErrorCode(t, rec) != CodeRequestTooLarge {
		t.Errorf("oversize body: status %d code %s", rec.Code, decodeErrorCode(t, rec))
	}
}

func TestChatProviderErrors(t *testing.T) {
	env := newPublicEnv(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream exploded terribly", http.StatusInternalServerError)
	}, 1<<20)
	auth, _ := insertTestKey(t, env.store, nil)
	rec := doPublic(env, http.MethodPost, "/v1/chat/completions", auth, chatBody(false))
	if rec.Code != http.StatusServiceUnavailable || decodeErrorCode(t, rec) != CodeProviderUnavailable {
		t.Errorf("provider 500: status %d code %s body %s", rec.Code, decodeErrorCode(t, rec), rec.Body.String())
	}

	env401 := newPublicEnv(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "secret-upstream-auth-detail", http.StatusUnauthorized)
	}, 1<<20)
	auth401, _ := insertTestKey(t, env401.store, nil)
	rec = doPublic(env401, http.MethodPost, "/v1/chat/completions", auth401, chatBody(false))
	if rec.Code != http.StatusBadGateway || decodeErrorCode(t, rec) != CodeProviderAuthError {
		t.Errorf("provider 401: status %d code %s", rec.Code, decodeErrorCode(t, rec))
	}
	if strings.Contains(rec.Body.String(), "secret-upstream-auth-detail") {
		t.Error("upstream body leaked into our error")
	}
}

func TestModelsEndpoint(t *testing.T) {
	env := newPublicEnv(t, fullResponseHandler(), 1<<20)
	auth, _ := insertTestKey(t, env.store, []string{"claude-sonnet-5"})

	rec := doPublic(env, http.MethodGet, "/v1/models", auth, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	var resp oai.ModelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Object != "list" || len(resp.Data) != 1 || resp.Data[0].ID != "claude-sonnet-5" {
		t.Errorf("models = %+v", resp)
	}

	rec = doPublic(env, http.MethodGet, "/v1/models", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated models: status %d", rec.Code)
	}
}

func TestUnknownRoute(t *testing.T) {
	env := newPublicEnv(t, fullResponseHandler(), 1<<20)
	rec := doPublic(env, http.MethodGet, "/v1/embeddings", "", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown route: status %d", rec.Code)
	}
	var body oai.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unknown route body not JSON: %q", rec.Body.String())
	}
	if body.Error.Code != "not_found" {
		t.Errorf("unknown route code = %q", body.Error.Code)
	}
}

func TestStreamAbortStillRecordsUsage(t *testing.T) {
	upstream := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":100,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":50}}`,
		}
		for _, ev := range events {
			fmt.Fprintf(w, "data: %s\n\n", ev)
		}
	}
	env := newPublicEnv(t, upstream, 1<<20)
	auth, _ := insertTestKey(t, env.store, nil)

	rec := doPublic(env, http.MethodPost, "/v1/chat/completions", auth, chatBody(true))
	if rec.Code != http.StatusOK {
		t.Fatalf("stream status = %d (headers were sent before the upstream died)", rec.Code)
	}

	spend, err := env.store.MonthSpend(t.Context(), accounting.Month(env.now))
	if err != nil {
		t.Fatal(err)
	}
	want := 100*3.0/1e6 + 50*15.0/1e6
	if math.Abs(spend-want) > 1e-9 {
		t.Fatalf("aborted stream spend = %v, want %v — billed tokens must be booked on error paths", spend, want)
	}
}

func TestUnresolvedModelNeverBecomesMetricLabel(t *testing.T) {
	env := newPublicEnv(t, fullResponseHandler(), 1<<20)
	body := `{"model":"totally-made-up-model-zzz","messages":[{"role":"user","content":"hi"}]}`
	rec := doPublic(env, http.MethodPost, "/v1/chat/completions", "Bearer mg_aaaaaaaa_bogus", body)
	if code := decodeErrorCode(t, rec); code != CodeInvalidAPIKey {
		t.Fatalf("code = %s, want invalid_api_key", code)
	}

	mrec := httptest.NewRecorder()
	env.metrics.Handler().ServeHTTP(mrec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	exposition := mrec.Body.String()
	if strings.Contains(exposition, "totally-made-up-model-zzz") {
		t.Fatal("client-controlled model string reached a metric label")
	}
	if !strings.Contains(exposition, `model="unknown"`) {
		t.Fatal("expected the rejected request to be counted under model=\"unknown\"")
	}
}

func TestUpstreamRejectionIsInvalidRequestAndSparesBreaker(t *testing.T) {
	upstream := func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"type":"invalid_request_error"}}`, http.StatusBadRequest)
	}
	env := newPublicEnv(t, upstream, 1<<20)
	auth, _ := insertTestKey(t, env.store, nil)

	for i := range 150 {
		rec := doPublic(env, http.MethodPost, "/v1/chat/completions", auth, chatBody(false))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("request %d: status = %d, want 400 (a 503 here means upstream 4xx opened the breaker)", i, rec.Code)
		}
		if code := decodeErrorCode(t, rec); code != CodeInvalidRequest {
			t.Fatalf("request %d: code = %s, want invalid_request_error", i, code)
		}
	}
}

const dualModelJSON = `{"models":{
	"claude-sonnet-5":{"provider_model":"claude-sonnet-5-20260115","input_usd_per_mtok":3,"output_usd_per_mtok":15,"cache_read_usd_per_mtok":0.30,"cache_write_usd_per_mtok":3.75},
	"gpt-5":{"provider":"openai","provider_model":"gpt-5-2026-01-01","input_usd_per_mtok":1.25,"output_usd_per_mtok":10,"cache_read_usd_per_mtok":0.125,"cache_write_usd_per_mtok":1.25},
	"gpt-responses":{"provider":"openai","upstream_api":"responses","provider_model":"gpt-5.6-luna","input_usd_per_mtok":1.25,"output_usd_per_mtok":10,"cache_read_usd_per_mtok":0.125,"cache_write_usd_per_mtok":1.25}}}`

type upstreamCall struct {
	path string
	body []byte
}

func upstreamPaths(calls []upstreamCall) []string {
	paths := make([]string, len(calls))
	for i, call := range calls {
		paths[i] = call.path
	}
	return paths
}

type dualEnv struct {
	handler       http.Handler
	store         *store.Store
	now           time.Time
	openaiSeen    *[]oai.ChatRequest
	openaiCalls   *[]upstreamCall
	anthropicHits *int
}

func newDualEnv(t *testing.T, openaiHandler, anthropicHandler http.HandlerFunc, breakerThreshold int) *dualEnv {
	t.Helper()
	var openaiSeen []oai.ChatRequest
	var openaiCalls []upstreamCall
	var anthropicHits int

	openaiUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer oai-upstream-key" {
			t.Errorf("openai upstream Authorization = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		openaiCalls = append(openaiCalls, upstreamCall{path: r.URL.Path, body: body})
		var req oai.ChatRequest
		if r.URL.Path == "/v1/chat/completions" {
			if err := json.Unmarshal(body, &req); err == nil {
				openaiSeen = append(openaiSeen, req)
			}
		}
		openaiHandler(w, r)
	}))
	t.Cleanup(openaiUpstream.Close)
	anthropicUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthropicHits++
		anthropicHandler(w, r)
	}))
	t.Cleanup(anthropicUpstream.Close)

	s := testStore(t)
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(dualModelJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	table, err := models.LoadTable(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return now }
	acct := accounting.New(s, 100)
	guards := NewGuards(s, acct, table, 1000, 8, nowFn)
	up := Upstreams{
		Anthropic:        provider.NewClient(anthropicUpstream.URL, "ant-upstream-key", anthropicUpstream.Client()),
		AnthropicBreaker: provider.NewBreaker(breakerThreshold, time.Minute, nowFn),
		OpenAI:           provider.NewOpenAIClient(openaiUpstream.URL, "oai-upstream-key", openaiUpstream.Client()),
		OpenAIBreaker:    provider.NewBreaker(breakerThreshold, time.Minute, nowFn),
	}
	m := NewMetrics(100)
	handler := NewPublicHandler(guards, table, acct, s, up, m, PublicConfig{DefaultMaxTokens: 4096, MaxOutputTokens: 128_000, MaxBodyBytes: 1 << 20, RequestDeadline: 5 * time.Second, Version: "test-build"}, nowFn)
	return &dualEnv{handler: handler, store: s, now: now, openaiSeen: &openaiSeen, openaiCalls: &openaiCalls, anthropicHits: &anthropicHits}
}

func openaiChatResponse() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		content := "hi from openai"
		json.NewEncoder(w).Encode(oai.ChatResponse{
			ID: "chatcmpl-upstream", Object: "chat.completion", Model: "gpt-5-2026-01-01",
			Choices: []oai.Choice{{Message: oai.ResponseMessage{Role: "assistant", Content: &content}, FinishReason: "stop"}},
			Usage: oai.Usage{PromptTokens: 200, CompletionTokens: 40, TotalTokens: 240,
				PromptTokensDetails: &oai.PromptTokensDetails{CachedTokens: 80}},
		})
	}
}

func doDual(env *dualEnv, auth, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", auth)
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	return rec
}

func TestOpenAIRoutingAndUsage(t *testing.T) {
	env := newDualEnv(t, openaiChatResponse(), fullResponseHandler(), 100)
	auth, _ := insertTestKey(t, env.store, nil)

	rec := doDual(env, auth, `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp oai.ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Model != "gpt-5" {
		t.Errorf("response model = %q, want the public id gpt-5", resp.Model)
	}
	seen := *env.openaiSeen
	if len(seen) != 1 || seen[0].Model != "gpt-5-2026-01-01" {
		t.Fatalf("upstream saw %+v, want one request with the provider model", seen)
	}
	if *env.anthropicHits != 0 {
		t.Errorf("anthropic upstream hit %d times for an openai model", *env.anthropicHits)
	}

	spend, err := env.store.MonthSpend(t.Context(), accounting.Month(env.now))
	if err != nil {
		t.Fatal(err)
	}
	want := 120*1.25/1e6 + 40*10.0/1e6 + 80*0.125/1e6
	if math.Abs(spend-want) > 1e-9 {
		t.Fatalf("spend = %v, want %v (cached tokens priced at the cache-read rate)", spend, want)
	}
}

func openaiStreamHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","model":"gpt-5-2026-01-01","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","model":"gpt-5-2026-01-01","choices":[{"index":0,"delta":{"content":"hey"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","model":"gpt-5-2026-01-01","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"id":"c1","object":"chat.completion.chunk","model":"gpt-5-2026-01-01","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110}}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

func TestOpenAIStreamUsageChunkSuppressedUnlessRequested(t *testing.T) {
	for _, wantUsage := range []bool{false, true} {
		env := newDualEnv(t, openaiStreamHandler(), fullResponseHandler(), 100)
		auth, _ := insertTestKey(t, env.store, nil)
		body := `{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`
		if wantUsage {
			body = `{"model":"gpt-5","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`
		}
		rec := doDual(env, auth, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		got := rec.Body.String()
		if strings.Contains(got, `"prompt_tokens":100`) != wantUsage {
			t.Errorf("include_usage=%v: usage chunk presence = %v; body:\n%s", wantUsage, !wantUsage, got)
		}
		if !strings.Contains(got, "data: [DONE]") {
			t.Errorf("missing [DONE]")
		}
		if strings.Contains(got, "gpt-5-2026-01-01") {
			t.Errorf("provider model name leaked into the stream")
		}
		spend, err := env.store.MonthSpend(t.Context(), accounting.Month(env.now))
		if err != nil {
			t.Fatal(err)
		}
		if spend <= 0 {
			t.Errorf("include_usage=%v: streamed spend not recorded", wantUsage)
		}
	}
}

func TestProviderBreakersAreIndependent(t *testing.T) {
	failing := func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) }
	env := newDualEnv(t, failing, fullResponseHandler(), 1)
	auth, _ := insertTestKey(t, env.store, nil)

	rec := doDual(env, auth, `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`)
	if code := decodeErrorCode(t, rec); code != CodeProviderUnavailable {
		t.Fatalf("first openai failure code = %s", code)
	}
	rec = doDual(env, auth, `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("openai breaker did not open: status %d", rec.Code)
	}
	rec = doDual(env, auth, chatBody(false))
	if rec.Code != http.StatusOK {
		t.Fatalf("anthropic request blocked by the openai breaker: status %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestOpenAIStreamAbortBooksEstimatedUsage(t *testing.T) {
	truncated := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5-2026-01-01\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"twelve chars\"},\"finish_reason\":null}]}\n\n")
	}
	env := newDualEnv(t, truncated, fullResponseHandler(), 100)
	auth, _ := insertTestKey(t, env.store, nil)

	body := `{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	doDual(env, auth, body)

	spend, err := env.store.MonthSpend(t.Context(), accounting.Month(env.now))
	if err != nil {
		t.Fatal(err)
	}
	want := float64((len(body)+3)/4)*1.25/1e6 + 3*10.0/1e6
	if math.Abs(spend-want) > 1e-12 {
		t.Fatalf("aborted openai stream spend = %v, want estimate %v (prompt bytes / 4 input, 12 chars -> 3 output tokens)", spend, want)
	}
}

func TestOpenAIDefaultCompletionCapApplied(t *testing.T) {
	env := newDualEnv(t, openaiChatResponse(), fullResponseHandler(), 100)
	auth, _ := insertTestKey(t, env.store, nil)
	doDual(env, auth, `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`)
	seen := *env.openaiSeen
	if len(seen) != 1 || seen[0].MaxCompletionTokens == nil || *seen[0].MaxCompletionTokens != 4096 {
		t.Fatalf("upstream request = %+v; want max_completion_tokens defaulted to 4096", seen)
	}
}

func openaiResponsesHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(oairesp.Response{
			ID: "resp_1", Object: "response", Status: "completed", Model: "gpt-5.6-luna",
			Output: []oairesp.Item{
				{Type: "reasoning", ID: "rs_1"},
				{Type: "function_call", ID: "fc_1", CallID: "call_1", Name: "get_weather", Arguments: `{"city":"Paris"}`},
			},
			Usage: &oairesp.Usage{
				InputTokens:         100,
				InputTokensDetails:  &oairesp.InputTokenDetails{CachedTokens: 40},
				OutputTokens:        60,
				OutputTokensDetails: &oairesp.OutputTokenDetails{ReasoningTokens: 30},
				TotalTokens:         160,
			},
		})
	}
}

func TestResponsesRoutingAndUsage(t *testing.T) {
	env := newDualEnv(t, openaiResponsesHandler(), fullResponseHandler(), 100)
	auth, _ := insertTestKey(t, env.store, nil)

	rec := doDual(env, auth, `{"model":"gpt-responses","reasoning_effort":"xhigh","messages":[{"role":"user","content":"weather?"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp oai.ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Model != "gpt-responses" {
		t.Errorf("response model = %q, want the public id", resp.Model)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("choices = %+v, want one with finish_reason tool_calls", resp.Choices)
	}
	calls := resp.Choices[0].Message.ToolCalls
	if len(calls) != 1 || calls[0].ID != "call_1" || calls[0].Function.Name != "get_weather" {
		t.Errorf("tool calls = %+v", calls)
	}
	if resp.Usage.CompletionTokensDetails == nil || resp.Usage.CompletionTokensDetails.ReasoningTokens != 30 {
		t.Errorf("usage = %+v, want reasoning tokens reported", resp.Usage)
	}

	seen := *env.openaiCalls
	if len(seen) != 1 || seen[0].path != "/v1/responses" {
		t.Fatalf("upstream calls = %v, want one to /v1/responses", upstreamPaths(seen))
	}
	var upstream oairesp.Request
	if err := json.Unmarshal(seen[0].body, &upstream); err != nil {
		t.Fatal(err)
	}
	if upstream.Model != "gpt-5.6-luna" {
		t.Errorf("upstream model = %q", upstream.Model)
	}
	if upstream.Reasoning == nil || upstream.Reasoning.Effort != "xhigh" {
		t.Errorf("upstream reasoning = %+v, want effort xhigh", upstream.Reasoning)
	}
	if *env.anthropicHits != 0 {
		t.Errorf("anthropic upstream hit %d times for an openai model", *env.anthropicHits)
	}

	spend, err := env.store.MonthSpend(t.Context(), accounting.Month(env.now))
	if err != nil {
		t.Fatal(err)
	}
	want := 60*1.25/1e6 + 60*10.0/1e6 + 40*0.125/1e6
	if math.Abs(spend-want) > 1e-12 {
		t.Fatalf("spend = %v, want %v (reasoning billed as output, cached input at the cache-read rate)", spend, want)
	}
}

func TestResponsesTerminalFailureBooksUsageAndFails(t *testing.T) {
	failed := func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(oairesp.Response{
			ID: "resp_1", Object: "response", Status: "failed", Model: "gpt-5.6-luna",
			Error:  &oairesp.APIError{Code: "server_error", Message: "upstream detail nobody should see"},
			Output: []oairesp.Item{},
			Usage:  &oairesp.Usage{InputTokens: 100, OutputTokens: 40, TotalTokens: 140},
		})
	}
	env := newDualEnv(t, failed, fullResponseHandler(), 100)
	auth, _ := insertTestKey(t, env.store, nil)

	rec := doDual(env, auth, `{"model":"gpt-responses","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusServiceUnavailable || decodeErrorCode(t, rec) != CodeProviderUnavailable {
		t.Fatalf("terminal failure: status %d code %s (a failed response must not read as an empty answer)", rec.Code, decodeErrorCode(t, rec))
	}
	if strings.Contains(rec.Body.String(), "upstream detail nobody should see") {
		t.Error("upstream error detail leaked into our error")
	}

	spend, err := env.store.MonthSpend(t.Context(), accounting.Month(env.now))
	if err != nil {
		t.Fatal(err)
	}
	want := 100*1.25/1e6 + 40*10.0/1e6
	if math.Abs(spend-want) > 1e-12 {
		t.Fatalf("failed response spend = %v, want %v — the provider billed the run", spend, want)
	}
}

func openaiResponsesStreamHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}`,
			`{"type":"response.output_text.delta","output_index":0,"item_id":"msg_1","delta":"twelve chars"}`,
			`{"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.6-luna","output":[],"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":40},"output_tokens":60,"output_tokens_details":{"reasoning_tokens":30},"total_tokens":160}}}`,
		}
		for _, ev := range events {
			fmt.Fprintf(w, "data: %s\n\n", ev)
		}
	}
}

func TestResponsesStreamUsageChunkSuppressedUnlessRequested(t *testing.T) {
	for _, wantUsage := range []bool{false, true} {
		env := newDualEnv(t, openaiResponsesStreamHandler(), fullResponseHandler(), 100)
		auth, _ := insertTestKey(t, env.store, nil)
		body := `{"model":"gpt-responses","stream":true,"messages":[{"role":"user","content":"hi"}]}`
		if wantUsage {
			body = `{"model":"gpt-responses","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`
		}
		rec := doDual(env, auth, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("include_usage=%v: status = %d, body %s", wantUsage, rec.Code, rec.Body.String())
		}
		got := rec.Body.String()
		if strings.Contains(got, `"completion_tokens":60`) != wantUsage {
			t.Errorf("include_usage=%v: usage chunk presence = %v; body:\n%s", wantUsage, !wantUsage, got)
		}
		if !strings.Contains(got, "data: [DONE]") {
			t.Errorf("include_usage=%v: missing [DONE]", wantUsage)
		}
		if strings.Contains(got, "gpt-5.6-luna") {
			t.Errorf("include_usage=%v: provider model name leaked into the stream", wantUsage)
		}
		if seen := *env.openaiCalls; len(seen) != 1 || seen[0].path != "/v1/responses" {
			t.Fatalf("include_usage=%v: upstream calls = %v, want one to /v1/responses", wantUsage, upstreamPaths(seen))
		}

		spend, err := env.store.MonthSpend(t.Context(), accounting.Month(env.now))
		if err != nil {
			t.Fatal(err)
		}
		want := 60*1.25/1e6 + 60*10.0/1e6 + 40*0.125/1e6
		if math.Abs(spend-want) > 1e-12 {
			t.Fatalf("include_usage=%v: streamed spend = %v, want %v (booked once, from the terminal event)", wantUsage, spend, want)
		}
	}
}

func TestResponsesStreamAbortBooksTheOutputBound(t *testing.T) {
	truncated := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"twelve chars\"}\n\n")
	}
	for name, tc := range map[string]struct {
		body       string
		wantOutput int
	}{
		"gateway default cap": {`{"model":"gpt-responses","stream":true,"messages":[{"role":"user","content":"hi"}]}`, 4096},
		"client cap":          {`{"model":"gpt-responses","stream":true,"max_completion_tokens":300,"messages":[{"role":"user","content":"hi"}]}`, 300},
	} {
		t.Run(name, func(t *testing.T) {
			env := newDualEnv(t, truncated, fullResponseHandler(), 100)
			auth, _ := insertTestKey(t, env.store, nil)
			doDual(env, auth, tc.body)

			spend, err := env.store.MonthSpend(t.Context(), accounting.Month(env.now))
			if err != nil {
				t.Fatal(err)
			}
			want := float64((len(tc.body)+3)/4)*1.25/1e6 + float64(tc.wantOutput)*10.0/1e6
			if math.Abs(spend-want) > 1e-12 {
				t.Fatalf("aborted responses stream spend = %v, want %v (prompt bytes / 4 input, and the %d-token output cap: the reasoning billed before the cut never streams)", spend, want, tc.wantOutput)
			}
		})
	}
}

func TestStreamCutWithAReasoningEffortBooksTheOutputBound(t *testing.T) {
	truncated := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5-2026-01-01\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"twelve chars\"},\"finish_reason\":null}]}\n\n")
	}
	env := newDualEnv(t, truncated, fullResponseHandler(), 100)
	auth, _ := insertTestKey(t, env.store, nil)

	body := `{"model":"gpt-5","stream":true,"reasoning_effort":"high","max_completion_tokens":500,"messages":[{"role":"user","content":"hi"}]}`
	doDual(env, auth, body)

	spend, err := env.store.MonthSpend(t.Context(), accounting.Month(env.now))
	if err != nil {
		t.Fatal(err)
	}
	want := float64((len(body)+3)/4)*1.25/1e6 + 500*10.0/1e6
	if math.Abs(spend-want) > 1e-12 {
		t.Fatalf("spend after a reasoning stream was cut = %v, want %v (the 500-token output cap)", spend, want)
	}
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

func requestLogRecords(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for line := range strings.Lines(logs.String()) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if record["msg"] == "request" {
			records = append(records, record)
		}
	}
	return records
}

func TestRequestLogRecordsResolvedEffortAndUpstream(t *testing.T) {
	logs := captureLogs(t)
	env := newDualEnv(t, openaiResponsesHandler(), fullResponseHandler(), 100)
	auth, gen := insertTestKey(t, env.store, nil)

	rec := doDual(env, auth, `{"model":"gpt-responses","reasoning_effort":"xhigh","messages":[{"role":"user","content":"a prompt nobody should log"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	records := requestLogRecords(t, logs)
	if len(records) != 1 {
		t.Fatalf("request log records = %d, want 1: %s", len(records), logs.String())
	}
	want := map[string]any{
		"key_id":           gen.ID,
		"model":            "gpt-responses",
		"upstream":         models.UpstreamResponses,
		"reasoning_effort": "xhigh",
		"stream":           false,
		"status":           "success",
	}
	for field, value := range want {
		if got := records[0][field]; got != value {
			t.Errorf("log field %s = %v, want %v", field, got, value)
		}
	}

	rec = doDual(env, auth, `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat completions status = %d, body %s", rec.Code, rec.Body.String())
	}
	records = requestLogRecords(t, logs)
	if len(records) != 2 {
		t.Fatalf("request log records = %d, want 2: %s", len(records), logs.String())
	}
	if got := records[1]["upstream"]; got != models.UpstreamChatCompletions {
		t.Errorf("second record upstream = %v, want %v", got, models.UpstreamChatCompletions)
	}

	secret := strings.TrimPrefix(gen.Full, gen.Prefix+"_")
	for _, forbidden := range []string{secret, gen.Full, "a prompt nobody should log", "oai-upstream-key"} {
		if strings.Contains(logs.String(), forbidden) {
			t.Errorf("the request log leaks %q", forbidden)
		}
	}
}

func TestRequestLogRecordsOnlyKnownReasoningEfforts(t *testing.T) {
	logs := captureLogs(t)
	env := newDualEnv(t, openaiChatResponse(), fullResponseHandler(), 100)
	auth, _ := insertTestKey(t, env.store, nil)

	injected := "high\" evict=" + strings.Repeat("x", 256)
	body := func(model, effort string) string {
		return fmt.Sprintf(`{"model":%q,"reasoning_effort":%q,"messages":[{"role":"user","content":"hi"}]}`, model, effort)
	}

	if rec := doDual(env, auth, body("gpt-5", injected)); rec.Code != http.StatusOK {
		t.Fatalf("chat completions status = %d, body %s", rec.Code, rec.Body.String())
	}
	if rec := doDual(env, auth, body("gpt-responses", injected)); rec.Code != http.StatusBadRequest {
		t.Fatalf("responses status = %d, want 400, body %s", rec.Code, rec.Body.String())
	}
	if rec := doDual(env, auth, body("gpt-5", "XHigh")); rec.Code != http.StatusOK {
		t.Fatalf("canonicalisation status = %d, body %s", rec.Code, rec.Body.String())
	}

	records := requestLogRecords(t, logs)
	if len(records) != 3 {
		t.Fatalf("request log records = %d, want 3: %s", len(records), logs.String())
	}
	for i, want := range []string{"", "", "xhigh"} {
		got, _ := records[i]["reasoning_effort"].(string)
		if got != want {
			t.Errorf("record %d reasoning_effort = %.32q (%d bytes), want %q", i, got, len(got), want)
		}
	}
	if strings.Contains(logs.String(), injected) || strings.Contains(logs.String(), "evict=") {
		t.Error("the request log echoes the client's reasoning_effort")
	}
}
