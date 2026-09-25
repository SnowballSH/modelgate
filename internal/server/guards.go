package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/SnowballSH/modelgate/internal/accounting"
	"github.com/SnowballSH/modelgate/internal/keys"
	"github.com/SnowballSH/modelgate/internal/models"
	"github.com/SnowballSH/modelgate/internal/store"
)

const (
	slotRetryAfter = time.Second
	// capReservedRetryAfter is long enough that a retry is not spinning and
	// short enough that the OpenAI SDKs, which ignore waits over a minute,
	// honour it.
	capReservedRetryAfter = 5 * time.Second
)

type Guards struct {
	store   *store.Store
	acct    *accounting.Accountant
	table   *models.Table
	rpm     int
	now     func() time.Time
	slots   chan struct{}
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func NewGuards(s *store.Store, acct *accounting.Accountant, table *models.Table, rpm, maxConcurrent int, now func() time.Time) *Guards {
	return &Guards{
		store:   s,
		acct:    acct,
		table:   table,
		rpm:     rpm,
		now:     now,
		slots:   make(chan struct{}, maxConcurrent),
		buckets: make(map[string]*bucket),
	}
}

type Admission struct {
	Key   store.KeyRecord
	Model models.Model
	// Release frees the concurrency slot and the cost reservation. Book the
	// request's usage before calling it, or the spend goes briefly uncounted.
	Release func()
}

// Refusal is a guard's answer to a request it will not admit. RetryAfter is
// set when waiting is enough for the same request to succeed.
type Refusal struct {
	Code       string
	RetryAfter time.Duration
}

func (g *Guards) Authenticate(ctx context.Context, authorization string) (store.KeyRecord, bool, error) {
	id, secret, ok := keys.ParseBearer(authorization)
	if !ok {
		return store.KeyRecord{}, false, nil
	}
	key, found, err := g.store.KeyByID(ctx, id)
	if err != nil {
		return store.KeyRecord{}, false, err
	}
	if !g.keyUsable(key, found, secret, g.now()) {
		return store.KeyRecord{}, false, nil
	}
	return key, true, nil
}

// ResolveModel runs the guards that need only the key and the model name:
// the key's rate limit, then the model table and the key's allowlist.
func (g *Guards) ResolveModel(key store.KeyRecord, requestedModel string) (models.Model, *Refusal) {
	if wait, ok := g.takeToken(key.ID, g.now()); !ok {
		return models.Model{}, &Refusal{Code: CodeRateLimited, RetryAfter: wait}
	}
	model, resolved := g.table.Resolve(requestedModel)
	if !resolved || (key.Models != nil && !slices.Contains(key.Models, requestedModel)) {
		return models.Model{}, &Refusal{Code: CodeModelNotFound}
	}
	return model, nil
}

// Admit takes the shared capacity a resolved request needs: a concurrency
// slot, then a reservation of its worst-case cost against the key quota and
// the budget.
func (g *Guards) Admit(ctx context.Context, key store.KeyRecord, model models.Model, demand accounting.Demand) (Admission, *Refusal) {
	now := g.now()
	select {
	case g.slots <- struct{}{}:
	default:
		return Admission{}, &Refusal{Code: CodeRateLimited, RetryAfter: slotRetryAfter}
	}
	reservation, err := g.acct.Reserve(ctx, now, key, accounting.WorstCaseCost(demand, model.Pricing))
	if err != nil {
		<-g.slots
		switch {
		case errors.Is(err, accounting.ErrQuotaExhausted):
			return Admission{}, &Refusal{Code: CodeQuotaExhausted}
		case errors.Is(err, accounting.ErrBudgetExhausted):
			return Admission{}, &Refusal{Code: CodeBudgetExhausted}
		case errors.Is(err, accounting.ErrCapReserved):
			return Admission{}, &Refusal{Code: CodeCapReserved, RetryAfter: capReservedRetryAfter}
		default:
			return Admission{}, &Refusal{Code: CodeInternal}
		}
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			reservation.Release()
			<-g.slots
		})
	}
	return Admission{Key: key, Model: model, Release: release}, nil
}

func (g *Guards) keyUsable(key store.KeyRecord, found bool, secret string, now time.Time) bool {
	if !found || len(key.SecretSHA256) != sha256.Size {
		return false
	}
	if !keys.Verify(secret, [sha256.Size]byte(key.SecretSHA256)) {
		return false
	}
	if key.RevokedAt != nil {
		return false
	}
	if key.ExpiresAt != nil && now.After(*key.ExpiresAt) {
		return false
	}
	return true
}

// takeToken spends one token from the key's bucket, or reports how long
// until the bucket refills to one.
func (g *Guards) takeToken(id string, now time.Time) (time.Duration, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	b, ok := g.buckets[id]
	if !ok {
		b = &bucket{tokens: float64(g.rpm), last: now}
		g.buckets[id] = b
	}
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = min(float64(g.rpm), b.tokens+elapsed.Minutes()*float64(g.rpm))
		b.last = now
	}
	if b.tokens < 1 {
		return time.Duration((1 - b.tokens) / float64(g.rpm) * float64(time.Minute)), false
	}
	b.tokens--
	return 0, true
}
