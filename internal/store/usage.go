package store

import (
	"context"
	"fmt"
)

// Usage is what requests consumed: tokens by kind, the provider-native web
// searches they ran, and what all of it cost.
type Usage struct {
	Requests, InputTokens, OutputTokens, CacheReadTokens, CacheWriteTokens int64
	WebSearchRequests                                                      int64
	CostUSD                                                                float64
}

func (u Usage) HasTokens() bool {
	return u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0
}

// Billable reports whether the usage carries anything a provider charges for.
func (u Usage) Billable() bool {
	return u.HasTokens() || u.WebSearchRequests > 0
}

func (u Usage) PromptTokens() int64 {
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

func (s *Store) AddUsage(ctx context.Context, month, keyID, model, provider string, u Usage) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO usage (month, key_id, model, provider, requests, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, web_search_requests, cost_usd)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (month, key_id, model, provider) DO UPDATE SET
	requests = requests + excluded.requests,
	input_tokens = input_tokens + excluded.input_tokens,
	output_tokens = output_tokens + excluded.output_tokens,
	cache_read_tokens = cache_read_tokens + excluded.cache_read_tokens,
	cache_write_tokens = cache_write_tokens + excluded.cache_write_tokens,
	web_search_requests = web_search_requests + excluded.web_search_requests,
	cost_usd = cost_usd + excluded.cost_usd`,
		month, keyID, model, provider,
		u.Requests, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.WebSearchRequests, u.CostUSD,
	)
	if err != nil {
		return fmt.Errorf("add usage %s/%s/%s/%s: %w", month, keyID, model, provider, err)
	}
	return nil
}

func (s *Store) MonthSpend(ctx context.Context, month string) (float64, error) {
	var total float64
	err := s.db.QueryRowContext(ctx,
		"SELECT COALESCE(SUM(cost_usd), 0) FROM usage WHERE month = ?", month,
	).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("month spend %s: %w", month, err)
	}
	return total, nil
}

func (s *Store) MonthSpendByKey(ctx context.Context, month, keyID string) (float64, error) {
	var total float64
	err := s.db.QueryRowContext(ctx,
		"SELECT COALESCE(SUM(cost_usd), 0) FROM usage WHERE month = ? AND key_id = ?", month, keyID,
	).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("month spend %s/%s: %w", month, keyID, err)
	}
	return total, nil
}

func (s *Store) MonthUsage(ctx context.Context, month string) (map[string]Usage, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT key_id, SUM(requests), SUM(input_tokens), SUM(output_tokens), SUM(cache_read_tokens), SUM(cache_write_tokens), SUM(web_search_requests), SUM(cost_usd)
FROM usage WHERE month = ? GROUP BY key_id`, month)
	if err != nil {
		return nil, fmt.Errorf("month usage %s: %w", month, err)
	}
	defer rows.Close()
	out := make(map[string]Usage)
	for rows.Next() {
		var keyID string
		var u Usage
		if err := rows.Scan(&keyID, &u.Requests, &u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.CacheWriteTokens, &u.WebSearchRequests, &u.CostUSD); err != nil {
			return nil, fmt.Errorf("month usage %s: %w", month, err)
		}
		out[keyID] = u
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("month usage %s: %w", month, err)
	}
	return out, nil
}

// Unattributed is the provider of usage booked before v0.8.0 for a model
// the table no longer served at the upgrade: it counts against the global
// budget and no provider's ceiling.
const Unattributed = ""

func (s *Store) MonthSpendByProvider(ctx context.Context, month, provider string) (float64, error) {
	var total float64
	err := s.db.QueryRowContext(ctx,
		"SELECT COALESCE(SUM(cost_usd), 0) FROM usage WHERE month = ? AND provider = ?", month, provider,
	).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("month spend %s/%s: %w", month, provider, err)
	}
	return total, nil
}

// MonthSpendPerProvider reports the month's spend for every provider with
// usage booked, Unattributed included.
func (s *Store) MonthSpendPerProvider(ctx context.Context, month string) (map[string]float64, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT provider, SUM(cost_usd) FROM usage WHERE month = ? GROUP BY provider", month)
	if err != nil {
		return nil, fmt.Errorf("month spend per provider %s: %w", month, err)
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var provider string
		var spend float64
		if err := rows.Scan(&provider, &spend); err != nil {
			return nil, fmt.Errorf("month spend per provider %s: %w", month, err)
		}
		out[provider] = spend
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("month spend per provider %s: %w", month, err)
	}
	return out, nil
}
