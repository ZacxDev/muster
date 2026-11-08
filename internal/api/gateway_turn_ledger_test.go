package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
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
// module's non-test source (TYPE-CHECKED, the default build) of a gateway turn
// method — by name AND arity, so a pane's two-argument Send is not a candidate —
// or of a transport primitive, keyed "<dir>.<enclosing func> → <receiver
// type>.<method>", with the NUMBER of such calls in that function and WHY it
// carries what it carries. A new door fails this test until it is classified
// here; a door removed fails it too; and so does a SECOND call of the same kind
// added inside a function that already has one (a toolless Chat beside the one
// gatewayTurn is allowed), because the count moved.
//
// ⚠ A gateway turn reached through a method with a different NAME is not a
// candidate, and the e2eterm build tag is not loaded. The behavioural half is
// the doors' own tests (machine_message_tools_pg_test.go,
// report_done_kinds_pg_test.go).

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

// gatewayTurnSite is one row: how many such calls the function makes, and why
// it carries what it carries.
type gatewayTurnSite struct {
	n   int
	why string
}

var gatewayTurnLedger = map[string]gatewayTurnSite{
	// The tool-carrying turns.
	"internal/api.(*Server).runTurnWithTools → api.Gateway.ChatWithTools":                     {1, turnCarriesTools}, // chatTurn (every chat door) and gatewayTurn (the machine message door)
	"internal/agentkickoff.(*Deliverer).firstTurnSender → agentkickoff.Gateway.SendWithTools": {1, turnCarriesTools}, // a gateway agent's first turn (KickoffTurnTools)

	// Toolless, and why.
	"internal/api.(*Server).runTurnWithTools → api.Gateway.Chat":                     {1, turnToolless404},
	"internal/api.(*Server).gatewayTurn → api.Gateway.Chat":                          {1, turnToollessNoTool},
	"internal/agentkickoff.(*Deliverer).firstTurnSender → agentkickoff.Gateway.Send": {1, turnToollessNoTool},
	"internal/api.(*Server).runJobTurn → api.Gateway.Chat":                           {1, turnToollessCC}, // namedJobAgent and the pool chooser refuse every other kind
	"internal/api.officerFleet.Reprompt → api.Gateway.Chat":                          {1, turnToollessCC}, // an officer's instances are created claude-code (officerFleet.Start)
	"internal/memorelay.(*Relay).send → memorelay.Gateway.Send":                      {1, turnToollessCC}, // Relay.classify counts any other kind ResultKindUnsupported
	"internal/api.(*Server).askChiefForNudge → api.Gateway.Chat":                     {1, turnToollessNudge},
	"cmd/ccd/livecheck.main → *agentgateway.Gateway.Chat":                            {1, turnLivecheck},

	// The gateway itself.
	"internal/agentgateway.(*Gateway).Chat → *agentgateway.Gateway.Send":                   {1, turnImplementation},
	"internal/agentgateway.(*Gateway).ChatWithTools → *agentgateway.Gateway.SendWithTools": {1, turnImplementation},
	"internal/agentgateway.(*Gateway).Send → agents.RunToollessTurn":                       {1, turnImplementation},
	"internal/agentgateway.(*Gateway).Send → agents.ChatStream":                            {1, turnImplementation},
	"internal/agentgateway.(*Gateway).SendWithTools → agents.RunToolLoop":                  {1, turnImplementation},
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
	for site, at := range got {
		row, ok := gatewayTurnLedger[site]
		switch {
		case !ok:
			t.Errorf("UNCLASSIFIED gateway turn: %s (%s). A turn a gateway agent can be sent must carry its muster "+
				"tools (gatewayTurnTools / KickoffTurnTools); add it to gatewayTurnLedger with the reason it carries what it does",
				site, strings.Join(at, ", "))
		case len(at) != row.n:
			t.Errorf("%s: %d call(s) (%s); the ledger classifies %d (%s). A second send in one function is a second "+
				"door: classify it", site, len(at), strings.Join(at, ", "), row.n, row.why)
		}
	}
	for site, row := range gatewayTurnLedger {
		if _, ok := got[site]; !ok {
			t.Errorf("gatewayTurnLedger lists %s (%s), and the module no longer calls it there: remove or move the row", site, row.why)
		}
	}
	// The doors that MUST carry tools: neither may go back to a toolless send.
	for _, site := range []string{"internal/api.(*Server).runTurnWithTools → api.Gateway.ChatWithTools",
		"internal/agentkickoff.(*Deliverer).firstTurnSender → agentkickoff.Gateway.SendWithTools"} {
		if gatewayTurnLedger[site].why != turnCarriesTools {
			t.Errorf("%s is classified %q; want %s", site, gatewayTurnLedger[site].why, turnCarriesTools)
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

// scanGatewayTurns type-checks every package of the module (non-test files,
// the default build, against `go list -export` data) and returns site → the
// position of each call.
func scanGatewayTurns(t *testing.T, root string) map[string][]string {
	t.Helper()
	cmd := exec.Command("go", "list", "-export", "-deps", "-json=ImportPath,Dir,GoFiles,Export,Module", "./...")
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v: %s", err, stderr.String())
	}
	type listed struct {
		ImportPath, Dir, Export string
		GoFiles                 []string
		Module                  *struct{ Path string }
	}
	exports := map[string]string{}
	var own []listed
	for dec := json.NewDecoder(bytes.NewReader(out)); ; {
		var lp listed
		if err := dec.Decode(&lp); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		exports[lp.ImportPath] = lp.Export
		if lp.Module != nil && lp.Module.Path == "github.com/ZacxDev/muster" {
			own = append(own, lp)
		}
	}
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		f, ok := exports[path]
		if !ok || f == "" {
			return nil, errors.New("no export data for " + path)
		}
		return os.Open(f)
	})
	qual := func(p *types.Package) string { return p.Name() }
	sites := map[string][]string{}
	for _, lp := range own {
		var files []*ast.File
		for _, name := range lp.GoFiles {
			f, err := parser.ParseFile(fset, filepath.Join(lp.Dir, name), nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
		info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
		if _, err := (&types.Config{Importer: imp}).Check(lp.ImportPath, fset, files, info); err != nil {
			t.Fatalf("type-check %s: %v", lp.ImportPath, err)
		}
		rel, _ := filepath.Rel(root, lp.Dir)
		dir := filepath.ToSlash(rel)
		for _, f := range files {
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
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					name := sel.Sel.Name
					var callee string
					if want, ok := gatewayTurnMethods[name]; ok && want == len(call.Args) {
						if s, ok := info.Selections[sel]; ok && s.Kind() == types.MethodVal {
							callee = types.TypeString(s.Recv(), qual) + "." + name
						}
					} else if gatewayTransportFuncs[name] {
						if obj, ok := info.Uses[sel.Sel].(*types.Func); ok && obj.Pkg() != nil {
							callee = obj.Pkg().Name() + "." + name
						}
					}
					if callee != "" {
						site := dir + "." + funcDeclName(fn) + " → " + callee
						sites[site] = append(sites[site], fset.Position(call.Pos()).String())
					}
					return true
				})
			}
		}
	}
	return sites
}

// funcDeclName is "(*T).M", "T.M" or "F".
func funcDeclName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return fn.Name.Name
	}
	typ := fn.Recv.List[0].Type
	star := false
	if s, ok := typ.(*ast.StarExpr); ok {
		typ, star = s.X, true
	}
	if ix, ok := typ.(*ast.IndexExpr); ok {
		typ = ix.X
	}
	id, ok := typ.(*ast.Ident)
	if !ok {
		return fn.Name.Name
	}
	if star {
		return "(*" + id.Name + ")." + fn.Name.Name
	}
	return id.Name + "." + fn.Name.Name
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
