package builtin

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// A BUILTIN OFFICER'S CLAUDE USAGE (officers.CapUsage): the usage collector
// (internal/officers/usage) reads each up instance's ccd GET /v1/usage — the
// instance's own Claude Code transcripts, per session and UTC hour — and
// pushes the window (usage.Window, 25 h) here, as it pushes an HQ officer's
// to HQ. This books it into officer_llm_usage (migrations 0075, 0110): one
// row per (owner, officer, pool, instance, UTC day), with requests, the
// tokens by direction, the USD estimated from officers' price list
// (officers.EstimateClaudeUSD) and estimated_requests = requests (every one
// of those dollars is an estimate: a Claude Code instance's transcript
// carries no cost).
//
// 🔴 A PUSH IS THE WHOLE WINDOW, NOT A DELTA, AND THE MERGE IS GREATEST.
// The collector re-sends the last 25 hours on every tick (10 min) and once
// more before a scale-to-zero, so adding would count every call ~150 times.
// Each column instead keeps the larger of what is booked and what this push
// totals for the day. A day's totals only grow while its transcripts are
// appended to, and the window covers ALL of today and, for the first hour
// after midnight UTC, all of yesterday too — so the day's last full-coverage
// push books its final total, and the later pushes that see only its tail
// (a smaller sum) leave it alone. What this does NOT recover: hours that
// fell out of the window while the instance was down (the collector reads up
// instances only), as for HQ.
//
// 🔴 A POOL ON THE LLM PROXY IS NEVER BOOKED HERE: the proxy already books
// every request it serves into the same row (llmproxy.PGStore.Book, adding),
// with the provider's reported cost. Its CLI's transcript holds those same
// calls, so booking it again would double them.
// ---------------------------------------------------------------------------

var _ officers.UsageBackend = (*Backend)(nil)

// dayUsage is one UTC day's totals for one instance.
type dayUsage struct {
	day                                            time.Time
	requests, input, output, cacheRead, cacheWrite int64
	usd                                            float64
}

// PutUsage implements officers.UsageBackend: books r's buckets into
// officer_llm_usage, one row per UTC day (the merge above). A report for a
// name that is not one of this officer's instances is refused (400); one
// for an instance of a pool on the LLM proxy books nothing.
func (b *Backend) PutUsage(ctx context.Context, r officers.UsageReport) error {
	pool, n, ok := officers.ParseInstance(b.officer, r.Instance)
	if !ok {
		return refuse(400, "bad_request", fmt.Sprintf("%q is not an instance of %s", r.Instance, b.officer.ID), nil)
	}
	if _, _, proxied := officers.ProxyAgent(b.officer, r.Instance); proxied {
		return nil
	}
	days := map[time.Time]*dayUsage{}
	for _, k := range r.Buckets {
		d := k.Hour.UTC().Truncate(24 * time.Hour)
		u := days[d]
		if u == nil {
			u = &dayUsage{day: d}
			days[d] = u
		}
		// A model the price list does not know is priced pessimistically
		// (officers.ClaudePriceOf), never as free.
		usd, _ := officers.EstimateClaudeUSD(k.Model, k.InputTokens, k.OutputTokens, k.CacheReadTokens, k.CacheCreationTokens)
		u.requests += max(k.APICalls, 0)
		u.input += max(k.InputTokens, 0)
		u.output += max(k.OutputTokens, 0)
		u.cacheRead += max(k.CacheReadTokens, 0)
		u.cacheWrite += max(k.CacheCreationTokens, 0)
		u.usd += usd
	}
	if len(days) == 0 {
		return nil
	}
	ordered := make([]*dayUsage, 0, len(days))
	for _, u := range days {
		ordered = append(ordered, u)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].day.Before(ordered[j].day) })
	_, err := b.tx(ctx, "usage", func(tx pgx.Tx) ([]byte, error) {
		for _, u := range ordered {
			if err := b.bookDay(ctx, tx, poolColumn(pool), n, u); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	return err
}

// poolColumn is officer_llm_usage.pool for instance pool p: 'routine' for the
// routine pool, 'judgement' for the judgement pool and for an untiered
// officer's instances (named as judgement instances are, `-<n>`).
func poolColumn(p officers.Officer) string {
	if p.Tiers != nil && p.Tier == officers.TierRoutine {
		return officers.TierRoutine
	}
	return officers.TierJudgement
}

// bookDay is the GREATEST merge of one day (the comment above).
func (b *Backend) bookDay(ctx context.Context, tx pgx.Tx, pool string, n int, u *dayUsage) error {
	usd := math.Round(u.usd*1e6) / 1e6 // officer_llm_usage.cost_usd is NUMERIC(14,6)
	_, err := tx.Exec(ctx, `
		INSERT INTO officer_llm_usage (owner_id, officer, pool, instance, day, requests, input_tokens, output_tokens,
		                               cache_read_tokens, cache_write_tokens, cost_usd, estimated_requests)
		VALUES ($1, $2, $3, $4, $5::date, $6, $7, $8, $9, $10, $11, $6)
		ON CONFLICT (owner_id, officer, pool, instance, day) DO UPDATE SET
			requests           = GREATEST(officer_llm_usage.requests, EXCLUDED.requests),
			input_tokens       = GREATEST(officer_llm_usage.input_tokens, EXCLUDED.input_tokens),
			output_tokens      = GREATEST(officer_llm_usage.output_tokens, EXCLUDED.output_tokens),
			cache_read_tokens  = GREATEST(officer_llm_usage.cache_read_tokens, EXCLUDED.cache_read_tokens),
			cache_write_tokens = GREATEST(officer_llm_usage.cache_write_tokens, EXCLUDED.cache_write_tokens),
			cost_usd           = GREATEST(officer_llm_usage.cost_usd, EXCLUDED.cost_usd),
			estimated_requests = GREATEST(officer_llm_usage.estimated_requests, EXCLUDED.estimated_requests),
			updated_at         = now()`,
		b.officer.Owner, b.officer.ID, pool, n, u.day.Format("2006-01-02"),
		u.requests, u.input, u.output, u.cacheRead, u.cacheWrite, usd)
	return err
}

// UsageDay is one row of an officer's usage as the admin API reads it.
type UsageDay struct {
	Day               string  `json:"day"`
	Pool              string  `json:"pool"`
	Instance          int     `json:"instance"`
	Requests          int64   `json:"requests"`
	InputTokens       int64   `json:"input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	CacheReadTokens   int64   `json:"cache_read_tokens"`
	CacheWriteTokens  int64   `json:"cache_write_tokens"`
	CostUSD           float64 `json:"cost_usd"`
	EstimatedRequests int64   `json:"estimated_requests"`
}

// UsageDays bounds the admin read: at most this many days back.
const UsageDays = 90

// ListUsage is owner's officer's officer_llm_usage rows for the last days UTC
// days (today included; 1..UsageDays, default 30), newest first — the
// proxy's rows and PutUsage's alike.
func (s *Store) ListUsage(ctx context.Context, owner int64, officer string, days int, now time.Time) ([]UsageDay, error) {
	if days <= 0 {
		days = 30
	}
	days = min(days, UsageDays)
	from := now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -(days - 1))
	rows, err := s.pool.Query(ctx, `SELECT day::text, pool, instance, requests, input_tokens, output_tokens,
		cache_read_tokens, cache_write_tokens, cost_usd::float8, estimated_requests
		FROM officer_llm_usage WHERE owner_id = $1 AND officer = $2 AND day >= $3::date
		ORDER BY day DESC, pool, instance`, owner, officer, from.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (UsageDay, error) {
		var u UsageDay
		err := r.Scan(&u.Day, &u.Pool, &u.Instance, &u.Requests, &u.InputTokens, &u.OutputTokens,
			&u.CacheReadTokens, &u.CacheWriteTokens, &u.CostUSD, &u.EstimatedRequests)
		return u, err
	})
	if list == nil {
		list = []UsageDay{}
	}
	return list, err
}
