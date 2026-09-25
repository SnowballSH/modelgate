package accounting

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/SnowballSH/modelgate/internal/models"
	"github.com/SnowballSH/modelgate/internal/store"
)

var (
	ErrQuotaExhausted  = errors.New("per-key quota exhausted")
	ErrBudgetExhausted = errors.New("global budget exhausted")
	// ErrCapReserved refuses a request whose quota or budget is not yet spent
	// but is fully held by in-flight reservations; it clears as they finish.
	ErrCapReserved = errors.New("remaining quota or budget held by in-flight requests")
)

func Month(t time.Time) string {
	return t.UTC().Format("2006-01")
}

func Cost(u store.Usage, p models.Pricing) float64 {
	return (float64(u.InputTokens)*p.InputUSDPerMTok +
		float64(u.OutputTokens)*p.OutputUSDPerMTok +
		float64(u.CacheReadTokens)*p.CacheReadUSDPerMTok +
		float64(u.CacheWriteTokens)*p.CacheWriteUSDPerMTok) / 1e6
}

// Demand is the most a request can consume: every prompt token and every
// output token the request allows.
type Demand struct {
	InputTokens, OutputTokens int64
}

// WorstCaseCost prices a demand as if every prompt token were billed at the
// dearest input rate, since the provider decides which prompt tokens are
// cache reads, cache writes or plain input.
func WorstCaseCost(d Demand, p models.Pricing) float64 {
	inputRate := max(p.InputUSDPerMTok, p.CacheReadUSDPerMTok, p.CacheWriteUSDPerMTok)
	return (float64(d.InputTokens)*inputRate + float64(d.OutputTokens)*p.OutputUSDPerMTok) / 1e6
}

type Accountant struct {
	store  *store.Store
	budget float64

	mu       sync.Mutex
	inFlight reserved
	byKey    map[string]*reserved
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

func New(s *store.Store, budgetUSD float64) *Accountant {
	return &Accountant{store: s, budget: budgetUSD, byKey: map[string]*reserved{}}
}

// Reservation holds a request's worst-case cost against its key quota and
// the monthly budget until the request's real usage is booked.
type Reservation struct {
	release func()
}

func (r *Reservation) Release() { r.release() }

// Reserve admits a request while the recorded spend plus every in-flight
// reservation stays below the key quota and the monthly budget, then holds
// bound against both. Concurrent requests therefore overshoot a cap by at
// most one request's bound. The check reads the store under the same lock
// that releases reservations, and callers book usage before releasing, so a
// request's cost is always counted either as a reservation or as spend.
//
// Recorded spend at a cap is exhaustion, which only the next month or an
// operator clears; a cap reached only by adding reservations is
// ErrCapReserved, which clears as the requests holding them finish.
func (a *Accountant) Reserve(ctx context.Context, now time.Time, key store.KeyRecord, bound float64) (*Reservation, error) {
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
	quotaHeld := key.QuotaUSD != nil && keySpend+a.reservedFor(key.ID) >= *key.QuotaUSD
	budgetHeld := spend+a.inFlight.amount >= a.budget
	if quotaHeld || budgetHeld {
		return nil, ErrCapReserved
	}

	perKey, ok := a.byKey[key.ID]
	if !ok {
		perKey = &reserved{}
		a.byKey[key.ID] = perKey
	}
	perKey.add(bound)
	a.inFlight.add(bound)
	var once sync.Once
	return &Reservation{release: func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			perKey.remove(bound)
			if perKey.count == 0 {
				delete(a.byKey, key.ID)
			}
			a.inFlight.remove(bound)
		})
	}}, nil
}

func (a *Accountant) reservedFor(keyID string) float64 {
	if r, ok := a.byKey[keyID]; ok {
		return r.amount
	}
	return 0
}

// Reserved reports the worst-case cost currently held by in-flight requests.
func (a *Accountant) Reserved() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inFlight.amount
}

func (a *Accountant) Record(ctx context.Context, now time.Time, keyID, model string, u store.Usage, p models.Pricing) error {
	u.CostUSD = Cost(u, p)
	if u.Requests == 0 {
		u.Requests = 1
	}
	return a.store.AddUsage(ctx, Month(now), keyID, model, u)
}
