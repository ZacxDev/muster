package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentoutcome"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// agent_report_done ON THE MIRROR DOOR (host_dispatch_done.go): a host-spawned
// agent, which its spawn prompt tells to call agent_report_done, is offered it
// on its host's mirror MCP; the call records ITS OWN dispatch done (the card
// reads Done, the row stays running and holds its slot) — and no other session
// can record it.

// The fixture's sessions: pairwise distinct, and none equal to the spawn
// reply's default (spawnMirror.SpawnAgent).
const (
	doneAgentSID = "3e5f6071-8293-4a41-9dce-3f4a5b6c7d8e" // the dispatch's own session
	doneOtherSID = "4f607182-93a4-4b52-8edf-4a5b6c7d8e9f" // the owner's other session, same host
)

// startRunningHostDispatch starts a no-task host dispatch on host-bravo whose
// spawn reply names doneAgentSID in window @<id>, and lists that window (so the
// row is running). It returns the dispatch id and its window.
func (f *hostFixture) startRunningHostDispatch(t *testing.T) (int64, string) {
	t.Helper()
	return f.startRunningHostDispatchOn(t, 0)
}

// startRunningHostDispatchOn is startRunningHostDispatch on task taskID (0: none).
func (f *hostFixture) startRunningHostDispatchOn(t *testing.T, taskID int64) (int64, string) {
	t.Helper()
	f.m.reply = func(_ string, dispatch int64, _ mirror.AgentSpawnReq) (mirror.AgentSpawned, error) {
		return mirror.AgentSpawned{Session: "agents", Window: "@" + strconv.FormatInt(dispatch, 10), Worktree: "/w/x", Branch: "muster/x",
			AgentSession: doneAgentSID}, nil
	}
	f.ownerTurn(t, f.chief, f.owner, "Fix the README typo mirrored on host-bravo.")
	args := hostCall("host/host-bravo/claude", "done-1", true)
	if taskID > 0 {
		args["task_id"] = taskID
		delete(args, "request_key")
	}
	id := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, args))
	win := "@" + strconv.FormatInt(id, 10)
	agent := mirror.Window{Session: "agents", ID: win, Claude: true, ClaudeSession: doneAgentSID, Dispatch: id, Status: mirror.StatusIdle}
	f.setBravoWindows(agent)
	f.srv.reconcileListing(context.Background(), f.owner, "host-bravo", []mirror.Window{agent}, f.srv.now())
	if r := f.row(t, id); r.Status != plugins.HostDispatchRunning || r.AgentSession != doneAgentSID || r.Window != win || r.TaskID != taskID {
		t.Fatalf("not running as its session: %+v", r)
	}
	return id, win
}

// newDoneFixture is newHostFixture on a deployment that records outcomes (the
// pod path's outcomesOn, which agent_report_done respects on both doors).
func newDoneFixture(t *testing.T) *hostFixture {
	t.Helper()
	f := newHostFixture(t)
	f.srv.ext.Outcomes = agentoutcome.NewPG(f.pool)
	return f
}

// setBravoWindows is host-bravo's listing in the hub's view.
func (f *hostFixture) setBravoWindows(ws ...mirror.Window) {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	for i := range f.m.hosts {
		if f.m.hosts[i].Host == "host-bravo" {
			f.m.hosts[i].Windows = ws
		}
	}
}

// mirrorDoor posts one JSON-RPC body to the REAL mirror door handler as host's
// token of owner, from Claude session sid, and returns the tool text (or the
// JSON-RPC error message).
func (f *hostFixture) mirrorDoor(t *testing.T, owner int64, host, sid, body string) string {
	t.Helper()
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: owner, Role: authz.RoleAdmin, Via: authz.ViaMirrorHost})
	ctx = context.WithValue(ctx, mirrorHostCtxKey{}, mirror.Token{ID: 9, OwnerID: owner, Host: host})
	req := httptest.NewRequest(http.MethodPost, "/mirror/mcp", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(mirrorSessionHeader, sid)
	w := httptest.NewRecorder()
	f.srv.handleMirrorMCP(w, req)
	var r mcpReply
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("not JSON-RPC (%d): %s", w.Code, w.Body)
	}
	if r.Error != nil {
		return "rpc error: " + r.Error.Message
	}
	return string(r.Result)
}

func (f *hostFixture) reportDone(t *testing.T, owner int64, host, sid, summary string) string {
	t.Helper()
	return f.mirrorDoor(t, owner, host, sid, mcpToolCall(agentReportDoneTool, map[string]any{"summary": summary}))
}

// TestAHostDispatchsOwnSessionReportsItDone: the door lists agent_report_done;
// the dispatch's own session calls it; the row records done with the summary
// and STAYS running (done is not ended: the slot is held until the window
// closes), the card reads Done with the summary, and a second call changes
// nothing. When the window then goes, the row ends and the card keeps saying it
// finished.
func TestAHostDispatchsOwnSessionReportsItDone(t *testing.T) {
	f := newDoneFixture(t)
	id, _ := f.startRunningHostDispatch(t)

	if list := f.mirrorDoor(t, f.owner, "host-bravo", doneAgentSID, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); !strings.Contains(list, `"name":"`+agentReportDoneTool+`"`) {
		t.Fatalf("the mirror door does not list %s: %s", agentReportDoneTool, list)
	}
	const summary = "Opened draft PR acme/widgets#41 fixing the README typo."
	out := f.reportDone(t, f.owner, "host-bravo", doneAgentSID, summary)
	if !strings.Contains(out, "Recorded: your owner is told you finished.") {
		t.Fatalf("the dispatch's own session: %s", out)
	}
	r := f.row(t, id)
	if r.DoneAt.IsZero() || r.DoneSummary != summary {
		t.Fatalf("not recorded done: %+v", r)
	}
	if r.Status != plugins.HostDispatchRunning {
		t.Fatalf("reporting done ENDED the row (its window is still open): %+v", r)
	}
	if b := f.budget(t); b.Live != 1 {
		t.Fatalf("a done dispatch whose window is open must still hold its slot: %+v", b)
	}
	c, err := f.srv.hostDispatchCardFromStore(f.asOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	if c.State != ui.DispatchDone || !strings.Contains(c.Detail, summary) {
		t.Fatalf("the card: %+v", c)
	}
	if again := f.reportDone(t, f.owner, "host-bravo", doneAgentSID, "something else"); !strings.Contains(again, "Already recorded as done.") {
		t.Fatalf("a second report: %s", again)
	}
	if r := f.row(t, id); r.DoneSummary != summary {
		t.Fatalf("a second report rewrote the summary: %+v", r)
	}
	// The window goes: ended as before, still read as finished.
	f.setBravoWindows()
	f.srv.reconcileListing(context.Background(), f.owner, "host-bravo", nil, f.srv.now())
	f.srv.reconcileListing(context.Background(), f.owner, "host-bravo", nil, f.srv.now())
	if r := f.row(t, id); r.Status != plugins.HostDispatchEnded {
		t.Fatalf("its window went: %+v", r)
	}
	if c, _ := f.srv.hostDispatchCardFromStore(f.asOwner, id); c.State != ui.DispatchDone || !strings.Contains(c.Detail, summary) {
		t.Fatalf("the ended card: %+v", c)
	}
}

// TestAnotherSessionCannotReportAHostDispatchDone: the negative half. None of
// these records the dispatch done:
//   - the owner's OTHER session on the same host, in a window it has tagged
//     with the dispatch's id (the tag is the caller's to set, so it is not
//     what finds the row);
//   - the dispatch's own session id, resumed in a DIFFERENT window (the row
//     is bound to its window as well as its session);
//   - the same session id claimed through ANOTHER host's token;
//   - another owner's token for a host of the same name.
//
// Then the dispatch's own session still can.
func TestAnotherSessionCannotReportAHostDispatchDone(t *testing.T) {
	f := newDoneFixture(t)
	id, win := f.startRunningHostDispatch(t)
	agent := mirror.Window{Session: "agents", ID: win, Claude: true, ClaudeSession: doneAgentSID, Dispatch: id, Status: mirror.StatusIdle}
	forged := mirror.Window{Session: "main", ID: "@901", Claude: true, ClaudeSession: doneOtherSID, Dispatch: id, Status: mirror.StatusIdle}
	f.setBravoWindows(agent, forged)
	if out := f.reportDone(t, f.owner, "host-bravo", doneOtherSID, "not mine"); !strings.Contains(out, hostReportDoneNotDispatched) {
		t.Fatalf("another session with a forged tag: %s", out)
	}
	assertNotDone := func(why string) {
		t.Helper()
		if r := f.row(t, id); !r.DoneAt.IsZero() || r.Status != plugins.HostDispatchRunning {
			t.Fatalf("%s recorded it: %+v", why, r)
		}
	}
	assertNotDone("another session")

	// The agent's own session id, resumed in another window (the agent window gone from the listing).
	resumed := mirror.Window{Session: "main", ID: "@902", Claude: true, ClaudeSession: doneAgentSID, Status: mirror.StatusIdle}
	f.setBravoWindows(resumed)
	if out := f.reportDone(t, f.owner, "host-bravo", doneAgentSID, "from elsewhere"); !strings.Contains(out, hostReportDoneNotDispatched) {
		t.Fatalf("its session in another window: %s", out)
	}
	assertNotDone("its session in another window")
	f.setBravoWindows(agent)

	// Another host's token, claiming the agent's session in a window of its own.
	f.m.mu.Lock()
	for i := range f.m.hosts {
		if f.m.hosts[i].Host == "host-charlie" {
			f.m.hosts[i].Windows = []mirror.Window{{Session: "agents", ID: win, Claude: true, ClaudeSession: doneAgentSID, Dispatch: id}}
		}
	}
	f.m.mu.Unlock()
	if out := f.reportDone(t, f.owner, "host-charlie", doneAgentSID, "from charlie"); !strings.Contains(out, hostReportDoneNotDispatched) {
		t.Fatalf("another host's token: %s", out)
	}
	assertNotDone("another host's token")

	// Another owner's token for a host named host-bravo: its hub view lists nothing.
	if out := f.reportDone(t, f.owner+1, "host-bravo", doneAgentSID, "from another owner"); strings.Contains(out, "Recorded") {
		t.Fatalf("another owner: %s", out)
	}
	assertNotDone("another owner")

	if out := f.reportDone(t, f.owner, "host-bravo", doneAgentSID, "Done."); !strings.Contains(out, "Recorded") {
		t.Fatalf("its own session, after all that: %s", out)
	}
}

// TestMarkHostDispatchDoneWritesOnlyItsOwnSessionsRunningRow: the store's own
// WHERE is a second binding, below the tool's: a wrong session, a wrong host,
// an empty session, or a row that is not running writes nothing.
func TestMarkHostDispatchDoneWritesOnlyItsOwnSessionsRunningRow(t *testing.T) {
	f := newDoneFixture(t)
	id, _ := f.startRunningHostDispatch(t)
	for _, c := range []struct{ why, host, sid string }{
		{"another session", "host-bravo", doneOtherSID},
		{"another host", "host-charlie", doneAgentSID},
		{"no session", "host-bravo", ""},
	} {
		if wrote, err := f.plugins.MarkHostDispatchDone(f.asOwner, id, c.host, c.sid, "x"); err != nil || wrote {
			t.Fatalf("%s: wrote=%v err=%v", c.why, wrote, err)
		}
	}
	if r := f.row(t, id); !r.DoneAt.IsZero() {
		t.Fatalf("recorded: %+v", r)
	}
	if _, err := f.srv.endHostDispatch(f.asOwner, f.owner, id, "ended by owner", false); err != nil {
		t.Fatal(err)
	}
	if wrote, err := f.plugins.MarkHostDispatchDone(f.asOwner, id, "host-bravo", doneAgentSID, "x"); err != nil || wrote {
		t.Fatalf("an ended row: wrote=%v err=%v", wrote, err)
	}
}

// TestHostDispatchBoundToNeedsItsOwnWindow: the window half of the binding
// (host and session are compared in HostDispatchForSession's SQL): another
// window, or none, does not bind.
func TestHostDispatchBoundToNeedsItsOwnWindow(t *testing.T) {
	h := plugins.HostDispatch{Host: "host-bravo", AgentSession: doneAgentSID, Window: "@41"}
	if !hostDispatchBoundTo(h, "@41") {
		t.Fatal("its own window does not bind")
	}
	if hostDispatchBoundTo(h, "@42") {
		t.Error("another window binds")
	}
	if hostDispatchBoundTo(plugins.HostDispatch{Host: "host-bravo", AgentSession: doneAgentSID}, "") {
		t.Error("an empty window binds a row with no window")
	}
}

// TestTheOlderOfTwoHostDispatchesOnOneHostReportsItsOwn: two agents running on
// host-bravo at once, with distinct sessions; the OLDER one reports done — its
// own row is recorded, the newer one's is not. (A lookup that took "the newest
// row on the host" would refuse it.)
func TestTheOlderOfTwoHostDispatchesOnOneHostReportsItsOwn(t *testing.T) {
	f := newDoneFixture(t)
	older, olderWin := f.startRunningHostDispatch(t)
	const newerSID = "5a718293-a4b5-4c63-9fe0-5b6c7d8e9fa0"
	f.m.reply = func(_ string, dispatch int64, _ mirror.AgentSpawnReq) (mirror.AgentSpawned, error) {
		return mirror.AgentSpawned{Session: "agents", Window: "@" + strconv.FormatInt(dispatch, 10), Worktree: "/w/y", Branch: "muster/y",
			AgentSession: newerSID}, nil
	}
	f.ownerTurn(t, f.chief, f.owner, "And another one mirrored on host-bravo.")
	newer := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, hostCall("host/host-bravo/claude", "done-2", true)))
	ws := []mirror.Window{
		{Session: "agents", ID: olderWin, Claude: true, ClaudeSession: doneAgentSID, Dispatch: older, Status: mirror.StatusIdle},
		{Session: "agents", ID: "@" + strconv.FormatInt(newer, 10), Claude: true, ClaudeSession: newerSID, Dispatch: newer, Status: mirror.StatusBusy},
	}
	f.setBravoWindows(ws...)
	f.srv.reconcileListing(context.Background(), f.owner, "host-bravo", ws, f.srv.now())
	if out := f.reportDone(t, f.owner, "host-bravo", doneAgentSID, "Older done."); !strings.Contains(out, "Recorded") {
		t.Fatalf("the older dispatch's own session: %s", out)
	}
	if r := f.row(t, older); r.DoneSummary != "Older done." {
		t.Fatalf("the older row: %+v", r)
	}
	if r := f.row(t, newer); !r.DoneAt.IsZero() || r.Status != plugins.HostDispatchRunning {
		t.Fatalf("the newer row was touched: %+v", r)
	}
}

// TestAHostDispatchReportsDoneOnlyWithOutcomesOn: as the pod path, a
// deployment with no outcome store records no done (and says so).
func TestAHostDispatchReportsDoneOnlyWithOutcomesOn(t *testing.T) {
	f := newDoneFixture(t)
	id, _ := f.startRunningHostDispatch(t)
	if !f.srv.outcomesOn() {
		t.Fatal("control: the fixture records outcomes")
	}
	saved := f.srv.ext.Outcomes
	f.srv.ext.Outcomes = nil
	out := f.reportDone(t, f.owner, "host-bravo", doneAgentSID, "Done.")
	f.srv.ext.Outcomes = saved
	if !strings.Contains(out, "this deployment records no outcomes") {
		t.Fatalf("with outcomes off: %s", out)
	}
	if r := f.row(t, id); !r.DoneAt.IsZero() {
		t.Fatalf("recorded with outcomes off: %+v", r)
	}
	if out := f.reportDone(t, f.owner, "host-bravo", doneAgentSID, "Done."); !strings.Contains(out, "Recorded") {
		t.Fatalf("control, outcomes on: %s", out)
	}
}

// TestAHostDoneSummaryIsScrubbed: the recorded summary is the first line, with
// control, format and invisible code points made visible (U+FFFD) — an ANSI
// escape, a bidi override and a zero-width space never reach the card or push.
func TestAHostDoneSummaryIsScrubbed(t *testing.T) {
	f := newDoneFixture(t)
	id, _ := f.startRunningHostDispatch(t)
	raw := "Opened \x1b[31mPR\u202e#41\u200b\nsecond line that must not survive"
	if out := f.reportDone(t, f.owner, "host-bravo", doneAgentSID, raw); !strings.Contains(out, "Recorded") {
		t.Fatalf("report: %s", out)
	}
	const want = "Opened \ufffd[31mPR\ufffd#41\ufffd"
	if r := f.row(t, id); r.DoneSummary != want {
		t.Fatalf("recorded %q, want %q", r.DoneSummary, want)
	}
}

// TestAHostDispatchsSessionWorksItsOwnTaskOnly: on the mirror door, the
// dispatch's own session (the same binding as agent_report_done) may read,
// comment on and set ready_for_review ITS dispatch's task — and nothing else
// is admitted: another of the owner's tasks; the task from another session
// (with a forged tag), from the agent's session resumed in another window,
// through another host's token, or as another owner; and its own task once the
// dispatch has ended.
func TestAHostDispatchsSessionWorksItsOwnTaskOnly(t *testing.T) {
	f := newDoneFixture(t)
	task := f.task(t, "Fix the README")
	other := f.task(t, "Someone else's job")
	id, win := f.startRunningHostDispatchOn(t, task.ID)
	tid, oid := strconv.FormatInt(task.ID, 10), strconv.FormatInt(other.ID, 10)
	call := func(owner int64, host, sid, tool string, args map[string]any) string {
		t.Helper()
		return f.mirrorDoor(t, owner, host, sid, mcpToolCall(tool, args))
	}
	noSuch := func(id string) string { return "no such task: " + id }

	if out := call(f.owner, "host-bravo", doneAgentSID, taskGetTool, map[string]any{"id": task.ID}); strings.Contains(out, noSuch(tid)) || !strings.Contains(out, "Fix the README") {
		t.Fatalf("its own task, task_get: %s", out)
	}
	if out := call(f.owner, "host-bravo", doneAgentSID, taskCommentTool, map[string]any{"id": task.ID, "body": "Draft PR: acme/widgets#41"}); strings.Contains(out, noSuch(tid)) || strings.Contains(out, `"isError":true`) {
		t.Fatalf("its own task, task_comment: %s", out)
	}
	if out := call(f.owner, "host-bravo", doneAgentSID, taskSetStatusTool, map[string]any{"id": task.ID, "status": "ready_for_review"}); strings.Contains(out, `"isError":true`) {
		t.Fatalf("its own task, ready_for_review: %s", out)
	}
	if n, err := f.srv.ext.Notes.Get(f.asOwner, task.ID); err != nil || n.Status != "ready_for_review" {
		t.Fatalf("the task after ready_for_review: %v %+v", err, n.Status)
	}
	// Another of the owner's tasks: not admitted.
	for _, tool := range []string{taskGetTool, taskCommentTool, taskSetStatusTool} {
		if out := call(f.owner, "host-bravo", doneAgentSID, tool, map[string]any{"id": other.ID, "body": "x", "status": "in_progress"}); !strings.Contains(out, noSuch(oid)) {
			t.Fatalf("another task, %s: %s", tool, out)
		}
	}
	// Another session on the same host, its window tagged with the dispatch.
	agent := mirror.Window{Session: "agents", ID: win, Claude: true, ClaudeSession: doneAgentSID, Dispatch: id, Status: mirror.StatusIdle}
	forged := mirror.Window{Session: "main", ID: "@901", Claude: true, ClaudeSession: doneOtherSID, Dispatch: id, Status: mirror.StatusIdle}
	f.setBravoWindows(agent, forged)
	if out := call(f.owner, "host-bravo", doneOtherSID, taskGetTool, map[string]any{"id": task.ID}); !strings.Contains(out, noSuch(tid)) {
		t.Fatalf("another session: %s", out)
	}
	// The agent's session resumed in another window.
	f.setBravoWindows(mirror.Window{Session: "main", ID: "@902", Claude: true, ClaudeSession: doneAgentSID, Status: mirror.StatusIdle})
	if out := call(f.owner, "host-bravo", doneAgentSID, taskGetTool, map[string]any{"id": task.ID}); !strings.Contains(out, noSuch(tid)) {
		t.Fatalf("its session in another window: %s", out)
	}
	f.setBravoWindows(agent)
	// Another host's token, listing the agent's session in a window of its own.
	f.m.mu.Lock()
	for i := range f.m.hosts {
		if f.m.hosts[i].Host == "host-charlie" {
			f.m.hosts[i].Windows = []mirror.Window{{Session: "agents", ID: win, Claude: true, ClaudeSession: doneAgentSID, Dispatch: id}}
		}
	}
	f.m.mu.Unlock()
	if out := call(f.owner, "host-charlie", doneAgentSID, taskGetTool, map[string]any{"id": task.ID}); !strings.Contains(out, noSuch(tid)) {
		t.Fatalf("another host's token: %s", out)
	}
	// Another owner.
	if out := call(f.owner+1, "host-bravo", doneAgentSID, taskGetTool, map[string]any{"id": task.ID}); strings.Contains(out, "Fix the README") {
		t.Fatalf("another owner: %s", out)
	}
	// Control: still admitted, then ended — no longer.
	if out := call(f.owner, "host-bravo", doneAgentSID, taskGetTool, map[string]any{"id": task.ID}); strings.Contains(out, noSuch(tid)) {
		t.Fatalf("control, before the end: %s", out)
	}
	if _, err := f.srv.endHostDispatch(f.asOwner, f.owner, id, "ended by owner", false); err != nil {
		t.Fatal(err)
	}
	if out := call(f.owner, "host-bravo", doneAgentSID, taskGetTool, map[string]any{"id": task.ID}); !strings.Contains(out, noSuch(tid)) {
		t.Fatalf("its own task after the dispatch ended: %s", out)
	}
}

// TestTheSpawnRulesAreRuntimeSpecific: claude's spawn prompt carries the pod
// rules (agent_report_done with no task, ready_for_review on one); opencode's,
// which has no muster tool path, names NO muster tool and ends with its own
// rules, on a task and without one.
func TestTheSpawnRulesAreRuntimeSpecific(t *testing.T) {
	task := notes.Note{ID: 77, Body: "Fix the typo."}
	tools := []string{"agent_report_done", "chief_ask", "attention_", "task_get", "task_comment", "task_set_status", "tasks_list", "route_reply"}
	for _, onTask := range []bool{false, true} {
		claude := spawnPrompt(mirror.SpawnRuntimeClaude, "host-bravo", "acme/widgets", "Do it.", task, onTask)
		oc := spawnPrompt(mirror.SpawnRuntimeOpenCode, "host-bravo", "acme/widgets", "Do it.", task, onTask)
		wantClaude, wantOC := chiefDispatchRulesNoTask, spawnRulesOpenCodeNoTask
		if onTask {
			wantClaude, wantOC = chiefDispatchRules, spawnRulesOpenCode
		}
		if !strings.HasSuffix(claude, wantClaude) {
			t.Errorf("on task %v: claude's prompt does not end with the pod rules", onTask)
		}
		if !strings.HasSuffix(oc, wantOC) {
			t.Errorf("on task %v: opencode's prompt does not end with its own rules", onTask)
		}
		for _, name := range tools {
			if strings.Contains(oc, name) {
				t.Errorf("on task %v: opencode's prompt names %q, a tool it cannot call:\n%s", onTask, name, oc)
			}
		}
		if !onTask && !strings.Contains(claude, "agent_report_done") {
			t.Error("claude's no-task prompt lost agent_report_done")
		}
		if onTask && !strings.Contains(claude, "ready_for_review") {
			t.Error("claude's task prompt lost ready_for_review")
		}
	}
	// A cut task on opencode is not pointed at task_get either.
	long := notes.Note{ID: 78, Body: strings.Repeat("task body line\n", 800)}
	if oc := spawnPrompt(mirror.SpawnRuntimeOpenCode, "host-bravo", "acme/widgets", "", long, true); strings.Contains(oc, "task_get") || !strings.Contains(oc, "[cut:") {
		t.Errorf("opencode's cut task: task_get named or not cut")
	}
}
