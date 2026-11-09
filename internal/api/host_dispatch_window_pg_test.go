package api

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
)

// WHICH WINDOW IS A HOST DISPATCH'S (dispatchWindow, host_dispatch_reconcile.go;
// the mirror door's session binding, mirror_mcp.go): the @muster-dispatch tag
// is untrusted, so it never moves a row's recorded window; and one session in
// two windows binds to the window listed last.

// TestAForgedTagNeverMovesAHostDispatchsWindow: another session's window
// carrying the dispatch's tag, listed AFTER the agent's and then BEFORE it,
// through real reconcileListing passes: the row keeps the window its spawn
// reply recorded, the real agent stays admitted (report done and its task),
// and the forger is refused.
func TestAForgedTagNeverMovesAHostDispatchsWindow(t *testing.T) {
	for _, forgedFirst := range []bool{false, true} {
		f := newDoneFixture(t)
		task := f.task(t, "Fix the README")
		id, win := f.startRunningHostDispatchOn(t, task.ID)
		agent := mirror.Window{Session: "agents", ID: win, Claude: true, ClaudeSession: doneAgentSID, Dispatch: id, Status: mirror.StatusIdle}
		forged := mirror.Window{Session: "main", ID: "@901", Claude: true, ClaudeSession: doneOtherSID, Dispatch: id, Status: mirror.StatusIdle}
		ws := []mirror.Window{agent, forged}
		if forgedFirst {
			ws = []mirror.Window{forged, agent}
		}
		f.setBravoWindows(ws...)
		for i := 0; i < 3; i++ {
			f.srv.reconcileListing(context.Background(), f.owner, "host-bravo", ws, f.srv.now())
		}
		if r := f.row(t, id); r.Window != win || r.Status != plugins.HostDispatchRunning {
			t.Fatalf("forged first %v: the row's window moved: %+v", forgedFirst, r)
		}
		tid := strconv.FormatInt(task.ID, 10)
		if out := f.mirrorDoor(t, f.owner, "host-bravo", doneOtherSID, mcpToolCall(taskGetTool, map[string]any{"id": task.ID})); !strings.Contains(out, "no such task: "+tid) {
			t.Fatalf("forged first %v: the forger read the task: %s", forgedFirst, out)
		}
		if out := f.reportDone(t, f.owner, "host-bravo", doneOtherSID, "forged"); !strings.Contains(out, hostReportDoneNotDispatched) {
			t.Fatalf("forged first %v: the forger reported done: %s", forgedFirst, out)
		}
		if out := f.mirrorDoor(t, f.owner, "host-bravo", doneAgentSID, mcpToolCall(taskGetTool, map[string]any{"id": task.ID})); strings.Contains(out, "no such task") {
			t.Fatalf("forged first %v: the real agent lost its task: %s", forgedFirst, out)
		}
		if out := f.reportDone(t, f.owner, "host-bravo", doneAgentSID, "Real."); !strings.Contains(out, "Recorded") {
			t.Fatalf("forged first %v: the real agent could not report done: %s", forgedFirst, out)
		}
	}
}

// TestDispatchWindowPicksTheRowsOwn: the reconciler's choice among windows
// carrying a row's tag — the recorded window wherever it is listed; with none
// recorded, the row's session's window, else a lone candidate; several
// unmatched, none; another runtime's window never.
func TestDispatchWindowPicksTheRowsOwn(t *testing.T) {
	a := mirror.Window{ID: "@5", Claude: true, ClaudeSession: doneAgentSID, Dispatch: 9}
	b := mirror.Window{ID: "@6", Claude: true, ClaudeSession: doneOtherSID, Dispatch: 9}
	oc := mirror.Window{ID: "@7", Runtime: "opencode", AgentSession: "ses_abc", Dispatch: 9}
	claude := plugins.HostDispatch{Runtime: mirror.SpawnRuntimeClaude}
	cases := []struct {
		why    string
		row    plugins.HostDispatch
		cands  []mirror.Window
		want   string
		wantOK bool
	}{
		{"recorded, listed first", plugins.HostDispatch{Runtime: "claude", Window: "@5"}, []mirror.Window{a, b}, "@5", true},
		{"recorded, listed last", plugins.HostDispatch{Runtime: "claude", Window: "@5"}, []mirror.Window{b, a}, "@5", true},
		{"recorded, not listed", plugins.HostDispatch{Runtime: "claude", Window: "@5"}, []mirror.Window{b}, "", false},
		{"no window, its session", plugins.HostDispatch{Runtime: "claude", AgentSession: doneAgentSID}, []mirror.Window{b, a}, "@5", true},
		{"no window, lone candidate", claude, []mirror.Window{b}, "@6", true},
		{"no window, ambiguous", claude, []mirror.Window{a, b}, "", false},
		{"another runtime", claude, []mirror.Window{oc}, "", false},
		{"opencode, its session", plugins.HostDispatch{Runtime: "opencode", AgentSession: "ses_abc"}, []mirror.Window{a, oc}, "@7", true},
	}
	for _, c := range cases {
		w, ok := dispatchWindow(c.row, c.cands)
		if ok != c.wantOK || w.ID != c.want {
			t.Errorf("%s: got %q %v, want %q %v", c.why, w.ID, ok, c.want, c.wantOK)
		}
	}
}

// TestOneSessionInTwoWindowsBindsToTheLastListed pins the DOCUMENTED guarantee
// (design 3.7): the host forwards only the caller's session id, not its pane,
// so when one Claude session runs in two windows muster binds a call to the
// window listed LAST that runs it, and the dispatch's tools are admitted only
// when that window is the dispatch's own.
func TestOneSessionInTwoWindowsBindsToTheLastListed(t *testing.T) {
	f := newDoneFixture(t)
	task := f.task(t, "Fix the README")
	id, win := f.startRunningHostDispatchOn(t, task.ID)
	agent := mirror.Window{Session: "agents", ID: win, Claude: true, ClaudeSession: doneAgentSID, Dispatch: id, Status: mirror.StatusIdle}
	resumed := mirror.Window{Session: "main", ID: "@902", Claude: true, ClaudeSession: doneAgentSID, Status: mirror.StatusIdle}
	get := func() string {
		return f.mirrorDoor(t, f.owner, "host-bravo", doneAgentSID, mcpToolCall(taskGetTool, map[string]any{"id": task.ID}))
	}
	f.setBravoWindows(resumed, agent)
	if out := get(); strings.Contains(out, "no such task") {
		t.Fatalf("the dispatch's window listed last: %s", out)
	}
	f.setBravoWindows(agent, resumed)
	if out := get(); !strings.Contains(out, "no such task") {
		t.Fatalf("the other window listed last: %s", out)
	}
}
