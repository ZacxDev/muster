package api

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

// TestEveryRouteTurnReChecksUnderTheClaim is the LEDGER OF startRouteTurn's
// CALLERS (issue #732). Each one checks "still owed, not on record" before
// startRouteTurn wins the chief's delivery claim, and that check can be stale
// once the claim is won: a racing delivery may have delivered in between. So
// every production caller must hand startRouteTurn an owed re-check (its 6th
// argument) — never the literal nil — and the set of callers is compared BOTH
// ways: a new caller fails until it is listed here (and passes a re-check),
// and so does a listed one that is gone.
//
// ⚠ STRUCTURAL: it pins THAT each caller passes a re-check, not that the
// re-check is right. The behaviour is TestRacingDeliveriesOfABatchMakeOneDigestTurn,
// TestRacingDeliveriesOfAnAnswerMakeOneTurn and TestRacingDeliveriesOfAHandBackMakeOneTurn.
func TestEveryRouteTurnReChecksUnderTheClaim(t *testing.T) {
	want := []string{
		"route_many.go (*Server).deliverRouteBatch → owed",
		"session_routes.go (*Server).deliverRouteAnswers → stillOwed",
		"session_routes.go (*Server).deliverToChief → owed",
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				if star, ok := fn.Recv.List[0].Type.(*ast.StarExpr); ok {
					if id, ok := star.X.(*ast.Ident); ok {
						name = "(*" + id.Name + ")." + name
					}
				}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var callee string
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					callee = fun.Sel.Name
				case *ast.Ident:
					callee = fun.Name
				}
				if callee != "startRouteTurn" {
					return true
				}
				owed := "<missing>"
				if len(call.Args) >= 6 {
					switch a := call.Args[5].(type) {
					case *ast.Ident:
						owed = a.Name
					default:
						owed = "<expr>"
					}
				}
				got = append(got, path+" "+name+" → "+owed)
				return true
			})
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("startRouteTurn's production callers changed:\n got:\n  %s\n want:\n  %s\n"+
			"Every caller re-checks what it delivers under the chief's claim (startRouteTurn's owed, never nil): "+
			"a check made before the claim can be stale once it is won (issue #732).",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}
