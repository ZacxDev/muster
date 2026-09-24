package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// THE GUARD THE const BLOCK'S HEADER CLAIMS, WRITTEN.
//
// 🔴 IT REPLACES A FABRICATED CITATION. config.go used to end that paragraph
// with "envNamesAreDocumented asserts these constants against the
// documentation" — an identifier with exactly one occurrence in the module, the
// sentence itself. A comment naming a guard tells the next reader the question
// is settled, so a fabricated one does not leave coverage where it was, it
// removes the prompt that would have created it. That is
// internal/modulegate/citations_test.go's whole thesis, and this is the case
// that got past it.
//
// 🔴 WHAT IT ASSERTS IS A RELATIONSHIP WITH TWO DIRECTIONS, AND A ONE-SIDED
// VERSION WOULD READ AS COVERAGE WHILE PROVIDING HALF:
//
//	SPELLED ONCE — every environment-variable string literal anywhere in this
//	  binary's non-test sources is one of the const block's. This is the failure
//	  the header names: a second spelling in a wiring call, where the comment
//	  says one name and the code reads another.
//	AND READ — every constant in the block is referenced somewhere outside the
//	  block. This is the failure the header OPENS with, which the first direction
//	  cannot see at all: internal/api and internal/ui documented a dozen MUSTER_*
//	  variables that NOTHING READ. A block of constants no code touches
//	  reproduces that exactly, and passes a spelled-once check perfectly.
// ---------------------------------------------------------------------------

// envLiteral matches a quoted environment-variable name of the shapes this
// binary reads. `GITHUB_CLIENT_*` is included because two of the constants are
// not MUSTER_-prefixed, and a pattern that only knew about the prefix would be
// structurally blind to a second spelling of exactly those two.
var envLiteral = regexp.MustCompile(`"(MUSTER_[A-Z0-9_]+|DATABASE_URL|GITHUB_CLIENT_[A-Z0-9_]+)"`)

// envConstDecl matches one line of the const block: `envName = "THE_NAME"`.
var envConstDecl = regexp.MustCompile(`^\s*(env[A-Za-z0-9_]*)\s*=\s*"([A-Z0-9_]+)"\s*$`)

// serverSources lists this package's non-test .go files.
func serverSources(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		out = append(out, n)
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatal("instrument check FAILED: no non-test .go files found in this package, so " +
			"every assertion below would pass over an empty set")
	}
	return out
}

// TestEveryServerEnvNameIsSpelledOnceAndRead is the gate.
func TestEveryServerEnvNameIsSpelledOnceAndRead(t *testing.T) {
	files := serverSources(t)

	// The const block, read out of config.go by its declaration shape.
	constByName := map[string]string{} // ident -> env name
	nameToIdent := map[string]string{} // env name -> ident
	cfgBody, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("read config.go: %v", err)
	}
	for _, line := range strings.Split(string(cfgBody), "\n") {
		if m := envConstDecl.FindStringSubmatch(line); m != nil {
			constByName[m[1]] = m[2]
			nameToIdent[m[2]] = m[1]
		}
	}

	// 🔴 POSITIVE CONTROL, REPORTED AS A NUMBER RATHER THAN ASSUMED. A "0
	// violations" verdict from a scan that extracted no constants is
	// indistinguishable from a clean tree, and this file's whole subject is a
	// check that was believed without being run.
	if len(constByName) < 15 {
		t.Fatalf("positive control FAILED: only %d env constant(s) were extracted from "+
			"config.go's const block, and this binary is known to declare ~21. The "+
			"declaration pattern has stopped matching, so every assertion below is "+
			"passing over an empty or truncated set.", len(constByName))
	}
	if _, ok := nameToIdent["MUSTER_PORT"]; !ok {
		t.Fatalf("positive control FAILED: MUSTER_PORT is declared in config.go's const "+
			"block and the extraction did not find it. It read %d constant(s); the "+
			"instrument is not measuring.", len(constByName))
	}

	// --- direction 1: SPELLED ONCE -----------------------------------------
	//
	// Every env-name literal in the package must be inside the const block. The
	// block lives in config.go, so a literal in ANY other file is a second
	// spelling by construction; within config.go a literal is legitimate only on
	// a line that is itself a const declaration.
	type spelling struct{ file, line, name string }
	var extra []spelling
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, line := range strings.Split(string(body), "\n") {
			// A comment may legitimately quote a variable name in prose.
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			for _, m := range envLiteral.FindAllStringSubmatch(line, -1) {
				if f == "config.go" && envConstDecl.MatchString(line) {
					continue
				}
				extra = append(extra, spelling{f, strings.TrimSpace(line), m[1]})
			}
		}
	}
	if len(extra) > 0 {
		for _, s := range extra {
			t.Errorf("SECOND SPELLING of %s outside the const block, at %s:\n    %s\n"+
				"    Every environment-variable name this binary reads is spelled exactly "+
				"once, in config.go's const block, and referenced by its identifier (%s). "+
				"A literal here is how the comment and the code come to name different "+
				"variables — and the operator sets the one the comment names.",
				s.name, s.file, s.line, identOrUnknown(nameToIdent, s.name))
		}
	}

	// --- direction 2: AND READ ---------------------------------------------
	//
	// Every constant must be referenced somewhere outside its own declaration.
	// A declared-but-unread constant IS the documented-variable-nothing-reads
	// failure the const block's header opens with, and direction 1 is blind to it.
	unread := map[string]bool{}
	for ident := range constByName {
		unread[ident] = true
	}
	identUse := regexp.MustCompile(`\b(env[A-Za-z0-9_]*)\b`)
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if f == "config.go" && envConstDecl.MatchString(line) {
				continue // the declaration itself is not a use
			}
			for _, m := range identUse.FindAllStringSubmatch(line, -1) {
				delete(unread, m[1])
			}
		}
	}
	if len(unread) > 0 {
		var names []string
		for ident := range unread {
			names = append(names, ident+" ("+constByName[ident]+")")
		}
		sort.Strings(names)
		t.Errorf("DECLARED BUT NEVER READ: %s\n"+
			"    Each names an environment variable this binary documents and no line "+
			"consumes. That is the state config.go's header exists to prevent — a dozen "+
			"MUSTER_* variables documented in comments that nothing read — reproduced "+
			"inside the block that was supposed to fix it. Wire it, or delete it.",
			strings.Join(names, ", "))
	}

	t.Logf("%d env constant(s) checked across %d source file(s): 0 second spellings, 0 unread",
		len(constByName), len(files))
}

func identOrUnknown(m map[string]string, name string) string {
	if id, ok := m[name]; ok {
		return id
	}
	return "it has no constant at all — add one"
}

// TestTheEnvNameScanCanActuallyFindASecondSpelling is the NEGATIVE CONTROL for
// direction 1, and it is a separate test because the gate above must be green.
//
// 🔴 A SCANNER THAT CANNOT GO RED REPORTS ZERO EXACTLY THE WAY A CLEAN TREE
// DOES. The gate's own verdict is therefore a claim about the tree only once the
// pattern has been watched to match a violation built the way a real one would
// be — a bare literal on an assignment line, not a textbook fixture.
func TestTheEnvNameScanCanActuallyFindASecondSpelling(t *testing.T) {
	// The exact shape the gate exists to catch: a wiring call that re-types the
	// name instead of using the constant.
	const violation = `	uiPassword := os.Getenv("MUSTER_UI_PASSWORD")`
	if !envLiteral.MatchString(violation) {
		t.Fatalf("negative control FAILED: the scan pattern does not match a literal "+
			"second spelling:\n    %s\nA gate that cannot see this reports 0 for a tree "+
			"full of them.", violation)
	}
	// ...and it must NOT fire on the declaration shape, or the gate would be
	// permanently red on its own const block, which trains everyone to skip it.
	const declaration = `	envUIPassword    = "MUSTER_UI_PASSWORD"`
	if !envConstDecl.MatchString(declaration) {
		t.Fatalf("negative control FAILED: the declaration pattern does not match the "+
			"block's own shape:\n    %s\nThe gate would report every constant as a "+
			"second spelling of itself.", declaration)
	}
	// And a prose mention must not be a violation — the comment skip is what
	// makes the header above legal to write.
	const prose = `	// MUSTER_UI_PASSWORD is the operator secret.`
	if !strings.HasPrefix(strings.TrimSpace(prose), "//") {
		t.Fatal("negative control FAILED: the comment-detection this gate relies on does " +
			"not recognise a Go line comment")
	}
}
