package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The pickers a chat writer can choose a session with, by name — the store's
// own (agents.Store / SessionRuntimeStore / ChatStore) and this package's
// helpers over them. A call to any of them in production code is a site.
var chiefSessionPickers = map[string]bool{
	"DefaultSession": true, "LatestOrCreateSession": true, "CreateSession": true, "CreateSessionOn": true,
	"FreshPrimary": true, "primarySession": true, "deliverySession": true, "chiefPrimaryID": true,
	"sessionFor": true, "DeliverySession": true, "SetSession": true,
}

// TestEveryChiefSessionPickerIsLedgered is THE LEDGER behind primarySession's
// header (chief_chats.go): every place in the module's production code that
// picks a chat session to write into or run a turn in — and what it is for —
// is listed here, so a new writer must say which rule it follows, and one that
// stops following it shows up in review. Compared BOTH WAYS: an unlisted site
// fails, and so does a listed site that is gone.
//
// The classes:
//
//	PRIMARY     a delivery to the chief: goes through primarySession (or its two
//	            wrappers, deliverySession and chiefPrimaryID), the one place.
//	ONE-PLACE   the one place itself (primarySession, deliverySession, the
//	            Deliverer's fallback when it has no Primary hook) and the store's
//	            own election.
//	OWNER       the owner chose the chat: a ?session the page names, a side chat
//	            or fresh primary they asked for, the chat they opened.
//	NOT-A-DELIVERY  a session for something that is not a delivery into a
//	            chief's chat: a new agent's first turn, a job's own session, an
//	            officer's, a memo relayed to a non-chief, a read.
//
// ⚠ IT IS STRUCTURAL: it pins WHICH picker each site calls, not that the site
// is reachable for the chief. The behaviour is TestDeliveriesGoOnlyToThePrimaryChat.
func TestEveryChiefSessionPickerIsLedgered(t *testing.T) {
	want := map[string]string{
		// --- the one place, and the store's election under it
		"internal/api/session_runtime.go (*Server).primarySession → DefaultSession":         "ONE-PLACE",
		"internal/api/session_runtime.go (*Server).primarySession → LatestOrCreateSession":  "ONE-PLACE (a store without 0030)",
		"internal/api/session_runtime.go (*Server).deliverySession → primarySession":        "ONE-PLACE (the chief)",
		"internal/api/session_runtime.go (*Server).deliverySession → LatestOrCreateSession": "NOT-A-DELIVERY (a non-chief agent's latest)",
		"internal/agents/pgstore.go (*PGStore).DefaultSession → LatestOrCreateSession":      "ONE-PLACE (the election)",
		"internal/plugins/deliver.go (*Deliverer).session → DefaultSession":                 "ONE-PLACE (no Primary hook: the store's default)",
		"internal/plugins/deliver.go (*Deliverer).session → CreateSession":                  "ONE-PLACE (a store without 0030: a session per plugin)",
		"internal/api/session_routes.go (*Server).chiefPrimaryID → primarySession":          "ONE-PLACE (by agent id)",

		// --- deliveries to the chief: the primary
		"internal/api/agent_outcome.go (*Server).writeChiefNote → primarySession":              "PRIMARY (outcome, host dispatch and proposal notes)",
		"internal/api/chief_ask_tool.go (*Server).putQuestionToChief → primarySession":         "PRIMARY (a session's question)",
		"internal/api/chief_proposal.go (*Server).ownerViewLine → primarySession":              "PRIMARY (a read of the primary)",
		"internal/api/machine_agents.go (*Server).handleAPIAgentSendMessage → deliverySession": "PRIMARY (`muster chief ask`)",
		"internal/api/task_routing.go (*Server).forwardToTaskAgent → deliverySession":          "PRIMARY (a task mention of the chief)",
		"internal/api/attention_delivery.go (*Server).answerSession → primarySession":          "PRIMARY (an answer with no asking chat)",
		"internal/api/session_routes.go (*Server).routeChiefSession → chiefPrimaryID":          "PRIMARY (a route's note, answer and hand-back)",
		"internal/api/plugin_drafts.go (*Server).noteDraftRefusedToChief → chiefPrimaryID":     "PRIMARY (a refused draft)",
		"internal/api/pr_closed.go (*Server).settleClosedPRMessage → chiefPrimaryID":           "PRIMARY (a closed PR)",
		"internal/api/view_chief.go (*Server).handleViewChief → primarySession":                "PRIMARY (a line typed on a session's view goes to the primary)",
		"internal/api/view_chief.go (*Server).handleViewChiefLog → primarySession":             "PRIMARY (a read of it)",
		"internal/api/chief_panel_ui.go (*Server).writeChiefPanelBodyFor → primarySession":     "PRIMARY (the panel elects it)",

		// --- the owner chose the chat
		"internal/api/agents.go (*Server).resolveSessions → primarySession":                 "OWNER (the chief page opens on the primary)",
		"internal/api/agents.go (*Server).resolveSessions → LatestOrCreateSession":          "OWNER (any other agent's page)",
		"internal/api/agents.go (*Server).resolveSession → LatestOrCreateSession":           "OWNER (no ?session on the socket or the composer)",
		"internal/api/agents.go (*Server).handleAgentSessionCreate → LatestOrCreateSession": "OWNER (New chat on a page)",
		"internal/api/agents.go (*Server).handleAgentSessionCreate → CreateSession":         "OWNER (New chat on a page)",
		"internal/api/agents.go (*Server).activeNotificationRows → LatestOrCreateSession":   "NOT-A-DELIVERY (a deep link)",
		"internal/api/session_runtime.go (*Server).createRuntimeSession → CreateSessionOn":  "OWNER (the runtime picker)",
		"internal/api/chief_chats.go (*Server).openSideChat → CreateSession":                "OWNER (a side chat)",
		"internal/api/chief_chats.go (*Server).openSideChat → CreateSessionOn":              "OWNER (a side chat, its own lane)",
		"internal/api/chief_chats.go (*Server).handleChiefSideChat → primarySession":        "OWNER (elects the primary first)",
		"internal/api/chief_chats.go (*Server).setChatArchived → primarySession":            "OWNER (archive leaves you on the primary)",
		"internal/api/chief_chats.go (*Server).handleChiefFreshPrimary → primarySession":    "OWNER (elects the primary first)",
		"internal/api/chief_chats.go (*Server).handleChiefFreshPrimary → FreshPrimary":      "OWNER (a fresh primary)",

		// --- not a delivery into a chief's chat
		"internal/agentkickoff/agentkickoff.go (*Deliverer).deliver → LatestOrCreateSession": "NOT-A-DELIVERY (a new agent's first turn: its first session)",
		"internal/api/jobs.go (*Server).jobSession → CreateSessionOn":                        "NOT-A-DELIVERY (a job's own session)",
		"internal/api/jobs.go (*Server).jobSession → CreateSession":                          "NOT-A-DELIVERY (a job's own session)",
		"internal/api/officers.go Reprompt → LatestOrCreateSession":                          "NOT-A-DELIVERY (an officer instance)",
		// opencode's own sessions (an opencode server's API), not chat_sessions rows.
		"cmd/muster-mirror/opencode_launch.go runOpenCodeLaunch → CreateSession":                    "NOT-A-DELIVERY (an opencode session)",
		"internal/mirrorhost/agentspawn_opencode.go (*openCodeSpawn).createSession → CreateSession": "NOT-A-DELIVERY (an opencode session)",
		"internal/memorelay/relay.go (*Relay).deliver → sessionFor":                                 "PRIMARY (a memo: SetSession is api.Server.DeliverySession)",
		"internal/memorelay/relay.go (*Relay).sessionFor → LatestOrCreateSession":                   "ONE-PLACE (no SetSession: a relay built without a server)",
		"internal/api/session_runtime.go (*Server).DeliverySession → deliverySession":               "ONE-PLACE (the memo relay's hook)",
		"cmd/muster-server/main.go buildApp → SetSession":                                           "ONE-PLACE (wires the memo relay to DeliverySession)",
		"internal/agents/chief_chats.go (*PGStore).SetArchived → DefaultSession":                    "ONE-PLACE (elects a primary before archiving, F4)",
		"internal/agents/pgstore.go (*PGStore).DefaultSession → CreateSession":                      "ONE-PLACE (every session archived: a new primary)",
	}
	got := chiefSessionPickerSites(t)
	have := map[string]bool{}
	for _, s := range got {
		have[s] = true
	}
	var extra, gone []string
	for _, s := range got {
		if _, ok := want[s]; !ok {
			extra = append(extra, s)
		}
	}
	for s := range want {
		if !have[s] {
			gone = append(gone, s)
		}
	}
	sort.Strings(extra)
	sort.Strings(gone)
	if len(extra) > 0 {
		t.Errorf("a session picker this ledger does not classify — say which rule it follows (a delivery to the chief goes "+
			"through primarySession):\n  %s", strings.Join(extra, "\n  "))
	}
	if len(gone) > 0 {
		t.Errorf("ledgered session pickers that are gone — remove them:\n  %s", strings.Join(gone, "\n  "))
	}
	// Positive control: the scanner reads the module, and sees both a store
	// picker and a helper.
	if !have["internal/api/agent_outcome.go (*Server).writeChiefNote → primarySession"] ||
		!have["internal/agents/pgstore.go (*PGStore).DefaultSession → LatestOrCreateSession"] {
		t.Fatalf("the scanner missed known sites (%d found); it is not reading the module", len(got))
	}
}

// TestEveryTurnDoorCountsTheSession: the doors that count a turn for
// "working…" (countSessionTurn) are exactly these — every one that runs a
// turn in a stored session. A door that stops counting leaves open chats
// without the row; a new counter must be one of them.
func TestEveryTurnDoorCountsTheSession(t *testing.T) {
	want := []string{
		"internal/api/agent.go (*Server).gatewayTurn → countSessionTurn",
		"internal/api/agent_send.go (*Server).startAgentTurn → countSessionTurn",
		"internal/api/agents.go (*Server).chatTurn → countSessionTurn",
		"internal/api/agents.go (*Server).handleAgentWS → countSessionTurn",
		"internal/api/chat_busy.go (*Server).chatTurnWhenFree → countSessionTurn",
	}
	got := callSites(t, map[string]bool{"countSessionTurn": true, "beginEdge": true, "begin": false})
	var calls []string
	for _, s := range got {
		if strings.HasSuffix(s, "→ countSessionTurn") {
			calls = append(calls, s)
		}
	}
	sort.Strings(calls)
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("the turn doors that count the session:\n  %s\nwant:\n  %s", strings.Join(calls, "\n  "), strings.Join(want, "\n  "))
	}
	// And nothing else touches the count's edges directly.
	for _, s := range got {
		if strings.HasSuffix(s, "→ beginEdge") && s != "internal/api/session_turns.go (*Server).countSessionTurn → beginEdge" &&
			s != "internal/api/session_turns.go (*sessionTurnCount).begin → beginEdge" {
			t.Errorf("a direct count outside countSessionTurn: %s", s)
		}
	}
}

func chiefSessionPickerSites(t *testing.T) []string {
	t.Helper()
	return callSites(t, chiefSessionPickers)
}

// callSites is every "<rel path> <func> → <callee>" in the module's non-test
// source where callee is a true key of names (a method call or a plain call),
// deduplicated.
func callSites(t *testing.T, names map[string]bool) []string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("not the module root %s: %v", root, err)
	}
	fset := token.NewFileSet()
	seen := map[string]bool{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == "node_modules" || n == "e2e" || (strings.HasPrefix(n, ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
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
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var callee string
				switch f := c.Fun.(type) {
				case *ast.SelectorExpr:
					callee = f.Sel.Name
				case *ast.Ident:
					callee = f.Name
				}
				if names[callee] {
					seen[filepath.ToSlash(rel)+" "+name+" → "+callee] = true
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
