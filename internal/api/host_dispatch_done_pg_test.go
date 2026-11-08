package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
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
	f.m.reply = func(_ string, dispatch int64, _ mirror.AgentSpawnReq) (mirror.AgentSpawned, error) {
		return mirror.AgentSpawned{Session: "agents", Window: "@" + strconv.FormatInt(dispatch, 10), Worktree: "/w/x", Branch: "muster/x",
			AgentSession: doneAgentSID}, nil
	}
	f.ownerTurn(t, f.chief, f.owner, "Fix the README typo mirrored on host-bravo.")
	id := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, hostCall("host/host-bravo/claude", "done-1", true)))
	win := "@" + strconv.FormatInt(id, 10)
	agent := mirror.Window{Session: "agents", ID: win, Claude: true, ClaudeSession: doneAgentSID, Dispatch: id, Status: mirror.StatusIdle}
	f.setBravoWindows(agent)
	f.srv.reconcileListing(context.Background(), f.owner, "host-bravo", []mirror.Window{agent}, f.srv.now())
	if r := f.row(t, id); r.Status != plugins.HostDispatchRunning || r.AgentSession != doneAgentSID || r.Window != win {
		t.Fatalf("not running as its session: %+v", r)
	}
	return id, win
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
	f := newHostFixture(t)
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
	f := newHostFixture(t)
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
	f := newHostFixture(t)
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

// TestHostDispatchBoundToNeedsEveryField: the binding rule, field by field —
// with fixture values pairwise distinct, so a mutant that drops or swaps one
// comparison is seen.
func TestHostDispatchBoundToNeedsEveryField(t *testing.T) {
	h := plugins.HostDispatch{Host: "host-bravo", AgentSession: doneAgentSID, Window: "@41"}
	if !hostDispatchBoundTo(h, "host-bravo", doneAgentSID, "@41") {
		t.Fatal("its own session, host and window do not bind")
	}
	for _, c := range []struct{ why, host, sid, window string }{
		{"another host", "host-charlie", doneAgentSID, "@41"},
		{"another session", "host-bravo", doneOtherSID, "@41"},
		{"another window", "host-bravo", doneAgentSID, "@42"},
		{"no window", "host-bravo", doneAgentSID, ""},
	} {
		if hostDispatchBoundTo(h, c.host, c.sid, c.window) {
			t.Errorf("%s binds", c.why)
		}
	}
	empty := plugins.HostDispatch{Host: "host-bravo", Window: "@41"}
	if hostDispatchBoundTo(empty, "host-bravo", "", "@41") {
		t.Error("an empty session binds a row with no session")
	}
	// A row with no window yet, and a caller with none (a per-session host's
	// entry has no window id): nothing to bind to.
	noWindow := plugins.HostDispatch{Host: "host-bravo", AgentSession: doneAgentSID}
	if hostDispatchBoundTo(noWindow, "host-bravo", doneAgentSID, "") {
		t.Error("an empty window binds a row with no window")
	}
}

// TestTheOlderOfTwoHostDispatchesOnOneHostReportsItsOwn: two agents running on
// host-bravo at once, with distinct sessions; the OLDER one reports done — its
// own row is recorded, the newer one's is not. (A lookup that took "the newest
// row on the host" would refuse it.)
func TestTheOlderOfTwoHostDispatchesOnOneHostReportsItsOwn(t *testing.T) {
	f := newHostFixture(t)
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
