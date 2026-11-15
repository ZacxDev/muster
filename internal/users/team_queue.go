package users

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// A MEMBER'S QUEUED SENDS END WITH THEIR ROLE (team sharing 2b.4;
// docs/design/team-sharing.md A, "Who can send, and the step-up"). A row a
// member queued into a team's window is the team's (owner_id) and the
// member's (queued_by). Removing them, or dropping them to viewer, cancels
// their still-queued rows on THAT team in the same transaction as the
// change; a disable and a sign-out everywhere cancel them on every team
// (internal/sendqueue CancelQueuedBy, team 0).
//
// 🔴 THE CANCEL IS THE COURTESY, THE DELIVERY CHECK IS THE GUARANTEE: the
// send queue's delivery re-reads the queuer's membership, role and account
// before it pastes (internal/api sendQueueOwnerRefusal), so a row a cancel
// missed — or a change made straight in the database — is still refused.
// ---------------------------------------------------------------------------

// Why a member's queued send was cancelled by a change to their membership.
const (
	QueueCancelLeftTeam = "Cancelled: the person who queued it is no longer a member of the team."
	QueueCancelToViewer = "Cancelled: the person who queued it is no longer an operator of the team."
)

// CancelQueuedByTx cancels, inside tx, every still-queued send_queue row
// user QUEUED into a team's queue — team's (team > 0) or every team's (team
// 0) — with detail as its reason, finished at at (zero: the transaction's
// now()). A person's own rows (owner_id = user) are not touched: those are
// CancelOwner's. It is THE statement for both callers: a membership change
// here, and internal/sendqueue's CancelQueuedBy.
func CancelQueuedByTx(ctx context.Context, tx pgx.Tx, team, user int64, at time.Time, detail string) (int64, error) {
	var when any
	if !at.IsZero() {
		when = at
	}
	tag, err := tx.Exec(ctx, `UPDATE send_queue SET state = 'cancelled', finished_at = coalesce($3::timestamptz, now()), detail = $4,
				held_until = NULL, hold_token = NULL -- sendqueue's clearHold: a cancelled row holds no edit
		WHERE queued_by = $2 AND owner_id <> $2 AND state = 'queued' AND ($1::bigint = 0 OR owner_id = $1)`,
		team, user, when, detail)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
