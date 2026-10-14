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
	bodyRefusals   = `modelgate_request_body_refusals_total{limit="gateway"}`
	keyRefusals    = `modelgate_request_body_refusals_total{limit="key"}`
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
	env := newPublicEnvWith(t, fullResponseHandler(), publicEnvOptions{maxBodyBytes: 8 << 20, budgetUSD: 100, rpm: 1000, maxBodies: 2, maxBodiesPerKey: 100})
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
	for i := range 2 {
		go func() { codes <- postBody(env, auth, strings.NewReader(chatBody(false))).Code }()
		eventually(t, func() bool { return int(arrivals.Load())+len(codes) == i+1 },
			func() string {
				return fmt.Sprintf("request %d never settled; %d reached the upstream", i, arrivals.Load())
			})
	}
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

// stallBody starts a request whose body never finishes arriving, and
// returns once the handler is reading it.
func stallBody(t *testing.T, env *publicEnv, auth string) {
	t.Helper()
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.CloseWithError(errors.New("test over")) })
	body := newWatchedBody(pr)
	go postBody(env, auth, body)
	waitFor(t, body.started, "a stalled body was never read")
}

func TestAKeyAtItsBodyLimitIsRefusedWhileAnotherKeyIsAdmitted(t *testing.T) {
	env := newPublicEnvWith(t, fullResponseHandler(), publicEnvOptions{maxBodyBytes: 1 << 20, budgetUSD: 100, rpm: 1000, maxBodies: 4, maxBodiesPerKey: 2})
	hog, _ := insertTestKey(t, env.store, nil)
	other, _ := insertTestKey(t, env.store, nil)
	stallBody(t, env, hog)
	stallBody(t, env, hog)

	body := newWatchedBody(strings.NewReader(chatBody(false)))
	rec := postBody(env, hog, body)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("a third body from the key: status %d Retry-After %q, want 429 after 1s", rec.Code, rec.Header().Get("Retry-After"))
	}
	if detail := decodeError(t, rec); detail.Code != CodeRateLimited || detail.Message != "this key's concurrent request limit is reached; retry shortly" {
		t.Errorf("refusal = %+v, want %s naming the key's limit", detail, CodeRateLimited)
	}
	if body.read.Load() {
		t.Error("the refused body was read")
	}
	if rec := postBody(env, other, strings.NewReader(chatBody(false))); rec.Code != http.StatusOK {
		t.Fatalf("another key while the first holds its limit: status %d body %s", rec.Code, rec.Body.String())
	}
	if got, gateway := scrape(t, env.metrics, keyRefusals), scrape(t, env.metrics, bodyRefusals); got != 1 || gateway != 0 {
		t.Errorf("refusals: key %v gateway %v, want 1 and 0", got, gateway)
	}
}

func TestTheGatewayBodyBoundCapsTheSumAcrossKeys(t *testing.T) {
	env := newPublicEnvWith(t, fullResponseHandler(), publicEnvOptions{maxBodyBytes: 1 << 20, budgetUSD: 100, rpm: 1000, maxBodies: 3, maxBodiesPerKey: 2})
	first, _ := insertTestKey(t, env.store, nil)
	second, _ := insertTestKey(t, env.store, nil)
	third, _ := insertTestKey(t, env.store, nil)
	stallBody(t, env, first)
	stallBody(t, env, first)
	stallBody(t, env, second)

	body := newWatchedBody(strings.NewReader(chatBody(false)))
	rec := postBody(env, third, body)
	if detail := decodeError(t, rec); rec.Code != http.StatusTooManyRequests || detail.Message != "too many request bodies in flight; retry shortly" {
		t.Fatalf("a fresh key with every gateway slot held: status %d error %+v, want 429 naming the gateway limit", rec.Code, detail)
	}
	if body.read.Load() {
		t.Error("the refused body was read")
	}
	if got := scrape(t, env.metrics, bodiesInFlight); got != 3 {
		t.Errorf("%s = %v, want 3", bodiesInFlight, got)
	}
	if got, key := scrape(t, env.metrics, bodyRefusals), scrape(t, env.metrics, keyRefusals); got != 1 || key != 0 {
		t.Errorf("refusals: gateway %v key %v, want 1 and 0", got, key)
	}
}

func TestASlowDripReleasesItsSlotAtTheReadDeadline(t *testing.T) {
	env := newPublicEnvWith(t, fullResponseHandler(), publicEnvOptions{maxBodyBytes: 1 << 20, budgetUSD: 100, rpm: 1000, bodyReadTimeout: 200 * time.Millisecond, maxBodies: 1, maxBodiesPerKey: 1})
	auth, _ := insertTestKey(t, env.store, nil)
	srv := httptest.NewServer(env.handler)
	t.Cleanup(srv.Close)

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	body := chatBody(false)
	start := time.Now()
	fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nAuthorization: %s\r\nContent-Length: %d\r\n\r\n", auth, len(body))
	go func() {
		for i := range len(body) - 1 {
			if _, err := conn.Write([]byte{body[i]}); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("a body dripping a byte every 20 ms: status %d, want 408", res.StatusCode)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the drip held its slot for %v against a 200 ms read deadline", elapsed)
	}
	eventually(t, func() bool { return scrape(t, env.metrics, bodiesInFlight) == 0 },
		func() string { return "the dripping request kept its body slot" })
	assertBodySlotFree(t, env, auth)
}

func TestAGatewayRefusalLeavesTheKeysAllowanceIntact(t *testing.T) {
	env := newPublicEnvWith(t, fullResponseHandler(), publicEnvOptions{maxBodyBytes: 1 << 20, budgetUSD: 100, rpm: 1000, maxBodies: 1, maxBodiesPerKey: 1})
	holder, _ := insertTestKey(t, env.store, nil)
	refused, _ := insertTestKey(t, env.store, nil)

	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.CloseWithError(errors.New("test over")) })
	stalled := newWatchedBody(pr)
	held := make(chan int, 1)
	go func() { held <- postBody(env, holder, stalled).Code }()
	waitFor(t, stalled.started, "the stalled body was never read")

	if rec := postBody(env, refused, strings.NewReader(chatBody(false))); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("with the only gateway slot held: status %d, want 429", rec.Code)
	}
	fmt.Fprint(pw, chatBody(false))
	_ = pw.Close()
	if code := <-held; code != http.StatusOK {
		t.Fatalf("the held request once its body arrived: status %d", code)
	}
	if rec := postBody(env, refused, strings.NewReader(chatBody(false))); rec.Code != http.StatusOK {
		t.Fatalf("the once-refused key with the gateway free: status %d body %s", rec.Code, rec.Body.String())
	}
}
