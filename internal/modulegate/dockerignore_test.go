package modulegate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// THE BUILD CONTEXT MUST NOT CARRY WHAT THE BUILD DOES NOT NEED — AND MUST
// STILL CARRY WHAT IT DOES.
//
// 🔴 THE FINDING: there was no .dockerignore, and the Dockerfile's build stage
// is `COPY . .`. So the whole of .git, plus any local node_modules/, .env or
// kubeconfig sitting in a developer's tree, was uploaded as build context and
// written into an intermediate layer.
//
// ⚠ THE SHIPPED IMAGE WAS NEVER AFFECTED, AND OVERSTATING THAT WOULD BE THE
// SAME KIND OF ERROR THIS FILE EXISTS TO CATCH. Stage 3 is distroless/static
// and copies exactly three binaries out of the build stage, so nothing above
// ever reached a published layer. This is build time and local-cache hygiene,
// not an artifact leak.
//
// 🔴 THE SECOND DIRECTION IS THE DANGEROUS ONE, AND IT IS WHY THIS TEST IS NOT
// JUST `test -f .dockerignore`. An entry that excludes a BUILD INPUT does not
// fail here — it fails inside the image build, with an error naming a Go
// package or a missing embed, nowhere near the line that caused it.
// .gitignore's own first line records that exact failure from the project
// muster was carved out of. So this checks both: .git is excluded, and nothing
// the Dockerfile explicitly COPYs is.
//
// ⚠ IT IS NOT A .dockerignore PATTERN ENGINE. It compares literal paths and
// prefixes, which covers every entry this file actually has. A guard that tried
// to reimplement Docker's matcher would be a second copy of a predicate, and
// wrong in the direction that reads as safe.
// ---------------------------------------------------------------------------

// dockerignoreEntries returns the file's meaningful lines: comments and blanks
// stripped.
//
// 🔴 THE COMMENT STRIP IS LOAD-BEARING, NOT TIDINESS. The previous round's
// Dockerfile guard was satisfied by the COMMENT explaining the thing it was
// checking for — a check that passes on its own documentation is a check that
// measures nothing. This file is mostly prose, so a substring search over the
// raw bytes would match every entry name from the header alone.
func dockerignoreEntries(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read .dockerignore: %v\n"+
			"    The Dockerfile's build stage is `COPY . .`, so without this file the "+
			"whole of .git and any local .env / kubeconfig / node_modules in the tree "+
			"are uploaded as build context.", err)
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// TestTheBuildContextExcludesGitAndKeepsEveryBuildInput is the gate.
func TestTheBuildContextExcludesGitAndKeepsEveryBuildInput(t *testing.T) {
	root := moduleRoot(t)
	entries := dockerignoreEntries(t, filepath.Join(root, ".dockerignore"))

	// 🔴 POSITIVE CONTROL. A .dockerignore of nothing but the header above would
	// yield zero entries, and every assertion below would pass over an empty
	// set — the shape that let a comment satisfy the previous round's guard.
	if len(entries) == 0 {
		t.Fatal("positive control FAILED: .dockerignore has no non-comment lines at all. " +
			"It is pure prose, which excludes nothing, and every check below would be " +
			"green over an empty set.")
	}

	// --- direction 1: .git must be out of the context -----------------------
	git := false
	for _, e := range entries {
		if e == ".git" || e == ".git/" || e == "**/.git" {
			git = true
		}
	}
	if !git {
		t.Errorf(".dockerignore does not exclude .git (entries: %v).\n"+
			"    The build stage is `COPY . .`, so every object, branch and commit "+
			"message is uploaded and lands in an intermediate layer. It is also the "+
			"single largest item in the context, and nothing in the Dockerfile reads "+
			"git history — VERSION arrives as a build arg precisely so it need not.",
			entries)
	}

	// --- direction 2: no build INPUT may be excluded -------------------------
	//
	// The css stage's COPY sources are the explicit, named build inputs. `COPY
	// . .` in the build stage is deliberately not consulted: its source is the
	// whole context, so every entry would "match" it and the check would be
	// unsatisfiable by construction.
	inputs := cssStageCopySources(t, filepath.Join(root, "Dockerfile"))
	inputs = append(inputs, "go.mod", "go.sum")
	if len(inputs) <= 2 {
		t.Fatal("positive control FAILED: no COPY sources were parsed out of the " +
			"Dockerfile's css stage, so direction 2 is comparing against nothing")
	}
	for _, in := range inputs {
		clean := strings.TrimPrefix(strings.TrimSuffix(in, "/"), "./")
		if clean == "" || clean == "." {
			continue
		}
		for _, e := range entries {
			if strings.HasPrefix(e, "!") {
				continue // a negation re-includes; it cannot exclude an input
			}
			pat := strings.TrimSuffix(e, "/")
			if pat != clean && !strings.HasPrefix(clean, pat+"/") {
				continue
			}
			t.Errorf(".dockerignore entry %q excludes %q, which the Dockerfile COPYs.\n"+
				"    A missing build input does not fail here — it fails INSIDE the image "+
				"build, with an error naming a Go package or a missing embed rather than "+
				"the line that caused it. .gitignore's first line in this repo records "+
				"exactly that failure: the project muster was carved out of ignored an "+
				"asset its own source go:embed'ed, and `go build` broke on a fresh clone.",
				e, in)
		}
	}

	// 🔴 web/static/app.css SPECIFICALLY. It is committed, go:embed'ed, and the
	// css stage rebuilds and `cmp`s it — so excluding it would break the
	// comparison that stops a STALE stylesheet shipping, which is a failure
	// nothing else in this suite would see.
	for _, e := range entries {
		pat := strings.TrimSuffix(strings.TrimPrefix(e, "/"), "/")
		if pat == "web" || pat == "web/static" || pat == "web/static/app.css" {
			t.Errorf(".dockerignore entry %q excludes the committed stylesheet. It is a "+
				"go:embed BUILD INPUT and the css stage `cmp`s the committed copy against "+
				"a fresh build; excluding it breaks the only check that catches a stale "+
				"stylesheet.", e)
		}
	}

	t.Logf("%d .dockerignore entr(ies); .git excluded; %d build input(s) checked clear",
		len(entries), len(inputs))
}

// TestTheDockerignoreScanRejectsAPureCommentFile is the NEGATIVE CONTROL.
//
// 🔴 A SCANNER THAT CANNOT GO RED REPORTS ZERO EXACTLY THE WAY A CLEAN TREE
// DOES — and the specific way THIS one could fail is the one that already
// happened once in this arc: a Dockerfile guard satisfied by the comment
// explaining it. So the stripper is shown to reduce a comments-only file to
// nothing, which is what makes the positive control above able to fire.
func TestTheDockerignoreScanRejectsAPureCommentFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".dockerignore")
	// Every entry the real file has, written as PROSE. A raw substring search
	// would find ".git" here and call the file compliant.
	const commentsOnly = "# This file excludes .git and node_modules and .env\n" +
		"#   .git\n" +
		"\n" +
		"   # even indented, a comment is not an entry\n"
	if err := os.WriteFile(path, []byte(commentsOnly), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// dockerignoreEntries t.Fatal()s on a read error only, so a sub-test is not
	// needed: it returns, and the assertion is on what it returned.
	got := dockerignoreEntries(t, path)
	if len(got) != 0 {
		t.Fatalf("negative control FAILED: the entry extraction returned %v from a file "+
			"containing nothing but comments. It is matching prose, so the gate above "+
			"would be satisfied by a .dockerignore that excludes nothing.", got)
	}
}
