//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SnowballSH/modelgate/internal/anthro"
	"github.com/SnowballSH/modelgate/internal/config"
	"github.com/SnowballSH/modelgate/internal/oai"
	"github.com/SnowballSH/modelgate/internal/oairesp"
	"github.com/SnowballSH/modelgate/internal/server"
)

const testModel = "claude-sonnet-5"

var defaultTable = fmt.Sprintf(`{"models":{%q:{"provider_model":%q,
	"input_usd_per_mtok":3,"output_usd_per_mtok":15,
	"cache_read_usd_per_mtok":0.3,"cache_write_usd_per_mtok":3.75}}}`, testModel, testModel)

const farmTable = `{"models":{
	"gpt-flag":{"provider":"openai","upstream_api":"responses","provider_model":"gpt-5.6-luna",
		"input_usd_per_mtok":1.25,"output_usd_per_mtok":10,
		"cache_read_usd_per_mtok":0.125,"cache_write_usd_per_mtok":1.25},
	"gpt-plain":{"provider":"openai","provider_model":"gpt-5.6-terra",
		"input_usd_per_mtok":1.25,"output_usd_per_mtok":10,
		"cache_read_usd_per_mtok":0.125,"cache_write_usd_per_mtok":1.25}}}`

func fakeAnthropic(t *testing.T) (*httptest.Server, *upstreamRecorder) {
	t.Helper()
	rec := &upstreamRecorder{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rec.record(r.URL.Path, body)
		var req anthro.MessagesRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch {
		case req.Stream && awaitsToolCall(req):
			serveFakeNoArgumentToolStream(w, req.Model)
		case req.Stream:
			serveFakeStream(w, req.Model)
		default:
			serveFakeMessage(w, req.Model)
		}
	}))
	t.Cleanup(upstream.Close)
	return upstream, rec
}

// awaitsToolCall reports a request offering tools whose last turn is not a
// tool result: the fake answers it with a call, and a tool result with text.
func awaitsToolCall(req anthro.MessagesRequest) bool {
	if len(req.Tools) == 0 || len(req.Messages) == 0 {
		return false
	}
	last := req.Messages[len(req.Messages)-1].Content
	return len(last) == 0 || last[0].Type != "tool_result"
}

// serveFakeNoArgumentToolStream replays the shape Anthropic streams for a call
// to a tool that takes no arguments: the block opens with an empty input and
// closes after one empty input_json_delta.
func serveFakeNoArgumentToolStream(w http.ResponseWriter, model string) {
	writeFakeEvents(w, []anthro.StreamEvent{
		{Type: "message_start", Message: &anthro.MessagesResponse{
			ID: "msg_tool", Type: "message", Role: "assistant", Model: model,
			Usage: anthro.Usage{InputTokens: 100, CacheCreationInputTokens: 900},
		}},
		{Type: "content_block_start", Index: 0, ContentBlock: &anthro.ContentBlock{Type: "tool_use", ID: "toolu_now", Name: "get_time", Input: json.RawMessage(`{}`)}},
		{Type: "content_block_delta", Index: 0, Delta: &anthro.StreamDelta{Type: "input_json_delta", PartialJSON: ""}},
		{Type: "content_block_stop", Index: 0},
		{Type: "message_delta", Delta: &anthro.StreamDelta{StopReason: "tool_use"}, Usage: &anthro.Usage{OutputTokens: 12}},
		{Type: "message_stop"},
	})
}

func serveFakeMessage(w http.ResponseWriter, model string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(anthro.MessagesResponse{
		ID:         "msg_fake",
		Type:       "message",
		Role:       "assistant",
		Model:      model,
		Content:    []anthro.ContentBlock{{Type: "text", Text: "hello from fake"}},
		StopReason: "end_turn",
		Usage:      anthro.Usage{InputTokens: 100, OutputTokens: 50},
	})
}

func serveFakeStream(w http.ResponseWriter, model string) {
	events := []anthro.StreamEvent{
		{Type: "message_start", Message: &anthro.MessagesResponse{
			ID:    "msg_fake",
			Type:  "message",
			Role:  "assistant",
			Model: model,
			Usage: anthro.Usage{InputTokens: 100},
		}},
		{Type: "content_block_start", Index: 0, ContentBlock: &anthro.ContentBlock{Type: "text"}},
		{Type: "content_block_delta", Index: 0, Delta: &anthro.StreamDelta{Type: "text_delta", Text: "hel"}},
		{Type: "content_block_delta", Index: 0, Delta: &anthro.StreamDelta{Type: "text_delta", Text: "lo"}},
		{Type: "content_block_stop", Index: 0},
		{Type: "message_delta",
			Delta: &anthro.StreamDelta{StopReason: "end_turn"},
			Usage: &anthro.Usage{OutputTokens: 50}},
		{Type: "message_stop"},
	}
	writeFakeEvents(w, events)
}

func writeFakeEvents(w http.ResponseWriter, events []anthro.StreamEvent) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	for _, ev := range events {
		payload, _ := json.Marshal(ev)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, payload)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// upstreamRecorder keeps every request a fake upstream served, so a test can
// assert which API the gateway spoke and what it sent.
type upstreamRecorder struct {
	mu       sync.Mutex
	requests []upstreamRequest
}

type upstreamRequest struct {
	Path string
	Body []byte
}

func (rec *upstreamRecorder) record(path string, body []byte) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.requests = append(rec.requests, upstreamRequest{Path: path, Body: body})
}

func (rec *upstreamRecorder) seen() []upstreamRequest {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return slices.Clone(rec.requests)
}

func fakeOpenAI(t *testing.T) (*httptest.Server, *upstreamRecorder) {
	t.Helper()
	rec := &upstreamRecorder{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rec.record(r.URL.Path, body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		if err := json.Unmarshal(body, &probe); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch {
		case r.URL.Path == "/v1/chat/completions":
			serveFakeOpenAIChat(w)
		case r.URL.Path == "/v1/responses" && probe.Stream:
			serveFakeResponsesStream(w)
		case r.URL.Path == "/v1/responses":
			serveFakeResponse(w)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	return upstream, rec
}

func serveFakeOpenAIChat(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	content := "hello from fake chat completions"
	json.NewEncoder(w).Encode(oai.ChatResponse{
		ID:      "chatcmpl-fake",
		Object:  "chat.completion",
		Model:   "gpt-5.6-terra",
		Choices: []oai.Choice{{Message: oai.ResponseMessage{Role: "assistant", Content: &content}, FinishReason: "stop"}},
		Usage:   oai.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
	})
}

func serveFakeResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(oairesp.Response{
		ID:     "resp_fake",
		Object: "response",
		Status: "completed",
		Model:  "gpt-5.6-luna",
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

// serveFakeResponsesStream replays the tool-call event sequence the translator
// unit tests are built on, with usage on the terminal event.
func serveFakeResponsesStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	events := []string{
		`{"type":"response.created","response":{"id":"resp_fake","object":"response","status":"in_progress","model":"gpt-5.6-luna","output":[]}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}`,
		`{"type":"response.output_text.delta","output_index":1,"item_id":"msg_1","delta":"Let me "}`,
		`{"type":"response.output_text.delta","output_index":1,"item_id":"msg_1","delta":"check."}`,
		`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":2,"item_id":"fc_1","delta":"{\"city\":"}`,
		`{"type":"response.function_call_arguments.delta","output_index":2,"item_id":"fc_1","delta":"\"Paris\"}"}`,
		`{"type":"response.function_call_arguments.done","output_index":2,"item_id":"fc_1","arguments":"{\"city\":\"Paris\"}"}`,
		`{"type":"response.completed","response":{"id":"resp_fake","object":"response","status":"completed","model":"gpt-5.6-luna","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Paris\"}"}],"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":40},"output_tokens":60,"output_tokens_details":{"reasoning_tokens":30},"total_tokens":160}}}`,
	}
	flusher, _ := w.(http.Flusher)
	for _, ev := range events {
		fmt.Fprintf(w, "data: %s\n\n", ev)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func tableUsesOpenAI(t *testing.T, table string) bool {
	t.Helper()
	var parsed struct {
		Models map[string]struct {
			Provider string `json:"provider"`
		} `json:"models"`
	}
	if err := json.Unmarshal([]byte(table), &parsed); err != nil {
		t.Fatalf("model table is not valid JSON: %v", err)
	}
	for _, model := range parsed.Models {
		if model.Provider == "openai" {
			return true
		}
	}
	return false
}

type gateway struct {
	PublicAddr  string
	AdminAddr   string
	MetricsAddr string
	OpenAI      *upstreamRecorder
	Anthropic   *upstreamRecorder
}

func startGateway(t *testing.T, budgetUSD float64, table string) gateway {
	t.Helper()
	upstream, anthropicSeen := fakeAnthropic(t)
	openaiUpstream, openaiSeen := fakeOpenAI(t)

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "anthropic-key")
	if err := os.WriteFile(keyFile, []byte("sk-ant-fake\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	modelsFile := filepath.Join(dir, "models.json")
	if err := os.WriteFile(modelsFile, []byte(table), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		PublicAddr:            "127.0.0.1:0",
		AdminAddr:             "127.0.0.1:0",
		MetricsAddr:           "127.0.0.1:0",
		DataDir:               filepath.Join(dir, "data"),
		AnthropicAPIKeyFile:   keyFile,
		AnthropicBaseURL:      upstream.URL,
		ModelsConfigFile:      modelsFile,
		BudgetMonthlyUSD:      budgetUSD,
		AdminIdentityHeader:   "Remote-User",
		DefaultMaxTokens:      4096,
		MaxBodyBytes:          1 << 20,
		RateLimitPerKeyRPM:    600,
		MaxConcurrentRequests: 8,
		RequestDeadline:       time.Minute,
	}
	if tableUsesOpenAI(t, table) {
		openaiKeyFile := filepath.Join(dir, "openai-key")
		if err := os.WriteFile(openaiKeyFile, []byte("sk-oai-fake\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg.OpenAIAPIKeyFile = openaiKeyFile
		cfg.OpenAIBaseURL = openaiUpstream.URL
	}

	s, err := server.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve returned %v after cancel", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after cancel")
		}
	})

	gw := gateway{
		PublicAddr:  s.PublicAddr(),
		AdminAddr:   s.AdminAddr(),
		MetricsAddr: s.MetricsAddr(),
		OpenAI:      openaiSeen,
		Anthropic:   anthropicSeen,
	}
	waitForReady(t, "http://"+gw.MetricsAddr+"/ready")
	return gw
}

func waitForReady(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res, err := http.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never answered 200", url)
}

func adminCreateKey(t *testing.T, adminAddr, label string) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{"label": label})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+adminAddr+"/api/keys", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Remote-User", "tester")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		payload, _ := io.ReadAll(res.Body)
		t.Fatalf("create key: got %d, want 201; body %s", res.StatusCode, payload)
	}
	var out struct {
		FullKey string `json:"full_key"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.FullKey == "" {
		t.Fatal("create key: empty full_key")
	}
	return out.FullKey
}

func adminGetJSON(t *testing.T, adminAddr, path string, dst any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+adminAddr+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Remote-User", "tester")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(res.Body)
		t.Fatalf("GET %s: got %d, want 200; body %s", path, res.StatusCode, payload)
	}
	if err := json.NewDecoder(res.Body).Decode(dst); err != nil {
		t.Fatal(err)
	}
}

func chatCompletionRaw(t *testing.T, publicAddr, bearer string) *http.Response {
	t.Helper()
	return chatCompletion(t, publicAddr, bearer,
		fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, testModel))
}

func chatCompletion(t *testing.T, publicAddr, bearer, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost,
		"http://"+publicAddr+"/v1/chat/completions", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// metricValue reads one counter or gauge out of the /metrics exposition; the
// series must be spelled with its labels exactly as the registry renders them.
func metricValue(t *testing.T, metricsAddr, series string) float64 {
	t.Helper()
	res, err := http.Get("http://" + metricsAddr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(body)) {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), series+" ")
		if !ok {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
		if err != nil {
			t.Fatalf("series %s has unparsable value %q: %v", series, rest, err)
		}
		return value
	}
	t.Fatalf("series %s absent from /metrics", series)
	return 0
}

func decodeErrorCode(t *testing.T, res *http.Response) string {
	t.Helper()
	defer res.Body.Close()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return body.Error.Code
}
