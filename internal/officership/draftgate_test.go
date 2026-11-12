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
	"time"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// THE DRAFT GATE, against the fake GitHub. Two repositories with pairwise
// distinct bases (main, trunk), prefixes (officer/, bot/) and forbidden lists,
// so a rule that read the other repository's entry cannot pass. Every value is
// synthetic; a secret-shaped value is spelled obviously fake.
// ---------------------------------------------------------------------------

const (
	dOfficer = "engineering:example-web" // slug engineering-example-web
	dInfra   = "example-org/example-infra"
	dBranch  = "bot/engineering-example-web/17-fix-oom" // infra, item 17
	dWebBr   = "officer/engineering-example-web/18-fix-ui"
	dPR      = 31
	// dInstance is the officer instance (the agent name) making the requests.
	dInstance = "officer-engineering-example-web-1"
)

// dSecret is a YAML credential line the redactor knows (internal/redact's
// "yaml password" shape), assembled so no single source line is one.
var dSecret = "password: " + "FAKEpass0000"

// dToken is a token by its well-known prefix (a Stripe live key's shape),
// which the redactor finds anywhere in a line.
var dToken = "sk_live_" + "FAKE0000example"

var dNow = time.Date(2000, 1, 5, 15, 0, 0, 0, time.UTC)

type draftRig struct {
	g          *PushGate
	web, infra *fakeGH
	notices    []officers.Notice
	creds      []connectors.Caller
	readCreds  []connectors.Caller
	// claims is HQ's view: item -> the instance holding it (ItemClaimed).
	claims map[int]string
	// kinds is HQ's item kinds (unset: an ordinary item).
	kinds map[int]string
}

func draftShipFx() *officers.Ship {
	return &officers.Ship{Stage: officers.StageDraft, Base: "main", BranchPrefix: "officer/", NoticePath: officers.DefaultNoticePath,
		NoticeChannel: "ch/example-alerts", Repos: []officers.ShipRepo{
			{Repo: fxRepo, Base: "main", BranchPrefix: "officer/", ForbiddenPaths: []string{".claude/**"}},
			{Repo: dInfra, Base: "trunk", BranchPrefix: "bot/", ForbiddenPaths: []string{"**/secrets/**", "nodes/**", ".sops.yaml"}},
		}}
}

func newDraftRig() *draftRig {
	r := &draftRig{web: newFakeGH(), infra: newFakeGH(), claims: map[int]string{17: dInstance, 18: dInstance}, kinds: map[int]string{}}
	r.g = &PushGate{Repo: fxRepo, Ship: draftShipFx(), OfficerID: dOfficer, GitHost: "github.com", APIHost: "api.github.com",
		GH: r.web, GHFor: map[string]GitHub{fxRepo: r.web, dInfra: r.infra},
		Cred: func(_ context.Context, c connectors.Caller, _, _ string) (string, bool, error) {
			r.creds = append(r.creds, c)
			return "officer-app", false, nil
		},
		ReadCred: func(_ context.Context, c connectors.Caller, _, _ string) (string, bool, error) {
			r.readCreds = append(r.readCreds, c)
			return "deployment-read", false, nil
		},
		Announce: func(_ context.Context, n officers.Notice) error { r.notices = append(r.notices, n); return nil },
		Now:      func() time.Time { return dNow },
		ItemClaimed: func(_ context.Context, instance string, item int) (officers.Item, error) {
			if r.claims[item] != instance {
				return officers.Item{}, fmt.Errorf("fake HQ: item %d is not claimed by %s", item, instance)
			}
			return officers.Item{ID: officers.ItemID(itoa(item)), Kind: r.kinds[item]}, nil
		}}
	// A clean comparison for the infra branch, and the infra pull request #31.
	r.infra.compares["trunk..."+dBranch] = Compare{Status: "ahead", AheadBy: 1, Commits: []string{fxHead},
		Files: []File{{Path: "clusters/prod/apps/x/deploy.yaml", Status: "modified", Changes: 2,
			Patch: "@@ -1,2 +1,2 @@\n-  memory: 1Gi\n+  memory: 2Gi"}}}
	r.infra.prs[dPR] = PR{Number: dPR, State: "open", Draft: true, Author: BotLogin(r.infra.slug), AuthorType: "Bot", HeadRef: dBranch,
		HeadSHA: fxHead, HeadRepo: dInfra, BaseRef: "trunk", BaseRepo: dInfra}
	r.infra.compares["trunk..."+fxHead] = r.infra.compares["trunk..."+dBranch]
	r.infra.branches[dBranch] = fxHead
	return r
}

func goodBody(item int) string {
	return officers.BodyFirstLine(dOfficer, item) + "\n\n## Root cause\nThe limit is low.\n\n## Evidence\naudit 51, audit 52\n\n" +
		"## Test\nkustomize build\n\n## Rollback\nrevert this commit\n"
}

func goodCreate() map[string]any {
	return map[string]any{"title": "Raise the limit", "head": dBranch, "base": "trunk", "draft": true, "body": goodBody(17)}
}

func apiWrite(g *PushGate, method, rest string, body map[string]any) error {
	_, err := apiAdmit(g, method, rest, body)
	return err
}

// apiAdmit is Admit of a REST write by dInstance (the caller in the request's
// context, as connectors.Proxy.Serve puts it), returning the admitted request.
func apiAdmit(g *PushGate, method, rest string, body map[string]any) (*http.Request, error) {
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(method, "/connect/github/api.github.com/"+rest, strings.NewReader(string(raw)))
	r = r.WithContext(connectors.WithCaller(r.Context(), connectors.Caller{AgentName: dInstance}))
	return g.Admit(r, "api.github.com", rest)
}

// create is a POST pulls; an admitted one's after-forward hook (when the gate
// attached one) is run as a request the proxy never forwarded, which is what
// releases the officer's create lock.
func (r *draftRig) create(body map[string]any) error {
	out, err := apiAdmit(r.g, http.MethodPost, "repos/"+dInfra+"/pulls", body)
	if err == nil {
		if after := connectors.AfterForwardOf(out); after != nil {
			after(context.Background(), connectors.ForwardResult{})
		}
	}
	return err
}

func (r *draftRig) patch(body map[string]any) error {
	return apiWrite(r.g, http.MethodPatch, "repos/"+dInfra+"/pulls/31", body)
}

func isConflict(err error) bool { return errors.Is(err, connectors.ErrGateConflict) }

// TestADraftOfficerOpensOnlyDraftPullRequests: no draft field, or false, is
// refused; the same request with "draft": true is admitted (control).
func TestADraftOfficerOpensOnlyDraftPullRequests(t *testing.T) {
	r := newDraftRig()
	for name, edit := range map[string]func(m map[string]any){
		"no draft field": func(m map[string]any) { delete(m, "draft") },
		"draft false":    func(m map[string]any) { m["draft"] = false },
		"draft a string": func(m map[string]any) { m["draft"] = "true" },
	} {
		m := goodCreate()
		edit(m)
		if err := r.create(m); !isGateRefusal(err) || !strings.Contains(err.Error(), "not a draft") {
			t.Errorf("%s: err = %v, want the not-a-draft refusal", name, err)
		}
	}
	if err := r.create(goodCreate()); err != nil {
		t.Fatalf("control: a draft pull request was refused: %v", err)
	}
	if len(r.notices) != 0 {
		t.Fatalf("a clean pull request posted %d notice(s)", len(r.notices))
	}
}

// TestADraftOfficersPatchCannotUndraftOrReopen: `draft` (either value) and
// state "open" are refused before any read; closing is admitted without a
// comparison; a title edit is admitted after one.
func TestADraftOfficersPatchCannotUndraftOrReopen(t *testing.T) {
	r := newDraftRig()
	for name, body := range map[string]map[string]any{
		"draft false":                      {"draft": false},
		"draft true":                       {"draft": true},
		"reopen":                           {"state": "open"},
		"reopen, Upper-case, with a title": {"state": "Open", "title": "x"},
	} {
		if err := r.patch(body); !isGateRefusal(err) || isConflict(err) {
			t.Errorf("%s: err = %v, want a 403 refusal", name, err)
		}
	}
	delete(r.infra.compares, "trunk..."+fxHead)
	if err := r.patch(map[string]any{"state": "closed"}); err != nil {
		t.Fatalf("closing its own draft: %v", err)
	}
	if err := r.patch(map[string]any{"title": "Raise the limit to 2Gi"}); err == nil || isGateRefusal(err) {
		t.Fatalf("a title edit with no readable comparison: err = %v, want a plain error (the policy could not decide)", err)
	}
	r.infra.compares["trunk..."+fxHead] = r.infra.compares["trunk..."+dBranch]
	if err := r.patch(map[string]any{"title": "Raise the limit to 2Gi"}); err != nil {
		t.Fatalf("control: a title edit with a clean comparison: %v", err)
	}
}

// TestTheForbiddenPathsBlockAtCreateAndAtEveryPatch: a changed, added,
// removed or renamed-from path under the repository's forbidden globs is a
// 409 and an HQ notice naming the path; another repository's globs do not
// apply (control: .claude/ is forbidden in example-web only).
func TestTheForbiddenPathsBlockAtCreateAndAtEveryPatch(t *testing.T) {
	for name, f := range map[string]File{
		"changed":      {Path: "clusters/prod/apps/db/secrets/creds.yaml", Status: "modified", Changes: 1, Patch: "@@ -1 +1 @@\n-a: 1\n+a: 2"},
		"added":        {Path: "nodes/cp-1/patch.yaml", Status: "added", Changes: 1, Patch: "@@ -0,0 +1 @@\n+a: 1"},
		"removed":      {Path: ".sops.yaml", Status: "removed", Changes: 1, Patch: "@@ -1 +0,0 @@\n-creation_rules: []"},
		"renamed from": {Path: "docs/old.yaml", Previous: "nodes/cp-1/old.yaml", Status: "renamed", Changes: 2, Patch: "@@ -1 +1 @@\n-a: 1\n+a: 2"},
	} {
		for _, at := range []string{"create", "patch"} {
			r := newDraftRig()
			cmp := r.infra.compares["trunk..."+dBranch]
			cmp.Files = append(append([]File(nil), cmp.Files...), f)
			r.infra.compares["trunk..."+dBranch], r.infra.compares["trunk..."+fxHead] = cmp, cmp
			var err error
			wantPR := 0
			if at == "create" {
				err = r.create(goodCreate())
			} else {
				err, wantPR = r.patch(map[string]any{"body": goodBody(17)}), dPR
			}
			if !isConflict(err) || !strings.Contains(err.Error(), "a forbidden path") {
				t.Errorf("%s at %s: err = %v, want a 409 forbidden-path refusal", name, at, err)
				continue
			}
			bad := f.Path
			if f.Previous != "" {
				bad = f.Previous
			}
			if len(r.notices) != 1 || r.notices[0].PR != wantPR || r.notices[0].Channel != "ch/example-alerts" ||
				len(r.notices[0].Paths) != 1 || r.notices[0].Paths[0] != bad || !strings.Contains(r.notices[0].Summary, dOfficer) {
				t.Errorf("%s at %s: notices %+v, want one naming %s", name, at, r.notices, bad)
			}
		}
	}
	// Control: example-web's own glob does not reach example-infra.
	r := newDraftRig()
	cmp := r.infra.compares["trunk..."+dBranch]
	cmp.Files = append(cmp.Files, File{Path: ".claude/skills/x/SKILL.md", Status: "modified", Changes: 1, Patch: "@@ -1 +1 @@\n-a\n+b"})
	r.infra.compares["trunk..."+dBranch] = cmp
	if err := r.create(goodCreate()); err != nil {
		t.Fatalf("control: another repository's forbidden glob refused this one: %v", err)
	}
}

// TestARemovedVolumeDatabaseOrCanaryManifestIsRefused: each kind in a REMOVED
// line is a 409; the same line ADDED, or another kind removed, is admitted.
func TestARemovedVolumeDatabaseOrCanaryManifestIsRefused(t *testing.T) {
	try := func(patch string) error {
		r := newDraftRig()
		cmp := r.infra.compares["trunk..."+dBranch]
		cmp.Files = []File{{Path: "clusters/prod/apps/x/thing.yaml", Status: "modified", Changes: 3, Patch: patch}}
		r.infra.compares["trunk..."+dBranch] = cmp
		return r.create(goodCreate())
	}
	for _, line := range []string{"kind: PersistentVolumeClaim", "kind: PersistentVolume", "apiVersion: postgresql.cnpg.io/v1",
		"kind: Canary", `  "kind": "Canary",`, "kind: 'PersistentVolume'"} {
		if err := try("@@ -1,3 +1,1 @@\n apiVersion: v1\n-" + line + "\n-metadata: {}"); !isConflict(err) || !strings.Contains(err.Error(), "removes a") {
			t.Errorf("removing %q: err = %v, want a 409", line, err)
		}
		if err := try("@@ -1,1 +1,3 @@\n apiVersion: v1\n+" + line + "\n+metadata: {}"); err != nil {
			t.Errorf("control: adding %q was refused: %v", line, err)
		}
	}
	for _, line := range []string{"kind: ConfigMap", "kind: CanaryAnalysis", "# the kind: Deployment"} {
		if err := try("@@ -1,2 +1,1 @@\n-" + line + "\n data: {}"); err != nil {
			t.Errorf("control: removing %q was refused: %v", line, err)
		}
	}
}

// TestASecretShapeInTheDiffOrBodyIsRefused: an ADDED line, the body or the
// title with a credential shape is a 409 with a notice that does not quote
// it; the same line REMOVED is admitted (control), as is a comment without
// one. A comment with one is refused.
func TestASecretShapeInTheDiffOrBodyIsRefused(t *testing.T) {
	withPatch := func(r *draftRig, patch string) {
		cmp := r.infra.compares["trunk..."+dBranch]
		cmp.Files = []File{{Path: "clusters/prod/apps/x/values.yaml", Status: "modified", Changes: 2, Patch: patch}}
		r.infra.compares["trunk..."+dBranch] = cmp
	}
	r := newDraftRig()
	withPatch(r, "@@ -1 +1,2 @@\n db:\n+  "+dSecret)
	if err := r.create(goodCreate()); !isConflict(err) || !strings.Contains(err.Error(), "credential-shaped") {
		t.Fatalf("an added secret line: err = %v, want a 409", err)
	}
	if len(r.notices) != 1 || strings.Contains(r.notices[0].Summary, "FAKEpass") || strings.Contains(strings.Join(r.notices[0].Paths, " "), "FAKEpass") {
		t.Fatalf("notices %+v: want one that does not quote the value", r.notices)
	}
	r = newDraftRig()
	withPatch(r, "@@ -1,2 +1 @@\n db:\n-  "+dSecret)
	if err := r.create(goodCreate()); err != nil {
		t.Fatalf("control: removing a secret line was refused: %v", err)
	}
	for name, edit := range map[string]func(m map[string]any){
		"body":  func(m map[string]any) { m["body"] = goodBody(17) + "\nconfig:\n  " + dSecret + "\n" },
		"title": func(m map[string]any) { m["title"] = "set " + dToken },
	} {
		r = newDraftRig()
		m := goodCreate()
		edit(m)
		if err := r.create(m); !isConflict(err) || len(r.notices) != 1 {
			t.Errorf("a secret in the %s: err = %v, notices %d; want a 409 and a notice", name, err, len(r.notices))
		}
	}
	r = newDraftRig()
	if err := r.patch(map[string]any{"body": goodBody(17) + "\n" + dSecret + "\n"}); !isConflict(err) {
		t.Errorf("a secret in a PATCHed body: err = %v, want a 409", err)
	}
	if err := apiWrite(r.g, http.MethodPost, "repos/"+dInfra+"/issues/31/comments", map[string]any{"body": "use " + dToken}); !isConflict(err) {
		t.Errorf("a secret in a comment: err = %v, want a 409", err)
	}
	if err := apiWrite(r.g, http.MethodPost, "repos/"+dInfra+"/issues/31/comments", map[string]any{"body": "CI is green"}); err != nil {
		t.Errorf("control: a plain comment: %v", err)
	}
}

// TestAWithheldDiffIsRefused: GitHub omits the patch of a very large file
// (changes > 0, no patch); such a file cannot be checked, so it is a 409.
// The binary shape (changes 0) is TestABinaryFileIsRefusedAsUnverifiable.
func TestAWithheldDiffIsRefused(t *testing.T) {
	r := newDraftRig()
	r.withFiles(File{Path: "tools/big.yaml", Status: "added", Changes: 40000})
	if err := r.create(goodCreate()); !isConflict(err) || !strings.Contains(err.Error(), "withholds its diff") {
		t.Fatalf("a withheld diff: err = %v, want a 409", err)
	}
}

// TestTheBodyContract: the first line exactly (this officer, the BRANCH's
// item) and the four headings each on a line; refused 403 otherwise.
func TestTheBodyContract(t *testing.T) {
	for name, body := range map[string]string{
		"empty":              "",
		"no first line":      strings.SplitN(goodBody(17), "\n", 2)[1],
		"another item":       goodBody(18),
		"another officer":    strings.Replace(goodBody(17), dOfficer, "engineering:other", 1),
		"a section short":    strings.Replace(goodBody(17), "## Rollback\n", "Rollback:\n", 1),
		"a heading inline":   strings.Replace(goodBody(17), "## Evidence\n", "see ## Evidence\n", 1),
		"no untrusted label": strings.Replace(goodBody(17), "authored from untrusted inputs; review as such", "trust me", 1),
	} {
		r := newDraftRig()
		m := goodCreate()
		m["body"] = body
		if err := r.create(m); !isGateRefusal(err) || isConflict(err) {
			t.Errorf("%s: err = %v, want a 403 body-contract refusal", name, err)
		}
		if err := r.patch(map[string]any{"body": body}); !isGateRefusal(err) || isConflict(err) {
			t.Errorf("%s (PATCH): err = %v, want a 403 body-contract refusal", name, err)
		}
	}
	r := newDraftRig()
	m := goodCreate()
	m["body"] = strings.ReplaceAll(goodBody(17), "\n", "\r\n")
	if err := r.create(m); err != nil {
		t.Fatalf("control: a CRLF body that keeps the contract: %v", err)
	}
}

// TestTheRateLimits: two open in this repository, three opened today across
// the officer's repositories, or one already for the item (any state) is a
// refusal. Pull requests by another author, of another shape, or opened
// before today's UTC midnight do not count toward what they must not
// (controls).
func TestTheRateLimits(t *testing.T) {
	own := func(r *draftRig, gh *fakeGH, prefix string, item int, state string, created time.Time) PR {
		return PR{Number: 100 + item, State: state, Author: BotLogin(gh.slug), AuthorType: "Bot",
			HeadRef: prefix + "engineering-example-web/" + itoa(item) + "-x", CreatedAt: created, Body: goodBody(item)}
	}
	yesterday := dNow.Add(-16 * time.Hour) // 23:00 the day before
	today := dNow.Add(-time.Hour)
	for name, c := range map[string]struct {
		setup func(r *draftRig)
		want  string
	}{
		"two open here": {func(r *draftRig) {
			r.infra.all = []PR{own(r, r.infra, "bot/", 1, "open", yesterday), own(r, r.infra, "bot/", 2, "open", yesterday)}
		}, "are open in " + dInfra},
		"three today across repos": {func(r *draftRig) {
			r.web.all = []PR{own(r, r.web, "officer/", 3, "closed", today), own(r, r.web, "officer/", 4, "closed", today)}
			r.infra.all = []PR{own(r, r.infra, "bot/", 5, "closed", today)}
		}, "opened 3 pull request(s) today"},
		"the item already has one (closed, other repo)": {func(r *draftRig) {
			r.web.all = []PR{own(r, r.web, "officer/", 17, "closed", yesterday.Add(-72*time.Hour))}
		}, "item 17 already has 1"},
	} {
		r := newDraftRig()
		c.setup(r)
		if err := r.create(goodCreate()); !isGateRefusal(err) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	// Controls: open elsewhere, someone else's, yesterday's, another shape.
	r := newDraftRig()
	other := PR{Number: 7, State: "open", Author: "someone[bot]", AuthorType: "Bot", HeadRef: dBranch, CreatedAt: today}
	human := own(r, r.infra, "bot/", 17, "open", today)
	human.Author, human.AuthorType = BotLogin(r.infra.slug), "User"
	r.infra.all = []PR{other, other, other, human, own(r, r.infra, "bot/", 1, "open", yesterday),
		{Number: 8, State: "open", Author: BotLogin(r.infra.slug), AuthorType: "Bot", HeadRef: "bot/engineering-other/17-x", CreatedAt: today}}
	r.infra.prs[8] = r.infra.all[len(r.infra.all)-1] // its body names no item: its branch is read
	r.web.all = []PR{own(r, r.web, "officer/", 2, "open", yesterday), own(r, r.web, "officer/", 3, "open", yesterday),
		own(r, r.web, "officer/", 4, "closed", today), own(r, r.web, "officer/", 5, "closed", today)}
	if err := r.create(goodCreate()); err != nil {
		t.Fatalf("control: within every limit: %v", err)
	}
	if r.web.listCalls != 1 || r.infra.listCalls != 1 {
		t.Fatalf("listed web %d, infra %d times; want each repository read once", r.web.listCalls, r.infra.listCalls)
	}
	// An own pull request whose body no longer names an item (a human edited
	// it) is counted by its branch, read from the pull request itself.
	r = newDraftRig()
	edited := own(r, r.web, "officer/", 17, "closed", yesterday.Add(-72*time.Hour))
	edited.Body = "rewritten by a human"
	r.web.all = []PR{edited}
	r.web.prs[edited.Number] = edited
	if err := r.create(goodCreate()); !isGateRefusal(err) || !strings.Contains(err.Error(), "item 17 already has 1") {
		t.Fatalf("an item's pull request with an edited body: err = %v, want the per-item refusal", err)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// TestADraftOfficersBranchesAndPushes: only <prefix><slug>/<item>-<slug> of
// each repository (with ITS prefix) is pushed; a branch deletion is refused;
// a pull request head of another shape is refused.
func TestADraftOfficersBranchesAndPushes(t *testing.T) {
	r := newDraftRig()
	pushTo := func(repo, ref, newOID string) error {
		body := pkt(oidA+" "+newOID+" "+ref+"\x00report-status\n") + "0000"
		rq := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
		_, err := r.g.Admit(rq, "github.com", repo+".git/git-receive-pack")
		return err
	}
	for _, c := range []struct {
		repo, ref, new string
		ok             bool
	}{
		{fxRepo, "refs/heads/" + dWebBr, oidB, true},
		{dInfra, "refs/heads/" + dBranch, oidB, true},
		{fxRepo, "refs/heads/officer/x", oidB, false},
		{dInfra, "refs/heads/officer/engineering-example-web/17-fix", oidB, false}, // web's prefix, infra's repo
		{dInfra, "refs/heads/bot/engineering-other/17-fix", oidB, false},
		{dInfra, "refs/heads/trunk", oidB, false},
		{dInfra, "refs/heads/" + dBranch, oid0, false}, // a deletion
		{"example-org/elsewhere", "refs/heads/" + dBranch, oidB, false},
	} {
		err := pushTo(c.repo, c.ref, c.new)
		if c.ok != (err == nil) {
			t.Errorf("push %s to %s (new %s…): err = %v, want ok=%v", c.ref, c.repo, c.new[:4], err, c.ok)
		}
	}
	m := goodCreate()
	m["head"] = "bot/engineering-example-web/fix-oom"
	if err := r.create(m); !isGateRefusal(err) || !strings.Contains(err.Error(), "<item>-<slug>") {
		t.Fatalf("a pull request from a branch of another shape: %v", err)
	}
	m = goodCreate()
	m["base"] = "main" // example-web's base, not example-infra's
	if err := r.create(m); !isGateRefusal(err) {
		t.Fatalf("a pull request into the other repository's base: %v", err)
	}
}

// TestADraftOfficersCredentialIsPerRepository: a request to a ship repository
// gets the officer App for THAT repository; to a read repository the
// deployment's read-only credential; to anything else no credential.
func TestADraftOfficersCredentialIsPerRepository(t *testing.T) {
	r := newDraftRig()
	caller := connectors.Caller{AgentName: "officer-x-1", Repo: fxRepo, ReadRepos: []string{"example-org/flags-state"}, ReadOnly: true}
	for _, c := range []struct {
		host, rest, kind, repo string
	}{
		{"api.github.com", "repos/" + dInfra + "/pulls/3", "officer", dInfra},
		{"github.com", dInfra + ".git/info/refs", "officer", dInfra},
		{"api.github.com", "repos/" + fxRepo + "/pulls", "officer", fxRepo},
		{"api.github.com", "user", "officer", fxRepo},
		{"api.github.com", "repos/example-org/flags-state/contents/x", "read", "example-org/flags-state"},
		{"api.github.com", "repos/example-org/elsewhere/contents/x", "anon", ""},
	} {
		r.creds, r.readCreds = nil, nil
		_, anon, err := r.g.Credential(context.Background(), caller, c.host, c.rest)
		if err != nil {
			t.Fatalf("%s %s: %v", c.host, c.rest, err)
		}
		switch c.kind {
		case "officer":
			if len(r.creds) != 1 || r.creds[0].Repo != c.repo || r.creds[0].ReadOnly || r.creds[0].ReadRepos != nil || len(r.readCreds) != 0 {
				t.Errorf("%s: officer creds %+v, read %+v; want the officer App for %s only", c.rest, r.creds, r.readCreds, c.repo)
			}
		case "read":
			if len(r.readCreds) != 1 || r.readCreds[0].Repo != c.repo || !r.readCreds[0].ReadOnly || len(r.creds) != 0 {
				t.Errorf("%s: read creds %+v, officer %+v; want the read-only credential for %s only", c.rest, r.readCreds, r.creds, c.repo)
			}
		case "anon":
			if !anon || len(r.creds)+len(r.readCreds) != 0 {
				t.Errorf("%s: anon %v, creds %d+%d; want no credential", c.rest, anon, len(r.creds), len(r.readCreds))
			}
		}
	}
}

// TestTheOtherStagesAreUnchanged (an INVARIANT guard, not a regression test:
// the pre-v2 gate never had these rules): a release-stage gate admits a
// pull request with no draft field and no body, never lists pull requests and
// never reads the comparison.
func TestTheOtherStagesAreUnchanged(t *testing.T) {
	g, gh := testGate()
	out, err := apiAdmit(g, http.MethodPost, "repos/"+fxRepo+"/pulls", map[string]any{"title": "t", "head": "officer/x", "base": "main"})
	if err != nil {
		t.Fatalf("a release-stage pull request: %v", err)
	}
	if connectors.AfterForwardOf(out) != nil {
		t.Fatal("a release-stage pull request carries an after-create check")
	}
	if gh.listCalls+gh.byBranch != 0 {
		t.Fatalf("a release-stage gate listed pull requests %d times", gh.listCalls)
	}
}

// TestMarkReadyIsRefusedOnTheDraftStage: the gate refuses before any read,
// and converts nothing.
func TestMarkReadyIsRefusedOnTheDraftStage(t *testing.T) {
	g, gh, _ := greenMergeFixture()
	g.Ship = draftShipFx()
	gh.prs[fxPR] = PR{Number: fxPR, State: "open", Draft: true, NodeID: "PR_node"}
	_, err := g.MarkReady(context.Background(), fxPR)
	if r, ok := AsRefusal(err); !ok || r.Gate != GateStage {
		t.Fatalf("mark ready on a draft officer: %v, want the stage gate", err)
	}
	if len(gh.readied) != 0 {
		t.Fatalf("converted %v", gh.readied)
	}
}

// TestTheOrphanReportListsOnlyOldBranchesWithNoPullRequest: an old branch
// with no pull request is listed; a young one, one with a pull request (any
// state), and another officer's are not. Nothing is deleted.
func TestTheOrphanReportListsOnlyOldBranchesWithNoPullRequest(t *testing.T) {
	r := newDraftRig()
	r.infra.branches = map[string]string{
		"bot/engineering-example-web/17-old":   "s-old",
		"bot/engineering-example-web/18-young": "s-young",
		"bot/engineering-example-web/19-haspr": "s-haspr",
		"bot/engineering-other/20-x":           "s-other",
		"trunk":                                "s-trunk",
	}
	for sha, age := range map[string]time.Duration{"s-old": 8 * 24 * time.Hour, "s-young": 6 * 24 * time.Hour, "s-haspr": 30 * 24 * time.Hour,
		"s-other": 30 * 24 * time.Hour, "s-trunk": 30 * 24 * time.Hour} {
		r.infra.commitTimes[sha] = dNow.Add(-age)
	}
	r.infra.all = []PR{{Number: 4, State: "closed", HeadRef: "bot/engineering-example-web/19-haspr", HeadRepo: dInfra}}
	r.infra.listed = nil // 🔴 not in any newest-N window: found by its branch
	r.web.branches = map[string]string{}
	got, err := OrphanBranches(context.Background(), dOfficer, fxRepo, r.g.Ship, r.g.ghFor, dNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Repo != dInfra || got[0].Branch != "bot/engineering-example-web/17-old" || got[0].SHA != "s-old" {
		t.Fatalf("orphans = %+v, want only bot/engineering-example-web/17-old", got)
	}
	n := OrphanNotice(dOfficer, "ch/example-alerts", got)
	if len(n.Paths) != 1 || n.Paths[0] != dInfra+":bot/engineering-example-web/17-old" || !strings.Contains(n.Summary, "never deletes") || n.Channel != "ch/example-alerts" {
		t.Fatalf("notice %+v", n)
	}
}
