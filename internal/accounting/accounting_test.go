package accounting

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/SnowballSH/modelgate/internal/models"
	"github.com/SnowballSH/modelgate/internal/store"
)

const epsilon = 1e-9

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir(), nil)
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

func admit(ctx context.Context, a *Accountant, now time.Time, key store.KeyRecord) error {
	r, err := a.Reserve(ctx, now, key, models.ProviderAnthropic, 0)
	if r != nil {
		r.Release()
	}
	return err
}

var noQuota = quotaKey("unlimited", nil)

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
	a := New(s, 10.0, nil)
	now := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)

	p := models.Pricing{InputUSDPerMTok: 1}
	if err := a.Record(ctx, now, "k1", "m", models.ProviderAnthropic, store.Usage{InputTokens: 9_990_000}, p); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := admit(ctx, a, now, noQuota); err != nil {
		t.Fatalf("budget under cap: got %v, want nil", err)
	}
	if err := a.Record(ctx, now, "k1", "m", models.ProviderAnthropic, store.Usage{InputTokens: 10_000}, p); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := admit(ctx, a, now, noQuota); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("budget at cap: got %v, want ErrBudgetExhausted", err)
	}
}

func TestQuotaIndependentOfBudget(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	a := New(s, 100.0, nil)
	now := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)

	p := models.Pricing{InputUSDPerMTok: 1}
	if err := a.Record(ctx, now, "keyA", "m", models.ProviderAnthropic, store.Usage{InputTokens: 2_000_000}, p); err != nil {
		t.Fatalf("record: %v", err)
	}

	if err := admit(ctx, a, now, quotaKey("keyA", ptr(1.0))); !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("keyA quota: got %v, want ErrQuotaExhausted", err)
	}
	if err := admit(ctx, a, now, noQuota); err != nil {
		t.Fatalf("global budget: got %v, want nil", err)
	}
	if err := admit(ctx, a, now, quotaKey("keyB", ptr(5.0))); err != nil {
		t.Fatalf("keyB quota: got %v, want nil", err)
	}
}

func TestNoQuotaKeyNeverTrips(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	a := New(s, 1e12, nil)
	now := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)

	p := models.Pricing{InputUSDPerMTok: 1000}
	if err := a.Record(ctx, now, "keyC", "m", models.ProviderAnthropic, store.Usage{InputTokens: 1_000_000_000}, p); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := admit(ctx, a, now, quotaKey("keyC", nil)); err != nil {
		t.Fatalf("nil quota: got %v, want nil", err)
	}
}

func TestMonthIsolation(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	a := New(s, 5.0, nil)
	january := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	february := time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC)

	p := models.Pricing{InputUSDPerMTok: 1}
	if err := a.Record(ctx, january, "keyA", "m", models.ProviderAnthropic, store.Usage{InputTokens: 9_000_000}, p); err != nil {
		t.Fatalf("record: %v", err)
	}

	if err := admit(ctx, a, january, noQuota); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("january budget: got %v, want ErrBudgetExhausted", err)
	}
	if err := admit(ctx, a, february, noQuota); err != nil {
		t.Fatalf("february budget: got %v, want nil", err)
	}
	if err := admit(ctx, a, february, quotaKey("keyA", ptr(1.0))); err != nil {
		t.Fatalf("february quota: got %v, want nil", err)
	}
}

func TestRecordSetsCostAndAccumulates(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	a := New(s, 1000.0, nil)
	now := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	p := models.Pricing{
		InputUSDPerMTok:      3,
		OutputUSDPerMTok:     15,
		CacheReadUSDPerMTok:  0.30,
		CacheWriteUSDPerMTok: 3.75,
	}
	u1 := store.Usage{InputTokens: 1_000_000, OutputTokens: 500_000}
	u2 := store.Usage{InputTokens: 200_000, CacheReadTokens: 1_000_000, CacheWriteTokens: 400_000}

	if err := a.Record(ctx, now, "keyA", "m", models.ProviderAnthropic, u1, p); err != nil {
		t.Fatalf("record u1: %v", err)
	}
	if err := a.Record(ctx, now, "keyA", "m", models.ProviderAnthropic, u2, p); err != nil {
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

func TestReservationsHoldTheCapUntilReleased(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	a := New(s, 1.0, nil)
	now := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	key := quotaKey("keyA", ptr(0.5))

	first, err := a.Reserve(ctx, now, key, models.ProviderAnthropic, 0.3)
	if err != nil {
		t.Fatalf("first reservation: %v", err)
	}
	second, err := a.Reserve(ctx, now, key, models.ProviderAnthropic, 0.3)
	if err != nil {
		t.Fatalf("second reservation while 0.3 of 0.5 is held: %v", err)
	}
	if _, err := a.Reserve(ctx, now, key, models.ProviderAnthropic, 0.3); !errors.Is(err, ErrCapReserved) {
		t.Fatalf("third reservation with 0.6 held against 0.5 and nothing spent: got %v, want ErrCapReserved", err)
	}
	other := quotaKey("keyB", nil)
	otherRes, err := a.Reserve(ctx, now, other, models.ProviderAnthropic, 0.5)
	if err != nil {
		t.Fatalf("other key under the budget: %v", err)
	}
	if _, err := a.Reserve(ctx, now, other, models.ProviderAnthropic, 0.1); !errors.Is(err, ErrCapReserved) {
		t.Fatalf("budget with 1.1 held against 1.0 and nothing spent: got %v, want ErrCapReserved", err)
	}

	first.Release()
	first.Release()
	if got := a.Reserved(); math.Abs(got-0.8) > epsilon {
		t.Fatalf("Reserved after one release (twice) = %v, want 0.8", got)
	}
	second.Release()
	otherRes.Release()
	if got := a.Reserved(); got != 0 {
		t.Fatalf("Reserved after every release = %v, want exactly 0", got)
	}
	if err := admit(ctx, a, now, key); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

func TestRecordedSpendAtACapIsExhaustionEvenWhileReservationsAreHeld(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	p := models.Pricing{InputUSDPerMTok: 1}

	for name, tc := range map[string]struct {
		budget float64
		quota  *float64
		hold   bool
		spent  int64
		want   error
	}{
		"key quota spent":                {budget: 100, quota: ptr(1.0), spent: 1_000_000, want: ErrQuotaExhausted},
		"key quota spent while held":     {budget: 100, quota: ptr(1.0), hold: true, spent: 1_000_000, want: ErrQuotaExhausted},
		"budget spent while held":        {budget: 1.0, hold: true, spent: 1_000_000, want: ErrBudgetExhausted},
		"budget spent, key quota held":   {budget: 1.0, quota: ptr(1.2), hold: true, spent: 1_000_000, want: ErrBudgetExhausted},
		"key quota held, nothing spent":  {budget: 100, quota: ptr(0.5), hold: true, want: ErrCapReserved},
		"budget held, part spent":        {budget: 0.6, hold: true, spent: 100_000, want: ErrCapReserved},
		"under both caps with room left": {budget: 2.0, quota: ptr(2.0), hold: true, spent: 100_000, want: nil},
	} {
		t.Run(name, func(t *testing.T) {
			a := New(newStore(t), tc.budget, nil)
			key := quotaKey("keyA", tc.quota)
			if tc.hold {
				held, err := a.Reserve(ctx, now, key, models.ProviderAnthropic, 0.5)
				if err != nil {
					t.Fatalf("reservation before any spend: %v", err)
				}
				defer held.Release()
			}
			if err := a.Record(ctx, now, key.ID, "m", models.ProviderAnthropic, store.Usage{InputTokens: tc.spent}, p); err != nil {
				t.Fatalf("record: %v", err)
			}
			r, err := a.Reserve(ctx, now, key, models.ProviderAnthropic, 0.5)
			if r != nil {
				r.Release()
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestReserveRefusesABoundThatIsNegativeOrNotANumber(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	for name, bound := range map[string]float64{"negative": -11.84, "NaN": math.NaN()} {
		t.Run(name, func(t *testing.T) {
			a := New(newStore(t), 100, nil)
			r, err := a.Reserve(ctx, now, noQuota, models.ProviderAnthropic, bound)
			if !errors.Is(err, ErrInvalidBound) || r != nil {
				t.Fatalf("Reserve(%v) = %v, %v; want ErrInvalidBound", bound, r, err)
			}
			if held := a.Reserved(); held != 0 {
				t.Errorf("a refused bound left USD %v reserved", held)
			}
		})
	}
}

var operatorCeilings = map[string]float64{models.ProviderAnthropic: 175, models.ProviderOpenAI: 50}

func spendUSD(usd float64) store.Usage {
	return store.Usage{InputTokens: int64(usd * 1_000_000)}
}

var dollarPerMTok = models.Pricing{InputUSDPerMTok: 1}

func TestProviderCeilingsAdmitUnderTheSameRuleAsTheBudget(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	anthropic, openai := models.ProviderAnthropic, models.ProviderOpenAI

	for name, tc := range map[string]struct {
		budget   float64
		ceilings map[string]float64
		quota    *float64
		spent    map[string]float64
		held     map[string]float64
		provider string
		want     error
	}{
		"openai spent to its ceiling":             {budget: 225, ceilings: operatorCeilings, spent: map[string]float64{openai: 50}, provider: openai, want: ErrProviderBudgetExhausted},
		"anthropic unaffected by openai's":        {budget: 225, ceilings: operatorCeilings, spent: map[string]float64{openai: 50}, provider: anthropic},
		"anthropic spent to its ceiling":          {budget: 225, ceilings: operatorCeilings, spent: map[string]float64{anthropic: 175}, provider: anthropic, want: ErrProviderBudgetExhausted},
		"openai unaffected by anthropic's":        {budget: 225, ceilings: operatorCeilings, spent: map[string]float64{anthropic: 175}, provider: openai},
		"openai ceiling held by a request":        {budget: 225, ceilings: operatorCeilings, spent: map[string]float64{openai: 49}, held: map[string]float64{openai: 1}, provider: openai, want: ErrCapReserved},
		"anthropic hold leaves openai alone":      {budget: 225, ceilings: operatorCeilings, held: map[string]float64{anthropic: 175}, provider: openai},
		"one bound may overshoot the ceiling":     {budget: 225, ceilings: operatorCeilings, spent: map[string]float64{openai: 49.99}, provider: openai},
		"global budget below the ceiling binds":   {budget: 100, ceilings: operatorCeilings, spent: map[string]float64{anthropic: 100}, provider: anthropic, want: ErrBudgetExhausted},
		"global budget refuses before a ceiling":  {budget: 50, ceilings: operatorCeilings, spent: map[string]float64{openai: 50}, provider: openai, want: ErrBudgetExhausted},
		"key quota refuses before a ceiling":      {budget: 225, ceilings: operatorCeilings, quota: ptr(50), spent: map[string]float64{openai: 50}, provider: openai, want: ErrQuotaExhausted},
		"key quota binds under the ceiling":       {budget: 225, ceilings: operatorCeilings, quota: ptr(1), spent: map[string]float64{anthropic: 1}, provider: openai, want: ErrQuotaExhausted},
		"no ceiling: provider spend never binds":  {budget: 2000, spent: map[string]float64{openai: 1000}, provider: openai},
		"no ceiling for this provider":            {budget: 2000, ceilings: map[string]float64{anthropic: 1}, spent: map[string]float64{openai: 1000}, provider: openai},
		"unattributed spend counts only globally": {budget: 225, ceilings: operatorCeilings, spent: map[string]float64{store.Unattributed: 100}, provider: openai},
	} {
		t.Run(name, func(t *testing.T) {
			a := New(newStore(t), tc.budget, tc.ceilings)
			key := quotaKey("keyA", tc.quota)
			for provider, usd := range tc.spent {
				if err := a.Record(ctx, now, key.ID, "m-"+provider, provider, spendUSD(usd), dollarPerMTok); err != nil {
					t.Fatal(err)
				}
			}
			for provider, usd := range tc.held {
				held, err := a.Reserve(ctx, now, noQuota, provider, usd)
				if err != nil {
					t.Fatalf("hold %v on %s: %v", usd, provider, err)
				}
				defer held.Release()
			}
			r, err := a.Reserve(ctx, now, key, tc.provider, 5)
			if r != nil {
				r.Release()
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Reserve on %s = %v, want %v", tc.provider, err, tc.want)
			}
		})
	}
}

func TestReleaseFreesTheProviderCeiling(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	a := New(newStore(t), 225, operatorCeilings)

	first, err := a.Reserve(ctx, now, noQuota, models.ProviderOpenAI, 50)
	if err != nil {
		t.Fatalf("first reservation: %v", err)
	}
	if _, err := a.Reserve(ctx, now, noQuota, models.ProviderOpenAI, 0); !errors.Is(err, ErrCapReserved) {
		t.Fatalf("with the openai ceiling held in full: got %v, want ErrCapReserved", err)
	}
	if err := a.Record(ctx, now, "unlimited", "gpt-5", models.ProviderOpenAI, spendUSD(10), dollarPerMTok); err != nil {
		t.Fatal(err)
	}
	first.Release()
	first.Release()
	if len(a.byProvider) != 0 || a.Reserved() != 0 {
		t.Fatalf("after release: provider holds %v, global %v; want none", a.byProvider, a.Reserved())
	}
	second, err := a.Reserve(ctx, now, noQuota, models.ProviderOpenAI, 40)
	if err != nil {
		t.Fatalf("after release with 10 of 50 spent: %v", err)
	}
	defer second.Release()
	if _, err := a.Reserve(ctx, now, noQuota, models.ProviderOpenAI, 0); !errors.Is(err, ErrCapReserved) {
		t.Fatalf("10 spent plus 40 held against 50: got %v, want ErrCapReserved", err)
	}
}

func TestProviderSpendSurvivesARestartAndAModelLeavingTheTable(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	s, err := store.Open(dir, map[string]string{"gpt-5": models.ProviderOpenAI})
	if err != nil {
		t.Fatal(err)
	}
	if err := New(s, 225, operatorCeilings).Record(ctx, now, "unlimited", "gpt-5", models.ProviderOpenAI, spendUSD(50), dollarPerMTok); err != nil {
		t.Fatal(err)
	}
	s.Close()

	reopened, err := store.Open(dir, map[string]string{"claude-sonnet-5": models.ProviderAnthropic})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	a := New(reopened, 225, operatorCeilings)
	if err := admitOn(ctx, a, now, models.ProviderOpenAI); !errors.Is(err, ErrProviderBudgetExhausted) {
		t.Fatalf("openai after a restart without gpt-5 in the table: got %v, want ErrProviderBudgetExhausted", err)
	}
	if err := admitOn(ctx, a, now, models.ProviderAnthropic); err != nil {
		t.Fatalf("anthropic after the restart: %v", err)
	}
}

func admitOn(ctx context.Context, a *Accountant, now time.Time, provider string) error {
	r, err := a.Reserve(ctx, now, noQuota, provider, 0)
	if r != nil {
		r.Release()
	}
	return err
}

func TestProviderSpendReportsCeilingsAndAttributedSpend(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		ceilings map[string]float64
		spent    map[string]float64
		want     []ProviderSpend
	}{
		"nothing configured or spent": {},
		"spend without ceilings": {
			spent: map[string]float64{models.ProviderOpenAI: 2, store.Unattributed: 1},
			want:  []ProviderSpend{{Provider: models.ProviderOpenAI, SpendUSD: 2}},
		},
		"ceilings without spend": {
			ceilings: operatorCeilings,
			want: []ProviderSpend{
				{Provider: models.ProviderAnthropic, BudgetUSD: ptr(175)},
				{Provider: models.ProviderOpenAI, BudgetUSD: ptr(50)},
			},
		},
		"one ceiling, both spent": {
			ceilings: map[string]float64{models.ProviderAnthropic: 175},
			spent:    map[string]float64{models.ProviderAnthropic: 3, models.ProviderOpenAI: 4},
			want: []ProviderSpend{
				{Provider: models.ProviderAnthropic, SpendUSD: 3, BudgetUSD: ptr(175)},
				{Provider: models.ProviderOpenAI, SpendUSD: 4},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			a := New(newStore(t), 1000, tc.ceilings)
			for provider, usd := range tc.spent {
				if err := a.Record(ctx, now, "k", "m-"+provider, provider, spendUSD(usd), dollarPerMTok); err != nil {
					t.Fatal(err)
				}
			}
			got, err := a.ProviderSpend(ctx, now)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.EqualFunc(got, tc.want, ProviderSpend.equal) {
				t.Fatalf("ProviderSpend = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func (p ProviderSpend) equal(q ProviderSpend) bool {
	sameBudget := (p.BudgetUSD == nil) == (q.BudgetUSD == nil) && (p.BudgetUSD == nil || *p.BudgetUSD == *q.BudgetUSD)
	return p.Provider == q.Provider && math.Abs(p.SpendUSD-q.SpendUSD) < epsilon && sameBudget
}

type countingStore struct {
	spendStore
	byProvider int
}

func (c *countingStore) MonthSpendByProvider(ctx context.Context, month, provider string) (float64, error) {
	c.byProvider++
	return c.spendStore.MonthSpendByProvider(ctx, month, provider)
}

func TestOnlyACeilingCostsAProviderSpendQuery(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		ceilings map[string]float64
		want     int
	}{
		"no ceilings":            {want: 0},
		"anthropic ceiling only": {ceilings: map[string]float64{models.ProviderAnthropic: 175}, want: 1},
	} {
		t.Run(name, func(t *testing.T) {
			a := New(newStore(t), 225, tc.ceilings)
			spy := &countingStore{spendStore: a.store}
			a.store = spy
			for _, provider := range models.Providers {
				if err := admitOn(ctx, a, now, provider); err != nil {
					t.Fatalf("%s: %v", provider, err)
				}
			}
			if spy.byProvider != tc.want {
				t.Fatalf("MonthSpendByProvider ran %d times for one request per provider, want %d", spy.byProvider, tc.want)
			}
		})
	}
}
