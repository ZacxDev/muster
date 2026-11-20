package officership

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// THE OPEN-PULL-REQUEST QUEUE (prqueue.go), against the fake GitHub: a
// create past the open cap is queued, not refused; the sweep opens the oldest
// queued branch once a slot frees; a vanished or superseded branch is
// skipped; the queue lives in a store a new gate reads; the stale-draft
// age-out is off unless enabled.
//
// The two queued branches are queued B (item 19) BEFORE A (item 17), the
// opposite of their names' and their items' order, so a drain that sorted by
// either instead of by when they were queued is caught.
// ---------------------------------------------------------------------------

const (
	qBranchA = dBranch                                   // item 17
	qBranchB = "bot/engineering-example-web/19-fix-disk" // item 19
	qShaA    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	qShaB    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	qShaC    = "cccccccccccccccccccccccccccccccccccccccc"
)

// memQueue is PRQueue over a memory store, bound to one officer (the api
// package's adapter does the same over either store).
type memQueue struct {
	st      officers.PRQueueStore
	officer string
}

func (q memQueue) Add(ctx context.Context, p officers.QueuedPR) (officers.QueuedPR, error) {
	return q.st.PRQueueAdd(ctx, q.officer, p)
}
func (q memQueue) List(ctx context.Context, repo string) ([]officers.QueuedPR, error) {
	return q.st.PRQueueList(ctx, q.officer, repo)
}
func (q memQueue) Settle(ctx context.Context, id int64, head, state string, pr int, note string) (bool, error) {
	return q.st.PRQueueSettle(ctx, q.officer, id, head, state, pr, note)
}

// newQueueRig is the draft rig with a queue, items 17 and 19 claimed, the
// two queue branches pushed (A at qShaA, B at qShaB, each with a clean
// comparison), and FOUR of the officer's draft pull requests open in the
// infra repository (#101..#104, opened yesterday, each clean at fxHead): the
// open cap is full.
func newQueueRig(t *testing.T) (*draftRig, memQueue) {
	t.Helper()
	r := newDraftRig()
	r.claims[19] = dInstance
	q := memQueue{st: officers.NewMemPRQueueStore(), officer: dOfficer}
	r.g.Queue = q
	r.g.CreateWait = 50 * time.Millisecond
	yesterday := dNow.Add(-16 * time.Hour)
	for item := 1; item <= 4; item++ {
		p := PR{Number: 100 + item, State: "open", Draft: true, Author: BotLogin(r.infra.slug), AuthorType: "Bot",
			HeadRef: "bot/engineering-example-web/" + itoa(item) + "-x", HeadSHA: fxHead, HeadRepo: dInfra, BaseRef: "trunk",
			CreatedAt: yesterday, UpdatedAt: dNow.Add(-time.Hour), Body: goodBody(item)}
		r.infra.all = append(r.infra.all, p)
		r.infra.prs[p.Number] = p
	}
	clean := r.infra.compares["trunk..."+dBranch]
	for b, sha := range map[string]string{qBranchA: qShaA, qBranchB: qShaB} {
		r.infra.branches[b] = sha
		c := clean
		c.Commits = []string{sha}
		r.infra.compares["trunk..."+b] = c
		r.infra.compares["trunk..."+sha] = c
	}
	return r, q
}

func createFor(branch string, item int) map[string]any {
	m := goodCreate()
	m["head"], m["body"], m["title"] = branch, goodBody(item), "Fix item "+itoa(item)
	return m
}

// closeOpen closes the officer's pull request n (a human closed or merged it).
func closeOpen(r *draftRig, n int) {
	for i := range r.infra.all {
		if r.infra.all[i].Number == n {
			r.infra.all[i].State = "closed"
		}
	}
	p := r.infra.prs[n]
	p.State = "closed"
	r.infra.prs[n] = p
}

func queuedBranches(t *testing.T, q memQueue) string {
	t.Helper()
	l, err := q.List(context.Background(), dInfra)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range l {
		out = append(out, e.Branch)
	}
	return strings.Join(out, ",")
}

func isQueued(err error) bool { return errors.Is(err, connectors.ErrGateQueued) }

// TestACappedCreateIsQueuedNotRefused: with four open, a fifth create is
// answered queued (ErrGateQueued, which is not a refusal) with a message that
// tells the officer not to escalate, and recorded at its branch's head; a
// second is queued behind it, and a repeat of the first keeps its place. A
// create whose change is refused is refused, never queued. Controls: with no
// queue the same create is the open-cap refusal, and with three open it is
// admitted and nothing is queued.
func TestACappedCreateIsQueuedNotRefused(t *testing.T) {
	r, q := newQueueRig(t)
	err := r.create(createFor(qBranchB, 19))
	if !isQueued(err) || isGateRefusal(err) {
		t.Fatalf("a fifth create (four open): err = %v, want queued and NOT a refusal", err)
	}
	for _, want := range []string{"QUEUED, not refused", "4 of this officer's pull requests are open in " + dInfra + "; the limit is 4",
		"do NOT escalate", "position 1 of 1 in the queue", "send this same create again"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the queued answer %q does not say %q", err, want)
		}
	}
	l, _ := q.List(context.Background(), dInfra)
	if len(l) != 1 || l[0].Branch != qBranchB || l[0].HeadSHA != qShaB || l[0].Item != 19 || l[0].Title != "Fix item 19" ||
		l[0].Body != goodBody(19) || l[0].Instance != dInstance || l[0].Repo != dInfra {
		t.Fatalf("the queue = %+v, want B at %s for item 19 by %s", l, qShaB, dInstance)
	}
	// The create lock was released: a second create does not wait out
	// CreateWait, and queues behind the first.
	if err := r.create(createFor(qBranchA, 17)); !isQueued(err) || !strings.Contains(err.Error(), "position 2 of 2") {
		t.Fatalf("a second capped create: %v, want queued at position 2 of 2", err)
	}
	if err := r.create(createFor(qBranchB, 19)); !isQueued(err) || !strings.Contains(err.Error(), "position 1 of 2") {
		t.Fatalf("the first branch asked again: %v, want it to keep position 1 of 2", err)
	}
	if got := queuedBranches(t, q); got != qBranchB+","+qBranchA {
		t.Fatalf("the queue lists %s", got)
	}
	if len(r.infra.openedDrafts) != 0 {
		t.Fatalf("muster opened %v at queue time", r.infra.openedDrafts)
	}
	// A refused change is refused, not queued.
	bad := r.infra.compares["trunk..."+qBranchA]
	bad.Files = []File{{Path: "nodes/n1.yaml", Status: "modified", Changes: 1, Patch: "@@ -1 +1 @@\n-a\n+b"}}
	r.infra.compares["trunk...bot/engineering-example-web/23-bad"] = bad
	r.infra.branches["bot/engineering-example-web/23-bad"] = qShaC
	r.claims[23] = dInstance
	if err := r.create(createFor("bot/engineering-example-web/23-bad", 23)); !isConflict(err) || isQueued(err) {
		t.Fatalf("a capped create with a forbidden path: %v, want the content refusal (409), not queued", err)
	}
	if got := queuedBranches(t, q); got != qBranchB+","+qBranchA {
		t.Fatalf("after the refused create the queue lists %s", got)
	}
	// Only the open cap queues: a create for an item that already has a pull
	// request (#101 is item 1's) is the per-item refusal, not queued.
	r.infra.branches["bot/engineering-example-web/1-again"] = qShaC
	r.infra.compares["trunk...bot/engineering-example-web/1-again"] = r.infra.compares["trunk..."+qBranchA]
	r.claims[1] = dInstance
	if err := r.create(createFor("bot/engineering-example-web/1-again", 1)); isQueued(err) || !isGateRefusal(err) ||
		!strings.Contains(err.Error(), "item 1 already has 1 pull request(s)") {
		t.Fatalf("a capped create for an item with a pull request: %v, want the per-item refusal, not queued", err)
	}
	if got := queuedBranches(t, q); got != qBranchB+","+qBranchA {
		t.Fatalf("after the per-item refusal the queue lists %s", got)
	}

	// Control: no queue — the open-cap refusal, as before.
	r2, _ := newQueueRig(t)
	r2.g.Queue = nil
	if err := r2.create(createFor(qBranchB, 19)); isQueued(err) || !isGateRefusal(err) || !strings.Contains(err.Error(), "the limit is 4") {
		t.Fatalf("control, no queue: %v, want the open-cap refusal", err)
	}
	// Control: three open — admitted, nothing queued.
	r3, q3 := newQueueRig(t)
	closeOpen(r3, 104)
	if err := r3.create(createFor(qBranchB, 19)); err != nil {
		t.Fatalf("control, three open: %v, want admitted", err)
	}
	if got := queuedBranches(t, q3); got != "" {
		t.Fatalf("control, three open: queued %s", got)
	}
}

// TestASlotFreeingOpensTheOldestQueuedBranch: B then A queued; a sweep with
// the cap still full opens nothing; one closed pull request frees ONE slot and
// the sweep opens exactly B, the first queued (a draft, its title, body and
// base as asked, at its head), and A waits; a second close opens A.
func TestASlotFreeingOpensTheOldestQueuedBranch(t *testing.T) {
	r, q := newQueueRig(t)
	for _, c := range []struct {
		b    string
		item int
	}{{qBranchB, 19}, {qBranchA, 17}} {
		if err := r.create(createFor(c.b, c.item)); !isQueued(err) {
			t.Fatalf("queueing %s: %v", c.b, err)
		}
	}
	ctx := context.Background()
	_ = r.g.Sweep(ctx)
	if len(r.infra.openedDrafts) != 0 || queuedBranches(t, q) != qBranchB+","+qBranchA {
		t.Fatalf("a sweep with four open opened %v (queue %s), want nothing", r.infra.openedDrafts, queuedBranches(t, q))
	}
	closeOpen(r, 102)
	_ = r.g.Sweep(ctx)
	if got := strings.Join(r.infra.openedDrafts, ","); got != qBranchB {
		t.Fatalf("one slot freed: opened %q, want only the first queued, %s", got, qBranchB)
	}
	opened := r.infra.prs[501]
	if !opened.Draft || opened.HeadRef != qBranchB || opened.HeadSHA != qShaB || opened.BaseRef != "trunk" || opened.Title != "Fix item 19" ||
		opened.Body != goodBody(19) {
		t.Fatalf("the opened pull request = %+v", opened)
	}
	if got := queuedBranches(t, q); got != qBranchA {
		t.Fatalf("after one slot: the queue lists %q, want %s waiting", got, qBranchA)
	}
	_ = r.g.Sweep(ctx)
	if len(r.infra.openedDrafts) != 1 {
		t.Fatalf("a sweep with the cap full again opened %v", r.infra.openedDrafts)
	}
	closeOpen(r, 103)
	_ = r.g.Sweep(ctx)
	if got := strings.Join(r.infra.openedDrafts, ","); got != qBranchB+","+qBranchA {
		t.Fatalf("a second slot: opened %q, want %s then %s", got, qBranchB, qBranchA)
	}
	if got := queuedBranches(t, q); got != "" {
		t.Fatalf("every slot used: the queue still lists %s", got)
	}
}

// TestAVanishedOrSupersededQueuedBranchIsSkipped: B (queued first) whose
// branch is deleted, or whose head moved with no new create, is skipped and
// A is opened in the one free slot. Control: B's new head asked for again is
// opened at that head.
func TestAVanishedOrSupersededQueuedBranchIsSkipped(t *testing.T) {
	for name, c := range map[string]struct {
		change   func(r *draftRig)
		want     string
		wantHead string
	}{
		"deleted": {func(r *draftRig) { delete(r.infra.branches, qBranchB) }, qBranchA, qShaA},
		// The new head's comparison is clean too, so only the superseded
		// rule (not an unreadable comparison) keeps B from being opened.
		"moved": {func(r *draftRig) {
			r.infra.branches[qBranchB] = qShaC
			c := r.infra.compares["trunk..."+qBranchB]
			c.Commits = []string{qShaC}
			r.infra.compares["trunk..."+qShaC] = c
		}, qBranchA, qShaA},
		// 🔴 NO QUEUE-JUMPING: B's comparison cannot be read, so B waits and
		// A, behind it, is not opened in the free slot either.
		"the first one's comparison unreadable": {func(r *draftRig) { delete(r.infra.compares, "trunk..."+qShaB) }, "", ""},
		"moved, and asked again (control)": {func(r *draftRig) {
			r.infra.branches[qBranchB] = qShaC
			c := r.infra.compares["trunk..."+qBranchB]
			c.Commits = []string{qShaC}
			r.infra.compares["trunk..."+qBranchB], r.infra.compares["trunk..."+qShaC] = c, c
			if err := r.create(createFor(qBranchB, 19)); !isQueued(err) || !strings.Contains(err.Error(), "position 1 of 2") {
				panic("asking again: " + errString(err))
			}
		}, qBranchB, qShaC},
	} {
		r, q := newQueueRig(t)
		for _, b := range []struct {
			br   string
			item int
		}{{qBranchB, 19}, {qBranchA, 17}} {
			if err := r.create(createFor(b.br, b.item)); !isQueued(err) {
				t.Fatalf("%s: queueing %s: %v", name, b.br, err)
			}
		}
		c.change(r)
		closeOpen(r, 101)
		_ = r.g.Sweep(context.Background())
		if got := strings.Join(r.infra.openedDrafts, ","); got != c.want || r.infra.prs[501].HeadSHA != c.wantHead {
			t.Errorf("%s: opened %q at %s, want %s at %s", name, got, r.infra.prs[501].HeadSHA, c.want, c.wantHead)
		}
		wantLeft := ""
		switch c.want {
		case qBranchB:
			wantLeft = qBranchA
		case "":
			wantLeft = qBranchB + "," + qBranchA
		}
		if got := queuedBranches(t, q); got != wantLeft {
			t.Errorf("%s: the queue lists %q, want %q", name, got, wantLeft)
		}
	}
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// TestQueuedCreatesOutliveTheGateThatQueuedThem: a create queued through one
// gate is opened by a NEW gate over the same store (a restarted server reads
// the durable store; the database store's own survival across a new pool is
// officers' TestTheDatabasePRQueueStoreKeepsTheContract).
func TestQueuedCreatesOutliveTheGateThatQueuedThem(t *testing.T) {
	r, q := newQueueRig(t)
	if err := r.create(createFor(qBranchB, 19)); !isQueued(err) {
		t.Fatalf("queueing: %v", err)
	}
	restarted := newDraftRig()
	restarted.web, restarted.infra = r.web, r.infra
	restarted.g.GH, restarted.g.GHFor = r.web, map[string]GitHub{fxRepo: r.web, dInfra: r.infra}
	restarted.g.Queue = memQueue{st: q.st, officer: dOfficer}
	closeOpen(r, 101)
	_ = restarted.g.Sweep(context.Background())
	if got := strings.Join(r.infra.openedDrafts, ","); got != qBranchB {
		t.Fatalf("the new gate opened %q, want the queued %s", got, qBranchB)
	}
}

// TestTheStaleDraftAgeOutIsOffUnlessEnabled: a draft of the officer's idle for
// four days is left alone with no stale_drafts block; enabled at 3 days, the
// sweep closes it, comments exactly why, and the freed slot opens the queued
// create in the same sweep. Controls: an idle-two-days draft and an idle
// pull request that is not a draft are left open.
func TestTheStaleDraftAgeOutIsOffUnlessEnabled(t *testing.T) {
	setup := func() (*draftRig, memQueue) {
		r, q := newQueueRig(t)
		if err := r.create(createFor(qBranchB, 19)); !isQueued(err) {
			t.Fatalf("queueing: %v", err)
		}
		age := func(n int, idle time.Duration, draft bool) {
			p := r.infra.prs[n]
			p.UpdatedAt, p.Draft = dNow.Add(-idle), draft
			r.infra.prs[n] = p
		}
		age(101, 4*24*time.Hour, true)   // stale
		age(102, 2*24*time.Hour, true)   // idle, not stale yet
		age(103, 10*24*time.Hour, false) // stale, but ready for review
		return r, q
	}
	r, q := setup()
	_ = r.g.Sweep(context.Background())
	if len(r.infra.closed) != 0 || len(r.infra.prComments) != 0 || len(r.infra.openedDrafts) != 0 {
		t.Fatalf("no stale_drafts block: closed %v, commented %v, opened %v; want nothing", r.infra.closed, r.infra.prComments, r.infra.openedDrafts)
	}
	if got := queuedBranches(t, q); got != qBranchB {
		t.Fatalf("no stale_drafts block: the queue lists %q", got)
	}

	r, q = setup()
	r.g.Ship.StaleDrafts = &officers.StaleDrafts{Days: 3}
	_ = r.g.Sweep(context.Background())
	if len(r.infra.closed) != 1 || r.infra.closed[0] != 101 {
		t.Fatalf("enabled at 3 days: closed %v, want only #101", r.infra.closed)
	}
	wantComment := "#101 This draft pull request had no activity for 3 days, so it was closed automatically to free " +
		"the open-pull-request slot it held. Its branch is kept: reopen this pull request, or open a new one, when the work resumes."
	if len(r.infra.prComments) != 1 || r.infra.prComments[0] != wantComment {
		t.Fatalf("the comments = %q, want exactly %q", r.infra.prComments, wantComment)
	}
	if got := strings.Join(r.infra.openedDrafts, ","); got != qBranchB {
		t.Fatalf("the slot the age-out freed: opened %q, want the queued %s", got, qBranchB)
	}
	if got := queuedBranches(t, q); got != "" {
		t.Fatalf("the queue still lists %s", got)
	}
}
