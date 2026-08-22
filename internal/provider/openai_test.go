package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnowballSH/modelgate/internal/oai"
)

func newTestOpenAIClient(baseURL string) *OpenAIClient {
	c := NewOpenAIClient(baseURL, "test-key", nil)
	c.sleep = func(time.Duration) {}
	return c
}

func chatRequest() oai.ChatRequest {
	return oai.ChatRequest{
		Model:    "gpt-test",
		Messages: []oai.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
}

func TestOpenAIChatSuccess(t *testing.T) {
	var gotAuth, gotContentType string
	var gotBody oai.ChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		content := "hello"
		_ = json.NewEncoder(w).Encode(oai.ChatResponse{
			ID:    "chatcmpl-1",
			Model: "gpt-test",
			Choices: []oai.Choice{{
				Message:      oai.ResponseMessage{Role: "assistant", Content: &content},
				FinishReason: "stop",
			}},
		})
	}))
	defer srv.Close()

	res, err := newTestOpenAIClient(srv.URL).Chat(context.Background(), chatRequest())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q", gotContentType)
	}
	if gotBody.Stream {
		t.Error("Chat sent stream=true")
	}
	if res.ID != "chatcmpl-1" || len(res.Choices) != 1 || *res.Choices[0].Message.Content != "hello" {
		t.Errorf("response = %+v", res)
	}
}

func TestOpenAIChatStatusMapping(t *testing.T) {
	cases := []struct {
		status       int
		wantErr      error
		wantRequests int32
	}{
		{http.StatusUnauthorized, ErrAuth, 1},
		{http.StatusTooManyRequests, ErrRateLimited, 3},
		{http.StatusInternalServerError, ErrUnavailable, 3},
		{http.StatusBadRequest, ErrInvalidRequest, 1},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":{"message":"SECRET-OAI"}}`))
			}))
			defer srv.Close()

			_, err := newTestOpenAIClient(srv.URL).Chat(context.Background(), chatRequest())
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), "SECRET-OAI") {
				t.Errorf("error leaks upstream body: %v", err)
			}
			if got := requests.Load(); got != tc.wantRequests {
				t.Errorf("requests = %d, want %d", got, tc.wantRequests)
			}
		})
	}
}

func sseChunk(t *testing.T, chunk oai.ChatChunk) string {
	t.Helper()
	b, err := json.Marshal(chunk)
	if err != nil {
		t.Fatalf("marshal chunk: %v", err)
	}
	return "data: " + string(b) + "\n\n"
}

func streamScript(t *testing.T) []string {
	stop := "stop"
	return []string{
		sseChunk(t, oai.ChatChunk{ID: "c", Choices: []oai.ChunkChoice{{Delta: oai.Delta{Role: "assistant"}}}}),
		sseChunk(t, oai.ChatChunk{ID: "c", Choices: []oai.ChunkChoice{{Delta: oai.Delta{Content: "Hel"}}}}),
		sseChunk(t, oai.ChatChunk{ID: "c", Choices: []oai.ChunkChoice{{Delta: oai.Delta{Content: "lo"}}}}),
		sseChunk(t, oai.ChatChunk{ID: "c", Choices: []oai.ChunkChoice{{FinishReason: &stop}}}),
		sseChunk(t, oai.ChatChunk{ID: "c", Usage: &oai.Usage{TotalTokens: 7}}),
		"data: [DONE]\n\n",
	}
}

func writeSSE(w http.ResponseWriter, lines []string) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher := w.(http.Flusher)
	for _, line := range lines {
		_, _ = w.Write([]byte(line))
		flusher.Flush()
	}
}

func TestOpenAIChatStreamSuccess(t *testing.T) {
	var gotBody oai.ChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeSSE(w, streamScript(t))
	}))
	defer srv.Close()

	var chunks []oai.ChatChunk
	req := chatRequest()
	req.StreamOptions = &oai.StreamOptions{IncludeUsage: true}
	err := newTestOpenAIClient(srv.URL).ChatStream(context.Background(), req, func(ch oai.ChatChunk) error {
		chunks = append(chunks, ch)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if !gotBody.Stream {
		t.Error("ChatStream sent stream=false")
	}
	if gotBody.StreamOptions == nil || !gotBody.StreamOptions.IncludeUsage {
		t.Error("stream_options not passed through")
	}
	if len(chunks) != 5 {
		t.Fatalf("chunks = %d, want 5", len(chunks))
	}
	if chunks[0].Choices[0].Delta.Role != "assistant" {
		t.Errorf("chunk 0 = %+v", chunks[0])
	}
	if chunks[1].Choices[0].Delta.Content != "Hel" || chunks[2].Choices[0].Delta.Content != "lo" {
		t.Errorf("content chunks = %+v %+v", chunks[1], chunks[2])
	}
	if chunks[3].Choices[0].FinishReason == nil || *chunks[3].Choices[0].FinishReason != "stop" {
		t.Errorf("finish chunk = %+v", chunks[3])
	}
	if chunks[4].Usage == nil || chunks[4].Usage.TotalTokens != 7 || len(chunks[4].Choices) != 0 {
		t.Errorf("usage chunk = %+v", chunks[4])
	}
}

func TestOpenAIChatStreamNoRetryAfterFirstChunk(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		writeSSE(w, streamScript(t)[:1])
	}))
	defer srv.Close()

	var delivered int
	err := newTestOpenAIClient(srv.URL).ChatStream(context.Background(), chatRequest(), func(oai.ChatChunk) error {
		delivered++
		return nil
	})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if delivered != 1 {
		t.Errorf("delivered = %d, want 1", delivered)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
}

func TestOpenAIChatStreamRetryBeforeFirstByte(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(529)
			return
		}
		writeSSE(w, streamScript(t))
	}))
	defer srv.Close()

	var delivered int
	err := newTestOpenAIClient(srv.URL).ChatStream(context.Background(), chatRequest(), func(oai.ChatChunk) error {
		delivered++
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("requests = %d, want 2", got)
	}
	if delivered != 5 {
		t.Errorf("delivered = %d, want 5", delivered)
	}
}

func TestOpenAIChatStreamEachErrorAborts(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		writeSSE(w, streamScript(t))
	}))
	defer srv.Close()

	sentinel := errors.New("consumer failed")
	err := newTestOpenAIClient(srv.URL).ChatStream(context.Background(), chatRequest(), func(oai.ChatChunk) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
}

func TestOpenAIChatContextCanceled(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	_, err := newTestOpenAIClient(srv.URL).Chat(ctx, chatRequest())
	if !errors.Is(err, ErrClientAborted) {
		t.Fatalf("err = %v, want ErrClientAborted", err)
	}
}

func TestOpenAIChatDeadlineExceeded(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := newTestOpenAIClient(srv.URL).Chat(ctx, chatRequest())
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}
