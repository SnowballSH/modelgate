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
	"testing"
	"time"

	"github.com/SnowballSH/modelgate/internal/anthro"
	"github.com/SnowballSH/modelgate/internal/config"
	"github.com/SnowballSH/modelgate/internal/server"
)

const testModel = "claude-sonnet-5"

func fakeAnthropic(t *testing.T) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		var req anthro.MessagesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Stream {
			serveFakeStream(w, req.Model)
			return
		}
		serveFakeMessage(w, req.Model)
	}))
	t.Cleanup(upstream.Close)
	return upstream
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
	w.Header().Set("Content-Type", "text/event-stream")
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
	flusher, _ := w.(http.Flusher)
	for _, ev := range events {
		payload, _ := json.Marshal(ev)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, payload)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

type gateway struct {
	PublicAddr  string
	AdminAddr   string
	MetricsAddr string
}

func startGateway(t *testing.T, budgetUSD float64) gateway {
	t.Helper()
	upstream := fakeAnthropic(t)

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "anthropic-key")
	if err := os.WriteFile(keyFile, []byte("sk-ant-fake\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	modelsFile := filepath.Join(dir, "models.json")
	table := fmt.Sprintf(`{"models":{%q:{"provider_model":%q,
		"input_usd_per_mtok":3,"output_usd_per_mtok":15,
		"cache_read_usd_per_mtok":0.3,"cache_write_usd_per_mtok":3.75}}}`, testModel, testModel)
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
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, testModel)
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
