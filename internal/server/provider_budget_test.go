package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnowballSH/modelgate/internal/accounting"
	"github.com/SnowballSH/modelgate/internal/models"
	"github.com/SnowballSH/modelgate/internal/oai"
	"github.com/SnowballSH/modelgate/internal/store"
)

const (
	gptBody       = `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`
	gptStreamBody = `{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`
)

var operatorCeilings = map[string]float64{models.ProviderAnthropic: 175, models.ProviderOpenAI: 50}

func newCeilingEnv(t *testing.T, budgetUSD float64, ceilings map[string]float64, openaiHandler, anthropicHandler http.HandlerFunc) *dualEnv {
	t.Helper()
	return newDualEnvWith(t, dualEnvOptions{budgetUSD: budgetUSD, providerBudgets: ceilings, breakerThreshold: 100}, openaiHandler, anthropicHandler)
}

func bookSpend(t *testing.T, env *dualEnv, keyID, provider string, usd float64) {
	t.Helper()
	model := map[string]string{models.ProviderAnthropic: "claude-sonnet-5", models.ProviderOpenAI: "gpt-5"}[provider]
	usage := store.Usage{InputTokens: int64(usd * 1_000_000)}
	if err := env.acct.Record(t.Context(), env.now, keyID, model, provider, usage, models.Pricing{InputUSDPerMTok: 1}); err != nil {
		t.Fatal(err)
	}
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) oai.ErrorDetail {
	t.Helper()
	var body oai.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return body.Error
}

func TestProviderCeilingRefusesOnlyItsProvider(t *testing.T) {
	for _, tc := range []struct {
		spent, refusedBody, admittedBody string
	}{
		{models.ProviderOpenAI, gptBody, chatBody(false)},
		{models.ProviderAnthropic, chatBody(false), gptBody},
	} {
		t.Run(tc.spent+" at its ceiling", func(t *testing.T) {
			env := newCeilingEnv(t, 225, operatorCeilings, openaiChatResponse(), fullResponseHandler())
			auth, _ := insertTestKey(t, env.store, nil)
			bookSpend(t, env, "earlier", tc.spent, operatorCeilings[tc.spent])

			rec := doDual(env, auth, tc.refusedBody)
			detail := decodeError(t, rec)
			if rec.Code != http.StatusTooManyRequests || detail.Type != "insufficient_quota" || detail.Code != "insufficient_quota" {
				t.Fatalf("refused request: status %d error %+v, want 429 insufficient_quota", rec.Code, detail)
			}
			if want := fmt.Sprintf("monthly %s spend ceiling exhausted", tc.spent); detail.Message != want {
				t.Errorf("message = %q, want %q", detail.Message, want)
			}
			if got := rec.Header().Get("x-should-retry"); got != "false" {
				t.Errorf("x-should-retry = %q, want false", got)
			}
			if got := env.acct.Reserved(); got != 0 {
				t.Errorf("a refused request left USD %v reserved", got)
			}
			outcome := `modelgate_requests_total{model="unknown",outcome="` + CodeProviderBudgetExhausted + `"}`
			if got := scrape(t, env.metrics, outcome); got != 1 {
				t.Errorf("%s = %v, want 1", outcome, got)
			}

			if rec := doDual(env, auth, tc.admittedBody); rec.Code != http.StatusOK {
				t.Fatalf("the other provider: status %d body %s, want 200", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestCapsAboveTheProviderCeilingStillBind(t *testing.T) {
	for name, tc := range map[string]struct {
		budget   float64
		quota    *float64
		spent    float64
		wantCode string
		wantMsg  string
	}{
		"global budget below the ceiling": {budget: 100, spent: 100, wantCode: CodeBudgetExhausted, wantMsg: "monthly budget exhausted"},
		"key quota under the ceiling":     {budget: 225, quota: ptr(1.0), spent: 1, wantCode: CodeQuotaExhausted, wantMsg: "monthly quota exhausted for this key"},
	} {
		t.Run(name, func(t *testing.T) {
			env := newCeilingEnv(t, tc.budget, operatorCeilings, openaiChatResponse(), fullResponseHandler())
			gen := insertKey(t, env.store, env.now, func(k *store.KeyRecord) { k.QuotaUSD = tc.quota })
			auth := bearer(gen.Full)
			bookSpend(t, env, gen.ID, models.ProviderAnthropic, tc.spent)

			rec := doDual(env, auth, chatBody(false))
			if detail := decodeError(t, rec); rec.Code != http.StatusTooManyRequests || detail.Message != tc.wantMsg {
				t.Fatalf("status %d error %+v, want 429 %q", rec.Code, detail, tc.wantMsg)
			}
			if got := scrape(t, env.metrics, fmt.Sprintf(`modelgate_requests_total{model="unknown",outcome=%q}`, tc.wantCode)); got != 1 {
				t.Errorf("outcome %s counted %v times, want 1", tc.wantCode, got)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestParallelRequestsCannotOvershootAProviderCeiling(t *testing.T) {
	upstream, arrivals, release := holding(openaiChatResponse())
	env := newCeilingEnv(t, 225, map[string]float64{models.ProviderOpenAI: 0.0001}, upstream, fullResponseHandler())
	auths := make([]string, 8)
	for i := range auths {
		auths[i], _ = insertTestKey(t, env.store, nil)
	}
	send := func(auth string) int { return doDual(env, auth, gptBody).Code }

	codes := parallelAgainstCap(t, send, arrivals, release, auths)

	if got := arrivals.Load(); got != 1 {
		t.Fatalf("%d of 8 keys reached openai against a ceiling one request exhausts, want 1 (statuses %v)", got, codes)
	}
}

func TestAHeldProviderCeilingLeavesTheOtherProviderAdmitted(t *testing.T) {
	upstream, arrivals, release := holding(openaiChatResponse())
	env := newCeilingEnv(t, 225, map[string]float64{models.ProviderOpenAI: 0.0001}, upstream, fullResponseHandler())
	auth, _ := insertTestKey(t, env.store, nil)

	first := make(chan int, 1)
	go func() { first <- doDual(env, auth, gptBody).Code }()
	eventually(t, func() bool { return arrivals.Load() == 1 }, func() string { return "the openai request never reached the provider" })

	held := doDual(env, auth, gptBody)
	anthropic := doDual(env, auth, chatBody(false))
	close(release)
	if code := <-first; code != http.StatusOK {
		t.Fatalf("held openai request: status %d", code)
	}
	detail := decodeError(t, held)
	if held.Code != http.StatusTooManyRequests || detail.Code != CodeCapReserved {
		t.Errorf("second openai request while the ceiling is held: status %d body %s, want 429 %s", held.Code, held.Body.String(), CodeCapReserved)
	}
	if want := "the remaining monthly quota, budget or provider ceiling is held by requests in flight; retry shortly"; detail.Message != want {
		t.Errorf("cap_reserved message = %q, want %q", detail.Message, want)
	}
	if anthropic.Code != http.StatusOK {
		t.Errorf("anthropic request while openai's ceiling is held: status %d body %s", anthropic.Code, anthropic.Body.String())
	}
}

func truncatedOpenAIStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5-2026-01-01\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"twelve chars\"},\"finish_reason\":null}]}\n\n")
}

func truncatedAnthropicStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"type\":\"message\",\"role\":\"assistant\",\"usage\":{\"input_tokens\":100,\"output_tokens\":1}}}\n\n")
	fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")
	fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", strings.Repeat("a", 40_000))
}

func TestBookedSpendIsAttributedToTheServingProvider(t *testing.T) {
	for name, tc := range map[string]struct {
		openai, anthropic         http.HandlerFunc
		openaiBody, anthropicBody string
		wantOpenAI, wantAnthropic float64
	}{
		"complete answers": {
			openai: openaiChatResponse(), anthropic: fullResponseHandler(),
			openaiBody: gptBody, anthropicBody: chatBody(false),
			wantOpenAI:    120*1.25/1e6 + 40*10.0/1e6 + 80*0.125/1e6,
			wantAnthropic: 100*3/1e6 + 50*15/1e6 + 10*0.30/1e6 + 5*3.75/1e6,
		},
		"cut stream estimates": {
			openai: truncatedOpenAIStream, anthropic: truncatedAnthropicStream,
			openaiBody: gptStreamBody, anthropicBody: chatBody(true),
			wantOpenAI:    float64((len(gptStreamBody)+3)/4)*1.25/1e6 + 3*10.0/1e6,
			wantAnthropic: 100*3.0/1e6 + 10_000*15.0/1e6,
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := newCeilingEnv(t, 225, operatorCeilings, tc.openai, tc.anthropic)
			auth, _ := insertTestKey(t, env.store, nil)
			doDual(env, auth, tc.openaiBody)
			doDual(env, auth, tc.anthropicBody)

			per, err := env.store.MonthSpendPerProvider(t.Context(), accounting.Month(env.now))
			if err != nil {
				t.Fatal(err)
			}
			for provider, want := range map[string]float64{models.ProviderOpenAI: tc.wantOpenAI, models.ProviderAnthropic: tc.wantAnthropic} {
				if math.Abs(per[provider]-want) > 1e-12 {
					t.Errorf("%s spend = %v, want %v (all spend %v)", provider, per[provider], want, per)
				}
				series := fmt.Sprintf(`modelgate_provider_month_spend_usd{provider=%q}`, provider)
				if got := scrape(t, env.metrics, series); math.Abs(got-want) > 1e-12 {
					t.Errorf("%s = %v, want %v", series, got, want)
				}
			}
			if len(per) != 2 {
				t.Errorf("spend attributed to %v, want exactly openai and anthropic", per)
			}
		})
	}
}

// TestEveryExitPathSettlesTheProviderHold runs each exit path against an
// openai ceiling below any booking: a path that booked nothing must leave
// the next request admitted, and one that booked must leave it refused as
// spent, never as held.
func TestEveryExitPathSettlesTheProviderHold(t *testing.T) {
	failing := func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) }
	for name, tc := range map[string]struct {
		upstream http.HandlerFunc
		body     string
		abort    bool
		booked   bool
	}{
		"success":                    {upstream: openaiChatResponse(), body: gptBody, booked: true},
		"upstream error":             {upstream: failing, body: gptBody},
		"cut stream":                 {upstream: truncatedOpenAIStream, body: gptStreamBody, booked: true},
		"client gone, non-streaming": {upstream: openaiChatResponse(), body: gptBody, abort: true, booked: true},
		"client gone, streaming":     {upstream: truncatedOpenAIStream, body: gptStreamBody, abort: true},
	} {
		t.Run(name, func(t *testing.T) {
			var arrivals atomic.Int32
			arrived, proceed := make(chan struct{}), make(chan struct{})
			upstream := func(w http.ResponseWriter, r *http.Request) {
				if arrivals.Add(1) > 1 {
					failing(w, r)
					return
				}
				close(arrived)
				<-proceed
				tc.upstream(w, r)
			}
			env := newCeilingEnv(t, 225, map[string]float64{models.ProviderOpenAI: 0.00001}, upstream, fullResponseHandler())
			auth, _ := insertTestKey(t, env.store, nil)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body))
			req.Header.Set("Authorization", auth)
			done := make(chan struct{})
			go func() {
				defer close(done)
				env.handler.ServeHTTP(httptest.NewRecorder(), req)
			}()
			waitFor(t, arrived, "the request never reached the upstream")
			if tc.abort {
				cancel()
			}
			close(proceed)
			waitFor(t, done, "the handler never returned")
			if got := env.acct.Reserved(); got != 0 {
				t.Fatalf("USD %v still reserved after the request returned", got)
			}

			before := arrivals.Load()
			next := doDual(env, auth, gptBody)
			switch {
			case tc.booked && next.Code == http.StatusTooManyRequests && strings.Contains(decodeError(t, next).Message, "ceiling"):
			case !tc.booked && arrivals.Load() > before:
			default:
				t.Fatalf("next request: status %d body %s, upstream reached %v; want booked=%v settled", next.Code, next.Body.String(), arrivals.Load() > before, tc.booked)
			}
		})
	}
}

func exposition(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func TestProviderGaugesFollowCeilingsAndSpend(t *testing.T) {
	env := newCeilingEnv(t, 225, map[string]float64{models.ProviderAnthropic: 175}, openaiChatResponse(), fullResponseHandler())
	bookSpend(t, env, "k", models.ProviderOpenAI, 2)

	for series, want := range map[string]float64{
		`modelgate_budget_usd`:                                     225,
		`modelgate_month_spend_usd`:                                2,
		`modelgate_provider_budget_usd{provider="anthropic"}`:      175,
		`modelgate_provider_month_spend_usd{provider="anthropic"}`: 0,
		`modelgate_provider_month_spend_usd{provider="openai"}`:    2,
	} {
		if got := scrape(t, env.metrics, series); math.Abs(got-want) > 1e-12 {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	if out := exposition(t, env.metrics); strings.Contains(out, `modelgate_provider_budget_usd{provider="openai"}`) {
		t.Errorf("openai has no ceiling but exports one:\n%s", out)
	}
}

func TestWithoutCeilingsProviderSpendNeverRefusesAndExportsNoCeiling(t *testing.T) {
	env := newCeilingEnv(t, 2000, nil, openaiChatResponse(), fullResponseHandler())
	auth, _ := insertTestKey(t, env.store, nil)
	if out := exposition(t, env.metrics); strings.Contains(out, "modelgate_provider_") {
		t.Errorf("a gateway with no ceilings and no spend exports provider series:\n%s", out)
	}
	bookSpend(t, env, "k", models.ProviderOpenAI, 1000)

	if rec := doDual(env, auth, gptBody); rec.Code != http.StatusOK {
		t.Fatalf("openai with 1000 of a 2000 budget spent and no ceiling: status %d body %s", rec.Code, rec.Body.String())
	}
	out := exposition(t, env.metrics)
	if strings.Contains(out, "modelgate_provider_budget_usd") {
		t.Errorf("a ceiling series without any ceiling configured:\n%s", out)
	}
	for _, series := range []string{"modelgate_budget_usd 2000", `modelgate_provider_month_spend_usd{provider="openai"}`} {
		if !strings.Contains(out, series) {
			t.Errorf("%s absent:\n%s", series, out)
		}
	}
}

func TestAdminUsageReportsProviderCeilings(t *testing.T) {
	s := testStore(t)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	acct := accounting.New(s, 225, map[string]float64{models.ProviderOpenAI: 50})
	if err := acct.Record(t.Context(), now, "k", "claude-sonnet-5", models.ProviderAnthropic, store.Usage{InputTokens: 2_000_000}, models.Pricing{InputUSDPerMTok: 1}); err != nil {
		t.Fatal(err)
	}
	handler := NewAdminHandler(s, acct, testTable(t), NewMetrics(225), testBoundary(t), 225, func() time.Time { return now }, nil, nil)
	rec := doAdmin(&adminEnv{handler: handler}, http.MethodGet, "/api/usage", "alice", "")

	var resp struct {
		BudgetUSD float64 `json:"budget_usd"`
		SpendUSD  float64 `json:"spend_usd"`
		Providers []struct {
			Provider  string   `json:"provider"`
			SpendUSD  float64  `json:"spend_usd"`
			BudgetUSD *float64 `json:"budget_usd"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if resp.BudgetUSD != 225 || resp.SpendUSD != 2 || len(resp.Providers) != 2 {
		t.Fatalf("usage = %s", rec.Body.String())
	}
	anthropic, openai := resp.Providers[0], resp.Providers[1]
	if anthropic.Provider != models.ProviderAnthropic || anthropic.SpendUSD != 2 || anthropic.BudgetUSD != nil {
		t.Errorf("anthropic = %+v, want USD 2 spent and no ceiling", anthropic)
	}
	if openai.Provider != models.ProviderOpenAI || openai.SpendUSD != 0 || openai.BudgetUSD == nil || *openai.BudgetUSD != 50 {
		t.Errorf("openai = %+v, want nothing spent against 50", openai)
	}
}

func TestProviderGaugesReportAFailedReadAsNaN(t *testing.T) {
	s, err := store.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	acct := accounting.New(s, 225, map[string]float64{models.ProviderOpenAI: 50})
	m := NewMetrics(225)
	m.TrackProviderSpend(acct, time.Now)
	s.Close()

	if got := scrape(t, m, `modelgate_provider_budget_usd{provider="openai"}`); got != 50 {
		t.Errorf("openai ceiling with the store unreadable = %v, want 50", got)
	}
	if got := scrape(t, m, `modelgate_provider_month_spend_usd{provider="openai"}`); !math.IsNaN(got) {
		t.Errorf("openai spend with the store unreadable = %v, want NaN", got)
	}
}
