package officership

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// DOCS-ONLY DRAFTS AND PUBLIC REPOSITORIES, against the fake GitHub
// (docsgate.go). The rig is the draft rig with a docs_only block, the
// example-infra pull request's comparison a clean prose edit of README.md, and
// README.md readable at the base (trunk) and at every head a check reads
// (the branch at create, fxHead at PATCH and sweep, oidB after a push).
// Every value is synthetic.
// ---------------------------------------------------------------------------

// readmeBase is README.md at the base. Lines: 1 title, 3 prose, 5-7 a fenced
// block, 9 prose.
const readmeBase = "# Example\n\nRun the tool:\n\n```sh\nmake deploy\n```\n\nThe end.\n"

// readmeHead is readmeBase with line 9 edited (prose).
const readmeHead = "# Example\n\nRun the tool:\n\n```sh\nmake deploy\n```\n\nThe finish.\n"

// proseEdit is the clean change: line 9, outside the fence.
var proseEdit = File{Path: "README.md", Status: "modified", Changes: 2, Patch: "@@ -8,2 +8,2 @@\n \n-The end.\n+The finish."}

var docsHeads = []string{dBranch, fxHead, oidB}

func docsRig(t *testing.T) *draftRig {
	t.Helper()
	r := newDraftRig()
	r.g.Ship.DocsOnly = &officers.DocsOnly{AllowedPaths: []string{"**/*.md", ".claude/skills/**/*.md", "claudedocs/**"},
		MaxDiffLines: 37, MaxFiles: 3, MaxPRsPerDay: officers.MaxNewPRsPerDay}
	// The charters stay out by the forbidden list, which still applies.
	r.g.Ship.Repos[1].ForbiddenPaths = append(r.g.Ship.Repos[1].ForbiddenPaths, "docs/officers/**", "OFFICERS.md")
	r.content("README.md", readmeBase, readmeHead)
	r.docsFiles(proseEdit)
	return r
}

// content puts p's text at the base and at every head.
func (r *draftRig) content(p, base, head string) {
	r.infra.contents[p+"@trunk"] = base
	for _, h := range docsHeads {
		r.infra.contents[p+"@"+h] = head
	}
}

// docsFiles makes files the comparison at every head.
func (r *draftRig) docsFiles(files ...File) {
	for _, h := range docsHeads {
		c := r.infra.compares["trunk..."+dBranch]
		c.Files = files
		if h == oidB {
			c.Commits = []string{oidB}
		}
		r.infra.compares["trunk..."+h] = c
	}
}

// at runs one check entry point: "create" (POST pulls), "patch" (PATCH
// pulls/31 setting the body), "sweep" (#31 open at fxHead). It reports the
// refusal (create/patch) or whether #31 was closed (sweep: closed => a
// non-nil error).
func (r *draftRig) at(t *testing.T, where string) error {
	t.Helper()
	switch where {
	case "create":
		return r.create(goodCreate())
	case "patch":
		return r.patch(map[string]any{"body": goodBody(17)})
	default:
		r.infra.all = []PR{r.infra.prs[dPR]}
		_ = r.g.Sweep(context.Background())
		if len(r.infra.closed) > 0 {
			return errClosedBySweep
		}
		return nil
	}
}

var errClosedBySweep = &sweepClosed{}

type sweepClosed struct{}

func (*sweepClosed) Error() string { return "the sweep closed the pull request" }

func refusedOrClosed(where string, err error) bool {
	if where == "sweep" {
		return err == errClosedBySweep
	}
	return isConflict(err)
}

// TestADocsOnlyPullRequestTouchingCodeIsRefusedWhole: one .md and one .go
// file — the .md change is clean — is refused WHOLE at create, at PATCH, and
// by the sweep (closed, its branch deleted); the notice names the .go path.
// Control: the .md file alone passes every entry point.
func TestADocsOnlyPullRequestTouchingCodeIsRefusedWhole(t *testing.T) {
	goFile := File{Path: "cmd/tool/main.go", Status: "modified", Changes: 2, Patch: "@@ -1 +1 @@\n-package main\n+package main // x"}
	for _, where := range []string{"create", "patch", "sweep"} {
		r := docsRig(t)
		r.docsFiles(proseEdit, goFile)
		err := r.at(t, where)
		if !refusedOrClosed(where, err) {
			t.Errorf("%s: .md + .go: err = %v, want the whole pull request refused", where, err)
			continue
		}
		if where != "sweep" && !strings.Contains(err.Error(), "outside docs_only.allowed_paths") {
			t.Errorf("%s: err = %v, want the allowlist named", where, err)
		}
		if len(r.notices) != 1 || !contains(r.notices[0].Paths, "cmd/tool/main.go") || contains(r.notices[0].Paths, "README.md") {
			t.Errorf("%s: notices %+v, want one naming cmd/tool/main.go only", where, r.notices)
		}
		if where == "sweep" && (len(r.infra.deleted) != 1 || r.infra.deleted[0] != dBranch) {
			t.Errorf("sweep: deleted %v, want the branch holding the code change", r.infra.deleted)
		}
		// Control.
		r = docsRig(t)
		if err := r.at(t, where); err != nil {
			t.Errorf("%s: control: a clean .md edit was refused: %v", where, err)
		}
	}
}

// TestTrickPathsCannotEscapeAllowedPaths: a rename or copy FROM code, a
// path-traversal or dot segment, a directory named like a doc, a backslash,
// an absolute path, a control character, an empty path, removing a code
// file, and a charter (a .md file the forbidden list keeps out) are each
// refused. Controls: nested allowed docs pass.
func TestTrickPathsCannotEscapeAllowedPaths(t *testing.T) {
	md := func(p string) File {
		return File{Path: p, Status: "added", Changes: 1, Patch: "@@ -0,0 +1 @@\n+prose"}
	}
	for name, c := range map[string]struct {
		f    File
		want string
	}{
		"rename code to .md":                {File{Path: "cmd/tool/main.md", Previous: "cmd/tool/main.go", Status: "renamed", Changes: 1, Patch: "@@ -1 +1 @@\n-x\n+y"}, "outside docs_only"},
		"rename .md to code":                {File{Path: "scripts/run.sh", Previous: "docs/run.md", Status: "renamed", Changes: 1, Patch: "@@ -1 +1 @@\n-x\n+y"}, "outside docs_only"},
		"copy from code":                    {File{Path: "docs/main.md", Previous: "cmd/tool/main.go", Status: "copied", Changes: 1, Patch: "@@ -1 +1 @@\n-x\n+y"}, "outside docs_only"},
		"traversal":                         {md("docs/../cmd/tool/main.md"), "not a clean repository path"},
		"dot segment":                       {md("./README2.md"), "not a clean repository path"},
		"double slash":                      {md("docs//x.md"), "not a clean repository path"},
		"a directory .md":                   {File{Path: "notes.md/run.go", Status: "added", Changes: 1, Patch: "@@ -0,0 +1 @@\n+package x"}, "outside docs_only"},
		"backslash":                         {md("docs\\..\\main.md"), "not a clean repository path"},
		"absolute":                          {md("/etc/motd.md"), "not a clean repository path"},
		"a newline":                         {md("a.md\nb.go"), "not a clean repository path"},
		"empty":                             {md(""), "not a clean repository path"},
		"removing code":                     {File{Path: "Makefile", Status: "removed", Changes: 1, Patch: "@@ -1 +0,0 @@\n-all:"}, "outside docs_only"},
		"a charter":                         {md("docs/officers/docs.md"), "a forbidden path"},
		"the officers index":                {md("OFFICERS.md"), "a forbidden path"},
		"rename from code named like a doc": {File{Path: "docs/x.md", Previous: "docs/x.md.go", Status: "renamed", Changes: 1, Patch: "@@ -1 +1 @@\n-x\n+y"}, "outside docs_only"},
	} {
		r := docsRig(t)
		r.docsFiles(proseEdit, c.f)
		if err := r.create(goodCreate()); !isConflict(err) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want a 409 naming %q", name, err, c.want)
		}
	}
	for _, p := range []string{"docs/deep/guide.md", ".claude/skills/x/SKILL.md", "claudedocs/handoff-x.md"} {
		r := docsRig(t)
		r.content(p, "", "prose\n")
		r.docsFiles(proseEdit, md(p))
		if err := r.create(goodCreate()); err != nil {
			t.Errorf("control: %s was refused: %v", p, err)
		}
	}
}

// TestAFencedCodeEditIsRefused: a changed line inside a fenced block (or a
// fence line itself) is refused, read from the WHOLE file so a hunk that
// starts inside a fence is caught; so is a non-Markdown allowed file.
// Controls: the line right after the closing fence (the boundary) and prose.
// No switch lifts the rule (docs_only has no allow_command_edits).
func TestAFencedCodeEditIsRefused(t *testing.T) {
	refused := map[string]struct {
		base, head string
		patch      string
	}{
		"inside a visible fence": {readmeBase, strings.Replace(readmeBase, "make deploy\n", "make deploy-all\n", 1),
			"@@ -5,3 +5,3 @@\n ```sh\n-make deploy\n+make deploy-all\n ```"},
		"a hunk that starts inside the fence": {readmeBase, strings.Replace(readmeBase, "make deploy\n", "make deploy-all\n", 1),
			"@@ -6 +6 @@\n-make deploy\n+make deploy-all"},
		"the closing fence line": {readmeBase, strings.Replace(readmeBase, "```\n\nThe", "````\n\nThe", 1),
			"@@ -7 +7 @@\n-```\n+````"},
		"removing the opening fence": {readmeBase, strings.Replace(readmeBase, "```sh\n", "", 1),
			"@@ -4,3 +4,2 @@\n \n-```sh\n make deploy"},
		"adding a fence around prose": {readmeBase, strings.Replace(readmeBase, "The end.\n", "```\nThe end.\n```\n", 1),
			"@@ -9 +9,3 @@\n+```\n The end.\n+```"},
		"a tilde fence": {"a\n~~~\nrm x\n~~~\n", "a\n~~~\nrm y\n~~~\n", "@@ -3 +3 @@\n-rm x\n+rm y"},
		"a fence indented under a list item": {"- step\n\n      ```\n      rm x\n      ```\n", "- step\n\n      ```\n      rm y\n      ```\n",
			"@@ -4 +4 @@\n-      rm x\n+      rm y"},
		"a fence in a blockquote": {"> ```\n> rm x\n> ```\n", "> ```\n> rm y\n> ```\n", "@@ -2 +2 @@\n-> rm x\n+> rm y"},
		// An indented literal ``` the strict reading ignores and the loose one
		// opens on: the loose reading then CLOSES on the real opener and calls
		// the real block's line prose; the strict reading must still win.
		"a desync attempt":              {"    ```\n\ntext\n\n```\nrm x\n```\n", "    ```\n\ntext\n\n```\nrm y\n```\n", "@@ -6 +6 @@\n-rm x\n+rm y"},
		"a line the file does not have": {readmeBase, readmeHead, "@@ -40 +40 @@\n-ghost\n+ghost2"},
	}
	for name, c := range refused {
		r := docsRig(t)
		r.content("README.md", c.base, c.head)
		r.docsFiles(File{Path: "README.md", Status: "modified", Changes: 2, Patch: c.patch})
		if err := r.create(goodCreate()); !isConflict(err) || !strings.Contains(err.Error(), "fenced code block") {
			t.Errorf("%s: err = %v, want the fenced-code refusal", name, err)
		}
	}
	r := docsRig(t)
	r.content("claudedocs/plan.json", "{}\n", "{\"a\": 1}\n")
	r.docsFiles(File{Path: "claudedocs/plan.json", Status: "modified", Changes: 2, Patch: "@@ -1 +1 @@\n-{}\n+{\"a\": 1}"})
	if err := r.create(goodCreate()); !isConflict(err) || !strings.Contains(err.Error(), "not a Markdown file") {
		t.Errorf("a non-Markdown allowed file: err = %v, want refused", err)
	}
	// Controls. The boundary: line 8, right after the closing fence.
	r = docsRig(t)
	r.content("README.md", readmeBase, strings.Replace(readmeBase, "```\n\nThe", "```\nNote.\nThe", 1))
	r.docsFiles(File{Path: "README.md", Status: "modified", Changes: 2, Patch: "@@ -7,3 +7,3 @@\n ```\n-\n+Note.\n The end."})
	if err := r.create(goodCreate()); err != nil {
		t.Errorf("control: the line after the closing fence was refused: %v", err)
	}
	// Fail closed: the head cannot be read.
	r = docsRig(t)
	delete(r.infra.contents, "README.md@"+dBranch)
	if err := r.create(goodCreate()); !isConflict(err) || !strings.Contains(err.Error(), "could not be checked") {
		t.Errorf("an unreadable head: err = %v, want refused", err)
	}
}

// TestTheDocsOnlySizeCaps: max_files 3 and max_diff_lines 37 admit AT the
// cap and in the middle, and refuse one over.
func TestTheDocsOnlySizeCaps(t *testing.T) {
	doc := func(i, changes int) File {
		return File{Path: "docs/d" + itoa(i) + ".md", Status: "modified", Changes: changes, Patch: "@@ -1 +1 @@\n-a\n+b"}
	}
	for name, c := range map[string]struct {
		files []int // each file's changes
		ok    bool
	}{
		"one file, 12 lines":   {[]int{12}, true},
		"three files (at cap)": {[]int{1, 1, 1}, true},
		"four files":           {[]int{1, 1, 1, 1}, false},
		"37 lines (at cap)":    {[]int{30, 7}, true},
		"38 lines":             {[]int{30, 8}, false},
	} {
		r := docsRig(t)
		var fs []File
		for i, n := range c.files {
			r.content("docs/d"+itoa(i)+".md", "a\n", "b\n")
			fs = append(fs, doc(i, n))
		}
		r.docsFiles(fs...)
		err := r.create(goodCreate())
		if c.ok && err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
		if !c.ok && (!isConflict(err) || !strings.Contains(err.Error(), "docs_only allows at most")) {
			t.Errorf("%s: err = %v, want the size cap", name, err)
		}
	}
}

// TestTheDocsOnlyPerDayCap: max_prs_per_day 1 refuses a second pull request
// today; the stage's own limit (3) admits it (control).
func TestTheDocsOnlyPerDayCap(t *testing.T) {
	for _, perDay := range []int{1, officers.MaxNewPRsPerDay} {
		r := docsRig(t)
		r.g.Ship.DocsOnly.MaxPRsPerDay = perDay
		r.web.all = []PR{{Number: 104, State: "closed", Author: BotLogin(r.web.slug), AuthorType: "Bot",
			HeadRef: "officer/engineering-example-web/4-x", CreatedAt: dNow.Add(-time.Hour), Body: goodBody(4)}}
		err := r.create(goodCreate())
		if perDay == 1 && (!isGateRefusal(err) || !strings.Contains(err.Error(), "the limit is 1 a day")) {
			t.Errorf("max_prs_per_day 1: err = %v, want the per-day refusal", err)
		}
		if perDay != 1 && err != nil {
			t.Errorf("control: the stage's limit refused a second pull request: %v", err)
		}
	}
}

// internalPatterns is a synthetic internal-details pattern file: line 2 an
// internal domain, line 4 an address range.
const internalPatterns = "# synthetic\n\\.corp\\.example\\b\n\n198\\.51\\.100\\.[0-9]+\n"

func publicRig(t *testing.T) *draftRig {
	t.Helper()
	r := docsRig(t)
	p := filepath.Join(t.TempDir(), "internal-patterns.txt")
	if err := os.WriteFile(p, []byte(internalPatterns), 0o600); err != nil {
		t.Fatal(err)
	}
	r.g.Ship.InternalPatternsFile = p
	r.g.Ship.Repos[1].Public = true
	return r
}

// TestAPublicRepositoryIsScannedForInternalDetails: an internal detail in an
// added line, a new path, a commit message, a commit identity, the title, the
// body, a PATCHed title or a comment is refused, naming the pattern's LINE and
// never the text. Controls: a REMOVED line holding one (cleaning it up), and
// the same change in a repository not marked public.
func TestAPublicRepositoryIsScannedForInternalDetails(t *testing.T) {
	leak := "db.corp.example"
	edit := func(added string) File {
		return File{Path: "README.md", Status: "modified", Changes: 2, Patch: "@@ -8,2 +8,2 @@\n \n-The end.\n+" + added}
	}
	for name, c := range map[string]struct {
		setup func(r *draftRig)
		do    func(r *draftRig) error
		line  string
	}{
		"an added line": {func(r *draftRig) {
			r.content("README.md", readmeBase, strings.Replace(readmeBase, "The end.", "Ask "+leak, 1))
			r.docsFiles(edit("Ask " + leak))
		}, nil, "line 2"},
		"a new path": {func(r *draftRig) {
			r.content("docs/db.corp.example.md", "", "x\n")
			r.docsFiles(proseEdit, File{Path: "docs/db.corp.example.md", Status: "added", Changes: 1, Patch: "@@ -0,0 +1 @@\n+x"})
		}, nil, "line 2"},
		"a commit message": {func(r *draftRig) {
			c := r.infra.compares["trunk..."+dBranch]
			c.CommitTexts = []string{"fix docs, seen on 198.51.100.34\nOfficer <bot@example.test>\nOfficer <bot@example.test>"}
			r.infra.compares["trunk..."+dBranch] = c
		}, nil, "line 4"},
		"a commit identity": {func(r *draftRig) {
			c := r.infra.compares["trunk..."+dBranch]
			c.CommitTexts = []string{"fix docs\nroot <root@pod-7." + leak + ">\nroot <root@pod-7." + leak + ">"}
			r.infra.compares["trunk..."+dBranch] = c
		}, nil, "line 2"},
		"the title": {nil, func(r *draftRig) error {
			m := goodCreate()
			m["title"] = "Fix the " + leak + " runbook"
			return r.create(m)
		}, "line 2"},
		"the body": {nil, func(r *draftRig) error {
			m := goodCreate()
			m["body"] = goodBody(17) + "\nSee 198.51.100.41\n"
			return r.create(m)
		}, "line 4"},
		"a PATCHed title": {nil, func(r *draftRig) error { return r.patch(map[string]any{"title": "x " + leak}) }, "line 2"},
		"a comment": {nil, func(r *draftRig) error {
			return apiWrite(r.g, http.MethodPost, "repos/"+dInfra+"/issues/31/comments", map[string]any{"body": "on " + leak})
		}, "line 2"},
	} {
		r := publicRig(t)
		if c.setup != nil {
			c.setup(r)
		}
		do := c.do
		if do == nil {
			do = func(r *draftRig) error { return r.create(goodCreate()) }
		}
		err := do(r)
		if !isConflict(err) || !strings.Contains(err.Error(), "internal-details pattern "+c.line) {
			t.Errorf("%s: err = %v, want a 409 naming pattern %s", name, err, c.line)
			continue
		}
		if name == "a new path" {
			continue // the path is the finding: the refusal must name it
		}
		for _, s := range append([]string{err.Error()}, noticeTexts(r)...) {
			if strings.Contains(s, "corp.example") || strings.Contains(s, "198.51.100.") {
				t.Errorf("%s: a refusal or notice quotes the matched text: %q", name, s)
			}
		}
	}
	// Control: removing an internal detail from a public repository passes.
	r := publicRig(t)
	r.content("README.md", strings.Replace(readmeBase, "The end.", "Ask "+leak, 1), readmeHead)
	r.docsFiles(File{Path: "README.md", Status: "modified", Changes: 2, Patch: "@@ -8,2 +8,2 @@\n \n-Ask " + leak + "\n+The finish."})
	if err := r.create(goodCreate()); err != nil {
		t.Errorf("control: removing an internal detail was refused: %v", err)
	}
	// Control: the same added line in a repository NOT marked public.
	r = publicRig(t)
	r.g.Ship.Repos[1].Public = false
	r.content("README.md", readmeBase, strings.Replace(readmeBase, "The end.", "Ask "+leak, 1))
	r.docsFiles(edit("Ask " + leak))
	if err := r.create(goodCreate()); err != nil {
		t.Errorf("control: a private repository was scanned: %v", err)
	}
	// Fail closed: the pattern file disappears after boot.
	r = publicRig(t)
	if err := os.Remove(r.g.Ship.InternalPatternsFile); err != nil {
		t.Fatal(err)
	}
	if err := r.create(goodCreate()); !isConflict(err) || !strings.Contains(err.Error(), "could not") {
		t.Errorf("an unreadable pattern file: err = %v, want refused", err)
	}
}

// TestAPushToAPublicBranchWithNoPullRequestIsCheckedAndDeleted: a public
// repository's branch is world-readable once pushed, so a push with no pull
// request open from it is checked at once and a hit deletes the branch, with
// a notice that calls it a leak. Controls: a clean push deletes nothing; the
// same hit in a private repository waits for the pull request, as before.
func TestAPushToAPublicBranchWithNoPullRequestIsCheckedAndDeleted(t *testing.T) {
	bad := File{Path: "README.md", Status: "modified", Changes: 2, Patch: "@@ -8,2 +8,2 @@\n \n-The end.\n+Ask db.corp.example"}
	for _, c := range []struct {
		name          string
		public, dirty bool
		wantDeleted   bool
	}{
		{"public, a hit", true, true, true},
		{"public, clean", true, false, false},
		{"private, a hit", false, true, false},
	} {
		r := publicRig(t)
		r.g.Ship.Repos[1].Public = c.public
		r.infra.all = nil // no pull request from the branch
		r.infra.branches[dBranch] = oidB
		r.content("README.md", readmeBase, strings.Replace(readmeBase, "The end.", "Ask db.corp.example", 1))
		switch {
		case c.dirty && !c.public:
			// A hit in ANY repository (a code path, refused by docs_only),
			// so only the public flag can be what decides.
			r.docsFiles(proseEdit, File{Path: "cmd/tool/main.go", Status: "modified", Changes: 2, Patch: "@@ -1 +1 @@\n-a\n+b"})
		case c.dirty:
			r.docsFiles(bad)
		default:
			r.content("README.md", readmeBase, readmeHead)
			r.docsFiles(proseEdit)
		}
		r.mustPush(t, dInfra, "refs/heads/"+dBranch)(context.Background(), applied("refs/heads/"+dBranch))
		deleted := len(r.infra.deleted) == 1 && r.infra.deleted[0] == dBranch
		if deleted != c.wantDeleted {
			t.Errorf("%s: deleted %v, want deleted=%v", c.name, r.infra.deleted, c.wantDeleted)
		}
		if c.wantDeleted && (len(r.notices) != 1 || !strings.Contains(r.notices[0].Summary, "treat it as a leak")) {
			t.Errorf("%s: notices %+v, want one calling it a leak", c.name, r.notices)
		}
		if !c.wantDeleted && len(r.notices) != 0 {
			t.Errorf("%s: notices %+v, want none", c.name, r.notices)
		}
	}
}

// TestAnUnreadablePatternFileFailsClosedOnAPush: the pattern file goes away
// after boot and a push lands in an open pull request of a public
// repository: the re-check cannot scan it, so the pull request is closed
// (fail closed) and its branch kept (nothing wrong is known about it). The
// title and body are not read here, so only the diff scan can refuse.
func TestAnUnreadablePatternFileFailsClosedOnAPush(t *testing.T) {
	for _, gone := range []bool{false, true} {
		r := publicRig(t)
		r.infra.all = []PR{r.infra.prs[dPR]}
		r.infra.listed = r.infra.all
		r.infra.branches[dBranch] = oidB
		if gone {
			if err := os.Remove(r.g.Ship.InternalPatternsFile); err != nil {
				t.Fatal(err)
			}
		}
		r.mustPush(t, dInfra, "refs/heads/"+dBranch)(context.Background(), applied("refs/heads/"+dBranch))
		if gone && (len(r.infra.closed) != 1 || len(r.infra.deleted) != 0 || len(r.notices) != 1 ||
			!strings.Contains(r.notices[0].Summary, "internal-details scan could not run")) {
			t.Errorf("pattern file gone: closed %v, deleted %v, notices %+v; want closed, kept, one notice", r.infra.closed, r.infra.deleted, r.notices)
		}
		if !gone && len(r.infra.closed)+len(r.notices) != 0 {
			t.Errorf("control: a clean push into a public pull request: closed %v, notices %+v", r.infra.closed, r.notices)
		}
	}
}

// TestTheSecretGateStillRefusesADocsChange (INVARIANT guard: docs_only adds
// refusals and removes none): a credential-shaped value added to a Markdown
// file is still the secret gate's 409.
func TestTheSecretGateStillRefusesADocsChange(t *testing.T) {
	r := docsRig(t)
	r.content("README.md", readmeBase, strings.Replace(readmeBase, "The end.", dToken, 1))
	r.docsFiles(File{Path: "README.md", Status: "modified", Changes: 2, Patch: "@@ -8,2 +8,2 @@\n \n-The end.\n+" + dToken})
	if err := r.create(goodCreate()); !isConflict(err) || !strings.Contains(err.Error(), "credential-shaped") {
		t.Fatalf("err = %v, want the secret gate's refusal", err)
	}
}

// TestCompareReadsEachCommitsMessageAndIdentities: the public scan reads
// what REST's comparison says about each commit — its message, its author
// and its committer — in commit order. Fixture values are pairwise distinct.
func TestCompareReadsEachCommitsMessageAndIdentities(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ahead", "total_commits": 2, "files": []any{},
			"commits": []any{
				map[string]any{"sha": oidA, "commit": map[string]any{"message": "first message",
					"author": map[string]any{"name": "Ann", "email": "ann@a.example.test"}, "committer": map[string]any{"name": "Cy", "email": "cy@c.example.test"}}},
				map[string]any{"sha": oidB, "commit": map[string]any{"message": "second message",
					"author": map[string]any{"name": "Bo", "email": "bo@b.example.test"}, "committer": map[string]any{"name": "Di", "email": "di@d.example.test"}}},
			}})
	}))
	defer srv.Close()
	c, err := restFor(srv).Compare(context.Background(), "trunk", dBranch)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"first message\nAnn <ann@a.example.test>\nCy <cy@c.example.test>", "second message\nBo <bo@b.example.test>\nDi <di@d.example.test>"}
	if len(c.CommitTexts) != 2 || c.CommitTexts[0] != want[0] || c.CommitTexts[1] != want[1] || c.Commits[1] != oidB {
		t.Fatalf("commit texts = %q (commits %v), want %q", c.CommitTexts, c.Commits, want)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func noticeTexts(r *draftRig) []string {
	var out []string
	for _, n := range r.notices {
		out = append(out, n.Summary)
		out = append(out, n.Paths...)
	}
	return out
}
