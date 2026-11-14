package builtin_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/backendtest"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// The item an outbound draft is filed for, and the bounded draft.refused
// queue (outbound.go), against the package's real Postgres.

// TestDraftEndedHoldsItsBoundUnderConcurrency (regression guard for the
// #636 audit's open race): many drafts ending AT ONCE, each a distinct
// subject, leave exactly max live follow-ups — the count and the insert are
// one transaction under one lock. Every caller past the bound is AtBound and
// writes nothing.
func TestDraftEndedHoldsItsBoundUnderConcurrency(t *testing.T) {
	w := newPGWorld(t, false)
	const max, callers = 3, 24
	for round := 0; round < 4; round++ {
		kind := "draft.refused.r" + strconv.Itoa(round)
		var wg sync.WaitGroup
		start := make(chan struct{})
		var mu sync.Mutex
		created, atBound := 0, 0
		var other []error
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				res, err := w.store.DraftEnded(ctxOp(), 1, builtin.DraftEnd{Officer: backendtest.OfficerA, Draft: int64(i + 1), Max: max,
					FollowUp: &builtin.NewItem{Officer: backendtest.OfficerA, Kind: kind, SubjectKey: "draft:" + strconv.Itoa(i)}}, w.clock())
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err != nil:
					other = append(other, err)
				case res.AtBound && res.FollowUp == 0:
					atBound++
				case res.FollowUp > 0 && !res.AtBound:
					created++
				default:
					other = append(other, fmt.Errorf("%+v", res))
				}
			}(i)
		}
		close(start)
		wg.Wait()
		var live int
		if err := w.store.Pool().QueryRow(ctxOp(), `SELECT count(*) FROM officer_items WHERE kind = $1`, kind).Scan(&live); err != nil {
			t.Fatal(err)
		}
		if live != max || created != max || atBound != callers-max || len(other) != 0 {
			t.Fatalf("round %d: %d live, %d created, %d at the bound, other %v; want %d, %d, %d, none",
				round, live, created, atBound, other, max, max, callers-max)
		}
	}
}

// TestDraftEndedSettlesTheItemThatWaitsOnIt is every way DraftEnded settles
// the item, and the items it must leave alone:
//   - waiting, with a follow-up: done no_action naming the follow-up;
//   - claimed under a LIVE lease (an instance works it): left claimed;
//   - claimed under a LAPSED lease (filed for before items waited on their
//     drafts): settled like a waiting one;
//   - past the bound: done no_action saying the officer was not told, and
//     nothing written — or escalated with its owner's notice when asked;
//   - no follow-up (an unconfirmed draft): escalated, notices for the owner;
//   - waiting with an OPEN ASK: left waiting (the ask settles it).
func TestDraftEndedSettlesTheItemThatWaitsOnIt(t *testing.T) {
	w, b, _ := asksWorld(t)
	ctx := ctxOp()
	hold := func(subject string, lease int) int64 {
		t.Helper()
		id := w.AddItem(backendtest.OfficerA, "plugin.reply", subject, "", "")
		if _, err := b.Claim(ctx, id, "inst-1", lease, ""); err != nil {
			t.Fatal(err)
		}
		n, _ := strconv.ParseInt(string(id), 10, 64)
		return n
	}
	waiting := func(subject string) int64 {
		t.Helper()
		id := hold(subject, 600)
		if err := w.waitOnDraft(id, "inst-1"); err != nil {
			t.Fatalf("wait on draft: %v", err)
		}
		return id
	}
	end := func(item int64, draft int, max int, follow, escalate bool) builtin.DraftSettled {
		t.Helper()
		e := builtin.DraftEnd{Officer: backendtest.OfficerA, Draft: int64(draft), Item: item, Max: max, EscalateAtBound: escalate,
			Ended: "draft " + strconv.Itoa(draft) + " ended, not posted."}
		if follow {
			e.FollowUp = &builtin.NewItem{Officer: backendtest.OfficerA, Kind: "draft.refused", SubjectKey: "draft:" + strconv.Itoa(draft)}
		}
		res, err := w.store.DraftEnded(ctx, 1, e, w.clock())
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	it := waiting("s-1")
	res := end(it, 1, 3, true, false)
	st, outcome, summary := w.itemState(it)
	if res.FollowUp == 0 || res.AtBound || res.Item != officers.StatusDone || st != officers.StatusDone || outcome != "no_action" ||
		summary != "draft 1 ended, not posted. The work continues on item "+strconv.FormatInt(res.FollowUp, 10)+"." {
		t.Fatalf("a waiting item: %+v, %s %s %q", res, st, outcome, summary)
	}

	held := hold("s-2", 600)
	if res := end(held, 2, 3, true, false); res.Item != "" || res.FollowUp == 0 {
		t.Fatalf("an item held under a live lease: %+v; want a follow-up and the item left", res)
	}
	if st, _, _ := w.itemState(held); st != officers.StatusClaimed {
		t.Fatalf("an item held under a live lease became %s", st)
	}

	lapsed := hold("s-3", 60)
	w.Advance(2 * time.Minute)
	if res := end(lapsed, 3, 9, true, false); res.Item != officers.StatusDone {
		t.Fatalf("an item claimed under a LAPSED lease: %+v; want it settled", res)
	}
	if st, outcome, _ := w.itemState(lapsed); st != officers.StatusDone || outcome != "no_action" {
		t.Fatalf("an item claimed under a lapsed lease: %s %s", st, outcome)
	}

	// Three draft.refused items are live now: the bound is 3.
	full := waiting("s-4")
	res = end(full, 4, 3, true, false)
	st, outcome, summary = w.itemState(full)
	if !res.AtBound || res.FollowUp != 0 || st != officers.StatusDone || outcome != "no_action" ||
		summary != "draft 4 ended, not posted. Its officer was not told: it already has 3 open draft.refused items." {
		t.Fatalf("past the bound: %+v, %s %s %q", res, st, outcome, summary)
	}
	var n int
	_ = w.store.Pool().QueryRow(ctx, `SELECT count(*) FROM officer_items WHERE subject_key = 'draft:4'`).Scan(&n)
	if n != 0 {
		t.Fatalf("past the bound %d follow-ups were written; want none", n)
	}
	fullEsc := waiting("s-5")
	if res := end(fullEsc, 5, 3, true, true); !res.AtBound || res.Item != officers.StatusEscalated {
		t.Fatalf("past the bound, escalating: %+v", res)
	}
	w.wantEscalatedToOwner(fullEsc, "draft 5 ended, not posted. Its officer was not told: it already has 3 open draft.refused items.")

	unconfirmed := waiting("s-6")
	if res := end(unconfirmed, 6, 3, false, false); res.FollowUp != 0 || res.AtBound || res.Item != officers.StatusEscalated {
		t.Fatalf("no follow-up: %+v; want the item escalated", res)
	}
	w.wantEscalatedToOwner(unconfirmed, "draft 6 ended, not posted.")

	asked := hold("s-7", 600)
	if _, err := b.CreateAsk(ctx, officers.ItemID(strconv.FormatInt(asked, 10)), "inst-1", officers.Ask{Question: "which?"}); err != nil {
		t.Fatal(err)
	}
	if res := end(asked, 7, 99, true, false); res.Item != "" {
		t.Fatalf("an item waiting on an open ask: %+v; want left", res)
	}
	if st, _, _ := w.itemState(asked); st != officers.StatusWaiting {
		t.Fatalf("an item waiting on an open ask became %s", st)
	}
}

// wantEscalatedToOwner: item id is escalated with summary and one notice,
// to the owner (user 1), not yet raised.
func (w *pgWorld) wantEscalatedToOwner(id int64, summary string) {
	w.t.Helper()
	st, outcome, got := w.itemState(id)
	var notices string
	_ = w.store.Pool().QueryRow(ctxOp(), `SELECT notices::text FROM officer_items WHERE id = $1`, id).Scan(&notices)
	if st != officers.StatusEscalated || outcome != "escalated" || got != summary ||
		!strings.Contains(notices, `"recipient": 1`) || !strings.Contains(notices, `"attention_id": null`) {
		w.t.Fatalf("item %d: %s %s %q notices %s; want escalated %q with a notice to user 1", id, st, outcome, got, notices, summary)
	}
}

// TestAnItemWaitingOnADraftCannotBeCompletedOrReleased: WaitOnDraftTx takes
// the item from its holder (waiting, no holder) and refuses one the instance
// does not hold; the backend then refuses complete and release with a
// message saying the item waits on its draft, and claim-next skips it.
func TestAnItemWaitingOnADraftCannotBeCompletedOrReleased(t *testing.T) {
	w := newPGWorld(t, false)
	ctx := ctxOp()
	b := w.b[backendtest.OfficerA]
	id := w.AddItem(backendtest.OfficerA, "plugin.reply", "s-1", "", "")
	n, _ := strconv.ParseInt(string(id), 10, 64)
	if err := w.waitOnDraft(n, "inst-1"); !errors.Is(err, builtin.ErrNotHeld) {
		t.Fatalf("an unclaimed item: %v; want ErrNotHeld", err)
	}
	if _, err := b.Claim(ctx, id, "inst-1", 600, ""); err != nil {
		t.Fatal(err)
	}
	if err := w.waitOnDraft(n, "inst-2"); !errors.Is(err, builtin.ErrNotHeld) {
		t.Fatalf("another instance's item: %v; want ErrNotHeld", err)
	}
	if err := w.waitOnDraft(n, "inst-1"); err != nil {
		t.Fatalf("the holder: %v", err)
	}
	if st, _, _ := w.itemState(n); st != officers.StatusWaiting {
		t.Fatalf("the item is %s; want waiting", st)
	}
	_, err := b.Complete(ctx, id, "inst-1", officers.Completion{Outcome: "done", Summary: "posted it"})
	var hq *officers.HQError
	if !errors.As(err, &hq) || hq.Status != 409 || !strings.Contains(string(hq.Body), "waits on the outbound draft filed for it") {
		t.Fatalf("complete on the waiting item: %v; want 409 naming its draft", err)
	}
	if _, err := b.Release(ctx, id, "inst-1"); !errors.As(err, &hq) || !strings.Contains(string(hq.Body), "waits on the outbound draft filed for it") {
		t.Fatalf("release on the waiting item: %v; want 409 naming its draft", err)
	}
	out, err := b.ClaimNext(ctx, "inst-2", 600, "")
	if err != nil || !strings.Contains(string(out), `"item":null`) {
		t.Fatalf("claim-next with only a waiting item: %s %v; want nothing", out, err)
	}
	if st, _, _ := w.itemState(n); st != officers.StatusWaiting {
		t.Fatalf("after the refusals the item is %s; want waiting", st)
	}
}

// TestDraftWaitingItemsDoNotFillTheAskSweepsPage (regression guard, PR #643
// audit round 1): items waiting on a draft carry no ask; if they counted in
// OpenAsks' page, a page of them returned no row and the sweep (which reads a
// short page as the last) never reached the asked item behind them. With
// more draft-waiting items than the page holds, ahead of it by id, the asked
// item's open ask is still in the first page.
func TestDraftWaitingItemsDoNotFillTheAskSweepsPage(t *testing.T) {
	w := newPGWorld(t, false)
	ctx := ctxOp()
	b := w.b[backendtest.OfficerA]
	const page = 5
	for i := 0; i < page+1; i++ {
		id := w.AddItem(backendtest.OfficerA, "plugin.reply", "d-"+strconv.Itoa(i), "", "")
		if _, err := b.Claim(ctx, id, "inst-1", 600, ""); err != nil {
			t.Fatal(err)
		}
		n, _ := strconv.ParseInt(string(id), 10, 64)
		if err := w.waitOnDraft(n, "inst-1"); err != nil {
			t.Fatal(err)
		}
	}
	asked := w.AddItem(backendtest.OfficerA, "plugin.reply", "asked", "", "")
	n, _ := strconv.ParseInt(string(asked), 10, 64)
	if _, err := w.store.Pool().Exec(ctx, `UPDATE officer_items SET status = 'waiting',
		asks = '[{"ask_id": 77, "question": "q", "asked_by": "inst-1", "asked_at": "2000-01-01T00:00:00Z", "status": "open"}]' WHERE id = $1`, n); err != nil {
		t.Fatal(err)
	}
	got, err := w.store.OpenAsks(ctx, 0, page)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Item != n || got[0].AskID != 77 {
		t.Fatalf("the first page of open asks: %+v; want item %d's ask 77", got, n)
	}
}

// waitOnDraft runs WaitOnDraftTx in a transaction of its own.
func (w *pgWorld) waitOnDraft(id int64, instance string) error {
	ctx := ctxOp()
	tx, err := w.store.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := w.store.WaitOnDraftTx(ctx, tx, 1, backendtest.OfficerA, id, instance, w.clock()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (w *pgWorld) itemState(id int64) (status, outcome, summary string) {
	w.t.Helper()
	err := w.store.Pool().QueryRow(ctxOp(), `SELECT status, coalesce(outcome, ''), coalesce(summary, '') FROM officer_items WHERE id = $1`, id).
		Scan(&status, &outcome, &summary)
	if errors.Is(err, pgx.ErrNoRows) || err != nil {
		w.t.Fatalf("item %d: %v", id, err)
	}
	return
}
