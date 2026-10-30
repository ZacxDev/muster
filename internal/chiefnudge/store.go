package chiefnudge

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

// Owner is a user the sweep visits.
type Owner struct {
	ID   int64
	Name string // display name, "" for none
}

// Store is chief_nudges.
type Store interface {
	// Owners is every user who is not disabled (system scope).
	Owners(ctx context.Context) ([]Owner, error)
	// Decided reports whether the caller's session (kind, key) already has a
	// decision for streak.
	Decided(ctx context.Context, kind, key, streak string) (bool, error)
	// Insert records d for the caller, with its audit row; inserted false (and
	// nothing written) when another decision for the same streak won.
	Insert(ctx context.Context, d Decision, action string) (Decision, bool, error)
	// NudgesForAnchor counts the caller's nudges (would_send, queued, sent) to
	// (kind, key) since the owner message anchor.
	NudgesForAnchor(ctx context.Context, kind, key, anchor string) (int, error)
	// NudgesSince counts every owner's LIVE nudges (queued, sent) decided at
	// or after since: the global hourly cap, which bounds what is sent and
	// never holds shadow mode back (system scope).
	NudgesSince(ctx context.Context, since time.Time) (int, error)
	// Injected is what the sweep pasted into the caller's session (kind, key):
	// every live nudge and relay text.
	Injected(ctx context.Context, kind, key string) ([]string, error)
	// Pending is every owner's live decision still in flight (queued, asked,
	// relay_queued), system scope.
	Pending(ctx context.Context, limit int) ([]Decision, error)
	// Get is the caller's decision id.
	Get(ctx context.Context, id int64) (Decision, error)
	// ByAsk is the caller's decision that raised ask id.
	ByAsk(ctx context.Context, askID int64) (Decision, error)
	// Move sets the caller's decision id from one of from to outcome, with its
	// detail and (when non-zero/non-empty) its ask, queued row and relay text,
	// writing action's audit row when action is not ""; moved false when the
	// row was not in one of from.
	Move(ctx context.Context, id int64, from []string, m Move, action string) (bool, error)
	// List is the caller's newest decisions.
	List(ctx context.Context, limit int) ([]Decision, error)
}

// Move is what Store.Move writes.
type Move struct {
	Outcome     string
	Detail      string
	AskID       int64
	SendQueueID int64
	RelayText   string
}

// ErrNotFound: no such decision among the caller's.
var ErrNotFound = errors.New("chiefnudge: no such decision")

// Object is the audit_log object naming decision id.
func Object(id int64) string { return "chief_nudge:" + strconv.FormatInt(id, 10) }

// PG is Store on Postgres, under row-level security.
type PG struct{ pool *pgxpool.Pool }

// NewPG is Store over pool.
func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool} }

const cols = `id, owner_id, session_kind, session_key, mirror_host, mirror_session, mirror_window, coalesce(agent_id, 0), coalesce(chat_session_id, 0),
	title, streak_key, anchor_key, coalesce(idle_since, 'epoch'::timestamptz), decided_at, decision, message, quote_id,
	coalesce(quote_at, 'epoch'::timestamptz), quote_text, covered_action, question, options, reason, mode, outcome, detail,
	coalesce(ask_id, 0), coalesce(send_queue_id, 0), relay_text, updated_at,
	jev_label, jev_needs_input, jev_asks_permission, jev_neither, jev_ask_score, jev_model, jev_latency_ms, jev_error`

func scan(row pgx.Row) (Decision, error) {
	var d Decision
	var opts []byte
	err := row.Scan(&d.ID, &d.Owner, &d.Kind, &d.Key, &d.Host, &d.TmuxSession, &d.Window, &d.AgentID, &d.ChatSessionID,
		&d.Title, &d.StreakKey, &d.AnchorKey, &d.IdleSince, &d.DecidedAt, &d.Decision, &d.Message, &d.QuoteID,
		&d.QuoteAt, &d.QuoteText, &d.Covered, &d.Question, &opts, &d.Reason, &d.Mode, &d.Outcome, &d.Detail,
		&d.AskID, &d.SendQueueID, &d.RelayText, &d.UpdatedAt,
		&d.JevLabel, &d.JevNeedsInput, &d.JevAsksPermission, &d.JevNeither, &d.JevAskScore, &d.JevModel, &d.JevLatencyMS, &d.JevError)
	if err != nil {
		return Decision{}, err
	}
	for _, t := range []*time.Time{&d.IdleSince, &d.QuoteAt} {
		if t.Unix() == 0 {
			*t = time.Time{}
		}
	}
	if len(opts) > 0 {
		_ = json.Unmarshal(opts, &d.Options)
	}
	return d, nil
}

func collect(rows pgx.Rows, err error) ([]Decision, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Decision
	for rows.Next() {
		d, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func nullID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

// audit appends one audit_log row about decision id as ctx's principal, in tx.
func audit(ctx context.Context, tx pgx.Tx, action string, id int64, detail map[string]any) error {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return errors.New("chiefnudge: an audited write needs a user principal")
	}
	return users.AuditTx(ctx, tx, users.Actor{UserID: p.UserID, Via: p.Via}, action, Object(id), detail)
}

// auditDetail is what every audit row carries about d: never the session's
// text, only the decision's own fields and where it went.
func auditDetail(d Decision) map[string]any {
	m := map[string]any{"session_kind": d.Kind, "session_key": d.Key, "decision": d.Decision, "mode": d.Mode, "outcome": d.Outcome}
	if d.Host != "" {
		m["host"] = d.Host
	}
	if d.QuoteID != "" {
		m["quote_id"] = d.QuoteID
	}
	if d.AskID > 0 {
		m["ask_id"] = d.AskID
	}
	if d.SendQueueID > 0 {
		m["send_queue_id"] = d.SendQueueID
	}
	if d.Detail != "" {
		m["detail"] = d.Detail
	}
	return m
}

// sys is the store's one system-scope helper (internal/authz's ledger names
// it): the sweep's three cross-owner reads — the owners it visits, the global
// hourly count and the live decisions in flight. Every one of those decisions
// is then acted on as its own owner, never under System.
func sys(ctx context.Context) context.Context { return authz.System(ctx) }

// Owners implements Store.
func (p *PG) Owners(ctx context.Context) ([]Owner, error) {
	rows, err := p.pool.Query(sys(ctx), `SELECT id, coalesce(display_name, '') FROM users WHERE disabled_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Owner
	for rows.Next() {
		var o Owner
		if err := rows.Scan(&o.ID, &o.Name); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// Decided implements Store.
func (p *PG) Decided(ctx context.Context, kind, key, streak string) (bool, error) {
	var n int
	err := p.pool.QueryRow(ctx, `SELECT count(*) FROM chief_nudges WHERE session_kind = $1 AND session_key = $2 AND streak_key = $3`,
		kind, key, streak).Scan(&n)
	return n > 0, err
}

// Insert implements Store.
func (p *PG) Insert(ctx context.Context, d Decision, action string) (Decision, bool, error) {
	d = Clean(d)
	opts, err := json.Marshal(d.Options)
	if err != nil {
		return Decision{}, false, err
	}
	var out Decision
	inserted := false
	err = pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `INSERT INTO chief_nudges (session_kind, session_key, mirror_host, mirror_session, mirror_window, agent_id,
				title, streak_key, anchor_key, idle_since, decision, message, quote_id, quote_at, quote_text, covered_action,
				question, options, reason, mode, outcome, detail, ask_id, send_queue_id, relay_text, chat_session_id,
				jev_label, jev_needs_input, jev_asks_permission, jev_neither, jev_ask_score, jev_model, jev_latency_ms, jev_error)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26,
				$27, $28, $29, $30, $31, $32, $33, $34)
			ON CONFLICT ON CONSTRAINT chief_nudges_one_per_streak DO NOTHING
			RETURNING `+cols,
			d.Kind, d.Key, d.Host, d.TmuxSession, d.Window, nullID(d.AgentID), d.Title, d.StreakKey, d.AnchorKey, nullTime(d.IdleSince),
			d.Decision, d.Message, d.QuoteID, nullTime(d.QuoteAt), d.QuoteText, d.Covered, d.Question, opts, d.Reason, d.Mode,
			d.Outcome, d.Detail, nullID(d.AskID), nullID(d.SendQueueID), d.RelayText, nullID(d.ChatSessionID),
			d.JevLabel, d.JevNeedsInput, d.JevAsksPermission, d.JevNeither, d.JevAskScore, d.JevModel, d.JevLatencyMS, d.JevError)
		var err error
		out, err = scan(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		inserted = true
		if action == "" {
			return nil
		}
		return audit(ctx, tx, action, out.ID, auditDetail(out))
	})
	if err != nil {
		return Decision{}, false, err
	}
	return out, inserted, nil
}

// nudgeOutcomes are the outcomes that count as a nudge given (the caps).
var nudgeOutcomes = []string{OutcomeWouldSend, OutcomeQueued, OutcomeSent}

// NudgesForAnchor implements Store.
func (p *PG) NudgesForAnchor(ctx context.Context, kind, key, anchor string) (int, error) {
	var n int
	err := p.pool.QueryRow(ctx, `SELECT count(*) FROM chief_nudges WHERE session_kind = $1 AND session_key = $2
		AND anchor_key = $3 AND decision = 'nudge' AND outcome = ANY($4)`, kind, key, anchor, nudgeOutcomes).Scan(&n)
	return n, err
}

// NudgesSince implements Store.
func (p *PG) NudgesSince(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := p.pool.QueryRow(sys(ctx), `SELECT count(*) FROM chief_nudges WHERE decided_at >= $1
		AND decision = 'nudge' AND mode = 'live' AND outcome = ANY($2)`, since, nudgeOutcomes).Scan(&n)
	return n, err
}

// Injected implements Store.
func (p *PG) Injected(ctx context.Context, kind, key string) ([]string, error) {
	rows, err := p.pool.Query(ctx, `SELECT message FROM chief_nudges WHERE session_kind = $1 AND session_key = $2
			AND mode = 'live' AND message <> '' AND outcome IN ('queued', 'sent', 'failed')
		UNION ALL
		SELECT relay_text FROM chief_nudges WHERE session_kind = $1 AND session_key = $2 AND relay_text <> ''`, kind, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Pending implements Store.
func (p *PG) Pending(ctx context.Context, limit int) ([]Decision, error) {
	return collect(p.pool.Query(sys(ctx), `SELECT `+cols+` FROM chief_nudges
		WHERE outcome IN ('queued', 'asked', 'relay_queued') ORDER BY id LIMIT $1`, limit))
}

// Get implements Store.
func (p *PG) Get(ctx context.Context, id int64) (Decision, error) {
	d, err := scan(p.pool.QueryRow(ctx, `SELECT `+cols+` FROM chief_nudges WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Decision{}, ErrNotFound
	}
	return d, err
}

// ByAsk implements Store.
func (p *PG) ByAsk(ctx context.Context, askID int64) (Decision, error) {
	d, err := scan(p.pool.QueryRow(ctx, `SELECT `+cols+` FROM chief_nudges WHERE ask_id = $1`, askID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Decision{}, ErrNotFound
	}
	return d, err
}

// Move implements Store.
func (p *PG) Move(ctx context.Context, id int64, from []string, m Move, action string) (bool, error) {
	clean := Clean(Decision{Detail: m.Detail, RelayText: m.RelayText})
	moved := false
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		d, err := scan(tx.QueryRow(ctx, `UPDATE chief_nudges SET outcome = $3, detail = $4,
				ask_id = coalesce($5, ask_id), send_queue_id = coalesce($6, send_queue_id),
				relay_text = CASE WHEN $7 = '' THEN relay_text ELSE $7 END, updated_at = now()
			WHERE id = $1 AND outcome = ANY($2) RETURNING `+cols,
			id, from, m.Outcome, clean.Detail, nullID(m.AskID), nullID(m.SendQueueID), clean.RelayText))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		moved = true
		if action == "" {
			return nil
		}
		return audit(ctx, tx, action, d.ID, auditDetail(d))
	})
	return moved, err
}

// List implements Store.
func (p *PG) List(ctx context.Context, limit int) ([]Decision, error) {
	return collect(p.pool.Query(ctx, `SELECT `+cols+` FROM chief_nudges ORDER BY decided_at DESC, id DESC LIMIT $1`, limit))
}
