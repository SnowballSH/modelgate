package server

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
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
	now     time.Time
}

func newPublicEnv(t *testing.T, providerHandler http.HandlerFunc, maxBodyBytes int64) *publicEnv {
	t.Helper()
	upstream := httptest.NewServer(providerHandler)
	t.Cleanup(upstream.Close)

	s := testStore(t)
	table := testTable(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return now }
	acct := accounting.New(s, 100)
	guards := NewGuards(s, acct, table, 1000, 8, maxBodyBytes, nowFn)
	client := provider.NewClient(upstream.URL, "test-upstream-key", upstream.Client())
	breaker := provider.NewBreaker(100, time.Minute, nowFn)
	m := NewMetrics(100)
	handler := NewPublicHandler(guards, table, acct, s, client, breaker, m, 4096, maxBodyBytes, 5*time.Second, nowFn)
	return &publicEnv{handler: handler, store: s, acct: acct, now: now}
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
