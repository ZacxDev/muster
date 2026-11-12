package officership

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/officers"
)

// The authoring officer's writes on a pull request in its review loop
// (fixpush.go), on the draft rig: #31 in example-infra, from dBranch (named
// for item 17, the ORIGINAL item), authored by the officer's own App. HQ put
// it in a loop (fakeLoops); the fix item is 44, kind `investigate.pr_fix`,
// subject example-org/example-infra#31, held by dInstance unless a test
// changes that.

const fixItem = "44"

// fakeLoops is PRLoops in memory.
type fakeLoops struct {
	mu      sync.Mutex
	state   map[string]string
	heads   map[string]string
	at      map[string]time.Time
	readErr error
	// settleErr fails every Settle (counted in settles).
	settleErr error
	settles   int
}

func newFakeLoops() *fakeLoops {
	return &fakeLoops{state: map[string]string{}, heads: map[string]string{}, at: map[string]time.Time{}}
}

func loopKey(repo string, n int) string { return strings.ToLower(repo) + "#" + fmt.Sprint(n) }

func (f *fakeLoops) Announced(_ context.Context, repo string, n int, head string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.state[loopKey(repo, n)]; !ok {
		f.state[loopKey(repo, n)], f.heads[loopKey(repo, n)] = officers.PRLoopPending, head
	}
	return nil
}
func (f *fakeLoops) Settle(_ context.Context, repo string, n int, state string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settles++
	if f.settleErr != nil {
		return f.settleErr
	}
	f.state[loopKey(repo, n)] = state
	return nil
}
func (f *fakeLoops) AnyInLoop(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, st := range f.state {
		if st == officers.PRLoopInLoop {
			return true, nil
		}
	}
	return false, nil
}
func (f *fakeLoops) Pending(context.Context) ([]officers.PRLoop, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []officers.PRLoop
	for k, st := range f.state {
		if st == officers.PRLoopPending {
			r, n, _ := subjectPR(k)
			out = append(out, officers.PRLoop{Repo: r, PR: n, HeadSHA: f.heads[k], State: st, AnnouncedAt: f.at[k]})
		}
	}
	return out, nil
}
func (f *fakeLoops) InLoop(_ context.Context, repo string, n int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state[loopKey(repo, n)] == officers.PRLoopInLoop, f.readErr
}
func (f *fakeLoops) get(repo string, n int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state[loopKey(repo, n)]
}

type fixRig struct {
	*draftRig
	loops         *fakeLoops
	held          []officers.Item
	heldErr       error
	officerShadow bool
	lines         []string
}

func newFixRig() *fixRig {
	r := &fixRig{draftRig: newDraftRig(), loops: newFakeLoops()}
	no := false
	r.loops.state[loopKey(dInfra, dPR)] = officers.PRLoopInLoop
	r.held = []officers.Item{{ID: fixItem, Kind: "investigate.pr_fix", SubjectKey: "example-org/example-infra#31", Shadow: &no}}
	r.g.Loops = r.loops
	r.g.FixItems = func(_ context.Context, instance string) ([]officers.Item, error) {
		if instance != dInstance {
			return nil, nil
		}
		return r.held, r.heldErr
	}
	r.g.OfficerShadow = func(context.Context) (bool, error) { return r.officerShadow, nil }
	r.g.Shadowed = func(it officers.Item, line string) { r.lines = append(r.lines, string(it.ID)+": "+line) }
	r.infra.all = []PR{r.infra.prs[dPR]}
	return r
}

// pushAsInstance is a push of ref from oidA to oidB by dInstance (the caller
// the proxy puts in the request's context).
func (r *fixRig) pushAsInstance(ref string) (*http.Request, error) {
	body := pkt(oidA+" "+oidB+" "+ref+"\x00report-status\n") + "0000"
	rq := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	rq = rq.WithContext(connectors.WithCaller(rq.Context(), connectors.Caller{AgentName: dInstance}))
	return r.g.Admit(rq, "github.com", dInfra+".git/git-receive-pack")
}

func ghWrites(f *fakeGH) int {
	return len(f.createdRefs) + len(f.updated) + len(f.readied) + len(f.deletedRefs) + len(f.merged) + len(f.closed) + len(f.deleted)
}

// loopWriteCase is one of the author's four write types on #31.
type loopWriteCase struct {
	name, would string
	do          func(r *fixRig) (*http.Request, error)
}

func loopWriteCases() []loopWriteCase {
	return []loopWriteCase{
		{"push", fmt.Sprintf("pushed %s..%s to %s", oidA[:12], oidB[:12], dBranch), func(r *fixRig) (*http.Request, error) {
			return r.pushAsInstance("refs/heads/" + dBranch)
		}},
		{"comment", fmt.Sprintf("commented on %s#%d (%d chars)", dInfra, dPR, len("Fixed the naming.")), func(r *fixRig) (*http.Request, error) {
			return apiAdmit(r.g, http.MethodPost, fmt.Sprintf("repos/%s/issues/%d/comments", dInfra, dPR), map[string]any{"body": "Fixed the naming."})
		}},
		{"title and body edit", fmt.Sprintf("edited the title and body of %s#%d", dInfra, dPR), func(r *fixRig) (*http.Request, error) {
			return apiAdmit(r.g, http.MethodPatch, fmt.Sprintf("repos/%s/pulls/%d", dInfra, dPR), map[string]any{"title": "Raise the limit (fixed)", "body": goodBody(17)})
		}},
		{"close", fmt.Sprintf("closed %s#%d", dInfra, dPR), func(r *fixRig) (*http.Request, error) {
			return apiAdmit(r.g, http.MethodPatch, fmt.Sprintf("repos/%s/pulls/%d", dInfra, dPR), map[string]any{"state": "closed"})
		}},
	}
}

// TestAnInLoopPullRequestTakesWritesOnlyUnderAHeldFixItem: each write type,
// on a pull request HQ put in its loop — held live and not in shadow:
// admitted; no fix item held (a lapsed lease and a released item read the
// same: FixItems returns live holds only): refused naming the rule; held in
// shadow: refused with its "would have …" line, recorded, nothing sent.
func TestAnInLoopPullRequestTakesWritesOnlyUnderAHeldFixItem(t *testing.T) {
	for _, w := range loopWriteCases() {
		t.Run(w.name+"/held live", func(t *testing.T) {
			r := newFixRig()
			if out, err := w.do(r); err != nil || out == nil {
				t.Fatalf("held live: err = %v", err)
			}
			if len(r.lines) != 0 {
				t.Fatalf("recorded %v out of shadow", r.lines)
			}
		})
		t.Run(w.name+"/not held", func(t *testing.T) {
			r := newFixRig()
			r.held = nil
			out, err := w.do(r)
			if out != nil || !errors.Is(err, connectors.ErrGateRefused) || !strings.Contains(err.Error(), "#31 is in its review loop") ||
				!strings.Contains(err.Error(), "*.pr_fix item for example-org/example-infra#31 held under a live lease") {
				t.Fatalf("not held: err = %v (forwarded %v)", err, out != nil)
			}
		})
		t.Run(w.name+"/held in shadow", func(t *testing.T) {
			r := newFixRig()
			r.held[0].Shadow = boolp(true)
			out, err := w.do(r)
			want := "shadow: would have " + w.would
			if out != nil || err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("held in shadow: err = %v (forwarded %v), want %q", err, out != nil, want)
			}
			if n := ghWrites(r.infra); n != 0 {
				t.Fatalf("%d GitHub writes for a shadowed %s", n, w.name)
			}
			if len(r.lines) != 1 || r.lines[0] != fixItem+": would have "+w.would {
				t.Fatalf("recorded %q", r.lines)
			}
		})
	}
}

// TestAPullRequestOutOfTheLoopIsUnchanged: not in the loop and no fix item
// held, each write is the draft stage's as before (admitted).
func TestAPullRequestOutOfTheLoopIsUnchanged(t *testing.T) {
	for _, w := range loopWriteCases() {
		r := newFixRig()
		r.held = nil
		r.loops.state[loopKey(dInfra, dPR)] = officers.PRLoopNoTaker
		if out, err := w.do(r); err != nil || out == nil {
			t.Errorf("%s out of the loop: err = %v", w.name, err)
		}
	}
}

// TestAFixItemForAnotherPullRequestDoesNotOpenThisOne: holding #32's fix item
// is not holding #31's: a write to #31 (in its loop) is refused; and #32's
// item in shadow records nothing for #31.
func TestAFixItemForAnotherPullRequestDoesNotOpenThisOne(t *testing.T) {
	r := newFixRig()
	r.held[0].SubjectKey = "example-org/example-infra#32"
	r.held[0].Shadow = boolp(true)
	if _, err := r.pushAsInstance("refs/heads/" + dBranch); err == nil || !strings.Contains(err.Error(), "#31 is in its review loop") {
		t.Fatalf("a push to #31 holding #32's fix item: err = %v, want the loop refusal", err)
	}
	if len(r.lines) != 0 {
		t.Fatalf("recorded %v for another pull request's item", r.lines)
	}
}

func TestAFixWriteToAPullRequestOfAnotherAppIsRefused(t *testing.T) {
	r := newFixRig()
	pr := r.infra.prs[dPR]
	pr.Author = "other-officer[bot]"
	r.infra.prs[dPR] = pr
	r.infra.all = []PR{pr}
	out, err := r.pushAsInstance("refs/heads/" + dBranch)
	if out != nil || !errors.Is(err, connectors.ErrGateRefused) || !strings.Contains(err.Error(), "is authored by other-officer[bot], not this officer's App") {
		t.Fatalf("a fix push to another App's pull request: err = %v", err)
	}
}

func TestFixShadowFollowsTheOfficerRowWhenTheItemSaysNothing(t *testing.T) {
	for name, c := range map[string]struct {
		set     func(r *fixRig)
		refused bool
	}{
		"officer shadow":     {func(r *fixRig) { r.held[0].Shadow = nil; r.officerShadow = true }, true},
		"officer not shadow": {func(r *fixRig) { r.held[0].Shadow = nil; r.officerShadow = false }, false},
		"the row unreadable": {func(r *fixRig) {
			r.held[0].Shadow = nil
			r.g.OfficerShadow = func(context.Context) (bool, error) { return false, errors.New("fake 503") }
		}, true},
	} {
		t.Run(name, func(t *testing.T) {
			r := newFixRig()
			c.set(r)
			_, err := r.pushAsInstance("refs/heads/" + dBranch)
			if refused := err != nil && strings.Contains(err.Error(), "shadow: would have pushed"); refused != c.refused {
				t.Fatalf("err = %v, want refused in shadow %v", err, c.refused)
			}
		})
	}
}

func TestUnreadableLoopStateRefuses(t *testing.T) {
	r := newFixRig()
	r.heldErr = errors.New("fake HQ: 503")
	if out, err := r.pushAsInstance("refs/heads/" + dBranch); out != nil || err == nil || !strings.Contains(err.Error(), "could not read HQ's items") {
		t.Fatalf("HQ unreadable: err = %v", err)
	}
	r = newFixRig()
	r.held = nil
	r.loops.readErr = errors.New("fake store: down")
	if out, err := r.pushAsInstance("refs/heads/" + dBranch); out != nil || err == nil || !strings.Contains(err.Error(), "could not read whether #31 is in its review loop") {
		t.Fatalf("the loop record unreadable: err = %v", err)
	}
}

// TestAFixItemOpensNoPullRequest: a POST pulls whose branch's item HQ shows
// to be a fix item is refused 403; the same create under an ordinary item
// (control) is admitted.
func TestAFixItemOpensNoPullRequest(t *testing.T) {
	r := newDraftRig()
	r.kinds[17] = "investigate.pr_fix"
	err := r.create(goodCreate())
	if !errors.Is(err, connectors.ErrGateRefused) || !strings.Contains(err.Error(), "a fix item pushes to the pull request's existing branch") {
		t.Fatalf("a create under a fix item: err = %v", err)
	}
	r = newDraftRig()
	r.kinds[17] = "investigate.opened"
	if err := r.create(goodCreate()); err != nil {
		t.Fatalf("control: a create under an ordinary item: %v", err)
	}
}

// TestAWriteOutOfTheLoopNeedsNoHQ: with HQ down, a write to a pull request
// out of its loop is admitted (the local record is read first; HQ is not
// asked), and one in its loop is refused (fail closed).
func TestAWriteOutOfTheLoopNeedsNoHQ(t *testing.T) {
	for _, w := range loopWriteCases() {
		r := newFixRig()
		r.heldErr = errors.New("fake HQ: 503")
		r.loops.state[loopKey(dInfra, dPR)] = officers.PRLoopNoTaker
		r.loops.state[loopKey(dInfra, 99)] = officers.PRLoopInLoop // another PR in a loop: the push's lookup runs
		if out, err := w.do(r); err != nil || out == nil {
			t.Errorf("%s out of the loop with HQ down: err = %v, want admitted", w.name, err)
		}
		r = newFixRig()
		r.heldErr = errors.New("fake HQ: 503")
		if out, err := w.do(r); out != nil || err == nil || !strings.Contains(err.Error(), "could not read HQ's items") {
			t.Errorf("%s in the loop with HQ down: err = %v, want refused", w.name, err)
		}
	}
}

// TestNoLoopAtAllSkipsTheBranchLookup: an officer with no pull request in a
// loop pushes without the branch's pull requests being looked up — a GitHub
// failure there blocks nothing (zero lookups); with one in a loop the
// lookup runs (control) and its failure refuses.
func TestNoLoopAtAllSkipsTheBranchLookup(t *testing.T) {
	r := newFixRig()
	r.loops.state[loopKey(dInfra, dPR)] = officers.PRLoopNoTaker
	r.infra.byBranchErr = errors.New("fake GitHub: 502")
	before := r.infra.byBranch
	if out, err := r.pushAsInstance("refs/heads/" + dBranch); err != nil || out == nil {
		t.Fatalf("no loop at all, GitHub failing: err = %v, want admitted", err)
	}
	if n := r.infra.byBranch - before; n != 0 {
		t.Fatalf("%d branch lookups with no loop at all, want 0", n)
	}
	r.loops.state[loopKey(dInfra, 99)] = officers.PRLoopInLoop
	if _, err := r.pushAsInstance("refs/heads/" + dBranch); err == nil || !strings.Contains(err.Error(), "could not look up the pull requests") {
		t.Fatalf("control, a loop elsewhere: err = %v, want the lookup failure", err)
	}
}
