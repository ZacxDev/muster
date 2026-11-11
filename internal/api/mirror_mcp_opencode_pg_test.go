package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
)

// A MIRRORED OPENCODE SESSION AT THE DOOR (mirror_mcp.go, "A MIRRORED OPENCODE
// SESSION GETS A NARROWER SET"), over a real Postgres: its host names it by
// its session's KEY (X-Muster-Agent-Session) and marks it (X-Muster-Client:
// opencode); it is bound only to the ONE window showing that session that its
// host sends to, and offered the attention and task tools only.

// The opencode windows the fixture adds to ws: @5 launcher-bound to
// ses_launch, @6 a verified title binding to ses_title, @7 a title match the
// host does not send to (ses_view), @8 and @9 both ses_twice.
func ocMirrorWindows() []mirror.Window {
	oc := func(id, sid string, send bool, bind string) mirror.Window {
		return mirror.Window{Session: "oc", ID: id, Index: 1, Active: true, Runtime: mirror.RuntimeOpenCode, AgentSession: sid,
			AgentSend: send, AgentBind: bind, Activity: 1_700_000_000}
	}
	return []mirror.Window{
		oc("@5", "ses_launch", true, ""),
		oc("@6", "ses_title", false, mirror.AgentBindVerified),
		oc("@7", "ses_view", false, mirror.AgentBindTitleShared),
		oc("@8", "ses_twice", true, ""),
		oc("@9", "ses_twice", true, ""),
	}
}

func newOpenCodeMirrorFixture(t *testing.T) (*attnFixture, string) {
	t.Helper()
	f, ws, _, _ := newMirrorMCPFixture(t)
	f.mirror.mu.Lock()
	f.mirror.windows = append(f.mirror.windows, ocMirrorWindows()...)
	f.mirror.mu.Unlock()
	prev := mirrorBindWait
	mirrorBindWait = 100 * time.Millisecond
	t.Cleanup(func() { mirrorBindWait = prev })
	return f, ws
}

// mirrorAsOpenCode is what muster-mirror forwards for an opencode caller:
// the host's token, the marker and (key != "") the bound session's key.
func mirrorAsOpenCode(token, key string) map[string]string {
	h := map[string]string{"Authorization": "Bearer " + token, mirrorClientHeader: mirrorClientOpenCode}
	if key != "" {
		h[mirrorAgentSessionHeader] = key
	}
	return h
}

func (f *attnFixture) toolList(t *testing.T, hdr map[string]string) string {
	t.Helper()
	code, r, body := f.mirrorRPC(t, hdr, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if code != 200 || r == nil || r.Error != nil {
		t.Fatalf("tools/list: %d %s", code, body)
	}
	var res struct {
		Tools []struct{ Name string } `json:"tools"`
	}
	_ = json.Unmarshal(r.Result, &res)
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	return strings.Join(names, " ")
}

// TestAMirroredOpenCodeSessionGetsTheAttentionAndTaskToolsOnly: tools/list
// for an opencode caller is exactly the attention and task tools (and
// muster_help), its instructions name an opencode session and nothing only
// Claude Code does, and every other tool of the door — named, with a bound
// key — is refused by name and does not run. The Claude Code list beside it
// is unchanged.
//
// 🔴 REGRESSION TEST: at the base the door ignored the marker and the key,
// listing every operator tool to opencode's caller.
func TestAMirroredOpenCodeSessionGetsTheAttentionAndTaskToolsOnly(t *testing.T) {
	f, ws := newOpenCodeMirrorFixture(t)
	const want = "attention_ask attention_wait attention_cancel tasks_list task_get task_comment task_set_status muster_help"
	for name, hdr := range map[string]map[string]string{
		"the marker alone":   mirrorAsOpenCode(ws, ""),
		"marker and key":     mirrorAsOpenCode(ws, "opencode:ses_launch"),
		"the key, no marker": {"Authorization": "Bearer " + ws, mirrorAgentSessionHeader: "opencode:ses_launch"},
		"a marker, any case": {"Authorization": "Bearer " + ws, mirrorClientHeader: " OpenCode "},
	} {
		if got := f.toolList(t, hdr); got != want {
			t.Errorf("%s: tools = %s\nwant %s", name, got, want)
		}
	}
	if got := f.toolList(t, mirrorAs(ws, focusSID)); !strings.Contains(got, "attention_answer") || !strings.Contains(got, "agent_destroy") {
		t.Fatalf("control: the Claude Code list lost its operator tools: %s", got)
	}
	_, r, _ := f.mirrorRPC(t, mirrorAsOpenCode(ws, ""), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	init := string(r.Result)
	if !strings.Contains(init, "mirrored opencode session") || strings.Contains(init, "Claude Code") || !strings.Contains(init, "call attention_ask") {
		t.Fatalf("opencode's initialize: %s", init)
	}
	for _, n := range []string{attentionAnswerTool, agentDestroyTool, chiefAskTool, connectorRequestsListTool} {
		_, r, body := f.mirrorRPC(t, mirrorAsOpenCode(ws, "opencode:ses_launch"), mcpToolCall(n, map[string]any{"id": 1, "answer": "x", "name": "x"}))
		if r == nil {
			t.Fatalf("%s: %s", n, body)
		}
		// Unlisted for this caller, so explained without a definition (serveMCP's
		// explainUnlisted) — never run.
		if text, isErr := toolTextRaw(t, r); !isErr || !strings.Contains(text, n+" is not offered to a mirrored opencode session") {
			t.Errorf("%s by an opencode session: %s", n, text)
		}
	}
}

// TestAMirroredOpenCodeSessionIsBoundByItsKeyOnly: an opencode caller's call
// runs only when its host lists ONE window showing that session that it sends
// to; a title match the host does not send to, two windows, an unknown or
// malformed key, no key, a Claude session in place of the key, and a key
// beside a Claude session are each refused before the tool runs, and nothing
// is asked. A Claude session header naming the opencode session's BARE id is
// no Claude session either.
func TestAMirroredOpenCodeSessionIsBoundByItsKeyOnly(t *testing.T) {
	f, ws := newOpenCodeMirrorFixture(t)
	ask := mcpToolCall(attentionAskTool, map[string]any{"question": "smuggled?"})
	both := mirrorAsOpenCode(ws, "opencode:ses_launch")
	both[mirrorSessionHeader] = focusSID
	claudeOnly := mirrorAsOpenCode(ws, "")
	claudeOnly[mirrorSessionHeader] = focusSID
	for name, c := range map[string]struct {
		hdr map[string]string
		why string
	}{
		"a title match it does not send to": {mirrorAsOpenCode(ws, "opencode:ses_view"), "lists no one window running opencode session \\\"ses_view\\\" that it sends to"},
		"two windows":                       {mirrorAsOpenCode(ws, "opencode:ses_twice"), "lists no one window running opencode session \\\"ses_twice\\\" that it sends to"},
		"an unknown session":                {mirrorAsOpenCode(ws, "opencode:ses_nobody"), "lists no one window running opencode session"},
		"a malformed key":                   {mirrorAsOpenCode(ws, "opencode:../x"), "lists no one window running opencode session"},
		"no key":                            {mirrorAsOpenCode(ws, ""), "is bound by its opencode session"},
		"a Claude session for the key":      {mirrorAsOpenCode(ws, focusSID), "is bound by its opencode session"},
		"a key beside a Claude session":     {both, "is bound by its opencode session"},
		"a Claude session and the marker":   {claudeOnly, "is bound by its opencode session"},
		"the bare id as a Claude session":   {mirrorAs(ws, "ses_launch"), "lists no window running Claude session"},
	} {
		code, r, body := f.mirrorRPC(t, c.hdr, ask)
		if code != http.StatusOK || r == nil {
			t.Errorf("%s: %d %s", name, code, body)
			continue
		}
		if text, isErr := toolText(t, r); !isErr || !strings.Contains(text, c.why) {
			t.Errorf("%s: %s; want %q", name, text, c.why)
		}
	}
	var n int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM attention_asks WHERE question = 'smuggled?'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d smuggled asks (%v)", n, err)
	}
	// CONTROL: both bindings the host sends on are bound.
	for _, key := range []string{"opencode:ses_launch", "opencode:ses_title"} {
		_, r, body := f.mirrorRPC(t, mirrorAsOpenCode(ws, key), ask)
		if r == nil {
			t.Fatalf("%s: %s", key, body)
		}
		if text, isErr := toolText(t, r); isErr || !strings.Contains(text, `"status":"pending"`) {
			t.Fatalf("control %s: %s", key, text)
		}
	}
}

// TestAMirroredOpenCodeSessionAsksAndItsWaitReturnsTheAnswer: the ask is the
// opencode session's — its host and its KEY, shown under its window — and the
// operator's answer reaches its wait (it is never pushed: delivery 'pull').
// The Claude Code session on the same host is another asker.
func TestAMirroredOpenCodeSessionAsksAndItsWaitReturnsTheAnswer(t *testing.T) {
	f, ws := newOpenCodeMirrorFixture(t)
	oc := mirrorAsOpenCode(ws, "opencode:ses_launch")
	_, r, body := f.mirrorRPC(t, oc, mcpToolCall(attentionAskTool, map[string]any{"question": "Ship the opencode door?", "options": []string{"yes", "no"}}))
	if r == nil {
		t.Fatal(body)
	}
	text, _ := toolText(t, r)
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.ID == 0 {
		t.Fatalf("ask: %s", text)
	}
	a, err := f.attn.Get(asUser(1), out.ID)
	if err != nil || a.Asker.MirrorHost != "ws" || a.Asker.MirrorSession != "opencode:ses_launch" || a.Asker.AgentID != 0 {
		t.Fatalf("%v %+v", err, a)
	}
	if src, _ := askSourceParts(a, f.mirror.Hosts(asUser(1))); strings.Contains(src, askNotShown) {
		t.Fatalf("the opencode session's ask is not shown under its window: %q", src)
	}
	rd, hdr := form(url.Values{"option": {"yes"}})
	if w := f.call(t, http.MethodPost, "/attention/asks/"+strconv.FormatInt(out.ID, 10)+"/answer", rd, merge(f.operator(), hdr)); w.Code != http.StatusSeeOther {
		t.Fatalf("answer: %d", w.Code)
	}
	var delivery string
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT coalesce(delivery, '') FROM attention_asks WHERE id = $1`, out.ID).Scan(&delivery); err != nil || delivery != "pull" {
		t.Fatalf("delivery %q (%v), want pull", delivery, err)
	}
	_, r, _ = f.mirrorRPC(t, oc, mcpToolCall(attentionWaitTool, map[string]any{"id": out.ID, "wait_seconds": 5}))
	if text, _ := toolText(t, r); !strings.Contains(text, `"answer":"yes"`) {
		t.Fatalf("wait: %s", text)
	}
	_, r, _ = f.mirrorRPC(t, mirrorAs(ws, focusSID), mcpToolCall(attentionWaitTool, map[string]any{"id": out.ID, "wait_seconds": 0}))
	if text, isErr := toolText(t, r); !isErr || !strings.Contains(text, "no such ask") {
		t.Fatalf("the Claude session's wait on the opencode ask: %s", text)
	}
}

// TestAnOpenCodeHostDispatchsSessionWorksItsOwnTaskAndClaimsNothing: the
// session of an opencode host dispatch — its row records the BARE session id
// — reaches exactly its dispatch's task through the task tools, called by its
// key; another task is no such task. Setting it in_progress claims nothing:
// a claim is judged by its Claude session's window (sessionEnded), which an
// opencode session never has.
func TestAnOpenCodeHostDispatchsSessionWorksItsOwnTaskAndClaimsNothing(t *testing.T) {
	f := newDoneFixture(t)
	task := f.task(t, "Fix the README")
	other := f.task(t, "Someone else's job")
	id, win := f.startRunningHostDispatchOn(t, task.ID)
	const ocSID = "ses_dispatched"
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE host_dispatches SET runtime = 'opencode', agent_session = $2 WHERE id = $1`, id, ocSID); err != nil {
		t.Fatal(err)
	}
	f.setBravoWindows(mirror.Window{Session: "agents", ID: win, Runtime: mirror.RuntimeOpenCode, AgentSession: ocSID, AgentSend: true, Dispatch: id})
	call := func(tool string, args map[string]any) string {
		t.Helper()
		ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.owner, Role: authz.RoleAdmin, Via: authz.ViaMirrorHost})
		ctx = context.WithValue(ctx, mirrorHostCtxKey{}, mirror.Token{ID: 9, OwnerID: f.owner, Host: "host-bravo"})
		req := httptest.NewRequest(http.MethodPost, "/mirror/mcp", strings.NewReader(mcpToolCall(tool, args))).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(mirrorClientHeader, mirrorClientOpenCode)
		req.Header.Set(mirrorAgentSessionHeader, "opencode:"+ocSID)
		w := httptest.NewRecorder()
		f.srv.handleMirrorMCP(w, req)
		return w.Body.String()
	}
	oid := strconv.FormatInt(other.ID, 10)
	if out := call(taskGetTool, map[string]any{"id": task.ID}); !strings.Contains(out, "Fix the README") {
		t.Fatalf("its own task, task_get: %s", out)
	}
	if out := call(taskSetStatusTool, map[string]any{"id": task.ID, "status": "in_progress"}); strings.Contains(out, `"isError":true`) {
		t.Fatalf("its own task, in_progress: %s", out)
	}
	if n, err := f.srv.ext.Notes.Get(f.asOwner, task.ID); err != nil || n.Status != "in_progress" {
		t.Fatalf("the task after in_progress: %v %+v", err, n.Status)
	}
	var links int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM task_sessions WHERE note_id = $1 AND session_id LIKE '%' || $2`, task.ID, ocSID).Scan(&links); err != nil || links != 0 {
		t.Fatalf("the opencode session claimed its task: %d link(s) (%v)", links, err)
	}
	if out := call(taskGetTool, map[string]any{"id": other.ID}); !strings.Contains(out, "no such task: "+oid) {
		t.Fatalf("another task: %s", out)
	}
	if out := call(agentReportDoneTool, map[string]any{"summary": "done"}); !strings.Contains(out, agentReportDoneTool+" is not offered to a mirrored opencode session") {
		t.Fatalf("agent_report_done from an opencode dispatch: %s", out)
	}
}
