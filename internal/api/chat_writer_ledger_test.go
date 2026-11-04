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

// The kinds of chat row a writer stores (TestEveryChatWriterIsClassified).
// Only chatOwnerTyped may carry a typer; every other kind is never quoted as
// the owner's covering words (operator decision 13).
const (
	chatOwnerTyped    = "owner-typed: a line a person typed into muster's chat (runEnteredTurn's typedBy)"
	chatReply         = "a runtime's reply (an assistant row: never anyone's typing)"
	chatSessionAsk    = "a session question or `chief ask` over the machine door (a token, not a person at muster's chat)"
	chatPluginMessage = "a plugin message delivered to the chief or a plugin's agent"
	chatRelayed       = "a relayed answer (attention delivery)"
	chatTaskEvent     = "a task event (a routed task, a merged or closed PR)"
	chatAgentSent     = "an agent's or muster's own turn (a kickoff, a job, an officer, a memo relay, a route's note)"
	chatE2ESeed       = "e2e fixture rows (the e2e seeders; never built into the server)"
)

// TestEveryChatWriterIsClassified is the LEDGER OF CHAT WRITERS behind
// chat_messages.typed_by (migration 0079; docs/design/session-state.md, step
// 3b-1). A Chief-chat turn is quoted as the owner's covering words only when it
// carries a typer, so every way a chat row is written is listed here, across
// the WHOLE module's non-test source, with what it writes and its kind, and
// compared BOTH ways: a new writer fails until it is classified, and so does a
// listed one that is gone.
//
//   - every call of AddChatMessage (chat_messages' only INSERT), with the
//     row's Role and TypedBy as written ("-" when the literal does not set it);
//   - ANY struct-literal key OR assignment whose target is named TypedBy — the
//     shapes a second setter would take;
//   - every string literal naming typed_by (the SQL).
//
// 🔴 Exactly one site sets TypedBy: runEnteredTurn, from its typedBy, which
// TestOnlyTheOwnersChatDoorsMarkAnOwnerTurn pins to ownerTypedBy(ctx) at the
// owner's two signed-in chat doors and notTyped everywhere else.
//
// ⚠ STRUCTURAL: it pins WHICH expression each site writes, not that it is
// right. The behaviour is TestOnlyAnOwnerTypedChatTurnCounts.
func TestEveryChatWriterIsClassified(t *testing.T) {
	want := map[string]string{
		// The one owner-typed writer, and its replies.
		`internal/api/agents.go (*Server).runEnteredTurn → AddChatMessage(role "user", TypedBy typedBy)`: chatOwnerTyped,
		`internal/api/agents.go (*Server).runEnteredTurn → literal TypedBy: typedBy`:                     chatOwnerTyped,
		`internal/api/agents.go (*Server).persistTurn → AddChatMessage(role "assistant", TypedBy -)`:     chatReply,
		// POST /api/agents/{name}/messages: a session's question, or `chief ask`.
		`internal/api/machine_agents.go (*Server).handleAPIAgentSendMessage → AddChatMessage(role "user", TypedBy -)`:      chatSessionAsk,
		`internal/api/machine_agents.go (*Server).handleAPIAgentSendMessage → AddChatMessage(role "assistant", TypedBy -)`: chatReply,
		// An owner's answer relayed to the agent that asked.
		`internal/api/attention_delivery.go (*Server).runDeliveredTurn → AddChatMessage(role "user", TypedBy -)`: chatRelayed,
		// Plugin messages: delivered as a turn, or handed to the chief without one.
		`internal/plugins/deliver.go (*Deliverer).Deliver → AddChatMessage(role "user", TypedBy -)`:      chatPluginMessage,
		`internal/plugins/deliver.go (*Deliverer).Deliver → AddChatMessage(role "assistant", TypedBy -)`: chatReply,
		`internal/plugins/deliver.go (*Deliverer).Handed → AddChatMessage(role "user", TypedBy -)`:       chatPluginMessage,
		// Task events.
		`internal/api/task_routing.go (*Server).forwardToTaskAgent → AddChatMessage(role "user", TypedBy -)`: chatTaskEvent,
		`internal/api/pr_closed.go (*Server).settleClosedPRMessage → AddChatMessage(role "user", TypedBy -)`: chatTaskEvent,
		// Agent-sent and muster's own turns.
		`internal/api/session_routes.go (*Server).noteToChief → AddChatMessage(role "user", TypedBy -)`:            chatAgentSent,
		`internal/api/jobs.go (*Server).runJobTurn → AddChatMessage(role "user", TypedBy -)`:                       chatAgentSent,
		`internal/api/jobs.go (*Server).runJobTurn → AddChatMessage(role "assistant", TypedBy -)`:                  chatReply,
		`internal/api/officers.go Reprompt → AddChatMessage(role "user", TypedBy -)`:                               chatAgentSent,
		`internal/api/officers.go Reprompt → AddChatMessage(role "assistant", TypedBy -)`:                          chatReply,
		`internal/agentkickoff/agentkickoff.go (*Deliverer).deliver → AddChatMessage(role "user", TypedBy -)`:      chatAgentSent,
		`internal/agentkickoff/agentkickoff.go (*Deliverer).deliver → AddChatMessage(role "assistant", TypedBy -)`: chatReply,
		`internal/memorelay/relay.go (*Relay).deliver → AddChatMessage(role "user", TypedBy -)`:                    chatAgentSent,
		`internal/memorelay/relay.go (*Relay).deliver → AddChatMessage(role "assistant", TypedBy -)`:               chatReply,
		// The e2e seeders: fixture rows from JSON, never built into the server.
		`e2e/mkchat/main.go appendMessages → AddChatMessage(role not a literal: m, TypedBy -)`: chatE2ESeed,
		`e2e/mkchat/main.go createThreads → AddChatMessage(role not a literal: m, TypedBy -)`:  chatE2ESeed,
		`e2e/seed/main.go run → AddChatMessage(role not a literal: m, TypedBy -)`:              chatE2ESeed,
		// The SQL: the insert (NULL unless a user row has a typer) and the one read.
		`internal/agents/pgstore.go (*PGStore).AddChatMessage → sql typed_by`:         chatOwnerTyped,
		`internal/agents/pgstore.go (*PGStore).OwnerTypedChatMessages → sql typed_by`: chatOwnerTyped,
	}
	got := chatWriterSites(t)
	var wantSites []string
	for site := range want {
		wantSites = append(wantSites, site)
	}
	sort.Strings(wantSites)
	if strings.Join(got, "\n") != strings.Join(wantSites, "\n") {
		t.Errorf("the chat writers changed.\n got:\n  %s\nwant:\n  %s\n"+
			"Classify every new writer of a chat row (who wrote it, and whether a person typed it). Only a line the owner "+
			"TYPED into muster's chat may carry a typer: it is what lets a Chief-chat turn be quoted as the owner's covering "+
			"words (docs/design/session-state.md, step 3b-1; operator decision 13).",
			strings.Join(got, "\n  "), strings.Join(wantSites, "\n  "))
	}
	// 🔴 Only the owner-typed kind may set a typer: a site of any other kind
	// that writes TypedBy is refused whatever the list says.
	for site, kind := range want {
		if kind != chatOwnerTyped && !strings.Contains(site, "TypedBy -)") {
			t.Errorf("%s is classified %q but writes a typer", site, kind)
		}
	}
	// Positive control on the scanner: it reached several packages and found
	// the one setter.
	setters := 0
	for _, g := range got {
		if strings.Contains(g, "TypedBy typedBy") {
			setters++
		}
	}
	if len(got) < 15 || setters != 1 {
		t.Fatalf("scanner found %d sites and %d setters; it is not reading the module", len(got), setters)
	}
}

// chatWriterSites scans the module's non-test Go source (from the repository
// root) for the shapes TestEveryChatWriterIsClassified lists.
func chatWriterSites(t *testing.T) []string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("not the module root %s: %v", root, err)
	}
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
					if k, ok := x.Key.(*ast.Ident); ok && k.Name == "TypedBy" {
						out = append(out, at+"literal TypedBy: "+render(x.Value))
					}
				case *ast.AssignStmt:
					for _, l := range x.Lhs {
						if sel, ok := l.(*ast.SelectorExpr); ok && sel.Sel.Name == "TypedBy" {
							out = append(out, at+"assign "+render(l))
						}
					}
				case *ast.BasicLit:
					if x.Kind == token.STRING && strings.Contains(x.Value, "typed_by") {
						out = append(out, at+"sql typed_by")
					}
				case *ast.CallExpr:
					sel, ok := x.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "AddChatMessage" || len(x.Args) < 2 {
						return true
					}
					role, typed := "?", "-"
					if lit, ok := x.Args[1].(*ast.CompositeLit); ok {
						for _, e := range lit.Elts {
							kv, ok := e.(*ast.KeyValueExpr)
							if !ok {
								continue
							}
							switch k, _ := kv.Key.(*ast.Ident); {
							case k == nil:
							case k.Name == "Role":
								role = render(kv.Value)
							case k.Name == "TypedBy":
								typed = render(kv.Value)
							}
						}
					} else {
						role = "not a literal: " + render(x.Args[1])
					}
					out = append(out, at+"AddChatMessage(role "+role+", TypedBy "+typed+")")
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

// chatWriterDedupe is sorted out without adjacent repeats: a function that writes the
// same row shape twice is one writer.
func chatWriterDedupe(out []string) []string {
	var d []string
	for i, s := range out {
		if i == 0 || s != out[i-1] {
			d = append(d, s)
		}
	}
	return d
}
