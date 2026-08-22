package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnowballSH/modelgate/internal/anthro"
)

func newTestClient(t *testing.T, url string) *Client {
	t.Helper()
	c := NewClient(url, "test-key", nil)
	c.sleep = func(time.Duration) {}
	return c
}

func sampleRequest() anthro.MessagesRequest {
	return anthro.MessagesRequest{
		Model:     "claude-test",
		MaxTokens: 16,
		Messages: []anthro.Message{{
			Role:    "user",
			Content: []anthro.ContentBlock{{Type: "text", Text: "hi"}},
		}},
	}
}

func TestMessagesSuccessSendsHeadersAndDecodes(t *testing.T) {
	var gotKey, gotVersion, gotContentType, gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotContentType = r.Header.Get("content-type")
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":5}}`)
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL).Messages(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/messages" {
		t.Fatalf("got %s %s, want POST /v1/messages", gotMethod, gotPath)
	}
	if gotKey != "test-key" {
		t.Fatalf("x-api-key = %q", gotKey)
	}
	if gotVersion != "2023-06-01" {
		t.Fatalf("anthropic-version = %q", gotVersion)
	}
	if gotContentType != "application/json" {
		t.Fatalf("content-type = %q", gotContentType)
	}
	if resp.ID != "msg_1" || len(resp.Content) != 1 || resp.Content[0].Text != "hello" {
		t.Fatalf("decoded response = %+v", resp)
	}
	if resp.Usage.OutputTokens != 5 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestMessagesStatusMapping(t *testing.T) {
	cases := []struct {
		status       int
		want         error
		wantRequests int32
	}{
		{status: 401, want: ErrAuth, wantRequests: 1},
		{status: 403, want: ErrAuth, wantRequests: 1},
		{status: 408, want: ErrTimeout, wantRequests: 1},
		{status: 429, want: ErrRateLimited, wantRequests: 3},
		{status: 500, want: ErrUnavailable, wantRequests: 3},
		{status: 529, want: ErrUnavailable, wantRequests: 3},
		{status: 400, want: ErrInvalidRequest, wantRequests: 1},
		{status: 422, want: ErrInvalidRequest, wantRequests: 1},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("status_%d", tc.status), func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				http.Error(w, "upstream detail", tc.status)
			}))
			defer srv.Close()

			_, err := newTestClient(t, srv.URL).Messages(context.Background(), sampleRequest())
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if got := requests.Load(); got != tc.wantRequests {
				t.Fatalf("requests = %d, want %d", got, tc.wantRequests)
			}
		})
	}
}

func TestMessagesRetryThenSuccess(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			http.Error(w, "overloaded", 529)
			return
		}
		fmt.Fprint(w, `{"id":"msg_2","content":[{"type":"text","text":"ok"}]}`)
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL).Messages(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if resp.ID != "msg_2" {
		t.Fatalf("resp = %+v", resp)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}

func TestMessagesErrorDoesNotLeakUpstreamBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "SECRET-DETAIL", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).Messages(context.Background(), sampleRequest())
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "SECRET-DETAIL") {
		t.Fatalf("error leaks upstream body: %q", err)
	}
}

func TestMessagesContextTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := newTestClient(t, srv.URL).Messages(ctx, sampleRequest())
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}

const streamBody = "event: message_start\n" +
	"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_s\",\"role\":\"assistant\"}}\n" +
	"\n" +
	"event: content_block_delta\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n" +
	"\n" +
	"event: message_stop\n" +
	"data: {\"type\":\"message_stop\"}\n" +
	"\n"

func TestMessagesStreamSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, streamBody)
	}))
	defer srv.Close()

	var types []string
	err := newTestClient(t, srv.URL).MessagesStream(context.Background(), sampleRequest(), func(ev anthro.StreamEvent) error {
		types = append(types, ev.Type)
		return nil
	})
	if err != nil {
		t.Fatalf("MessagesStream: %v", err)
	}
	want := []string{"message_start", "content_block_delta", "message_stop"}
	if !slices.Equal(types, want) {
		t.Fatalf("event types = %v, want %v", types, want)
	}
}

func TestMessagesStreamSetsStreamTrue(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		fmt.Fprint(w, streamBody)
	}))
	defer srv.Close()

	err := newTestClient(t, srv.URL).MessagesStream(context.Background(), sampleRequest(), func(anthro.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("MessagesStream: %v", err)
	}
	if !strings.Contains(gotBody, `"stream":true`) {
		t.Fatalf("request body missing stream:true: %s", gotBody)
	}
}

func TestMessagesStreamNoRetryAfterFirstEvent(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()

	var delivered int
	err := newTestClient(t, srv.URL).MessagesStream(context.Background(), sampleRequest(), func(anthro.StreamEvent) error {
		delivered++
		return nil
	})
	if err == nil {
		t.Fatal("expected error from truncated stream")
	}
	if delivered != 1 {
		t.Fatalf("delivered = %d, want 1", delivered)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1 (no retry after first event)", got)
	}
}

func TestMessagesStreamRetryBeforeFirstByte(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			http.Error(w, "overloaded", 529)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, streamBody)
	}))
	defer srv.Close()

	var types []string
	err := newTestClient(t, srv.URL).MessagesStream(context.Background(), sampleRequest(), func(ev anthro.StreamEvent) error {
		types = append(types, ev.Type)
		return nil
	})
	if err != nil {
		t.Fatalf("MessagesStream: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
	if len(types) != 3 || types[2] != "message_stop" {
		t.Fatalf("event types = %v", types)
	}
}

func TestMessagesStreamEachErrorReturnedUnchanged(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, streamBody)
	}))
	defer srv.Close()

	sentinel := errors.New("consumer failed")
	err := newTestClient(t, srv.URL).MessagesStream(context.Background(), sampleRequest(), func(anthro.StreamEvent) error {
		return sentinel
	})
	if err != sentinel {
		t.Fatalf("err = %v, want the each() error unchanged", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

func TestMessagesStreamMalformedDataLine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "event: message_start\ndata: {not json\n\n")
	}))
	defer srv.Close()

	err := newTestClient(t, srv.URL).MessagesStream(context.Background(), sampleRequest(), func(anthro.StreamEvent) error { return nil })
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestMessagesStreamContextTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := newTestClient(t, srv.URL).MessagesStream(ctx, sampleRequest(), func(anthro.StreamEvent) error { return nil })
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}
