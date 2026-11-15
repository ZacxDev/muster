package officership

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// THE DOCS-ONLY AND PUBLIC-REPOSITORY RULES of a draft officer's comparison
// (officers.DocsOnly, officers.ShipRepo.Public; docs/officers.md "Docs-only
// drafts"). checkWall runs them on every comparison it reads for a draft
// officer, so they hold at create, at every PATCH, after every push and on
// every sweep, exactly like the forbidden paths.
//
//   - docs_only: every changed path AND every renamed-from path must be clean
//     (officers.CleanRepoPath) and match an allowed glob
//     (officers.MatchAllowed). ONE path outside refuses the whole pull
//     request: there is no partial admission. Then (no switch lifts this):
//     a changed file that is not Markdown is refused whole, and so is a Markdown file one of whose changed lines (added or
//     removed) sits inside a fenced code block of the file it belongs to —
//     read whole at the base (removed lines) and at the head (added lines),
//     so a hunk that starts inside a fence is judged right. And the size
//     caps: max_files changed files, max_diff_lines added+removed lines.
//   - public: the added lines, the new paths, and every commit's message and
//     identities are scanned with the officer's internal-details patterns,
//     read fresh from muster-server's disk. A refusal names the pattern's
//     LINE NUMBER, never the pattern or what it matched.
//
// FAIL CLOSED: a file that cannot be read for the fence rule, or a pattern
// file that cannot be read, refuses (unverified: an open pull request is
// closed with its branch kept, and the sweep retries; a public branch with NO
// pull request is deleted, since nothing else reads it again —
// publicBranchCheck). A public repository's branch NAME is scanned before
// the push is forwarded (allowedRef). A shepherd runs the public half on its
// own pushes (it has no docs_only).
// ---------------------------------------------------------------------------

// docsVerdict is the docs-only and public rules' answer over one comparison.
type docsVerdict struct {
	paths []string
	whys  []string
	// unverified: every refusal is "could not check", none names content.
	unverified bool
}

func (d *docsVerdict) add(p, why string) {
	d.paths = append(d.paths, p)
	d.whys = append(d.whys, p+": "+why)
}

// docsWall is the docs-only and public rules of sr over cmp (base...head).
func (g *PushGate) docsWall(ctx context.Context, sr officers.ShipRepo, gh GitHub, cmp Compare, base, head string) docsVerdict {
	var v docsVerdict
	content, unchecked := 0, 0
	hit := func(p, why string) { v.add(p, why); content++ }
	miss := func(p, why string) { v.add(p, why); unchecked++ }
	if d := g.Ship.DocsOnly; d != nil {
		if n := len(cmp.Files); n > d.MaxFiles {
			for _, f := range cmp.Files {
				hit(f.Path, fmt.Sprintf("the pull request changes %d files; docs_only allows at most %d", n, d.MaxFiles))
			}
		}
		total := 0
		for _, f := range cmp.Files {
			total += f.Changes
		}
		if total > d.MaxDiffLines {
			for _, f := range cmp.Files {
				hit(f.Path, fmt.Sprintf("the pull request changes %d lines; docs_only allows at most %d", total, d.MaxDiffLines))
			}
		}
		for _, f := range cmp.Files {
			outside := false
			for i, p := range []string{f.Path, f.Previous} {
				if i == 1 && p == "" {
					continue // not a rename or a copy
				}
				switch {
				case !officers.CleanRepoPath(p):
					hit(orEmpty(p), "not a clean repository path (an empty, '.' or '..' segment, a backslash or a control character)")
					outside = true
				case !allowedAny(d.AllowedPaths, p):
					hit(p, "outside docs_only.allowed_paths (a docs-only officer changes docs only, and one such path refuses the whole pull request)")
					outside = true
				}
			}
			if outside {
				continue
			}
			if !isMarkdown(f.Path) || (f.Previous != "" && !isMarkdown(f.Previous)) {
				hit(f.Path, "not a Markdown file: its whole content is treated as commands, which docs_only does not let an officer edit")
				continue
			}
			fenced, err := fencedChange(ctx, gh, cmp, f, base, head)
			switch {
			case err != nil:
				miss(f.Path, fmt.Sprintf("its fenced code blocks could not be checked: %v", err))
			case fenced > 0:
				hit(f.Path, fmt.Sprintf("line %d changes a fenced code block (a command), which docs_only does not let an officer edit", fenced))
			}
		}
	}
	if sr.Public {
		pats, err := g.Ship.InternalPatterns()
		if err != nil {
			for _, f := range cmp.Files {
				miss(f.Path, fmt.Sprintf("this public repository's internal-details scan could not run: %v", err))
			}
			if len(cmp.Files) == 0 {
				miss("(the comparison)", fmt.Sprintf("this public repository's internal-details scan could not run: %v", err))
			}
		} else {
			for _, f := range cmp.Files {
				if n := internalMatch(pats, f.Path); n > 0 {
					hit(f.Path, fmt.Sprintf("its path matches internal-details pattern line %d (a public repository)", n))
				}
				var added []string
				for _, l := range patchLines(f.Patch) {
					if l.op == '+' {
						added = append(added, l.text)
					}
				}
				if n := internalMatch(pats, strings.Join(added, "\n")); n > 0 {
					hit(f.Path, fmt.Sprintf("an added line matches internal-details pattern line %d (a public repository)", n))
				}
			}
			for i, t := range cmp.CommitTexts {
				if n := internalMatch(pats, t); n > 0 {
					sha := ""
					if i < len(cmp.Commits) {
						sha = shortSHA(cmp.Commits[i])
					}
					hit("commit "+sha, fmt.Sprintf("its message or author matches internal-details pattern line %d (a public repository)", n))
				}
			}
		}
	}
	v.unverified = unchecked > 0 && content == 0
	return v
}

// publicTextWhy is why text (a title, a body, a comment) may not go to sr, ""
// when it may: sr is public and text matches an internal-details pattern, or
// the patterns cannot be read (fail closed).
func (g *PushGate) publicTextWhy(sr officers.ShipRepo, what, text string) string {
	if !sr.Public {
		return ""
	}
	pats, err := g.Ship.InternalPatterns()
	if err != nil {
		return fmt.Sprintf("%s could not be scanned for internal details (a public repository): %v", what, err)
	}
	if n := internalMatch(pats, text); n > 0 {
		return fmt.Sprintf("%s matches internal-details pattern line %d (a public repository)", what, n)
	}
	return ""
}

// internalMatch is the line number of the first pattern text matches, 0 for
// none.
func internalMatch(pats []officers.InternalPattern, text string) int {
	for _, p := range pats {
		if p.Re.MatchString(text) {
			return p.Line
		}
	}
	return 0
}

func allowedAny(globs []string, p string) bool {
	for _, g := range globs {
		if officers.MatchAllowed(g, p) {
			return true
		}
	}
	return false
}

func orEmpty(p string) string {
	if p == "" {
		return "(an empty path)"
	}
	return strconv.Quote(p)
}

// isMarkdown reports whether p names a Markdown file (.md, .markdown; not
// .mdx, which holds code).
func isMarkdown(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".md", ".markdown":
		return true
	}
	return false
}

var hunkRE = regexp.MustCompile(`^@@ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@`)

// changedLines are the line numbers a unified diff removes (in the old file)
// and adds (in the new one). A patch whose hunks cannot be read is an error.
func changedLines(patch string) (removed, added []int, err error) {
	oldN, newN, inHunk := 0, 0, false
	for _, l := range strings.Split(patch, "\n") {
		if strings.HasPrefix(l, "@@") {
			m := hunkRE.FindStringSubmatch(l)
			if m == nil {
				return nil, nil, fmt.Errorf("a hunk header muster cannot read")
			}
			oldN, _ = strconv.Atoi(m[1])
			newN, _ = strconv.Atoi(m[3])
			inHunk = true
			continue
		}
		if strings.HasPrefix(l, "\\") {
			continue
		}
		if !inHunk {
			if l == "" {
				continue
			}
			return nil, nil, fmt.Errorf("diff lines before any hunk header")
		}
		switch {
		case strings.HasPrefix(l, "-"):
			removed = append(removed, oldN)
			oldN++
		case strings.HasPrefix(l, "+"):
			added = append(added, newN)
			newN++
		default:
			oldN++
			newN++
		}
	}
	return removed, added, nil
}

// fencedChange is the first changed line of f (removed lines numbered in the
// old file, added lines in the new) that is inside, or is, a fenced code
// block of its file, 0 when none is. The old file is read at the
// comparison's merge base (base when GitHub names none), the new at head. A
// removed file GitHub shows no diff for (an empty or binary file) counts as
// removing every line it had.
func fencedChange(ctx context.Context, gh GitHub, cmp Compare, f File, base, head string) (int, error) {
	at := cmp.MergeBase
	if at == "" {
		at = base
	}
	oldPath := f.Path
	if f.Previous != "" {
		oldPath = f.Previous
	}
	read := func(p, ref string) ([]bool, error) {
		raw, err := gh.FileAt(ctx, p, ref)
		if err != nil {
			return nil, fmt.Errorf("%s could not be read at %s: %v", p, shortSHA(ref), err)
		}
		return fenceMask(string(raw)), nil
	}
	if f.Patch == "" {
		if f.Status != "removed" {
			return 0, fmt.Errorf("GitHub shows no diff for it")
		}
		mask, err := read(oldPath, at)
		if err != nil {
			return 0, err
		}
		for n := 1; n < len(mask); n++ {
			if mask[n] {
				return n, nil
			}
		}
		return 0, nil
	}
	removed, added, err := changedLines(f.Patch)
	if err != nil {
		return 0, err
	}
	for _, side := range []struct {
		lines   []int
		p, ref  string
		missing bool
	}{{removed, oldPath, at, f.Status == "added"}, {added, f.Path, head, f.Status == "removed"}} {
		if len(side.lines) == 0 {
			continue
		}
		if side.missing {
			return side.lines[0], fmt.Errorf("the diff changes lines of a side the file does not have (%s)", f.Status)
		}
		mask, err := read(side.p, side.ref)
		if err != nil {
			return 0, err
		}
		for _, n := range side.lines {
			// 🔴 A line the file does not have is not "outside every fence":
			// the diff and the file disagree, so it counts as fenced.
			if n < 1 || n >= len(mask) || mask[n] {
				return n, nil
			}
		}
	}
	return 0, nil
}

// fenceMask is, for each line of a Markdown text (1-based: mask[0] is
// unused), whether it is inside a fenced code block or is a fence line.
//
// 🔴 FOUR READINGS, UNIONED: strict (a fence indented at most three spaces,
// after any blockquote markers) or loose (any indentation: a fence inside a
// list item or an indented block), each either container-AWARE or
// container-BLIND. A line any reading puts in a fence is fenced, so each
// reading can only ADD marks. One reading alone can be walked out of sync by
// a line another reads differently (an indented "```" the strict reading
// ignores and the loose one opens on, or an unindented line that ends a list
// item and the fence it opened), leaving the next real block's lines outside;
// the union cannot be, because each reading on its own still marks every line
// it believes fenced.
//   - container-aware: a fence opens after any run of blockquote markers AND
//     list markers ("- ", "* ", "+ ", "1. ", "1) "), so "- ```sh" opens one;
//     it closes only on a fence line at the SAME blockquote depth that carries
//     NO list marker ("> ```" inside an unquoted fence is content, and
//     "- ```" starts a new list item, so neither closes it).
//   - container-blind: list markers are not stripped (so "- ```sh" opens
//     nothing) and a fence line at any blockquote depth closes. It is the one
//     that marks "rm -rf /" in "- ```sh / safe / ``` / rm -rf / / ```": there
//     "safe" ends the list item and its fence, and the next "```" opens a
//     block, which the container-aware reading reads as a close.
//
// 🔴 OVER-MARKING IS THE SAFE ERROR, AND IT IS PAID. A line marked fenced that
// CommonMark calls prose costs a false refusal; the reverse admits a command
// edit. No single reading errs toward fenced everywhere: each misreads some
// shape in both directions, and only the union is never narrower than any one
// of them. The price is real: the container-blind reading reads the indented
// closer of a list item's fence as an opener, so prose after one is refused.
//
// A leading YAML front matter block (line 1 "---", through the next "---", or
// to the end when none closes it) is marked whole: an agent's tools: or a
// skill's allowed-tools: is configuration, not prose. YAML's "..." does not
// close it here: a reader that closes only on "---" would read the lines
// after "..." as front matter.
func fenceMask(text string) []bool {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	mask := make([]bool, len(lines)+1)
	if len(lines) > 0 && strings.TrimRight(strings.TrimPrefix(lines[0], "\ufeff"), " \t") == "---" {
		mask[1] = true
		for i := 1; i < len(lines); i++ {
			mask[i+1] = true
			if strings.TrimRight(lines[i], " \t") == "---" {
				break
			}
		}
	}
	for _, rd := range []struct{ loose, aware bool }{{false, true}, {true, true}, {false, false}, {true, false}} {
		var fch byte
		flen, fdepth := 0, 0
		for i, l := range lines {
			f, ok := fenceLine(l, rd.loose, rd.aware)
			if fch == 0 {
				if ok {
					fch, flen, fdepth = f.c, f.n, f.depth
					mask[i+1] = true
				}
				continue
			}
			mask[i+1] = true
			if ok && f.c == fch && f.n >= flen && (!rd.aware || (f.depth == fdepth && !f.listed)) && strings.TrimSpace(f.rest) == "" {
				fch, flen, fdepth = 0, 0, 0
			}
		}
	}
	return mask
}

// fence is a line read as a fence: its character ('`' or '~'), the run's
// length (at least 3), what follows the run, how many blockquote markers
// precede it and whether a list marker does.
type fence struct {
	c      byte
	n      int
	rest   string
	depth  int
	listed bool
}

// listMarkerRE is a CommonMark list marker and the one space or tab after it.
var listMarkerRE = regexp.MustCompile(`^(?:[-*+]|[0-9]{1,9}[.)])[ \t]`)

// fenceLine reads l as a fence. Blockquote '>' markers are stripped first,
// and, when aware, list markers too, in any order and any number; then
// strict allows at most three spaces of indentation, loose any.
func fenceLine(l string, loose, aware bool) (fence, bool) {
	s, depth, listed := l, 0, false
	for {
		t := strings.TrimLeft(s, " \t")
		if strings.HasPrefix(t, ">") {
			s, depth = t[1:], depth+1
			continue
		}
		if m := listMarkerRE.FindString(t); aware && m != "" {
			s, listed = t[len(m):], true
			continue
		}
		break
	}
	indent := len(s) - len(strings.TrimLeft(s, " \t"))
	if !loose && (indent > 3 || strings.Contains(s[:indent], "\t")) {
		return fence{}, false
	}
	s = s[indent:]
	if len(s) < 3 || (s[0] != '`' && s[0] != '~') {
		return fence{}, false
	}
	c, n := s[0], 0
	for n < len(s) && s[n] == c {
		n++
	}
	if n < 3 {
		return fence{}, false
	}
	return fence{c: c, n: n, rest: s[n:], depth: depth, listed: listed}, true
}
