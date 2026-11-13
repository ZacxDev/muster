package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestEveryAttentionKindHasAHome: every attention item kind this package
// declares — every const or var named attentionKind* anywhere in its non-test
// source (package level or inside a function), read by
// parsing the source (attentionKindConsts), not from a hand-typed list — has
// exactly one home: one Ask-tab group, or, for the reply draft, the Reply
// drafts tab (askTabItems strips it from the Ask tab). A new kind constant
// with no home fails (as "is in no group"), and so does a group holding a
// kind no constant declares.
func TestEveryAttentionKindHasAHome(t *testing.T) {
	held := map[string]string{attentionKindPlugin: "the Reply drafts tab"}
	for _, g := range askGroups {
		for _, k := range g.kinds {
			if prev, dup := held[k]; dup {
				t.Errorf("kind %q is in two groups (%s, %s)", k, prev, g.key)
			}
			held[k] = g.key
		}
	}
	kinds := attentionKindConsts(t)
	var names []string
	for k := range kinds {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if held[k] == "" {
			t.Errorf("kind %q (%s) is in no group", k, kinds[k])
		}
	}
	for k, home := range held {
		if _, ok := kinds[k]; !ok {
			t.Errorf("%s holds kind %q, which no attentionKind* const or var declares", home, k)
		}
	}
}

// attentionKindConsts is every const or var named attentionKind* anywhere in
// this package's non-test .go files (package level or inside a function), by
// value, each with its name: the kinds an attention item can carry. One it
// cannot read as a string literal fails the test.
func attentionKindConsts(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	out := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		af, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		// The WHOLE file (ast.Inspect), not its top-level declarations alone:
		// a kind declared inside a function, or as a var, is still a kind.
		ast.Inspect(af, func(nd ast.Node) bool {
			gd, ok := nd.(*ast.GenDecl)
			if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
				return true
			}
			for _, sp := range gd.Specs {
				vs := sp.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if !strings.HasPrefix(n.Name, "attentionKind") {
						continue
					}
					var lit *ast.BasicLit
					if i < len(vs.Values) {
						lit, _ = vs.Values[i].(*ast.BasicLit)
					}
					if lit == nil || lit.Kind != token.STRING {
						t.Fatalf("%s: %s is not a string literal this ledger can read", fset.Position(n.Pos()), n.Name)
					}
					v, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					out[v] = n.Name
				}
			}
			return true
		})
	}
	// Positive control: the parse sees the kinds the page is known to draw,
	// so an empty read (wrong directory, wrong prefix) cannot pass as "every
	// kind has a home".
	for _, k := range []string{attentionKindAsk, attentionKindSession, attentionKindPlugin} {
		if _, ok := out[k]; !ok {
			t.Fatalf("the source parse found no constant for kind %q (found %v)", k, out)
		}
	}
	return out
}
