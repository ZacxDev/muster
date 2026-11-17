package chiefusage

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ---------------------------------------------------------------------------
// WHAT A CHIEF'S OPENROUTER SESSIONS SPEND (migration 0142; docs/chief.md
// "OpenRouter sessions, budget").
//
// One chief_session_llm_usage row per (owner, chat session, UTC day) muster's
// LLM proxy served for a chief session on OpenRouter. The proxy books every
// request into it from the provider's own reported `usage.cost` — muster's own
// count, never the session runtime's (opencode reports list price × tokens, or
// 0) — and the budget is read back from it: the owner's day for the daily cap,
// the session's lifetime for the optional per-session cap.
//
// 🔴 THE CAPS HOLD UNDER CONCURRENCY BY RESERVATION, NOT BY A READ. Checking
// the booked spend and booking after the response let N concurrent requests
// all pass the same check. Reserve instead checks AND records the request's
// pessimistic estimate in one transaction, under a per-owner advisory lock
// (pg_advisory_xact_lock), so the next request — in any muster process — sees
// it; Settle replaces it by the booked cost, Release drops it, and one whose
// request died stops counting at expires_at.
//
// 🔴 OWNED AND UNDER ROW-LEVEL SECURITY: ctx acts as the chief's owner; every
// write lands under that owner and every read is that owner's alone.
// ---------------------------------------------------------------------------

// SessionRef is a chat session as the LLM proxy's chief-session door reads it
// by its session key: whose it is, the agent it belongs to, its stored model
// and whether it is the chief's primary (default) chat.
type SessionRef struct {
	ID        int64
	OwnerID   int64
	AgentID   int64
	Model     string
	IsDefault bool
}

// SessionLLMUsage is one proxied request of a chief session, booked to its
// (owner, session, day) row.
type SessionLLMUsage struct {
	SessionID int64
	// Model is the session's stored model (the pin), kept with the row so the
	// usage page names it after the session is gone.
	Model                                string
	Day                                  time.Time // a UTC midnight
	Input, Output, CacheRead, CacheWrite int64
	CostUSD                              float64
	Estimated                            bool
}

// SessionSpend is what a chief session's caps are checked against: the
// owner's booked USD on the day and in flight (live reservations, any
// session), and the session's booked USD over its life and in flight.
type SessionSpend struct {
	DaySpent, DayReserved         float64
	SessionSpent, SessionReserved float64
}

// SessionLLMStore is the chief sessions' spend book. PGStore implements it.
type SessionLLMStore interface {
	// SessionRef reads the session with this session key, as ctx's owner sees
	// it (row-level security: another owner's session reads as none); ok
	// false when there is none.
	SessionRef(ctx context.Context, sessionKey string) (SessionRef, bool, error)
	// Spend is session sessionID's SessionSpend on day, as ctx's owner.
	Spend(ctx context.Context, sessionID int64, day time.Time) (SessionSpend, error)
	// Reserve reads the SessionSpend and asks decide how much to reserve for
	// one request, in ONE transaction under the owner's lock: decide's
	// answer above 0 is recorded as a reservation (id), lasting ttl; 0
	// reserves nothing (id 0). The spend decide saw is returned.
	Reserve(ctx context.Context, sessionID int64, day time.Time, ttl time.Duration,
		decide func(SessionSpend) float64) (int64, SessionSpend, error)
	// Settle books u and drops reservation id, in one transaction.
	Settle(ctx context.Context, id int64, u SessionLLMUsage) error
	// Release drops reservation id (its request was not sent, or failed).
	Release(ctx context.Context, id int64) error
	// SessionLLMReport is the usage page's section over [since, now).
	SessionLLMReport(ctx context.Context, since time.Time) (SessionLLMReport, error)
}

// SessionLLMReport is the usage page's "Chief sessions on OpenRouter" numbers.
type SessionLLMReport struct {
	// Today is the owner's booked USD on the current UTC day.
	Today float64
	// Days is one entry per UTC day from since's day to today, oldest first,
	// days with nothing included (zeros).
	Days []SessionLLMDay
	// Sessions is every session with spend in the window, costliest first.
	Sessions []SessionLLMRow
}

// SessionLLMDay is one UTC day's chief-session spend.
type SessionLLMDay struct {
	Day      time.Time
	Requests int64
	CostUSD  float64
}

// SessionLLMRow is one session's spend in the window: its model, the turns
// recorded for it (chief_turn_usage), its tokens and USD.
type SessionLLMRow struct {
	SessionID int64
	Model     string
	Turns     int64
	Requests  int64
	Tokens    Tokens
	CostUSD   float64
}

var _ SessionLLMStore = (*PGStore)(nil)

// SessionRef implements SessionLLMStore.
func (s *PGStore) SessionRef(ctx context.Context, sessionKey string) (SessionRef, bool, error) {
	if sessionKey == "" {
		return SessionRef{}, false, nil
	}
	var r SessionRef
	err := s.pool.QueryRow(ctx, `SELECT id, owner_id, agent_id, model, is_default FROM chat_sessions WHERE session_key = $1`,
		sessionKey).Scan(&r.ID, &r.OwnerID, &r.AgentID, &r.Model, &r.IsDefault)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionRef{}, false, nil
	}
	if err != nil {
		return SessionRef{}, false, err
	}
	return r, true, nil
}

// DaySpent implements SessionLLMStore.
func (s *PGStore) DaySpent(ctx context.Context, day time.Time) (float64, error) {
	o, err := owner(ctx)
	if err != nil {
		return 0, err
	}
	var usd float64
	err = s.pool.QueryRow(ctx, `SELECT coalesce(sum(cost_usd), 0)::float8 FROM chief_session_llm_usage
		WHERE owner_id = $1 AND day = $2::date`, o, day.UTC().Format("2006-01-02")).Scan(&usd)
	return usd, err
}

// SessionSpent implements SessionLLMStore.
func (s *PGStore) SessionSpent(ctx context.Context, sessionID int64) (float64, error) {
	o, err := owner(ctx)
	if err != nil {
		return 0, err
	}
	var usd float64
	err = s.pool.QueryRow(ctx, `SELECT coalesce(sum(cost_usd), 0)::float8 FROM chief_session_llm_usage
		WHERE owner_id = $1 AND session_id = $2`, o, sessionID).Scan(&usd)
	return usd, err
}

// execer is a pool or a transaction.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// BookSessionLLM adds u to its (owner, session, day) row: one statement, so
// two concurrent requests never lose a count. Settle is the proxy's path.
func (s *PGStore) BookSessionLLM(ctx context.Context, u SessionLLMUsage) error {
	return bookSessionLLM(ctx, s.pool, u)
}

func bookSessionLLM(ctx context.Context, x execer, u SessionLLMUsage) error {
	o, err := owner(ctx)
	if err != nil {
		return err
	}
	est := int64(0)
	if u.Estimated {
		est = 1
	}
	_, err = x.Exec(ctx, `
		INSERT INTO chief_session_llm_usage (owner_id, session_id, day, model, requests, input_tokens, output_tokens,
		                                     cache_read_tokens, cache_write_tokens, cost_usd, estimated_requests)
		VALUES ($1, $2, $3::date, $4, 1, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (owner_id, session_id, day) DO UPDATE SET
			requests           = chief_session_llm_usage.requests + 1,
			input_tokens       = chief_session_llm_usage.input_tokens + EXCLUDED.input_tokens,
			output_tokens      = chief_session_llm_usage.output_tokens + EXCLUDED.output_tokens,
			cache_read_tokens  = chief_session_llm_usage.cache_read_tokens + EXCLUDED.cache_read_tokens,
			cache_write_tokens = chief_session_llm_usage.cache_write_tokens + EXCLUDED.cache_write_tokens,
			cost_usd           = chief_session_llm_usage.cost_usd + EXCLUDED.cost_usd,
			estimated_requests = chief_session_llm_usage.estimated_requests + EXCLUDED.estimated_requests,
			model              = EXCLUDED.model,
			updated_at         = now()`,
		o, u.SessionID, u.Day.UTC().Format("2006-01-02"), cut(u.Model, 200), max(u.Input, 0), max(u.Output, 0),
		max(u.CacheRead, 0), max(u.CacheWrite, 0), max(u.CostUSD, 0), est)
	return err
}

// sessionSpend reads the SessionSpend inside q (a transaction or the pool).
func sessionSpend(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, o, sessionID int64, day time.Time) (SessionSpend, error) {
	var sp SessionSpend
	err := q.QueryRow(ctx, `SELECT
		(SELECT coalesce(sum(cost_usd), 0)::float8 FROM chief_session_llm_usage WHERE owner_id = $1 AND day = $3::date),
		(SELECT coalesce(sum(amount_usd), 0)::float8 FROM chief_session_llm_reservations WHERE owner_id = $1 AND expires_at > now()),
		(SELECT coalesce(sum(cost_usd), 0)::float8 FROM chief_session_llm_usage WHERE owner_id = $1 AND session_id = $2),
		(SELECT coalesce(sum(amount_usd), 0)::float8 FROM chief_session_llm_reservations
			WHERE owner_id = $1 AND session_id = $2 AND expires_at > now())`,
		o, sessionID, day.UTC().Format("2006-01-02")).Scan(&sp.DaySpent, &sp.DayReserved, &sp.SessionSpent, &sp.SessionReserved)
	return sp, err
}

// Spend implements SessionLLMStore.
func (s *PGStore) Spend(ctx context.Context, sessionID int64, day time.Time) (SessionSpend, error) {
	o, err := owner(ctx)
	if err != nil {
		return SessionSpend{}, err
	}
	return sessionSpend(ctx, s.pool, o, sessionID, day)
}

// Reserve implements SessionLLMStore. The advisory lock is per owner and
// transaction-scoped: every Reserve of one owner, in every muster process,
// runs one at a time, and each sees the reservations of those before it.
func (s *PGStore) Reserve(ctx context.Context, sessionID int64, day time.Time, ttl time.Duration,
	decide func(SessionSpend) float64) (int64, SessionSpend, error) {
	o, err := owner(ctx)
	if err != nil {
		return 0, SessionSpend{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, SessionSpend{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('muster.chief-session-llm:' || $1::bigint::text, 0))`, o); err != nil {
		return 0, SessionSpend{}, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM chief_session_llm_reservations WHERE owner_id = $1 AND expires_at <= now()`, o); err != nil {
		return 0, SessionSpend{}, err
	}
	sp, err := sessionSpend(ctx, tx, o, sessionID, day)
	if err != nil {
		return 0, sp, err
	}
	amount := decide(sp)
	if !(amount > 0) {
		return 0, sp, tx.Commit(ctx)
	}
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO chief_session_llm_reservations (owner_id, session_id, amount_usd, expires_at)
		VALUES ($1, $2, $3, now() + make_interval(secs => $4)) RETURNING id`, o, sessionID, amount, ttl.Seconds()).Scan(&id); err != nil {
		return 0, sp, err
	}
	return id, sp, tx.Commit(ctx)
}

// Settle implements SessionLLMStore.
func (s *PGStore) Settle(ctx context.Context, id int64, u SessionLLMUsage) error {
	o, err := owner(ctx)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `DELETE FROM chief_session_llm_reservations WHERE id = $1 AND owner_id = $2`, id, o); err != nil {
		return err
	}
	if err := bookSessionLLM(ctx, tx, u); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Release implements SessionLLMStore.
func (s *PGStore) Release(ctx context.Context, id int64) error {
	o, err := owner(ctx)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `DELETE FROM chief_session_llm_reservations WHERE id = $1 AND owner_id = $2`, id, o)
	return err
}

// SessionLLMReport implements SessionLLMStore.
func (s *PGStore) SessionLLMReport(ctx context.Context, since time.Time) (SessionLLMReport, error) {
	o, err := owner(ctx)
	if err != nil {
		return SessionLLMReport{}, err
	}
	now := time.Now().UTC()
	since = since.UTC()
	day0 := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, time.UTC)
	var rep SessionLLMReport
	byDay := map[string]*SessionLLMDay{}
	for d := day0; !d.After(now); d = d.AddDate(0, 0, 1) {
		rep.Days = append(rep.Days, SessionLLMDay{Day: d})
	}
	for i := range rep.Days {
		byDay[rep.Days[i].Day.Format("2006-01-02")] = &rep.Days[i]
	}
	rows, err := s.pool.Query(ctx, `SELECT to_char(day, 'YYYY-MM-DD'), coalesce(sum(requests), 0)::bigint,
		coalesce(sum(cost_usd), 0)::float8
		FROM chief_session_llm_usage WHERE owner_id = $1 AND day >= $2::date GROUP BY 1`, o, day0.Format("2006-01-02"))
	if err != nil {
		return SessionLLMReport{}, err
	}
	today := now.Format("2006-01-02")
	for rows.Next() {
		var key string
		var d SessionLLMDay
		if err := rows.Scan(&key, &d.Requests, &d.CostUSD); err != nil {
			rows.Close()
			return SessionLLMReport{}, err
		}
		if x := byDay[key]; x != nil {
			x.Requests, x.CostUSD = d.Requests, d.CostUSD
		}
		if key == today {
			rep.Today = d.CostUSD
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return SessionLLMReport{}, err
	}

	rows, err = s.pool.Query(ctx, `SELECT u.session_id, max(u.model), coalesce(sum(u.requests), 0)::bigint,
		coalesce(sum(u.input_tokens), 0)::bigint, coalesce(sum(u.output_tokens), 0)::bigint,
		coalesce(sum(u.cache_read_tokens), 0)::bigint, coalesce(sum(u.cache_write_tokens), 0)::bigint,
		coalesce(sum(u.cost_usd), 0)::float8,
		(SELECT count(*) FROM chief_turn_usage t WHERE t.owner_id = $1 AND t.session_id = u.session_id
			AND t.created_at >= ($2::date)::timestamp AT TIME ZONE 'UTC')
		FROM chief_session_llm_usage u WHERE u.owner_id = $1 AND u.day >= $2::date
		GROUP BY u.session_id ORDER BY sum(u.cost_usd) DESC, u.session_id`, o, day0.Format("2006-01-02"))
	if err != nil {
		return SessionLLMReport{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var r SessionLLMRow
		if err := rows.Scan(&r.SessionID, &r.Model, &r.Requests, &r.Tokens.Input, &r.Tokens.Output, &r.Tokens.CacheRead,
			&r.Tokens.CacheCreation, &r.CostUSD, &r.Turns); err != nil {
			return SessionLLMReport{}, err
		}
		rep.Sessions = append(rep.Sessions, r)
	}
	return rep, rows.Err()
}
