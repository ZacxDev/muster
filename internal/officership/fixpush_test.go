package officership

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/officers"
)

// A draft officer's fix of its own pull request (fixpush.go), on the draft
// rig: #31 in example-infra, from dBranch (named for item 17, the ORIGINAL
// item), authored by the officer's own App. The fix item is 44, kind
// `investigate.pr_fix`, subject example-org/example-infra#31.

const fixItem = "44"

type fixRig struct {
	*draftRig
	held          []officers.Item
	heldErr       error
	fixReads      int
	officerShadow bool
	lines         []string
}

func newFixRig() *fixRig {
	r := &fixRig{draftRig: newDraftRig()}
	no := false
	r.held = []officers.Item{{ID: fixItem, Kind: "investigate.pr_fix", SubjectKey: "example-org/example-infra#31", Shadow: &no}}
	r.g.FixItems = func(_ context.Context, instance string) ([]officers.Item, error) {
		r.fixReads++
		if instance != dInstance {
			return nil, nil
		}
		return r.held, r.heldErr
	}
	r.g.OfficerShadow = func(context.Context) (bool, error) { return r.officerShadow, nil }
	r.g.Shadowed = func(it officers.Item, line string) { r.lines = append(r.lines, string(it.ID)+": "+line) }
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

func TestAFixPushToTheOwnPullRequestIsAdmittedUnderTheHeldFixItem(t *testing.T) {
	r := newFixRig()
	out, err := r.pushAsInstance("refs/heads/" + dBranch)
	if err != nil || out == nil {
		t.Fatalf("a fix push to #31's branch under the held fix item: err = %v", err)
	}
	if r.fixReads != 1 {
		t.Fatalf("HQ's fix items read %d times, want 1 (the push is looked up)", r.fixReads)
	}
	if connectors.AfterForwardOf(out) == nil {
		t.Fatal("the fix push lost the draft stage's push re-check")
	}
}

// TestAFixItemForAnotherPullRequestChangesNothing: the existing rule (a draft
// officer's push to its own branch needs no item) still admits it.
func TestAFixItemForAnotherPullRequestChangesNothing(t *testing.T) {
	r := newFixRig()
	r.held[0].SubjectKey = "example-org/example-infra#32"
	r.infra.prs[32] = PR{Number: 32, State: "open", Author: BotLogin(r.infra.slug), HeadRef: "bot/engineering-example-web/20-other",
		HeadRepo: dInfra, BaseRef: "trunk", BaseRepo: dInfra}
	r.held[0].Shadow = boolp(true) // shadow on the OTHER pull request's item: not this push's
	if out, err := r.pushAsInstance("refs/heads/" + dBranch); err != nil || out == nil {
		t.Fatalf("a push to #31's branch while holding #32's fix item: err = %v, want admitted (the existing rule)", err)
	}
	if len(r.lines) != 0 {
		t.Fatalf("recorded %v for another pull request's item", r.lines)
	}
}

func TestAFixPushToAPullRequestOfAnotherAppIsRefused(t *testing.T) {
	r := newFixRig()
	pr := r.infra.prs[dPR]
	pr.Author = "other-officer[bot]"
	r.infra.prs[dPR] = pr
	out, err := r.pushAsInstance("refs/heads/" + dBranch)
	if out != nil || !errors.Is(err, connectors.ErrGateRefused) || !strings.Contains(err.Error(), "is authored by other-officer[bot], not this officer's App") {
		t.Fatalf("a fix push to another App's pull request: err = %v", err)
	}
	pr.Author, pr.State = BotLogin(r.infra.slug), "closed"
	r.infra.prs[dPR] = pr
	if _, err := r.pushAsInstance("refs/heads/" + dBranch); err == nil || !strings.Contains(err.Error(), "a fix goes to an open pull request") {
		t.Fatalf("a fix push to a closed pull request: err = %v", err)
	}
}

func TestAFixPushInShadowIsRefusedBeforeGitHub(t *testing.T) {
	for name, set := range map[string]func(r *fixRig){
		"item shadow":                 func(r *fixRig) { r.held[0].Shadow = boolp(true) },
		"item absent, officer shadow": func(r *fixRig) { r.held[0].Shadow = nil; r.officerShadow = true },
		"item absent, the row unreadable": func(r *fixRig) {
			r.held[0].Shadow = nil
			r.g.OfficerShadow = func(context.Context) (bool, error) { return false, errors.New("fake 503") }
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newFixRig()
			set(r)
			out, err := r.pushAsInstance("refs/heads/" + dBranch)
			want := "shadow: would have pushed " + oidA[:12] + ".." + oidB[:12] + " to " + dBranch
			if out != nil || err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v (forwarded %v), want %q", err, out != nil, want)
			}
			if n := ghWrites(r.infra); n != 0 {
				t.Fatalf("%d GitHub writes for a shadowed push", n)
			}
			if len(r.lines) != 1 || r.lines[0] != fixItem+": would have pushed "+oidA[:12]+".."+oidB[:12]+" to "+dBranch {
				t.Fatalf("recorded %q", r.lines)
			}
		})
	}
}

func TestHQUnreadableRefusesAFixLookup(t *testing.T) {
	r := newFixRig()
	r.heldErr = errors.New("fake HQ: 503")
	if out, err := r.pushAsInstance("refs/heads/" + dBranch); out != nil || err == nil || !strings.Contains(err.Error(), "could not read HQ's items") {
		t.Fatalf("HQ unreadable: err = %v", err)
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
