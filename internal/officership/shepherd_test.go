package officership

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// THE SHEPHERD GATE, against the fake GitHub. One repository (base trunk,
// prefix officer/), one pull request #57 by the authoring officer's bot on
// its officer/ branch, head oidA; a shepherd push moves it to oidB. The
// author's wall (nodes/**, plus a list file at trunk naming
// clusters/prod/identity/**) is the rule's, so a wall read from anywhere else
// cannot pass. Every value is synthetic and pairwise distinct.
// ---------------------------------------------------------------------------

const (
	sOfficer       = "shepherd:example-infra"
	sRepo          = "example-org/example-infra"
	sAuthorOfficer = "investigate:example-infra"
	sAuthorLogin   = "example-investigator[bot]"
	sBranch        = "officer/investigate-example-infra/17-fix-oom"
	sPR            = 57
	sInstance      = "officer-shepherd-example-infra-1"
	sNode          = "PR_kwDOshepherd57node"
	sListFile      = "scripts/ci/officer-forbidden.txt"
)

type shepRig struct {
	g       *PushGate
	gates   *ShepherdGates
	gh      *fakeGH
	notices []officers.Notice
	// held is HQ's view: subject -> the instance holding it.
	held map[string]string
	// authorLogin is what the author officer's App reads as; authorErr fails it.
	authorLogin string
	authorErr   error
}

func newShepRig() *shepRig {
	r := &shepRig{gh: newFakeGH(), held: map[string]string{officers.ShepherdSubject(sRepo, sPR): sInstance}, authorLogin: sAuthorLogin}
	ship := &officers.Ship{Stage: officers.StageShepherd, Base: "trunk", BranchPrefix: "officer/", NoticeChannel: "ch/example-alerts",
		RequiredChecks: []string{"ci / gate"},
		Repos:          []officers.ShipRepo{{Repo: sRepo, Base: "trunk", BranchPrefix: "officer/"}},
		Shepherd: &officers.Shepherd{Authors: []officers.ShepherdAuthor{{Login: sAuthorLogin, Officer: sAuthorOfficer},
			{Login: "ship-officer[bot]"}, {Login: "example-human"}}}}
	rules := []officers.ShepherdRule{
		{Repo: sRepo, Login: sAuthorLogin, AuthorOfficer: sAuthorOfficer, ForbiddenPaths: []string{"nodes/**"}, ForbiddenPathsFiles: []string{sListFile}},
		// The shepherd's OWN bot, listed on purpose: so the own-App check is
		// reached past the listed-author check (TestTheShepherdRule...).
		{Repo: sRepo, Login: "ship-officer[bot]", ForbiddenPaths: []string{"nodes/**"}},
		{Repo: sRepo, Login: "example-human", ForbiddenPaths: []string{"nodes/**"}},
	}
	sh := &Shepherd{OfficerID: sOfficer, Repo: sRepo, Ship: ship, Rules: rules,
		AuthorLogin: func(_ context.Context, id string) (string, error) {
			if id != sAuthorOfficer {
				return "", fmt.Errorf("fake: no officer %s", id)
			}
			return r.authorLogin, r.authorErr
		},
		ItemHeld: func(_ context.Context, instance, repo string, n int) error {
			if r.held[officers.ShepherdSubject(repo, n)] != instance {
				return fmt.Errorf("fake HQ: %s does not hold %s", instance, officers.ShepherdSubject(repo, n))
			}
			return nil
		}}
	r.g = &PushGate{Repo: sRepo, Ship: ship, OfficerID: sOfficer, GitHost: "github.com", APIHost: "api.github.com", GH: r.gh,
		GHFor: map[string]GitHub{sRepo: r.gh}, Shepherd: sh,
		Cred: func(context.Context, connectors.Caller, string, string) (string, bool, error) {
			return "shepherd-app", false, nil
		},
		Announce: func(_ context.Context, n officers.Notice) error { r.notices = append(r.notices, n); return nil }}
	r.gates = &ShepherdGates{Shepherd: sh, GHFor: func(repo string) GitHub {
		if strings.EqualFold(repo, sRepo) {
			return r.gh
		}
		return nil
	}}
	pr := PR{Number: sPR, State: "open", Draft: true, NodeID: sNode, Author: sAuthorLogin, AuthorType: "Bot", HeadRef: sBranch, HeadSHA: oidA,
		HeadRepo: sRepo, BaseRef: "trunk", BaseRepo: sRepo, Title: "Raise the limit"}
	r.gh.prs[sPR] = pr
	r.gh.all = []PR{pr}
	r.gh.branches[sBranch] = oidB // the push landed
	r.gh.compares[oidA+"..."+oidB] = Compare{Status: "ahead", AheadBy: 1, Commits: []string{oidB}}
	r.gh.compares["trunk..."+oidB] = Compare{Status: "ahead", AheadBy: 2, Commits: []string{oidA, oidB},
		Files: []File{{Path: "clusters/prod/apps/x/deploy.yaml", Status: "modified", Changes: 2, Patch: "@@ -1,2 +1,2 @@\n-  memory: 1Gi\n+  memory: 2Gi"}}}
	r.gh.contents[sListFile+"@trunk"] = "# the author's list\nclusters/prod/identity/**\n"
	r.gh.statuses[oidA] = map[string]string{"ci / gate": "success"}
	return r
}

// setPR edits the fixture pull request everywhere the fake answers it.
func (r *shepRig) setPR(f func(p *PR)) {
	p := r.gh.prs[sPR]
	f(&p)
	r.gh.prs[sPR] = p
	r.gh.all = []PR{p}
}

func asInstance(req *http.Request, instance string) *http.Request {
	return req.WithContext(connectors.WithCaller(req.Context(), connectors.Caller{AgentName: instance}))
}

// pushAs is a receive-pack of one command `old new ref` by instance.
func (r *shepRig) pushAs(instance, old, nw, ref string) (*http.Request, error) {
	body := pkt(old+" "+nw+" "+ref+"\x00report-status\n") + "0000PACK\x00\x00\x00\x02fake"
	rq := asInstance(httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body)), instance)
	return r.g.Admit(rq, "github.com", sRepo+".git/git-receive-pack")
}

// mustAfter is the re-check the gate attached to an admitted push, failing
// the test (never panicking the package) when there is none.
func mustAfter(t *testing.T, out *http.Request) func(context.Context, connectors.ForwardResult) {
	t.Helper()
	after := connectors.AfterForwardOf(out)
	if after == nil {
		t.Fatal("the shepherd's push carries no re-check")
	}
	return after
}

func (r *shepRig) push() (*http.Request, error) {
	return r.pushAs(sInstance, oidA, oidB, "refs/heads/"+sBranch)
}

// api is a REST write by instance.
func (r *shepRig) api(instance, method, rest string, body any) (*http.Request, error) {
	raw := ""
	if body != nil {
		b, _ := json.Marshal(body)
		raw = string(b)
	}
	rq := asInstance(httptest.NewRequest(method, "/x", strings.NewReader(raw)), instance)
	return r.g.Admit(rq, "api.github.com", rest)
}

func (r *shepRig) comment(body string) error {
	_, err := r.api(sInstance, http.MethodPost, fmt.Sprintf("repos/%s/issues/%d/comments", sRepo, sPR), map[string]any{"body": body})
	return err
}

func (r *shepRig) updateBranch(body any) error {
	_, err := r.api(sInstance, http.MethodPut, fmt.Sprintf("repos/%s/pulls/%d/update-branch", sRepo, sPR), body)
	return err
}

// TestAShepherdPushesAFastForwardToAPullRequestItHolds is the allow half of
// the push: admitted, re-checked after GitHub applied it (fast-forward,
// clean against the author's wall), and left alone — no revert, no notice.
func TestAShepherdPushesAFastForwardToAPullRequestItHolds(t *testing.T) {
	r := newShepRig()
	out, err := r.push()
	if err != nil {
		t.Fatalf("a fast-forward push to a held pull request was refused: %v", err)
	}
	after := connectors.AfterForwardOf(out)
	if after == nil {
		t.Fatal("the shepherd's push carries no re-check")
	}
	calls := r.gh.compareCalls
	after(context.Background(), applied("refs/heads/"+sBranch))
	if r.gh.compareCalls-calls != 2 {
		t.Fatalf("the re-check made %d comparisons, want 2 (old...new, then base...new)", r.gh.compareCalls-calls)
	}
	if len(r.gh.restored) != 0 || len(r.notices) != 0 {
		t.Fatalf("a clean fast-forward was reverted %v / announced %v", r.gh.restored, r.notices)
	}
}

// TestTheShepherdRuleRefusesEveryOtherPullRequest: each case breaks ONE input
// of the allow fixture; Admit refuses it by THAT gate, and so does every
// write path (push, comment, update-branch, mark ready), with nothing
// forwarded and nothing converted.
func TestTheShepherdRuleRefusesEveryOtherPullRequest(t *testing.T) {
	cases := []struct {
		name   string
		gate   string
		break_ func(r *shepRig)
	}{
		{"closed", GatePR, func(r *shepRig) { r.setPR(func(p *PR) { p.State = "closed" }) }},
		{"merged", GatePR, func(r *shepRig) { r.setPR(func(p *PR) { p.Merged = true }) }},
		{"into another base", GateBase, func(r *shepRig) { r.setPR(func(p *PR) { p.BaseRef = "main" }) }},
		{"into another repository", GateBase, func(r *shepRig) { r.setPR(func(p *PR) { p.BaseRepo = "someone/example-infra" }) }},
		{"a branch outside the prefix", GateHeadBranch, func(r *shepRig) { r.setPR(func(p *PR) { p.HeadRef = "feature/x" }) }},
		{"from a fork", GateHeadBranch, func(r *shepRig) { r.setPR(func(p *PR) { p.HeadRepo = "someone/example-infra" }) }},
		{"an unlisted author", GateAuthor, func(r *shepRig) { r.setPR(func(p *PR) { p.Author = "another-bot[bot]" }) }},
		{"a listed bot login typed User", GateAuthor, func(r *shepRig) { r.setPR(func(p *PR) { p.AuthorType = "User" }) }},
		{"the shepherd's own App", GateAuthor, func(r *shepRig) { r.setPR(func(p *PR) { p.Author = "ship-officer[bot]" }) }},
		{"the author officer's App is another bot", GateAuthor, func(r *shepRig) { r.authorLogin = "someone-else[bot]" }},
		{"the author officer's App cannot be read", GateAuthor, func(r *shepRig) { r.authorErr = errors.New("fake: GitHub is down") }},
		{"no item held", GateItem, func(r *shepRig) { r.held = map[string]string{} }},
		{"the item held by another instance", GateItem, func(r *shepRig) {
			r.held[officers.ShepherdSubject(sRepo, sPR)] = "officer-shepherd-example-infra-2"
		}},
		{"an item for another pull request", GateItem, func(r *shepRig) {
			r.held = map[string]string{officers.ShepherdSubject(sRepo, sPR+1): sInstance}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newShepRig()
			c.break_(r)
			_, err := r.g.Shepherd.Admit(context.Background(), r.g.Ship.Repos[0], r.gh, r.gh.prs[sPR], sInstance, func(string) {})
			if rf, ok := AsRefusal(err); !ok || rf.Gate != c.gate {
				t.Fatalf("Admit: err = %v, want a refusal by gate %q", err, c.gate)
			}
			want := "refused by gate " + c.gate
			for what, err := range map[string]error{
				"comment":       r.comment("evidence"),
				"update-branch": r.updateBranch(map[string]any{"expected_head_sha": oidA}),
			} {
				if !isGateRefusal(err) || !strings.Contains(err.Error(), want) {
					t.Errorf("%s: err = %v, want a 403 %q", what, err, want)
				}
			}
			// A push finds the pull request by its branch first, so a closed
			// one, another branch or a fork's is refused as "no open pull
			// request from this branch" before Admit; either way, a 403.
			if _, err := r.push(); !isGateRefusal(err) {
				t.Errorf("push: err = %v, want a 403", err)
			}
			if _, err := r.gates.MarkReady(context.Background(), sInstance, sRepo, sPR); err == nil {
				t.Error("mark ready was admitted")
			} else if rf, ok := AsRefusal(err); !ok || rf.Gate != c.gate {
				t.Errorf("mark ready: err = %v, want gate %q", err, c.gate)
			}
			if len(r.gh.readied) != 0 {
				t.Errorf("refused by %s and still converted %v", c.gate, r.gh.readied)
			}
		})
	}
	// Control: the instance named in the request is the one checked — the
	// right item held by the right instance passes every path.
	r := newShepRig()
	if _, err := r.push(); err != nil {
		t.Fatalf("control push: %v", err)
	}
	if err := r.comment("evidence"); err != nil {
		t.Fatalf("control comment: %v", err)
	}
}

// TestAShepherdPushMustUpdateTheCurrentHead: a create, a delete, an update
// from anything but the pull request's head, a branch that heads no open (or
// several) pull requests, a request from no instance, and refs outside the
// prefix are each refused before anything is forwarded.
func TestAShepherdPushMustUpdateTheCurrentHead(t *testing.T) {
	head := "refs/heads/" + sBranch
	for name, c := range map[string]struct {
		edit           func(r *shepRig)
		instance       string
		old, new_, ref string
		says           string
	}{
		"a create":                    {nil, sInstance, oid0, oidB, head, "creating"},
		"a delete":                    {nil, sInstance, oidA, oid0, head, "never deletes a branch"},
		"not from the head (a force)": {nil, sInstance, oidB, oidA, head, "not pull request #57's head"},
		"no open pull request": {func(r *shepRig) { r.gh.all = nil }, sInstance, oidA, oidB, head,
			"head of 0 open pull requests"},
		"two open pull requests": {func(r *shepRig) { p := r.gh.prs[sPR]; q := p; q.Number = sPR + 1; r.gh.all = []PR{p, q} }, sInstance, oidA, oidB, head,
			"head of 2 open pull requests"},
		"no instance":     {nil, "", oidA, oidB, head, "refused by gate item"},
		"the base branch": {nil, sInstance, oidA, oidB, "refs/heads/trunk", "only branches under refs/heads/officer/"},
		"a tag":           {nil, sInstance, oidA, oidB, "refs/tags/v1.0.0", "no tags"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newShepRig()
			if c.edit != nil {
				c.edit(r)
			}
			_, err := r.pushAs(c.instance, c.old, c.new_, c.ref)
			if !isGateRefusal(err) || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("err = %v, want a 403 saying %q", err, c.says)
			}
		})
	}
	// And a shepherd with no rules loaded refuses every write.
	r := newShepRig()
	r.g.Shepherd = nil
	if _, err := r.push(); !isGateRefusal(err) {
		t.Fatalf("a shepherd with no rules: push err = %v", err)
	}
	if err := r.comment("x"); !isGateRefusal(err) {
		t.Fatalf("a shepherd with no rules: comment err = %v", err)
	}
}

// TestANonFastForwardShepherdPushIsReverted: GitHub took the push, but its
// comparison old...new is not a fast-forward (or cannot be read): the branch
// is moved back to the head checked before forwarding and HQ is told.
func TestANonFastForwardShepherdPushIsReverted(t *testing.T) {
	for name, cmp := range map[string]*Compare{
		"diverged":   {Status: "diverged", AheadBy: 1, BehindBy: 1},
		"behind":     {Status: "behind", BehindBy: 1},
		"unreadable": nil,
	} {
		t.Run(name, func(t *testing.T) {
			r := newShepRig()
			out, err := r.push()
			if err != nil {
				t.Fatal(err)
			}
			if cmp == nil {
				delete(r.gh.compares, oidA+"..."+oidB)
			} else {
				r.gh.compares[oidA+"..."+oidB] = *cmp
			}
			mustAfter(t, out)(context.Background(), applied("refs/heads/"+sBranch))
			if want := sBranch + " " + oidB + "->" + oidA; len(r.gh.restored) != 1 || r.gh.restored[0] != want {
				t.Fatalf("restored %v, want [%s]", r.gh.restored, want)
			}
			if len(r.notices) != 1 || !strings.Contains(r.notices[0].Summary, "reverted a push") || r.notices[0].PR != sPR {
				t.Fatalf("notices = %+v", r.notices)
			}
		})
	}
}

// TestAShepherdPushIntoTheAuthorsWallIsReverted: a fast-forward that touches
// a glob of the author's wall, or a glob of its list file at the target
// branch, or carries a credential, is reverted with a notice naming the path;
// a path outside the wall is not (control).
func TestAShepherdPushIntoTheAuthorsWallIsReverted(t *testing.T) {
	for name, c := range map[string]struct {
		f      File
		revert bool
	}{
		"the author's glob":      {File{Path: "nodes/cp-1/patch.yaml", Status: "modified", Changes: 1, Patch: "@@ -1 +1 @@\n-a: 1\n+a: 2"}, true},
		"the author's list file": {File{Path: "clusters/prod/identity/role.yaml", Status: "modified", Changes: 1, Patch: "@@ -1 +1 @@\n-a: 1\n+a: 2"}, true},
		"the list file itself":   {File{Path: sListFile, Status: "modified", Changes: 1, Patch: "@@ -1 +1 @@\n-a\n+b"}, true},
		"a credential":           {File{Path: "clusters/prod/apps/x/cfg.yaml", Status: "modified", Changes: 1, Patch: "@@ -1 +1 @@\n-a: 1\n+" + dSecret}, true},
		"outside the wall":       {File{Path: "clusters/prod/apps/x/hpa.yaml", Status: "modified", Changes: 1, Patch: "@@ -1 +1 @@\n-max: 3\n+max: 4"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			r := newShepRig()
			cmp := r.gh.compares["trunk..."+oidB]
			cmp.Files = []File{c.f}
			r.gh.compares["trunk..."+oidB] = cmp
			out, err := r.push()
			if err != nil {
				t.Fatal(err)
			}
			mustAfter(t, out)(context.Background(), applied("refs/heads/"+sBranch))
			if !c.revert {
				if len(r.gh.restored) != 0 || len(r.notices) != 0 {
					t.Fatalf("a clean push was reverted %v / announced %v", r.gh.restored, r.notices)
				}
				return
			}
			if len(r.gh.restored) != 1 {
				t.Fatalf("restored %v, want one revert", r.gh.restored)
			}
			if len(r.notices) != 1 || !strings.Contains(strings.Join(r.notices[0].Paths, " "), c.f.Path) {
				t.Fatalf("notices = %+v, want one naming %s", r.notices, c.f.Path)
			}
			if strings.Contains(r.notices[0].Summary, "FAKEpass") {
				t.Fatal("the notice quotes the content")
			}
		})
	}
}

// TestARevertNeverMovesABranchThatMovedAgain: when the branch is no longer at
// the pushed commit, muster does not touch it, and the notice says a human
// must.
func TestARevertNeverMovesABranchThatMovedAgain(t *testing.T) {
	r := newShepRig()
	out, err := r.push()
	if err != nil {
		t.Fatal(err)
	}
	r.gh.compares[oidA+"..."+oidB] = Compare{Status: "diverged", AheadBy: 1, BehindBy: 1}
	r.gh.branches[sBranch] = fxHead // moved again after the push
	mustAfter(t, out)(context.Background(), applied("refs/heads/"+sBranch))
	if len(r.gh.restored) != 0 || r.gh.branches[sBranch] != fxHead {
		t.Fatalf("a branch that moved again was restored: %v, now %s", r.gh.restored, r.gh.branches[sBranch])
	}
	if len(r.notices) != 1 || !strings.Contains(r.notices[0].Summary, "could NOT revert") {
		t.Fatalf("notices = %+v", r.notices)
	}
	// A push GitHub did not apply is not checked at all.
	r = newShepRig()
	out, _ = r.push()
	mustAfter(t, out)(context.Background(), connectors.ForwardResult{Forwarded: true, Status: http.StatusOK,
		Body: report("unpack ok", "ng refs/heads/"+sBranch+" non-fast-forward"), Complete: true})
	if len(r.gh.restored) != 0 || len(r.notices) != 0 {
		t.Fatalf("a push GitHub refused was acted on: %v %v", r.gh.restored, r.notices)
	}
}

// TestAShepherdCommentsAndUpdatesTheBranch: a comment and an update-branch
// pinned to the current head are forwarded; a credential in a comment is a
// 409 and a notice; update-branch without the head, at a stale head or with
// another key is refused.
func TestAShepherdCommentsAndUpdatesTheBranch(t *testing.T) {
	r := newShepRig()
	if err := r.comment("Round 1 found nothing blocking."); err != nil {
		t.Fatalf("a comment was refused: %v", err)
	}
	if err := r.updateBranch(map[string]any{"expected_head_sha": oidA}); err != nil {
		t.Fatalf("update-branch at the head was refused: %v", err)
	}
	if err := r.comment("leaked " + dToken); !isConflict(err) || len(r.notices) != 1 {
		t.Fatalf("a credential in a comment: err %v, notices %d", err, len(r.notices))
	}
	for name, body := range map[string]any{
		"no body":         nil,
		"no head":         map[string]any{},
		"a stale head":    map[string]any{"expected_head_sha": oidB},
		"not a sha":       map[string]any{"expected_head_sha": "trunk"},
		"another key too": map[string]any{"expected_head_sha": oidA, "update_method": "rebase"},
	} {
		if err := r.updateBranch(body); !isGateRefusal(err) {
			t.Errorf("update-branch with %s: err = %v, want a 403", name, err)
		}
	}
}

// TestAShepherdNeverMergesClosesOrRewrites: every write outside the
// shepherd's allowlist is refused, on the very pull request it holds.
func TestAShepherdNeverMergesClosesOrRewrites(t *testing.T) {
	pr := fmt.Sprintf("repos/%s/pulls/%d", sRepo, sPR)
	for _, c := range []struct {
		name, method, rest string
		body               any
	}{
		{"merge", http.MethodPut, pr + "/merge", map[string]any{"merge_method": "squash"}},
		{"close", http.MethodPatch, pr, map[string]any{"state": "closed"}},
		{"reopen", http.MethodPatch, pr, map[string]any{"state": "open"}},
		{"edit the title", http.MethodPatch, pr, map[string]any{"title": "x"}},
		{"open a pull request", http.MethodPost, "repos/" + sRepo + "/pulls", map[string]any{"head": sBranch, "base": "trunk", "title": "x"}},
		{"force a ref", http.MethodPatch, "repos/" + sRepo + "/git/refs/heads/" + sBranch, map[string]any{"sha": oidB, "force": true}},
		{"create a ref", http.MethodPost, "repos/" + sRepo + "/git/refs", map[string]any{"ref": "refs/heads/officer/x", "sha": oidB}},
		{"delete the branch", http.MethodDelete, "repos/" + sRepo + "/git/refs/heads/" + sBranch, nil},
		{"the merges API", http.MethodPost, "repos/" + sRepo + "/merges", map[string]any{"base": "trunk", "head": sBranch}},
		{"the contents API", http.MethodPut, "repos/" + sRepo + "/contents/x.yaml", map[string]any{"message": "x", "content": "eA=="}},
		{"a review", http.MethodPost, pr + "/reviews", map[string]any{"event": "APPROVE"}},
		{"GraphQL", http.MethodPost, "graphql", map[string]any{"query": "mutation { mergePullRequest }"}},
	} {
		if _, err := newShepRig().api(sInstance, c.method, c.rest, c.body); !isGateRefusal(err) {
			t.Errorf("%s: err = %v, want a 403", c.name, err)
		}
	}
}

// TestTheShepherdMarksAHeldDraftReady: the allow half reports every gate in
// order and converts the node id read; a pull request that is not a draft, a
// repository that is not the shepherd's and red checks are refused by their
// gates, nothing converted.
func TestTheShepherdMarksAHeldDraftReady(t *testing.T) {
	r := newShepRig()
	out, err := r.gates.MarkReady(context.Background(), sInstance, "Example-Org/Example-Infra", sPR)
	if err != nil {
		t.Fatalf("a held draft was refused: %v", err)
	}
	if want := "stage pull_request base head_branch author item checks ready"; strings.Join(out.Passed, " ") != want {
		t.Fatalf("gates passed = %q, want %q", strings.Join(out.Passed, " "), want)
	}
	if len(r.gh.readied) != 1 || r.gh.readied[0] != sNode || !out.Ready {
		t.Fatalf("readied %v (%+v), want [%s]", r.gh.readied, out, sNode)
	}
	for _, c := range []struct {
		name, gate string
		break_     func(r *shepRig)
		repo       string
	}{
		{"already ready", GatePR, func(r *shepRig) { r.setPR(func(p *PR) { p.Draft = false }) }, sRepo},
		{"another repository", GateBase, func(*shepRig) {}, "example-org/example-web"},
		{"a red check", GateChecks, func(r *shepRig) { r.gh.statuses[oidA] = map[string]string{"ci / gate": "failure"} }, sRepo},
		{"a missing check", GateChecks, func(r *shepRig) { r.gh.statuses[oidA] = map[string]string{} }, sRepo},
		{"GitHub refuses", GateReady, func(r *shepRig) { r.gh.readyErr = errors.New("fake: not permitted") }, sRepo},
	} {
		r := newShepRig()
		c.break_(r)
		_, err := r.gates.MarkReady(context.Background(), sInstance, c.repo, sPR)
		if rf, ok := AsRefusal(err); !ok || rf.Gate != c.gate {
			t.Errorf("%s: err = %v, want gate %q", c.name, err, c.gate)
		}
		if c.gate != GateReady && len(r.gh.readied) != 0 {
			t.Errorf("%s: converted %v", c.name, r.gh.readied)
		}
	}
	// Not a shepherd: refused before any read.
	r = newShepRig()
	r.gates.Shepherd.Ship = &officers.Ship{Stage: officers.StageDraft}
	if _, err := r.gates.MarkReady(context.Background(), sInstance, sRepo, sPR); err == nil {
		t.Fatal("a draft officer's rules marked a pull request ready")
	}
}
