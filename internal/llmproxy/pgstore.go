package llmproxy

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PGStore is Store on officer_llm_usage (migration 0073), under row-level
// security: ctx must act as the officer's owner (the request's principal is
// the calling instance's owner, which is its officer's), and the row lands
// under that owner. Numbers only: no column can hold a prompt, a response or
// a key.
type PGStore struct{ pool *pgxpool.Pool }

// NewPGStore is Store over pool.
func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

// SpentToday implements Store.
func (s *PGStore) SpentToday(ctx context.Context, officer string, day time.Time) (float64, error) {
	var usd float64
	err := s.pool.QueryRow(ctx, `SELECT coalesce(sum(cost_usd), 0)::float8 FROM officer_llm_usage WHERE officer = $1 AND day = $2`,
		officer, day.UTC().Format("2006-01-02")).Scan(&usd)
	return usd, err
}

// Book implements Store: one statement, so two concurrent requests never lose
// a count.
func (s *PGStore) Book(ctx context.Context, u Usage) error {
	est := int64(0)
	if u.Estimated {
		est = 1
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO officer_llm_usage (officer, instance, day, requests, input_tokens, output_tokens,
		                               cache_read_tokens, cache_write_tokens, cost_usd, estimated_requests)
		VALUES ($1, $2, $3::date, 1, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (owner_id, officer, instance, day) DO UPDATE SET
			requests           = officer_llm_usage.requests + 1,
			input_tokens       = officer_llm_usage.input_tokens + EXCLUDED.input_tokens,
			output_tokens      = officer_llm_usage.output_tokens + EXCLUDED.output_tokens,
			cache_read_tokens  = officer_llm_usage.cache_read_tokens + EXCLUDED.cache_read_tokens,
			cache_write_tokens = officer_llm_usage.cache_write_tokens + EXCLUDED.cache_write_tokens,
			cost_usd           = officer_llm_usage.cost_usd + EXCLUDED.cost_usd,
			estimated_requests = officer_llm_usage.estimated_requests + EXCLUDED.estimated_requests,
			updated_at         = now()`,
		u.Officer, u.Instance, u.Day.UTC().Format("2006-01-02"), u.Input, u.Output, u.CacheRead, u.CacheWrite, u.CostUSD, est)
	return err
}
