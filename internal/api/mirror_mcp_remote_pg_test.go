package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
)

// A REMOTE CALLER (mirror_mcp.go, "A remote caller"): a session mirrored to
// ANOTHER muster, calling this one's admin tools through its host's mirror
// with that host's own mirror token here. Over a real Postgres.
//
// ⚠ These tests name the headers and the marker as string literals, so the
// file compiles against the pre-feature tree, and each test is red there for a
// behavioural reason: the old door ignores the marker, lists every tool and
// binds the call to a listed session, which an MCP-only host has none of.

// remoteSID is the calling session on the other muster's host.
const remoteSID = "7e8f9a0b-1c2d-4e3f-8a4b-5c6d7e8f9a0b"

// remoteAs is a remote caller's headers: token, the marker and the session.
func remoteAs(token, sid string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token, "X-Muster-Client": "remote", "X-Muster-Remote-Session": sid}
}

// TestARemoteCallerIsOfferedTheAdminToolsOnly: tools/list for a remote caller
// is exactly the admin tools of the door (and muster_help), marked remote in
// its _meta; a bound caller's list carries no mark (the control). A
// session-bound tool called by name is refused saying why.
func TestARemoteCallerIsOfferedTheAdminToolsOnly(t *testing.T) {
	f, _, _, _ := newMirrorMCPFixture(t)
	remote := mirrorTok('r')
	f.tokens[remote] = mirror.Token{ID: 9, OwnerID: 1, Host: "far"}
	code, r, body := f.mirrorRPC(t, remoteAs(remote, ""), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if code != 200 || r == nil || r.Error != nil {
		t.Fatalf("%d %s", code, body)
	}
	var res struct {
		Tools []struct{ Name string } `json:"tools"`
		Meta  map[string]any          `json:"_meta"`
	}
	_ = json.Unmarshal(r.Result, &res)
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	want := "tasks_list task_get task_comment task_set_status attention_answer agent_destroy agents_list connector_requests_list connector_request_decide plugin_drafts_list plugin_draft_decide plugin_draft_retry plugin_requests_list plugin_request_decide muster_help"
	if strings.Join(names, " ") != want {
		t.Fatalf("tools = %v, want %s", names, want)
	}
	if res.Meta["muster/caller"] != "remote" {
		t.Fatalf("_meta = %v", res.Meta)
	}
	_, r, _ = f.mirrorRPC(t, mirrorAs(remote, ""), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if strings.Contains(string(r.Result), "muster/caller") {
		t.Fatalf("CONTROL: a bound caller's list is marked: %s", r.Result)
	}
	for _, n := range []string{attentionAskTool, attentionWaitTool, chiefAskTool} {
		// Unlisted for this caller, so explained (serveMCP's disabled hook) as a
		// tool error that carries no current_definition: read raw.
		_, r, body := f.mirrorRPC(t, remoteAs(remote, remoteSID), mcpToolCall(n, map[string]any{"question": "q?", "id": 1}))
		if r == nil || !strings.Contains(string(r.Result), `"isError":true`) || !strings.Contains(body, "tied to the calling session") {
			t.Fatalf("%s by a remote caller: %s", n, body)
		}
	}
}

// TestARemoteOwnerAuthorityCallNeedsThePhone: the MCP-only host "far"
// (mirrored to another muster, so this one cannot see whether its machine
// runs a host officer) answering the chief's ask through attention_answer
// files a step-up CARD and answers nothing — although no host officer is
// declared here. The browser and another machine's MCP door cannot answer the
// card; the owner's phone can, and then the answer runs once, its row naming
// that host and the claimed session. Every owner-authority tool a remote
// caller is listed files a card the same way. The same token sent as a BOUND
// caller is refused (the control: the remote path is what reaches the tool).
func TestARemoteOwnerAuthorityCallNeedsThePhone(t *testing.T) {
	f := newAnswerFixture(t)
	remote := mirrorTok('r')
	f.tokens[remote] = mirror.Token{ID: 9, OwnerID: 1, Host: "far"}
	prev := mirrorBindWait
	mirrorBindWait = 0
	t.Cleanup(func() { mirrorBindWait = prev })

	id := f.ask(t, f.chiefTok, "Dispatch task 9?", "dispatch it", "leave it")
	_, r, body := f.mirrorRPC(t, mirrorAs(remote, remoteSID), mcpToolCall(attentionAnswerTool, map[string]any{"id": id, "answer": "dispatch it"}))
	if r == nil {
		t.Fatal(body)
	}
	if text, isErr := toolText(t, r); !isErr {
		t.Fatalf("CONTROL: bound, an MCP-only host's session ran: %s", text)
	}
	_, r, body = f.mirrorRPC(t, remoteAs(remote, remoteSID), mcpToolCall(attentionAnswerTool, map[string]any{"id": id, "answer": "dispatch it"}))
	if r == nil {
		t.Fatal(body)
	}
	text, isErr := toolText(t, r)
	if isErr {
		t.Fatalf("remote attention_answer: %s", text)
	}
	card := stepUpCardOf(t, text)
	if via, _, _ := f.provenance(t, id); via != "" {
		t.Fatalf("the ask was answered (via %q) before the owner confirmed", via)
	}
	// Not the browser, not another machine's MCP door.
	if _, err := f.srv.answerAskFrom(asUser(1), card, hostDecisionConfirm, attention.AnswerSource{}); !errors.Is(err, errHostDecisionNeedsPhone) {
		t.Fatalf("a browser answer of the card: %v", err)
	}
	if text, _ := f.answerAs(t, f.laptop, laptopSID, card, hostDecisionConfirm); !strings.Contains(text, "paired phone") {
		t.Fatalf("an MCP answer of the card: %s", text)
	}
	if via, _, _ := f.provenance(t, id); via != "" {
		t.Fatalf("the ask was answered (via %q) by a refused confirmation", via)
	}
	// The phone: it runs once, as the remote session.
	if _, err := f.srv.answerAskFrom(asDevice(1), card, hostDecisionConfirm, attention.AnswerSource{}); err != nil {
		t.Fatalf("the phone's confirm: %v", err)
	}
	if via, host, sess := f.provenance(t, id); via != "mcp" || host != "far" || sess != remoteSID {
		t.Fatalf("provenance (%q, %q, %q); want (mcp, far, %s)", via, host, sess, remoteSID)
	}

	// Every owner-authority tool a remote caller is LISTED here files a card
	// (the tools whose feature this fixture has off are unlisted, so refused
	// before they could run anything).
	_, r, body = f.mirrorRPC(t, remoteAs(remote, ""), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if r == nil {
		t.Fatal(body)
	}
	var listed struct {
		Tools []struct{ Name string } `json:"tools"`
	}
	_ = json.Unmarshal(r.Result, &listed)
	var gated []string
	for _, tl := range listed.Tools {
		if _, owner := hostStepUpTools[tl.Name]; owner {
			gated = append(gated, tl.Name)
		}
	}
	if len(gated) < 2 {
		t.Fatalf("POSITIVE CONTROL: only %v of the listed tools are owner-authority", gated)
	}
	for _, name := range gated {
		_, r, body := f.mirrorRPC(t, remoteAs(remote, remoteSID), mcpToolCall(name, map[string]any{"id": 1, "answer": "x", "decision": "approve"}))
		if r == nil || !strings.Contains(body, "awaiting_owner_confirmation") {
			t.Errorf("%s by a remote caller filed no step-up card: %s", name, body)
		}
	}
}

// TestARemoteCallIsRefusedWhenItCouldBeBound: a remote-marked call from a host
// that IS mirrored here (ws, online) is refused — its calls are bound to its
// sessions — and so is one that also names a bound caller's session, or names
// no valid session. Nothing is answered.
func TestARemoteCallIsRefusedWhenItCouldBeBound(t *testing.T) {
	f := newAnswerFixture(t)
	remote := mirrorTok('r')
	f.tokens[remote] = mirror.Token{ID: 9, OwnerID: 1, Host: "far"}
	// "gone" is a host this hub knows but that is OFFLINE now: still mirrored
	// here, so still never served unbound.
	gone := mirrorTok('g')
	f.tokens[gone] = mirror.Token{ID: 10, OwnerID: 1, Host: "gone"}
	f.mirror.mu.Lock()
	f.mirror.others = append(f.mirror.others, mirror.HostView{Host: "gone", Online: false})
	f.mirror.mu.Unlock()
	id := f.ask(t, f.chiefTok, "Dispatch task 10?", "dispatch it")
	both := remoteAs(remote, remoteSID)
	both[mirrorSessionHeader] = focusSID
	for name, c := range map[string]struct {
		hdr  map[string]string
		want string
	}{
		"a host mirrored here":          {remoteAs(f.ws, remoteSID), "mirrors its sessions to this muster"},
		"a host mirrored here, offline": {remoteAs(gone, remoteSID), "mirrors its sessions to this muster"},
		"a bound session too":           {both, "never beside"},
		"no session":                    {remoteAs(remote, ""), "not a Claude session id"},
		"a malformed session":           {remoteAs(remote, "../etc"), "not a Claude session id"},
	} {
		code, r, body := f.mirrorRPC(t, c.hdr, mcpToolCall(attentionAnswerTool, map[string]any{"id": id, "answer": "dispatch it"}))
		if code != http.StatusOK || r == nil {
			t.Fatalf("%s: %d %s", name, code, body)
		}
		if text, isErr := toolText(t, r); !isErr || !strings.Contains(text, c.want) {
			t.Errorf("%s: %s; want a refusal containing %q", name, text, c.want)
		}
	}
	if via, _, _ := f.provenance(t, id); via != "" {
		t.Fatalf("the ask was answered (via %q) by a refused call", via)
	}
}

// TestARemoteSessionWorksTheTasksItCreatedOnThatInstance: why the task tools
// are on a remote caller's list. `muster task create`, pointed at THIS
// instance (MUSTER_API_URL) from inside a session mirrored elsewhere, sends
// that session's id (X-Muster-Session-Id, from CLAUDE_CODE_SESSION_ID); the
// task's `created` link names it — so the remote caller's task scope here is
// that task, not empty. Another remote session (the control) sees none.
func TestARemoteSessionWorksTheTasksItCreatedOnThatInstance(t *testing.T) {
	f, _, _, _ := newMirrorMCPFixture(t)
	remote := mirrorTok('r')
	f.tokens[remote] = mirror.Token{ID: 9, OwnerID: 1, Host: "far"}
	req := httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(`{"title":"remote-created","body":"made from a session mirrored elsewhere"}`))
	req.Header.Set("X-Muster-Session-Id", remoteSID)
	req = req.WithContext(asUser(1))
	rec := httptest.NewRecorder()
	f.srv.handleAPITaskCreate(rec, req)
	var created struct{ ID int64 }
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &created) != nil || created.ID == 0 {
		t.Fatalf("task create: %d %s", rec.Code, rec.Body)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM notes WHERE id = $1`, created.ID)
	})
	list := func(sid string) []int64 {
		_, r, body := f.mirrorRPC(t, remoteAs(remote, sid), mcpToolCall(tasksListTool, map[string]any{}))
		if r == nil {
			t.Fatal(body)
		}
		text, isErr := toolText(t, r)
		var out struct{ Tasks []struct{ ID int64 } }
		if isErr || json.Unmarshal([]byte(text), &out) != nil {
			t.Fatalf("tasks_list as %s: %s", sid, text)
		}
		var ids []int64
		for _, tk := range out.Tasks {
			ids = append(ids, tk.ID)
		}
		return ids
	}
	if ids := list(remoteSID); len(ids) != 1 || ids[0] != created.ID {
		t.Fatalf("the creating session's remote tasks_list = %v, want [%d]", ids, created.ID)
	}
	if ids := list(secondSID); len(ids) != 0 {
		t.Fatalf("CONTROL: another remote session's tasks_list = %v, want none", ids)
	}
	_, r, _ := f.mirrorRPC(t, remoteAs(remote, remoteSID), mcpToolCall(taskCommentTool, map[string]any{"id": created.ID, "body": "from far"}))
	if text, isErr := toolText(t, r); isErr || !strings.Contains(text, `"body":"from far"`) {
		t.Fatalf("task_comment: %s", text)
	}
}

// TestARemoteDispatchStartsAfterOnePhoneConfirmation: the LA flow. A session
// mirrored to another muster asks THIS one, as a remote caller, to start an
// agent: dispatch_request files a hold (ungated) and starts nothing;
// dispatch_approve files a step-up card and still starts nothing; the
// owner's phone confirms the card, and exactly one agent starts, its approval
// row naming the remote host and session.
func TestARemoteDispatchStartsAfterOnePhoneConfirmation(t *testing.T) {
	h := newHoldFixture(t)
	remote := mirrorTok('r')
	h.tokens[remote] = mirror.Token{ID: 19, OwnerID: h.owner, Host: "far"}
	call := func(name string, args any) string {
		t.Helper()
		hdr := remoteAs(remote, remoteSID)
		hdr["Content-Type"] = "application/json"
		w := h.connFixture.call(t, http.MethodPost, "/mirror/mcp", strings.NewReader(mcpToolCall(name, args)), hdr)
		var r mcpReply
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &r) != nil || r.Error != nil {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body)
		}
		text, isErr := toolText(t, &r)
		if isErr {
			t.Fatalf("%s: %s", name, text)
		}
		return text
	}
	var held holdAnswerOut
	if err := json.Unmarshal([]byte(call(dispatchRequestTool, gatewayRequest())), &held); err != nil || !plugins.ValidHoldID(held.HoldID) {
		t.Fatalf("dispatch_request: %v %+v", err, held)
	}
	h.nothingStarted(t)
	card := stepUpCardOf(t, call(dispatchApproveTool, map[string]any{"hold_id": held.HoldID}))
	h.nothingStarted(t)
	if _, err := h.srv.answerAskFrom(asDevice(h.owner), card, hostDecisionConfirm, attention.AnswerSource{}); err != nil {
		t.Fatalf("the phone's confirm: %v", err)
	}
	if got := h.ownersAgents(t); len(got) != 1 {
		t.Fatalf("agents after the confirm: %d, want exactly one", len(got))
	}
	if ad, _ := h.auditDetail(t, plugins.ActionDispatchApproved, held.HoldID); ad["approved_from"] != "far/"+remoteSID || ad["requested_from"] != "far/"+remoteSID {
		t.Fatalf("approval row %v: want it to name far and the remote session", ad)
	}
}
