package ccpool

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PGStore is the Postgres-backed [Store] (table cc_account_marks, migration 0003).
type PGStore struct{ pool *pgxpool.Pool }

// NewPG constructs the Postgres store.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

// Marks returns every account's record.
func (s *PGStore) Marks(ctx context.Context) (map[string]Mark, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT account, rate_limited_at, rate_limited_detail, auth_failed_at, auth_failed_detail, auth_failed_token
		FROM cc_account_marks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Mark{}
	for rows.Next() {
		var m Mark
		var rl, af *time.Time
		if err := rows.Scan(&m.Account, &rl, &m.RateLimitedDetail, &af, &m.AuthFailedDetail, &m.AuthFailedToken); err != nil {
			return nil, err
		}
		if rl != nil {
			m.RateLimitedAt = *rl
		}
		if af != nil {
			m.AuthFailedAt = *af
		}
		out[m.Account] = m
	}
	return out, rows.Err()
}

// liveStatuses are the statuses that count as a live agent on an account.
// `stopped` and `error` agents still OWN their account (they keep it for life)
// but are not using it, so they do not count against it.
const liveStatuses = `('pending', 'provisioning', 'running')`

// LiveCounts returns, per account, how many live claude-code agents use it.
func (s *PGStore) LiveCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT cc_account, count(*) FROM agents
		WHERE kind = 'claude-code' AND status IN `+liveStatuses+`
		GROUP BY cc_account`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, rows.Err()
}

// MarkRateLimited upserts the account's rate-limit timestamp and detail.
func (s *PGStore) MarkRateLimited(ctx context.Context, account, detail string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO cc_account_marks (account, rate_limited_at, rate_limited_detail) VALUES ($1, $2, $3)
		ON CONFLICT (account) DO UPDATE SET rate_limited_at = EXCLUDED.rate_limited_at,
			rate_limited_detail = EXCLUDED.rate_limited_detail`, account, at, detail)
	return err
}

// MarkAuthFailed upserts the account's auth-failure timestamp, detail and the
// failing token's fingerprint.
func (s *PGStore) MarkAuthFailed(ctx context.Context, account, fingerprint, detail string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO cc_account_marks (account, auth_failed_at, auth_failed_detail, auth_failed_token) VALUES ($1, $2, $3, $4)
		ON CONFLICT (account) DO UPDATE SET auth_failed_at = EXCLUDED.auth_failed_at,
			auth_failed_detail = EXCLUDED.auth_failed_detail, auth_failed_token = EXCLUDED.auth_failed_token`,
		account, at, detail, fingerprint)
	return err
}
