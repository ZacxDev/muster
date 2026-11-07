package api

import (
	"bytes"
	"context"
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

	"github.com/ZacxDev/muster/internal/authz"
)

// TestOnlyTheOwnersChatDoorsMarkAnOwnerTurn is the SETTER LEDGER behind the
// owner-turn marker (agents.owner_turn_*, migration 0069) — the one thing
// task_dispatch's owner_asked releases a gate on (chief_owner_release.go). Every
// way a value can reach the marker is listed here, across the WHOLE module's
// non-test source, and compared BOTH ways: a new setter fails, and so does a
// listed one that is gone.
//
//   - every call of SetOwnerTurn / ClearOwnerTurn (the store's only writers)
//     and of markOwnerTurn (their only caller), with its typer argument;
//   - every call of the two turn functions that carry the typer, with that
//     argument as written: only the owner's two signed-in chat doors pass
//     ownerTypedBy(ctx); every other caller passes notTyped;
//   - every call of ownerTypedBy and of agents.WithTurnAccepted;
//   - every string literal naming an owner_turn_ column (the only SQL);
//   - ANY struct-literal key OR assignment whose target is a field or
//     variable named OwnerTurn*/ownerTurn* — the shapes a future setter would
//     take (`x.OwnerTurnBy = …`, `Agent{OwnerTurnBy: …}`), which a call-only
//     scan would miss.
//
// ⚠ STRUCTURAL: it pins WHICH expression each site passes, not that it is
// right. The behaviour is TestTheOwnersChatDoorsMarkOnlyAnAcceptedOwnerTurn,
// TestMarkOwnerTurnMarksOnlyAnAcceptedOwnersTurn and
// TestOwnerTypedByIsTheSignedInBrowserOnly.
func TestOnlyTheOwnersChatDoorsMarkAnOwnerTurn(t *testing.T) {
	want := []string{
		// The store's writers, called from markOwnerTurn only.
		"internal/api/chief_owner_release.go (*Server).markOwnerTurn → ClearOwnerTurn(wctx)",
		"internal/api/chief_owner_release.go (*Server).markOwnerTurn → SetOwnerTurn(wctx)",
		"internal/api/chief_owner_release.go (*Server).markOwnerTurn → WithTurnAccepted(ctx)",
		// Not a marker setter: a fresh_session_per_item officer turn's
		// acceptance frees the instance's conversation (officers.go
		// officerConversationAccepted) and writes no owner_turn_ field. Listed
		// because every WithTurnAccepted is, so a new one is looked at.
		"internal/api/officers.go (*Server).OfficerTurnContext → WithTurnAccepted(ctx)",
		// The one caller of markOwnerTurn: the turn's own user row, its typer.
		"internal/api/agents.go (*Server).runEnteredTurn → markOwnerTurn(typedBy)",
		// Who passes what into the turn.
		"internal/api/agents.go (*Server).handleAgentWS → runEnteredTurn(ownerTypedBy(ctx))",
		"internal/api/agents.go (*Server).runUserTurn → runEnteredTurn(notTyped)",
		"internal/api/agent_send.go (*Server).startAgentTurn → runEnteredTurn(typedBy)",
		"internal/api/agent_send.go (*Server).handleAgentSend → startAgentTurn(ownerTypedBy(ctx))",
		"internal/api/chief_nudge.go (*Server).sendNudgeTurn → startAgentTurn(notTyped)",
		"internal/api/ext.go (*Server).UseExtensions → startAgentTurn(notTyped)",
		// ownerTypedBy is called by the two doors and nothing else.
		"internal/api/agents.go (*Server).handleAgentWS → ownerTypedBy(ctx)",
		"internal/api/agent_send.go (*Server).handleAgentSend → ownerTypedBy(ctx)",
		// The SQL: set, clear, read.
		"internal/agents/pgstore.go (*PGStore).ClearOwnerTurn → sql owner_turn_",
		"internal/agents/pgstore.go (*PGStore).OwnerTurnInFlight → sql owner_turn_",
		"internal/agents/pgstore.go (*PGStore).SetOwnerTurn → sql owner_turn_",
	}
	got := ownerTurnSites(t)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the owner-turn marker's setters changed.\n got:\n  %s\nwant:\n  %s\n"+
			"Only the owner's own signed-in chat, once the runtime accepted the text, may mark an owner turn: it is what "+
			"releases a gated task (chief_owner_release.go). An injected turn must never mark one.",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	// Positive control on the scanner: it reached both packages and every shape.
	if len(got) < 12 {
		t.Fatalf("scanner found only %d sites; it is not reading the module", len(got))
	}
}

// ownerTurnName reports whether a field or variable name is one a marker
// setter would be spelled with.
func ownerTurnName(n string) bool {
	return strings.HasPrefix(n, "OwnerTurn") || strings.HasPrefix(n, "ownerTurn")
}

// ownerTurnSites scans the module's non-test Go source (from the repository
// root) for the shapes TestOnlyTheOwnersChatDoorsMarkAnOwnerTurn lists.
func ownerTurnSites(t *testing.T) []string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("not the module root %s: %v", root, err)
	}
	// Calls pinned with the argument at that index (-1: the first).
	calls := map[string]int{"SetOwnerTurn": 0, "ClearOwnerTurn": 0, "WithTurnAccepted": 0, "markOwnerTurn": 3,
		"runEnteredTurn": 4, "startAgentTurn": 4, "ownerTypedBy": 0}
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
					if k, ok := x.Key.(*ast.Ident); ok && ownerTurnName(k.Name) {
						out = append(out, at+"literal "+k.Name+": "+render(x.Value))
					}
				case *ast.AssignStmt:
					for _, l := range x.Lhs {
						var id string
						switch v := l.(type) {
						case *ast.SelectorExpr:
							id = v.Sel.Name
						case *ast.Ident:
							id = v.Name
						}
						if ownerTurnName(id) {
							out = append(out, at+"assign "+render(l))
						}
					}
				case *ast.BasicLit:
					if x.Kind == token.STRING && strings.Contains(x.Value, "owner_turn_") {
						out = append(out, at+"sql owner_turn_")
					}
				case *ast.CallExpr:
					var callee string
					switch f := x.Fun.(type) {
					case *ast.SelectorExpr:
						callee = f.Sel.Name
					case *ast.Ident:
						callee = f.Name
					}
					if i, ok := calls[callee]; ok && i < len(x.Args) {
						out = append(out, at+callee+"("+render(x.Args[i])+")")
					}
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
	return out
}

// TestOwnerTypedByIsTheSignedInBrowserOnly: the typer is the principal's user
// ONLY on a signed-in browser session. Every machine principal — a personal API
// token, the hook token, an agent, a plugin, a relay, a route — and an
// all-scope one is nobody typing, so 0.
func TestOwnerTypedByIsTheSignedInBrowserOnly(t *testing.T) {
	const uid = 4242
	if got := ownerTypedBy(authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession})); got != uid {
		t.Fatalf("a signed-in session typed by %d, want %d", got, uid)
	}
	for _, via := range []authz.Via{authz.ViaToken, authz.ViaHook, authz.ViaService, authz.ViaAgent, authz.ViaPlugin,
		authz.ViaRelay, authz.ViaRoute, authz.ViaAttention, authz.ViaQueue} {
		if got := ownerTypedBy(authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: via})); got != 0 {
			t.Errorf("a %s principal typed %d; only a signed-in session types", via, got)
		}
	}
	if got := ownerTypedBy(context.Background()); got != 0 {
		t.Errorf("no principal typed %d", got)
	}
	if got := ownerTypedBy(authz.System(context.Background())); got != 0 {
		t.Errorf("the system principal typed %d", got)
	}
}
