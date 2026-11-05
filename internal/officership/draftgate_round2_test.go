package officership

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/connectors"
)

// ---------------------------------------------------------------------------
// The draft gate's second round: a binary file is unverifiable (GitHub
// reports it changes 0 with no patch); the push re-check runs whatever the
// client does and finds the pull requests of a branch by the branch, not in a
// newest-N window; a new pull request is re-checked at the head GitHub
// created it on; the sweep re-checks every open one; the item must be the
// instance's claim; a closed pull request's branch is deleted; and the
// removal rule reaches $patch: delete, a renamed metadata.name and a dropped
// kustomization entry. Every value is synthetic.
// ---------------------------------------------------------------------------

// TestABinaryFileIsRefusedAsUnverifiable (R1): GitHub's REAL shape for a
// binary file is changes 0 with no patch (added, modified or renamed), so the
// file's content — a token beside one NUL byte — is never read. Each is
// refused; a removal with changes 0 (a binary or empty file, which cannot
// declare a manifest) is admitted, and a removal with changes > 0 but no
// patch (a large text file, whose removed lines cannot be read) is refused.
func TestABinaryFileIsRefusedAsUnverifiable(t *testing.T) {
	for name, f := range map[string]File{
		"added binary":    {Path: "tools/creds.txt", Status: "added"},
		"modified binary": {Path: "images/logo.png", Status: "modified"},
		"renamed, no patch (a pure rename or a binary)": {Path: "docs/new.bin", Previous: "docs/old.bin", Status: "renamed"},
		"copied, no patch": {Path: "docs/copy.bin", Previous: "docs/old.bin", Status: "copied"},
		"removed large":    {Path: "clusters/prod/apps/db/pvc.yaml", Status: "removed", Changes: 40000},
	} {
		paths, why := ForbiddenFiles(nil, []File{f})
		if len(paths) != 1 || paths[0] != f.Path || !strings.Contains(why, "cannot be checked") {
			t.Errorf("%s: ForbiddenFiles = %v %q, want %s refused as unverifiable", name, paths, why, f.Path)
		}
		r := newDraftRig()
		r.withFiles(f)
		if err := r.create(goodCreate()); !isConflict(err) {
			t.Errorf("%s: create err = %v, want a 409", name, err)
		}
	}
	for name, f := range map[string]File{
		"removed binary": {Path: "images/old.png", Status: "removed"},
		"removed empty":  {Path: "docs/.keep", Status: "removed"},
	} {
		if paths, why := ForbiddenFiles(nil, []File{f}); len(paths) != 0 {
			t.Errorf("control %s: refused %v (%s)", name, paths, why)
		}
	}
	// A removed binary on a forbidden path is still a forbidden path.
	if paths, _ := ForbiddenFiles([]string{"nodes/**"}, []File{{Path: "nodes/cp-1/blob", Status: "removed"}}); len(paths) != 1 {
		t.Errorf("a removed forbidden binary: %v, want refused", paths)
	}
}

// TestAPushRecheckRunsWhenTheClientWentAway (R2): an officer that drops its
// connection once the pack is sent leaves the proxy with no status (0) and no
// report; the push WAS forwarded, so it is checked anyway and the hit closes
// the pull request. A request the proxy never forwarded is not read.
func TestAPushRecheckRunsWhenTheClientWentAway(t *testing.T) {
	r := pushRig()
	r.pushedFiles(modified(".sops.yaml"))
	r.mustPush(t, dInfra, "refs/heads/"+dBranch)(context.Background(), connectors.ForwardResult{Forwarded: true})
	if len(r.infra.closed) != 1 || r.infra.closed[0] != dPR || len(r.notices) != 1 {
		t.Fatalf("a forwarded push with no answer: closed %v, notices %d; want #%d closed and a notice", r.infra.closed, len(r.notices), dPR)
	}
	// An abandoned push GitHub never applied: the pushed commit is unknown to
	// it, so the pull request is checked at the head it shows (clean).
	r = pushRig()
	delete(r.infra.compares, "trunk..."+oidB)
	r.infra.prs[dPR] = r.infra.all[0] // head fxHead, clean
	r.mustPush(t, dInfra, "refs/heads/"+dBranch)(context.Background(), connectors.ForwardResult{Forwarded: true})
	if len(r.infra.closed)+len(r.notices) != 0 {
		t.Fatalf("an unapplied abandoned push: closed %v, notices %+v; want nothing (its head is clean)", r.infra.closed, r.notices)
	}
	// INVARIANT guard: not forwarded, nothing read.
	r = pushRig()
	r.pushedFiles(modified(".sops.yaml"))
	r.mustPush(t, dInfra, "refs/heads/"+dBranch)(context.Background(), connectors.ForwardResult{})
	if r.infra.byBranch+r.infra.listCalls+len(r.infra.closed) != 0 {
		t.Fatalf("a push never forwarded: looked up %d+%d, closed %v", r.infra.byBranch, r.infra.listCalls, r.infra.closed)
	}
}

// TestAPushRecheckFindsAPullRequestOutsideTheNewest300 (R3): the pull
// request is not in any newest-N window (listed is empty), only in the
// repository; the push re-check finds it by its branch and closes it.
func TestAPushRecheckFindsAPullRequestOutsideTheNewest300(t *testing.T) {
	r := pushRig()
	r.infra.listed = nil
	r.pushedFiles(modified(".sops.yaml"))
	r.mustPush(t, dInfra, "refs/heads/"+dBranch)(context.Background(), applied("refs/heads/"+dBranch))
	if len(r.infra.closed) != 1 || r.infra.closed[0] != dPR {
		t.Fatalf("an aged-out pull request: closed %v, want [%d]", r.infra.closed, dPR)
	}
}

// TestAPushHitDeletesTheBranch (Y5): after closing, muster deletes the
// branch that holds the refused change and says so; an UNREADABLE comparison
// closes but deletes nothing (nothing is known to be wrong with it); a delete
// GitHub refuses is said too.
func TestAPushHitDeletesTheBranch(t *testing.T) {
	r := pushRig()
	r.pushedFiles(File{Path: "apps/x/values.yaml", Status: "modified", Changes: 1, Patch: "@@ -0,0 +1 @@\n+" + dSecret})
	r.mustPush(t, dInfra, "refs/heads/"+dBranch)(context.Background(), applied("refs/heads/"+dBranch))
	if len(r.infra.deleted) != 1 || r.infra.deleted[0] != dBranch {
		t.Fatalf("deleted %v, want [%s]", r.infra.deleted, dBranch)
	}
	if len(r.notices) != 1 || !strings.Contains(r.notices[0].Summary, "deleted branch "+dBranch) {
		t.Fatalf("notices %+v, want one saying the branch was deleted", r.notices)
	}
	r = pushRig()
	delete(r.infra.compares, "trunk..."+oidB)
	r.mustPush(t, dInfra, "refs/heads/"+dBranch)(context.Background(), applied("refs/heads/"+dBranch))
	if len(r.infra.closed) != 1 || len(r.infra.deleted) != 0 {
		t.Fatalf("an unreadable comparison: closed %v, deleted %v; want closed, nothing deleted", r.infra.closed, r.infra.deleted)
	}
	r = pushRig()
	r.pushedFiles(modified(".sops.yaml"))
	r.infra.deleteErr = errors.New("fake: 422")
	r.mustPush(t, dInfra, "refs/heads/"+dBranch)(context.Background(), applied("refs/heads/"+dBranch))
	if len(r.notices) != 1 || !strings.Contains(r.notices[0].Summary, "could NOT delete branch") {
		t.Fatalf("a refused delete: notices %+v, want one saying so", r.notices)
	}
}

// created is GitHub's 201 answer to POST pulls: pull request n on head sha.
func created(n int, sha string) connectors.ForwardResult {
	return connectors.ForwardResult{Forwarded: true, Status: http.StatusCreated, Complete: true,
		Body: []byte(`{"number":` + itoa(n) + `,"state":"open","draft":true,"head":{"ref":"` + dBranch + `","sha":"` + sha +
			`","repo":{"full_name":"` + dInfra + `"}},"base":{"ref":"trunk","repo":{"full_name":"` + dInfra + `"}}}`)}
}

// TestANewPullRequestIsRecheckedAtTheHeadGitHubCreatedItOn (R4): a push lands
// between the create's check of the branch and GitHub creating the pull
// request; the after-create check reads the comparison at the head GitHub
// answered and closes the pull request. With no readable answer, the branch's
// open pull requests are looked up and checked at their heads.
func TestANewPullRequestIsRecheckedAtTheHeadGitHubCreatedItOn(t *testing.T) {
	r := newDraftRig()
	out, err := apiAdmit(r.g, http.MethodPost, "repos/"+dInfra+"/pulls", goodCreate())
	if err != nil {
		t.Fatal(err)
	}
	after := connectors.AfterForwardOf(out)
	if after == nil {
		t.Fatal("a draft officer's create carries no after-create check")
	}
	cmp := r.infra.compares["trunk..."+dBranch]
	cmp.Files, cmp.Commits = []File{modified("nodes/cp-1/patch.yaml")}, []string{oidB}
	r.infra.compares["trunk..."+oidB] = cmp
	after(context.Background(), created(dPR, oidB))
	if len(r.infra.closed) != 1 || r.infra.closed[0] != dPR || len(r.notices) != 1 || r.notices[0].HeadSHA != oidB {
		t.Fatalf("a create on a moved head: closed %v, notices %+v; want #%d closed at %s", r.infra.closed, r.notices, dPR, oidB[:7])
	}

	r = newDraftRig()
	out, err = apiAdmit(r.g, http.MethodPost, "repos/"+dInfra+"/pulls", goodCreate())
	if err != nil {
		t.Fatal(err)
	}
	moved := r.infra.prs[dPR]
	moved.HeadSHA = oidB
	r.infra.all = []PR{moved}
	r.infra.compares["trunk..."+oidB] = cmp
	connectors.AfterForwardOf(out)(context.Background(), connectors.ForwardResult{Forwarded: true})
	if len(r.infra.closed) != 1 || r.infra.closed[0] != dPR {
		t.Fatalf("a create with no readable answer: closed %v, want #%d found by its branch", r.infra.closed, dPR)
	}
}

// sweepRig is the draft rig with #31 open in example-infra on fxHead.
func sweepRig() *draftRig {
	r := newDraftRig()
	r.infra.all = []PR{r.infra.prs[dPR]}
	return r
}

// TestTheSweepRechecksEveryOpenPullRequestAtItsHead (R2/R4 backstop): a clean
// pull request is read once and not again while its head is unchanged; a
// head that moves to a refused change is closed (and its branch deleted).
func TestTheSweepRechecksEveryOpenPullRequestAtItsHead(t *testing.T) {
	r := sweepRig()
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := r.g.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if r.infra.compareCalls != 1 || len(r.infra.closed) != 0 {
		t.Fatalf("two sweeps of an unchanged clean head: %d comparison reads, closed %v; want 1 and none", r.infra.compareCalls, r.infra.closed)
	}
	pr := r.infra.prs[dPR]
	pr.HeadSHA = oidB
	r.infra.prs[dPR] = pr
	cmp := r.infra.compares["trunk..."+dBranch]
	cmp.Files, cmp.Commits = []File{modified(".sops.yaml")}, []string{oidB}
	r.infra.compares["trunk..."+oidB] = cmp
	if err := r.g.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.infra.closed) != 1 || r.infra.closed[0] != dPR || len(r.infra.deleted) != 1 || len(r.notices) != 1 ||
		!strings.Contains(r.notices[0].Summary, ".sops.yaml") {
		t.Fatalf("a head moved to a refused change: closed %v, deleted %v, notices %+v", r.infra.closed, r.infra.deleted, r.notices)
	}
}

// TestTheSweepFailsClosedOnAPersistentlyUnreadableComparison: one unreadable
// read is retried (a GitHub blip closes nothing); maxSweepMisses in a row
// close the pull request, deleting nothing.
func TestTheSweepFailsClosedOnAPersistentlyUnreadableComparison(t *testing.T) {
	r := sweepRig()
	delete(r.infra.compares, "trunk..."+fxHead)
	ctx := context.Background()
	for i := 1; i < maxSweepMisses; i++ {
		_ = r.g.Sweep(ctx)
		if len(r.infra.closed) != 0 {
			t.Fatalf("closed after %d unreadable read(s), want only after %d", i, maxSweepMisses)
		}
	}
	_ = r.g.Sweep(ctx)
	if len(r.infra.closed) != 1 || len(r.infra.deleted) != 0 {
		t.Fatalf("after %d unreadable reads: closed %v, deleted %v; want closed, nothing deleted", maxSweepMisses, r.infra.closed, r.infra.deleted)
	}
}

// TestANewPullRequestNeedsTheInstancesClaimOnTheItem (Y2): the branch names
// item 17; HQ must show THIS instance holding it. Another holder, no claim,
// or no caller at all is a 403 before any comparison read.
func TestANewPullRequestNeedsTheInstancesClaimOnTheItem(t *testing.T) {
	for name, claims := range map[string]map[int]string{
		"another instance holds it": {17: "officer-engineering-example-web-2"},
		"nobody holds it":           {},
	} {
		r := newDraftRig()
		r.claims = claims
		if err := r.create(goodCreate()); !isGateRefusal(err) || isConflict(err) || !strings.Contains(err.Error(), "item 17") {
			t.Errorf("%s: err = %v, want a 403 naming item 17", name, err)
		}
		if r.infra.compareCalls != 0 {
			t.Errorf("%s: the comparison was read %d times before the claim check", name, r.infra.compareCalls)
		}
	}
	r := newDraftRig()
	raw, _ := json.Marshal(goodCreate())
	req, _ := http.NewRequest(http.MethodPost, "/x", strings.NewReader(string(raw)))
	if _, err := r.g.Admit(req, "api.github.com", "repos/"+dInfra+"/pulls"); !isGateRefusal(err) || !strings.Contains(err.Error(), "instance") {
		t.Errorf("no caller: err = %v, want a refusal naming the unknown instance", err)
	}
	r.g.ItemClaimed = nil
	if err := r.create(goodCreate()); !isGateRefusal(err) {
		t.Errorf("no HQ claim check configured: err = %v, want a refusal (fail closed)", err)
	}
}

// TestCreatesAreSerialisedPerOfficer (Y3): a second create while the first
// is still being forwarded waits for it, and is refused once CreateWait
// passes; after the first's after-forward hook it is admitted.
func TestCreatesAreSerialisedPerOfficer(t *testing.T) {
	r := newDraftRig()
	r.g.CreateWait = 50 * time.Millisecond
	out, err := apiAdmit(r.g, http.MethodPost, "repos/"+dInfra+"/pulls", goodCreate())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.create(goodCreate()); !isGateRefusal(err) || !strings.Contains(err.Error(), "being created") {
		t.Fatalf("a concurrent create: err = %v, want the busy refusal", err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	var second error
	go func() { defer wg.Done(); r.g.CreateWait = time.Second; second = r.create(goodCreate()) }()
	time.Sleep(10 * time.Millisecond)
	connectors.AfterForwardOf(out)(context.Background(), connectors.ForwardResult{})
	wg.Wait()
	if second != nil {
		t.Fatalf("a create after the first finished: %v", second)
	}
}

// TestTheRemovalRuleReachesEveryShapeOfDeletion (Y6): $patch: delete, a
// renamed metadata.name of a protected kind, and a dropped kustomization
// entry whose file (at the base) declares one are each refused; dropping an
// entry that declares something else, or renaming a Deployment, is admitted.
func TestTheRemovalRuleReachesEveryShapeOfDeletion(t *testing.T) {
	kust := "clusters/prod/apps/db/kustomization.yaml"
	for name, c := range map[string]struct {
		f    File
		base map[string]string
		ok   bool
	}{
		"$patch: delete": {f: File{Path: "clusters/prod/apps/db/drop.yaml", Status: "added", Changes: 4,
			Patch: "@@ -0,0 +1,4 @@\n+apiVersion: v1\n+kind: PersistentVolumeClaim\n+metadata: {name: data}\n+$patch: delete"}},
		"renamed pvc": {f: File{Path: "clusters/prod/apps/db/pvc.yaml", Status: "modified", Changes: 2,
			Patch: "@@ -1,4 +1,4 @@\n apiVersion: v1\n kind: PersistentVolumeClaim\n metadata:\n-  name: data\n+  name: data-2"}},
		"renamed cnpg": {f: File{Path: "clusters/prod/apps/db/cluster.yaml", Status: "modified", Changes: 2,
			Patch: "@@ -1,4 +1,4 @@\n apiVersion: postgresql.cnpg.io/v1\n kind: Cluster\n metadata:\n-  name: pg\n+  name: pg2"}},
		"dropped pvc entry": {f: File{Path: kust, Status: "modified", Changes: 1,
			Patch: "@@ -1,4 +1,3 @@\n resources:\n   - deploy.yaml\n-  - pvc.yaml\n   - svc.yaml"},
			base: map[string]string{"clusters/prod/apps/db/pvc.yaml": "apiVersion: v1\nkind: PersistentVolumeClaim\n"}},
		"dropped dir entry": {f: File{Path: kust, Status: "modified", Changes: 1,
			Patch: "@@ -1,3 +1,2 @@\n resources:\n-  - ./storage\n   - svc.yaml"}},
		"dropped unreadable entry": {f: File{Path: kust, Status: "modified", Changes: 1,
			Patch: "@@ -1,3 +1,2 @@\n resources:\n-  - gone.yaml\n   - svc.yaml"}},
		"dropped configmap entry (control)": {f: File{Path: kust, Status: "modified", Changes: 1,
			Patch: "@@ -1,3 +1,2 @@\n resources:\n-  - cm.yaml\n   - svc.yaml"},
			base: map[string]string{"clusters/prod/apps/db/cm.yaml": "apiVersion: v1\nkind: ConfigMap\n"}, ok: true},
		"dropped patch entry (control)": {f: File{Path: kust, Status: "modified", Changes: 1,
			Patch: "@@ -1,3 +1,2 @@\n patches:\n-  - path: pvc-size.yaml\n   - path: other.yaml"}, ok: true},
		"renamed deployment (control)": {f: File{Path: "clusters/prod/apps/x/deploy.yaml", Status: "modified", Changes: 2,
			Patch: "@@ -1,4 +1,4 @@\n apiVersion: apps/v1\n kind: Deployment\n metadata:\n-  name: web\n+  name: web-2"}, ok: true},
	} {
		r := newDraftRig()
		r.withFiles(c.f)
		for p, body := range c.base {
			r.infra.contents[p+"@trunk"] = body
		}
		err := r.create(goodCreate())
		if c.ok != (err == nil) {
			t.Errorf("%s: err = %v, want ok=%v", name, err, c.ok)
		}
		if !c.ok && !isConflict(err) {
			t.Errorf("%s: err = %v, want a 409", name, err)
		}
	}
}

// TestAPlainSecretManifestIsRefused (Y1): an added kind: Secret with literal
// data or stringData values is refused, whatever the keys are called; the
// same Secret SOPS-encrypted (ENC[...] values, a sops: block) is admitted, as
// is a ConfigMap with the same data (controls). A credential-named key with a
// short literal is refused too.
func TestAPlainSecretManifestIsRefused(t *testing.T) {
	plain := "@@ -0,0 +1,6 @@\n+apiVersion: v1\n+kind: Secret\n+metadata:\n+  name: app\n+stringData:\n+  DATABASE_URL: postgres-main"
	enc := "@@ -0,0 +1,9 @@\n+apiVersion: v1\n+kind: Secret\n+metadata:\n+  name: app\n+stringData:\n" +
		"+  DATABASE_URL: ENC[AES256_GCM,data:Zm9v,iv:YmFy,tag:YmF6,type:str]\n+sops:\n+  version: 3.8.1\n+  mac: ENC[AES256_GCM,data:eA==,iv:eQ==,tag:eg==,type:str]"
	cm := strings.Replace(strings.Replace(plain, "kind: Secret", "kind: ConfigMap", 1), "stringData:", "data:", 1)
	for name, c := range map[string]struct {
		patch string
		ok    bool
	}{
		"plain stringData":         {patch: plain},
		"plain data":               {patch: strings.Replace(plain, "stringData:", "data:", 1)},
		"short credential literal": {patch: "@@ -0,0 +1 @@\n+password: hunter2"},
		"passphrase literal":       {patch: "@@ -0,0 +1 @@\n+  password: correct-horse-battery-staple"},
		"sops-encrypted (control)": {patch: enc, ok: true},
		"configmap (control)":      {patch: cm, ok: true},
	} {
		r := newDraftRig()
		r.withFiles(File{Path: "clusters/prod/apps/x/secret.yaml", Status: "added", Changes: 9, Patch: c.patch})
		err := r.create(goodCreate())
		if c.ok != (err == nil) || (!c.ok && !isConflict(err)) {
			t.Errorf("%s: err = %v, want ok=%v", name, err, c.ok)
		}
	}
}
