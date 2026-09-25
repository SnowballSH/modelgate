package server

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnowballSH/modelgate/internal/accounting"
)

func monthSpend(t *testing.T, env *publicEnv) float64 {
	t.Helper()
	spend, err := env.store.MonthSpend(t.Context(), accounting.Month(env.now))
	if err != nil {
		t.Fatal(err)
	}
	return spend
}

func TestAnthropicStreamCutAfterLongDeltaBooksEstimatedOutput(t *testing.T) {
	long := strings.Repeat("a", 40_000)
	env := newPublicEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"type\":\"message\",\"role\":\"assistant\",\"usage\":{\"input_tokens\":100,\"output_tokens\":1}}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")
		fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", long)
	}, 1<<20)
	auth, _ := insertTestKey(t, env.store, nil)

	doPublic(env, http.MethodPost, "/v1/chat/completions", auth, chatBody(true))

	want := 100*3.0/1e6 + 10_000*15.0/1e6
	if got := monthSpend(t, env); math.Abs(got-want) > 1e-9 {
		t.Fatalf("spend after a stream cut mid-answer = %v, want %v (input 100 plus 40,000 chars estimated at 10,000 output tokens)", got, want)
	}
}

func TestNonStreamClientAbortStillBooksUsage(t *testing.T) {
	arrived := make(chan struct{})
	release := make(chan struct{})
	full := fullResponseHandler()
	env := newPublicEnv(t, func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		select {
		case <-release:
			full(w, r)
		case <-r.Context().Done():
		}
	}, 1<<20)
	auth, _ := insertTestKey(t, env.store, nil)

	ctx, cancel := context.WithCancel(t.Context())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody(false)))
	req.Header.Set("Authorization", auth)
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.handler.ServeHTTP(httptest.NewRecorder(), req)
	}()

	waitFor(t, arrived, "the upstream call never started")
	cancel()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	waitFor(t, done, "the handler never returned")

	want := 100*3/1e6 + 50*15/1e6 + 10*0.30/1e6 + 5*3.75/1e6
	if got := monthSpend(t, env); math.Abs(got-want) > 1e-12 {
		t.Fatalf("spend after the client hung up = %v, want %v (the provider billed the call)", got, want)
	}
}

func waitFor(t *testing.T, ch <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal(failure)
	}
}

func eventually(t *testing.T, cond func() bool, failure func() string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(failure())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// parallelAgainstCap sends one request per auth at once to an upstream that
// holds every call until each request has reached it or been answered, and
// returns every response status.
func parallelAgainstCap(t *testing.T, env *publicEnv, arrivals *atomic.Int32, release chan struct{}, auths []string) []int {
	t.Helper()
	var returned atomic.Int32
	codes := make(chan int, len(auths))
	for _, auth := range auths {
		go func() {
			code := doPublic(env, http.MethodPost, "/v1/chat/completions", auth, chatBody(false)).Code
			returned.Add(1)
			codes <- code
		}()
	}
	eventually(t,
		func() bool { return int(arrivals.Load()+returned.Load()) == len(auths) },
		func() string {
			return fmt.Sprintf("requests never settled: %d at the upstream, %d answered", arrivals.Load(), returned.Load())
		})
	close(release)
	out := make([]int, 0, len(auths))
	for range auths {
		out = append(out, <-codes)
	}
	return out
}

func holdingUpstream() (http.HandlerFunc, *atomic.Int32, chan struct{}) {
	var arrivals atomic.Int32
	release := make(chan struct{})
	full := fullResponseHandler()
	return func(w http.ResponseWriter, r *http.Request) {
		arrivals.Add(1)
		<-release
		full(w, r)
	}, &arrivals, release
}

func TestParallelRequestsCannotOvershootKeyQuota(t *testing.T) {
	upstream, arrivals, release := holdingUpstream()
	env := newPublicEnv(t, upstream, 1<<20)
	auth := insertQuotaKey(t, env, 0.0001)

	codes := parallelAgainstCap(t, env, arrivals, release, slices.Repeat([]string{auth}, 8))

	if got := arrivals.Load(); got != 1 {
		t.Fatalf("%d of 8 parallel requests reached the provider against a quota one request exhausts, want 1 (statuses %v)", got, codes)
	}
}

func TestParallelRequestsCannotOvershootMonthlyBudget(t *testing.T) {
	upstream, arrivals, release := holdingUpstream()
	env := newPublicEnvWith(t, upstream, publicEnvOptions{maxBodyBytes: 1 << 20, budgetUSD: 0.0001, rpm: 1000})
	auths := make([]string, 8)
	for i := range auths {
		auths[i], _ = insertTestKey(t, env.store, nil)
	}

	codes := parallelAgainstCap(t, env, arrivals, release, auths)

	if got := arrivals.Load(); got != 1 {
		t.Fatalf("%d of 8 keys reached the provider against a budget one request exhausts, want 1 (statuses %v)", got, codes)
	}
}
