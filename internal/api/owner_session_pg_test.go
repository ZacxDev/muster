package api

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
)

// ownersOwnSession (owner_session.go): every tool that acts with the owner's
// authority refuses a session muster started for the chief — its window,
// as its host lists it, carries a dispatch — and keeps working for the owner's
// own untagged sessions. A caller whose window the host does not list is
// refused (the closed direction).

// ownerTools is every owner-authority tool, called as c (each with arguments
// the guard sees before anything else).
func ownerTools(s *Server, ctx context.Context, c toolCaller) map[string]func() string {
	return map[string]func() string{
		"agent_destroy":            func() string { return s.toolAgentDestroy(ctx, c, `{"id": 1}`) },
		"agents_list":              func() string { return s.toolAgentsList(ctx, c, `{}`) },
		"attention_answer":         func() string { return s.toolAttentionAnswer(ctx, c, `{"id": 1, "answer": "yes"}`) },
		"chief_restart":            func() string { return s.toolChiefRestart(ctx, c, `{}`) },
		"connector_requests_list":  func() string { return s.toolConnectorRequestsList(ctx, c) },
		"connector_request_decide": func() string { return s.toolConnectorRequestDecide(ctx, c, `{"id": 1, "decision": "deny"}`) },
		"plugin_messages_list":     func() string { return s.toolPluginMessagesList(ctx, c) },
		"plugin_message_route":     func() string { return s.toolPluginMessageRoute(ctx, c, `{"id": 1}`) },
		"plugin_drafts_list":       func() string { return s.toolPluginDraftsList(ctx, c) },
		"plugin_draft_decide":      func() string { return s.toolPluginDraftDecide(ctx, c, `{"id": 1, "decision": "reject"}`) },
		"route_reply":              func() string { return s.toolRouteReply(ctx, c, `{"route_id": "x", "text": "y"}`) },
		"dispatch_request":         func() string { return s.toolDispatchRequest(ctx, c, `{"target": "pod/claude-code", "prompt": "p"}`) },
		"dispatch_approve":         func() string { return s.toolDispatchApprove(ctx, c, `{"id": "held:00000000000000000000000000000000"}`) },
	}
}

// TestOwnerToolsRefuseASessionMusterStartedForTheChief: each of the thirteen
// tools, called from a window whose listing carries a dispatch, is refused
// with the owner-authority sentence; from the SAME session's window untagged
// none is; from a window the host does not list, each is refused as unknown.
func TestOwnerToolsRefuseASessionMusterStartedForTheChief(t *testing.T) {
	f := newAnswerFixture(t)
	c := toolCaller{asker: attention.Asker{MirrorHost: "ws", MirrorSession: focusSID, Label: "s"}, mirrorWindow: "@1"}
	// The real door's principal: the host's token, as its owner (requireMirrorHostToken).
	ownerCtx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaMirrorHost})
	set := func(dispatch int64) {
		f.mirror.mu.Lock()
		for i := range f.mirror.windows {
			if f.mirror.windows[i].ID == "@1" {
				f.mirror.windows[i].Dispatch = dispatch
			}
		}
		f.mirror.mu.Unlock()
	}
	set(77)
	for name, call := range ownerTools(f.srv, ownerCtx, c) {
		if got := call(); !strings.Contains(got, ownerAuthoritySpawned) {
			t.Errorf("%s from a dispatched window: %s", name, got)
		}
	}
	set(0)
	for name, call := range ownerTools(f.srv, ownerCtx, c) {
		if got := call(); strings.Contains(got, "this tool acts with your owner's authority") {
			t.Errorf("%s from the owner's own window was refused: %s", name, got)
		}
	}
	gone := c
	gone.mirrorWindow = "@404"
	for name, call := range ownerTools(f.srv, ownerCtx, gone) {
		if got := call(); !strings.Contains(got, ownerAuthorityUnknown) {
			t.Errorf("%s from an unlisted window: %s", name, got)
		}
	}
}

// TestMirroredSessionIsCalledOnlyThroughOwnersOwnSession: a ledger over
// internal/api's Go source — mirroredSession() is called by ownersOwnSession
// and the dispatch-hold door's caller check, and nowhere else. It fails when
// the set grows or shrinks.
func TestMirroredSessionIsCalledOnlyThroughOwnersOwnSession(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	call := regexp.MustCompile(`\bc\.mirroredSession\(\)`)
	var got []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for range call.FindAllIndex(b, -1) {
			got = append(got, f)
		}
	}
	slices.Sort(got)
	want := []string{"dispatch_hold_tools.go", "owner_session.go"}
	if !slices.Equal(got, want) {
		t.Fatalf("mirroredSession() is called from %v; want exactly %v (every other owner tool goes through ownersOwnSession)", got, want)
	}
}

// TestAgentDestroyEndsAHostDispatchAndReleasesItsTask: the owner's session
// ends a pending and a running host dispatch of theirs (the task's start is
// released: a new start on it succeeds); the answer says the window stays;
// another owner's row reads as missing.
func TestAgentDestroyEndsAHostDispatchAndReleasesItsTask(t *testing.T) {
	f := newHostFixture(t)
	task := f.task(t, "Destroy me")
	f.ownerTurn(t, f.chief, f.owner, "Do it mirrored on host-bravo.")
	args := hostCall("host/host-bravo/claude", "", true)
	delete(args, "request_key")
	args["task_id"] = task.ID
	id := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, args))
	c := toolCaller{asker: attention.Asker{MirrorHost: "host-bravo", MirrorSession: focusSID, Label: "owner's session"}}
	out := f.srv.toolAgentDestroy(f.asOwner, c, `{"host_dispatch": `+strconv.FormatInt(id, 10)+`}`)
	if !strings.Contains(out, `"ended":true`) || !strings.Contains(out, "still open on host-bravo: close it there") {
		t.Fatalf("agent_destroy host_dispatch: %s", out)
	}
	if r := f.row(t, id); r.Status != plugins.HostDispatchEnded || r.EndedWhy != "ended by owner" {
		t.Fatalf("the row: %+v", r)
	}
	f.ownerTurn(t, f.chief, f.owner, "Start it again on host-charlie.")
	args["target"] = "host/host-charlie/claude"
	again := f.callRaw(f.chief, agentDispatchTool, args)
	second := hostDispatchIDOf(t, again)
	// A running one, ended the same way.
	f.srv.reconcileListing(context.Background(), f.owner, "host-charlie",
		[]mirror.Window{{Session: "agents", ID: "@" + strconv.FormatInt(second, 10), Claude: true, Dispatch: second}}, f.srv.now())
	if r := f.row(t, second); r.Status != plugins.HostDispatchRunning {
		t.Fatalf("not running: %+v", r)
	}
	if out := f.srv.toolAgentDestroy(f.asOwner, c, `{"host_dispatch": `+strconv.FormatInt(second, 10)+`}`); !strings.Contains(out, `"ended":true`) {
		t.Fatalf("a running row: %s", out)
	}
	if b := f.budget(t); b.Live != 0 {
		t.Fatalf("ended dispatches hold slots: %+v", b)
	}
	other := asUser(1)
	if out := f.srv.toolAgentDestroy(other, c, `{"host_dispatch": `+strconv.FormatInt(second, 10)+`}`); !strings.Contains(out, "no such host dispatch") {
		t.Fatalf("another owner's row: %s", out)
	}
}
