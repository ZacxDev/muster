package officership

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// forbidden_paths_file: a repository that publishes its forbidden list in-repo
// names the file, and the draft gate reads it from the pull request's TARGET
// branch at every check (one source: muster holds no copy). The fixture's file
// lives ONLY at "trunk", so a read at the head branch (or any other ref) finds
// nothing and the cases below that must pass would be refused.
// ---------------------------------------------------------------------------

const dListFile = "scripts/ci/officer-forbidden-paths.txt"

// fileRig is the draft rig with example-infra's list in a file at trunk.
func fileRig(list string) *draftRig {
	r := newDraftRig()
	r.g.Ship.Repos[1].ForbiddenPathsFile = dListFile
	r.infra.contents[dListFile+"@trunk"] = list
	return r
}

// withFiles sets the comparison (create and PATCH alike) to files.
func (r *draftRig) withFiles(files ...File) {
	cmp := r.infra.compares["trunk..."+dBranch]
	cmp.Files = files
	r.infra.compares["trunk..."+dBranch], r.infra.compares["trunk..."+fxHead] = cmp, cmp
}

func modified(path string) File {
	return File{Path: path, Status: "modified", Changes: 2, Patch: "@@ -1 +1 @@\n-a: 1\n+a: 2"}
}

// TestTheForbiddenPathsFileIsReadFromTheTargetBranchAndEnforced: a glob from
// the file refuses a pull request (409, naming the glob) at create and at
// PATCH; comments and blank lines are skipped; the inline list still applies
// beside it (the union); an unlisted path is admitted (control, which also
// proves the read was at the target branch).
func TestTheForbiddenPathsFileIsReadFromTheTargetBranchAndEnforced(t *testing.T) {
	list := "# the identity apps, and the overlays that apply them\n\noverlays/*/read-role/**\n  clusters/*-overlay/*read-role*  \n"
	for _, at := range []string{"create", "patch"} {
		for _, p := range []string{"overlays/site-a/read-role/patch.yaml", "clusters/site-a-overlay/apps-read-role.yaml", "nodes/cp-1/x.yaml"} {
			r := fileRig(list)
			r.withFiles(modified(p))
			err := r.create(goodCreate())
			if at == "patch" {
				err = r.patch(map[string]any{"title": "x"})
			}
			if !isConflict(err) || !strings.Contains(err.Error(), "a forbidden path") {
				t.Errorf("%s %s: err = %v, want a 409 forbidden-path refusal", at, p, err)
			}
		}
		r := fileRig(list)
		r.withFiles(modified("clusters/prod/apps/x/deploy.yaml"))
		err := r.create(goodCreate())
		if at == "patch" {
			err = r.patch(map[string]any{"title": "x"})
		}
		if err != nil {
			t.Errorf("control %s: an unlisted path: %v", at, err)
		}
	}
}

// TestAnUnreadableForbiddenPathsFileRefusesThePullRequest: fail closed. A
// file missing at the target branch, or holding a line that is not a glob, is
// a 409 even when the comparison is clean, and HQ is told.
func TestAnUnreadableForbiddenPathsFileRefusesThePullRequest(t *testing.T) {
	r := newDraftRig()
	r.g.Ship.Repos[1].ForbiddenPathsFile = dListFile // set, but no such file
	if err := r.create(goodCreate()); !isConflict(err) || !strings.Contains(err.Error(), dListFile) || len(r.notices) != 1 {
		t.Fatalf("a missing list file: err = %v, notices %d; want a 409 naming the file and a notice", err, len(r.notices))
	}
	r = fileRig("overlays/*/read-role/**\nnodes/[ab]*\n")
	if err := r.create(goodCreate()); !isConflict(err) || !strings.Contains(err.Error(), dListFile) {
		t.Fatalf("a list file with a bad line: err = %v, want a 409 naming the file", err)
	}
	r = fileRig("overlays/*/read-role/**\n")
	if err := r.create(goodCreate()); err != nil {
		t.Fatalf("control: a readable list and a clean comparison: %v", err)
	}
}

// TestAPullRequestThatEditsTheListFileIsRefused: the file is implicitly
// forbidden, so an officer cannot widen its own wall — changed, removed or
// renamed away.
func TestAPullRequestThatEditsTheListFileIsRefused(t *testing.T) {
	for name, f := range map[string]File{
		"changed": modified(dListFile),
		"removed": {Path: dListFile, Status: "removed", Changes: 1, Patch: "@@ -1 +0,0 @@\n-overlays/*/read-role/**"},
		"renamed": {Path: "scripts/ci/old.txt", Previous: dListFile, Status: "renamed"},
	} {
		r := fileRig("overlays/*/read-role/**\n")
		r.withFiles(f)
		if err := r.create(goodCreate()); !isConflict(err) || !strings.Contains(err.Error(), dListFile) {
			t.Errorf("%s: err = %v, want a 409 naming the list file", name, err)
		}
	}
}
