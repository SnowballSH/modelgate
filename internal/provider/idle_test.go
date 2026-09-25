package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SnowballSH/modelgate/internal/anthro"
	"github.com/SnowballSH/modelgate/internal/oai"
	"github.com/SnowballSH/modelgate/internal/oairesp"
)

// testIdleTimeout is armed before the request is sent, so it must also cover
// connecting and the first event under -race on a loaded machine.
const testIdleTimeout = 500 * time.Millisecond

// stallingServer sends head, flushes, then goes silent until the test ends.
func stallingServer(t *testing.T, head string) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if head != "" {
			w.Header().Set("content-type", "text/event-stream")
			fmt.Fprint(w, head)
			w.(http.Flusher).Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	return srv
}

func assertIdleAbort(t *testing.T, err error, elapsed time.Duration) {
	t.Helper()
	if !errors.Is(err, ErrTimeout) || !errors.Is(err, errStreamIdle) {
		t.Fatalf("err = %v, want an idle ErrTimeout", err)
	}
	if errors.Is(err, ErrClientAborted) {
		t.Fatalf("err = %v, an idle upstream is not a caller abort", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("abort took %s, want it bounded by the idle timeout", elapsed)
	}
}

func TestAnthropicStreamAbortsWhenUpstreamGoesQuiet(t *testing.T) {
	srv := stallingServer(t, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
	c := newTestClient(t, srv.URL)
	c.SetStreamIdleTimeout(testIdleTimeout)

	var delivered int
	start := time.Now()
	err := c.MessagesStream(context.Background(), sampleRequest(), func(anthro.StreamEvent) error {
		delivered++
		return nil
	})
	assertIdleAbort(t, err, time.Since(start))
	if delivered != 1 {
		t.Errorf("delivered = %d, want the one event sent before the stall", delivered)
	}
}

func TestAnthropicStreamAbortsWhenHeadersNeverArrive(t *testing.T) {
	srv := stallingServer(t, "")
	c := newTestClient(t, srv.URL)
	c.SetStreamIdleTimeout(testIdleTimeout)

	start := time.Now()
	err := c.MessagesStream(context.Background(), sampleRequest(), func(anthro.StreamEvent) error { return nil })
	assertIdleAbort(t, err, time.Since(start))
}

func TestAnthropicStreamSlowButAliveCompletes(t *testing.T) {
	events := []string{
		`{"type":"message_start"}`,
		`{"type":"ping"}`,
		`{"type":"ping"}`,
		`{"type":"ping"}`,
		`{"type":"message_stop"}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		for _, ev := range events {
			fmt.Fprintf(w, "data: %s\n\n", ev)
			w.(http.Flusher).Flush()
			time.Sleep(testIdleTimeout / 4)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	c.SetStreamIdleTimeout(testIdleTimeout)

	start := time.Now()
	var delivered int
	err := c.MessagesStream(context.Background(), sampleRequest(), func(anthro.StreamEvent) error {
		delivered++
		return nil
	})
	if err != nil {
		t.Fatalf("MessagesStream: %v", err)
	}
	if delivered != len(events) {
		t.Errorf("delivered = %d, want %d", delivered, len(events))
	}
	if elapsed := time.Since(start); elapsed < testIdleTimeout {
		t.Errorf("stream took %s, want it to outlast one idle timeout", elapsed)
	}
}

func TestAnthropicStreamCallerCancelIsStillAnAbort(t *testing.T) {
	srv := stallingServer(t, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
	c := newTestClient(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	err := c.MessagesStream(ctx, sampleRequest(), func(anthro.StreamEvent) error {
		cancel()
		return nil
	})
	if !errors.Is(err, ErrClientAborted) {
		t.Fatalf("err = %v, want ErrClientAborted", err)
	}
}

func TestOpenAIChatStreamAbortsWhenUpstreamGoesQuiet(t *testing.T) {
	srv := stallingServer(t, "data: {\"id\":\"c\",\"choices\":[]}\n\n")
	c := newTestOpenAIClient(srv.URL)
	c.SetStreamIdleTimeout(testIdleTimeout)

	start := time.Now()
	err := c.ChatStream(context.Background(), chatRequest(), func(oai.ChatChunk) error { return nil })
	assertIdleAbort(t, err, time.Since(start))
}

func TestOpenAIResponsesStreamAbortsWhenUpstreamGoesQuiet(t *testing.T) {
	srv := stallingServer(t, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
	c := newTestOpenAIClient(srv.URL)
	c.SetStreamIdleTimeout(testIdleTimeout)

	start := time.Now()
	err := c.ResponsesStream(context.Background(), responsesRequest(), func(oairesp.StreamEvent) error { return nil })
	assertIdleAbort(t, err, time.Since(start))
}

func TestZeroIdleTimeoutTurnsTheGuardOff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, streamBody)
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	c.SetStreamIdleTimeout(0)
	if err := c.MessagesStream(context.Background(), sampleRequest(), func(anthro.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("MessagesStream with the guard off: %v", err)
	}
}

func TestDefaultStreamIdleTimeoutIsInstalled(t *testing.T) {
	if got := NewClient("http://x", "k", nil).idleTimeout; got != DefaultStreamIdleTimeout {
		t.Errorf("anthropic idle timeout = %s", got)
	}
	if got := NewOpenAIClient("http://x", "k", nil).idleTimeout; got != DefaultStreamIdleTimeout {
		t.Errorf("openai idle timeout = %s", got)
	}
}
