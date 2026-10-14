package server

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnowballSH/modelgate/internal/models"
	"github.com/SnowballSH/modelgate/internal/store"
)

const (
	bodiesInFlight = "modelgate_request_bodies_in_flight"
	bodyRefusals   = "modelgate_request_body_refusals_total"
)

// watchedBody is a request body that records whether the handler read it,
// and can stall until released.
type watchedBody struct {
	r       io.Reader
	read    atomic.Bool
	started chan struct{}
	once    sync.Once
}

func newWatchedBody(r io.Reader) *watchedBody {
	return &watchedBody{r: r, started: make(chan struct{})}
}

func (b *watchedBody) Read(p []byte) (int, error) {
	b.read.Store(true)
	b.once.Do(func() { close(b.started) })
	return b.r.Read(p)
}

func postBody(env *publicEnv, auth string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Authorization", auth)
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	return rec
}

func largeChatBody(size int) string {
	return fmt.Sprintf(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":%q}]}`, strings.Repeat("x", size))
}

func TestABodyBurstBeyondTheBoundIsRefusedUnread(t *testing.T) {
	env := newPublicEnvWith(t, fullResponseHandler(), publicEnvOptions{maxBodyBytes: 8 << 20, budgetUSD: 100, rpm: 1000, maxBodies: 2})
	auth, _ := insertTestKey(t, env.store, nil)

	writers := make([]*io.PipeWriter, 2)
	stalled := make([]chan int, 2)
	for i := range writers {
		pr, pw := io.Pipe()
		t.Cleanup(func() { pw.CloseWithError(errors.New("test over")) })
		writers[i], stalled[i] = pw, make(chan int, 1)
		body := newWatchedBody(pr)
		go func() { stalled[i] <- postBody(env, auth, body).Code }()
		waitFor(t, body.started, "a stalled body was never read")
	}

	large := largeChatBody(5 << 20)
	for i := range 16 {
		body := newWatchedBody(strings.NewReader(large))
		start := time.Now()
		rec := postBody(env, auth, body)
		if rec.Code != http.StatusTooManyRequests || decodeErrorCode(t, rec) != CodeRateLimited || rec.Header().Get("Retry-After") != "1" {
			t.Fatalf("burst request %d: status %d body %s Retry-After %q, want 429 %s after 1s", i, rec.Code, rec.Body.String(), rec.Header().Get("Retry-After"), CodeRateLimited)
		}
		if body.read.Load() {
			t.Fatalf("burst request %d: its body was read while %d bodies were already held", i, len(writers))
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("burst request %d took %v to refuse", i, elapsed)
		}
	}
	if got := scrape(t, env.metrics, bodiesInFlight); got != 2 {
		t.Errorf("%s during the burst = %v, want 2", bodiesInFlight, got)
	}
	if got := scrape(t, env.metrics, bodyRefusals); got != 16 {
		t.Errorf("%s = %v, want 16", bodyRefusals, got)
	}

	for i, pw := range writers {
		fmt.Fprint(pw, chatBody(false))
		_ = pw.Close()
		if code := <-stalled[i]; code != http.StatusOK {
			t.Errorf("stalled request %d once its body arrived: status %d, want 200", i, code)
		}
	}
	if got := scrape(t, env.metrics, bodiesInFlight); got != 0 {
		t.Errorf("%s after every request = %v, want 0", bodiesInFlight, got)
	}
}

func TestARateLimitedRequestIsRefusedUnread(t *testing.T) {
	env := newPublicEnvWith(t, fullResponseHandler(), publicEnvOptions{maxBodyBytes: 1 << 20, budgetUSD: 100, rpm: 1})
	auth, _ := insertTestKey(t, env.store, nil)
	if rec := postBody(env, auth, strings.NewReader(chatBody(false))); rec.Code != http.StatusOK {
		t.Fatalf("first request: status %d", rec.Code)
	}

	body := newWatchedBody(strings.NewReader(chatBody(false)))
	rec := postBody(env, auth, body)
	if rec.Code != http.StatusTooManyRequests || decodeErrorCode(t, rec) != CodeRateLimited || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("second request in the minute: status %d body %s, want 429 %s with Retry-After", rec.Code, rec.Body.String(), CodeRateLimited)
	}
	if body.read.Load() {
		t.Fatal("the rate-limited request's body was read")
	}
	if got := scrape(t, env.metrics, bodyRefusals); got != 0 {
		t.Errorf("a rate refusal counted as a body refusal: %v", got)
	}
}

type failingAfter struct {
	r io.Reader
}

func (f failingAfter) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if errors.Is(err, io.EOF) {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

// TestEveryExitPathReleasesTheBodySlot runs each way a request can end
// with one body slot, then checks the slot is free for the next request.
func TestEveryExitPathReleasesTheBodySlot(t *testing.T) {
	failing := func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) }
	for name, tc := range map[string]struct {
		upstream http.HandlerFunc
		maxBody  int64
		budget   float64
		body     io.Reader
		want     int
	}{
		"success":              {body: strings.NewReader(chatBody(false)), want: http.StatusOK},
		"upstream error":       {upstream: failing, body: strings.NewReader(chatBody(false)), want: http.StatusServiceUnavailable},
		"client gone mid-body": {body: failingAfter{strings.NewReader(chatBody(false)[:20])}, want: http.StatusBadRequest},
		"oversize body":        {maxBody: 64, body: strings.NewReader(largeChatBody(1024)), want: http.StatusRequestEntityTooLarge},
		"invalid JSON":         {body: strings.NewReader("{"), want: http.StatusBadRequest},
		"unknown model":        {body: strings.NewReader(`{"model":"gpt-9","messages":[]}`), want: http.StatusNotFound},
		"translation error":    {body: strings.NewReader(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":[{"type":"bogus"}]}]}`), want: http.StatusBadRequest},
		"admission refusal":    {budget: 0.0000001, body: strings.NewReader(chatBody(false)), want: http.StatusTooManyRequests},
	} {
		t.Run(name, func(t *testing.T) {
			upstream := tc.upstream
			if upstream == nil {
				upstream = fullResponseHandler()
			}
			opts := publicEnvOptions{maxBodyBytes: 1 << 20, budgetUSD: 100, rpm: 1000, maxBodies: 1}
			if tc.maxBody > 0 {
				opts.maxBodyBytes = tc.maxBody
			}
			if tc.budget > 0 {
				opts.budgetUSD = tc.budget
			}
			env := newPublicEnvWith(t, upstream, opts)
			auth, gen := insertTestKey(t, env.store, nil)
			if tc.budget > 0 {
				if err := env.acct.Record(t.Context(), env.now, gen.ID, "claude-sonnet-5", models.ProviderAnthropic, store.Usage{InputTokens: 1}, models.Pricing{InputUSDPerMTok: 1}); err != nil {
					t.Fatal(err)
				}
			}

			if rec := postBody(env, auth, tc.body); rec.Code != tc.want {
				t.Fatalf("status %d body %s, want %d", rec.Code, rec.Body.String(), tc.want)
			}
			assertBodySlotFree(t, env, auth)
		})
	}
}

func assertBodySlotFree(t *testing.T, env *publicEnv, auth string) {
	t.Helper()
	if got := scrape(t, env.metrics, bodiesInFlight); got != 0 {
		t.Fatalf("%s after the request = %v, want 0", bodiesInFlight, got)
	}
	next := newWatchedBody(strings.NewReader(chatBody(false)))
	postBody(env, auth, next)
	if !next.read.Load() || scrape(t, env.metrics, bodyRefusals) != 0 {
		t.Fatalf("the next request was refused a body slot: read %v, refusals %v", next.read.Load(), scrape(t, env.metrics, bodyRefusals))
	}
}

func TestABodyReadTimeoutReleasesTheBodySlot(t *testing.T) {
	env := newPublicEnvWith(t, fullResponseHandler(), publicEnvOptions{maxBodyBytes: 1 << 20, budgetUSD: 100, rpm: 1000, bodyReadTimeout: 150 * time.Millisecond, maxBodies: 1})
	auth, _ := insertTestKey(t, env.store, nil)
	srv := httptest.NewServer(env.handler)
	t.Cleanup(srv.Close)

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	body := chatBody(false)
	fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nAuthorization: %s\r\nContent-Length: %d\r\n\r\n%s", auth, len(body), body[:10])
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("a stalled body: status %d, want 408", res.StatusCode)
	}
	eventually(t, func() bool { return scrape(t, env.metrics, bodiesInFlight) == 0 },
		func() string { return "the timed-out request kept its body slot" })
	assertBodySlotFree(t, env, auth)
}

func TestAnAdmittedRequestHandsItsBodySlotOn(t *testing.T) {
	upstream, arrivals, release := holdingUpstream()
	env := newPublicEnvWith(t, upstream, publicEnvOptions{maxBodyBytes: 1 << 20, budgetUSD: 100, rpm: 1000, maxBodies: 1})
	auth, _ := insertTestKey(t, env.store, nil)
	releaseUpstream := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseUpstream)

	codes := make(chan int, 2)
	for range 2 {
		go func() { codes <- postBody(env, auth, strings.NewReader(chatBody(false))).Code }()
	}
	eventually(t, func() bool { return arrivals.Load()+int32(len(codes)) == 2 },
		func() string { return fmt.Sprintf("%d requests reached the upstream", arrivals.Load()) })
	inFlight := scrape(t, env.metrics, bodiesInFlight)
	releaseUpstream()
	for range 2 {
		if code := <-codes; code != http.StatusOK {
			t.Errorf("status %d, want 200", code)
		}
	}
	if arrivals.Load() != 2 || inFlight != 0 {
		t.Fatalf("with one body slot, %d of 2 requests reached the upstream and %v bodies were held while both waited on it; want 2 and 0", arrivals.Load(), inFlight)
	}
}
