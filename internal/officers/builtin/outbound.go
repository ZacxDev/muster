package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// --- an item and the outbound draft filed for it ---------------------------------
//
// officer_post_draft with an item_id (internal/api plugin_outbound.go) hands
// the item to the draft: in the filing's transaction the item becomes
// `waiting` with no holder (WaitOnDraftTx), so the instance can neither
// complete it before the post exists nor let its lease lapse into a second
// claim and a second draft. What the draft becomes settles the item:
//
//   - posted: muster completes it `done` with the post's reference
//     (Store.ResolveItem);
//   - refused by its plugin or rejected by its owner: muster queues a
//     draft.refused item and, in the same transaction, finishes the waiting
//     item `no_action` naming that follow-up (EnqueueBounded) — so the work
//     continues on ONE item, never two. Past the bound (no follow-up queued)
//     the item stays waiting: the owner's push is the signal, and the owner
//     resolves it by hand.

// ErrNotHeld is WaitOnDraftTx's refusal: the instance holds no live claim on
// the item.
var ErrNotHeld = errors.New("builtin: the instance holds no live claim on the item")

// ErrAtBound is EnqueueBounded's refusal: the officer already has max live
// items of that kind. Nothing was written.
var ErrAtBound = errors.New("builtin: the officer has as many live items of that kind as it may")

// WaitOnDraftTx sets owner's officer's item id WAITING with no holder, inside
// the caller's transaction tx (the outbound draft's filing), provided
// instance holds it under a lease live at now — read under the item's row
// lock. ErrNotHeld otherwise (another officer's item, or none, included).
func (s *Store) WaitOnDraftTx(ctx context.Context, tx pgx.Tx, owner int64, officer string, id int64, instance string, now time.Time) error {
	it, err := scanItem(tx.QueryRow(ctx, `SELECT `+itemCols+` FROM officer_items WHERE owner_id = $1 AND officer = $2 AND id = $3 FOR UPDATE`,
		owner, officer, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotHeld
	}
	if err != nil {
		return err
	}
	if !it.live(now) || it.ClaimedBy != instance {
		return ErrNotHeld
	}
	_, err = tx.Exec(ctx, `UPDATE officer_items SET status = 'waiting', claimed_by = NULL, lease_expires_at = NULL, updated_at = $2
		WHERE id = $1`, it.ID, now)
	return err
}

// EnqueueBounded enqueues n (as Enqueue: the live item of the same kind and
// subject is the answer, created false) unless owner's officer n.Officer
// already has max live items of kind n.Kind (ErrAtBound, nothing written).
//
// 🔴 THE COUNT AND THE INSERT ARE ONE TRANSACTION, SERIALISED PER (owner,
// officer, kind) BY AN ADVISORY LOCK: two refusals settling at once cannot
// both read max-1 and both insert. A plain transaction would not do it (READ
// COMMITTED reads neither's uncommitted row).
//
// With settle, item *settle — when it is still WAITING with no open ask —
// is finished in the same transaction, outcome no_action, its summary
// settlePrefix and the new item's id: the work moves to the new item. A
// settle item that is not waiting is left as it is.
func (s *Store) EnqueueBounded(ctx context.Context, owner int64, n NewItem, max int, settle *int64, settlePrefix string, now time.Time) (id int64, created bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('officer_items.bounded'), hashtext($1::bigint::text || '/' || $2::text || '/' || $3::text))`,
		owner, n.Officer, n.Kind); err != nil {
		return 0, false, err
	}
	var live int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM officer_items WHERE owner_id = $1 AND officer = $2 AND kind = $3
		AND status IN ('open', 'claimed', 'waiting')`, owner, n.Officer, n.Kind).Scan(&live); err != nil {
		return 0, false, err
	}
	if live >= max {
		return 0, false, fmt.Errorf("%w: %d of %d", ErrAtBound, live, max)
	}
	var existed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM officer_items WHERE owner_id = $1 AND officer = $2 AND kind = $3
		AND subject_key = $4 AND status IN ('open', 'claimed', 'waiting'))`, owner, n.Officer, n.Kind, n.SubjectKey).Scan(&existed); err != nil {
		return 0, false, err
	}
	if id, err = s.EnqueueTx(ctx, tx, owner, n, now); err != nil {
		return 0, false, err
	}
	if settle != nil {
		summary := fmt.Sprintf("%s The work continues on item %d.", settlePrefix, id)
		if _, err := tx.Exec(ctx, `UPDATE officer_items SET status = 'done', outcome = 'no_action', summary = $4, updated_at = $5
			WHERE owner_id = $1 AND officer = $2 AND id = $3 AND status = 'waiting' AND NOT asks @> '[{"status": "open"}]'`,
			owner, n.Officer, *settle, summary, now); err != nil {
			return 0, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, err
	}
	return id, !existed, nil
}

// waitingRefusal is the lease_lost refusal of a verb on a WAITING item, saying
// what it waits on (its asks name an open one, or else the outbound draft
// filed for it): the code is the contract's, the words are the instance's.
func waitingRefusal(it itemRow) error {
	why := "it waits on the outbound draft filed for it, and muster finishes it when that draft is posted, refused or rejected"
	var asks []askEntry
	_ = json.Unmarshal(it.Asks, &asks)
	for _, a := range asks {
		if a.Status == AskOpen {
			why = "it waits on an open ask, and is re-opened once the ask is answered"
			break
		}
	}
	return refuse(409, "lease_lost", fmt.Sprintf("item %d is waiting with no holder: %s. There is nothing for you to complete or release on it.", it.ID, why), nil)
}
