package mirrorhost

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// guardRuleValues is every rule* constant by its name.
var guardRuleValues = map[string]string{
	"ruleMultiplexer": ruleMultiplexer,
	"ruleOpenCode":    ruleOpenCode,
}

// TestEveryRuleTheGuardReturnsIsOwned: every rule the paste guard can name —
// read from the SOURCE, as every rule* constant a return statement of
// guardShape or a runtime's shape method names — is the multiplexer rule or
// is OWNED by one runtime's pasteGuard, so guardPane refuses it in that
// runtime's words, never by its fall-back. The table above must also hold
// every rule* constant the package declares, so a new rule cannot be missed
// by a stale list.
func TestEveryRuleTheGuardReturnsIsOwned(t *testing.T) {
	fset := token.NewFileSet()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	declared, returned := map[string]bool{}, map[string]bool{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for _, n := range vs.Names {
							if isRuleName(n.Name) {
								declared[n.Name] = true
							}
						}
					}
				}
			case *ast.FuncDecl:
				name := d.Name.Name
				if name != "guardShape" && name != "shape" {
					continue
				}
				ast.Inspect(d.Body, func(n ast.Node) bool {
					ret, ok := n.(*ast.ReturnStmt)
					if !ok {
						return true
					}
					ast.Inspect(ret, func(m ast.Node) bool {
						if id, ok := m.(*ast.Ident); ok && isRuleName(id.Name) {
							returned[id.Name] = true
						}
						return true
					})
					return true
				})
			}
		}
	}
	var dk, tk []string
	for k := range declared {
		dk = append(dk, k)
	}
	for k := range guardRuleValues {
		tk = append(tk, k)
	}
	sort.Strings(dk)
	sort.Strings(tk)
	if !slices.Equal(dk, tk) {
		t.Fatalf("the package declares rules %q; this test's table holds %q", dk, tk)
	}
	if len(returned) < 2 {
		t.Fatalf("instrument check: the guard's functions return only %d rules (%v)", len(returned), returned)
	}
	for name := range returned {
		rule := guardRuleValues[name]
		if rule == ruleMultiplexer {
			continue
		}
		owners := 0
		for _, r := range knownRuntimes {
			if g, ok := r.(pasteGuard); ok && g.owns(rule) {
				owners++
			}
		}
		if owners != 1 {
			t.Errorf("the guard can return %s (%q), which %d runtimes own; want exactly one", name, rule, owners)
		}
	}
}

// isRuleName reports whether an identifier names a guard rule: "rule" and an
// upper-case letter (ruleOpenCode), never a plain word like "rule" itself.
func isRuleName(n string) bool {
	return len(n) > 4 && strings.HasPrefix(n, "rule") && n[4] >= 'A' && n[4] <= 'Z'
}
