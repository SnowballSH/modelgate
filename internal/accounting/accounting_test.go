package accounting

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/SnowballSH/modelgate/internal/models"
	"github.com/SnowballSH/modelgate/internal/store"
)

const epsilon = 1e-9

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func quotaKey(id string, quota *float64) store.KeyRecord {
	return store.KeyRecord{ID: id, QuotaUSD: quota}
}

func ptr(v float64) *float64 { return &v }

func TestMonth(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
		want string
	}{
		{"end of january utc", time.Date(2026, 1, 31, 23, 59, 59, 0, time.UTC), "2026-01"},
		{"start of february utc", time.Date(2026, 2, 1, 0, 0, 1, 0, time.UTC), "2026-02"},
		{"non-utc crossing month boundary", time.Date(2026, 1, 31, 19, 0, 0, 0, time.FixedZone("MST", -7*3600)), "2026-02"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Month(tc.in); got != tc.want {
				t.Errorf("Month(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCost(t *testing.T) {
	u := store.Usage{
		InputTokens:      1_000_000,
		OutputTokens:     500_000,
		CacheReadTokens:  2_000_000,
		CacheWriteTokens: 100_000,
	}
	p := models.Pricing{
		InputUSDPerMTok:      3,
		OutputUSDPerMTok:     15,
		CacheReadUSDPerMTok:  0.30,
		CacheWriteUSDPerMTok: 3.75,
	}
	got := Cost(u, p)
	want := 11.475
	if math.Abs(got-want) > epsilon {
		t.Errorf("Cost = %v, want %v", got, want)
	}
}

func TestGlobalBudgetTripsAtCap(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	a := New(s, 10.0)
	now := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)

	p := models.Pricing{InputUSDPerMTok: 1}
	if err := a.Record(ctx, now, "k1", "m", store.Usage{InputTokens: 9_990_000}, p); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := a.CheckGlobalBudget(ctx, now); err != nil {
		t.Fatalf("budget under cap: got %v, want nil", err)
	}
	if err := a.Record(ctx, now, "k1", "m", store.Usage{InputTokens: 10_000}, p); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := a.CheckGlobalBudget(ctx, now); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("budget at cap: got %v, want ErrBudgetExhausted", err)
	}
}

func TestQuotaIndependentOfBudget(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	a := New(s, 100.0)
	now := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)

	p := models.Pricing{InputUSDPerMTok: 1}
	if err := a.Record(ctx, now, "keyA", "m", store.Usage{InputTokens: 2_000_000}, p); err != nil {
		t.Fatalf("record: %v", err)
	}

	if err := a.CheckKeyQuota(ctx, now, quotaKey("keyA", ptr(1.0))); !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("keyA quota: got %v, want ErrQuotaExhausted", err)
	}
	if err := a.CheckGlobalBudget(ctx, now); err != nil {
		t.Fatalf("global budget: got %v, want nil", err)
	}
	if err := a.CheckKeyQuota(ctx, now, quotaKey("keyB", ptr(5.0))); err != nil {
		t.Fatalf("keyB quota: got %v, want nil", err)
	}
}

func TestNoQuotaKeyNeverTrips(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	a := New(s, 1e12)
	now := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)

	p := models.Pricing{InputUSDPerMTok: 1000}
	if err := a.Record(ctx, now, "keyC", "m", store.Usage{InputTokens: 1_000_000_000}, p); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := a.CheckKeyQuota(ctx, now, quotaKey("keyC", nil)); err != nil {
		t.Fatalf("nil quota: got %v, want nil", err)
	}
}

func TestMonthIsolation(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	a := New(s, 5.0)
	january := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	february := time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC)

	p := models.Pricing{InputUSDPerMTok: 1}
	if err := a.Record(ctx, january, "keyA", "m", store.Usage{InputTokens: 9_000_000}, p); err != nil {
		t.Fatalf("record: %v", err)
	}

	if err := a.CheckGlobalBudget(ctx, january); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("january budget: got %v, want ErrBudgetExhausted", err)
	}
	if err := a.CheckGlobalBudget(ctx, february); err != nil {
		t.Fatalf("february budget: got %v, want nil", err)
	}
	if err := a.CheckKeyQuota(ctx, february, quotaKey("keyA", ptr(1.0))); err != nil {
		t.Fatalf("february quota: got %v, want nil", err)
	}
}

func TestRecordSetsCostAndAccumulates(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	a := New(s, 1000.0)
	now := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	p := models.Pricing{
		InputUSDPerMTok:      3,
		OutputUSDPerMTok:     15,
		CacheReadUSDPerMTok:  0.30,
		CacheWriteUSDPerMTok: 3.75,
	}
	u1 := store.Usage{InputTokens: 1_000_000, OutputTokens: 500_000}
	u2 := store.Usage{InputTokens: 200_000, CacheReadTokens: 1_000_000, CacheWriteTokens: 400_000}

	if err := a.Record(ctx, now, "keyA", "m", u1, p); err != nil {
		t.Fatalf("record u1: %v", err)
	}
	if err := a.Record(ctx, now, "keyA", "m", u2, p); err != nil {
		t.Fatalf("record u2: %v", err)
	}

	spend, err := s.MonthSpend(ctx, Month(now))
	if err != nil {
		t.Fatalf("month spend: %v", err)
	}
	want := Cost(u1, p) + Cost(u2, p)
	if math.Abs(spend-want) > epsilon {
		t.Errorf("MonthSpend = %v, want %v", spend, want)
	}

	usage, err := s.MonthUsage(ctx, Month(now))
	if err != nil {
		t.Fatalf("month usage: %v", err)
	}
	if got := usage["keyA"].Requests; got != 2 {
		t.Errorf("Requests = %d, want 2 (each Record defaults to 1)", got)
	}
}
