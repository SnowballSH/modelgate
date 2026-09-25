package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SnowballSH/modelgate/internal/accounting"
	"github.com/SnowballSH/modelgate/internal/models"
	"github.com/SnowballSH/modelgate/internal/provider"
	"github.com/SnowballSH/modelgate/internal/store"
)

func scrape(t *testing.T, m *Metrics, series string) float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for line := range strings.Lines(rec.Body.String()) {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), series+" "); ok {
			v, err := strconv.ParseFloat(rest, 64)
			if err != nil {
				t.Fatalf("%s value %q: %v", series, rest, err)
			}
			return v
		}
	}
	t.Fatalf("series %s absent from:\n%s", series, rec.Body.String())
	return 0
}

func TestMonthSpendGaugeFollowsTheCalendar(t *testing.T) {
	s := testStore(t)
	clk := &clock{t: time.Date(2026, 8, 31, 23, 59, 0, 0, time.UTC)}
	pricing := models.Pricing{InputUSDPerMTok: 1}
	if err := accounting.New(s, 100).Record(t.Context(), clk.t, "k", "m", store.Usage{InputTokens: 2_000_000}, pricing); err != nil {
		t.Fatal(err)
	}
	m := NewMetrics(100)
	m.TrackMonthSpend(s, clk.now)

	if got := scrape(t, m, "modelgate_month_spend_usd"); got != 2 {
		t.Fatalf("August spend = %v, want 2", got)
	}
	clk.advance(2 * time.Minute)
	if got := scrape(t, m, "modelgate_month_spend_usd"); got != 0 {
		t.Fatalf("spend after rolling into September with no booking = %v, want 0", got)
	}
}

func TestBreakerGaugeClosesWhenTheCooldownEnds(t *testing.T) {
	clk := &clock{t: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)}
	b := provider.NewBreaker(1, 30*time.Second, clk.now)
	m := NewMetrics(100)
	m.TrackBreaker(models.ProviderAnthropic, b)
	series := `modelgate_breaker_open{provider="anthropic"}`

	if got := scrape(t, m, series); got != 0 {
		t.Fatalf("fresh breaker gauge = %v, want 0", got)
	}
	b.Record(provider.ErrUnavailable)
	if got := scrape(t, m, series); got != 1 {
		t.Fatalf("tripped breaker gauge = %v, want 1", got)
	}
	clk.advance(31 * time.Second)
	if got := scrape(t, m, series); got != 0 {
		t.Fatalf("breaker gauge after the cooldown, with no request since = %v, want 0", got)
	}
}

func TestBookingFailureIsCounted(t *testing.T) {
	var env *publicEnv
	full := fullResponseHandler()
	env = newPublicEnv(t, func(w http.ResponseWriter, r *http.Request) {
		env.store.Close()
		full(w, r)
	}, 1<<20)
	auth, _ := insertTestKey(t, env.store, nil)

	if got := scrape(t, env.metrics, "modelgate_usage_booking_failures_total"); got != 0 {
		t.Fatalf("booking failures before any request = %v", got)
	}
	doPublic(env, http.MethodPost, "/v1/chat/completions", auth, chatBody(false))
	if got := scrape(t, env.metrics, "modelgate_usage_booking_failures_total"); got != 1 {
		t.Fatalf("booking failures after the store refused a booking = %v, want 1", got)
	}
}

type fakeReadiness struct{ pingErr, writeErr error }

func (f fakeReadiness) Ping(context.Context) error                  { return f.pingErr }
func (f fakeReadiness) ProbeWrite(context.Context, time.Time) error { return f.writeErr }

func TestReadyFailsWhenTheStoreIsNotWritable(t *testing.T) {
	cases := map[string]struct {
		store    fakeReadiness
		wantCode int
		wantBody string
	}{
		"healthy":      {fakeReadiness{}, http.StatusOK, "ready"},
		"unreachable":  {fakeReadiness{pingErr: errors.New("gone")}, http.StatusServiceUnavailable, "store unreachable"},
		"not writable": {fakeReadiness{writeErr: errors.New("attempt to write a readonly database")}, http.StatusServiceUnavailable, "store not writable"},
	}
	for name, tc := range cases {
		rec := httptest.NewRecorder()
		NewReadyHandler(tc.store, time.Now).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
		if rec.Code != tc.wantCode || !strings.Contains(rec.Body.String(), tc.wantBody) {
			t.Errorf("%s: %d %q, want %d %q", name, rec.Code, rec.Body.String(), tc.wantCode, tc.wantBody)
		}
	}
}
