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
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
)

// TestOnlyTheOwnersChatDoorsStampTypedBy is the WRITER LEDGER behind
// chat_messages.typed_by (migration 0069) — the provenance task_dispatch's gate
// release trusts (chief_owner_release.go). A chat row whose typed_by names the
// chief's owner is "the owner typed this", and that can release a gate an
// outside sender's text never may. So every way a value reaches that column is
// listed here, across the WHOLE module's non-test source, and compared BOTH
// ways: a new writer fails, and so does a listed one that is gone.
//
//   - Every composite literal that sets ChatMessage.TypedBy (a plugin delivery,
//     an attention relay, a route note, a kickoff, a forward, a job, an officer
//     or relay turn, `chief ask`… set NOTHING, so they are absent here and their
//     rows are NULL).
//   - Every call to the two turn functions that carry typedBy, with the
//     argument as written: only the owner's two signed-in chat doors pass
//     ownerTypedBy(ctx); every other caller passes notTyped.
//   - Every call to ownerTypedBy.
//   - Every string literal naming the column (the only SQL that writes or reads
//     it).
//
// ⚠ STRUCTURAL: it pins WHICH expression each site passes, not that the
// expression is right. ownerTypedBy's own behaviour is
// TestOwnerTypedByIsTheSignedInBrowserOnly; the doors' is
// TestTheOwnersChatDoorsStampTheTyper.
func TestOnlyTheOwnersChatDoorsStampTypedBy(t *testing.T) {
	want := []string{
		// The one literal that sets it: the turn's own user row.
		"internal/api/agents.go (*Server).runEnteredTurn → ChatMessage{TypedBy: typedBy}",
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
		// The SQL: the insert (NULLIF(…, 0): zero is NULL) and the release's read.
		"internal/agents/pgstore.go (*PGStore).AddChatMessage → sql typed_by",
		"internal/agents/pgstore.go (*PGStore).LatestUserTurns → sql typed_by",
		"internal/agents/pgstore.go (*PGStore).LatestUserTurns → sql typed_by",
	}
	got := typedBySites(t)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("chat_messages.typed_by writers changed.\n got:\n  %s\nwant:\n  %s\n"+
			"Only the owner's own signed-in chat may stamp typed_by: it is what releases a gated task "+
			"(chief_owner_release.go). An injected turn must leave it unset.",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	// Positive control on the scanner: it reached both packages and every shape.
	if len(got) < 10 {
		t.Fatalf("scanner found only %d sites; it is not reading the module", len(got))
	}
}

// typedBySites scans the module's non-test Go source (from the repository
// root) for the four shapes TestOnlyTheOwnersChatDoorsStampTypedBy lists.
func typedBySites(t *testing.T) []string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("not the module root %s: %v", root, err)
	}
	turnFns := map[string]bool{"runEnteredTurn": true, "startAgentTurn": true}
	fset := token.NewFileSet()
	var out []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == "node_modules" || n == ".git" || (strings.HasPrefix(n, ".") && path != root) {
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
					if k, ok := x.Key.(*ast.Ident); ok && k.Name == "TypedBy" {
						out = append(out, at+"ChatMessage{TypedBy: "+render(x.Value)+"}")
					}
				case *ast.BasicLit:
					if x.Kind == token.STRING && strings.Contains(x.Value, "typed_by") {
						out = append(out, at+"sql typed_by")
					}
				case *ast.CallExpr:
					switch f := x.Fun.(type) {
					case *ast.SelectorExpr:
						// s.runEnteredTurn(ctx, a, session, text, typedBy, …) /
						// s.startAgentTurn(ctx, a, session, text, typedBy)
						if turnFns[f.Sel.Name] && len(x.Args) >= 5 {
							out = append(out, at+f.Sel.Name+"("+render(x.Args[4])+")")
						}
					case *ast.Ident:
						if f.Name == "ownerTypedBy" {
							out = append(out, at+"ownerTypedBy("+render(x.Args[0])+")")
						}
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
// token, the hook token, an agent, a plugin, a relay, a route, the system — and
// an all-scope one is nobody typing, so 0.
func TestOwnerTypedByIsTheSignedInBrowserOnly(t *testing.T) {
	const uid = 4242
	if got := ownerTypedBy(authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession})); got != uid {
		t.Fatalf("a signed-in session typed by %d, want %d", got, uid)
	}
	for _, via := range []authz.Via{authz.ViaToken, authz.ViaHook, authz.ViaService, authz.ViaAgent, authz.ViaPlugin,
		authz.ViaRelay, authz.ViaRoute, authz.ViaAttention, authz.ViaQueue} {
		if got := ownerTypedBy(authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: via})); got != 0 {
			t.Errorf("a %s principal stamped typed_by %d; only a signed-in session types", via, got)
		}
	}
	if got := ownerTypedBy(context.Background()); got != 0 {
		t.Errorf("no principal stamped typed_by %d", got)
	}
	if got := ownerTypedBy(authz.System(context.Background())); got != 0 {
		t.Errorf("the system principal stamped typed_by %d", got)
	}
	// The stale bound and the sentence that states it agree.
	if ownerRequestMaxAge.Hours() != 2 || !strings.Contains(chiefReleaseStale, "(more than "+strconv.Itoa(int(ownerRequestMaxAge.Hours()))+" hours)") {
		t.Errorf("chiefReleaseStale states a bound other than ownerRequestMaxAge (%s)", ownerRequestMaxAge)
	}
}
