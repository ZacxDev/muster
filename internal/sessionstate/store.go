package sessionstate

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/chiefnudge"
)

// Episode is one row of session_episodes: one stop of one session.
type Episode struct {
	ID    int64
	Owner int64
	// Kind and Key name the session as the stall sweep keys it.
	Kind, Key string
	Seq       int
	State     string // StateNeedsYou or StateStalled
	// Waiting: opened (or last seen) waiting on a prompt rather than idle.
	Waiting bool
	// Asked: the leave rule's input (Asked).
	Asked bool
	// Where the session is.
	Runtime                   string
	Host, TmuxSession, Window string
	Index                     int
	AgentID, ChatSessionID    int64
	StreakKey                 string
	// The stall classifier on the session's last message, for the streak it
	// classified (ClassifiedStreak; "" = not classified yet).
	ClassifiedStreak string
	Jev              Classification
	OpenedAt         time.Time
	StateSince       time.Time
	BusySince        time.Time // zero: not working
	BusySeen         time.Time
	EndedAt          time.Time // zero: open
	EndReason        string
	PushedAt         time.Time
	ClearedAt        time.Time
	// Quiet is why the episode is not pushed now (a Quiet* constant), "" for
	// none. Not final: a later reason to push clears it (ClaimPush).
	Quiet string
}

// Open reports whether e has not ended.
func (e Episode) Open() bool { return e.EndedAt.IsZero() }

// Classification is the stall classifier's answer as an episode keeps it
// (chiefnudge.Result's fields; Err its error class, "" = none).
type Classification struct {
	Label                               string
	NeedsInput, AsksPermission, Neither float64
	AskScore                            float64
	Model                               string
	Err                                 string
}

// Store is session_episodes, under row-level security: every method but
// Owners acts as ctx's principal, on its rows only.
type Store interface {
	// Owners is every user who is not disabled: the owners the state pass
	// visits, each then as its own principal. The stall nudges' read
	// (chiefnudge.PG.Owners), one query and one system-scope helper.
	Owners(ctx context.Context) ([]int64, error)
	// Open opens an episode for the caller's session (e.Kind, e.Key), at the
	// next seq. 🔴 THE CLAIM: claimed is false, and nothing is written, when
	// the session already has an open episode (session_episodes_one_open) —
	// another pass, or another replica, won it.
	Open(ctx context.Context, e Episode) (Episode, bool, error)
	// OpenEpisodes is the caller's open episodes, oldest first.
	OpenEpisodes(ctx context.Context) ([]Episode, error)
	// Observe writes what a pass saw of open episode id: its state, whether it
	// waits, where it is and its streak, and its run of work.
	Observe(ctx context.Context, id int64, o Observation) error
	// SetClassification stores the classifier's answer for streak on open
	// episode id, and the leave rule's input it gives.
	SetClassification(ctx context.Context, id int64, streak string, c Classification, asked bool) error
	// OpenFor is the caller's open episode for session (kind, key), if any.
	OpenFor(ctx context.Context, kind, key string) (Episode, bool, error)
	// End ends open episode id for reason; ended false when it had already
	// ended.
	End(ctx context.Context, id int64, reason string, at time.Time) (bool, error)
	// Get is the caller's episode id; ok false when it is none of theirs.
	Get(ctx context.Context, id int64) (Episode, bool, error)
	// ClaimPush claims open episode id's push at at. 🔴 THE PUSH CLAIM:
	// claimed is true for exactly one caller — the one whose write took a row
	// with no pushed_at — and only that caller sends the push. It clears the
	// episode's quiet reason.
	ClaimPush(ctx context.Context, id int64, at time.Time) (bool, error)
	// SetQuiet records why open, unpushed episode id is quiet now (a Quiet*
	// constant).
	SetQuiet(ctx context.Context, id int64, reason string) error
	// PreviousPush is when the caller's session (kind, key) last had an
	// episode before seq pushed; zero for never.
	PreviousPush(ctx context.Context, kind, key string, seq int) (time.Time, error)
	// Uncleared is the caller's ended episodes whose push was shown and not
	// yet closed (cleared_at null): what the restart rule clears.
	Uncleared(ctx context.Context) ([]Episode, error)
	// MarkCleared records that ended episode id's notification was closed.
	MarkCleared(ctx context.Context, id int64, at time.Time) error
}

// The reasons an episode is quiet (Episode.Quiet, migration 0077).
const (
	QuietLeft        = "left"         // the chief said leave it, and the session asked nothing
	QuietNudged      = "nudged"       // the chief's nudge to it was sent
	QuietOwnerLast   = "owner_last"   // the owner spoke last (known without a classifier)
	QuietRepushFloor = "repush_floor" // held: its previous episode pushed under the repush floor ago; pushed once it passes
)

func validQuiet(q string) bool {
	return q == QuietLeft || q == QuietNudged || q == QuietOwnerLast || q == QuietRepushFloor
}

// Observation is what Store.Observe writes.
type Observation struct {
	State                     string
	Waiting                   bool
	Host, TmuxSession, Window string
	Index                     int
	StreakKey                 string
	BusySince, BusySeen       time.Time
	At                        time.Time
}

// PG is Store on Postgres.
type PG struct{ pool *pgxpool.Pool }

// NewPG is Store over pool.
func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool} }

// ErrBadEpisode: an episode a write refused before it reached the database.
var ErrBadEpisode = errors.New("sessionstate: not a valid episode")

const cols = `id, owner_id, session_kind, session_key, seq, state, waiting, asked, runtime, mirror_host, mirror_session, mirror_window,
	window_index, coalesce(agent_id, 0), coalesce(chat_session_id, 0), streak_key, classified_streak,
	jev_label, jev_needs_input, jev_asks_permission, jev_neither, jev_ask_score, jev_model, jev_error,
	opened_at, state_since, coalesce(busy_since, 'epoch'::timestamptz), coalesce(busy_seen_at, 'epoch'::timestamptz),
	coalesce(ended_at, 'epoch'::timestamptz), end_reason, coalesce(pushed_at, 'epoch'::timestamptz), coalesce(cleared_at, 'epoch'::timestamptz), quiet`

func scan(row pgx.Row) (Episode, error) {
	var e Episode
	err := row.Scan(&e.ID, &e.Owner, &e.Kind, &e.Key, &e.Seq, &e.State, &e.Waiting, &e.Asked, &e.Runtime, &e.Host, &e.TmuxSession, &e.Window,
		&e.Index, &e.AgentID, &e.ChatSessionID, &e.StreakKey, &e.ClassifiedStreak,
		&e.Jev.Label, &e.Jev.NeedsInput, &e.Jev.AsksPermission, &e.Jev.Neither, &e.Jev.AskScore, &e.Jev.Model, &e.Jev.Err,
		&e.OpenedAt, &e.StateSince, &e.BusySince, &e.BusySeen, &e.EndedAt, &e.EndReason, &e.PushedAt, &e.ClearedAt, &e.Quiet)
	if err != nil {
		return Episode{}, err
	}
	for _, t := range []*time.Time{&e.BusySince, &e.BusySeen, &e.EndedAt, &e.PushedAt, &e.ClearedAt} {
		if t.Unix() == 0 {
			*t = time.Time{}
		}
	}
	return e, nil
}

func collect(rows pgx.Rows, err error) ([]Episode, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Episode
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
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

// cut bounds s to n runes.
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func validState(s string) bool { return s == StateNeedsYou || s == StateStalled }

// Owners implements Store, through chiefnudge.PG.Owners.
func (p *PG) Owners(ctx context.Context) ([]int64, error) {
	os, err := chiefnudge.NewPG(p.pool).Owners(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(os))
	for _, o := range os {
		out = append(out, o.ID)
	}
	return out, nil
}

// Open implements Store.
func (p *PG) Open(ctx context.Context, e Episode) (Episode, bool, error) {
	if (e.Kind != KindMirror && e.Kind != KindAgent) || e.Key == "" || !validState(e.State) {
		return Episode{}, false, ErrBadEpisode
	}
	// 🔴 ON CONFLICT DO NOTHING WITH NO TARGET: both unique indexes refuse a
	// second open episode — session_episodes_one_open (the claim) and the seq
	// key, which two passes computing the same next seq would both reach.
	row := p.pool.QueryRow(ctx, `INSERT INTO session_episodes (session_kind, session_key, seq, state, waiting, asked, runtime,
			mirror_host, mirror_session, mirror_window, window_index, agent_id, chat_session_id, streak_key)
		SELECT $1, $2, coalesce(max(seq), 0) + 1, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13
		  FROM session_episodes WHERE session_kind = $1 AND session_key = $2
		ON CONFLICT DO NOTHING
		RETURNING `+cols,
		e.Kind, e.Key, e.State, e.Waiting, e.Asked, cut(e.Runtime, 32), cut(e.Host, 200), cut(e.TmuxSession, 200), cut(e.Window, 32),
		e.Index, nullID(e.AgentID), nullID(e.ChatSessionID), cut(e.StreakKey, 100))
	out, err := scan(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Episode{}, false, nil
	}
	if err != nil {
		return Episode{}, false, err
	}
	return out, true, nil
}

// OpenFor implements Store.
func (p *PG) OpenFor(ctx context.Context, kind, key string) (Episode, bool, error) {
	e, err := scan(p.pool.QueryRow(ctx, `SELECT `+cols+` FROM session_episodes WHERE session_kind = $1 AND session_key = $2 AND ended_at IS NULL`,
		kind, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return Episode{}, false, nil
	}
	if err != nil {
		return Episode{}, false, err
	}
	return e, true, nil
}

// OpenEpisodes implements Store.
func (p *PG) OpenEpisodes(ctx context.Context) ([]Episode, error) {
	return collect(p.pool.Query(ctx, `SELECT `+cols+` FROM session_episodes WHERE ended_at IS NULL ORDER BY opened_at, id`))
}

// Observe implements Store. The state's since-time moves only when the state
// changes.
func (p *PG) Observe(ctx context.Context, id int64, o Observation) error {
	if !validState(o.State) {
		return ErrBadEpisode
	}
	_, err := p.pool.Exec(ctx, `UPDATE session_episodes SET
			state_since = CASE WHEN state = $2 THEN state_since ELSE $11 END,
			state = $2, waiting = $3, mirror_host = $4, mirror_session = $5, mirror_window = $6, window_index = $7,
			streak_key = $8, busy_since = $9, busy_seen_at = $10, updated_at = $11
		WHERE id = $1 AND ended_at IS NULL`,
		id, o.State, o.Waiting, cut(o.Host, 200), cut(o.TmuxSession, 200), cut(o.Window, 32), o.Index, cut(o.StreakKey, 100),
		nullTime(o.BusySince), nullTime(o.BusySeen), o.At)
	return err
}

// SetClassification implements Store.
func (p *PG) SetClassification(ctx context.Context, id int64, streak string, c Classification, asked bool) error {
	_, err := p.pool.Exec(ctx, `UPDATE session_episodes SET classified_streak = $2, jev_label = $3, jev_needs_input = $4,
			jev_asks_permission = $5, jev_neither = $6, jev_ask_score = $7, jev_model = $8, jev_error = $9, asked = $10, updated_at = now()
		WHERE id = $1 AND ended_at IS NULL`,
		id, cut(streak, 100), c.Label, c.NeedsInput, c.AsksPermission, c.Neither, c.AskScore, cut(c.Model, 200), cut(c.Err, 100), asked)
	return err
}

// End implements Store.
func (p *PG) End(ctx context.Context, id int64, reason string, at time.Time) (bool, error) {
	if reason != EndWorked && reason != EndGone && reason != EndOwner {
		return false, ErrBadEpisode
	}
	tag, err := p.pool.Exec(ctx, `UPDATE session_episodes SET ended_at = $2, end_reason = $3, updated_at = $2
		WHERE id = $1 AND ended_at IS NULL`, id, at, reason)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Get implements Store.
func (p *PG) Get(ctx context.Context, id int64) (Episode, bool, error) {
	e, err := scan(p.pool.QueryRow(ctx, `SELECT `+cols+` FROM session_episodes WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Episode{}, false, nil
	}
	if err != nil {
		return Episode{}, false, err
	}
	return e, true, nil
}

// ClaimPush implements Store.
func (p *PG) ClaimPush(ctx context.Context, id int64, at time.Time) (bool, error) {
	tag, err := p.pool.Exec(ctx, `UPDATE session_episodes SET pushed_at = $2, quiet = '', updated_at = $2
		WHERE id = $1 AND ended_at IS NULL AND pushed_at IS NULL`, id, at)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// SetQuiet implements Store.
func (p *PG) SetQuiet(ctx context.Context, id int64, reason string) error {
	if !validQuiet(reason) {
		return ErrBadEpisode
	}
	_, err := p.pool.Exec(ctx, `UPDATE session_episodes SET quiet = $2, updated_at = now()
		WHERE id = $1 AND ended_at IS NULL AND pushed_at IS NULL AND quiet <> $2`, id, reason)
	return err
}

// PreviousPush implements Store.
func (p *PG) PreviousPush(ctx context.Context, kind, key string, seq int) (time.Time, error) {
	var at *time.Time
	err := p.pool.QueryRow(ctx, `SELECT max(pushed_at) FROM session_episodes WHERE session_kind = $1 AND session_key = $2 AND seq < $3`,
		kind, key, seq).Scan(&at)
	if err != nil || at == nil {
		return time.Time{}, err
	}
	return *at, nil
}

// Uncleared implements Store.
func (p *PG) Uncleared(ctx context.Context) ([]Episode, error) {
	return collect(p.pool.Query(ctx, `SELECT `+cols+` FROM session_episodes
		WHERE ended_at IS NOT NULL AND pushed_at IS NOT NULL AND cleared_at IS NULL ORDER BY id LIMIT 200`))
}

// MarkCleared implements Store.
func (p *PG) MarkCleared(ctx context.Context, id int64, at time.Time) error {
	_, err := p.pool.Exec(ctx, `UPDATE session_episodes SET cleared_at = $2, updated_at = $2
		WHERE id = $1 AND ended_at IS NOT NULL AND cleared_at IS NULL`, id, at)
	return err
}
