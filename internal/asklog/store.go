package asklog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/redact"
)

// Store is the ask log, under row-level security: every method but Purge acts
// as ctx's principal, on its rows only.
type Store interface {
	// Open records a. created is false, and nothing is written, when the
	// caller already has a row of a.Kind for a.SourceRef (asks_one_per_source);
	// id is then that row's. A plain-text ask (SourceRef "") is always new.
	Open(ctx context.Context, a Ask) (id int64, created bool, err error)
	// Resolve records how ask id ended. ok is false, and nothing is written,
	// when the row is not the caller's, has already ended, or ended softly
	// (moved on, expired, superseded, gone) and r is not an answer.
	Resolve(ctx context.Context, id int64, r Resolution) (Ask, bool, error)
	// ResolveRef is Resolve for the caller's row of one of kinds whose
	// source_ref is ref.
	ResolveRef(ctx context.Context, kinds []string, ref string, r Resolution) (Ask, bool, error)
	// Get is the caller's row id; ok false when it is none of theirs.
	Get(ctx context.Context, id int64) (Ask, bool, error)
	// List is the caller's newest rows, at most limit.
	List(ctx context.Context, limit int) ([]Ask, error)
	// Purge is the retention sweep, for every owner (the system scope): it
	// clears the text of rows asked before now-textTTL (text_purged_at) and
	// deletes rows asked before now-rowTTL. It returns how many of each.
	Purge(ctx context.Context, now time.Time, textTTL, rowTTL time.Duration) (purged, deleted int, err error)
}

// PG is Store on Postgres.
type PG struct{ pool *pgxpool.Pool }

// NewPG is Store over pool.
func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool} }

// ErrBadAsk: a write refused before it reached the database.
var ErrBadAsk = errors.New("asklog: not a valid ask")

// sys is the store's one system-scope helper: the retention sweep, which
// clears text and deletes rows BY AGE ONLY, returning counts (internal/authz's
// systemLedger lists it).
func sys(ctx context.Context) context.Context { return authz.System(ctx) }

const cols = `id, owner_id, kind, runtime, mirror_host, mirror_session, mirror_window, claude_session,
	coalesce(agent_id, 0), coalesce(chat_session_id, 0), repo, cwd, coalesce(episode_id, 0), source_ref,
	question, question_norm, options,
	jev_label, jev_needs_input, jev_asks_permission, jev_neither, jev_ask_score, jev_model, jev_error,
	answer, answered_by, answer_provenance, outcome, asked_at,
	coalesce(answered_at, 'epoch'::timestamptz), coalesce(text_purged_at, 'epoch'::timestamptz)`

func scan(row pgx.Row) (Ask, error) {
	var a Ask
	var opts []byte
	err := row.Scan(&a.ID, &a.Owner, &a.Kind, &a.Runtime, &a.Host, &a.TmuxSession, &a.Window, &a.ClaudeSession,
		&a.AgentID, &a.ChatSessionID, &a.Repo, &a.Cwd, &a.EpisodeID, &a.SourceRef,
		&a.Question, &a.QuestionNorm, &opts,
		&a.Jev.Label, &a.Jev.NeedsInput, &a.Jev.AsksPermission, &a.Jev.Neither, &a.Jev.AskScore, &a.Jev.Model, &a.Jev.Err,
		&a.Answer, &a.AnsweredBy, &a.Provenance, &a.Outcome, &a.AskedAt, &a.AnsweredAt, &a.TextPurgedAt)
	if err != nil {
		return Ask{}, err
	}
	a.Options = json.RawMessage(opts)
	for _, t := range []*time.Time{&a.AnsweredAt, &a.TextPurgedAt} {
		if t.Unix() == 0 {
			*t = time.Time{}
		}
	}
	return a, nil
}

func nullID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

// redactedText is s redacted (redact.Secrets: the one redactor) and cut to n
// runes. 🔴 Redaction BEFORE the cut, so a cut cannot split a secret into a
// fragment the redactor no longer recognises.
func redactedText(s string, n int) string { return cut(redact.Secrets(s), n) }

// redactedOptions is raw (a JSON array; nil or empty is []) with every string
// in it redacted, re-encoded. Walked as values, not as one string, so the
// redactor can never break the JSON it runs over.
func redactedOptions(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return []byte("[]"), nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("%w: options are not JSON: %v", ErrBadAsk, err)
	}
	if _, ok := v.([]any); !ok {
		return nil, fmt.Errorf("%w: options are not an array", ErrBadAsk)
	}
	return json.Marshal(redactValue(v))
}

func redactValue(v any) any {
	switch x := v.(type) {
	case string:
		return redact.Secrets(x)
	case []any:
		for i := range x {
			x[i] = redactValue(x[i])
		}
		return x
	case map[string]any:
		for k := range x {
			x[k] = redactValue(x[k])
		}
		return x
	}
	return v
}

// Open implements Store.
func (p *PG) Open(ctx context.Context, a Ask) (int64, bool, error) {
	if !validKind(a.Kind) || (a.Runtime != RuntimeClaudeCode && a.Runtime != RuntimeOpenCode && a.Runtime != RuntimeAgent) {
		return 0, false, ErrBadAsk
	}
	opts, err := redactedOptions(a.Options)
	if err != nil {
		return 0, false, err
	}
	q := redactedText(a.Question, MaxQuestionRunes)
	// The normal form is computed from the REDACTED question, so it can hold
	// nothing the question does not.
	qn := cut(Normalise(q), MaxQuestionNormRunes)
	at := a.AskedAt
	if at.IsZero() {
		at = time.Now()
	}
	ref := cut(a.SourceRef, MaxRefRunes)
	var id int64
	err = p.pool.QueryRow(ctx, `INSERT INTO asks (kind, runtime, mirror_host, mirror_session, mirror_window, claude_session,
			agent_id, chat_session_id, repo, cwd, episode_id, source_ref, question, question_norm, options,
			jev_label, jev_needs_input, jev_asks_permission, jev_neither, jev_ask_score, jev_model, jev_error, asked_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)
		ON CONFLICT (owner_id, kind, source_ref) WHERE source_ref <> '' DO NOTHING
		RETURNING id`,
		a.Kind, a.Runtime, cut(a.Host, MaxNameRunes), cut(a.TmuxSession, MaxNameRunes), cut(a.Window, MaxWindowRunes),
		cut(a.ClaudeSession, MaxNameRunes), nullID(a.AgentID), nullID(a.ChatSessionID), cut(a.Repo, MaxNameRunes),
		cut(a.Cwd, MaxCwdRunes), nullID(a.EpisodeID), ref, q, qn, opts,
		a.Jev.Label, a.Jev.NeedsInput, a.Jev.AsksPermission, a.Jev.Neither, a.Jev.AskScore, cut(a.Jev.Model, 200), cut(a.Jev.Err, 100), at,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// The source's row exists already: a capture that ran twice.
		err = p.pool.QueryRow(ctx, `SELECT id FROM asks WHERE kind = $1 AND source_ref = $2 AND source_ref <> ''`, a.Kind, ref).Scan(&id)
		if err != nil {
			return 0, false, fmt.Errorf("asklog: open: %w", err)
		}
		return id, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("asklog: open: %w", err)
	}
	return id, true, nil
}

// resolveSet is the UPDATE's SET and guard, shared by Resolve and ResolveRef.
//
// 🔴 ONE END PER ASK, EXCEPT A SOFT ONE: a row that ended is not moved again
// — unless it ended softly (moved on, expired, superseded, gone) and this is
// an answer, which replaces it (see soft). answered_at is set only for an
// answer; the answer's text is written only while the row's text is held.
const resolveSet = `SET outcome = $2,
		answer = CASE WHEN text_purged_at IS NULL THEN $3 ELSE '' END,
		answered_by = $4, answer_provenance = $5,
		answered_at = CASE WHEN $7 THEN $6::timestamptz ELSE NULL END
	WHERE (outcome = '' OR (outcome = ANY($8) AND $7))`

func resolveArgs(r Resolution) ([]any, error) {
	if !validOutcome(r.Outcome) {
		return nil, ErrBadAsk
	}
	at := r.At
	if at.IsZero() {
		at = time.Now()
	}
	answer := ""
	if r.Answer != "" {
		answer = redactedText(r.Answer, MaxAnswerRunes)
	}
	return []any{r.Outcome, answer, r.AnsweredBy, r.Provenance, at, Answered(r.Outcome),
		[]string{OutcomeMovedOn, OutcomeExpired, OutcomeGone, OutcomeSuperseded}}, nil
}

// Resolve implements Store.
func (p *PG) Resolve(ctx context.Context, id int64, r Resolution) (Ask, bool, error) {
	args, err := resolveArgs(r)
	if err != nil {
		return Ask{}, false, err
	}
	a, err := scan(p.pool.QueryRow(ctx, `UPDATE asks `+resolveSet+` AND id = $1 RETURNING `+cols, append([]any{id}, args...)...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Ask{}, false, nil
	}
	if err != nil {
		return Ask{}, false, fmt.Errorf("asklog: resolve: %w", err)
	}
	return a, true, nil
}

// ResolveRef implements Store.
func (p *PG) ResolveRef(ctx context.Context, kinds []string, ref string, r Resolution) (Ask, bool, error) {
	if ref == "" || len(kinds) == 0 {
		return Ask{}, false, ErrBadAsk
	}
	args, err := resolveArgs(r)
	if err != nil {
		return Ask{}, false, err
	}
	a, err := scan(p.pool.QueryRow(ctx, `UPDATE asks `+resolveSet+` AND source_ref = $1 AND kind = ANY($9) RETURNING `+cols,
		append(append([]any{ref}, args...), kinds)...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Ask{}, false, nil
	}
	if err != nil {
		return Ask{}, false, fmt.Errorf("asklog: resolve: %w", err)
	}
	return a, true, nil
}

// Get implements Store.
func (p *PG) Get(ctx context.Context, id int64) (Ask, bool, error) {
	a, err := scan(p.pool.QueryRow(ctx, `SELECT `+cols+` FROM asks WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Ask{}, false, nil
	}
	if err != nil {
		return Ask{}, false, fmt.Errorf("asklog: get: %w", err)
	}
	return a, true, nil
}

// List implements Store.
func (p *PG) List(ctx context.Context, limit int) ([]Ask, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := p.pool.Query(ctx, `SELECT `+cols+` FROM asks ORDER BY asked_at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("asklog: list: %w", err)
	}
	defer rows.Close()
	var out []Ask
	for rows.Next() {
		a, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Purge implements Store.
//
// 🔴 THE TEXT TTL IS STRICT: a row asked exactly textTTL ago keeps its text
// until the next sweep; one asked a moment earlier loses it. The rest of the
// row (kind, where, scores, who answered, latency, outcome) is kept until
// rowTTL, so the metrics and rule statistics outlive the text.
func (p *PG) Purge(ctx context.Context, now time.Time, textTTL, rowTTL time.Duration) (int, int, error) {
	if textTTL <= 0 || rowTTL <= 0 {
		return 0, 0, fmt.Errorf("%w: a retention must be positive", ErrBadAsk)
	}
	sctx := sys(ctx)
	// The rows past the row TTL go first, so a row is never purged and then
	// deleted in one sweep (the counts say what each step did).
	tag, err := p.pool.Exec(sctx, `DELETE FROM asks WHERE asked_at < $1`, now.Add(-rowTTL))
	if err != nil {
		return 0, 0, fmt.Errorf("asklog: purge rows: %w", err)
	}
	deleted := int(tag.RowsAffected())
	tag, err = p.pool.Exec(sctx, `UPDATE asks SET question = '', question_norm = '', answer = '', proposed_answer = '', text_purged_at = $1
		WHERE text_purged_at IS NULL AND asked_at < $2`, now, now.Add(-textTTL))
	if err != nil {
		return 0, deleted, fmt.Errorf("asklog: purge text: %w", err)
	}
	return int(tag.RowsAffected()), deleted, nil
}
