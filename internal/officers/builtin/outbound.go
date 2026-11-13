package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/officers"
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
//   - ended unsent — refused by its plugin, rejected by its owner, expired
//     undecided, withdrawn at the claim for a broken binding, or refused
//     unchecked when Checks replies is turned off: muster
//     queues a draft.refused item and, in the same transaction, finishes the
//     waiting item `no_action` naming that follow-up (DraftEndedTx) — so the
//     work continues on ONE item, never two. Past the bound no follow-up is
//     queued and the item is STILL finished, saying the officer was not told
//     (the owner is told: a push, or an escalation);
//   - unconfirmed (its plugin's lease lapsed with no result, so it may have
//     been posted): no follow-up — the officer must never redraft it blindly
//     — and the item is ESCALATED to its owner, who is asked through the
//     existing escalation notice.
//
// No terminal state of a draft leaves its item waiting.

// ErrNotHeld is WaitOnDraftTx's refusal: the instance holds no live claim on
// the item.
var ErrNotHeld = errors.New("builtin: the instance holds no live claim on the item")

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

// DraftEnd is how the outbound draft filed for an item ended UNSENT — a
// refusal, a rejection, an expiry, a lapsed lease, a broken binding — and
// what muster does about it (Store.DraftEnded). internal/api words it.
type DraftEnd struct {
	Officer string
	// Draft is the draft's id; Item the item it was filed for, 0 for none.
	Draft, Item int64
	// Ended is the item's summary: what became of the draft, one sentence
	// ("draft 5 expired, not posted.").
	Ended string
	// FollowUp is the item the officer redrafts from (a draft.refused one),
	// queued unless Max live items of its kind already wait. nil when the
	// officer must NOT redraft (an unconfirmed draft, which may have been
	// posted): the item is then ESCALATED to the officer's owner.
	FollowUp *NewItem
	Max      int
	// EscalateAtBound: past the bound the item is escalated to its owner
	// (the existing Attention notice), for a caller that has no other way
	// to tell them; otherwise it is finished no_action and the caller tells
	// the owner (internal/api's push).
	EscalateAtBound bool
}

// DraftSettled is what Store.DraftEnded did.
type DraftSettled struct {
	// FollowUp is the follow-up item's id: new, or the live one of the same
	// subject. 0 when none was queued.
	FollowUp int64
	// AtBound: a follow-up was due and the bound refused it — the officer
	// was NOT told.
	AtBound bool
	// Item is what became of the item: its new status (done, escalated),
	// or "" when it was left as it was.
	Item string
}

// DraftEnded is DraftEndedTx in a transaction of its own.
func (s *Store) DraftEnded(ctx context.Context, owner int64, e DraftEnd, now time.Time) (DraftSettled, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DraftSettled{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	out, err := s.DraftEndedTx(ctx, tx, owner, e, now)
	if err != nil {
		return DraftSettled{}, err
	}
	return out, tx.Commit(ctx)
}

// DraftEndedTx settles, inside the caller's transaction tx (the one that
// ended the draft, where it can), owner's officer's outbound draft e.Draft
// that ended unsent:
//
//   - with e.FollowUp, that item is queued for the officer to redraft from —
//     unless e.Max live items of its kind already wait (AtBound);
//   - the item the draft was filed for (e.Item) is SETTLED, never left
//     waiting: finished no_action naming the follow-up; escalated to its
//     owner when there is no follow-up to queue (e.FollowUp nil); past the
//     bound, finished no_action saying the officer was not told, or
//     escalated (e.EscalateAtBound).
//
// The item is settled only while it waits on this draft: WAITING with no
// open ask — or CLAIMED under a lapsed lease, an item filed for before items
// waited on their drafts (muster 0.218.0) — and no OTHER draft for it is
// still pending or approved. Any other item is left as it is (an owner who
// finished it by hand, an instance working it now).
//
// 🔴 THE BOUND'S COUNT AND THE INSERT ARE SERIALISED PER (owner, officer,
// kind) BY A TRANSACTION-SCOPED ADVISORY LOCK: two refusals settling at once
// cannot both read max-1 and both insert. A plain transaction would not do
// it (READ COMMITTED reads neither's uncommitted row).
func (s *Store) DraftEndedTx(ctx context.Context, tx pgx.Tx, owner int64, e DraftEnd, now time.Time) (DraftSettled, error) {
	var out DraftSettled
	if n := e.FollowUp; n != nil {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('officer_items.bounded'), hashtext($1::bigint::text || '/' || $2::text || '/' || $3::text))`,
			owner, n.Officer, n.Kind); err != nil {
			return out, err
		}
		// The same draft's follow-up already live is the answer, bound or not.
		err := tx.QueryRow(ctx, `SELECT id FROM officer_items WHERE owner_id = $1 AND officer = $2 AND kind = $3 AND subject_key = $4
			AND status IN ('open', 'claimed', 'waiting')`, owner, n.Officer, n.Kind, n.SubjectKey).Scan(&out.FollowUp)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return out, err
		}
		if out.FollowUp == 0 {
			var live int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM officer_items WHERE owner_id = $1 AND officer = $2 AND kind = $3
				AND status IN ('open', 'claimed', 'waiting')`, owner, n.Officer, n.Kind).Scan(&live); err != nil {
				return out, err
			}
			if live >= e.Max {
				out.AtBound = true
			} else if out.FollowUp, err = s.EnqueueTx(ctx, tx, owner, *n, now); errors.Is(err, ErrNotFound) {
				// The officer is gone: there is no one to redraft, and no item.
				return DraftSettled{}, nil
			} else if err != nil {
				return out, err
			}
		}
	}
	if e.Item <= 0 {
		return out, nil
	}
	it, err := scanItem(tx.QueryRow(ctx, `SELECT `+itemCols+` FROM officer_items WHERE owner_id = $1 AND officer = $2 AND id = $3 FOR UPDATE`,
		owner, e.Officer, e.Item))
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	var asks []askEntry
	_ = json.Unmarshal(it.Asks, &asks)
	for _, a := range asks {
		if a.Status == AskOpen {
			return out, nil
		}
	}
	lapsed := it.Status == officers.StatusClaimed && !it.live(now)
	if it.Status != officers.StatusWaiting && !lapsed {
		return out, nil
	}
	var other bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM plugin_reply_drafts WHERE owner_id = $1 AND officer_item_id = $2
		AND id <> $3 AND status IN ('pending', 'approved'))`, owner, it.ID, e.Draft).Scan(&other); err != nil {
		return out, err
	}
	if other {
		return out, nil
	}
	summary := e.Ended
	escalate := e.FollowUp == nil
	switch {
	case out.FollowUp > 0:
		summary += fmt.Sprintf(" The work continues on item %d.", out.FollowUp)
	case out.AtBound:
		summary += fmt.Sprintf(" Its officer was not told: it already has %d open %s items.", e.Max, e.FollowUp.Kind)
		escalate = e.EscalateAtBound
	}
	if escalate {
		notices, err := noticesFor(ctx, tx, owner, e.Officer)
		if err != nil {
			return out, err
		}
		if _, err := tx.Exec(ctx, `UPDATE officer_items SET status = 'escalated', outcome = 'escalated', notices = $2, summary = $3,
			claimed_by = NULL, lease_expires_at = NULL, updated_at = $4 WHERE id = $1`, it.ID, notices, summary, now); err != nil {
			return out, err
		}
		out.Item = officers.StatusEscalated
		return out, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE officer_items SET status = 'done', outcome = 'no_action', summary = $2,
		claimed_by = NULL, lease_expires_at = NULL, updated_at = $3 WHERE id = $1`, it.ID, summary, now); err != nil {
		return out, err
	}
	out.Item = officers.StatusDone
	return out, nil
}

// waitingRefusal is the lease_lost refusal of a verb on a WAITING item, saying
// what it waits on (its asks name an open one, or else the outbound draft
// filed for it): the code is the contract's, the words are the instance's.
func waitingRefusal(it itemRow) error {
	return refuse(409, "lease_lost", fmt.Sprintf("item %d is waiting with no holder: %s. There is nothing for you to complete or release on it.", it.ID, waitingWhy(it)), nil)
}

// waitingWhy is what a WAITING item waits on: an open ask (asks.go), or else
// the outbound draft filed for it. Claim's refusal says it too.
func waitingWhy(it itemRow) string {
	var asks []askEntry
	_ = json.Unmarshal(it.Asks, &asks)
	for _, a := range asks {
		if a.Status == AskOpen {
			return "it waits on an open ask, and is re-opened once the ask is answered"
		}
	}
	return "it waits on the outbound draft filed for it, and muster settles it when that draft ends: posted, refused, rejected, expired or unconfirmed"
}
