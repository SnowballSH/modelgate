package accounting

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/SnowballSH/modelgate/internal/models"
	"github.com/SnowballSH/modelgate/internal/store"
)

var (
	ErrQuotaExhausted  = errors.New("per-key quota exhausted")
	ErrBudgetExhausted = errors.New("global budget exhausted")
	// ErrCapReserved refuses a request whose quota, budget or provider ceiling
	// is not yet spent but is fully held by in-flight reservations; it clears
	// as they finish.
	ErrCapReserved = errors.New("remaining quota, budget or provider ceiling held by in-flight requests")
	// ErrInvalidBound is an invariant violation: a request's worst-case
	// cost is never negative, so a negative or non-numeric bound means a
	// size computation upstream of admission is wrong.
	ErrInvalidBound            = errors.New("worst-case bound is negative or not a number")
	ErrProviderBudgetExhausted = errors.New("provider monthly ceiling exhausted")
)

func Month(t time.Time) string {
	return t.UTC().Format("2006-01")
}

func Cost(u store.Usage, p models.Pricing) float64 {
	return (float64(u.InputTokens)*p.InputUSDPerMTok+
		float64(u.OutputTokens)*p.OutputUSDPerMTok+
		float64(u.CacheReadTokens)*p.CacheReadUSDPerMTok+
		float64(u.CacheWriteTokens)*p.CacheWriteUSDPerMTok)/1e6 +
		float64(u.WebSearchRequests)*p.USDPerSearch
}

// Demand is the most a request can consume: every prompt token, every output
// token and every provider-native web search the request allows.
type Demand struct {
	InputTokens, OutputTokens int64
	WebSearches               int64
}

// WorstCaseCost prices a demand as if every prompt token were billed at the
// dearest input rate, since the provider decides which prompt tokens are
// cache reads, cache writes or plain input.
func WorstCaseCost(d Demand, p models.Pricing) float64 {
	inputRate := max(p.InputUSDPerMTok, p.CacheReadUSDPerMTok, p.CacheWriteUSDPerMTok)
	return (float64(d.InputTokens)*inputRate+float64(d.OutputTokens)*p.OutputUSDPerMTok)/1e6 +
		float64(d.WebSearches)*p.USDPerSearch
}

// spendStore is the part of the store the accountant reads and books.
type spendStore interface {
	MonthSpend(ctx context.Context, month string) (float64, error)
	MonthSpendByKey(ctx context.Context, month, keyID string) (float64, error)
	MonthSpendByProvider(ctx context.Context, month, provider string) (float64, error)
	MonthSpendPerProvider(ctx context.Context, month string) (map[string]float64, error)
	AddUsage(ctx context.Context, month, keyID, model, provider string, u store.Usage) error
}

type Accountant struct {
	store           spendStore
	budget          float64
	providerBudgets map[string]float64

	mu         sync.Mutex
	inFlight   reserved
	byKey      holds
	byProvider holds
}

type reserved struct {
	count  int
	amount float64
}

func (r *reserved) add(amount float64) {
	r.count++
	r.amount += amount
}

func (r *reserved) remove(amount float64) {
	r.count--
	r.amount -= amount
	if r.count == 0 {
		r.amount = 0
	}
}

// holds tallies reservations per key or per provider, keeping an entry
// only while a reservation holds it.
type holds map[string]*reserved

func (h holds) amount(id string) float64 {
	if r, ok := h[id]; ok {
		return r.amount
	}
	return 0
}

func (h holds) add(id string, amount float64) {
	r, ok := h[id]
	if !ok {
		r = &reserved{}
		h[id] = r
	}
	r.add(amount)
}

func (h holds) remove(id string, amount float64) {
	r := h[id]
	r.remove(amount)
	if r.count == 0 {
		delete(h, id)
	}
}

// New caps all spend at budgetUSD and each provider in providerBudgetsUSD
// at its own ceiling; a provider without one is capped only by the budget.
func New(s *store.Store, budgetUSD float64, providerBudgetsUSD map[string]float64) *Accountant {
	return &Accountant{
		store:           s,
		budget:          budgetUSD,
		providerBudgets: maps.Clone(providerBudgetsUSD),
		byKey:           holds{},
		byProvider:      holds{},
	}
}

// Reservation holds a request's worst-case cost against its key quota, its
// provider's ceiling and the monthly budget until the request's real usage
// is booked.
type Reservation struct {
	release func()
}

func (r *Reservation) Release() { r.release() }

// Reserve admits a request while the recorded spend plus every in-flight
// reservation stays below the key quota, the monthly budget and the
// ceiling of the provider serving it, then holds bound against all three.
// Concurrent requests therefore overshoot a cap by at most one request's
// bound. The check reads the store under the same lock that releases
// reservations, and callers book usage, attributed to the same provider,
// before releasing, so a request's cost is always counted either as a
// reservation or as spend.
//
// Recorded spend at a cap is exhaustion, which only the next month or an
// operator clears, checked in the order key quota, budget, provider
// ceiling; a cap reached only by adding reservations is ErrCapReserved,
// which clears as the requests holding them finish.
func (a *Accountant) Reserve(ctx context.Context, now time.Time, key store.KeyRecord, provider string, bound float64) (*Reservation, error) {
	if !(bound >= 0) {
		return nil, ErrInvalidBound
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	month := Month(now)
	var keySpend float64
	if key.QuotaUSD != nil {
		var err error
		if keySpend, err = a.store.MonthSpendByKey(ctx, month, key.ID); err != nil {
			return nil, fmt.Errorf("check quota for key %s: %w", key.ID, err)
		}
		if keySpend >= *key.QuotaUSD {
			return nil, ErrQuotaExhausted
		}
	}
	spend, err := a.store.MonthSpend(ctx, month)
	if err != nil {
		return nil, fmt.Errorf("check global budget: %w", err)
	}
	if spend >= a.budget {
		return nil, ErrBudgetExhausted
	}
	ceiling, capped := a.providerBudgets[provider]
	var providerSpend float64
	if capped {
		if providerSpend, err = a.store.MonthSpendByProvider(ctx, month, provider); err != nil {
			return nil, fmt.Errorf("check %s ceiling: %w", provider, err)
		}
		if providerSpend >= ceiling {
			return nil, ErrProviderBudgetExhausted
		}
	}
	quotaHeld := key.QuotaUSD != nil && keySpend+a.byKey.amount(key.ID) >= *key.QuotaUSD
	budgetHeld := spend+a.inFlight.amount >= a.budget
	ceilingHeld := capped && providerSpend+a.byProvider.amount(provider) >= ceiling
	if quotaHeld || budgetHeld || ceilingHeld {
		return nil, ErrCapReserved
	}

	a.byKey.add(key.ID, bound)
	a.byProvider.add(provider, bound)
	a.inFlight.add(bound)
	var once sync.Once
	return &Reservation{release: func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.byKey.remove(key.ID, bound)
			a.byProvider.remove(provider, bound)
			a.inFlight.remove(bound)
		})
	}}, nil
}

// Reserved reports the worst-case cost currently held by in-flight requests.
func (a *Accountant) Reserved() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inFlight.amount
}

func (a *Accountant) Record(ctx context.Context, now time.Time, keyID, model, provider string, u store.Usage, p models.Pricing) error {
	u.CostUSD = Cost(u, p)
	if u.Requests == 0 {
		u.Requests = 1
	}
	return a.store.AddUsage(ctx, Month(now), keyID, model, provider, u)
}

// ProviderSpend is one provider's month-to-date spend and its ceiling, nil
// when only the global budget caps it.
type ProviderSpend struct {
	Provider  string
	SpendUSD  float64
	BudgetUSD *float64
}

// ProviderSpend reports, by provider name, every provider with a ceiling
// or with spend attributed to it this month.
func (a *Accountant) ProviderSpend(ctx context.Context, now time.Time) ([]ProviderSpend, error) {
	spent, err := a.store.MonthSpendPerProvider(ctx, Month(now))
	if err != nil {
		return nil, err
	}
	delete(spent, store.Unattributed)
	names := slices.Collect(maps.Keys(spent))
	for provider := range a.providerBudgets {
		if _, ok := spent[provider]; !ok {
			names = append(names, provider)
		}
	}
	slices.Sort(names)
	out := make([]ProviderSpend, len(names))
	for i, provider := range names {
		out[i] = ProviderSpend{Provider: provider, SpendUSD: spent[provider], BudgetUSD: a.ProviderBudget(provider)}
	}
	return out, nil
}

// ProviderBudget is the provider's monthly ceiling, nil when it has none.
func (a *Accountant) ProviderBudget(provider string) *float64 {
	if ceiling, ok := a.providerBudgets[provider]; ok {
		return &ceiling
	}
	return nil
}
