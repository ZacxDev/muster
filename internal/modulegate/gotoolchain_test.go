package modulegate

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// EVERY GO BUILD SITE NAMES THE GO RELEASE go.mod's `go` DIRECTIVE NAMES.
//
// muster's binaries are compiled in four places, and each one picks its Go
// toolchain by a different mechanism:
//
//   1. go.mod's `go` directive — what `go build` in a checkout requires. It is
//      THE pin; the other three must agree with it.
//   2. CI's actions/setup-go — follows go.mod, but ONLY via `go-version-file`;
//      a literal `go-version:` is a second, independent statement.
//   3. every Dockerfile's `FROM golang:<tag>` — the official image sets
//      GOTOOLCHAIN=local, so go.mod's version is never downloaded there: the
//      tag IS the compiler.
//   4. flake.nix's `buildGo1NNModule` / `go_1_NN` — nix's own Go, from
//      flake.lock.
//
// 🔴 THE DEFECT THIS PINS IS DISAGREEMENT, NOT ONE STALE FILE. Each site can
// look correct in isolation while the release binaries come from a different
// one. So this test pins the RELATIONSHIP: every site on the directive's
// minor, and no second statement of the version beside the directive.
//
// 🔴 THE PATCH FLOOR IS THE DIRECTIVE ITSELF, NOT THIS TEST. With `go 1.26.11`,
// a toolchain older than that refuses the module outright ("go.mod requires
// go >= 1.26.11 (running go 1.26.8; GOTOOLCHAIN=local)"), so a stale golang
// image or a stale flake.lock FAILS rather than builds an unpatched binary.
// Which patch is patched enough is the `govulncheck` CI job's claim. A
// `toolchain` line is refused here: it is ignored under GOTOOLCHAIN=local, and
// it would be a second number to keep in step.
//
// ⚠ WHAT THIS DOES NOT SEE: the PATCH of nix's `go_1_NN` (flake.lock decides
// it, and only evaluating nixpkgs can read it) or of a floating image tag.
// For both, the directive's refusal is the guard.
// ---------------------------------------------------------------------------

var (
	goDirective        = regexp.MustCompile(`(?m)^go\s+(\S+)\s*$`)
	toolchainDirective = regexp.MustCompile(`(?m)^toolchain\s+(\S+)\s*$`)
	// FROM [--platform=…] golang:<tag> [AS name]
	fromGolang  = regexp.MustCompile(`(?mi)^\s*FROM\s+(?:--\S+\s+)*(?:docker\.io/)?(?:library/)?golang:(\S+)`)
	nixBuilder  = regexp.MustCompile(`\bbuildGo1(\d+)Module\b`)
	nixCompiler = regexp.MustCompile(`\bgo_1_(\d+)\b`)
	// Nix comments: `# …` to end of line, and `/* … */`.
	nixComment = regexp.MustCompile(`(?s)/\*.*?\*/|#[^\n]*`)
	setupGo    = regexp.MustCompile(`(?m)^\s*-?\s*uses:\s*actions/setup-go@`)
	goVerFile  = regexp.MustCompile(`(?m)^\s*go-version-file:\s*go\.mod\s*$`)
	goVerLit   = regexp.MustCompile(`(?m)^\s*go-version:`)
	goMinorRe  = regexp.MustCompile(`^1\.(\d+)(?:\.\d+)?(?:-|$)`)
)

// goMinor returns NN from "1.NN", "1.NN.P" or "1.NN[.P]-variant". ok is false
// for anything else (a floating "1-alpine", "alpine", a release candidate),
// which the callers treat as a failure, not a skip.
func goMinor(s string) (int, bool) {
	m := goMinorRe.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// nixCode is flake.nix with its comments removed, so a version named in prose
// can neither satisfy the positive control nor fail the agreement check.
func nixCode(src string) string { return nixComment.ReplaceAllString(src, "") }

func TestGoMinorParsesTheShapesTheSitesUse(t *testing.T) {
	cases := map[string]struct {
		want int
		ok   bool
	}{
		"1.26.11":        {26, true},
		"1.26.11-alpine": {26, true},
		"1.26-alpine":    {26, true},
		"1.27":           {27, true},
		"1-alpine":       {0, false},
		"alpine":         {0, false},
		"1.26rc1":        {0, false},
	}
	for in, c := range cases {
		got, ok := goMinor(in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("goMinor(%q) = %d,%v; want %d,%v", in, got, ok, c.want, c.ok)
		}
	}
}

func TestNixCodeDropsCommentsOnly(t *testing.T) {
	src := "a = pkgs.go_1_26; # go_1_25 in prose\n/* buildGo124Module */ b = pkgs.buildGo126Module;\n"
	code := nixCode(src)
	if got := nixCompiler.FindAllString(code, -1); len(got) != 1 || got[0] != "go_1_26" {
		t.Errorf("compiler refs in code = %v; want [go_1_26]", got)
	}
	if got := nixBuilder.FindAllString(code, -1); len(got) != 1 || got[0] != "buildGo126Module" {
		t.Errorf("builder refs in code = %v; want [buildGo126Module]", got)
	}
}

func TestGoToolchainAgreesAcrossGoModDockerfilesFlakeAndCI(t *testing.T) {
	root := moduleRoot(t)

	// --- 1. go.mod: the pin --------------------------------------------------
	gomod := readFile(t, filepath.Join(root, "go.mod"))
	m := goDirective.FindStringSubmatch(gomod)
	if m == nil {
		t.Fatal("positive control FAILED: no `go` directive parsed from go.mod")
	}
	directive := m[1]
	minor, ok := goMinor(directive)
	if !ok {
		t.Fatalf("go.mod: `go %s` does not parse as a Go 1.NN release", directive)
	}
	if tc := toolchainDirective.FindStringSubmatch(gomod); tc != nil {
		t.Errorf("go.mod: `toolchain %s` is a second statement of the version; the `go` directive "+
			"(`go %s`) is the pin, and GOTOOLCHAIN=local builds ignore a toolchain line", tc[1], directive)
	}

	// --- 3. every Dockerfile that builds on the golang image ----------------
	var froms []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !(strings.HasPrefix(name, "Dockerfile") || strings.HasSuffix(name, ".Dockerfile") || name == "Containerfile") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		for _, fm := range fromGolang.FindAllStringSubmatch(string(b), -1) {
			froms = append(froms, rel+": golang:"+fm[1])
			if v, ok := goMinor(fm[1]); !ok || v != minor {
				t.Errorf("%s: `golang:%s` is not Go 1.%d, which go.mod's `go %s` names; "+
					"the image runs GOTOOLCHAIN=local, so its tag IS the compiler", rel, fm[1], minor, directive)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// 🔴 POSITIVE CONTROL: the server image and the agent image both build Go.
	// Fewer than two hits means the pattern stopped matching, and every
	// assertion above passed over nothing.
	if len(froms) < 2 {
		t.Fatalf("positive control FAILED: found %d `FROM golang:` lines (%v); want at least 2 "+
			"(Dockerfile, images/claude-code-agent/Dockerfile)", len(froms), froms)
	}

	// --- 4. flake.nix, code only --------------------------------------------
	code := nixCode(readFile(t, filepath.Join(root, "flake.nix")))
	builders := nixBuilder.FindAllStringSubmatch(code, -1)
	compilers := nixCompiler.FindAllStringSubmatch(code, -1)
	// 🔴 POSITIVE CONTROL, ON CODE: `buildGoPinned` and the dev shell's
	// compiler. A mention in a comment does not count, so deleting either
	// expression cannot be papered over by the prose that explains it.
	if len(builders) == 0 || len(compilers) == 0 {
		t.Fatalf("positive control FAILED: flake.nix code has %d buildGo1NNModule and %d go_1_NN "+
			"expression(s); want at least one of each", len(builders), len(compilers))
	}
	for _, h := range append(builders, compilers...) {
		if got, _ := strconv.Atoi(h[1]); got != minor {
			t.Errorf("flake.nix: `%s` is Go 1.%s but go.mod's `go %s` is Go 1.%d", h[0], h[1], directive, minor)
		}
	}

	// --- 2. CI: setup-go must follow go.mod, never a literal ----------------
	wfs, _ := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.y*ml"))
	uses, files := 0, 0
	for _, wf := range wfs {
		b := readFile(t, wf)
		rel, _ := filepath.Rel(root, wf)
		u := len(setupGo.FindAllStringIndex(b, -1))
		if u == 0 {
			continue
		}
		files++
		uses += u
		if f := len(goVerFile.FindAllStringIndex(b, -1)); f != u {
			t.Errorf("%s: %d actions/setup-go steps but %d `go-version-file: go.mod`; every one must follow go.mod", rel, u, f)
		}
		if goVerLit.MatchString(b) {
			t.Errorf("%s: a literal `go-version:` is a second statement of the toolchain; use `go-version-file: go.mod`", rel)
		}
	}
	if uses == 0 {
		t.Fatal("positive control FAILED: no actions/setup-go step found in .github/workflows")
	}
	t.Logf("go.mod go %s; %d golang image(s) %v; flake code: %d builder, %d compiler; %d setup-go step(s) in %d workflow(s)",
		directive, len(froms), froms, len(builders), len(compilers), uses, files)
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}
