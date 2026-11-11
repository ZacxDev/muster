package modulegate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// EVERY "IS THERE A PROVISIONER?" DECISION, LEDGERED
//
// 🔴 A WIRED PROVISIONER USED TO MEAN "THIS SERVER DISPATCHES AGENTS", AND ON
// THE DOCKER COMPOSE CHIEF IT DOES NOT. MUSTER_CHIEF_RUNTIME=compose wires a
// provisioner that runs the admin's chief and nothing else
// (internal/api/compose_chief.go). Every place that asked `Provisioner != nil`
// to decide whether to OFFER or PERFORM a dispatch would light up again over
// it — POST /api/agents, agent_dispatch's targets (and, until
// docs/design/chief-dispatch.md phase 2 deleted them, the sidebar's "+" and a
// task card's Dispatch) — while every place that asks it to run the
// chief's own lifecycle must keep answering yes.
//
// So the question has two names now (internal/api/ext.go):
//
//   - lifecycleServed() — a provisioner: start, stop, delete, logs, the
//     chief's provision and replace;
//   - dispatchServed() — a provisioner that dispatches (not the Compose chief).
//
// and this ledger is every site that decides it, by raw nil-check or by
// either name. It fails when the set GROWS (a new site must be classified:
// does it dispatch, or run the chief's lifecycle?) and when it SHRINKS (a
// ledger entry for a site that is gone is coverage that can never fail).
// Each entry's comment is its classification; a reviewer reading a new one
// should ask that one question.
//
// ⚠ IT IS FUNCTION-GRANULAR AND COUNTS SITES, which is what it can say: a new
// check inside an already-ledgered function moves its count. Whether a site
// asks the RIGHT question is the behavioural tests' (TestNothingDispatchesOnTheComposeChief,
// TestTheComposeChiefIsWrittenWhereItsContainerReadsIt).
// ---------------------------------------------------------------------------

var provisionerGateLedger = map[string]int{
	// --- the two predicates themselves (internal/api/ext.go) ---
	"internal/api/ext.go (*Server).lifecycleServed Provisioner!=nil":              1,
	"internal/api/ext.go (*Server).dispatchServed lifecycleServed()":              1,
	"internal/api/ext.go (*Server).requireLifecycleProvisioner lifecycleServed()": 1, // the lifecycle routes' 503
	"internal/api/ext.go (*Server).requireDispatchServed dispatchServed()":        1, // POST /api/agents' second door

	// --- DISPATCH: must read dispatchServed (the Compose chief answers no) ---
	"internal/api/agent_dispatch.go (*Server).startReserved dispatchServed()":                1, // agent_dispatch's and dispatch_approve's start
	"internal/api/task_actions.go (*Server).chiefDispatchDeploymentRefusal dispatchServed()": 1, // agent_dispatch's refusal, the task actions' offer
	"cmd/muster-server/features.go (*app).featureStatuses Provisioner!=nil":                  2, // Dispatch and Claude Code agents, each in the arm AFTER the ComposeChief one

	// --- LIFECYCLE: a provisioner is enough (the chief's own, or an agent that exists) ---
	"cmd/muster-server/features.go (*app).featureStatuses Provisioner==nil":                  1, // the Chief entry: no runtime at all
	"cmd/muster-server/features.go chiefMissing Provisioner==nil":                            1, // features that act through the chief
	"cmd/muster-server/main.go (*app).logBanner Provisioner==nil":                            2, // the boot banner's provisioning lines
	"internal/api/agent_destroy_tool.go (*Server).toolAgentDestroy Provisioner==nil":         1, // destroy an existing agent (never the chief)
	"internal/api/end_session.go (*Server).toolEndSessionPropose Provisioner==nil":           1, // propose ending an existing pod agent (never the chief)
	"internal/api/end_session.go (*Server).acceptEndSession Provisioner==nil":                1, // end it on the owner's tap: destroy, never the chief
	"internal/api/agent_reap.go (*Server).reapStoppedAgents Provisioner==nil":                1, // the stopped-agent reaper (never the chief)
	"internal/api/chief_restart_tool.go (*Server).chiefRestartOn Provisioner!=nil":           1, // chief_restart offered
	"internal/api/chief_restart_tool.go (*Server).toolChiefRestart Provisioner==nil":         1, // chief_restart run
	"internal/api/chief_toggle.go (*Server).chiefToggleAvailable Provisioner!=nil":           1, // Account → Chief drawn (after the Compose owner check)
	"internal/api/compose_chief.go (*Server).reapplyComposeChief lifecycleServed()":          1, // the Compose chief's boot re-apply: its own Start, never a dispatch
	"internal/api/chief_toggle.go (*Server).handleAccountChief Provisioner!=nil":             2, // the switch's Start and Stop
	"internal/api/chief_toggle.go (*Server).handleAccountChief Provisioner==nil":             1, // the switch's create
	"internal/api/ext.go (Extensions).defects Provisioner!=nil":                              1, // the namespace-prefix wiring check
	"internal/api/live_status.go (*Server).instanceIndex Provisioner==nil":                   1, // live status reads
	"internal/api/live_status.go (*Server).instanceIndexWithin Provisioner==nil":             1, // live status reads, bounded
	"internal/api/notes.go (*Server).dismissTask Provisioner!=nil":                           1, // a dismissed task's linked agent torn down
	"internal/api/officers.go (*Server).EnableOfficers Provisioner==nil":                     1, // officers (refused at boot beside the Compose chief)
	"internal/api/consumer_usage.go (*Server).EnableConsumerUsage Provisioner==nil":          1, // consumer usage: reads existing agents' instances, never dispatches
	"internal/api/session_routes.go (*Server).handBackRoute Provisioner==nil":                1, // a route handed back to the chief
	"internal/api/session_routes.go (*Server).deliverRouteAnswer Provisioner!=nil":           1, // a session's answer delivered to the chief as a turn
	"internal/api/task_complete_stop.go (*Server).stopChiefAgentOnComplete Provisioner==nil": 1, // stop the chief's own dispatched agent (no ChiefDispatch here)
	"internal/api/terminal.go (*Server).handleAgentTerminalPage Provisioner==nil":            1, // the Terminal view (the Compose chief's says why it has none)
}

// provisionerGateSites finds every `<x>.Provisioner == nil` / `!= nil` and
// every call of dispatchServed / lifecycleServed in the non-test files of
// internal/api and cmd/muster-server, keyed "<dir>/<file> <func> <form>".
func provisionerGateSites(t *testing.T, root string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, dir := range []string{"internal/api", "cmd/muster-server"} {
		files, err := filepath.Glob(filepath.Join(root, dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			fset := token.NewFileSet()
			af, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", f, err)
			}
			for _, d := range af.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				name := fd.Name.Name
				if fd.Recv != nil && len(fd.Recv.List) > 0 {
					name = "(" + recvType(fd.Recv.List[0].Type) + ")." + name
				}
				key := func(form string) string { return dir + "/" + filepath.Base(f) + " " + name + " " + form }
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.BinaryExpr:
						if (x.Op == token.EQL || x.Op == token.NEQ) &&
							((selectorNamed(x.X, "Provisioner") && isNilIdent(x.Y)) || (selectorNamed(x.Y, "Provisioner") && isNilIdent(x.X))) {
							out[key("Provisioner"+x.Op.String()+"nil")]++
						}
					case *ast.CallExpr:
						if s, ok := x.Fun.(*ast.SelectorExpr); ok && (s.Sel.Name == "dispatchServed" || s.Sel.Name == "lifecycleServed") {
							out[key(s.Sel.Name+"()")]++
						}
					}
					return true
				})
			}
		}
	}
	return out
}

func recvType(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return "*" + recvType(t.X)
	case *ast.Ident:
		return t.Name
	}
	return "?"
}

func selectorNamed(e ast.Expr, name string) bool {
	s, ok := e.(*ast.SelectorExpr)
	return ok && s.Sel.Name == name
}

func isNilIdent(e ast.Expr) bool {
	i, ok := e.(*ast.Ident)
	return ok && i.Name == "nil"
}

func TestEveryProvisionerGateIsLedgered(t *testing.T) {
	got := provisionerGateSites(t, moduleRoot(t))
	// 🔴 POSITIVE CONTROL: the instrument finds the one predicate everything
	// else is built on. An enumeration that found nothing would agree with an
	// empty ledger.
	if got["internal/api/ext.go (*Server).lifecycleServed Provisioner!=nil"] != 1 {
		t.Fatalf("positive control FAILED: the walk did not find lifecycleServed's own nil-check; found %d site(s)", len(got))
	}
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if want, ok := provisionerGateLedger[k]; !ok {
			t.Errorf("NEW provisioner gate: %s (×%d). Decide what it asks — does it offer or perform a DISPATCH "+
				"(use dispatchServed: the Compose chief must answer no) or run the chief's own LIFECYCLE "+
				"(a provisioner is enough)? — and ledger it with that classification.", k, got[k])
		} else if want != got[k] {
			t.Errorf("%s: %d site(s), the ledger says %d. Re-classify the function's checks.", k, got[k], want)
		}
	}
	for k := range provisionerGateLedger {
		if _, ok := got[k]; !ok {
			t.Errorf("ledgered provisioner gate %q no longer exists: remove it from the ledger", k)
		}
	}
	t.Logf("%d provisioner gate site(s) in %d function(s)", sum(got), len(got))
}

func sum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}
