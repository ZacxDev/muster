package officership

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// test_only_paths_file ("money paths"): a repository publishes, in-repo, the
// globs an officer's pull request may change ONLY in a test file — a
// `*.test.*` or `*.spec.*` name, the target repository's own CI checker's
// rule. The draft gate reads the file from the TARGET branch at every check,
// as it reads forbidden_paths_file. The fixture's file lives ONLY at "trunk".
// ---------------------------------------------------------------------------

const dMoneyFile = ".github/officer/money-paths.txt"

// moneyRig is the draft rig with example-infra's money paths in a file at
// trunk.
func moneyRig(list string) *draftRig {
	r := newDraftRig()
	r.g.Ship.Repos[1].TestOnlyPathsFile = dMoneyFile
	r.infra.contents[dMoneyFile+"@trunk"] = list
	return r
}

// TestAMoneyPathIsChangedOnlyInATest: a non-test file under a money glob
// refuses the pull request (409, naming the glob and the rule) at create and
// at PATCH — modified, added, removed, or renamed into or out of it; a
// `*.test.*` or `*.spec.*` file under the same glob is admitted, as is any
// file outside every glob (control: the read was at the target branch).
func TestAMoneyPathIsChangedOnlyInATest(t *testing.T) {
	list := "# money\n\nsrc/server/services/billing/**\n  src/pages/api/webhooks/*.ts  \n"
	refused := map[string]File{
		"modified":      modified("src/server/services/billing/transfer.ts"),
		"added":         {Path: "src/server/services/billing/new.ts", Status: "added", Changes: 1, Patch: "@@ -0,0 +1 @@\n+export {}"},
		"removed":       {Path: "src/pages/api/webhooks/payments.ts", Status: "removed", Changes: 1, Patch: "@@ -1 +0,0 @@\n-export {}"},
		"renamed in":    {Path: "src/server/services/billing/moved.ts", Previous: "src/utils/x.ts", Status: "renamed", Changes: 1, Patch: "@@ -1 +1 @@\n-a\n+b"},
		"renamed out":   {Path: "src/utils/moved.ts", Previous: "src/server/services/billing/transfer.ts", Status: "renamed", Changes: 1, Patch: "@@ -1 +1 @@\n-a\n+b"},
		"test-like dir": modified("src/server/services/billing/transfer.test/index.ts"),
	}
	admitted := map[string]File{
		"a .test. file":  modified("src/server/services/billing/transfer.test.ts"),
		"a .spec. file":  {Path: "src/pages/api/webhooks/payments.spec.ts", Status: "added", Changes: 1, Patch: "@@ -0,0 +1 @@\n+it('x', () => {})"},
		"outside a glob": modified("src/components/Button.tsx"),
	}
	for _, at := range []string{"create", "patch"} {
		for name, f := range refused {
			r := moneyRig(list)
			r.withFiles(f)
			err := r.create(goodCreate())
			if at == "patch" {
				err = r.patch(map[string]any{"title": "x"})
			}
			if !isConflict(err) || !strings.Contains(err.Error(), "a money path") || !strings.Contains(err.Error(), "*.test.*") {
				t.Errorf("%s %s: err = %v, want a 409 money-path refusal", at, name, err)
			}
		}
		for name, f := range admitted {
			r := moneyRig(list)
			r.withFiles(f)
			err := r.create(goodCreate())
			if at == "patch" {
				err = r.patch(map[string]any{"title": "x"})
			}
			if err != nil {
				t.Errorf("%s %s: %v, want it admitted", at, name, err)
			}
		}
	}
}

// TestAnUnreadableMoneyPathsFileRefusesThePullRequest: fail closed. A file
// missing at the target branch, or holding a line that is not a glob, is a
// 409 naming the file even when the comparison is clean, and HQ is told.
// Control: a readable list and a clean comparison pass.
func TestAnUnreadableMoneyPathsFileRefusesThePullRequest(t *testing.T) {
	r := newDraftRig()
	r.g.Ship.Repos[1].TestOnlyPathsFile = dMoneyFile // set, but no such file
	if err := r.create(goodCreate()); !isConflict(err) || !strings.Contains(err.Error(), dMoneyFile) || len(r.notices) != 1 {
		t.Fatalf("a missing money list: err = %v, notices %d; want a 409 naming the file and a notice", err, len(r.notices))
	}
	r = moneyRig("src/server/services/billing/**\nsrc/[ab]*\n")
	if err := r.create(goodCreate()); !isConflict(err) || !strings.Contains(err.Error(), dMoneyFile) {
		t.Fatalf("a money list with a bad line: err = %v, want a 409 naming the file", err)
	}
	r = moneyRig("src/server/services/billing/**\n")
	if err := r.create(goodCreate()); err != nil {
		t.Fatalf("control: a readable list and a clean comparison: %v", err)
	}
}

// TestAPullRequestThatEditsTheMoneyListIsRefused: the list file is itself
// forbidden, so an officer cannot shrink its own money paths — even by
// renaming it to a test-shaped name.
func TestAPullRequestThatEditsTheMoneyListIsRefused(t *testing.T) {
	for name, f := range map[string]File{
		"changed": modified(dMoneyFile),
		"renamed": {Path: ".github/officer/money-paths.test.txt", Previous: dMoneyFile, Status: "renamed", Changes: 1, Patch: "@@ -1 +1 @@\n-a\n+b"},
	} {
		r := moneyRig("src/server/services/billing/**\n")
		r.withFiles(f)
		if err := r.create(goodCreate()); !isConflict(err) || !strings.Contains(err.Error(), dMoneyFile) {
			t.Errorf("%s: err = %v, want a 409 naming the money list", name, err)
		}
	}
}

// TestIsTestFileIsTheCheckersRule: the file's NAME (its last segment) matches
// `*.test.*` or `*.spec.*`; a directory so named, a bare `test.ts`, a
// `.test` suffix and a name containing "test" otherwise do not.
func TestIsTestFileIsTheCheckersRule(t *testing.T) {
	for p, want := range map[string]bool{
		"src/a/b.test.ts": true, "b.spec.tsx": true, "src/a.test.d.ts": true, "src/x/.test.ts": true,
		"src/a/b.test": false, "src/a/test.ts": false, "src/a.test/b.ts": false, "src/a/btest.ts": false, "src/a/b.tests.ts": false,
	} {
		if got := IsTestFile(p); got != want {
			t.Errorf("IsTestFile(%q) = %v, want %v", p, got, want)
		}
	}
}
