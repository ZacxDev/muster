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
// EVERY GO BUILD SITE NAMES THE SAME GO RELEASE, AND IT IS A PATCHED ONE.
//
// muster's binaries are compiled in four places, and each one picks its Go
// toolchain by a different mechanism:
//
//   1. go.mod's `go` directive — what `go build` in a checkout requires.
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
// one. So this test pins the RELATIONSHIP: one minor everywhere, at or above
// goFloor, and the `go` directive itself carries the PATCH floor.
//
// 🔴 WHY THE PATCH LIVES IN THE `go` DIRECTIVE, not only in a `toolchain` line:
// with `go 1.26.11`, a toolchain older than that refuses the module outright
// ("go.mod requires go >= 1.26.11 (running go 1.26.8; GOTOOLCHAIN=local)"),
// so a stale golang image or a stale flake.lock FAILS rather than builds an
// unpatched binary. A `toolchain` line is ignored under GOTOOLCHAIN=local and
// would let exactly that build through.
//
// ⚠ WHAT THIS DOES NOT SEE: the PATCH of nix's `go_1_NN` (flake.lock decides
// it, and only evaluating nixpkgs can read it) — the `go` directive above is
// what makes a lock below the floor fail the `nix` CI job. And whether a newer
// advisory has landed: that is the `govulncheck` CI job's claim, not this one.
// ---------------------------------------------------------------------------

// goFloor is the oldest Go release muster may be built with. Raise it when a
// standard-library advisory reachable from muster is fixed in a newer patch
// (the `govulncheck` CI job names it); never lower it.
const goFloor = "1.26.11"

var (
	goDirective        = regexp.MustCompile(`(?m)^go\s+(\S+)\s*$`)
	toolchainDirective = regexp.MustCompile(`(?m)^toolchain\s+go(\S+)\s*$`)
	// FROM [--platform=…] golang:<tag> [AS name]
	fromGolang = regexp.MustCompile(`(?mi)^\s*FROM\s+(?:--\S+\s+)*(?:docker\.io/)?(?:library/)?golang:(\S+)`)
	nixGo      = regexp.MustCompile(`\b(?:buildGo1(\d+)Module|go_1_(\d+))\b`)
	setupGo    = regexp.MustCompile(`(?m)^\s*-?\s*uses:\s*actions/setup-go@`)
	goVerFile  = regexp.MustCompile(`(?m)^\s*go-version-file:\s*go\.mod\s*$`)
	goVerLit   = regexp.MustCompile(`(?m)^\s*go-version:`)
	imageTagGo = regexp.MustCompile(`^(\d+\.\d+(?:\.\d+)?)(?:-|$)`)
)

// goVersion parses "1.26", "1.26.11" or "1.26.11-alpine" into major, minor,
// patch. ok is false for anything else (a floating "1-alpine", "alpine", a
// release candidate), which the callers treat as a failure, not a skip.
func goVersion(s string) (v [3]int, ok bool) {
	m := imageTagGo.FindStringSubmatch(s)
	if m == nil {
		return v, false
	}
	parts := strings.Split(m[1], ".")
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

func versionLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func TestGoVersionParsesTheShapesTheSitesUse(t *testing.T) {
	cases := map[string]struct {
		want [3]int
		ok   bool
	}{
		"1.26.11":        {[3]int{1, 26, 11}, true},
		"1.26.11-alpine": {[3]int{1, 26, 11}, true},
		"1.25-alpine":    {[3]int{1, 25, 0}, true},
		"1.27.4":         {[3]int{1, 27, 4}, true},
		"1-alpine":       {[3]int{}, false},
		"alpine":         {[3]int{}, false},
		"1.26rc1":        {[3]int{}, false},
	}
	for in, c := range cases {
		got, ok := goVersion(in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("goVersion(%q) = %v,%v; want %v,%v", in, got, ok, c.want, c.ok)
		}
	}
	if !versionLess([3]int{1, 26, 10}, [3]int{1, 26, 11}) ||
		versionLess([3]int{1, 26, 11}, [3]int{1, 26, 11}) ||
		!versionLess([3]int{1, 25, 14}, [3]int{1, 26, 0}) {
		t.Fatal("versionLess orders patch/minor wrongly")
	}
}

func TestGoToolchainFloorAgreesAcrossGoModDockerfilesFlakeAndCI(t *testing.T) {
	root := moduleRoot(t)
	floor, ok := goVersion(goFloor)
	if !ok {
		t.Fatalf("goFloor %q does not parse", goFloor)
	}

	// --- 1. go.mod ---------------------------------------------------------
	gomod := readFile(t, filepath.Join(root, "go.mod"))
	m := goDirective.FindStringSubmatch(gomod)
	if m == nil {
		t.Fatal("positive control FAILED: no `go` directive parsed from go.mod")
	}
	modV, ok := goVersion(m[1])
	if !ok || strings.Count(m[1], ".") != 2 {
		t.Fatalf("go.mod: `go %s` must name a full patch release (e.g. `go %s`): "+
			"the patch in the `go` directive is what makes an older toolchain refuse to build",
			m[1], goFloor)
	}
	if versionLess(modV, floor) {
		t.Errorf("go.mod: `go %s` is below the floor %s", m[1], goFloor)
	}
	if tc := toolchainDirective.FindStringSubmatch(gomod); tc != nil {
		if v, ok := goVersion(tc[1]); !ok || versionLess(v, floor) || v[1] != modV[1] {
			t.Errorf("go.mod: `toolchain go%s` must be on the `go` directive's minor and >= %s", tc[1], goFloor)
		}
	}
	minor := modV[1]

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
			v, ok := goVersion(fm[1])
			switch {
			case !ok || strings.Count(strings.SplitN(fm[1], "-", 2)[0], ".") != 2:
				t.Errorf("%s: `golang:%s` must name a full patch release (e.g. golang:%s-alpine); "+
					"the image runs GOTOOLCHAIN=local, so its tag IS the compiler", rel, fm[1], goFloor)
			case v[1] != minor:
				t.Errorf("%s: `golang:%s` is Go 1.%d but go.mod is Go 1.%d", rel, fm[1], v[1], minor)
			case versionLess(v, floor):
				t.Errorf("%s: `golang:%s` is below the floor %s", rel, fm[1], goFloor)
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

	// --- 4. flake.nix ------------------------------------------------------
	flake := readFile(t, filepath.Join(root, "flake.nix"))
	nixHits := nixGo.FindAllStringSubmatch(flake, -1)
	// 🔴 POSITIVE CONTROL: buildGoPinned and the dev shell's compiler.
	if len(nixHits) < 2 {
		t.Fatalf("positive control FAILED: found %d buildGo1NNModule/go_1_NN references in flake.nix; want at least 2", len(nixHits))
	}
	for _, h := range nixHits {
		n := h[1] + h[2]
		if got, _ := strconv.Atoi(n); got != minor {
			t.Errorf("flake.nix: `%s` is Go 1.%s but go.mod is Go 1.%d", h[0], n, minor)
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
	t.Logf("go.mod go %d.%d.%d; %d golang image(s) %v; %d flake reference(s); %d setup-go step(s) in %d workflow(s)",
		modV[0], modV[1], modV[2], len(froms), froms, len(nixHits), uses, files)
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}
