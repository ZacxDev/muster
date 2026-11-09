package plugins

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// SESSION SUCCESSIONS (migration 0099; docs/design/chief-in-agent-view.md,
// phase 2): a new session that continues one the owner was viewing. The chief
// PROPOSES it from a line the owner typed on that session's view; the owner's
// one tap on its card is what starts it.
//
// 🔴 ONE SEAM LEDGER, SIX WRITERS, EACH WITH ITS NAMED CALLERS (a test asserts
// the set over the Go source):
//
//   - CreateSuccession: the proposal. Called only by succession_propose;
//   - the LINK, only inside ReserveHostDispatch's reservation transaction
//     (HostDispatchNew.Succession): decided = started and host_dispatch_id, on
//     a row still UNDECIDED — or, for the base fallback, still linked to the
//     dispatch the caller names (HostDispatchNew.SuccessionPrev). Single use:
//     a second tap's reservation finds no row to link and is rolled back
//     (ErrSuccessionDecided), so it inserts no dispatch either;
//   - DeclineSuccession: decided = declined, on an undecided row. Called only
//     by the card's "Not now";
//   - MarkSuperseded: pred_state = superseded on a STARTED replace row. Called
//     only when the successor's dispatch turns running;
//   - SetSuccessionNotified: notified_at, once. Called only by the 30 s push;
//   - SetSuccessorSeen: successor_seen_at, once. Called only when the
//     successor's own view is opened.

// Succession modes (D1/D2).
const (
	SuccessionReplace = "replace"
	SuccessionSpinoff = "spinoff"
)

// Succession decisions (decided).
const (
	SuccessionOpen     = ""
	SuccessionStarted  = "started"
	SuccessionDeclined = "declined"
)

// Succession is one row as read.
type Succession struct {
	ID, Owner, ChiefAgent, OwnerMessage        int64
	PredKey, PredRuntime, PredHost             string
	PredTmuxSession, PredTmuxWindow, PredTitle string
	PredDispatch                               int64
	Mode                                       string // "" while the owner has not chosen
	Target, Model, Repo, Base                  string
	Kickoff, KickoffSHA256, KickoffRef         string
	KickoffBlock                               int // -1 for the whole message
	AskID                                      int64
	Decided                                    string
	DecidedAt                                  time.Time
	HostDispatchID                             int64
	BaseUsed                                   string
	PredState                                  string
	SuccessorSeenAt, NotifiedAt, CreatedAt     time.Time
}

// SuccessionNew is a proposal's fields.
type SuccessionNew struct {
	ChiefAgent, OwnerMessage                   int64
	PredKey, PredRuntime, PredHost             string
	PredTmuxSession, PredTmuxWindow, PredTitle string
	PredDispatch                               int64
	Mode                                       string
	Target, Model, Repo, Base                  string
	Kickoff, KickoffSHA256, KickoffRef         string
	KickoffBlock                               int // -1 for the whole message
	AskID                                      int64
}

var (
	// ErrSuccessionNotFound: no row of the principal's has that id (or ask).
	ErrSuccessionNotFound = errors.New("plugins: no such succession")
	// ErrSuccessionDecided: the row was already started or declined.
	ErrSuccessionDecided = errors.New("plugins: the succession was already decided")
)

const successionCols = `id, owner_id, chief_agent, owner_message, pred_key, pred_runtime, pred_host, pred_tmux_session,
	pred_tmux_window, pred_title, coalesce(pred_dispatch, 0), coalesce(mode, ''), target, model, repo, base, kickoff,
	kickoff_sha256, kickoff_ref, coalesce(kickoff_block, -1), coalesce(ask_id, 0), decided, coalesce(decided_at, 'epoch'),
	coalesce(host_dispatch_id, 0), base_used, pred_state, coalesce(successor_seen_at, 'epoch'), coalesce(notified_at, 'epoch'),
	created_at`

func scanSuccession(row pgx.Row) (Succession, error) {
	var x Succession
	err := row.Scan(&x.ID, &x.Owner, &x.ChiefAgent, &x.OwnerMessage, &x.PredKey, &x.PredRuntime, &x.PredHost, &x.PredTmuxSession,
		&x.PredTmuxWindow, &x.PredTitle, &x.PredDispatch, &x.Mode, &x.Target, &x.Model, &x.Repo, &x.Base, &x.Kickoff,
		&x.KickoffSHA256, &x.KickoffRef, &x.KickoffBlock, &x.AskID, &x.Decided, &x.DecidedAt,
		&x.HostDispatchID, &x.BaseUsed, &x.PredState, &x.SuccessorSeenAt, &x.NotifiedAt, &x.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Succession{}, ErrSuccessionNotFound
	}
	for _, t := range []*time.Time{&x.DecidedAt, &x.SuccessorSeenAt, &x.NotifiedAt} {
		if t.Unix() == 0 {
			*t = time.Time{}
		}
	}
	return x, err
}

func nullable[T comparable](v, zero T) any {
	if v == zero {
		return nil
	}
	return v
}

// CreateSuccession records the chief's proposal, as the principal.
func (s *PGStore) CreateSuccession(ctx context.Context, n SuccessionNew) (Succession, error) {
	owner, err := hostDispatchOwner(ctx, "a succession")
	if err != nil {
		return Succession{}, err
	}
	var block any
	if n.KickoffBlock >= 0 {
		block = n.KickoffBlock
	}
	return scanSuccession(s.pool.QueryRow(ctx, `INSERT INTO session_successions (owner_id, chief_agent, owner_message, pred_key,
		pred_runtime, pred_host, pred_tmux_session, pred_tmux_window, pred_title, pred_dispatch, mode, target, model, repo, base,
		kickoff, kickoff_sha256, kickoff_ref, kickoff_block, ask_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20) RETURNING `+successionCols,
		owner, n.ChiefAgent, n.OwnerMessage, n.PredKey, n.PredRuntime, n.PredHost, n.PredTmuxSession, n.PredTmuxWindow, n.PredTitle,
		nullable(n.PredDispatch, 0), nullable(n.Mode, ""), n.Target, n.Model, n.Repo, n.Base, n.Kickoff, n.KickoffSHA256, n.KickoffRef,
		block, nullable(n.AskID, 0)))
}

// SuccessionByID is the principal's succession id.
func (s *PGStore) SuccessionByID(ctx context.Context, id int64) (Succession, error) {
	owner, err := hostDispatchOwner(ctx, "a succession")
	if err != nil {
		return Succession{}, err
	}
	return scanSuccession(s.pool.QueryRow(ctx, `SELECT `+successionCols+` FROM session_successions WHERE id = $1 AND owner_id = $2`, id, owner))
}

// SuccessionByAsk is the principal's succession whose card is ask askID.
func (s *PGStore) SuccessionByAsk(ctx context.Context, askID int64) (Succession, error) {
	owner, err := hostDispatchOwner(ctx, "a succession")
	if err != nil {
		return Succession{}, err
	}
	return scanSuccession(s.pool.QueryRow(ctx, `SELECT `+successionCols+` FROM session_successions
		WHERE owner_id = $1 AND ask_id = $2 ORDER BY id DESC LIMIT 1`, owner, askID))
}

// LatestSuccessionOf is the principal's newest succession of the session
// keyed predKey ("my successor").
func (s *PGStore) LatestSuccessionOf(ctx context.Context, predKey string) (Succession, error) {
	owner, err := hostDispatchOwner(ctx, "a succession")
	if err != nil {
		return Succession{}, err
	}
	return scanSuccession(s.pool.QueryRow(ctx, `SELECT `+successionCols+` FROM session_successions
		WHERE owner_id = $1 AND pred_key = $2 ORDER BY id DESC LIMIT 1`, owner, predKey))
}

// SuccessionForDispatch is the principal's succession whose successor is host
// dispatch id ("my predecessor").
func (s *PGStore) SuccessionForDispatch(ctx context.Context, hostDispatchID int64) (Succession, error) {
	owner, err := hostDispatchOwner(ctx, "a succession")
	if err != nil {
		return Succession{}, err
	}
	return scanSuccession(s.pool.QueryRow(ctx, `SELECT `+successionCols+` FROM session_successions
		WHERE owner_id = $1 AND host_dispatch_id = $2`, owner, hostDispatchID))
}

// SupersededKeys is every session key of the principal's that a running
// replace superseded (the sidebar's Superseded group).
func (s *PGStore) SupersededKeys(ctx context.Context) (map[string]bool, error) {
	owner, err := hostDispatchOwner(ctx, "successions")
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT pred_key FROM session_successions WHERE owner_id = $1 AND pred_state = 'superseded'`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}

// linkSuccessionTx is the link (see the ledger): inside the reservation's
// transaction, the row is set started with the new dispatch — only while it
// is undecided (prev 0), or still linked to dispatch prev (the base
// fallback's second reservation). mode is the mode the owner's tap chose.
func linkSuccessionTx(ctx context.Context, tx pgx.Tx, owner, id, prev, dispatch int64, mode, base string) error {
	var tag interface{ RowsAffected() int64 }
	var err error
	if prev == 0 {
		tag, err = tx.Exec(ctx, `UPDATE session_successions SET decided = 'started', decided_at = now(), host_dispatch_id = $3,
			mode = $4, base_used = $5 WHERE id = $1 AND owner_id = $2 AND decided = ''`, id, owner, dispatch, mode, base)
	} else {
		tag, err = tx.Exec(ctx, `UPDATE session_successions SET host_dispatch_id = $3, base_used = $5
			WHERE id = $1 AND owner_id = $2 AND decided = 'started' AND host_dispatch_id = $6 AND mode = $4`,
			id, owner, dispatch, mode, base, prev)
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrSuccessionDecided
	}
	return nil
}

// DeclineSuccession records the owner's "Not now" on an undecided row.
func (s *PGStore) DeclineSuccession(ctx context.Context, id int64) error {
	owner, err := hostDispatchOwner(ctx, "a succession")
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE session_successions SET decided = 'declined', decided_at = now()
		WHERE id = $1 AND owner_id = $2 AND decided = ''`, id, owner)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrSuccessionDecided
	}
	return nil
}

// MarkSuperseded marks a STARTED replace row's predecessor superseded, once;
// reports whether it changed.
func (s *PGStore) MarkSuperseded(ctx context.Context, id int64) (bool, error) {
	owner, err := hostDispatchOwner(ctx, "a succession")
	if err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE session_successions SET pred_state = 'superseded'
		WHERE id = $1 AND owner_id = $2 AND decided = 'started' AND mode = 'replace' AND pred_state = 'active'`, id, owner)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// SetSuccessionNotified records the 30 s push, once; reports whether this
// call set it.
func (s *PGStore) SetSuccessionNotified(ctx context.Context, id int64) (bool, error) {
	owner, err := hostDispatchOwner(ctx, "a succession")
	if err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE session_successions SET notified_at = now()
		WHERE id = $1 AND owner_id = $2 AND notified_at IS NULL AND successor_seen_at IS NULL`, id, owner)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// SetSuccessorSeen records that the successor's view was opened, once.
func (s *PGStore) SetSuccessorSeen(ctx context.Context, id int64) error {
	owner, err := hostDispatchOwner(ctx, "a succession")
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE session_successions SET successor_seen_at = now()
		WHERE id = $1 AND owner_id = $2 AND successor_seen_at IS NULL`, id, owner)
	return err
}
