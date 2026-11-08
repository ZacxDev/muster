package api

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestOnlyTheViewDoorBindsASubject is the WRITER LEDGER behind chat_messages.about
// (migration 0097; docs/design/chief-in-agent-view.md 3.1): the session the owner
// was viewing when they typed a line to the chief. Phase 2's succession binds to
// it ("successor_of must equal about.session_key"), so a second writer would be
// a second way to point the chief at a session — every way a value can reach
// the field is listed here, across the WHOLE module's non-test source, and
// compared BOTH ways: a new site fails, and so does a listed one that is gone.
//
//   - ANY struct-literal key OR assignment whose target is named About;
//   - every call of the two turn functions that carry the subject
//     (runEnteredTurn, startAgentTurn), with that argument as written: only
//     the view's chief door (handleViewChief) passes one; every other caller
//     passes nil;
//   - every call of aboutArg (the insert's only use of the field).
//
// ⚠ STRUCTURAL: it pins WHICH expression each site passes, not that it is
// right. The behaviour — the subject is the server's, whatever the page sends
// — is TestTheViewChiefDoorBindsTheServersSubject.
func TestOnlyTheViewDoorBindsASubject(t *testing.T) {
	want := []string{
		// The one setter: the user row runEnteredTurn stores, from its about.
		"internal/api/agents.go (*Server).runEnteredTurn → literal About: about",
		// Who passes what into the turn.
		"internal/api/agents.go (*Server).handleAgentWS → runEnteredTurn(nil)",
		"internal/api/agents.go (*Server).runUserTurn → runEnteredTurn(nil)",
		"internal/api/agent_send.go (*Server).startAgentTurn → runEnteredTurn(about)",
		"internal/api/agent_send.go (*Server).handleAgentSend → startAgentTurn(nil)",
		"internal/api/chief_nudge.go (*Server).sendNudgeTurn → startAgentTurn(nil)",
		"internal/api/ext.go (*Server).UseExtensions → startAgentTurn(nil)",
		// 🔴 THE ONE NON-NIL SUBJECT: resolved by viewChiefSubject from the hub.
		"internal/api/view_chief.go (*Server).handleViewChief → startAgentTurn(&about)",
		// The store: the insert writes it (NULL unless a user row has one) and
		// the one scan reads it back.
		"internal/agents/pgstore.go (*PGStore).AddChatMessage → aboutArg(m)",
		"internal/agents/pgstore.go scanChatMessage → assign m.About",
	}
	got := aboutSites(t)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the writers of a chat row's subject changed.\n got:\n  %s\nwant:\n  %s\n"+
			"Only the view's chief door may bind a chat line to a session (chat_messages.about), and only with the subject the "+
			"SERVER resolved: phase 2's successor_of is checked against it (docs/design/chief-in-agent-view.md 4.2).",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	// Positive control on the scanner: it reached both packages and the setter.
	setters := 0
	for _, g := range got {
		if strings.Contains(g, "literal About: about") {
			setters++
		}
	}
	if len(got) < 8 || setters != 1 {
		t.Fatalf("scanner found %d sites and %d setters; it is not reading the module", len(got), setters)
	}
}

// aboutSites scans the module's non-test Go source (from the repository root)
// for the shapes TestOnlyTheViewDoorBindsASubject lists.
func aboutSites(t *testing.T) []string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("not the module root %s: %v", root, err)
	}
	// The turn functions, with the subject argument's index.
	calls := map[string]int{"runEnteredTurn": 6, "startAgentTurn": 5, "aboutArg": 0}
	fset := token.NewFileSet()
	var out []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == "node_modules" || (strings.HasPrefix(n, ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return err
		}
		render := func(n ast.Node) string {
			var b bytes.Buffer
			_ = printer.Fprint(&b, fset, n)
			return b.String()
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
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
			at := rel + " " + name + " → "
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.KeyValueExpr:
					if k, ok := x.Key.(*ast.Ident); ok && k.Name == "About" {
						out = append(out, at+"literal About: "+render(x.Value))
					}
				case *ast.AssignStmt:
					for _, l := range x.Lhs {
						if sel, ok := l.(*ast.SelectorExpr); ok && sel.Sel.Name == "About" {
							out = append(out, at+"assign "+render(l))
						}
					}
				case *ast.CallExpr:
					var callee string
					switch f := x.Fun.(type) {
					case *ast.SelectorExpr:
						callee = f.Sel.Name
					case *ast.Ident:
						callee = f.Name
					}
					i, ok := calls[callee]
					if !ok {
						return true
					}
					arg := "<missing>"
					if i < len(x.Args) {
						arg = render(x.Args[i])
					}
					out = append(out, at+callee+"("+arg+")")
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return chatWriterDedupe(out)
}
