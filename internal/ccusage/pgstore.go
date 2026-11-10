package ccusage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PGStore is Store on cc_account_usage (migration 0108), plus the attribution
// reads over agents and jobs. Every query names the owner: the tables are under
// row-level security, which scopes a user principal on its own, and the owner
// predicate keeps a scope-all caller from merging two owners' accounts of one
// name (ccpool.PGStore's rule).
type PGStore struct{ pool *pgxpool.Pool }

// NewPG is the Postgres store.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

// Latest implements Store.
func (s *PGStore) Latest(ctx context.Context, owner int64) (map[string]Row, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT account, windows, source, status, representative_claim, read_at, checked_at, error_kind, error, token_fp,
		       usage_api, usage_api_next_at, rate_limit_streak, next_poll_at
		FROM cc_account_usage WHERE owner_id = $1`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Row{}
	for rows.Next() {
		var r Row
		var raw []byte
		var readAt, next, apiNext *time.Time
		if err := rows.Scan(&r.Account, &raw, &r.Source, &r.Status, &r.RepresentativeClaim, &readAt, &r.CheckedAt, &r.ErrorKind,
			&r.Error, &r.TokenFP, &r.UsageAPI, &apiNext, &r.RateLimitStreak, &next); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &r.Windows); err != nil {
			return nil, fmt.Errorf("account %s: windows: %w", r.Account, err)
		}
		if readAt != nil {
			r.ReadAt = *readAt
		}
		if next != nil {
			r.NextAt = *next
		}
		if apiNext != nil {
			r.UsageAPINextAt = *apiNext
		}
		out[r.Account] = r
	}
	return out, rows.Err()
}

// Save implements Store: one upsert per (owner, account).
func (s *PGStore) Save(ctx context.Context, owner int64, r Row) error {
	w := r.Windows
	if w == nil {
		w = map[string]Window{}
	}
	raw, err := json.Marshal(w)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO cc_account_usage (owner_id, account, windows, read_at, checked_at, error_kind, error, token_fp,
		                              rate_limit_streak, next_poll_at, source, status, representative_claim,
		                              usage_api, usage_api_next_at, updated_at)
		VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, now())
		ON CONFLICT (owner_id, account) DO UPDATE SET
			windows = EXCLUDED.windows, read_at = EXCLUDED.read_at, checked_at = EXCLUDED.checked_at,
			source = EXCLUDED.source, status = EXCLUDED.status, representative_claim = EXCLUDED.representative_claim,
			usage_api = EXCLUDED.usage_api, usage_api_next_at = EXCLUDED.usage_api_next_at,
			error_kind = EXCLUDED.error_kind, error = EXCLUDED.error, token_fp = EXCLUDED.token_fp,
			rate_limit_streak = EXCLUDED.rate_limit_streak, next_poll_at = EXCLUDED.next_poll_at, updated_at = now()`,
		owner, r.Account, string(raw), nullTime(r.ReadAt), r.CheckedAt, r.ErrorKind, r.Error, r.TokenFP,
		r.RateLimitStreak, nullTime(r.NextAt), r.Source, r.Status, r.RepresentativeClaim, r.UsageAPI, nullTime(r.UsageAPINextAt))
	return err
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// liveStatuses is ccpool's: the statuses that count as an agent using its
// account now.
const liveStatuses = `('pending', 'provisioning', 'running')`

// AgentsSince is every one of owner's claude-code agents on an account that is
// live now or stopped at or after since, with when it was last active (now
// for a live one).
func (s *PGStore) AgentsSince(ctx context.Context, owner int64, since, now time.Time) ([]AgentUse, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, cc_account, worker_pool, CASE WHEN status IN `+liveStatuses+` THEN $3 ELSE stopped_at END
		FROM agents
		WHERE owner_id = $1 AND kind = 'claude-code' AND cc_account <> ''
		  AND (status IN `+liveStatuses+` OR stopped_at >= $2)`, owner, since, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentUse
	for rows.Next() {
		var u AgentUse
		if err := rows.Scan(&u.Name, &u.CCAccount, &u.WorkerPool, &u.LastActive); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// JobCounts is owner's jobs created in the last week, by the account their
// last turn ran on and their requester, counted over the last day and week
// before now.
func (s *PGStore) JobCounts(ctx context.Context, owner int64, now time.Time) ([]JobCount, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT cc_account, requested_by,
		       count(*) FILTER (WHERE created_at >= $2),
		       count(*)
		FROM jobs
		WHERE owner_id = $1 AND cc_account <> '' AND created_at >= $3 AND created_at <= $4
		GROUP BY cc_account, requested_by`, owner, now.Add(-Day), now.Add(-Week), now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobCount
	for rows.Next() {
		var j JobCount
		if err := rows.Scan(&j.CCAccount, &j.RequestedBy, &j.Last24h, &j.Last7d); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
