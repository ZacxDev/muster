package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
)

// TestARouteToAnOpenCodeSessionIsRefusedBeforeTheQueue: the send queue now
// takes a launcher-bound opencode window on an opted-in host
// (mirrorQueueOff), but a chief-routed message reaches a Claude Code session
// only — refused at the target lookup, "window_gone", before anything is
// queued. The control: a Claude session on the same host gets PAST that
// lookup (it is refused later, for the fixture's missing queue store), so the
// refusal above is the lookup's, not the fixture's.
func TestARouteToAnOpenCodeSessionIsRefusedBeforeTheQueue(t *testing.T) {
	ws := append(ocWindows(), mirror.Window{Session: "main", ID: "@9", Index: 9, Claude: true, ClaudeSession: "0e2e0000-0000-4000-8000-0000000000aa",
		Status: mirror.StatusIdle, StatusSince: 9})
	fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}, windows: ws, statusReport: true, openCodeSend: true}
	s, _ := viewServer(t, fm)
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession})
	refused := func(key string) string {
		t.Helper()
		_, msg := s.resolveRouteTarget(ctx, focus.Subject{Kind: focus.KindMirror, Key: key})
		var r struct {
			Refused string `json:"refused"`
		}
		if msg == "" || json.Unmarshal([]byte(msg), &r) != nil {
			t.Fatalf("%s: not refused (%q)", key, msg)
		}
		return r.Refused
	}
	// Instrument check: the bound opencode window IS sendable through the
	// queue's gate, so the route's refusal is not that gate's.
	if why := mirrorQueueOff(fm.Hosts(ctx)[0], mirror.SessionView{Host: "ws", Session: "main", Window: "@1",
		Runtime: mirror.RuntimeOpenCode, AgentSession: ws[0].AgentSession, AgentSend: true}); why != "" {
		t.Fatalf("instrument check: the bound window's queue is off: %q", why)
	}
	if got := refused(mirror.RuntimeOpenCode + ":" + ws[0].AgentSession); got != RefusedWindowGone {
		t.Fatalf("a route to the bound opencode session: refused %q, want %q", got, RefusedWindowGone)
	}
	if got := refused(ws[2].ClaudeSession); got != RefusedQueueUnavailable {
		t.Fatalf("control: a route to the Claude session: refused %q, want %q (past the lookup)", got, RefusedQueueUnavailable)
	}
}
