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
// create past the open cap is queued, not refused; queued at the cap, it
// closes the officer's oldest idle draft and the freed slot opens the queue at
// once; the sweep opens the oldest queued branch once a slot frees; a new
// create never jumps the queue; every skip is told to HQ; the queue lives in a
// store a new gate reads.
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

// hqItems is HQ's view of the items for the drain (PushGate.ItemGone): an
// item named in gone is gone with that reason; any other is still in HQ
// (whether or not anyone claims it, and whether or not it is completed: the
// api package's itemGone decides that). err, when set, fails every read.
type hqItems struct {
	gone map[int]string
	err  error
}

func (h *hqItems) itemGone(_ context.Context, item int) (bool, string, error) {
	if h.err != nil {
		return false, "", h.err
	}
	if why, ok := h.gone[item]; ok {
		return true, why, nil
	}
	return false, "", nil
}

// newQueueRig is the draft rig with a queue, items 17 and 19 claimed, the
// two queue branches pushed (A at qShaA, B at qShaB, each with a clean
// comparison), and FOUR of the officer's draft pull requests open in the
// infra repository (#101..#104, opened yesterday, active an hour ago, each
// clean at fxHead): the open cap is full. No cap-triggered close (the fixture
// ship has none; a test turns it on).
func newQueueRig(t *testing.T) (*draftRig, memQueue) {
	r, q, _ := newQueueRigHQ(t)
	return r, q
}

// newQueueRigHQ is newQueueRig with HQ's item view, to change.
func newQueueRigHQ(t *testing.T) (*draftRig, memQueue, *hqItems) {
	t.Helper()
	r := newDraftRig()
	r.claims[19] = dInstance
	q := memQueue{st: officers.NewMemPRQueueStore(), officer: dOfficer}
	hq := &hqItems{gone: map[int]string{}}
	r.g.Queue = q
	r.g.ItemGone = hq.itemGone
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
	return r, q, hq
}

func createFor(branch string, item int) map[string]any {
	m := goodCreate()
	m["head"], m["body"], m["title"] = branch, goodBody(item), "Fix item "+itoa(item)
	return m
}

// setPR changes the officer's pull request n in both of the fake's views.
func setPR(r *draftRig, n int, f func(p *PR)) {
	for i := range r.infra.all {
		if r.infra.all[i].Number == n {
			f(&r.infra.all[i])
		}
	}
	p := r.infra.prs[n]
	f(&p)
	r.infra.prs[n] = p
}

// closeOpen closes the officer's pull request n (a human closed or merged it).
func closeOpen(r *draftRig, n int) { setPR(r, n, func(p *PR) { p.State = "closed" }) }

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

// queueBA queues B then A at a full cap.
func queueBA(t *testing.T, r *draftRig) {
	t.Helper()
	for _, c := range []struct {
		b    string
		item int
	}{{qBranchB, 19}, {qBranchA, 17}} {
		if err := r.create(createFor(c.b, c.item)); !isQueued(err) {
			t.Fatalf("queueing %s: %v", c.b, err)
		}
	}
}

// skipNotices are the notices that say a queued create was dropped.
func skipNotices(r *draftRig) []officers.Notice {
	var out []officers.Notice
	for _, n := range r.notices {
		if strings.Contains(n.Summary, "did NOT open the queued draft pull request") {
			out = append(out, n)
		}
	}
	return out
}

// TestACappedCreateIsQueuedNotRefused: with four open, a fifth create is
// answered queued (ErrGateQueued, which is not a refusal) with a message that
// tells the officer not to escalate and to keep its item, and recorded at its
// branch's head; a second is queued behind it, and a repeat of the first keeps
// its place. A create whose change is refused, or whose item has its pull
// request, is refused, never queued. Controls: with no queue the same create
// is the open-cap refusal, and with three open it is admitted and nothing is
// queued.
func TestACappedCreateIsQueuedNotRefused(t *testing.T) {
	r, q := newQueueRig(t)
	err := r.create(createFor(qBranchB, 19))
	if !isQueued(err) || isGateRefusal(err) {
		t.Fatalf("a fifth create (four open): err = %v, want queued and NOT a refusal", err)
	}
	for _, want := range []string{"QUEUED, not refused", "4 of this officer's pull requests are open in " + dInfra + "; the limit is 4",
		"do NOT escalate", "position 1 of 1 in the queue",
		// The 202 says what the charter says (officers' kickoff pins the text).
		officers.QueuedCreateDropRule} {
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
		t.Fatalf("muster opened %v with the cap full", r.infra.openedDrafts)
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
		t.Fatalf("after the refused creates the queue lists %s", got)
	}

	// Control: no queue — the open-cap refusal, as before.
	r2, _ := newQueueRig(t)
	r2.g.Queue = nil
	if err := r2.create(createFor(qBranchB, 19)); isQueued(err) || !isGateRefusal(err) || !strings.Contains(err.Error(), "the limit is 4") {
		t.Fatalf("control, no queue: %v, want the open-cap refusal", err)
	}
	// Control: three open and nothing queued — admitted, nothing queued.
	r3, q3 := newQueueRig(t)
	closeOpen(r3, 104)
	if err := r3.create(createFor(qBranchB, 19)); err != nil {
		t.Fatalf("control, three open: %v, want admitted", err)
	}
	if got := queuedBranches(t, q3); got != "" {
		t.Fatalf("control, three open: queued %s", got)
	}
}

// TestANewCreateDoesNotJumpTheQueue: B is queued at a full cap; a human then
// closes one pull request; A's create, with that slot free, is queued BEHIND
// B, and the drain it starts opens B (the oldest), not A, which waits.
func TestANewCreateDoesNotJumpTheQueue(t *testing.T) {
	r, q := newQueueRig(t)
	if err := r.create(createFor(qBranchB, 19)); !isQueued(err) {
		t.Fatalf("queueing B: %v", err)
	}
	closeOpen(r, 101)
	// The cap-triggered close is on, and #102 is idle: A's create, queued
	// with a slot FREE, must not close it (only a full cap closes a draft).
	r.g.Ship.CloseOldestDraft = &officers.CloseOldestDraft{IdleDays: 2}
	setPR(r, 102, func(p *PR) { p.UpdatedAt = dNow.Add(-5 * 24 * time.Hour) })
	err := r.create(createFor(qBranchA, 17))
	if len(r.infra.closed) != 0 {
		t.Fatalf("A's create with a slot free closed %v, want nothing", r.infra.closed)
	}
	if !isQueued(err) || !strings.Contains(err.Error(), "1 earlier pull request(s) of this officer are queued in "+dInfra) {
		t.Fatalf("A's create with a slot free but B queued: %v, want queued behind B", err)
	}
	if got := strings.Join(r.infra.openedDrafts, ","); got != qBranchB {
		t.Fatalf("the drain A's create started opened %q, want only B, the oldest", got)
	}
	if got := queuedBranches(t, q); got != qBranchA {
		t.Fatalf("the queue lists %q, want A waiting", got)
	}
}

// TestASlotFreeingOpensTheOldestQueuedBranch: B then A queued; a sweep with
// the cap still full opens nothing; one closed pull request frees ONE slot and
// the sweep opens exactly B, the first queued (a draft, its title, body and
// base as asked, at its head), and A waits; a second close opens A.
func TestASlotFreeingOpensTheOldestQueuedBranch(t *testing.T) {
	r, q := newQueueRig(t)
	queueBA(t, r)
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
	if n := skipNotices(r); len(n) != 0 {
		t.Fatalf("a clean drain told HQ of skips: %v", n)
	}
}

// TestTheDrainSkipsAndTellsHQ: B (queued first) is skipped, and HQ told why,
// when its branch is gone, its item is no longer claimed, its stored title or
// body no longer passes, its item has its pull request now, or its change at
// the branch's current head is refused; A then takes the one free slot. A
// moved head with a clean change is opened at that head (no "superseded"
// skip). HQ unreadable stops the drain: nothing is opened, nothing is told.
func TestTheDrainSkipsAndTellsHQ(t *testing.T) {
	for name, c := range map[string]struct {
		change func(r *draftRig, q memQueue, hq *hqItems)
		// opened is the branch opened (at head); why what the skip notice
		// says ("" for no notice); paths its paths.
		opened, head, why string
		paths             []string
	}{
		"its branch is gone": {func(r *draftRig, _ memQueue, _ *hqItems) { delete(r.infra.branches, qBranchB) }, qBranchA, qShaA, "its branch is gone", nil},
		"its item is cancelled": {func(_ *draftRig, _ memQueue, hq *hqItems) {
			hq.gone[19] = "item 19 is no longer in HQ (cancelled or removed)"
		}, qBranchA, qShaA, "item 19 is no longer in HQ (cancelled or removed)", nil},
		// 🔴 A RELEASED (or lapsed, or re-claimed) item still has its pull
		// request opened: the work is in the pushed branch.
		"its item was released (no claim)": {func(r *draftRig, _ memQueue, _ *hqItems) { delete(r.claims, 19) }, qBranchB, qShaB, "", nil},
		"its stored body holds a credential": {func(r *draftRig, q memQueue, _ *hqItems) {
			_, _ = q.Add(context.Background(), officers.QueuedPR{Repo: dInfra, Branch: qBranchB, HeadSHA: qShaB, Title: "Fix item 19",
				Body: goodBody(19) + "\n" + dToken, Item: 19})
		}, qBranchA, qShaA, "its title or body was refused: its title or body has a credential-shaped value", nil},
		"its stored body breaks the contract": {func(r *draftRig, q memQueue, _ *hqItems) {
			_, _ = q.Add(context.Background(), officers.QueuedPR{Repo: dInfra, Branch: qBranchB, HeadSHA: qShaB, Title: "Fix item 19",
				Body: "no first line", Item: 19})
		}, qBranchA, qShaA, "its body no longer keeps the contract: the body's first line must be exactly", nil},
		"its item has its pull request now": {func(r *draftRig, _ memQueue, _ *hqItems) {
			r.infra.all = append(r.infra.all, PR{Number: 120, State: "closed", Author: BotLogin(r.infra.slug), AuthorType: "Bot",
				HeadRef: "bot/engineering-example-web/19-other", CreatedAt: dNow.Add(-48 * time.Hour), Body: goodBody(19)})
		}, qBranchA, qShaA, "item 19 already has 1 pull request(s)", nil},
		"its change at the current head is refused": {func(r *draftRig, _ memQueue, _ *hqItems) {
			r.infra.branches[qBranchB] = qShaC
			bad := r.infra.compares["trunk..."+qShaB]
			bad.Commits = []string{qShaC}
			bad.Files = []File{{Path: "nodes/n1.yaml", Status: "modified", Changes: 1, Patch: "@@ -1 +1 @@\n-a\n+b"}}
			r.infra.compares["trunk..."+qShaC] = bad
		}, qBranchA, qShaA, "its change was refused", []string{"nodes/n1.yaml"}},
		"its head moved, cleanly (opened at the new head)": {func(r *draftRig, _ memQueue, _ *hqItems) {
			r.infra.branches[qBranchB] = qShaC
			c := r.infra.compares["trunk..."+qShaB]
			c.Commits = []string{qShaC}
			r.infra.compares["trunk..."+qShaC] = c
		}, qBranchB, qShaC, "", nil},
		// 🔴 NO QUEUE-JUMPING: HQ cannot be read for B, so B waits and A,
		// behind it, is not opened in the free slot either.
		"HQ unreadable": {func(_ *draftRig, _ memQueue, hq *hqItems) { hq.err = errors.New("fake HQ: 503") }, "", "", "", nil},
		// The same for B's comparison.
		"its comparison unreadable": {func(r *draftRig, _ memQueue, _ *hqItems) { delete(r.infra.compares, "trunk..."+qShaB) }, "", "", "", nil},
	} {
		r, q, hq := newQueueRigHQ(t)
		queueBA(t, r)
		c.change(r, q, hq)
		r.notices = nil
		closeOpen(r, 101)
		_ = r.g.Sweep(context.Background())
		if got := strings.Join(r.infra.openedDrafts, ","); got != c.opened || r.infra.prs[501].HeadSHA != c.head {
			t.Errorf("%s: opened %q at %q, want %q at %q", name, got, r.infra.prs[501].HeadSHA, c.opened, c.head)
		}
		wantLeft := map[string]string{qBranchA: "", qBranchB: qBranchA, "": qBranchB + "," + qBranchA}[c.opened]
		if got := queuedBranches(t, q); got != wantLeft {
			t.Errorf("%s: the queue lists %q, want %q", name, got, wantLeft)
		}
		n := skipNotices(r)
		if c.why == "" {
			if len(n) != 0 {
				t.Errorf("%s: HQ was told of a skip: %+v", name, n)
			}
			continue
		}
		if len(n) != 1 || !strings.Contains(n[0].Summary, "from "+qBranchB+" in "+dInfra+" (item 19)") || !strings.Contains(n[0].Summary, c.why) ||
			strings.Join(n[0].Paths, ",") != strings.Join(c.paths, ",") || n[0].Channel != "ch/example-alerts" {
			t.Errorf("%s: the skip notices = %+v, want one for B saying %q with paths %v", name, n, c.why, c.paths)
		}
	}
}

// TestAPermanentGitHubRefusalDropsTheRowAndTransientOneWaits: B (queued
// first) is opened by GitHub with an error. A permanent refusal — 422 only
// ("No commits between" for a branch with nothing ahead of its base) — drops
// B with a notice naming GitHub's answer, and A, behind it, takes the free
// slot: B can never open, and left first it would block A for good. A
// transient one (5xx, 400, 401, 403, 404, 409, 410, 429, a network error)
// keeps both rows queued, tells HQ nothing and opens nothing (no
// queue-jumping); once GitHub accepts, the next drain opens B.
func TestAPermanentGitHubRefusalDropsTheRowAndTransientOneWaits(t *testing.T) {
	noCommits := `{"message":"Validation Failed","errors":[{"resource":"PullRequest","code":"custom","message":"No commits between trunk and ` + qBranchB + `"}]}`
	for name, c := range map[string]struct {
		err       error
		permanent bool
	}{
		"422 no commits between": {&httpError{status: 422, body: noCommits, op: "opening a draft pull request"}, true},
		"400":                    {&httpError{status: 400, body: `{"message":"Problems parsing JSON"}`, op: "opening a draft pull request"}, false},
		"410":                    {&httpError{status: 410, body: `{"message":"Gone"}`, op: "opening a draft pull request"}, false},
		"502":                    {&httpError{status: 502, body: "bad gateway", op: "opening a draft pull request"}, false},
		"500":                    {&httpError{status: 500, body: "", op: "opening a draft pull request"}, false},
		"429":                    {&httpError{status: 429, body: `{"message":"rate limit"}`, op: "opening a draft pull request"}, false},
		"403 secondary limit":    {&httpError{status: 403, body: `{"message":"You have exceeded a secondary rate limit"}`, op: "opening a draft pull request"}, false},
		"401":                    {&httpError{status: 401, body: `{"message":"Bad credentials"}`, op: "opening a draft pull request"}, false},
		"404":                    {&httpError{status: 404, body: `{"message":"Not Found"}`, op: "opening a draft pull request"}, false},
		"409":                    {&httpError{status: 409, body: `{"message":"Conflict"}`, op: "opening a draft pull request"}, false},
		"network":                {errors.New("could not reach GitHub while opening a draft pull request: connection reset"), false},
	} {
		r, q, _ := newQueueRigHQ(t)
		queueBA(t, r)
		r.notices = nil
		r.infra.openDraftErrFor = map[string]error{qBranchB: c.err}
		closeOpen(r, 101)
		_ = r.g.Sweep(context.Background())
		n := skipNotices(r)
		if c.permanent {
			if got := strings.Join(r.infra.openedDrafts, ","); got != qBranchA {
				t.Errorf("%s: opened %q, want %s (B dropped, A behind it opened)", name, got, qBranchA)
			}
			if got := queuedBranches(t, q); got != "" {
				t.Errorf("%s: the queue lists %q, want nothing", name, got)
			}
			if len(n) != 1 || !strings.Contains(n[0].Summary, "from "+qBranchB+" in "+dInfra+" (item 19)") ||
				!strings.Contains(n[0].Summary, "GitHub refused to open it ("+itoa(statusOf(c.err))+")") ||
				!strings.Contains(n[0].Summary, "GitHub answered "+itoa(statusOf(c.err))+" while opening a draft pull request: "+c.err.(*httpError).body) {
				t.Errorf("%s: the skip notices = %+v, want one for B naming GitHub's refusal", name, n)
			}
			continue
		}
		if got := strings.Join(r.infra.openedDrafts, ","); got != "" {
			t.Errorf("%s: opened %q, want nothing (B waits, A does not jump it)", name, got)
		}
		if got := queuedBranches(t, q); got != qBranchB+","+qBranchA {
			t.Errorf("%s: the queue lists %q, want both still queued", name, got)
		}
		if len(n) != 0 {
			t.Errorf("%s: HQ was told of a skip: %+v", name, n)
		}
		r.infra.openDraftErrFor = nil
		_ = r.g.Sweep(context.Background())
		if got := strings.Join(r.infra.openedDrafts, ","); got != qBranchB {
			t.Errorf("%s: once GitHub accepts, opened %q, want %s", name, got, qBranchB)
		}
	}
}

// TestA422PullRequestAlreadyExistsKeepsTheRowAndTellsNobody: GitHub answers
// B's open with 422 "A pull request already exists" (one was opened from B
// after the drain's own reads found none). That is not a refusal — nothing
// is lost — so HQ is told nothing, the row is KEPT (first, so A does not
// jump it) and the drain's error says why. The next drain's own read finds
// that pull request and drops B's row with no notice.
func TestA422PullRequestAlreadyExistsKeepsTheRowAndTellsNobody(t *testing.T) {
	exists := `{"message":"Validation Failed","errors":[{"resource":"PullRequest","code":"custom","message":"A pull request already exists for example:` + qBranchB + `."}]}`
	r, q, _ := newQueueRigHQ(t)
	queueBA(t, r)
	r.notices = nil
	r.infra.openDraftErrFor = map[string]error{qBranchB: &httpError{status: 422, body: exists, op: "opening a draft pull request"}}
	closeOpen(r, 101)
	sr := r.g.Ship.Repos[1]
	err := r.g.drainQueue(context.Background(), sr, r.infra)
	if err == nil || !strings.Contains(err.Error(), "GitHub says a pull request already exists from its branch, though none was listed open: the row is kept") {
		t.Errorf("the drain's error = %v, want it to say the pull request already exists and the row is kept", err)
	}
	if got := strings.Join(r.infra.openedDrafts, ","); got != "" {
		t.Errorf("opened %q, want nothing (B kept first, A does not jump it)", got)
	}
	if got := queuedBranches(t, q); got != qBranchB+","+qBranchA {
		t.Errorf("the queue lists %q, want both still queued", got)
	}
	if n := skipNotices(r); len(n) != 0 {
		t.Errorf("HQ was told of a skip: %+v", n)
	}
	// The pull request GitHub named is open from B (it fills the freed slot).
	p := PR{Number: 777, State: "open", Draft: true, Author: BotLogin(r.infra.slug), AuthorType: "Bot", HeadRef: qBranchB, HeadSHA: qShaB,
		HeadRepo: dInfra, BaseRef: "trunk", CreatedAt: dNow, UpdatedAt: dNow, Body: goodBody(19)}
	r.infra.all = append(r.infra.all, p)
	r.infra.prs[p.Number] = p
	r.infra.openDraftErrFor = nil
	if err := r.g.drainQueue(context.Background(), sr, r.infra); err != nil {
		t.Fatalf("the next drain: %v", err)
	}
	if got := queuedBranches(t, q); got != qBranchA {
		t.Errorf("after the next drain the queue lists %q, want only %s (B's row dropped, its pull request found)", got, qBranchA)
	}
	if got := strings.Join(r.infra.openedDrafts, ","); got != "" {
		t.Errorf("the next drain opened %q, want nothing (B has #777; the cap is full again)", got)
	}
	if n := skipNotices(r); len(n) != 0 {
		t.Errorf("HQ was told of a skip: %+v", n)
	}
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
	restarted.web, restarted.infra, restarted.claims = r.web, r.infra, r.claims
	restarted.g.GH, restarted.g.GHFor = r.web, map[string]GitHub{fxRepo: r.web, dInfra: r.infra}
	restarted.g.Queue = memQueue{st: q.st, officer: dOfficer}
	restarted.g.ItemGone = (&hqItems{}).itemGone
	closeOpen(r, 101)
	_ = restarted.g.Sweep(context.Background())
	if got := strings.Join(r.infra.openedDrafts, ","); got != qBranchB {
		t.Fatalf("the new gate opened %q, want the queued %s", got, qBranchB)
	}
}

// otherOfficerPR is a draft of ANOTHER officer that shares this one's App: its
// author is the same bot, its branch and first line are the other officer's.
func otherOfficerPR(r *draftRig, idle time.Duration) PR {
	return PR{Number: 130, State: "open", Draft: true, Author: BotLogin(r.infra.slug), AuthorType: "Bot",
		HeadRef: "bot/engineering-other/9-x", HeadSHA: fxHead, HeadRepo: dInfra, BaseRef: "trunk", CreatedAt: dNow.Add(-30 * 24 * time.Hour),
		UpdatedAt: dNow.Add(-idle), Body: officers.BodyFirstLine("engineering:other", 9) + "\n## Root cause\n## Evidence\n## Test\n## Rollback\n"}
}

// TestACappedCreateClosesTheOldestIdleDraft: queued at a full cap with the
// close on at 2 days, B's create closes the officer's ONE oldest draft by last
// activity — #102, idle 5 days — not the older pull request marked ready
// (#103), not ANOTHER officer's even older draft on the same App (#130), not
// the other idle one (#101, 3 days); it comments exactly why, tells HQ, and
// the freed slot opens B at once. Controls: with every own draft active under
// 2 days, or the close off, nothing is closed and B stays queued.
func TestACappedCreateClosesTheOldestIdleDraft(t *testing.T) {
	setup := func(t *testing.T, idle map[int]time.Duration) (*draftRig, memQueue) {
		r, q := newQueueRig(t)
		for n, d := range idle {
			d := d
			setPR(r, n, func(p *PR) { p.UpdatedAt = dNow.Add(-d) })
		}
		setPR(r, 103, func(p *PR) { p.Draft = false })
		o := otherOfficerPR(r, 20*24*time.Hour)
		r.infra.all = append(r.infra.all, o)
		r.infra.prs[o.Number] = o
		r.g.Ship.CloseOldestDraft = &officers.CloseOldestDraft{IdleDays: 2}
		return r, q
	}
	day := 24 * time.Hour
	r, q := setup(t, map[int]time.Duration{101: 3 * day, 102: 5 * day, 103: 10 * day, 104: time.Hour})
	err := r.create(createFor(qBranchB, 19))
	if !isQueued(err) || !strings.Contains(err.Error(), "muster closed your oldest idle draft, #102, to free a slot") {
		t.Fatalf("a capped create with an idle draft: %v, want queued, naming #102 closed", err)
	}
	if len(r.infra.closed) != 1 || r.infra.closed[0] != 102 {
		t.Fatalf("closed %v, want only #102 (the oldest idle own draft)", r.infra.closed)
	}
	wantComment := "#102 This draft pull request had no activity for 2 days or more, and its author reached its limit of open " +
		"pull requests here, so it was closed automatically to make room for a newer one. Its branch is kept: reopen this pull request when " +
		"the work resumes."
	if len(r.infra.prComments) != 1 || r.infra.prComments[0] != wantComment {
		t.Fatalf("the comments = %q, want exactly %q", r.infra.prComments, wantComment)
	}
	told := false
	for _, n := range r.notices {
		told = told || (n.PR == 102 && strings.Contains(n.Summary, "muster closed its idle draft #102"))
	}
	if !told {
		t.Fatalf("HQ was not told of the close: %+v", r.notices)
	}
	if got := strings.Join(r.infra.openedDrafts, ","); got != qBranchB {
		t.Fatalf("the freed slot opened %q, want B at once", got)
	}
	if got := queuedBranches(t, q); got != "" {
		t.Fatalf("the queue still lists %s", got)
	}

	for name, c := range map[string]struct {
		idle    map[int]time.Duration
		off     bool
		touched bool
	}{
		"every own draft active under 2 days": {idle: map[int]time.Duration{101: 47 * time.Hour, 102: 30 * time.Hour, 103: 10 * day, 104: time.Hour}},
		"the close off":                       {idle: map[int]time.Duration{101: 3 * day, 102: 5 * day, 103: 10 * day, 104: time.Hour}, off: true},
		// The list says #102 is idle, but a fresh read shows activity since:
		// it is read again before the close, and left open.
		"the oldest was active since it was listed": {idle: map[int]time.Duration{101: 30 * time.Hour, 102: 5 * day, 103: 10 * day, 104: time.Hour},
			touched: true},
	} {
		r, q := setup(t, c.idle)
		if c.off {
			r.g.Ship.CloseOldestDraft = nil
		}
		if c.touched {
			p := r.infra.prs[102]
			p.UpdatedAt = dNow.Add(-time.Minute)
			r.infra.prs[102] = p
		}
		if err := r.create(createFor(qBranchB, 19)); !isQueued(err) || strings.Contains(err.Error(), "closed your oldest") {
			t.Errorf("%s: %v, want queued with nothing closed", name, err)
		}
		if len(r.infra.closed) != 0 || len(r.infra.prComments) != 0 || len(r.infra.openedDrafts) != 0 {
			t.Errorf("%s: closed %v, commented %v, opened %v; want nothing", name, r.infra.closed, r.infra.prComments, r.infra.openedDrafts)
		}
		if got := queuedBranches(t, q); got != qBranchB {
			t.Errorf("%s: the queue lists %q", name, got)
		}
	}
}

// TestTheCloseNeverReachesBeyondTheOfficersOwnDrafts: with every one of the
// officer's own open pull requests either active or marked ready, the only
// idle drafts in the repository are another officer's on the same App — and
// the capped create closes nothing.
func TestTheCloseNeverReachesBeyondTheOfficersOwnDrafts(t *testing.T) {
	r, q := newQueueRig(t)
	setPR(r, 101, func(p *PR) { p.Draft, p.UpdatedAt = false, dNow.Add(-40*24*time.Hour) })
	o := otherOfficerPR(r, 20*24*time.Hour)
	r.infra.all = append(r.infra.all, o)
	r.infra.prs[o.Number] = o
	r.g.Ship.CloseOldestDraft = &officers.CloseOldestDraft{IdleDays: 2}
	if err := r.create(createFor(qBranchB, 19)); !isQueued(err) {
		t.Fatalf("queueing: %v", err)
	}
	if len(r.infra.closed) != 0 || len(r.infra.prComments) != 0 {
		t.Fatalf("closed %v, commented %v: want nothing (#101 is ready, #130 is another officer's)", r.infra.closed, r.infra.prComments)
	}
	if got := queuedBranches(t, q); got != qBranchB {
		t.Fatalf("the queue lists %q", got)
	}
}

// TestARepeatCreateOfAQueuedBranchClosesNoMoreDrafts: with the close on and
// FOUR idle drafts, B's first capped create closes ONE (#101, the oldest);
// B cannot be opened yet (its head's comparison is unreadable, so the drain
// waits), and the freed slot is taken by another pull request. Asked again
// four times at the full cap, B — still queued, refreshed in place — closes
// nothing more.
func TestARepeatCreateOfAQueuedBranchClosesNoMoreDrafts(t *testing.T) {
	r, q := newQueueRig(t)
	day := 24 * time.Hour
	for n, idle := range map[int]time.Duration{101: 9 * day, 102: 8 * day, 103: 7 * day, 104: 6 * day} {
		idle := idle
		setPR(r, n, func(p *PR) { p.UpdatedAt = dNow.Add(-idle) })
	}
	r.g.Ship.CloseOldestDraft = &officers.CloseOldestDraft{IdleDays: 2}
	delete(r.infra.compares, "trunk..."+qShaB)
	if err := r.create(createFor(qBranchB, 19)); !isQueued(err) || !strings.Contains(err.Error(), "#101") {
		t.Fatalf("B's first create: %v, want queued, closing #101", err)
	}
	if got := strings.Join(intsOf(r.infra.closed), ","); got != "101" {
		t.Fatalf("B's first create closed %s, want 101", got)
	}
	taken := PR{Number: 140, State: "open", Draft: true, Author: BotLogin(r.infra.slug), AuthorType: "Bot",
		HeadRef: "bot/engineering-example-web/40-x", HeadSHA: fxHead, HeadRepo: dInfra, BaseRef: "trunk",
		CreatedAt: dNow.Add(-20 * time.Hour), UpdatedAt: dNow, Body: goodBody(40)}
	r.infra.all = append(r.infra.all, taken)
	r.infra.prs[taken.Number] = taken
	for i := 0; i < 4; i++ {
		if err := r.create(createFor(qBranchB, 19)); !isQueued(err) || strings.Contains(err.Error(), "closed your oldest") {
			t.Fatalf("B asked again (%d): %v, want queued with nothing closed", i+1, err)
		}
	}
	if got := strings.Join(intsOf(r.infra.closed), ","); got != "101" {
		t.Fatalf("after four re-asks: closed %s, want only 101", got)
	}
	if got := queuedBranches(t, q); got != qBranchB {
		t.Fatalf("the queue lists %q, want B", got)
	}
}

// TestNoDraftIsClosedWhileTheDailyLimitIsFull: at the full cap with an idle
// draft, but with three pull requests already opened today (in the other
// repository), the capped create closes nothing: the slot could not be used
// today. Control: the same with two opened today closes the idle draft.
func TestNoDraftIsClosedWhileTheDailyLimitIsFull(t *testing.T) {
	for _, c := range []struct {
		today  int
		closed string
	}{{3, ""}, {2, "102"}} {
		r, _ := newQueueRig(t)
		setPR(r, 102, func(p *PR) { p.UpdatedAt = dNow.Add(-5 * 24 * time.Hour) })
		r.g.Ship.CloseOldestDraft = &officers.CloseOldestDraft{IdleDays: 2}
		for i := 0; i < c.today; i++ {
			r.web.all = append(r.web.all, PR{Number: 60 + i, State: "closed", Author: BotLogin(r.web.slug), AuthorType: "Bot",
				HeadRef: "officer/engineering-example-web/" + itoa(60+i) + "-x", CreatedAt: dNow.Add(-time.Hour), Body: goodBody(60 + i)})
		}
		if err := r.create(createFor(qBranchB, 19)); !isQueued(err) {
			t.Fatalf("%d today: %v, want queued", c.today, err)
		}
		if got := strings.Join(intsOf(r.infra.closed), ","); got != c.closed {
			t.Errorf("%d opened today: closed %q, want %q", c.today, got, c.closed)
		}
	}
}

// TestTwoDrainsOfOneRowOpenOnePullRequestAndTellNoDrop: drain X holds the
// create lock while drain Y, which listed the row and passed its first reads,
// waits for it. X then (a) OPENS the row's pull request and settles it, (b)
// opens it but has NOT settled it yet (its settle still in flight, or
// failed), or (c) DROPS the row (settles it skipped) while the item gains a
// pull request elsewhere; Y, once it has the lock, must open no second pull
// request and send HQ no "dropped" notice of its own.
func TestTwoDrainsOfOneRowOpenOnePullRequestAndTellNoDrop(t *testing.T) {
	for _, mode := range []string{"opened", "opened, unsettled", "dropped"} {
		opened := mode != "dropped"
		r, q, hq := newQueueRigHQ(t)
		r.g.CreateWait = 10 * time.Second
		if err := r.create(createFor(qBranchB, 19)); !isQueued(err) {
			t.Fatalf("queueing B: %v", err)
		}
		closeOpen(r, 101)
		ctx := context.Background()
		row, _ := q.List(ctx, dInfra)
		// X takes the lock; Y signals once past its reads before the lock.
		if err := r.g.acquireCreate(ctx); err != nil {
			t.Fatal(err)
		}
		reached := make(chan struct{}, 1)
		r.g.ItemGone = func(c context.Context, item int) (bool, string, error) {
			select {
			case reached <- struct{}{}:
			default:
			}
			return hq.itemGone(c, item)
		}
		done := make(chan error, 1)
		go func() { _, err := r.g.drainOne(ctx, r.g.Ship.Repos[1], r.infra, row[0]); done <- err }()
		<-reached
		time.Sleep(100 * time.Millisecond) // Y is now waiting for the lock
		if opened {
			pr, _ := r.infra.OpenDraftPR(ctx, row[0].Title, row[0].Branch, "trunk", row[0].Body)
			if mode == "opened" {
				if ok, _ := q.Settle(ctx, row[0].ID, row[0].HeadSHA, officers.PRQueueOpened, pr.Number, ""); !ok {
					t.Fatal("X's settle")
				}
			}
		} else {
			if ok, _ := q.Settle(ctx, row[0].ID, row[0].HeadSHA, officers.PRQueueSkipped, 0, "X dropped it"); !ok {
				t.Fatal("X's settle")
			}
			r.infra.all = append(r.infra.all, PR{Number: 121, State: "closed", Author: BotLogin(r.infra.slug), AuthorType: "Bot",
				HeadRef: "bot/engineering-example-web/19-other", CreatedAt: dNow.Add(-48 * time.Hour), Body: goodBody(19)})
		}
		r.notices = nil
		r.g.releaseCreate()
		if err := <-done; err != nil {
			t.Fatalf("%s: Y: %v", mode, err)
		}
		wantOpened := 0
		if opened {
			wantOpened = 1
		}
		if len(r.infra.openedDrafts) != wantOpened {
			t.Errorf("%s: %d pull request(s) opened from B (%v), want %d", mode, len(r.infra.openedDrafts), r.infra.openedDrafts, wantOpened)
		}
		if n := skipNotices(r); len(n) != 0 {
			t.Errorf("%s: Y told HQ of a drop: %+v", mode, n)
		}
	}
}

func intsOf(ns []int) []string {
	var out []string
	for _, n := range ns {
		out = append(out, itoa(n))
	}
	return out
}
