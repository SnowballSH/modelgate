package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SnowballSH/modelgate/internal/oai"
)

type countingReader struct {
	r    io.Reader
	read int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += n
	return n, err
}

func TestUnauthenticatedRequestIsRefusedBeforeTheBodyIsRead(t *testing.T) {
	env := newPublicEnv(t, fullResponseHandler(), 1<<20)
	for name, body := range map[string]string{
		"malformed JSON": `{"model":`,
		"oversized":      strings.Repeat("x", 2<<20),
	} {
		t.Run(name, func(t *testing.T) {
			reader := &countingReader{r: strings.NewReader(body)}
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", reader)
			req.Header.Set("Authorization", "Bearer mg_aaaaaaaa_wrong")
			rec := httptest.NewRecorder()
			env.handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized || decodeErrorCode(t, rec) != CodeInvalidAPIKey {
				t.Fatalf("status %d body %s, want 401 invalid_api_key", rec.Code, rec.Body.String())
			}
			if reader.read != 0 {
				t.Fatalf("read %d body bytes before authenticating", reader.read)
			}
		})
	}
}

func TestOversizedAuthenticatedBodyIsOpenAIShaped413(t *testing.T) {
	env := newPublicEnv(t, fullResponseHandler(), 64)
	auth, _ := insertTestKey(t, env.store, nil)
	rec := doPublic(env, http.MethodPost, "/v1/chat/completions", auth, `{"model":"claude-sonnet-5","messages":[`+strings.Repeat(" ", 128)+`]}`)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	var body oai.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("413 body %q is not an OpenAI error: %v", rec.Body.String(), err)
	}
	if body.Error.Type != "invalid_request_error" || body.Error.Code != CodeRequestTooLarge {
		t.Fatalf("413 error = %+v", body.Error)
	}
}

func TestCapExhaustionIsInsufficientQuota(t *testing.T) {
	keyEnv := newPublicEnv(t, fullResponseHandler(), 1<<20)
	budgetEnv := newPublicEnvWith(t, fullResponseHandler(), publicEnvOptions{maxBodyBytes: 1 << 20, budgetUSD: 0.0001, rpm: 1000})
	budgetAuth, _ := insertTestKey(t, budgetEnv.store, nil)
	if rec := doPublic(budgetEnv, http.MethodPost, "/v1/chat/completions", budgetAuth, chatBody(false)); rec.Code != http.StatusOK {
		t.Fatalf("first request under budget: status %d", rec.Code)
	}

	for name, tc := range map[string]struct {
		env  *publicEnv
		auth string
	}{
		"key quota":      {keyEnv, insertQuotaKey(t, keyEnv, 0)},
		"monthly budget": {budgetEnv, budgetAuth},
	} {
		t.Run(name, func(t *testing.T) {
			rec := doPublic(tc.env, http.MethodPost, "/v1/chat/completions", tc.auth, chatBody(false))
			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429", rec.Code)
			}
			var body oai.ErrorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Type != "insufficient_quota" || body.Error.Code != "insufficient_quota" {
				t.Fatalf("error = %+v, want type and code insufficient_quota", body.Error)
			}
			if got := rec.Header().Get("x-should-retry"); got != "false" {
				t.Errorf("x-should-retry = %q, want false", got)
			}
		})
	}
}

func TestRateLimitedResponseCarriesRetryAfter(t *testing.T) {
	env := newPublicEnvWith(t, fullResponseHandler(), publicEnvOptions{maxBodyBytes: 1 << 20, budgetUSD: 100, rpm: 1})
	auth, _ := insertTestKey(t, env.store, nil)
	if rec := doPublic(env, http.MethodPost, "/v1/chat/completions", auth, chatBody(false)); rec.Code != http.StatusOK {
		t.Fatalf("first request: status %d", rec.Code)
	}
	rec := doPublic(env, http.MethodPost, "/v1/chat/completions", auth, chatBody(false))
	if rec.Code != http.StatusTooManyRequests || decodeErrorCode(t, rec) != CodeRateLimited {
		t.Fatalf("second request: status %d body %s", rec.Code, rec.Body.String())
	}
	seconds, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || seconds < 1 || seconds > 60 {
		t.Fatalf("Retry-After = %q, want whole seconds in [1, 60]", rec.Header().Get("Retry-After"))
	}
}

func TestRequestIDIsEchoedValidatedAndLogged(t *testing.T) {
	logs := captureLogs(t)
	env := newPublicEnv(t, fullResponseHandler(), 1<<20)
	auth, _ := insertTestKey(t, env.store, nil)

	send := func(requestID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody(false)))
		req.Header.Set("Authorization", auth)
		if requestID != "" {
			req.Header.Set("X-Request-ID", requestID)
		}
		rec := httptest.NewRecorder()
		env.handler.ServeHTTP(rec, req)
		return rec
	}

	if got := send("agent-7.run_42:step-3").Header().Get("X-Request-ID"); got != "agent-7.run_42:step-3" {
		t.Errorf("valid X-Request-ID echoed as %q", got)
	}
	for _, bad := range []string{strings.Repeat("a", 129), "has space", "quote\"inject", "new\nline"} {
		got := send(bad).Header().Get("X-Request-ID")
		if got == bad || !validRequestID(got) {
			t.Errorf("invalid X-Request-ID %.20q answered with %q, want a fresh valid id", bad, got)
		}
	}
	generated := send("").Header().Get("X-Request-ID")
	if !validRequestID(generated) {
		t.Fatalf("generated X-Request-ID %q is not valid", generated)
	}

	records := requestLogRecords(t, logs)
	last := records[len(records)-1]
	if last["request_id"] != generated {
		t.Errorf("request log request_id = %v, want %q", last["request_id"], generated)
	}
	if _, ok := last["duration_ms"].(float64); !ok {
		t.Errorf("request log lacks a numeric duration_ms: %v", last)
	}
	if last["version"] != "test-build" {
		t.Errorf("request log version = %v, want test-build", last["version"])
	}
	if strings.Contains(logs.String(), "quote\\\"inject") {
		t.Error("an invalid client request id reached the log")
	}
}

func TestStalledBodyIsAnsweredAndACompleteBodyIsNotTimed(t *testing.T) {
	arrived := make(chan struct{}, 1)
	full := fullResponseHandler()
	env := newPublicEnvWith(t, func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		time.Sleep(400 * time.Millisecond)
		full(w, r)
	}, publicEnvOptions{maxBodyBytes: 1 << 20, budgetUSD: 100, rpm: 1000, bodyReadTimeout: 150 * time.Millisecond})
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
		t.Fatalf("a body that stalls: status %d, want 408", res.StatusCode)
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", auth)
	res, err = srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var answer oai.ChatResponse
	if err := json.NewDecoder(res.Body).Decode(&answer); err != nil || res.StatusCode != http.StatusOK || len(answer.Choices) != 1 {
		t.Fatalf("an upstream call outlasting the body read timeout: status %d, decode %v, answer %+v; want the full 200 answer", res.StatusCode, err, answer)
	}
	select {
	case <-arrived:
	default:
		t.Fatal("the upstream was never called")
	}
}
