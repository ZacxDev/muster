package officership

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/connectors"
)

// ---------------------------------------------------------------------------
// THE PUSH RE-CHECK: a push to a branch that already has an open pull request
// changes that pull request's diff without any PATCH. After the upstream
// accepts such a push, the draft gate reads the comparison again (base...the
// PUSHED commit) through the same check create and PATCH use, and on a hit
// CLOSES the pull request and posts an HQ notice naming paths only.
// ---------------------------------------------------------------------------

// pushRig is the draft rig with example-infra's #31 listed open on dBranch.
func pushRig() *draftRig {
	r := newDraftRig()
	r.infra.listed = []PR{r.infra.prs[dPR]}
	r.infra.compares["trunk..."+oidB] = r.infra.compares["trunk..."+dBranch]
	return r
}

// pushedFiles sets the comparison of the pushed commit oidB.
func (r *draftRig) pushedFiles(files ...File) {
	cmp := r.infra.compares["trunk..."+dBranch]
	cmp.Files, cmp.Commits = files, []string{oidB}
	r.infra.compares["trunk..."+oidB] = cmp
}

// push admits a push of ref to oidB in repo and returns the hook the gate
// attached for after the upstream's answer (nil: none).
func (r *draftRig) push(t *testing.T, repo, ref string) func(context.Context, int) {
	t.Helper()
	body := pkt(oidA+" "+oidB+" "+ref+"\x00report-status\n") + "0000"
	rq := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	out, err := r.g.Admit(rq, "github.com", repo+".git/git-receive-pack")
	if err != nil {
		t.Fatalf("push %s: %v", ref, err)
	}
	return connectors.AfterForwardOf(out)
}

// mustPush is push, failing the test when no hook was attached.
func (r *draftRig) mustPush(t *testing.T, repo, ref string) func(context.Context, int) {
	t.Helper()
	after := r.push(t, repo, ref)
	if after == nil {
		t.Fatalf("the gate attached no after-push check to a push of %s", ref)
	}
	return after
}

// TestAPushIntoAnOpenPullRequestIsRecheckedAndClosedOnAHit: each content
// rule (an inline forbidden glob, a glob from the forbidden-path file at the
// target branch, a removed Canary, a secret value in an added line, a
// withheld diff) closes #31 after the push and posts ONE notice naming the
// path, the pull request and the pushed commit, never the content. The push
// itself was admitted: the pack cannot be diffed before GitHub has it.
func TestAPushIntoAnOpenPullRequestIsRecheckedAndClosedOnAHit(t *testing.T) {
	secret := "password: " + "Zq7mR2vK9xL4pT8wFAKE"
	for name, c := range map[string]struct {
		file File
		list string // the forbidden-path file at trunk, when set
		says string
	}{
		"inline forbidden": {file: modified(".sops.yaml"), says: "a forbidden path"},
		"listed forbidden": {file: modified("overlays/site-a/read-role/patch.yaml"), list: "overlays/*/read-role/**\n", says: "a forbidden path"},
		"removed canary":   {file: File{Path: "apps/x/canary.yaml", Status: "modified", Changes: 1, Patch: "@@ -1 +0,0 @@\n-kind: Canary"}, says: "removes a"},
		"added secret":     {file: File{Path: "apps/x/values.yaml", Status: "modified", Changes: 1, Patch: "@@ -0,0 +1 @@\n+" + secret}, says: "credential-shaped"},
		"withheld diff":    {file: File{Path: "tools/blob.bin", Status: "added", Changes: 40}, says: "withholds its diff"},
	} {
		r := pushRig()
		if c.list != "" {
			r.g.Ship.Repos[1].ForbiddenPathsFile = dListFile
			r.infra.contents[dListFile+"@trunk"] = c.list
		}
		r.pushedFiles(c.file)
		after := r.push(t, dInfra, "refs/heads/"+dBranch)
		if after == nil {
			t.Fatalf("%s: the gate attached no after-push check to a push into an open pull request's branch", name)
		}
		after(context.Background(), http.StatusOK)
		if len(r.infra.closed) != 1 || r.infra.closed[0] != dPR {
			t.Errorf("%s: closed %v, want [%d]", name, r.infra.closed, dPR)
		}
		if len(r.notices) != 1 {
			t.Errorf("%s: %d notices, want 1", name, len(r.notices))
			continue
		}
		n := r.notices[0]
		if n.PR != dPR || n.HeadSHA != oidB || len(n.Paths) != 1 || n.Paths[0] != c.file.Path || n.Channel != "ch/example-alerts" ||
			!strings.Contains(n.Summary, "closed pull request #31") || !strings.Contains(n.Summary, c.says) {
			t.Errorf("%s: notice %+v, want #31 at %s naming %s and %q", name, n, oidB[:7], c.file.Path, c.says)
		}
		if strings.Contains(n.Summary, "Zq7mR2vK") {
			t.Errorf("%s: the notice quotes the secret: %s", name, n.Summary)
		}
	}
}

// TestAPushRecheckClosesNothingItMustNot: a clean comparison, a push the
// upstream refused (no read at all), a branch whose only pull request is
// closed, and another branch's open pull request are each left alone.
func TestAPushRecheckClosesNothingItMustNot(t *testing.T) {
	r := pushRig()
	r.pushedFiles(modified("clusters/prod/apps/x/deploy.yaml"))
	r.mustPush(t, dInfra, "refs/heads/"+dBranch)(context.Background(), http.StatusOK)
	if len(r.infra.closed)+len(r.notices) != 0 {
		t.Fatalf("a clean push closed %v and posted %d notice(s)", r.infra.closed, len(r.notices))
	}

	r = pushRig()
	r.pushedFiles(modified(".sops.yaml"))
	r.mustPush(t, dInfra, "refs/heads/"+dBranch)(context.Background(), http.StatusForbidden)
	if r.infra.listCalls != 0 || len(r.infra.closed) != 0 {
		t.Fatalf("a push the upstream refused: listed %d times, closed %v; want neither", r.infra.listCalls, r.infra.closed)
	}

	r = pushRig()
	r.pushedFiles(modified(".sops.yaml"))
	closed := r.infra.prs[dPR]
	closed.State = "closed"
	other := r.infra.prs[dPR]
	other.Number, other.HeadRef = 32, "bot/engineering-example-web/19-other"
	r.infra.listed = []PR{closed, other}
	r.mustPush(t, dInfra, "refs/heads/"+dBranch)(context.Background(), http.StatusOK)
	if len(r.infra.closed)+len(r.notices) != 0 {
		t.Fatalf("closed %v, notices %d; want nothing (no OPEN pull request on the pushed branch)", r.infra.closed, len(r.notices))
	}
}

// TestAPushRecheckFailsClosed: a comparison that cannot be read closes the
// pull request too (the wall could not be checked), with a notice that says so.
func TestAPushRecheckFailsClosed(t *testing.T) {
	r := pushRig()
	delete(r.infra.compares, "trunk..."+oidB)
	r.mustPush(t, dInfra, "refs/heads/"+dBranch)(context.Background(), http.StatusOK)
	if len(r.infra.closed) != 1 || len(r.notices) != 1 || !strings.Contains(r.notices[0].Summary, "could not compare") {
		t.Fatalf("an unreadable comparison: closed %v, notices %+v; want #31 closed and a notice", r.infra.closed, r.notices)
	}
	// A close GitHub refuses is still announced, saying it did not close.
	r = pushRig()
	r.pushedFiles(modified(".sops.yaml"))
	r.infra.closeErr = errors.New("fake: 502")
	r.mustPush(t, dInfra, "refs/heads/"+dBranch)(context.Background(), http.StatusOK)
	if len(r.notices) != 1 || !strings.Contains(r.notices[0].Summary, "could NOT close") {
		t.Fatalf("a failed close: notices %+v; want one saying it could not close", r.notices)
	}
}

// TestAClosedPullRequestCannotBeReopenedByTheOfficer (an INVARIANT guard: the
// draft gate refused a reopen before the push re-check existed): after the
// re-check closes #31, the officer's PATCH state "open" is refused before any
// read.
func TestAClosedPullRequestCannotBeReopenedByTheOfficer(t *testing.T) {
	r := pushRig()
	r.pushedFiles(modified(".sops.yaml"))
	if after := r.push(t, dInfra, "refs/heads/"+dBranch); after != nil {
		after(context.Background(), http.StatusOK)
	}
	if err := r.patch(map[string]any{"state": "open"}); !isGateRefusal(err) || !strings.Contains(err.Error(), "never reopens") {
		t.Fatalf("a reopen: err = %v, want the refusal", err)
	}
}

// TestOtherStagesAttachNoPushRecheck (an INVARIANT guard): a release-stage
// gate's push carries no after-push check.
func TestOtherStagesAttachNoPushRecheck(t *testing.T) {
	g, _ := testGate()
	body := pkt(oidA+" "+oidB+" refs/heads/officer/x\x00report-status\n") + "0000"
	out, err := g.Admit(httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body)), "github.com", fxRepo+".git/git-receive-pack")
	if err != nil {
		t.Fatal(err)
	}
	if connectors.AfterForwardOf(out) != nil {
		t.Fatal("a release-stage push carries an after-push check")
	}
}

// TestOrdinaryManifestKeysPassTheDraftGate: added manifest lines that NAME a
// credential and hold a boolean or a reference are admitted, in the diff and
// in the body; dSecret beside them is still refused (control).
func TestOrdinaryManifestKeysPassTheDraftGate(t *testing.T) {
	lines := []string{"automountServiceAccountToken: false", "existingSecret: x", "passwordSecret: x", "apiKey: ${API_KEY}"}
	r := newDraftRig()
	r.withFiles(File{Path: "clusters/prod/apps/x/sa.yaml", Status: "modified", Changes: len(lines),
		Patch: "@@ -0,0 +1,4 @@\n+" + strings.Join(lines, "\n+")})
	m := goodCreate()
	m["body"] = goodBody(17) + "\nThe service account sets `" + lines[0] + "`.\n"
	if err := r.create(m); err != nil {
		t.Fatalf("ordinary manifest keys were refused: %v", err)
	}
	r = newDraftRig()
	r.withFiles(File{Path: "clusters/prod/apps/x/sa.yaml", Status: "modified", Changes: 2,
		Patch: "@@ -0,0 +1,2 @@\n+" + lines[0] + "\n+" + dSecret})
	if err := r.create(goodCreate()); !isConflict(err) {
		t.Fatalf("control: a secret value beside them: err = %v, want a 409", err)
	}
}
