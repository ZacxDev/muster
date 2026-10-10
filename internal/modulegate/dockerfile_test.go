package modulegate

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// THE CSS STAGE MUST SEE EVERY FILE TAILWIND SCANS.
//
// 🔴 TAILWIND DOES NOT ERROR ON A CONTENT PATH THAT IS NOT THERE. It emits
// nothing for it and exits 0 — so a stage that forgets a COPY produces a
// smaller, perfectly valid stylesheet and a page that renders unstyled IN THE
// CONTAINER while looking correct on a dev box, where the file is present.
// Measured on this tree with one glob pointed at a directory that does not
// exist: 7,139 valid bytes instead of ~38,000, exit 0, no warning.
//
// `make css-check` catches this AT BUILD TIME and the Dockerfile runs it — but
// only for the classes CSS_REQUIRED_CLASSES names. This test catches it at
// REVIEW time, structurally, for every entry: a content path added to
// tailwind.config.js without a matching COPY reddens here whether or not anyone
// remembered to add a control class for it.
//
// 🔴 WHAT THIS WOULD STILL ACCEPT: it compares PATHS, so a COPY that lands the
// right files in the wrong place inside the stage satisfies it. The build-time
// `make css-check` is what catches that, because the classes would go missing.
// The two checks fail for different reasons and neither subsumes the other.
// ---------------------------------------------------------------------------

// contentEntry pulls a quoted path out of tailwind.config.js's content array.
// A leading `!` marks a NEGATED entry, which excludes rather than scans and
// therefore needs no COPY.
var contentEntry = regexp.MustCompile(`^\s*'(!?)(\./[^']+)'`)

// copySource pulls the SOURCE operand out of a Dockerfile COPY. The last token
// is the destination; everything before it is a source.
var copyLine = regexp.MustCompile(`^\s*COPY\s+(.+)$`)

// TestTheCSSStageCopiesEveryTailwindContentPath is the structural half.
func TestTheCSSStageCopiesEveryTailwindContentPath(t *testing.T) {
	root := moduleRoot(t)

	wanted := tailwindContentPaths(t, filepath.Join(root, "tailwind.config.js"))
	// 🔴 POSITIVE CONTROL: THE PARSE MUST HAVE FOUND SOMETHING. An empty set of
	// content paths makes every assertion below vacuously true — which is
	// exactly the shape of green this whole test is about.
	if len(wanted) == 0 {
		t.Fatal("positive control FAILED: no content entries parsed from tailwind.config.js. " +
			"The config has at least two; this test is measuring nothing.")
	}

	copied := cssStageCopySources(t, filepath.Join(root, "Dockerfile"))
	if len(copied) == 0 {
		t.Fatal("positive control FAILED: no COPY sources parsed from the Dockerfile's " +
			"css stage. Every assertion below would fail for the wrong reason.")
	}
	t.Logf("controls: %d tailwind content path(s), %d COPY source(s) in the css stage: %s",
		len(wanted), len(copied), strings.Join(copied, " "))

	for _, want := range wanted {
		if !coveredByACopy(want, copied) {
			t.Errorf("tailwind.config.js scans %q and the Dockerfile's css stage never "+
				"COPYs it.\n"+
				"  Tailwind will not error: it emits a smaller, valid stylesheet and\n"+
				"  exits 0, and the page renders unstyled IN THE CONTAINER while looking\n"+
				"  correct on your machine.\n"+
				"  Add a COPY for it to the css stage.", want)
		}
	}
}

// TestTheCSSStageVerifiesItsOwnOutput pins that the stage does not merely BUILD
// the stylesheet but checks it — and that it does so by calling the target that
// owns the check rather than a second copy of it.
//
// 🔴 A STAGE THAT ONLY RUNS THE BUILD IS THE SILENT FAILURE, NOT A MITIGATION OF
// IT. `npm run build:css` exits 0 on a stylesheet with nothing in it. The
// assertion has to be on the OUTPUT.
//
// ⚠ IT PINS THE TARGET NAME, WHICH IS A SPELLING — a weaker guard than pinning
// behaviour, and it is stated as such. What makes it more than a spelling is
// that `make css-check` is itself exercised by every contributor and by the
// image build, so a rename that left this red is a rename somebody is already
// looking at. The behaviour is pinned where it belongs: in the Makefile, which
// carries its own controls.
func TestTheCSSStageVerifiesItsOwnOutput(t *testing.T) {
	root := moduleRoot(t)

	// 🔴 INSTRUCTIONS, NOT THE RAW STAGE, AND THIS LINE IS A FIX FOR A MEASURED
	// FAILURE OF THIS VERY TEST. The first version matched `make css-check`
	// against the stage text including its comments — and the comment three
	// lines above the RUN says "`make css-check`, NOT A HAND-ROLLED test -s". So
	// a mutant that replaced the RUN with a bare `npm run build:css` SURVIVED: the
	// guard was satisfied by the prose explaining the guard. That is a guard
	// pinning a SPELLING rather than an instruction, and the only reason it was
	// caught is that the mutation was run.
	instr := cssStageInstructions(t, filepath.Join(root, "Dockerfile"))

	// 🔴 POSITIVE CONTROL: comment-stripping must not have eaten everything.
	if len(instr) == 0 {
		t.Fatal("positive control FAILED: the css stage has no instruction lines once " +
			"comments are stripped; every assertion below is vacuous")
	}

	ranCheck := false
	for _, line := range instr {
		if strings.HasPrefix(strings.TrimSpace(line), "RUN ") && strings.Contains(line, "make css-check") {
			ranCheck = true
		}
	}
	if !ranCheck {
		t.Fatalf("no RUN instruction in the Dockerfile's css stage executes `make css-check`.\n"+
			"  Building the stylesheet is not checking it — Tailwind exits 0 on a\n"+
			"  stylesheet that scanned nothing. `make css-check` asserts the control\n"+
			"  classes are present AND that the committed copy matches; it is the one\n"+
			"  place that predicate lives, and a second copy here would be the one\n"+
			"  that rots.\n  instructions: %v", instr)
	}

	// The target needs bash (the Makefile sets SHELL := /usr/bin/env bash) and
	// make itself. Alpine has neither by default, and the failure without them
	// says nothing about CSS.
	joined := strings.Join(instr, "\n")
	for _, pkg := range []string{"make", "bash"} {
		if !regexp.MustCompile(`apk add[^\n]*\b` + pkg + `\b`).MatchString(joined) {
			t.Errorf("the css stage runs `make css-check` but never installs %q. "+
				"node:20-alpine ships neither make nor bash.", pkg)
		}
	}
}

// tailwindContentPaths reads the non-negated entries of the `content` array.
func tailwindContentPaths(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var out []string
	inContent := false
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "content: [") {
			inContent = true
			continue
		}
		if inContent && strings.TrimSpace(line) == "]," {
			break
		}
		if !inContent {
			continue
		}
		m := contentEntry.FindStringSubmatch(line)
		if m == nil || m[1] == "!" {
			continue
		}
		out = append(out, m[2])
	}
	sort.Strings(out)
	return out
}

// cssStage returns the text of the Dockerfile stage named `css`.
func cssStage(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	lines := strings.Split(string(b), "\n")
	var stage []string
	in := false
	for _, line := range lines {
		if strings.HasPrefix(line, "FROM ") {
			in = strings.HasSuffix(strings.TrimSpace(line), " AS css")
			continue
		}
		if in {
			stage = append(stage, line)
		}
	}
	if len(stage) == 0 {
		t.Fatalf("no `FROM … AS css` stage in %s", path)
	}
	return strings.Join(stage, "\n")
}

// cssStageInstructions returns the css stage's lines with comments and blanks
// removed, so a check on what the stage DOES cannot be satisfied by prose about
// what it does.
func cssStageInstructions(t *testing.T, path string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(cssStage(t, path), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// cssStageCopySources lists every SOURCE operand of a COPY in the css stage.
func cssStageCopySources(t *testing.T, path string) []string {
	t.Helper()
	// Instructions, not the raw stage: a COPY written inside a comment is prose,
	// and this test is about what the build does. Same reason as
	// TestTheCSSStageVerifiesItsOwnOutput, which was measured wrong the other way.
	var out []string
	for _, line := range cssStageInstructions(t, path) {
		m := copyLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		fields := strings.Fields(m[1])
		// Drop --from=… and friends, and drop the destination (the last field).
		var srcs []string
		for _, f := range fields {
			if !strings.HasPrefix(f, "--") {
				srcs = append(srcs, f)
			}
		}
		if len(srcs) < 2 {
			continue
		}
		out = append(out, srcs[:len(srcs)-1]...)
	}
	sort.Strings(out)
	return out
}

// coveredByACopy reports whether a tailwind content path is inside something the
// stage copies.
//
// A glob like `./internal/ui/**/*.go` is covered by `internal/ui/`; a file entry
// like `./internal/api/login.go` is covered by exactly that file or by a
// directory above it.
func coveredByACopy(contentPath string, copied []string) bool {
	p := strings.TrimPrefix(contentPath, "./")
	// Cut the glob off at the first wildcard segment, leaving the directory
	// prefix Tailwind will walk.
	if i := strings.IndexAny(p, "*?["); i >= 0 {
		p = p[:i]
	}
	p = strings.TrimSuffix(p, "/")
	for _, c := range copied {
		c = strings.TrimSuffix(strings.TrimPrefix(c, "./"), "/")
		if c == "" || c == "." {
			return true // the whole context
		}
		if p == c || strings.HasPrefix(p+"/", c+"/") {
			return true
		}
	}
	return false
}
