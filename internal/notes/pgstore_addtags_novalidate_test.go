package notes

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// packageCallGraph parses every non-test .go file in dir and returns, for each
// function or method NAME, the set of names its body calls — plus the set of
// names actually declared.
//
// 🔴 NAMES ARE UNQUALIFIED ON PURPOSE. Within one package that OVER-approximates:
// two functions sharing a name merge into one node. Over-approximation is the safe
// direction for a "must never reach" assertion — it can produce a loud false
// positive that names its path, never a silent pass. Under-approximating is what
// the first version of this guard did, and it let two mutants through.
func packageCallGraph(t *testing.T, dir string) (graph map[string]map[string]bool, declared map[string]bool) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	graph, declared = map[string]map[string]bool{}, map[string]bool{}
	files := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files++
		parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			declared[fn.Name.Name] = true
			if graph[fn.Name.Name] == nil {
				graph[fn.Name.Name] = map[string]bool{}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch f := call.Fun.(type) {
				case *ast.Ident:
					graph[fn.Name.Name][f.Name] = true
				case *ast.SelectorExpr:
					graph[fn.Name.Name][f.Sel.Name] = true
				}
				return true
			})
		}
	}
	if files == 0 {
		t.Fatalf("no non-test .go files found in %s — the scan covered nothing", dir)
	}
	return graph, declared
}

// reaches reports whether target is reachable from start, and by what path.
func reaches(graph map[string]map[string]bool, start, target string) (bool, []string) {
	type node struct {
		name string
		path []string
	}
	seen := map[string]bool{start: true}
	queue := []node{{start, []string{start}}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		callees := make([]string, 0, len(graph[cur.name]))
		for c := range graph[cur.name] {
			callees = append(callees, c)
		}
		sort.Strings(callees) // deterministic path in the failure message
		for _, c := range callees {
			if c == target {
				return true, append(append([]string{}, cur.path...), c)
			}
			if seen[c] {
				continue
			}
			seen[c] = true
			queue = append(queue, node{c, append(append([]string{}, cur.path...), c)})
		}
	}
	return false, nil
}

// TestPGStoreAddTagsDoesNotReachValidateTags pins the assumption the merge route's
// checked 500 rests on: `AddTags` is NOT a validation boundary, so stamping a loser
// that already carries MaxTags tags cannot fail on the cap, and turning that write's
// error into a 500 therefore cannot make a merge fail that succeeds today.
//
// 🔴 THIS EXISTS BECAUSE TWO EARLIER VERSIONS OF IT WERE TOO NARROW, EACH IN A WAY
// THAT PASSED WHILE THE PROPERTY WAS FALSE.
//   - v1 lived in internal/api and ran against `fakeNotes`, a different
//     implementation, so adding validation to the real store left it GREEN.
//   - v2 scanned the literal body of AddTags in one hardcoded file, so it stayed
//     GREEN when validation moved into a package-local helper AddTags calls, and
//     when AddTags itself moved to another file in the same package.
//
// This version walks the whole package's call graph transitively, and FAILS LOUDLY
// if it cannot find AddTags at all — a not-found decl must never be a quiet pass,
// which is exactly how v2 leaked.
//
// If you are here because this test failed, you made `ValidateTags` reachable from
// `PGStore.AddTags`. That is a defensible change — but the supersede stamp in
// internal/api/merge.go then needs its error path revisited, because a loser already
// at MaxTags will start 500ing a merge that succeeds today, AFTER the winner's tags
// and comment have already been written. Read the comment at that call site first.
//
// ⚠ ITS SCOPE IS THE NAME `ValidateTags`, NOT "validation" IN GENERAL, and the
// sentence above is deliberately worded to say so. An open-coded cap check inside
// AddTags — `if len(add) > MaxTags { … }`, never naming ValidateTags — is invisible
// here, and nothing else covers it either: every Postgres-backed test in this
// package skips without MUSTER_TEST_DATABASE_URL. That is the accepted ceiling of
// a call-graph technique, not an oversight. Calls through a generic instantiation
// are invisible for the same reason; there are none in this package today.
func TestPGStoreAddTagsDoesNotReachValidateTags(t *testing.T) {
	graph, declared := packageCallGraph(t, ".")

	if !declared["AddTags"] {
		t.Fatal("no AddTags declaration found in this package — the scan cannot have " +
			"proven anything about it, and a silent pass here is how the previous " +
			"version of this guard leaked")
	}
	if ok, path := reaches(graph, "AddTags", "ValidateTags"); ok {
		t.Errorf("AddTags now reaches ValidateTags via %s — see this test's doc comment; "+
			"internal/api/merge.go's supersede stamp assumes it does NOT",
			strings.Join(path, " -> "))
	}
}

// TestTheCallGraphGuardCanActuallySeeAReachableCall is the positive control, in the
// two directions that matter. Without it, the clean result above is a fact about the
// walker and nothing else.
//
// 🔴 THE TRANSITIVE HALF IS CONTROLLED SYNTHETICALLY BECAUSE THE PACKAGE HAS NO
// TRANSITIVE EXAMPLE. `MergeTaskTags` calls `ValidateTags` directly, so it exercises
// depth 1 only — and depth 1 is precisely what the leaked v2 guard already did. A
// control that only proves what the broken version proved is not a control.
func TestTheCallGraphGuardCanActuallySeeAReachableCall(t *testing.T) {
	graph, declared := packageCallGraph(t, ".")

	// Direct, against the real corpus: the scan must find a call it demonstrably has.
	if !declared["MergeTaskTags"] {
		t.Fatal("MergeTaskTags not found — the scan is not reading this package")
	}
	if ok, _ := reaches(graph, "MergeTaskTags", "ValidateTags"); !ok {
		t.Fatal("the scan cannot see ValidateTags in MergeTaskTags, where it " +
			"demonstrably is — every negative result in this file is meaningless")
	}

	// Transitive, synthetic: a -> b -> ValidateTags must be REACHABLE...
	hop := map[string]map[string]bool{
		"a": {"b": true},
		"b": {"ValidateTags": true},
	}
	if ok, path := reaches(hop, "a", "ValidateTags"); !ok {
		t.Error("the traversal does not follow a two-hop path, so a guard hidden behind " +
			"one helper would report clean")
	} else if len(path) != 3 {
		t.Errorf("two-hop path = %v, want 3 nodes", path)
	}
	// ...and a graph without it must NOT be, or "reachable" is not a real answer.
	noHop := map[string]map[string]bool{
		"a": {"b": true},
		"b": {"NormalizeTags": true},
	}
	if ok, path := reaches(noHop, "a", "ValidateTags"); ok {
		t.Errorf("the traversal reported ValidateTags reachable via %v in a graph that "+
			"does not contain it", path)
	}
}

// TestNormalizeTagsDoesNotEnforceTheCountCap pins the mechanism underneath all of
// this: `AddTags` runs NormalizeTags and nothing else, and NormalizeTags does not
// apply MaxTags. ValidateTags does. That asymmetry is the whole reason the stamp
// cannot push a merge into a 400.
func TestNormalizeTagsDoesNotEnforceTheCountCap(t *testing.T) {
	over := make([]string, 0, MaxTags+1)
	for i := 0; i <= MaxTags; i++ {
		over = append(over, "t"+strings.Repeat("x", i+1))
	}
	if got := NormalizeTags(over); len(got) != MaxTags+1 {
		t.Errorf("NormalizeTags returned %d of %d tags — it must NOT apply the count cap",
			len(got), MaxTags+1)
	}
	// The control on the same input: the validating path DOES refuse it, so the
	// assertion above is about the cap and not about the fixture being legal.
	if err := ValidateTags(NormalizeTags(over)); err == nil {
		t.Fatal("ValidateTags accepted MaxTags+1 tags — the asymmetry this test " +
			"describes does not exist, so the assertion above proves nothing")
	}
}
