package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
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
	want := "tasks_list task_get task_comment task_set_status attention_answer agent_destroy agents_list connector_requests_list connector_request_decide plugin_drafts_list plugin_draft_decide plugin_requests_list plugin_request_decide muster_help"
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

// TestARemoteCallerRunsAnAdminToolUnboundAndIsNamedOnTheRow: the MCP-only
// host "far" (online nowhere here) answers the chief's ask through
// attention_answer; the row's provenance names that host and the claimed
// session. The same token sent as a BOUND caller is refused (the control: the
// unbound path is what made it run).
func TestARemoteCallerRunsAnAdminToolUnboundAndIsNamedOnTheRow(t *testing.T) {
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
	if text, isErr := toolText(t, r); isErr || !strings.Contains(text, `"status":"answered"`) {
		t.Fatalf("remote attention_answer: %s", text)
	}
	if via, host, sess := f.provenance(t, id); via != "mcp" || host != "far" || sess != remoteSID {
		t.Fatalf("provenance (%q, %q, %q); want (mcp, far, %s)", via, host, sess, remoteSID)
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
	id := f.ask(t, f.chiefTok, "Dispatch task 10?", "dispatch it")
	both := remoteAs(remote, remoteSID)
	both[mirrorSessionHeader] = focusSID
	for name, c := range map[string]struct {
		hdr  map[string]string
		want string
	}{
		"a host mirrored here": {remoteAs(f.ws, remoteSID), "mirrors its sessions to this muster"},
		"a bound session too":  {both, "never beside"},
		"no session":           {remoteAs(remote, ""), "not a Claude session id"},
		"a malformed session":  {remoteAs(remote, "../etc"), "not a Claude session id"},
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
