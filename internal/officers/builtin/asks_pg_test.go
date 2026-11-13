package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/backendtest"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// fakeAsker is builtin.Asker recording what it raised and withdrew.
type fakeAsker struct {
	mu        sync.Mutex
	next      int64
	raised    []string
	withdrawn []int64
	fail      error
}

func (f *fakeAsker) Raise(_ context.Context, _ officers.Officer, q string, _ []string, _ time.Duration) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return 0, f.fail
	}
	f.next++
	f.raised = append(f.raised, q)
	return 900 + f.next, nil
}

func (f *fakeAsker) Withdraw(_ context.Context, _ officers.Officer, id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.withdrawn = append(f.withdrawn, id)
}

func (f *fakeAsker) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.raised) }

// asksWorld is a pgWorld whose officer A asks through a fakeAsker.
func asksWorld(t *testing.T) (*pgWorld, *builtin.Backend, *fakeAsker) {
	t.Helper()
	w := newPGWorld(t, false)
	ask := &fakeAsker{}
	return w, w.b[backendtest.OfficerA].WithAsker(ask), ask
}

// item reads item id's status and asks.
func (w *pgWorld) item(id officers.ItemID) (status string, asks []map[string]any, notices []map[string]any) {
	w.t.Helper()
	var a, n []byte
	if err := w.store.Pool().QueryRow(ctxOp(), `SELECT status, asks, notices FROM officer_items WHERE id = $1`, string(id)).Scan(&status, &a, &n); err != nil {
		w.t.Fatal(err)
	}
	_ = json.Unmarshal(a, &asks)
	_ = json.Unmarshal(n, &notices)
	return status, asks, notices
}

func claimed(t *testing.T, b *builtin.Backend, id officers.ItemID) {
	t.Helper()
	if _, err := b.Claim(context.Background(), id, "i1", 300, ""); err != nil {
		t.Fatalf("claim %s: %v", id, err)
	}
}

func askID(t *testing.T, raw []byte) int64 {
	t.Helper()
	var out struct {
		Ask struct {
			ID int64 `json:"id"`
		} `json:"ask"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Ask.ID == 0 {
		t.Fatalf("ask answer %s: %v", raw, err)
	}
	return out.Ask.ID
}

// TestAnAskEndsTheLeaseAndTheAnswerReopensTheItem: HQ's asks semantics on
// the built-in backend. Only the holder of a live lease may ask (nothing is
// raised otherwise); asking raises one Attention ask (through the Asker) and
// sets the item waiting with no holder, which claim-next skips; the answer
// re-opens it with the answer on it, once.
func TestAnAskEndsTheLeaseAndTheAnswerReopensTheItem(t *testing.T) {
	w, b, asker := asksWorld(t)
	ctx := context.Background()
	id := w.AddItem(backendtest.OfficerA, "release.post", "v1", officers.TierJudgement, "")
	if _, err := b.CreateAsk(ctx, id, "i1", officers.Ask{Question: "ship?"}); !isStatus(err, http.StatusConflict) || asker.count() != 0 {
		t.Fatalf("an ask with no lease: %v (raised %d), want 409 lease_lost and nothing raised", err, asker.count())
	}
	claimed(t, b, id)
	if _, err := b.CreateAsk(ctx, id, "i2", officers.Ask{Question: "ship?"}); !isStatus(err, http.StatusConflict) || asker.count() != 0 {
		t.Fatalf("an ask by another instance: %v, want 409 and nothing raised", err)
	}
	if _, err := b.CreateAsk(ctx, id, "i1", officers.Ask{Question: "ship?", Images: []string{"img-1"}}); !isStatus(err, http.StatusBadRequest) {
		t.Fatalf("an ask with images: %v, want 400", err)
	}
	raw, err := b.CreateAsk(ctx, id, "i1", officers.Ask{Question: "Post the v1 announcement as drafted?", Options: []string{"yes", "no"}})
	if err != nil {
		t.Fatal(err)
	}
	ask := askID(t, raw)
	status, asks, _ := w.item(id)
	if status != officers.StatusWaiting || len(asks) != 1 || asks[0]["status"] != builtin.AskOpen || asker.count() != 1 {
		t.Fatalf("after the ask: %s %v (raised %d), want waiting with one open ask", status, asks, asker.count())
	}
	if got := asker.raised[0]; got != "Officer "+backendtest.OfficerA+" asks about item "+string(id)+" (release.post v1):\n\nPost the v1 announcement as drafted?" {
		t.Fatalf("the Attention question is %q", got)
	}
	if _, err := b.Heartbeat(ctx, id, "i1", 300); !isStatus(err, http.StatusConflict) {
		t.Fatalf("heartbeat after asking: %v, want 409 (the lease ended)", err)
	}
	if out, _ := b.ClaimNext(ctx, "i2", 300, ""); string(out) != `{"item":null,"lease_expires_at":null}` {
		t.Fatalf("claim-next took a waiting item: %s", out)
	}
	got, err := w.store.ResolveAsk(ctxOp(), 1, mustInt(id), ask, builtin.AskAnswered, "yes, post it", w.clock())
	if err != nil || got != builtin.Reopened {
		t.Fatalf("answer: %v, %v; want reopened", got, err)
	}
	if again, err := w.store.ResolveAsk(ctxOp(), 1, mustInt(id), ask, builtin.AskAnswered, "yes", w.clock()); err != nil || again != builtin.Stale {
		t.Fatalf("a second settlement of one ask: %v, %v; want stale", again, err)
	}
	status, asks, _ = w.item(id)
	if status != officers.StatusOpen || asks[0]["status"] != builtin.AskAnswered || asks[0]["answer"] != "yes, post it" {
		t.Fatalf("after the answer: %s %v, want open with the answer on the ask", status, asks)
	}
	out, err := b.ClaimNext(ctx, "i2", 300, "")
	if err != nil || !json.Valid(out) || !containsJSON(out, "yes, post it") {
		t.Fatalf("claim after the answer: %s (%v), want the item with its answer", out, err)
	}
}

// TestTheItemReopensOnlyWhenNoAskIsOpen: with two asks open, an answer to one
// keeps the item waiting; withdrawing the other re-opens it.
func TestTheItemReopensOnlyWhenNoAskIsOpen(t *testing.T) {
	w, b, _ := asksWorld(t)
	ctx := context.Background()
	id := w.AddItem(backendtest.OfficerA, "k", "s", officers.TierJudgement, "")
	claimed(t, b, id)
	// The built-in backend raises an ask only for the item's holder, so two
	// open at once are one raised through CreateAsk and a second written
	// beside it (as a waiting item's further ask would be).
	second := askID(t, must(b.CreateAsk(ctx, id, "i1", officers.Ask{Question: "two?"})))
	if _, err := w.store.Pool().Exec(ctxOp(), `UPDATE officer_items SET asks = asks || '[{"ask_id": 77, "question": "three?", "status": "open"}]' WHERE id = $1`, string(id)); err != nil {
		t.Fatal(err)
	}
	if got, err := w.store.ResolveAsk(ctxOp(), 1, mustInt(id), second, builtin.AskAnswered, "a2", w.clock()); err != nil || got != builtin.Settled {
		t.Fatalf("one of two open asks answered: %v, %v; want settled (still waiting)", got, err)
	}
	if status, _, _ := w.item(id); status != officers.StatusWaiting {
		t.Fatalf("with an ask still open the item is %s, want waiting", status)
	}
	if got, err := w.store.ResolveAsk(ctxOp(), 1, mustInt(id), 77, builtin.AskWithdrawn, "", w.clock()); err != nil || got != builtin.Reopened {
		t.Fatalf("the last open ask withdrawn: %v, %v; want reopened", got, err)
	}
}

// TestAnExpiredAskEscalatesWithItsNotices: an ask nobody answered in time
// escalates the item, and the escalation's notices are written with it: the
// owner and each escalate_to once; suppressed (recorded, never due) in shadow.
func TestAnExpiredAskEscalatesWithItsNotices(t *testing.T) {
	for _, mode := range []string{"live", "shadow"} {
		w, b, _ := asksWorld(t)
		addUser2(t)
		if err := w.store.PutDef(ctxOp(), 1, builtin.Def{Officer: backendtest.OfficerA, Enabled: true, Mode: mode, EscalateTo: []int64{2, 1, 2}}); err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		id := w.AddItem(backendtest.OfficerA, "k", "s-"+mode, officers.TierJudgement, "")
		claimed(t, b, id)
		ask := askID(t, must(b.CreateAsk(ctx, id, "i1", officers.Ask{Question: "?"})))
		got, err := w.store.ResolveAsk(ctxOp(), 1, mustInt(id), ask, builtin.AskExpired, "", w.clock())
		if err != nil || got != builtin.Escalated {
			t.Fatalf("%s: an expired ask: %v, %v; want escalated", mode, got, err)
		}
		status, _, notices := w.item(id)
		if status != officers.StatusEscalated || len(notices) != 2 || notices[0]["recipient"] != float64(1) || notices[1]["recipient"] != float64(2) {
			t.Fatalf("%s: after expiry: %s notices %v, want escalated with one notice each for users 1 and 2", mode, status, notices)
		}
		due, err := w.store.DueNotices(authz.System(ctx), 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		want := 2
		if mode == "shadow" {
			want = 0
		}
		if len(due) != want {
			t.Fatalf("%s: %d notices due, want %d", mode, len(due), want)
		}
	}
}

// TestAnEscalatingCompleteWritesItsNoticesOnce: officer_complete escalated
// writes one notice per recipient in the same transaction (none for done);
// of two raisers of one notice exactly one records it.
func TestAnEscalatingCompleteWritesItsNoticesOnce(t *testing.T) {
	w, b, _ := asksWorld(t)
	ctx := context.Background()
	done := w.AddItem(backendtest.OfficerA, "k", "done", officers.TierJudgement, "")
	claimed(t, b, done)
	if _, err := b.Complete(ctx, done, "i1", officers.Completion{Outcome: "done", Summary: "fine"}); err != nil {
		t.Fatal(err)
	}
	if _, _, n := w.item(done); len(n) != 0 {
		t.Fatalf("a done item carries notices %v", n)
	}
	esc := w.AddItem(backendtest.OfficerA, "k", "esc", officers.TierJudgement, "")
	claimed(t, b, esc)
	if _, err := b.Complete(ctx, esc, "i1", officers.Completion{Outcome: "escalated", Summary: "needs a human"}); err != nil {
		t.Fatal(err)
	}
	due, err := w.store.DueNotices(authz.System(ctx), 0, 10)
	if err != nil || len(due) != 1 || due[0].Recipient != 1 || due[0].Summary != "needs a human" || due[0].Item != mustInt(esc) {
		t.Fatalf("due after an escalation: %+v (%v), want one notice to the owner carrying the summary", due, err)
	}
	won, err := w.store.NoticeRaised(authz.System(ctx), 1, mustInt(esc), 1, 5001)
	if err != nil || !won {
		t.Fatalf("first raiser: %v, %v", won, err)
	}
	won, err = w.store.NoticeRaised(authz.System(ctx), 1, mustInt(esc), 1, 5002)
	if err != nil || won {
		t.Fatalf("second raiser of the same notice: %v, %v; want it to lose", won, err)
	}
	if due, _ := w.store.DueNotices(authz.System(ctx), 0, 10); len(due) != 0 {
		t.Fatalf("a raised notice is still due: %+v", due)
	}
}

// TestARefusedRaiseLeavesTheItemHeld: the Asker's decision (the owner's open
// asks at their cap) is the instance's answer, and the item stays held.
func TestARefusedRaiseLeavesTheItemHeld(t *testing.T) {
	w, b, asker := asksWorld(t)
	id := w.AddItem(backendtest.OfficerA, "k", "s", officers.TierJudgement, "")
	claimed(t, b, id)
	asker.fail = &officers.HQError{Status: http.StatusConflict, Body: []byte(`{"code":"too_many_asks"}`)}
	if _, err := b.CreateAsk(context.Background(), id, "i1", officers.Ask{Question: "?"}); !isStatus(err, http.StatusConflict) {
		t.Fatalf("a refused raise: %v, want its 409", err)
	}
	if status, asks, _ := w.item(id); status != officers.StatusClaimed || len(asks) != 0 {
		t.Fatalf("after a refused raise the item is %s with asks %v, want still claimed with none", status, asks)
	}
}

func isStatus(err error, status int) bool {
	var dec *officers.HQError
	return errors.As(err, &dec) && dec.Status == status
}

func mustInt(id officers.ItemID) int64 {
	n, _ := strconv.ParseInt(string(id), 10, 64)
	return n
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

func containsJSON(raw []byte, s string) bool {
	b, _ := json.Marshal(s)
	return json.Valid(raw) && len(raw) > 0 && bytesContains(raw, b[1:len(b)-1])
}

func bytesContains(h, n []byte) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if string(h[i:i+len(n)]) == string(n) {
			return true
		}
	}
	return false
}

func addUser2(t *testing.T) {
	t.Helper()
	dbtest.OwnerExec(t, dbtest.DSN(t), `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member') ON CONFLICT DO NOTHING`)
}

// TestAnAnsweredEscalationSendsTheItemBack: an escalation is asked of each
// recipient; the first ANSWER re-opens the item for the officer (open,
// judgement, no outcome, the answer on its asks[]) and names the other
// recipients' asks to withdraw; a later answer changes nothing. A DISMISSED
// escalation leaves its item escalated.
func TestAnAnsweredEscalationSendsTheItemBack(t *testing.T) {
	w, b, _ := asksWorld(t)
	addUser2(t)
	if err := w.store.PutDef(ctxOp(), 1, builtin.Def{Officer: backendtest.OfficerA, Enabled: true, EscalateTo: []int64{2}}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sys := authz.System(ctx)
	escalate := func(subject string) int64 {
		id := w.AddItem(backendtest.OfficerA, "k", subject, officers.TierRoutine, "")
		claimed(t, b, id)
		if _, err := b.Complete(ctx, id, "i1", officers.Completion{Outcome: "escalated", Summary: "which tone?"}); err != nil {
			t.Fatal(err)
		}
		return mustInt(id)
	}
	item := escalate("answered")
	for r, att := range map[int64]int64{1: 5001, 2: 5002} {
		if won, err := w.store.NoticeRaised(sys, 1, item, r, att); err != nil || !won {
			t.Fatalf("raise for user %d: %v %v", r, won, err)
		}
	}
	open, err := w.store.OpenEscalations(sys, 0, 10)
	if err != nil || len(open) != 2 {
		t.Fatalf("open escalation asks: %+v (%v), want two", open, err)
	}
	reopened, withdraw, err := w.store.SettleEscalation(sys, 1, item, 1, 5001, true, "keep it dry", w.clock())
	if err != nil || !reopened || len(withdraw) != 1 || withdraw[0].Recipient != 2 || withdraw[0].AttentionID != 5002 {
		t.Fatalf("the owner's answer: reopened %v withdraw %+v (%v); want re-opened and user 2's ask withdrawn", reopened, withdraw, err)
	}
	var status, tier string
	var outcome *string
	if err := w.store.Pool().QueryRow(ctxOp(), `SELECT status, tier, outcome FROM officer_items WHERE id = $1`, item).Scan(&status, &tier, &outcome); err != nil {
		t.Fatal(err)
	}
	_, asks, _ := w.item(officers.ItemID(strconv.FormatInt(item, 10)))
	if status != officers.StatusOpen || tier != officers.TierJudgement || outcome != nil || len(asks) != 1 || asks[0]["answer"] != "keep it dry" {
		t.Fatalf("after the answer: %s/%s outcome %v asks %v; want open judgement, no outcome, the answer on it", status, tier, outcome, asks)
	}
	if again, _, err := w.store.SettleEscalation(sys, 1, item, 2, 5002, true, "late", w.clock()); err != nil || again {
		t.Fatalf("a second recipient's later answer: %v %v, want nothing", again, err)
	}
	if open, _ := w.store.OpenEscalations(sys, 0, 10); len(open) != 0 {
		t.Fatalf("escalation asks still open after the item went back: %+v", open)
	}

	dismissed := escalate("dismissed")
	if _, err := w.store.NoticeRaised(sys, 1, dismissed, 1, 6001); err != nil {
		t.Fatal(err)
	}
	if re, _, err := w.store.SettleEscalation(sys, 1, dismissed, 1, 6001, false, "", w.clock()); err != nil || re {
		t.Fatalf("a dismissed escalation: reopened %v (%v), want not", re, err)
	}
	if status, _, _ := w.item(officers.ItemID(strconv.FormatInt(dismissed, 10))); status != officers.StatusEscalated {
		t.Fatalf("a dismissed escalation left the item %s, want escalated", status)
	}
}

// TestTheSweepReadsPageByItem: OpenAsks pages by ITEM (every open ask of an
// item in the same page), so a page full of asks still awaiting an answer
// never hides the items after it — the caller pages on from the last id.
func TestTheSweepReadsPageByItem(t *testing.T) {
	w, b, _ := asksWorld(t)
	ctx := context.Background()
	var ids []int64
	for _, subj := range []string{"p1", "p2", "p3"} {
		id := w.AddItem(backendtest.OfficerA, "k", subj, officers.TierJudgement, "")
		claimed(t, b, id)
		must(b.CreateAsk(ctx, id, "i1", officers.Ask{Question: subj}))
		ids = append(ids, mustInt(id))
	}
	// a second open ask on the first item: both must come in its page
	if _, err := w.store.Pool().Exec(ctxOp(), `UPDATE officer_items SET asks = asks || '[{"ask_id": 4242, "status": "open"}]' WHERE id = $1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	sys := authz.System(ctx)
	p1, err := w.store.OpenAsks(sys, 0, 1)
	if err != nil || len(p1) != 2 || p1[0].Item != ids[0] || p1[1].Item != ids[0] {
		t.Fatalf("page 1 (one item): %+v (%v), want both asks of item %d", p1, err, ids[0])
	}
	p2, err := w.store.OpenAsks(sys, ids[0], 2)
	if err != nil || len(p2) != 2 || p2[0].Item != ids[1] || p2[1].Item != ids[2] {
		t.Fatalf("page 2 after item %d: %+v (%v), want items %d and %d", ids[0], p2, err, ids[1], ids[2])
	}
}

// TestAnUnraisableEscalationIsGivenUp: NoticeAbandoned marks a recipient's
// unraised entry suppressed and settled, so it is never due again, and
// leaves the other recipients' entries due.
func TestAnUnraisableEscalationIsGivenUp(t *testing.T) {
	w, b, _ := asksWorld(t)
	addUser2(t)
	if err := w.store.PutDef(ctxOp(), 1, builtin.Def{Officer: backendtest.OfficerA, Enabled: true, EscalateTo: []int64{2}}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id := w.AddItem(backendtest.OfficerA, "k", "s", officers.TierJudgement, "")
	claimed(t, b, id)
	if _, err := b.Complete(ctx, id, "i1", officers.Completion{Outcome: "escalated", Summary: "x"}); err != nil {
		t.Fatal(err)
	}
	sys := authz.System(ctx)
	if err := w.store.NoticeAbandoned(sys, 1, mustInt(id), 2); err != nil {
		t.Fatal(err)
	}
	due, err := w.store.DueNotices(sys, 0, 10)
	if err != nil || len(due) != 1 || due[0].Recipient != 1 {
		t.Fatalf("due after giving up user 2: %+v (%v), want only the owner's", due, err)
	}
	missing, err := w.store.UsersExist(sys, []int64{1, 2, 987654})
	if err != nil || len(missing) != 1 || missing[0] != 987654 {
		t.Fatalf("UsersExist: %v (%v), want [987654]", missing, err)
	}
}

// TestEscalationPagingAndTheFullItem: a page of OpenEscalations holds only
// items with a raised, unsettled ask (an unraised one cannot end the sweep's
// paging early), and an answer to an escalated item that already carries its
// 20 asks still settles (the re-open cannot land; the settlement commits).
func TestEscalationPagingAndTheFullItem(t *testing.T) {
	w, b, _ := asksWorld(t)
	ctx := context.Background()
	sys := authz.System(ctx)
	var ids []int64
	for _, subj := range []string{"e1", "e2", "e3"} {
		id := w.AddItem(backendtest.OfficerA, "k", subj, officers.TierJudgement, "")
		claimed(t, b, id)
		if _, err := b.Complete(ctx, id, "i1", officers.Completion{Outcome: "escalated", Summary: "x"}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, mustInt(id))
	}
	for _, i := range []int{0, 2} { // e2 stays unraised
		if won, err := w.store.NoticeRaised(sys, 1, ids[i], 1, 7000+int64(i)); err != nil || !won {
			t.Fatal(won, err)
		}
	}
	page, err := w.store.OpenEscalations(sys, ids[0], 1)
	if err != nil || len(page) != 1 || page[0].Item != ids[2] {
		t.Fatalf("the page after e1 with a batch of one: %+v (%v), want e3 (the unraised e2 holds no slot)", page, err)
	}
	full := make([]string, 20)
	for i := range full {
		full[i] = fmt.Sprintf(`{"ask_id": %d, "status": "answered"}`, 100+i)
	}
	if _, err := w.store.Pool().Exec(ctxOp(), `UPDATE officer_items SET asks = $2::jsonb WHERE id = $1`, ids[0], "["+strings.Join(full, ",")+"]"); err != nil {
		t.Fatal(err)
	}
	reopened, _, err := w.store.SettleEscalation(sys, 1, ids[0], 1, 7000, true, "go", w.clock())
	if err != nil || reopened {
		t.Fatalf("an answer to an item with 20 asks: reopened %v, %v; want settled without re-opening", reopened, err)
	}
	if open, _ := w.store.OpenEscalations(sys, 0, 10); len(open) != 1 || open[0].Item != ids[2] {
		t.Fatalf("after settling the full item, open: %+v, want only e3", open)
	}
}
