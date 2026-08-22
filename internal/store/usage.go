package store

import (
	"context"
	"fmt"
)

type Usage struct {
	Requests, InputTokens, OutputTokens, CacheReadTokens, CacheWriteTokens int64
	CostUSD                                                                float64
}

func (u Usage) HasTokens() bool {
	return u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0
}

func (s *Store) AddUsage(ctx context.Context, month, keyID, model string, u Usage) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO usage (month, key_id, model, requests, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (month, key_id, model) DO UPDATE SET
	requests = requests + excluded.requests,
	input_tokens = input_tokens + excluded.input_tokens,
	output_tokens = output_tokens + excluded.output_tokens,
	cache_read_tokens = cache_read_tokens + excluded.cache_read_tokens,
	cache_write_tokens = cache_write_tokens + excluded.cache_write_tokens,
	cost_usd = cost_usd + excluded.cost_usd`,
		month, keyID, model,
		u.Requests, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.CostUSD,
	)
	if err != nil {
		return fmt.Errorf("add usage %s/%s/%s: %w", month, keyID, model, err)
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
SELECT key_id, SUM(requests), SUM(input_tokens), SUM(output_tokens), SUM(cache_read_tokens), SUM(cache_write_tokens), SUM(cost_usd)
FROM usage WHERE month = ? GROUP BY key_id`, month)
	if err != nil {
		return nil, fmt.Errorf("month usage %s: %w", month, err)
	}
	defer rows.Close()
	out := make(map[string]Usage)
	for rows.Next() {
		var keyID string
		var u Usage
		if err := rows.Scan(&keyID, &u.Requests, &u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.CacheWriteTokens, &u.CostUSD); err != nil {
			return nil, fmt.Errorf("month usage %s: %w", month, err)
		}
		out[keyID] = u
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("month usage %s: %w", month, err)
	}
	return out, nil
}
