package accounting

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SnowballSH/modelgate/internal/models"
	"github.com/SnowballSH/modelgate/internal/store"
)

var (
	ErrQuotaExhausted  = errors.New("per-key quota exhausted")
	ErrBudgetExhausted = errors.New("global budget exhausted")
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

type Accountant struct {
	store  *store.Store
	budget float64
}

func New(s *store.Store, budgetUSD float64) *Accountant {
	return &Accountant{store: s, budget: budgetUSD}
}

func (a *Accountant) CheckKeyQuota(ctx context.Context, now time.Time, key store.KeyRecord) error {
	if key.QuotaUSD == nil {
		return nil
	}
	spend, err := a.store.MonthSpendByKey(ctx, Month(now), key.ID)
	if err != nil {
		return fmt.Errorf("check quota for key %s: %w", key.ID, err)
	}
	if spend >= *key.QuotaUSD {
		return ErrQuotaExhausted
	}
	return nil
}

func (a *Accountant) CheckGlobalBudget(ctx context.Context, now time.Time) error {
	spend, err := a.store.MonthSpend(ctx, Month(now))
	if err != nil {
		return fmt.Errorf("check global budget: %w", err)
	}
	if spend >= a.budget {
		return ErrBudgetExhausted
	}
	return nil
}

func (a *Accountant) Record(ctx context.Context, now time.Time, keyID, model string, u store.Usage, p models.Pricing) error {
	u.CostUSD = Cost(u, p)
	if u.Requests == 0 {
		u.Requests = 1
	}
	return a.store.AddUsage(ctx, Month(now), keyID, model, u)
}
