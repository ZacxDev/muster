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

// EVERY CALL THAT STARTS AN AGENT TURN THROUGH THE GATEWAY, AND WHAT IT CARRIES.
//
// 🔴 THE BUG CLASS THIS PINS: a door that starts a gateway agent's turn with a
// toolless send (Chat / Send) where a tool-carrying one (ChatWithTools /
// SendWithTools) belongs. The kickoff's first turn was one (#510); the machine
// message door (POST /api/agents/{name}/messages) was another. Each such turn
// tells a gateway agent about muster's tools — agent_report_done above all —
// and offers none of them.
//
// The ledger is the RELATIONSHIP, not a component: every call site in the
// module's non-test source of a gateway turn method (by name AND arity, so
// push.Service.Send and a pane's Send are not counted) or of the transport
// primitives, keyed "<dir>.<enclosing func> → <method>", each with WHY it
// carries what it carries. A new door fails this test until it is classified
// here; a door removed fails it too, so a stale row cannot stand in for one
// that moved.
//
// ⚠ THIS IS SYNTACTIC: it reads the method's NAME and ARITY, not its receiver's
// type. A new type with a 5-argument Chat would be counted (a false row to
// classify, the safe direction); a gateway turn reached through a differently
// named method would not. The behavioural half is the doors' own tests
// (machine_message_tools_pg_test.go, report_done_kinds_pg_test.go).

// gatewayTurnMethods is method name → the arity of the gateway's turn method.
var gatewayTurnMethods = map[string]int{
	"Chat":          5, // ctx, agent, sessionKey, message, emit — toolless
	"ChatWithTools": 8, // ctx, agent, sessionKey, instructions, message, tools, dispatch, emit
	"Send":          5, // ctx, target, sessionKey, message, emit — toolless
	"SendWithTools": 8, // ctx, target, sessionKey, instructions, message, tools, dispatch, emit
}

// gatewayTransportFuncs are the transport primitives a turn ends in: a call to
// one outside internal/agentgateway would be a door that bypasses the gateway.
var gatewayTransportFuncs = map[string]bool{"RunToollessTurn": true, "RunToolLoop": true, "ChatStream": true}

const (
	turnCarriesTools   = "TOOLS"
	turnToollessCC     = "toolless: claude-code agents only (their tools come over MCP; ccd ignores a request's tools)"
	turnToollessNoTool = "toolless: the fallback when gatewayTurnTools/KickoffTurnTools answers not ok (not a gateway agent, or a row with no owner)"
	turnToolless404    = "toolless: the fallback for an image without /v1/responses (a 404 on the FIRST request only)"
	turnToollessNudge  = "toolless BY DESIGN: the chief nudge is a decision check that runs without tools (docs/chief-nudges.md)"
	turnImplementation = "the gateway's own implementation"
	turnLivecheck      = "toolless: a manual live check against one runtime (cmd/ccd/livecheck), not a muster door"
)

var gatewayTurnLedger = map[string]string{
	// The tool-carrying turns.
	"internal/api.runTurnWithTools → ChatWithTools":         turnCarriesTools, // chatTurn (every chat door) and gatewayTurn (the machine message door)
	"internal/agentkickoff.firstTurnSender → SendWithTools": turnCarriesTools, // a gateway agent's first turn (KickoffTurnTools)

	// Toolless, and why.
	"internal/api.runTurnWithTools → Chat":         turnToolless404,
	"internal/api.gatewayTurn → Chat":              turnToollessNoTool,
	"internal/agentkickoff.firstTurnSender → Send": turnToollessNoTool,
	"internal/api.runJobTurn → Chat":               turnToollessCC, // namedJobAgent and the pool chooser refuse every other kind
	"internal/api.Reprompt → Chat":                 turnToollessCC, // an officer's instances are created claude-code (officerFleet.Start)
	"internal/memorelay.send → Send":               turnToollessCC, // Relay.classify counts any other kind ResultKindUnsupported
	"internal/api.askChiefForNudge → Chat":         turnToollessNudge,
	"cmd/ccd/livecheck.main → Chat":                turnLivecheck,

	// The gateway itself.
	"internal/agentgateway.Chat → Send":                   turnImplementation,
	"internal/agentgateway.ChatWithTools → SendWithTools": turnImplementation,
	"internal/agentgateway.Send → RunToollessTurn":        turnImplementation,
	"internal/agentgateway.Send → ChatStream":             turnImplementation,
	"internal/agentgateway.SendWithTools → RunToolLoop":   turnImplementation,
}

func TestEveryGatewayTurnCallSiteIsClassified(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	got := scanGatewayTurns(t, root)
	// Positive control on the scan: it must see the turns this package is known
	// to start, or a broken walk would read as a tidy ledger.
	if len(got) < 10 {
		t.Fatalf("instrument check FAILED: the scan found only %d gateway turn call site(s): %v", len(got), got)
	}
	for site := range got {
		if _, ok := gatewayTurnLedger[site]; !ok {
			t.Errorf("UNCLASSIFIED gateway turn: %s (%s). A turn a gateway agent can be sent must carry its muster "+
				"tools (gatewayTurnTools / KickoffTurnTools); add it to gatewayTurnLedger with the reason it carries what it does",
				site, strings.Join(got[site], ", "))
		}
	}
	for site, why := range gatewayTurnLedger {
		if _, ok := got[site]; !ok {
			t.Errorf("gatewayTurnLedger lists %s (%s), and the module no longer calls it there: remove or move the row", site, why)
		}
	}
	// The doors that MUST carry tools: neither may go back to a toolless send.
	for _, site := range []string{"internal/api.runTurnWithTools → ChatWithTools", "internal/agentkickoff.firstTurnSender → SendWithTools"} {
		if gatewayTurnLedger[site] != turnCarriesTools {
			t.Errorf("%s is classified %q; want %s", site, gatewayTurnLedger[site], turnCarriesTools)
		}
	}
}

// TestTheMachineMessageDoorStartsItsTurnThroughGatewayTurn: the machine message
// handler's turn goes through gatewayTurn (the tool-carrying door), and calls
// the gateway directly nowhere — the ledger above keys by the enclosing
// function, so a handler that went back to s.ext.Gateway.Chat would show up as
// a NEW unclassified row; this names the expected call itself.
func TestTheMachineMessageDoorStartsItsTurnThroughGatewayTurn(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	calls := callsIn(t, filepath.Join(root, "internal/api/machine_agents.go"), "handleAPIAgentSendMessage")
	if !calls["gatewayTurn"] {
		t.Errorf("handleAPIAgentSendMessage does not call gatewayTurn: a gateway agent asked over POST "+
			"/api/agents/{name}/messages would answer without its muster tools (calls: %v)", sortedCallNames(calls))
	}
	for _, m := range []string{"Chat", "ChatWithTools", "Send", "SendWithTools"} {
		if calls[m] {
			t.Errorf("handleAPIAgentSendMessage calls %s directly; its turn must go through gatewayTurn", m)
		}
	}
}

// scanGatewayTurns walks every non-test .go file under root (skipping
// vendored/hidden dirs, node_modules, e2e fixtures) and returns site → positions.
func scanGatewayTurns(t *testing.T, root string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "testdata" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		rel = filepath.ToSlash(rel)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var name string
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					name = fun.Sel.Name
				case *ast.Ident:
					name = fun.Name
				default:
					return true
				}
				if want, ok := gatewayTurnMethods[name]; ok && want == len(call.Args) {
					if _, isSel := call.Fun.(*ast.SelectorExpr); isSel {
						site := rel + "." + fn.Name.Name + " → " + name
						out[site] = append(out[site], fset.Position(call.Pos()).String())
					}
				} else if gatewayTransportFuncs[name] {
					site := rel + "." + fn.Name.Name + " → " + name
					out[site] = append(out[site], fset.Position(call.Pos()).String())
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// callsIn is the set of called names (a selector's Sel, or a bare identifier)
// in function fn of file path.
func callsIn(t *testing.T, path, fn string) map[string]bool {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	found := false
	for _, decl := range f.Decls {
		d, ok := decl.(*ast.FuncDecl)
		if !ok || d.Name.Name != fn || d.Body == nil {
			continue
		}
		found = true
		ast.Inspect(d.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					out[fun.Sel.Name] = true
				case *ast.Ident:
					out[fun.Name] = true
				}
			}
			return true
		})
	}
	if !found {
		t.Fatalf("instrument check FAILED: no func %s in %s", fn, path)
	}
	return out
}

func sortedCallNames(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
