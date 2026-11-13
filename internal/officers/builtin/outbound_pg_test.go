package builtin_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/backendtest"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// The item an outbound draft is filed for, and the bounded draft.refused
// queue (outbound.go), against the package's real Postgres.

// TestEnqueueBoundedHoldsItsBoundUnderConcurrency (regression guard for the
// #636 audit's open race): many refusals settling AT ONCE, each a distinct
// subject, leave exactly max live items — the count and the insert are one
// transaction under one lock. Every caller past the bound gets ErrAtBound and
// writes nothing.
func TestEnqueueBoundedHoldsItsBoundUnderConcurrency(t *testing.T) {
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
				_, c, err := w.store.EnqueueBounded(ctxOp(), 1, builtin.NewItem{Officer: backendtest.OfficerA, Kind: kind,
					SubjectKey: "draft:" + strconv.Itoa(i)}, max, nil, "", w.clock())
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil && c:
					created++
				case errors.Is(err, builtin.ErrAtBound):
					atBound++
				default:
					other = append(other, err)
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
			t.Fatalf("round %d: %d live, %d created, %d at the bound, other errors %v; want %d, %d, %d, none",
				round, live, created, atBound, other, max, max, callers-max)
		}
	}
}

// TestEnqueueBoundedSettlesOnlyAWaitingItem: the settle item is finished
// no_action naming the new item only while it is WAITING with no open ask;
// a claimed one is left as it is. At the bound nothing is written — neither
// the new item nor the settle.
func TestEnqueueBoundedSettlesOnlyAWaitingItem(t *testing.T) {
	w := newPGWorld(t, false)
	ctx := ctxOp()
	b := w.b[backendtest.OfficerA]
	hold := func(subject string) int64 {
		t.Helper()
		id := w.AddItem(backendtest.OfficerA, "plugin.reply", subject, "", "")
		if _, err := b.Claim(ctx, id, "inst-1", 600, ""); err != nil {
			t.Fatal(err)
		}
		n, _ := strconv.ParseInt(string(id), 10, 64)
		return n
	}
	wait := func(id int64) {
		t.Helper()
		if err := w.waitOnDraft(id, "inst-1"); err != nil {
			t.Fatalf("wait on draft: %v", err)
		}
	}
	waiting := hold("s-1")
	wait(waiting)
	newID, created, err := w.store.EnqueueBounded(ctx, 1, builtin.NewItem{Officer: backendtest.OfficerA, Kind: "draft.refused", SubjectKey: "draft:1"},
		3, &waiting, "draft 1 was rejected by the owner, not posted.", w.clock())
	if err != nil || !created {
		t.Fatalf("enqueue: %v %v", created, err)
	}
	st, outcome, summary := w.itemState(waiting)
	if st != officers.StatusDone || outcome != "no_action" || summary != "draft 1 was rejected by the owner, not posted. The work continues on item "+strconv.FormatInt(newID, 10)+"." {
		t.Fatalf("the waiting item: %s %s %q", st, outcome, summary)
	}
	claimed := hold("s-2")
	if _, _, err := w.store.EnqueueBounded(ctx, 1, builtin.NewItem{Officer: backendtest.OfficerA, Kind: "draft.refused", SubjectKey: "draft:2"},
		3, &claimed, "x", w.clock()); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := w.itemState(claimed); st != officers.StatusClaimed {
		t.Fatalf("a CLAIMED settle item became %s; want left claimed", st)
	}
	atBound := hold("s-3")
	wait(atBound)
	if _, _, err := w.store.EnqueueBounded(ctx, 1, builtin.NewItem{Officer: backendtest.OfficerA, Kind: "draft.refused", SubjectKey: "draft:3"},
		2, &atBound, "x", w.clock()); !errors.Is(err, builtin.ErrAtBound) {
		t.Fatalf("past the bound: %v; want ErrAtBound", err)
	}
	if st, _, _ := w.itemState(atBound); st != officers.StatusWaiting {
		t.Fatalf("the settle item past the bound became %s; want left waiting", st)
	}
	var n int
	_ = w.store.Pool().QueryRow(ctx, `SELECT count(*) FROM officer_items WHERE subject_key = 'draft:3'`).Scan(&n)
	if n != 0 {
		t.Fatalf("past the bound %d items were written; want none", n)
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
