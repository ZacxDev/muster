package officers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// DOCS-ONLY DRAFTS AND PUBLIC REPOSITORIES (docs/officers.md "Docs-only
// drafts").
//
// Two additions to the `draft` stage, each enforced by internal/officership's
// draft gate on the comparison of every pull request (at create, at every
// PATCH, after every push and on every sweep):
//
//   - `docs_only`: an ALLOWLIST. Every path a pull request changes, adds,
//     removes or renames from must match one of `allowed_paths` (and still
//     match no forbidden path); one path outside it refuses the WHOLE pull
//     request. Unless `allow_command_edits`, no changed line may sit inside a
//     fenced code block, and a changed file that is not Markdown is refused
//     whole. `max_diff_lines`, `max_files` and `max_prs_per_day` cap it.
//   - `public: true` on a repository: the diff's added lines, its new paths,
//     its commit messages and the pull request's title, body and comments
//     are scanned with the patterns of `internal_patterns_file` — a file on
//     muster-server's disk, never in any repository muster ships, so the
//     patterns (which name what is internal) stay private.
//
// 🔴 NEITHER WIDENS ANYTHING: both only add refusals to the draft stage's,
// and both are refused on every other stage rather than accepted and ignored.
// ---------------------------------------------------------------------------

// Defaults and bounds of a docs_only block. None widens: each is a cap.
const (
	DefaultDocsMaxDiffLines = 300
	DefaultDocsMaxFiles     = 10
	maxDocsDiffLines        = 5000
	maxDocsFiles            = 100
	maxAllowedPaths         = 64
	maxInternalPatterns     = 512
)

// DocsOnly is a draft officer's docs-only policy (its `docs_only` block).
type DocsOnly struct {
	// AllowedPaths are globs; every path a pull request touches must match
	// one (MatchAllowed).
	AllowedPaths []string
	// AllowCommandEdits lets a changed line sit inside a fenced code block,
	// and a changed file be other than Markdown.
	AllowCommandEdits bool
	// MaxDiffLines caps the comparison's added+removed lines; MaxFiles its
	// changed files; MaxPRsPerDay the pull requests opened per UTC day
	// (never above MaxNewPRsPerDay).
	MaxDiffLines int
	MaxFiles     int
	MaxPRsPerDay int
}

// codeProbes are paths no docs-only allowlist entry may match: a sanity check
// against an entry (`*`, `**`, `**/*`) that would allow everything.
var codeProbes = []string{"main.go", "cmd/app/main.go", "Makefile", "scripts/run.sh", ".github/workflows/ci.yml", "src/index.ts"}

type declaredDocsOnly struct {
	AllowedPaths      []string `json:"allowed_paths"`
	AllowCommandEdits bool     `json:"allow_command_edits"`
	MaxDiffLines      int      `json:"max_diff_lines"`
	MaxFiles          int      `json:"max_files"`
	MaxPRsPerDay      int      `json:"max_prs_per_day"`
}

// NewPRsPerDay is how many pull requests the officer may open per UTC day:
// docs_only's max_prs_per_day when set, else MaxNewPRsPerDay.
func (s *Ship) NewPRsPerDay() int {
	if s != nil && s.DocsOnly != nil && s.DocsOnly.MaxPRsPerDay > 0 {
		return s.DocsOnly.MaxPRsPerDay
	}
	return MaxNewPRsPerDay
}

// parseDocsOnly reads a `docs_only` block.
func parseDocsOnly(raw json.RawMessage) (*DocsOnly, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var d declaredDocsOnly
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("ship: docs_only: %w", err)
	}
	o := &DocsOnly{AllowCommandEdits: d.AllowCommandEdits, MaxDiffLines: d.MaxDiffLines, MaxFiles: d.MaxFiles, MaxPRsPerDay: d.MaxPRsPerDay}
	if n := len(d.AllowedPaths); n < 1 || n > maxAllowedPaths {
		return nil, fmt.Errorf("ship: docs_only.allowed_paths needs 1..%d globs, got %d: an allowlist with nothing on it has no default", maxAllowedPaths, n)
	}
	seen := map[string]bool{}
	for _, p := range d.AllowedPaths {
		p = strings.TrimSpace(p)
		if _, err := compileGlob(p); err != nil {
			return nil, fmt.Errorf("ship: docs_only.allowed_paths entry %q: %v", p, err)
		}
		for _, probe := range codeProbes {
			if MatchAllowed(p, probe) {
				return nil, fmt.Errorf("ship: docs_only.allowed_paths entry %q allows the code path %s: a docs-only allowlist names docs", p, probe)
			}
		}
		if !seen[p] {
			seen[p] = true
			o.AllowedPaths = append(o.AllowedPaths, p)
		}
	}
	if o.MaxDiffLines == 0 {
		o.MaxDiffLines = DefaultDocsMaxDiffLines
	}
	if o.MaxDiffLines < 1 || o.MaxDiffLines > maxDocsDiffLines {
		return nil, fmt.Errorf("ship: docs_only.max_diff_lines %d is not 1..%d", d.MaxDiffLines, maxDocsDiffLines)
	}
	if o.MaxFiles == 0 {
		o.MaxFiles = DefaultDocsMaxFiles
	}
	if o.MaxFiles < 1 || o.MaxFiles > maxDocsFiles {
		return nil, fmt.Errorf("ship: docs_only.max_files %d is not 1..%d", d.MaxFiles, maxDocsFiles)
	}
	if o.MaxPRsPerDay == 0 {
		o.MaxPRsPerDay = MaxNewPRsPerDay
	}
	if o.MaxPRsPerDay < 1 || o.MaxPRsPerDay > MaxNewPRsPerDay {
		return nil, fmt.Errorf("ship: docs_only.max_prs_per_day %d is not 1..%d (it can only lower the draft stage's limit)", d.MaxPRsPerDay, MaxNewPRsPerDay)
	}
	return o, nil
}

// MatchAllowed reports whether repository path p matches allowed-path glob
// pat. The glob syntax is MatchForbidden's, with ONE difference that matters
// for an allowlist: a pattern with no '/' matches the LAST segment only (the
// file's own name), never any segment — so `*.md` allows `docs/a.md` and not
// `notes.md/main.go`. A pattern ending in '/' is a directory: anything under
// it. A path that is not clean (CleanRepoPath) matches nothing.
func MatchAllowed(pat, p string) bool {
	if !CleanRepoPath(p) {
		return false
	}
	re, err := compileGlob(pat)
	if err != nil {
		return false
	}
	if !strings.Contains(pat, "/") {
		return re.MatchString(path.Base(p))
	}
	return re.MatchString(p)
}

// CleanRepoPath reports whether p is a clean repository-relative path: not
// empty, no leading or trailing '/', no empty, '.' or '..' segment, no
// backslash and no control character. git stores no other kind, so anything
// else is a trick and is refused rather than interpreted.
func CleanRepoPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// InternalPattern is one compiled line of an internal-details pattern file;
// Line is its 1-based line number (what a refusal names: never the pattern,
// never the text it matched).
type InternalPattern struct {
	Line int
	Re   *regexp.Regexp
}

// InternalPatterns reads the officer's internal_patterns_file FRESH (an
// operator may update it in place) and compiles it. An unset file, one that
// cannot be read, a line that is not a pattern, or a file with none is an
// error: the caller refuses rather than scans with nothing.
func (s *Ship) InternalPatterns() ([]InternalPattern, error) {
	if s == nil || s.InternalPatternsFile == "" {
		return nil, errors.New("no internal_patterns_file is configured")
	}
	raw, err := os.ReadFile(s.InternalPatternsFile)
	if err != nil {
		return nil, fmt.Errorf("the internal-details pattern file could not be read: %v", err)
	}
	return ParseInternalPatterns(string(raw))
}

// ParseInternalPatterns reads an internal-details pattern file: one RE2
// regular expression per line, surrounding space trimmed, blank lines and
// lines starting with '#' skipped. Matching is case-insensitive. A line that
// does not compile, or matches the empty string (it would match everything),
// is an error naming its line number; so is a file with no pattern.
func ParseInternalPatterns(content string) ([]InternalPattern, error) {
	var out []InternalPattern
	for i, l := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		re, err := regexp.Compile("(?i)" + l)
		if err != nil {
			return nil, fmt.Errorf("internal-details pattern line %d does not compile: %v", i+1, err)
		}
		if re.MatchString("") {
			return nil, fmt.Errorf("internal-details pattern line %d matches the empty string, so it would match everything", i+1)
		}
		out = append(out, InternalPattern{Line: i + 1, Re: re})
		if len(out) > maxInternalPatterns {
			return nil, fmt.Errorf("the internal-details pattern file has more than %d patterns", maxInternalPatterns)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("the internal-details pattern file has no pattern")
	}
	return out, nil
}

// validPatternFilePath reports whether f may name an internal_patterns_file:
// an absolute, clean local path.
func validPatternFilePath(f string) bool {
	return strings.HasPrefix(f, "/") && CleanRepoPath(strings.TrimPrefix(f, "/"))
}

// parseDocsAndPublic reads the docs_only block and internal_patterns_file of
// a ship block whose repositories (with their `public` flags) are parsed, and
// refuses either on any stage but draft.
func parseDocsAndPublic(s *Ship, d declaredShip) error {
	hasDocs := len(d.DocsOnly) > 0 && string(d.DocsOnly) != "null"
	anyPublic := false
	for _, r := range s.Repos {
		anyPublic = anyPublic || r.Public
	}
	f := strings.TrimSpace(d.InternalPatternsFile)
	if s.Stage != StageDraft {
		for name, set := range map[string]bool{"docs_only": hasDocs, "public": anyPublic, "internal_patterns_file": f != ""} {
			if set {
				return fmt.Errorf("ship: %s is enforced by the %s stage only; stage %s would ignore it — remove it", name, StageDraft, s.Stage)
			}
		}
		return nil
	}
	if hasDocs {
		o, err := parseDocsOnly(d.DocsOnly)
		if err != nil {
			return err
		}
		s.DocsOnly = o
	}
	if f != "" && !validPatternFilePath(f) {
		return fmt.Errorf("ship: internal_patterns_file %q must be an absolute local path on muster-server (never a repository file)", d.InternalPatternsFile)
	}
	if anyPublic && f == "" {
		return fmt.Errorf("ship: a repository marked public needs internal_patterns_file: the internal-details scanner has no default patterns")
	}
	if f != "" && !anyPublic {
		return fmt.Errorf("ship: internal_patterns_file is read for repositories marked public only, and none is — remove it or mark one")
	}
	s.InternalPatternsFile = f
	if f != "" {
		// Read once at boot so a missing or broken file is a boot refusal;
		// the gate reads it again at every check.
		if _, err := s.InternalPatterns(); err != nil {
			return fmt.Errorf("ship: internal_patterns_file %s: %v", f, err)
		}
	}
	return nil
}
