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
//     work continues on ONE item, never two. Past the bound the follow-up is
//     STILL queued, but HELD (blocked: not claimable) until one of the
//     officer's claimable items of its kind is finished (PromoteHeld) — never
//     dropped;
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
	// always queued: claimable while fewer than Max live items of its kind
	// are claimable, HELD (blocked) otherwise, and delivered later by
	// PromoteHeld. nil when the officer must NOT redraft (an unconfirmed
	// draft, which may have been posted): the item is then ESCALATED to the
	// officer's owner.
	FollowUp *NewItem
	Max      int
	// Quiet: no follow-up and no escalation — the item is finished
	// no_action with Ended as its summary (an outreach entry its owner did
	// not tick in its batch: there is nothing to redraft).
	Quiet bool
	// ClaimedAt is when its plugin claimed the draft, nil for one never
	// claimed. Set, a LATE result on an unconfirmed draft (refused or
	// failed after its lease lapsed) may settle the item that draft's
	// unconfirmed end escalated (escalatedByDraft).
	ClaimedAt *time.Time
}

// DraftSettled is what Store.DraftEnded did.
type DraftSettled struct {
	// FollowUp is the follow-up item's id: new, or the live one of the same
	// subject. 0 when none was queued.
	FollowUp int64
	// Held: the follow-up was filed HELD — Max items of its kind were
	// already claimable — and is delivered (made claimable) when one of
	// them is finished (PromoteHeld).
	Held bool
	// Item is what became of the item: its new status (done, escalated),
	// or "" when it was left as it was.
	Item string
	// Withdraw is the raised escalation asks of an item settled out of the
	// escalation its draft's unconfirmed end caused: the caller withdraws
	// them, so the owner is not left asking whether it was posted.
	Withdraw []OpenEscalation
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
//     HELD (Held: blocked, not claimable) when e.Max live items of its kind
//     are already claimable, for PromoteHeld to deliver when one finishes;
//   - the item the draft was filed for (e.Item) is SETTLED, never left
//     waiting: finished no_action naming the follow-up (saying it is held,
//     when it is); escalated to its owner when there is no follow-up to
//     queue (e.FollowUp nil).
//
// The item is settled only while it waits on this draft: WAITING with no
// open ask — or CLAIMED under a lapsed lease, an item filed for before items
// waited on their drafts (muster 0.218.0), or ESCALATED by this same draft's
// unconfirmed end when a late result arrives (escalatedByDraft) — and no
// OTHER draft for it is still pending or approved. Any other item is left
// as it is (an owner who finished or escalated it by hand, an instance
// working it now).
//
// 🔴 THE BOUND'S COUNT, THE PROMOTION AND THE INSERT ARE SERIALISED PER
// (owner, officer, kind) BY A TRANSACTION-SCOPED ADVISORY LOCK (lockBound):
// two refusals settling at once cannot both read max-1 and both file a
// claimable item. A plain transaction would not do it (READ COMMITTED reads
// neither's uncommitted row).
func (s *Store) DraftEndedTx(ctx context.Context, tx pgx.Tx, owner int64, e DraftEnd, now time.Time) (DraftSettled, error) {
	var out DraftSettled
	// A retry chain whose draft e.Draft already has a LIVE later attempt (the
	// owner retried it by hand in the moment between its failure and this
	// settlement): it is not over, so nothing goes back to the officer and its
	// item is left to that attempt.
	var live bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM plugin_reply_drafts n, plugin_reply_drafts d
		WHERE d.id = $2 AND d.owner_id = $1 AND n.owner_id = $1 AND n.id > d.id AND n.retry_of = coalesce(d.retry_of, d.id)
		  AND n.status IN ('pending', 'approved'))`, owner, e.Draft).Scan(&live); err != nil {
		return out, err
	}
	if live {
		return out, nil
	}
	if n := e.FollowUp; n != nil {
		if err := lockBound(ctx, tx, owner, n.Officer, n.Kind); err != nil {
			return out, err
		}
		// The same draft's follow-up already live is the answer, held or not:
		// a draft ends once, and a retried end never files a second item.
		err := tx.QueryRow(ctx, `SELECT id, blocked FROM officer_items WHERE owner_id = $1 AND officer = $2 AND kind = $3 AND subject_key = $4
			AND status IN ('open', 'claimed', 'waiting')`, owner, n.Officer, n.Kind, n.SubjectKey).Scan(&out.FollowUp, &out.Held)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return out, err
		}
		if out.FollowUp == 0 {
			// Older held items go first (and a hold a sweep has not yet
			// delivered is delivered here): only then is this one counted.
			claimable, _, err := promoteHeldTx(ctx, tx, owner, n.Officer, n.Kind, e.Max, now)
			if err != nil {
				return out, err
			}
			if out.FollowUp, err = s.EnqueueTx(ctx, tx, owner, *n, now); errors.Is(err, ErrNotFound) {
				// The officer is gone: there is no one to redraft, and no item.
				return DraftSettled{}, nil
			} else if err != nil {
				return out, err
			}
			if claimable >= e.Max {
				if _, err := tx.Exec(ctx, `UPDATE officer_items SET blocked = true WHERE id = $1`, out.FollowUp); err != nil {
					return out, err
				}
				out.Held = true
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
	switch {
	case it.Status == officers.StatusWaiting, lapsed:
	case it.Status == officers.StatusEscalated && e.ClaimedAt != nil:
		// A late refusal or failure of the draft whose unconfirmed end
		// escalated it: it was NOT posted after all.
		ours, open, err := escalatedByDraft(ctx, tx, owner, it, e.Draft, *e.ClaimedAt)
		if err != nil || !ours {
			return out, err
		}
		out.Withdraw = open
	default:
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
	escalate := e.FollowUp == nil && !e.Quiet
	switch {
	case out.Held:
		summary += fmt.Sprintf(" Its officer already has %d open %s items, so the work is held on item %d and delivered to it when one of them is finished.",
			e.Max, e.FollowUp.Kind, out.FollowUp)
	case out.FollowUp > 0:
		summary += fmt.Sprintf(" The work continues on item %d.", out.FollowUp)
	}
	if escalate {
		notices, err := noticesFor(ctx, tx, owner, e.Officer, e.Draft)
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

// --- a retry chain handed back, and the owner's manual retry -------------------
//
// A RETRY CHAIN (internal/plugins draft_retry.go) that ended unsent went back
// to its officer: a draft.refused follow-up, subject draft:<attempt id>. The
// owner may still retry the chain by hand — but never once the officer has
// TAKEN IT UP, or the redraft and the retry would both be posted.

// chainRedraftedSQL is THE ONE RULE for "the officer has taken up retry chain
// $2's hand-back" (owner $1, the chain's item $3, 0 for none): a draft.refused
// follow-up of any attempt of the chain is no longer open (claimed, waiting on
// a new draft, finished or escalated), or a draft outside the chain, newer
// than its root, names one of those follow-ups or the chain's own item.
// RetryRedrafted reads it for display; RetryTakeOverTx, under the
// follow-ups' row locks, before a retry.
const chainRedraftedSQL = `WITH chain AS (SELECT id FROM plugin_reply_drafts WHERE owner_id = $1 AND (id = $2 OR retry_of = $2)),
	follow AS (SELECT i.id, i.status FROM officer_items i WHERE i.owner_id = $1 AND i.kind = '` + DraftRefusedKind + `'
	            AND i.subject_key IN (SELECT 'draft:' || id FROM chain))
	SELECT EXISTS (SELECT 1 FROM follow WHERE status <> 'open')
	    OR EXISTS (SELECT 1 FROM plugin_reply_drafts n WHERE n.owner_id = $1 AND n.id > $2 AND n.id NOT IN (SELECT id FROM chain)
	                AND (n.officer_item_id IN (SELECT id FROM follow) OR ($3 > 0 AND n.officer_item_id = $3)))`

// RetryRedrafted reports whether the officer has taken up owner's retry
// chain root's hand-back (chainRedraftedSQL), read through q.
func RetryRedrafted(ctx context.Context, q rowQuerier, owner, root, item int64) (bool, error) {
	var taken bool
	err := q.QueryRow(ctx, chainRedraftedSQL, owner, root, item).Scan(&taken)
	return taken, err
}

// RetryRedraftedNow is RetryRedrafted through the store's pool, as ctx's
// principal.
func (s *Store) RetryRedraftedNow(ctx context.Context, owner, root, item int64) (bool, error) {
	return RetryRedrafted(ctx, s.pool, owner, root, item)
}

// RetryTakeOverTx is a manual retry's half, inside its transaction tx: the
// chain's follow-ups are locked, then ErrRedrafted when the officer has taken
// up the hand-back (chainRedraftedSQL); otherwise every still-open follow-up
// is finished no_action, so the officer never redrafts what the retry sends.
func (s *Store) RetryTakeOverTx(ctx context.Context, tx pgx.Tx, owner, root, item, retried int64, now time.Time) error {
	if _, err := tx.Exec(ctx, `SELECT id FROM officer_items WHERE owner_id = $1 AND kind = $2
		AND subject_key IN (SELECT 'draft:' || id FROM plugin_reply_drafts WHERE owner_id = $1 AND (id = $3 OR retry_of = $3))
		ORDER BY id FOR UPDATE`, owner, DraftRefusedKind, root); err != nil {
		return err
	}
	taken, err := RetryRedrafted(ctx, tx, owner, root, item)
	if err != nil {
		return err
	}
	if taken {
		return ErrRedrafted
	}
	_, err = tx.Exec(ctx, `UPDATE officer_items SET status = 'done', outcome = 'no_action', blocked = false, claimed_by = NULL, lease_expires_at = NULL,
		summary = $4, updated_at = $5
		WHERE owner_id = $1 AND kind = $2 AND status = 'open'
		  AND subject_key IN (SELECT 'draft:' || id FROM plugin_reply_drafts WHERE owner_id = $1 AND (id = $3 OR retry_of = $3))`,
		owner, DraftRefusedKind, root, fmt.Sprintf("the owner retried draft %d, which sends the approved post again: nothing to redraft.", retried), now)
	return err
}

// lockBound takes the transaction-scoped advisory lock that serialises the
// bound of owner's officer's items of kind: its count, its promotions
// (promoteHeldTx) and a new follow-up's filing (DraftEndedTx).
func lockBound(ctx context.Context, tx pgx.Tx, owner int64, officer, kind string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('officer_items.bounded'), hashtext($1::bigint::text || '/' || $2::text || '/' || $3::text))`,
		owner, officer, kind)
	return err
}

// promoteHeldTx delivers owner's officer's HELD items of kind — open and
// blocked, filed past the bound by DraftEndedTx — oldest first, while fewer
// than max of its live items of kind are claimable (not blocked), inside tx,
// which must hold lockBound. claimable is the count after it; promoted the
// items it delivered.
//
// 🔴 A HELD ITEM IS AN OPEN, BLOCKED ITEM OF A BOUNDED KIND: the built-in
// store sets blocked on nothing else (SetBlocked is the conformance suite's
// fixture), so the bounded kind's blocked items are exactly its held ones.
// The UPDATE re-checks open and blocked on the row it locks, so an item its
// owner finished meanwhile is skipped, never re-opened.
func promoteHeldTx(ctx context.Context, tx pgx.Tx, owner int64, officer, kind string, max int, now time.Time) (claimable int, promoted []int64, err error) {
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM officer_items WHERE owner_id = $1 AND officer = $2 AND kind = $3
		AND status IN ('open', 'claimed', 'waiting') AND NOT blocked`, owner, officer, kind).Scan(&claimable); err != nil {
		return 0, nil, err
	}
	if claimable >= max {
		return claimable, nil, nil
	}
	rows, err := tx.Query(ctx, `UPDATE officer_items SET blocked = false, updated_at = $5
		WHERE id IN (SELECT id FROM officer_items WHERE owner_id = $1 AND officer = $2 AND kind = $3 AND status = 'open' AND blocked
			ORDER BY id LIMIT $4)
		AND status = 'open' AND blocked
		RETURNING id`, owner, officer, kind, max-claimable, now)
	if err != nil {
		return 0, nil, err
	}
	promoted, err = pgx.CollectRows(rows, pgx.RowTo[int64])
	return claimable + len(promoted), promoted, err
}

// Promoted is one held item PromoteHeld delivered.
type Promoted struct {
	Owner   int64
	Officer string
	Item    int64
}

// promoteBatch bounds the (owner, officer) pairs one PromoteHeld pass reads.
const promoteBatch = 500

// PromoteHeld is the sweep's delivery of HELD items of kind (DraftEndedTx,
// past the bound): for every owner's officer with any, each in a transaction
// of its own under lockBound, its oldest held items are made claimable while
// fewer than max of its items of kind are (promoteHeldTx). ctx must act for
// every owner (the system scope).
//
// 🔴 EXACTLY ONCE, AND IT SURVIVES A RESTART: a held item is a row — it is
// filed once (the live-subject index, and DraftEndedTx's lookup of the
// draft's follow-up), and delivered by an UPDATE conditional on it still
// being held, so a second pass, a second replica or a process started since
// finds nothing more to do.
func (s *Store) PromoteHeld(ctx context.Context, kind string, max int, now time.Time) ([]Promoted, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT owner_id, officer FROM officer_items WHERE kind = $1 AND status = 'open' AND blocked
		ORDER BY owner_id, officer LIMIT $2`, kind, promoteBatch)
	if err != nil {
		return nil, err
	}
	type pair struct {
		owner   int64
		officer string
	}
	pairs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (pair, error) {
		var p pair
		return p, r.Scan(&p.owner, &p.officer)
	})
	if err != nil {
		return nil, err
	}
	var out []Promoted
	for _, p := range pairs {
		ids, err := s.promoteOne(ctx, p.owner, p.officer, kind, max, now)
		if err != nil {
			return out, err
		}
		for _, id := range ids {
			out = append(out, Promoted{Owner: p.owner, Officer: p.officer, Item: id})
		}
	}
	return out, nil
}

// promoteOne is promoteHeldTx for one officer, in a transaction of its own.
func (s *Store) promoteOne(ctx context.Context, owner int64, officer, kind string, max int, now time.Time) ([]int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := lockBound(ctx, tx, owner, officer, kind); err != nil {
		return nil, err
	}
	_, ids, err := promoteHeldTx(ctx, tx, owner, officer, kind, max, now)
	if err != nil {
		return nil, err
	}
	return ids, tx.Commit(ctx)
}

// DraftPosted settles owner's officer's item id, the item outbound draft
// draft was filed for, now that the draft was reported POSTED: done, with
// summary. It is the owner's ResolveItem (refused while an instance holds the
// item under a live lease; finished items are left as they are) widened by
// one case, the LATE result: an item ESCALATED because its draft ended
// unconfirmed (DraftEndedTx) is settled too, provided no newer draft was
// filed for it — the owner's question ("may it have been posted?") is now
// answered — and withdraw names the escalation asks already raised, for the
// caller to withdraw. settled false: the item was left as it is.
//
// 🔴 ONLY THE ESCALATION THIS DRAFT CAUSED: an item the owner sent back
// (SettleEscalation re-opens it, recording an asks entry) and that was
// escalated again since is about something else, so an asks entry made at or
// after claimedAt — the draft's claim, before it went unconfirmed — leaves it
// escalated.
func (s *Store) DraftPosted(ctx context.Context, owner int64, officer string, id, draft int64, claimedAt time.Time, summary string, now time.Time) (settled bool, withdraw []OpenEscalation, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	it, err := scanItem(tx.QueryRow(ctx, `SELECT `+itemCols+` FROM officer_items WHERE owner_id = $1 AND officer = $2 AND id = $3 FOR UPDATE`,
		owner, officer, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	switch {
	case it.Status == officers.StatusDone, it.live(now):
		return false, nil, nil
	case it.Status == officers.StatusEscalated:
		ours, open, err := escalatedByDraft(ctx, tx, owner, it, draft, claimedAt)
		if err != nil || !ours {
			return false, nil, err
		}
		withdraw = open
	}
	// Done is enough for the escalation's notices: an unraised one is never
	// raised (DueNotices reads escalated items only), and a raised one's
	// answer re-opens nothing (SettleEscalation re-opens only while
	// escalated).
	if _, err := tx.Exec(ctx, `UPDATE officer_items SET status = 'done', outcome = 'done', summary = $2, claimed_by = NULL, lease_expires_at = NULL,
		updated_at = $3 WHERE id = $1`, it.ID, summary, now); err != nil {
		return false, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, nil, err
	}
	return true, withdraw, nil
}

// escalatedByDraft reports whether owner's ESCALATED item it is escalated
// because outbound draft draft (claimed at claimedAt) ended unconfirmed
// (DraftEndedTx), so that draft's late result may settle it: its notices
// name that draft (the escalation DraftEndedTx wrote — never an owner's own
// escalation by hand, which writes none); no newer draft was filed for it;
// and no asks entry was made at or after the claim (an owner who sent the
// item back re-opens it with one — SettleEscalation — and keeps the old
// notices, so an escalation since is about something else). open is the escalation's
// raised, unsettled asks, for the caller to withdraw once it is settled.
// THE ONE RULE for both late results: DraftPosted (ok) and DraftEndedTx
// (not ok).
func escalatedByDraft(ctx context.Context, tx pgx.Tx, owner int64, it itemRow, draft int64, claimedAt time.Time) (ours bool, open []OpenEscalation, err error) {
	var named, newer, handled bool
	if err := tx.QueryRow(ctx, `SELECT
			EXISTS (SELECT 1 FROM officer_items WHERE id = $2 AND notices @> jsonb_build_array(jsonb_build_object('draft', $3::bigint))),
			EXISTS (SELECT 1 FROM plugin_reply_drafts WHERE owner_id = $1 AND officer_item_id = $2 AND id > $3),
			EXISTS (SELECT 1 FROM officer_items i, jsonb_array_elements(i.asks) e WHERE i.id = $2 AND (e->>'asked_at')::timestamptz >= $4)`,
		owner, it.ID, draft, claimedAt).Scan(&named, &newer, &handled); err != nil {
		return false, nil, err
	}
	if !named || newer || handled {
		return false, nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT (n->>'recipient')::bigint, (n->>'attention_id')::bigint FROM officer_items i, jsonb_array_elements(i.notices) n
		WHERE i.id = $1 AND n->'settled' = 'false'::jsonb AND n->'attention_id' <> 'null'::jsonb`, it.ID)
	if err != nil {
		return false, nil, err
	}
	open, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (OpenEscalation, error) {
		e := OpenEscalation{Owner: owner, Item: it.ID, Officer: it.OfficerID}
		err := r.Scan(&e.Recipient, &e.AttentionID)
		return e, err
	})
	return err == nil, open, err
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
