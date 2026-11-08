package plugins

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/authz"
)

// HOST DISPATCHES (migration 0093; docs/design/chief-dispatch.md 2.6 and 3.7): an
// agent the chief started in a tmux window on one of its owner's hosts.
//
// 🔴 ONE SEAM LEDGER, FOUR WRITERS, EACH WITH ITS NAMED CALLERS (a test asserts
// the set over the Go source):
//
//   - the PENDING INSERT, only inside ReserveHostDispatch's reservation
//     transaction (reserveChiefDispatch's onReserved), recording the
//     reservation's id;
//   - UpdateHostDispatch: the window and session, running, missing_listings and
//     host_seen_at — ONLY WHERE status <> 'ended', so a late reply or listing
//     never revives an ended row. Called only by the reconciler;
//   - EndHostDispatch: THE ONLY WRITER OF ended, and the only caller of
//     releaseHostDispatchTx, in one transaction under the owner's lock. Called
//     only by the reconciler and agent_destroy;
//   - SetHostDispatchOrphanNotice: orphan_notice_id only, on an ended row
//     whose column is still empty. Called only by the reconciler.

// Host dispatch statuses.
const (
	HostDispatchPending = "pending"
	HostDispatchRunning = "running"
	HostDispatchEnded   = "ended"
)

// HostDispatch is one row as read.
type HostDispatch struct {
	ID, Owner                    int64
	Host, Runtime, Model, HeldID string
	Repo, Base, Branch, Worktree string
	TaskID                       int64
	ReservationID                int64
	AgentSession, Window         string
	Status                       string
	MissingListings              int
	HostSeenAt, CreatedAt        time.Time
	EndedAt                      time.Time
	Refusal, EndedWhy, Warning   string
	OrphanNoticeID               int64
}

// HostDispatchNew is a pending row's fields, written with its reservation.
type HostDispatchNew struct {
	Host, Runtime, Model, HeldID, Repo, Base string
	TaskID                                   int64
}

// HostDispatchUpdate is what UpdateHostDispatch may write; a nil field is left.
type HostDispatchUpdate struct {
	Window, AgentSession, Worktree, Branch, Warning *string
	Running                                         bool
	MissingListings                                 *int
	HostSeen                                        bool
}

// ErrHostDispatchNotFound: no row of the principal's has that id.
var ErrHostDispatchNotFound = errors.New("plugins: no such host dispatch")

const hostDispatchCols = `id, owner_id, host, runtime, coalesce(model, ''), coalesce(held_id, ''), repo, base, branch, worktree,
	coalesce(task_id, 0), reservation_id, agent_session, tmux_window, status, missing_listings, host_seen_at, created_at,
	coalesce(ended_at, 'epoch'), refusal, ended_why, warning, coalesce(orphan_notice_id, 0)`

func scanHostDispatch(row pgx.Row) (HostDispatch, error) {
	var h HostDispatch
	err := row.Scan(&h.ID, &h.Owner, &h.Host, &h.Runtime, &h.Model, &h.HeldID, &h.Repo, &h.Base, &h.Branch, &h.Worktree,
		&h.TaskID, &h.ReservationID, &h.AgentSession, &h.Window, &h.Status, &h.MissingListings, &h.HostSeenAt, &h.CreatedAt,
		&h.EndedAt, &h.Refusal, &h.EndedWhy, &h.Warning, &h.OrphanNoticeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return HostDispatch{}, ErrHostDispatchNotFound
	}
	return h, err
}

func hostDispatchOwner(ctx context.Context, what string) (int64, error) {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return 0, errors.New("plugins: " + what + " needs a user principal")
	}
	return p.UserID, nil
}

// ReserveHostDispatch reserves one chief start for object exactly as
// ReserveChiefDispatch does — the same request_key, prior-start, check and cap
// rules, under the same lock — and, IN THE SAME TRANSACTION, inserts the host
// dispatch's pending row recording the reservation's id. A replay or a prior
// start inserts nothing (the returned HostDispatch is then the zero value).
func (s *PGStore) ReserveHostDispatch(ctx context.Context, object string, detail map[string]any, check func() error, row HostDispatchNew) (ChiefDispatch, HostDispatch, error) {
	owner, err := hostDispatchOwner(ctx, "a host dispatch")
	if err != nil {
		return ChiefDispatch{}, HostDispatch{}, err
	}
	var hd HostDispatch
	res, err := s.reserveChiefDispatch(ctx, object, detail, check, func(tx pgx.Tx, reservationID int64) error {
		var task any
		if row.TaskID > 0 {
			task = row.TaskID
		}
		var model, held any
		if row.Model != "" {
			model = row.Model
		}
		if row.HeldID != "" {
			held = row.HeldID
		}
		var err error
		hd, err = scanHostDispatch(tx.QueryRow(ctx, `INSERT INTO host_dispatches (owner_id, host, runtime, model, held_id, repo, base, task_id, reservation_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+hostDispatchCols,
			owner, row.Host, row.Runtime, model, held, row.Repo, row.Base, task, reservationID))
		return err
	})
	if err != nil {
		return res, HostDispatch{}, err
	}
	return res, hd, nil
}

// HostDispatchByID is the principal's host dispatch id as it stands now.
func (s *PGStore) HostDispatchByID(ctx context.Context, id int64) (HostDispatch, error) {
	owner, err := hostDispatchOwner(ctx, "a host dispatch")
	if err != nil {
		return HostDispatch{}, err
	}
	return scanHostDispatch(s.pool.QueryRow(ctx, `SELECT `+hostDispatchCols+` FROM host_dispatches WHERE id = $1 AND owner_id = $2`, id, owner))
}

// OpenHostDispatches is the principal's pending and running host dispatches,
// oldest first.
func (s *PGStore) OpenHostDispatches(ctx context.Context) ([]HostDispatch, error) {
	owner, err := hostDispatchOwner(ctx, "host dispatches")
	if err != nil {
		return nil, err
	}
	return s.hostDispatches(ctx, `owner_id = $1 AND status <> 'ended' ORDER BY id`, owner)
}

// RunningHostDispatchesForTask is the principal's running host dispatches on
// taskID, newest first (liveOnTask's host half).
func (s *PGStore) RunningHostDispatchesForTask(ctx context.Context, taskID int64) ([]HostDispatch, error) {
	owner, err := hostDispatchOwner(ctx, "host dispatches")
	if err != nil {
		return nil, err
	}
	return s.hostDispatches(ctx, `owner_id = $1 AND task_id = $2 AND status = 'running' ORDER BY id DESC`, owner, taskID)
}

// HostDispatchesForHost is the principal's rows for host that are not ended,
// plus the ended ones whose window a listing may still carry (orphans), newest
// first, at most 200: the reconciler's view of one listing.
func (s *PGStore) HostDispatchesForHost(ctx context.Context, host string) ([]HostDispatch, error) {
	owner, err := hostDispatchOwner(ctx, "host dispatches")
	if err != nil {
		return nil, err
	}
	return s.hostDispatches(ctx, `owner_id = $1 AND host = $2 AND (status <> 'ended' OR ended_at > now() - interval '7 days') ORDER BY id DESC LIMIT 200`, owner, host)
}

// StaleHostDispatches is the principal's open rows whose host was last seen
// before cutoff (the 24-hour sweep's input).
func (s *PGStore) StaleHostDispatches(ctx context.Context, olderThan time.Duration) ([]HostDispatch, error) {
	owner, err := hostDispatchOwner(ctx, "host dispatches")
	if err != nil {
		return nil, err
	}
	return s.hostDispatches(ctx, `owner_id = $1 AND status <> 'ended' AND host_seen_at < now() - make_interval(secs => $2) ORDER BY id`, owner, olderThan.Seconds())
}

// HostDispatchOwners is every owner with an open host dispatch (SYSTEM scope:
// the sweep then acts as each, as the stopped-agent reaper finds its owners).
func (s *PGStore) HostDispatchOwners(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(authz.System(ctx), `SELECT DISTINCT owner_id FROM host_dispatches WHERE status <> 'ended' ORDER BY owner_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var o int64
		if err := rows.Scan(&o); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *PGStore) hostDispatches(ctx context.Context, where string, args ...any) ([]HostDispatch, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+hostDispatchCols+` FROM host_dispatches WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HostDispatch{}
	for rows.Next() {
		h, err := scanHostDispatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// UpdateHostDispatch writes u onto the principal's row id — ONLY while it is not
// ended. It reports whether a row changed.
func (s *PGStore) UpdateHostDispatch(ctx context.Context, id int64, u HostDispatchUpdate) (bool, error) {
	owner, err := hostDispatchOwner(ctx, "a host dispatch")
	if err != nil {
		return false, err
	}
	var missing any
	if u.MissingListings != nil {
		missing = *u.MissingListings
	}
	tag, err := s.pool.Exec(ctx, `UPDATE host_dispatches SET
		  tmux_window   = coalesce($3, tmux_window),
		  agent_session = coalesce($4, agent_session),
		  worktree      = coalesce($5, worktree),
		  branch        = coalesce($6, branch),
		  warning       = coalesce($7, warning),
		  status        = CASE WHEN $8 THEN 'running' ELSE status END,
		  missing_listings = coalesce($9::smallint, missing_listings),
		  host_seen_at  = CASE WHEN $10 THEN now() ELSE host_seen_at END
		WHERE id = $1 AND owner_id = $2 AND status <> 'ended'`,
		id, owner, u.Window, u.AgentSession, u.Worktree, u.Branch, u.Warning, u.Running, missing, u.HostSeen)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ErrHostDispatchEnded: the row was ended already; nothing was written.
var ErrHostDispatchEnded = errors.New("plugins: that host dispatch has already ended")

// EndHostDispatch ends the principal's row id with why — refused when the host
// REFUSED the spawn (its refusal is recorded, and the task's start and the
// request_key are freed by refused_reservation) — and, in the SAME
// transaction under the owner's reservation lock, releases the start its
// reservation made (releaseHostDispatchTx). THE ONLY WRITER OF ended.
func (s *PGStore) EndHostDispatch(ctx context.Context, id int64, why string, refused bool) (HostDispatch, error) {
	owner, err := hostDispatchOwner(ctx, "a host dispatch")
	if err != nil {
		return HostDispatch{}, err
	}
	if len(why) > 200 {
		why = why[:200]
	}
	var out HostDispatch
	// inChiefDispatchLock: the owner's reservation lock, waited for without
	// holding a pool connection (the reconciler ends rows concurrently with
	// agent_dispatch starts).
	err = s.inChiefDispatchLock(ctx, owner, func(tx pgx.Tx) error {
		cur, err := scanHostDispatch(tx.QueryRow(ctx, `SELECT `+hostDispatchCols+` FROM host_dispatches WHERE id = $1 AND owner_id = $2 FOR UPDATE`, id, owner))
		if err != nil {
			return err
		}
		if cur.Status == HostDispatchEnded {
			out = cur
			return ErrHostDispatchEnded
		}
		refusal, endedWhy := "", why
		if refused {
			refusal, endedWhy = why, "refused"
		}
		out, err = scanHostDispatch(tx.QueryRow(ctx, `UPDATE host_dispatches SET status = 'ended', ended_at = now(), refusal = $3, ended_why = $4
			WHERE id = $1 AND owner_id = $2 AND status <> 'ended' RETURNING `+hostDispatchCols, id, owner, refusal, endedWhy))
		if err != nil {
			return err
		}
		return releaseHostDispatchTx(ctx, tx, owner, out, refused)
	})
	return out, err
}

// releaseHostDispatchTx releases the start host dispatch h's reservation made,
// keyed by THAT reservation (never "the latest"): when it is still its object's
// latest unreleased reservation, an ActionChiefDispatchReleased row names it —
// {host_dispatch_id, refused_reservation} for a refusal (which also frees the
// request_key, requestKeyMatch), {host_dispatch_id, reservation} otherwise —
// whether or not an agent row exists. A newer reservation, or one already
// released, writes nothing: that start is no longer this dispatch's.
func releaseHostDispatchTx(ctx context.Context, tx pgx.Tx, owner int64, h HostDispatch, refused bool) error {
	var object string
	err := tx.QueryRow(ctx, `SELECT object FROM audit_log WHERE id = $1 AND user_id = $2 AND action = $3`,
		h.ReservationID, owner, ActionChiefDispatch).Scan(&object)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	reserved, released, err := chiefStartRows(ctx, tx, owner, object)
	if err != nil {
		return err
	}
	if reserved != h.ReservationID || released > reserved {
		return nil
	}
	key := "reservation"
	if refused {
		key = "refused_reservation"
	}
	return audit(ctx, tx, ActionChiefDispatchReleased, object, map[string]any{"host_dispatch_id": h.ID, key: h.ReservationID})
}

// SetHostDispatchOrphanNotice records the orphan notice raised for the
// principal's ENDED row id, only while it has none. It reports whether it wrote.
func (s *PGStore) SetHostDispatchOrphanNotice(ctx context.Context, id, noticeID int64) (bool, error) {
	owner, err := hostDispatchOwner(ctx, "a host dispatch")
	if err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE host_dispatches SET orphan_notice_id = $3
		WHERE id = $1 AND owner_id = $2 AND status = 'ended' AND orphan_notice_id IS NULL`, id, owner, noticeID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// RecordHostDispatchAgent writes the audit-only ActionChiefDispatchAgent row of
// a host dispatch whose window exists ({host_dispatch_id}): the trail, and what
// latestStartAgent reads as "this start made a host dispatch". The live count
// does not read it (source C reads the row's status).
func (s *PGStore) RecordHostDispatchAgent(ctx context.Context, object string, hostDispatchID int64) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		return audit(ctx, tx, ActionChiefDispatchAgent, object, map[string]any{"host_dispatch_id": hostDispatchID})
	})
}

// HostDispatchObject is the ledger object a host dispatch's reservation names.
func (s *PGStore) HostDispatchObject(ctx context.Context, h HostDispatch) (string, error) {
	var object string
	err := s.pool.QueryRow(ctx, `SELECT object FROM audit_log WHERE id = $1 AND user_id = $2 AND action = $3`,
		h.ReservationID, h.Owner, ActionChiefDispatch).Scan(&object)
	return object, err
}
