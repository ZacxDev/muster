package api

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers"
)

// Delete makes officerAgents an agents store a destroy can remove a row from.
func (s *officerAgents) Delete(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.rows {
		if a.ID == id {
			s.rows = append(s.rows[:i], s.rows[i+1:]...)
			return nil
		}
	}
	return errors.New("no such agent")
}

// mirrorToolDefFuncs are the definition functions mirrorTools calls, by name,
// as THIS test calls them: the source of every tool name the mirror door can
// list. TestEveryMirrorToolIsClassifiedForTheStepUp parses mirrorTools and
// fails when it calls a function this map does not name (a new tool family).
var mirrorToolDefFuncs = map[string]func() []agents.ToolDef{
	"attentionToolDefs":          attentionToolDefs,
	"taskToolDefs":               taskToolDefs,
	"attentionAnswerToolDef":     func() []agents.ToolDef { return []agents.ToolDef{attentionAnswerToolDef()} },
	"agentDestroyToolDef":        func() []agents.ToolDef { return []agents.ToolDef{agentDestroyToolDef()} },
	"chiefAskToolDef":            func() []agents.ToolDef { return []agents.ToolDef{chiefAskToolDef()} },
	"agentsListToolDef":          func() []agents.ToolDef { return []agents.ToolDef{agentsListToolDef()} },
	"chiefRestartToolDef":        func() []agents.ToolDef { return []agents.ToolDef{chiefRestartToolDef()} },
	"connectorDecideToolDefs":    connectorDecideToolDefs,
	"pluginDraftToolDefs":        pluginDraftToolDefs,
	"routeReplyToolDef":          func() []agents.ToolDef { return []agents.ToolDef{routeReplyToolDef()} },
	"pluginMessageRouteToolDefs": pluginMessageRouteToolDefs,
	"dispatchHoldToolDefs":       dispatchHoldToolDefs,
	"pageToolDefs":               pageToolDefs,
	"agentReportDoneToolDef":     func() []agents.ToolDef { return []agents.ToolDef{agentReportDoneToolDef()} },
	"hostOfficerToolDefs":        hostOfficerToolDefs,
	"connectorToolDefs":          connectorToolDefs,
}

// TestEveryMirrorToolIsClassifiedForTheStepUp: every tool the mirror door can
// list is either an owner-authority tool (hostStepUpTools: gated on a host
// officer's machine) or exempt with a reason (hostStepUpExempt) — never both,
// never neither. The set is read from mirrorTools' own source, so a new tool
// family fails here until it is classified.
func TestEveryMirrorToolIsClassifiedForTheStepUp(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "mirror_mcp.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var called []string
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "mirrorTools" {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if id, ok := c.Fun.(*ast.Ident); ok && (strings.HasSuffix(id.Name, "ToolDef") || strings.HasSuffix(id.Name, "ToolDefs")) {
					called = append(called, id.Name)
				}
			}
			return true
		})
	}
	slices.Sort(called)
	called = slices.Compact(called)
	if len(called) < 10 {
		t.Fatalf("instrument check FAILED: mirrorTools calls %d definition functions as parsed", len(called))
	}
	names := 0
	for _, fn := range called {
		defs, ok := mirrorToolDefFuncs[fn]
		if !ok {
			t.Errorf("mirrorTools calls %s, which this ledger does not know: add it, and classify its tools in hostStepUpTools or hostStepUpExempt", fn)
			continue
		}
		for _, d := range defs() {
			names++
			_, gated := hostStepUpTools[d.Name]
			_, exempt := hostStepUpExempt[d.Name]
			if gated == exempt {
				t.Errorf("mirror tool %s is gated=%v exempt=%v: it must be exactly one", d.Name, gated, exempt)
			}
		}
	}
	for fn := range mirrorToolDefFuncs {
		if !slices.Contains(called, fn) {
			t.Errorf("the ledger names %s, which mirrorTools no longer calls: remove it", fn)
		}
	}
	if names < 25 {
		t.Fatalf("instrument check FAILED: %d tool names classified", names)
	}
	// The gated set, pinned exactly: a change is a decision.
	want := []string{agentDestroyTool, attentionAnswerTool, chiefRestartTool, connectorRequestDecideTool, dispatchApproveTool,
		pluginDraftDecideTool, pluginMessageRouteTool, routeReplyToolName, pluginRequestDecideTool}
	slices.Sort(want)
	if got := hostStepUpToolNames(); !slices.Equal(got, want) {
		t.Fatalf("the gated set = %v, want %v", got, want)
	}
}

// TestTheStepUpGateIsALedger: the gate is the mirror door's alone; a confirmed
// decision runs only from the card's hook, which only answerAskFrom reaches.
func TestTheStepUpGateIsALedger(t *testing.T) {
	assertCallers(t, ".", "hostStepUpGate", []string{"mirror_mcp.go:handleMirrorMCP"})
	assertCallers(t, ".", "runStepUpTool", []string{"host_stepup.go:hostDecisionForAnswer"})
	assertCallers(t, ".", "hostDecisionForAnswer", []string{"attention.go:answerAskFrom"})
	assertCallers(t, ".", "destroyHostRow", nil)
	assertCallers(t, ".", "RecordHostSpawnSession", []string{"officer_host.go:followHostSpawnSession"})
	assertCallers(t, ".", "followHostSpawnSession", []string{"officer_host.go:hostInstances"})
}

// TestTheStepUpGateSparesAMachineWithNoHostOfficer: a gated tool from a
// machine with no host officer of the caller's owner is not the gate's — it
// runs as before; a non-gated tool is never the gate's.
func TestTheStepUpGateSparesAMachineWithNoHostOfficer(t *testing.T) {
	r := newHostOfficerRig(t, false)
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: hostOffOwner, Role: authz.RoleAdmin, Via: authz.ViaMirrorHost})
	c := toolCaller{asker: attention.Asker{MirrorHost: hostOffOther, MirrorSession: "55555555-6666-4777-8888-999999999999"}, mirrorWindow: "@3"}
	if _, ok := r.s.hostStepUpGate(ctx, hostOffOther, c, pluginDraftDecideTool, `{}`); ok {
		t.Fatal("the gate took a call from a machine with no host officer")
	}
	other := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 7, Role: authz.RoleMember, Via: authz.ViaMirrorHost})
	if _, ok := r.s.hostStepUpGate(other, hostOffHost, c, pluginDraftDecideTool, `{}`); ok {
		t.Fatal("the gate took another owner's call on a machine of the same name")
	}
	if _, ok := r.s.hostStepUpGate(ctx, hostOffHost, c, attentionAskTool, `{}`); ok {
		t.Fatal("the gate took a tool that is not owner authority")
	}
	// Control: the same gated call on the host officer's machine IS the gate's
	// (with no attention store: a refusal naming the web UI).
	out, ok := r.s.hostStepUpGate(ctx, hostOffHost, c, pluginDraftDecideTool, `{}`)
	if !ok || !strings.Contains(out, "web UI") {
		t.Fatalf("control: the gate on the host officer's machine = %v %s", ok, out)
	}
}

// TestTheBindingChecksOwnerOfficerAndHost (round 1's 🟢4): each of
// officerHostCaller's checks is reached by a case only it refuses — a row
// whose owner is not the officer's, a spawn recorded for another officer
// under the instance's name, and a host row of another host (an officer that
// moved) called from that host — each answered by the BINDING refusal, not a
// later one.
func TestTheBindingChecksOwnerOfficerAndHost(t *testing.T) {
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: hostOffOwner, Role: authz.RoleAdmin, Via: authz.ViaMirrorHost})
	call := func(r *hostOfficerRig, host, sid, window string) string {
		out, ok := r.s.hostOfficerMirrorTool(ctx, host, toolCaller{asker: attention.Asker{MirrorHost: host, MirrorSession: sid}, mirrorWindow: window},
			officers.ToolBrief, ``)
		if !ok {
			t.Fatal("not routed by the host officer rule")
		}
		return out
	}
	const binding = "is an officer tool"
	t.Run("owner", func(t *testing.T) {
		r := newHostOfficerRig(t, false)
		row, _ := r.s.ensureHostInstanceRow(ctx, r.host, 1)
		h, _ := r.spawns.InsertHostSpawn(ctx, offB, officers.InstanceName(offB, 1), hostOffHost)
		_, _ = r.spawns.RecordHostSpawnWindow(ctx, h.ID, hostOffHost, "@91", hostOffSess)
		if out := call(r, hostOffHost, hostOffSess, "@91"); isToolErr(out) {
			t.Fatalf("control: %s", out)
		}
		r.store.mu.Lock()
		for i := range r.store.rows {
			if r.store.rows[i].ID == row.ID {
				r.store.rows[i].OwnerID = 9
			}
		}
		r.store.mu.Unlock()
		if out := call(r, hostOffHost, hostOffSess, "@91"); !strings.Contains(out, binding) {
			t.Fatalf("a row of another owner: %s, want the binding refusal", out)
		}
	})
	t.Run("officer id", func(t *testing.T) {
		r := newHostOfficerRig(t, false)
		_, _ = r.s.ensureHostInstanceRow(ctx, r.host, 1)
		h, _ := r.spawns.InsertHostSpawn(ctx, "other:example", officers.InstanceName(offB, 1), hostOffHost)
		_, _ = r.spawns.RecordHostSpawnWindow(ctx, h.ID, hostOffHost, "@91", hostOffSess)
		if out := call(r, hostOffHost, hostOffSess, "@91"); !strings.Contains(out, binding) {
			t.Fatalf("a spawn recorded for another officer: %s, want the binding refusal", out)
		}
	})
	t.Run("host", func(t *testing.T) {
		r := newHostOfficerRig(t, false)
		r.store.rows = append(r.store.rows, agents.Agent{ID: 88, Name: officers.InstanceName(offB, 1), OwnerID: hostOffOwner, RunsOnHost: hostOffOther})
		h, _ := r.spawns.InsertHostSpawn(ctx, offB, officers.InstanceName(offB, 1), hostOffOther)
		_, _ = r.spawns.RecordHostSpawnWindow(ctx, h.ID, hostOffOther, "@94", hostOffSess)
		if out := call(r, hostOffOther, hostOffSess, "@94"); !strings.Contains(out, binding) {
			t.Fatalf("a moved officer's old host row, from its old host: %s, want the binding refusal", out)
		}
	})
}

// TestAClearedWindowIsFollowed (round 1's 🟢3): a /clear in the recorded
// window (another Claude conversation there) is followed by the next fleet
// read — re-recorded on the spawn — so the instance is re-prompted and closed
// again instead of wedging. A window that runs no Claude yet is left alone.
func TestAClearedWindowIsFollowed(t *testing.T) {
	r := newHostOfficerRig(t, false)
	ctx := context.Background()
	h, _ := r.spawns.InsertHostSpawn(ctx, offB, officers.InstanceName(offB, 1), hostOffHost)
	_, _ = r.spawns.RecordHostSpawnWindow(ctx, h.ID, hostOffHost, "@91", hostOffSess)
	const cleared = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	r.m.list(hostOffHost, mirror.Window{ID: "@91", Session: mirror.AgentSpawnSession, Dispatch: h.ID})
	f := officerFleet{s: r.s}
	if _, err := f.Instances(ctx, r.host); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.spawns.HostSpawnByID(ctx, h.ID); got.AgentSession != hostOffSess {
		t.Fatalf("a window running no Claude moved the record to %q", got.AgentSession)
	}
	r.m.list(hostOffHost, mirror.Window{ID: "@91", Session: mirror.AgentSpawnSession, Dispatch: h.ID, Claude: true, ClaudeSession: cleared})
	insts, err := f.Instances(ctx, r.host)
	if err != nil || !insts[0].Up {
		t.Fatalf("instances = %+v, %v", insts, err)
	}
	if got, _ := r.spawns.HostSpawnByID(ctx, h.ID); got.AgentSession != cleared {
		t.Fatalf("the record = %q, want the cleared window's conversation", got.AgentSession)
	}
	r.m.state[hostOffHost+"/@91"] = mirror.AgentState{Status: mirror.StatusIdle, Session: cleared}
	if err := f.Reprompt(ctx, r.host, 1, "next"); err != nil {
		t.Fatalf("re-prompt after the clear: %v", err)
	}
	if err := f.Stop(ctx, r.host, 1); err != nil {
		t.Fatal(err)
	}
	r.waitCloses(t, 1)
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if r.m.closes[0].Key != cleared {
		t.Fatalf("the close named %q, want the followed conversation", r.m.closes[0].Key)
	}
}

// TestAnInstanceIsClosedOnlyWhenIdle (round 1's 🟢5, code side): a window the
// host reports mid-turn or waiting is not asked to close (agent-close would
// close it at once, without the /exit grace); an idle one is.
func TestAnInstanceIsClosedOnlyWhenIdle(t *testing.T) {
	r := newHostOfficerRig(t, false)
	ctx := context.Background()
	h, _ := r.spawns.InsertHostSpawn(ctx, offB, officers.InstanceName(offB, 1), hostOffHost)
	_, _ = r.spawns.RecordHostSpawnWindow(ctx, h.ID, hostOffHost, "@91", hostOffSess)
	f := officerFleet{s: r.s}
	for _, st := range []string{mirror.StatusBusy, mirror.StatusWaiting} {
		r.m.state[hostOffHost+"/@91"] = mirror.AgentState{Status: st, Session: hostOffSess}
		if err := f.Stop(ctx, r.host, 1); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if _, c, _ := r.m.counts(); c != 0 {
		t.Fatalf("%d closes of a window that is not idle", c)
	}
	r.m.state[hostOffHost+"/@91"] = mirror.AgentState{Status: mirror.StatusIdle, Session: hostOffSess}
	if err := f.Stop(ctx, r.host, 1); err != nil {
		t.Fatal(err)
	}
	r.waitCloses(t, 1)
}

// TestAHostRowIsRemovedWithoutAProvisioner (round 1's 🟡2): a host instance's
// row is removed by destroyAgent's host-row branch — the row alone, never the
// provisioner — when its officer is undeclared or moved; the declared
// officer's own row stays protected; `muster agent rm` (DELETE
// /api/agents/{name}) reaches it with no provisioner, and refuses a pod row
// without one. A pod officer over a host row says how to clear it.
func TestAHostRowIsRemovedWithoutAProvisioner(t *testing.T) {
	r := newHostOfficerRig(t, false)
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: hostOffOwner, Role: authz.RoleAdmin, Via: authz.ViaSession})
	own, err := r.s.ensureHostInstanceRow(ctx, r.host, 1)
	if err != nil {
		t.Fatal(err)
	}
	orphan := agents.Agent{ID: 82, Name: "officer-gone-example-1", OwnerID: hostOffOwner, RunsOnHost: hostOffHost}
	pod := agents.Agent{ID: 83, Name: "brave-otter", OwnerID: hostOffOwner, Namespace: "ns-otter"}
	r.store.mu.Lock()
	r.store.rows = append(r.store.rows, orphan, pod)
	r.store.mu.Unlock()
	refuse := func(context.Context, int64) error {
		t.Fatal("the provisioner's destroy was called for a host row")
		return nil
	}

	if _, err := r.s.destroyAgent(ctx, own.ID, refuse, destroyHooks{}); err == nil {
		t.Fatal("the declared host officer's own row was not protected")
	}
	rm := func(name string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete, "/api/agents/"+name, nil).WithContext(ctx)
		req.SetPathValue("name", name)
		w := httptest.NewRecorder()
		r.s.handleAPIAgentRemove(w, req)
		return w
	}
	if _, err := r.s.destroyAgent(ctx, orphan.ID, refuse, destroyHooks{}); err != nil {
		t.Fatalf("destroy of an undeclared officer's row: %v", err)
	}
	r.s.destroyInFlight.Wait()
	if _, err := r.store.GetByName(ctx, orphan.Name); err == nil {
		t.Error("the undeclared officer's row is still there")
	}
	// A MOVED officer: its instance name holds a row of its OLD host. It is
	// not protected, `muster agent rm` removes it, and the officer then starts
	// on its new host.
	r2 := newHostOfficerRig(t, false)
	moved := agents.Agent{ID: 81, Name: officers.InstanceName(offB, 1), OwnerID: hostOffOwner, RunsOnHost: hostOffOther}
	r2.store.rows = append(r2.store.rows, moved)
	r2.hq.AddItem(offB, "triage.look", "svc:moved")
	if res := r2.rec.Check(context.Background(), offB, true); len(res.ActErrs) != 1 || !strings.Contains(res.ActErrs[0].Error(), "muster agent rm "+moved.Name) {
		t.Fatalf("start over the old host's row = %+v, want the refusal naming the command", res)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/agents/"+moved.Name, nil).WithContext(ctx)
	req.SetPathValue("name", moved.Name)
	w := httptest.NewRecorder()
	r2.s.handleAPIAgentRemove(w, req)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"teardown":"row"`) {
		t.Fatalf("rm of a moved officer's old row: %d %s", w.Code, w.Body)
	}
	r2.s.destroyInFlight.Wait()
	r2.rec.Check(context.Background(), offB, true)
	r2.waitSpawned(t)
	if a, err := r2.store.GetByName(ctx, moved.Name); err != nil || a.RunsOnHost != hostOffHost {
		t.Fatalf("after the rm the instance's row = %+v (%v), want a new one on %s", a, err, hostOffHost)
	}
	if _, err := r.store.GetByName(ctx, own.Name); err != nil {
		t.Error("the declared officer's own row was removed")
	}
	if w := rm(pod.Name); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("rm of a pod row with no provisioner: %d %s, want 503", w.Code, w.Body)
	}
	if w := rm("no-such-agent"); w.Code != http.StatusNotFound {
		t.Fatalf("rm of a missing agent: %d", w.Code)
	}
}

// TestAPodOfficerOverAHostRowSaysHowToClearIt: an officer moved back to pods
// whose instance name still holds a host row is refused at Start with the
// command that clears it — never handed to the provisioner.
func TestAPodOfficerOverAHostRowSaysHowToClearIt(t *testing.T) {
	r := newOfficerRig(t)
	r.store.mu.Lock()
	r.store.rows = append(r.store.rows, agents.Agent{ID: 97, Name: officers.InstanceName(offA, 1), OwnerID: 1, RunsOnHost: hostOffHost})
	r.store.mu.Unlock()
	err := officerFleet{s: r.s}.Start(context.Background(), r.a, 1, "kickoff")
	if err == nil || !strings.Contains(err.Error(), "muster agent rm "+officers.InstanceName(offA, 1)) {
		t.Fatalf("start over a host row: %v, want the actionable refusal", err)
	}
	if d, s, _ := r.prov.counts(); d+s != 0 {
		t.Fatal("the provisioner was asked to start a host row")
	}
}

// TestAHostDecisionRunsOnceAndOnlyForItsOwner: the pending decision is taken
// exactly once; an answer from another owner's phone (or a browser) leaves it
// pending and runs nothing.
func TestAHostDecisionRunsOnceAndOnlyForItsOwner(t *testing.T) {
	r := newHostOfficerRig(t, false)
	r.s.hostDecisions.put(41, hostDecision{owner: hostOffOwner, host: hostOffHost, tool: agentsListTool, args: `{}`})
	dev := func(uid int64) context.Context {
		return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaDevice})
	}
	if handled, err := r.s.hostDecisionForAnswer(dev(9), 41, hostDecisionConfirm); !handled || err == nil {
		t.Fatalf("another owner's phone: %v %v, want refused", handled, err)
	}
	browser := authz.WithPrincipal(context.Background(), authz.Principal{UserID: hostOffOwner, Role: authz.RoleAdmin, Via: authz.ViaSession})
	if _, err := r.s.hostDecisionForAnswer(browser, 41, hostDecisionConfirm); !errors.Is(err, errHostDecisionNeedsPhone) {
		t.Fatalf("the owner's browser: %v", err)
	}
	if _, ok := r.s.hostDecisions.peek(41); !ok {
		t.Fatal("a refused answer consumed the decision")
	}
	if _, ok := r.s.hostDecisions.take(41); !ok {
		t.Fatal("take: not pending")
	}
	if _, ok := r.s.hostDecisions.take(41); ok {
		t.Fatal("a decision was taken twice")
	}
}
