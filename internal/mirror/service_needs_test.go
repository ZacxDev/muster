package mirror

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

// serviceNeeds is EVERY Service method that reads whose entries it acts on,
// with how (team sharing 2b.3, access):
//
//   - "viewer" / "operator": access with that role — a member's request aimed
//     at a team host (ForOwner) reaches it at that role, and only while
//     teamNeedsOpen opens the role (viewing since 2b.3, operating since
//     2b.4);
//   - "need": access with a role the method computes (AttachWindow: viewer
//     read-only, operator with live keys);
//   - "own": owner, the principal's OWN entries whatever ctx targets — the
//     listing that stays own-only (Hosts), its team-aware sibling (which
//     reads memberships itself), and the token and hold reads.
//
// TestEveryServiceMethodSaysItsNeed fails when a method that reads its owner
// is added, removed or changes how, so a new one is a decision.
var serviceNeeds = map[string]string{
	"AgentState":         "viewer",
	"WatchWindow":        "viewer",
	"ScrollWindow":       "viewer",
	"SubagentTail":       "viewer",
	"HostRanks":          "viewer",
	"AttachWindow":       "need",
	"SpawnAgent":         "operator",
	"CloseAgent":         "operator",
	"PasteWindow":        "operator",
	"CatalogWindow":      "operator",
	"CancelAsk":          "operator",
	"AnswerWindow":       "operator",
	"AgentClones":        "operator",
	"AnswerOpenCodeAsk":  "operator",
	"OpenCodeForm":       "operator",
	"AnswerOpenCodeForm": "operator",
	"SpawnResume":        "operator",
	"Hosts":              "own",
	"VisibleHosts":       "own",
	"HostToken":          "own",
	"LiveTokenHosts":     "own",
	"HeldReplacements":   "own",
	"ConfirmCode":        "own",
	"HeldHere":           "own",
}

// TestEveryServiceMethodSaysItsNeed walks this package's Service methods and
// classes each by its call to access or owner (serviceNeeds).
func TestEveryServiceMethodSaysItsNeed(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	fset := token.NewFileSet()
	for _, fn := range files {
		if strings.HasSuffix(fn, "_test.go") {
			continue
		}
		src, err := os.ReadFile(fn)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, fn, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 || fd.Body == nil {
				continue
			}
			star, ok := fd.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			if id, ok := star.X.(*ast.Ident); !ok || id.Name != "Service" {
				continue
			}
			var classes []string
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				switch id.Name {
				case "owner":
					classes = append(classes, "own")
				case "access":
					class := "need"
					if len(call.Args) == 2 {
						if sel, ok := call.Args[1].(*ast.SelectorExpr); ok {
							switch sel.Sel.Name {
							case "TeamViewer":
								class = "viewer"
							case "TeamOperator":
								class = "operator"
							case "TeamManager":
								class = "manager"
							}
						}
					}
					classes = append(classes, class)
				}
				return true
			})
			switch len(classes) {
			case 0:
			case 1:
				got[fd.Name.Name] = classes[0]
			default:
				t.Errorf("Service.%s reads its owner %d times (%v): one read, one decision", fd.Name.Name, len(classes), classes)
			}
		}
	}
	var names []string
	for m := range got {
		names = append(names, m)
	}
	for m := range serviceNeeds {
		if _, ok := got[m]; !ok {
			names = append(names, m)
		}
	}
	sort.Strings(names)
	for _, m := range names {
		want, listed := serviceNeeds[m]
		have, found := got[m]
		switch {
		case !listed:
			t.Errorf("Service.%s reads its owner (%s) and is not in serviceNeeds: class it", m, have)
		case !found:
			t.Errorf("serviceNeeds lists Service.%s, which no longer reads its owner: remove it", m)
		case want != have:
			t.Errorf("Service.%s reads its owner as %q; serviceNeeds says %q", m, have, want)
		}
	}
}
