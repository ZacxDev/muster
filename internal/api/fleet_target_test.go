package api

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/fleet"
	"github.com/ZacxDev/muster/internal/mirror"
)

// THE FLEET'S TARGET RESOLVER, one row per kind of subject (fleetTarget,
// fleetMirrorDelivery), and the SEAM LEDGER: every writer of fleet_actions
// and every caller of the resolver.

func targetHost(interactive, openCodeSend bool, ws ...mirror.Window) mirror.HostView {
	return mirror.HostView{Host: "rig-west", Online: true, Interactive: interactive, StatusReport: true, OpenCodeSend: openCodeSend,
		Windows: mirror.NormalizeWindows(ws, 0)}
}

// TestTheFleetTargetResolverPerSubjectKind: a mirrored Claude Code window and
// a window muster dispatched on a host go through the send queue; a
// launcher-bound opencode pane on a host running -opencode-send goes through
// the same queue (its gated send); any other opencode pane, and a read-only
// host, are view_only; a pod agent is a turn.
func TestTheFleetTargetResolverPerSubjectKind(t *testing.T) {
	cc := mirror.Window{Session: "work", ID: "@41", Claude: true, ClaudeSession: "c1aade00-0000-4000-8000-000000000041", Status: mirror.StatusIdle}
	dispatched := mirror.Window{Session: "work", ID: "@42", Claude: true, ClaudeSession: "c1aade00-0000-4000-8000-000000000042", Status: mirror.StatusIdle, Dispatch: 913}
	bound := mirror.Window{Session: "work", ID: "@43", Runtime: mirror.RuntimeOpenCode, AgentSession: "ses_0b0und43", AgentSend: true, Status: mirror.StatusIdle}
	unbound := mirror.Window{Session: "work", ID: "@44", Runtime: mirror.RuntimeOpenCode, AgentSession: "ses_0unb0d44", Status: mirror.StatusIdle}
	entry := func(h mirror.HostView, w mirror.Window) mirror.SessionView {
		for _, e := range h.Entries() {
			if e.Window == w.ID {
				return e
			}
		}
		t.Fatalf("fixture: window %s not listed", w.ID)
		return mirror.SessionView{}
	}
	all := targetHost(true, true, cc, dispatched, bound, unbound)
	noSend := targetHost(true, false, cc, dispatched, bound, unbound)
	readOnly := targetHost(false, true, cc, dispatched, bound, unbound)
	for _, c := range []struct {
		name         string
		h            mirror.HostView
		w            mirror.Window
		how, refused string
	}{
		{"mirrored Claude Code", all, cc, fleetDeliverQueue, ""},
		{"a mirror-dispatched window", all, dispatched, fleetDeliverQueue, ""},
		{"launcher-bound opencode, host -opencode-send", all, bound, fleetDeliverQueue, ""},
		{"launcher-bound opencode, host without -opencode-send", noSend, bound, "", fleetRefusedViewOnly},
		{"an unbound opencode pane", all, unbound, "", fleetRefusedViewOnly},
		{"a read-only host", readOnly, cc, "", fleetRefusedViewOnly},
	} {
		d := fleetMirrorDelivery(c.h, entry(c.h, c.w))
		if d.how != c.how || d.refused != c.refused {
			t.Errorf("%s: how %q refused %q (%s); want how %q refused %q", c.name, d.how, d.refused, d.why, c.how, c.refused)
		}
	}

	// A pod agent: a turn — when the gateway is on and the agent runs.
	s := New(nil, AuthConfig{}, nil)
	ag := agents.Agent{ID: 7301, OwnerID: 61, Name: "lint-sweeper", Status: agents.StatusRunning}
	s.UseExtensions(Extensions{Agents: oneAgentStore{a: ag}, Gateway: stubGateway{}})
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 61, Role: authz.RoleMember, Via: authz.ViaSession})
	d := s.fleetTarget(ctx, fleet.Action{SubjectKind: "agent", SubjectKey: "7301:88", AgentID: 7301, ChatSessionID: 88})
	if d.how != fleetDeliverTurn || d.agent.ID != 7301 {
		t.Errorf("a pod agent: %+v; want a turn", d)
	}
	other := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 62, Role: authz.RoleMember, Via: authz.ViaSession})
	if d := s.fleetTarget(other, fleet.Action{SubjectKind: "agent", SubjectKey: "7301:88", AgentID: 7301}); d.refused != fleetRefusedNotFound {
		t.Errorf("another owner's agent: %+v; want not_found", d)
	}
}

// TestTheFleetSeamLedger pins the SEAM between the action log and the code
// that writes it, and between a fleet text and where it goes:
//
//   - every function in internal/api that writes fleet_actions (calls
//     FleetActions.Propose or .MoveAction);
//   - every caller of the fleet target resolver (fleetTarget).
//
// It fails when either set GROWS or SHRINKS: a new writer is a new way into
// the log (and must keep its rules: ids-only audit, the one-open rule, the
// re-read); a new resolver caller is a new delivery that must not open-code
// its own target.
func TestTheFleetSeamLedger(t *testing.T) {
	want := map[string]string{
		"Propose":     "fleetFile,fleetProposeHandoffs",
		"MoveAction":  "fleetMove",
		"fleetTarget": "fleetDeliver,fleetFile,fleetProposeHandoffs,fleetStartTurn",
		// fleetMove is the one door every state change goes through.
		"fleetMove": "fleetApprove,fleetDeliver,fleetExpire,fleetReject,fleetRouteTask,fleetSettle,fleetSettleQueued,fleetStartTurn,fleetWrapClose,sendQueueFleetGate",
	}
	got := map[string]map[string]bool{}
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("instrument: no source files (%v)", err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
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
				if _, watched := want[name]; !watched {
					return true
				}
				// Propose and MoveAction only on the action log (…FleetActions.X).
				if name == "Propose" || name == "MoveAction" {
					inner, ok := sel.X.(*ast.SelectorExpr)
					if !ok || inner.Sel.Name != "FleetActions" {
						return true
					}
				}
				if got[name] == nil {
					got[name] = map[string]bool{}
				}
				got[name][fn.Name.Name] = true
				return true
			})
		}
	}
	// The positive control: the instrument found the writers at all.
	if len(got["Propose"]) == 0 || len(got["fleetTarget"]) == 0 {
		t.Fatalf("instrument: found no Propose or fleetTarget call: %v", got)
	}
	for name, w := range want {
		var callers []string
		for c := range got[name] {
			callers = append(callers, c)
		}
		sort.Strings(callers)
		if strings.Join(callers, ",") != w {
			t.Errorf("%s's callers changed:\n got %s\nwant %s\nA new writer of fleet_actions, or a new delivery, is a seam: decide it here.",
				name, strings.Join(callers, ","), w)
		}
	}
}
