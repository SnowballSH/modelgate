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

type Guards struct {
	store        *store.Store
	acct         *accounting.Accountant
	table        *models.Table
	rpm          int
	maxBodyBytes int64
	now          func() time.Time
	slots        chan struct{}
	mu           sync.Mutex
	buckets      map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func NewGuards(s *store.Store, acct *accounting.Accountant, table *models.Table, rpm, maxConcurrent int, maxBodyBytes int64, now func() time.Time) *Guards {
	return &Guards{
		store:        s,
		acct:         acct,
		table:        table,
		rpm:          rpm,
		maxBodyBytes: maxBodyBytes,
		now:          now,
		slots:        make(chan struct{}, maxConcurrent),
		buckets:      make(map[string]*bucket),
	}
}

type Admission struct {
	Key     store.KeyRecord
	Model   models.Model
	Release func()
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

func (g *Guards) Admit(ctx context.Context, authorization string, bodyLen int64, requestedModel string) (Admission, string, bool) {
	if bodyLen > g.maxBodyBytes {
		return Admission{}, CodeRequestTooLarge, false
	}
	key, ok, err := g.Authenticate(ctx, authorization)
	if err != nil {
		return Admission{}, CodeInternal, false
	}
	if !ok {
		return Admission{}, CodeInvalidAPIKey, false
	}
	now := g.now()
	if !g.takeToken(key.ID, now) {
		return Admission{}, CodeRateLimited, false
	}
	model, resolved := g.table.Resolve(requestedModel)
	if !resolved || (key.Models != nil && !slices.Contains(key.Models, requestedModel)) {
		return Admission{}, CodeModelNotFound, false
	}
	select {
	case g.slots <- struct{}{}:
	default:
		return Admission{}, CodeRateLimited, false
	}
	var once sync.Once
	release := func() {
		once.Do(func() { <-g.slots })
	}
	if err := g.acct.CheckKeyQuota(ctx, now, key); err != nil {
		release()
		if errors.Is(err, accounting.ErrQuotaExhausted) {
			return Admission{}, CodeQuotaExhausted, false
		}
		return Admission{}, CodeInternal, false
	}
	if err := g.acct.CheckGlobalBudget(ctx, now); err != nil {
		release()
		if errors.Is(err, accounting.ErrBudgetExhausted) {
			return Admission{}, CodeBudgetExhausted, false
		}
		return Admission{}, CodeInternal, false
	}
	return Admission{Key: key, Model: model, Release: release}, "", true
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

func (g *Guards) takeToken(id string, now time.Time) bool {
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
		return false
	}
	b.tokens--
	return true
}
