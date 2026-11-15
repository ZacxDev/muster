package officers_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// DOCS-ONLY DRAFTS AND PUBLIC REPOSITORIES: the configuration half
// (ship_docs.go). The gate half is internal/officership's docsgate_test.go.
// Every value is synthetic.
// ---------------------------------------------------------------------------

// docsShip is draftShip() with a docs_only block and example-web public.
func docsShip(t *testing.T) map[string]any {
	m := draftShip()
	m["docs_only"] = map[string]any{"allowed_paths": []string{"**/*.md", "claudedocs/**"}, "max_diff_lines": 120, "max_files": 4, "max_prs_per_day": 2}
	repos := m["repos"].([]any)
	repos[1].(map[string]any)["public"] = true
	m["internal_patterns_file"] = patternFile(t, "# synthetic\n\\.corp\\.example\\b\n198\\.51\\.100\\.\n")
	return m
}

func patternFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "internal-patterns.txt")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestParseReadsADocsOnlyDraftShip: the block, the per-repository public
// flag (on example-web only) and the pattern file are read as declared, and
// the per-day cap lowers the stage's.
func TestParseReadsADocsOnlyDraftShip(t *testing.T) {
	list, err := parseDecl(t, draftDecl(docsShip(t)), shipEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	s := list[0].Ship
	d := s.DocsOnly
	if d == nil || len(d.AllowedPaths) != 2 || d.MaxDiffLines != 120 || d.MaxFiles != 4 || d.MaxPRsPerDay != 2 {
		t.Fatalf("docs_only = %+v", d)
	}
	if s.NewPRsPerDay() != 2 {
		t.Fatalf("NewPRsPerDay = %d, want the configured 2", s.NewPRsPerDay())
	}
	web, _ := s.RepoFor(v2Web, v2Web)
	infra, _ := s.RepoFor(v2Web, v2Infra)
	if !web.Public || infra.Public {
		t.Fatalf("public: web %v infra %v, want only web", web.Public, infra.Public)
	}
	pats, err := s.InternalPatterns()
	if err != nil || len(pats) != 2 || pats[0].Line != 2 || pats[1].Line != 3 {
		t.Fatalf("patterns = %+v, %v", pats, err)
	}
	if !pats[0].Re.MatchString("db.CORP.example") {
		t.Fatal("a pattern is not case-insensitive")
	}
	// Control: without the block the stage's own limit stands.
	list, err = parseDecl(t, draftDecl(draftShip()), shipEnv(t))
	if err != nil || list[0].Ship.DocsOnly != nil || list[0].Ship.NewPRsPerDay() != officers.MaxNewPRsPerDay {
		t.Fatalf("no docs_only: %+v, %v", list[0].Ship.DocsOnly, err)
	}
}

// TestParseDefaultsTheDocsOnlyCaps: an unset cap is the default, never
// "unlimited".
func TestParseDefaultsTheDocsOnlyCaps(t *testing.T) {
	m := draftShip()
	m["docs_only"] = map[string]any{"allowed_paths": []string{"**/*.md"}}
	list, err := parseDecl(t, draftDecl(m), shipEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	d := list[0].Ship.DocsOnly
	if d.MaxDiffLines != officers.DefaultDocsMaxDiffLines || d.MaxFiles != officers.DefaultDocsMaxFiles || d.MaxPRsPerDay != officers.MaxNewPRsPerDay {
		t.Fatalf("defaults = %+v", d)
	}
}

// TestParseRefusesABadDocsOnlyShip: each refusal names what is wrong; the
// control is docsShip itself (above).
func TestParseRefusesABadDocsOnlyShip(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(t *testing.T, m map[string]any)
		want string
	}{
		"no allowed paths": {func(_ *testing.T, m map[string]any) {
			m["docs_only"] = map[string]any{"allowed_paths": []string{}}
		}, "allowed_paths needs 1.."},
		"allow everything": {func(_ *testing.T, m map[string]any) {
			m["docs_only"] = map[string]any{"allowed_paths": []string{"**/*.md", "**"}}
		}, "allows the code path"},
		"allow every basename": {func(_ *testing.T, m map[string]any) {
			m["docs_only"] = map[string]any{"allowed_paths": []string{"*"}}
		}, "allows the code path"},
		"a bad glob": {func(_ *testing.T, m map[string]any) {
			m["docs_only"] = map[string]any{"allowed_paths": []string{"docs/../x.md"}}
		}, "'..' segment"},
		"an unknown key": {func(_ *testing.T, m map[string]any) {
			m["docs_only"] = map[string]any{"allowed_paths": []string{"**/*.md"}, "allow_code": true}
		}, "unknown field"},
		// There is no switch that lifts the fenced-code and non-Markdown rules:
		// the removed allow_command_edits key is refused, never read.
		"allow_command_edits": {func(_ *testing.T, m map[string]any) {
			m["docs_only"] = map[string]any{"allowed_paths": []string{"**/*.md"}, "allow_command_edits": true}
		}, `unknown field "allow_command_edits"`},
		"a per-day cap above the stage's": {func(_ *testing.T, m map[string]any) {
			m["docs_only"] = map[string]any{"allowed_paths": []string{"**/*.md"}, "max_prs_per_day": officers.MaxNewPRsPerDay + 1}
		}, "can only lower"},
		"a negative diff cap": {func(_ *testing.T, m map[string]any) {
			m["docs_only"] = map[string]any{"allowed_paths": []string{"**/*.md"}, "max_diff_lines": -1}
		}, "max_diff_lines -1"},
		"public without patterns": {func(_ *testing.T, m map[string]any) { delete(m, "internal_patterns_file") }, "needs internal_patterns_file"},
		"patterns without public": {func(_ *testing.T, m map[string]any) {
			delete(m["repos"].([]any)[1].(map[string]any), "public")
		}, "none is"},
		"a relative pattern path": {func(_ *testing.T, m map[string]any) { m["internal_patterns_file"] = "scripts/patterns.txt" }, "absolute local path"},
		"a missing pattern file": {func(t *testing.T, m map[string]any) {
			m["internal_patterns_file"] = filepath.Join(t.TempDir(), "absent.txt")
		}, "could not be read"},
		"a pattern file with no pattern": {func(t *testing.T, m map[string]any) {
			m["internal_patterns_file"] = patternFile(t, "# only a comment\n\n")
		}, "has no pattern"},
		"a pattern that matches everything": {func(t *testing.T, m map[string]any) {
			m["internal_patterns_file"] = patternFile(t, "ok\n.*\n")
		}, "line 2 matches the empty string"},
		"a pattern that does not compile": {func(t *testing.T, m map[string]any) {
			m["internal_patterns_file"] = patternFile(t, "(\n")
		}, "line 1 does not compile"},
	} {
		m := docsShip(t)
		c.edit(t, m)
		_, err := parseDecl(t, draftDecl(m), shipEnv(t))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

// TestDocsOnlyAndPublicAreDraftOnly: another stage would ignore them, so each
// is a refusal there.
func TestDocsOnlyAndPublicAreDraftOnly(t *testing.T) {
	for _, key := range []string{"docs_only", "public", "internal_patterns_file"} {
		m := fullShip()
		switch key {
		case "docs_only":
			m[key] = map[string]any{"allowed_paths": []string{"**/*.md"}}
		case "public":
			m[key] = true
		default:
			m[key] = patternFile(t, "x\n")
		}
		_, err := parseShip(t, m, shipEnv(t))
		want := key + " is enforced by the draft and shepherd stages only"
		if key == "docs_only" {
			want = key + " is enforced by the draft stage only"
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s on stage %v: err = %v", key, m["stage"], err)
		}
	}
}

// TestAShepherdMayMarkARepositoryPublic (round 1 finding 6): a shepherd
// pushes into pull requests of the repositories it ships, so `public` and
// `internal_patterns_file` are read on the shepherd stage too and its pushes
// are scanned; docs_only stays draft-only, and public still needs a pattern
// file. Control: shepShip without either parses with no public repository.
func TestAShepherdMayMarkARepositoryPublic(t *testing.T) {
	m := shepShip()
	m["repos"].([]any)[0].(map[string]any)["public"] = true
	m["internal_patterns_file"] = patternFile(t, "\\.corp\\.example\\b\n")
	list, err := parseShep(t, shepAuthorDecl(), shepDecl(m))
	if err != nil {
		t.Fatalf("a public shepherd repository: %v", err)
	}
	s := byID(t, list, shepID).Ship
	if !s.Repos[0].Public {
		t.Fatal("the shepherd's repository is not read as public")
	}
	if pats, err := s.InternalPatterns(); err != nil || len(pats) != 1 {
		t.Fatalf("patterns = %+v, %v", pats, err)
	}
	delete(m, "internal_patterns_file")
	if _, err := parseShep(t, shepAuthorDecl(), shepDecl(m)); err == nil || !strings.Contains(err.Error(), "needs internal_patterns_file") {
		t.Fatalf("public without patterns on a shepherd: err = %v", err)
	}
	m = shepShip()
	m["docs_only"] = map[string]any{"allowed_paths": []string{"**/*.md"}}
	if _, err := parseShep(t, shepAuthorDecl(), shepDecl(m)); err == nil || !strings.Contains(err.Error(), "docs_only is enforced by the draft stage only") {
		t.Fatalf("docs_only on a shepherd: err = %v", err)
	}
	list, err = parseShep(t, shepAuthorDecl(), shepDecl(shepShip()))
	if err != nil || byID(t, list, shepID).Ship.Repos[0].Public {
		t.Fatalf("control: %v", err)
	}
}

// TestAPublicKickoffNamesTheBranch (round 1 finding 2): the PUBLIC note lists
// the branch name among what is scanned, since a branch name is public too.
func TestAPublicKickoffNamesTheBranch(t *testing.T) {
	list, err := parseDecl(t, draftDecl(docsShip(t)), shipEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	got := officers.KickoffText(list[0], 1)
	const want = "into a branch name, a diff, a commit message, your git author identity, a title, a body or a comment there: muster scans all of them, refuses a push whose branch name matches, deletes a pushed branch whose content matches"
	if !strings.Contains(got, want) {
		t.Fatalf("the PUBLIC note does not name the branch:\n%s", got)
	}
}

// TestMatchAllowedIsAnAllowlistMatcher: a no-slash pattern matches the file's
// OWN name only (not a directory named like a doc), and an unclean path
// matches nothing whatever the pattern.
func TestMatchAllowedIsAnAllowlistMatcher(t *testing.T) {
	for _, c := range []struct {
		pat, p string
		want   bool
	}{
		{"*.md", "README.md", true},
		{"*.md", "docs/deep/guide.md", true},
		{"*.md", "notes.md/main.go", false},
		{"**/*.md", "a/b.md", true},
		{"**/*.md", "b.md", true},
		{"**/*.md", "a.md/b.go", false},
		{"**/*.md", "a/../b.md", false},
		{"**/*.md", "./b.md", false},
		{"**/*.md", "a//b.md", false},
		{"**/*.md", "a\\b.md", false},
		{"**/*.md", "/abs/b.md", false},
		{"**/*.md", "a/b.md\n", false},
		{"claudedocs/**", "claudedocs/x/y.json", true},
		{"claudedocs/**", "claudedocs", false},
		{"claudedocs/**", "src/claudedocs/x.md", false},
		{"claudedocs/", "claudedocs/x.md", true},
		{".claude/skills/**/*.md", ".claude/skills/a/SKILL.md", true},
		{".claude/skills/**/*.md", ".claude/settings.json", false},
	} {
		if got := officers.MatchAllowed(c.pat, c.p); got != c.want {
			t.Errorf("MatchAllowed(%q, %q) = %v, want %v", c.pat, c.p, got, c.want)
		}
	}
}

// TestADocsOnlyKickoffForbidsCommandEdits: a docs-only draft officer's first
// message carries the whole DOCS ONLY note, its fenced-code and non-Markdown
// sentence included. No configuration drops that sentence: docs_only has no
// allow_command_edits.
func TestADocsOnlyKickoffForbidsCommandEdits(t *testing.T) {
	list, err := parseDecl(t, draftDecl(docsShip(t)), shipEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	const note = "\nDOCS ONLY: every path your pull request changes, adds, removes or renames from must match one of **/*.md, claudedocs/** (and no forbidden path); one path outside refuses the whole pull request. At most 4 files and 120 changed lines per pull request." +
		" Never change a line inside a fenced code block (a command) and never change a file that is not Markdown: muster refuses both. Propose a command change as text in the pull request body instead.\n"
	if got := officers.KickoffText(list[0], 1); !strings.Contains(got, note) {
		t.Fatalf("docs-only kickoff lacks the whole DOCS ONLY note:\n%s\n--- want it to contain ---\n%s", got, note)
	}
}
