package officership

import (
	"context"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// The draft gate's third round: a removed file GitHub shows no diff for is
// judged by its content at the base; .gitattributes (which can make a file's
// diff disappear) is refused anywhere; a credential's env pair is judged
// with its unchanged name line as context; and the creator filter the rate
// limits and the sweep read is cross-checked by the branch. Every value is
// synthetic.
// ---------------------------------------------------------------------------

// r3MergeBase is the comparison's merge base: the base read of a removed
// file is at this commit, never at the base branch's tip (which may since
// have changed).
const r3MergeBase = "b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3"

func (r *draftRig) withMergeBase(sha string) {
	for _, k := range []string{"trunk..." + dBranch, "trunk..." + fxHead} {
		c := r.infra.compares[k]
		c.MergeBase = sha
		r.infra.compares[k] = c
	}
}

// TestARemovedFileWithNoDiffIsJudgedByItsContentAtTheBase (R3): a removal
// GitHub reports with changes 0 and no patch (a binary file, or a manifest a
// .gitattributes made "binary") is read at the merge base and refused when it
// declares a protected kind or holds a credential, or when it cannot be read;
// a removed binary that declares nothing is admitted (control).
func TestARemovedFileWithNoDiffIsJudgedByItsContentAtTheBase(t *testing.T) {
	pvc := "clusters/prod/apps/db/pvc.yaml"
	for name, c := range map[string]struct {
		path, body string
		at         string // the ref the body is stored at ("" = unreadable)
		ok         bool
	}{
		"a pvc":           {path: pvc, body: "apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata:\n  name: data\n", at: r3MergeBase},
		"a cnpg cluster":  {path: "clusters/prod/apps/db/cluster.yaml", body: "apiVersion: postgresql.cnpg.io/v1\nkind: Cluster\n", at: r3MergeBase},
		"a canary":        {path: "clusters/prod/apps/web/canary.yaml", body: "apiVersion: flagger.app/v1beta1\nkind: Canary\n", at: r3MergeBase},
		"a credential":    {path: "clusters/prod/apps/x/values.yaml", body: "db:\n  " + dSecret + "\n", at: r3MergeBase},
		"unreadable":      {path: pvc},
		"only at the tip": {path: pvc, body: "apiVersion: v1\nkind: ConfigMap\n", at: "trunk"},
		"a binary that declares nothing (control)": {path: "images/old.png", body: "\x89PNG\r\n\x1a\n\x00\x00", at: r3MergeBase, ok: true},
	} {
		r := newDraftRig()
		r.withFiles(File{Path: c.path, Status: "removed"})
		r.withMergeBase(r3MergeBase)
		if c.at != "" {
			r.infra.contents[c.path+"@"+c.at] = c.body
		}
		err := r.create(goodCreate())
		if c.ok != (err == nil) || (!c.ok && !isConflict(err)) {
			t.Errorf("%s: err = %v, want ok=%v (a 409 when refused)", name, err, c.ok)
		}
	}
	// The same through a push re-check: the pull request is closed.
	r := pushRig()
	cmp := r.infra.compares["trunk..."+oidB]
	cmp.Files, cmp.MergeBase = []File{{Path: pvc, Status: "removed"}}, r3MergeBase
	r.infra.compares["trunk..."+oidB] = cmp
	r.infra.contents[pvc+"@"+r3MergeBase] = "apiVersion: v1\nkind: PersistentVolumeClaim\n"
	r.mustPush(t, dInfra, "refs/heads/"+dBranch)(context.Background(), applied("refs/heads/"+dBranch))
	if len(r.infra.closed) != 1 || r.infra.closed[0] != dPR {
		t.Fatalf("a push removing a no-diff pvc: closed %v, want [%d]", r.infra.closed, dPR)
	}
}

// TestAGitattributesChangeIsRefusedAnywhere (R3): a .gitattributes can mark
// manifests -diff or binary, which makes GitHub show their changes as no
// diff; added, modified or removed, at the root or in any directory, it is
// refused. A file merely NAMED like it is not (control).
func TestAGitattributesChangeIsRefusedAnywhere(t *testing.T) {
	for name, f := range map[string]File{
		"added at the root":    {Path: ".gitattributes", Status: "added", Changes: 1, Patch: "@@ -0,0 +1 @@\n+clusters/**/*.yaml -diff"},
		"added in a directory": {Path: "clusters/prod/.gitattributes", Status: "added", Changes: 1, Patch: "@@ -0,0 +1 @@\n+*.yaml binary"},
		"modified":             {Path: "apps/.gitattributes", Status: "modified", Changes: 2, Patch: "@@ -1 +1 @@\n-*.png binary\n+* binary"},
		"removed":              {Path: ".gitattributes", Status: "removed", Changes: 1, Patch: "@@ -1 +0,0 @@\n-*.png binary"},
		"renamed into place":   {Path: "clusters/.gitattributes", Previous: "docs/attrs.txt", Status: "renamed", Changes: 0},
	} {
		paths, why := ForbiddenFiles(nil, []File{f})
		// The reason, not the path: every reason names its path, and the path
		// already spells .gitattributes.
		if len(paths) == 0 || !strings.Contains(why, "hide a file's diff") {
			t.Errorf("%s: ForbiddenFiles = %v %q, want refused as a .gitattributes change", name, paths, why)
		}
	}
	if paths, why := ForbiddenFiles(nil, []File{{Path: "docs/about-gitattributes.md", Status: "added", Changes: 1,
		Patch: "@@ -0,0 +1 @@\n+How .gitattributes works."}}); len(paths) != 0 {
		t.Errorf("control: a doc about it: refused %v (%s)", paths, why)
	}
}

// TestAnEnvCredentialPairIsRefused (Y1): an officer that turns a Deployment's
// valueFrom into a literal value under an unchanged `- name: DB_PASSWORD`
// (the name line is the hunk's context) is refused; the same edit of a
// harmless variable is admitted (control).
func TestAnEnvCredentialPairIsRefused(t *testing.T) {
	hunk := func(name, value string) string {
		return "@@ -18,6 +18,5 @@ spec:\n           env:\n             - name: " + name + "\n" +
			"-              valueFrom:\n-                secretKeyRef: {name: db-credentials, key: password}\n" +
			"+              value: " + value + "\n           ports:\n             - containerPort: 8080"
	}
	for name, c := range map[string]struct {
		patch string
		ok    bool
	}{
		"a literal password":        {patch: hunk("DB_PASSWORD", "plainword")},
		"a long literal token":      {patch: hunk("API_TOKEN", "S0me"+"Passw0rd"+"Example")},
		"a harmless name (control)": {patch: hunk("DB_HOST", "db.example.internal"), ok: true},
		"a dependent var (control)": {patch: hunk("DB_PASSWORD", "$(PG_PASSWORD)"), ok: true},
	} {
		r := newDraftRig()
		r.withFiles(File{Path: "clusters/prod/apps/web/deploy.yaml", Status: "modified", Changes: 3, Patch: c.patch})
		err := r.create(goodCreate())
		if c.ok != (err == nil) || (!c.ok && !isConflict(err)) {
			t.Errorf("%s: err = %v, want ok=%v", name, err, c.ok)
		}
	}
}

// TestACreatorFilterThatListsNothingFailsClosed (Y2): GitHub answers the
// issues list's creator filter with an EMPTY list (200, not an error) for a
// login it does not match — measured live. A filter that misses the
// officer's pull requests would void the rate limits and the sweep, so both
// cross-check it by the officer's branches: a pull request of the officer's
// App found from one of its branches that the filter did not list is an
// error. The rate limits refuse; the sweep reports it and still checks the
// pull requests the branches found.
func TestACreatorFilterThatListsNothingFailsClosed(t *testing.T) {
	ownOpen := func(r *draftRig) {
		pr := r.infra.prs[dPR]
		pr.Body = goodBody(17)
		r.infra.all = []PR{pr}
	}
	// The rate limits: item 17 already has #31, which only the branch finds.
	r := newDraftRig()
	ownOpen(r)
	r.infra.creatorBlind = true
	if err := r.create(goodCreate()); err == nil || !strings.Contains(err.Error(), "creator filter") {
		t.Fatalf("a blind creator filter: create err = %v, want an error naming the creator filter", err)
	}
	// Control: the filter sees it, so the per-item limit refuses as before.
	r = newDraftRig()
	ownOpen(r)
	if err := r.create(goodCreate()); !isGateRefusal(err) || !strings.Contains(err.Error(), "item 17 already has 1") {
		t.Fatalf("control: err = %v, want the per-item refusal", err)
	}
	// The sweep: an error, and the pull request the branch found is still
	// checked (its refused change closes it).
	r = sweepRig()
	r.infra.creatorBlind = true
	cmp := r.infra.compares["trunk..."+fxHead]
	cmp.Files = []File{modified(".sops.yaml")}
	r.infra.compares["trunk..."+fxHead] = cmp
	err := r.g.Sweep(context.Background())
	if err == nil || !strings.Contains(err.Error(), "creator filter") {
		t.Fatalf("a blind creator filter: Sweep err = %v, want an error naming the creator filter", err)
	}
	if len(r.infra.closed) != 1 || r.infra.closed[0] != dPR {
		t.Fatalf("a blind creator filter: closed %v, want #%d (found by its branch) closed", r.infra.closed, dPR)
	}
	// Control: another bot's or a human's pull request from an officer
	// branch is not the officer's, so the filter not listing it is no error.
	r = sweepRig()
	other := r.infra.prs[dPR]
	other.Number, other.Author, other.AuthorType = 77, "someone", "User"
	r.infra.all = append(r.infra.all, other)
	if err := r.g.Sweep(context.Background()); err != nil {
		t.Fatalf("control: a human's pull request from an officer branch: Sweep err = %v", err)
	}
}
