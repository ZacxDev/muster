package plugins

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// A REVIEW OF A PULL REQUEST THAT IS ALREADY MERGED OR CLOSED needs nothing
// more: muster settles its message itself (SettledByMuster) rather than paste
// it into a session, hand it back to the chief, or raise the "not acted on"
// ask. Whether the pull request is closed is the caller's to read
// (internal/api pr_closed.go asks GitHub); this store only writes the outcome.
// docs/plugins.md#a-review-of-a-pull-request-already-merged-or-closed.

// PRReviewSettler is the store side of settling a closed pull request's review.
type PRReviewSettler interface {
	// SettleQueuedAsMuster takes QUEUED message messageID of owner ownerID and
	// hands it to chief agent chiefID WITHOUT a turn: delivered, settled with
	// reason, audited (via names the path). A message that is no longer queued
	// (another delivery took it) or is another owner's is ErrNotFound, and
	// nothing is written.
	SettleQueuedAsMuster(ctx context.Context, chiefID, ownerID, messageID int64, reason, via string) error
	// SettleAsMuster settles message messageID with reason when it is not
	// settled yet (the first reason is kept), audited "by" muster.
	SettleAsMuster(ctx context.Context, messageID int64, reason, via string) error
}

var _ PRReviewSettler = (*PGStore)(nil)

// SettleQueuedAsMuster: see PRReviewSettler.
func (s *PGStore) SettleQueuedAsMuster(ctx context.Context, chiefID, ownerID, messageID int64, reason, via string) error {
	return s.settleQueuedAsMuster(ctx, chiefID, ownerID, messageID, reason, via, true)
}

// SettleQueuedTaskEventAsMuster is SettleQueuedAsMuster for a task event the
// task triage placed (internal/api task_triage.go) WITHOUT counting a delivery
// attempt: no attempt was made, and the attempt stamp (last_attempt_at) is the
// anchor "the chief acted on this event" is measured from (chiefActedOn) — a
// stamp written after the triage's own routing would put the task back on
// "Task to route".
func (s *PGStore) SettleQueuedTaskEventAsMuster(ctx context.Context, chiefID, ownerID, messageID int64, reason, via string) error {
	return s.settleQueuedAsMuster(ctx, chiefID, ownerID, messageID, reason, via, false)
}

func (s *PGStore) settleQueuedAsMuster(ctx context.Context, chiefID, ownerID, messageID int64, reason, via string, attempt bool) error {
	if chiefID <= 0 || ownerID <= 0 {
		return invalid("the agent has no owner")
	}
	reason, err := cleanSettleReason(reason)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		m, err := scanMessage(tx.QueryRow(ctx, `SELECT `+messageCols+` FROM plugin_messages WHERE id = $1 FOR UPDATE`, messageID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		// Only a queued message, as ReplyAsMuster takes one: one another
		// delivery already holds is not taken from under it.
		if m.Status != StatusQueued || m.OwnerID != ownerID {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, `UPDATE plugin_messages SET status = 'delivered', agent_id = $2, delivered_at = now(),
			attempts = attempts + CASE WHEN $4 THEN 1 ELSE 0 END,
			last_attempt_at = CASE WHEN $4 THEN now() ELSE last_attempt_at END,
			settle_reason = coalesce(settle_reason, $3) WHERE id = $1`,
			messageID, chiefID, reason, attempt); err != nil {
			return err
		}
		if err := audit(ctx, tx, "plugin.delivered", MessageObject(messageID), map[string]any{
			"plugin_id": m.PluginID, "agent_id": chiefID, "outcome": string(StatusDelivered), "answered_by": "muster", "via": via}); err != nil {
			return err
		}
		return audit(ctx, tx, "plugin.message.settled", MessageObject(messageID), map[string]any{
			"agent_id": chiefID, "by": "muster", "via": via})
	})
}

// SettleAsMuster: see PRReviewSettler.
func (s *PGStore) SettleAsMuster(ctx context.Context, messageID int64, reason, via string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		return settleByMusterTx(ctx, tx, messageID, reason, map[string]any{"via": via})
	})
}
