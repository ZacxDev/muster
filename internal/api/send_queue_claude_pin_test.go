package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// A QUEUED CLAUDE CODE MESSAGE IS FOR THE CLAUDE SESSION IT WAS QUEUED IN
// (mirror's session_pin.go), QUEUE HALF: the queue post stores the window's
// Claude session with the row, and delivery refuses the row — failed, saying
// so, never superseded — once the window holds another session. The fake
// mirror applies the hub's own function (Window.ShownSessionRefusal).
//
// 🔴 REGRESSION TEST. At origin/main 9d0fb60d a Claude row stored no session,
// and was delivered into whatever Claude session the pane held by then.

const (
	claudeQueuedIn  = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	claudeRestarted = "6f5e4d3c-2b1a-4098-8776-5d4c3b2a1f0e"
)

// TestQueueingForAClaudeWindowStoresItsSession: no form field (a Claude
// window's page names none) — the row is stored for the session the window
// holds now.
func TestQueueingForAClaudeWindowStoresItsSession(t *testing.T) {
	q := &memQueue{}
	fm := queueMirror()
	fm.windows[0].ClaudeSession = claudeQueuedIn
	s, ts := queueServer(t, fm, q, &turnGateway{})
	if resp, body := postQueue(t, ts, sessionCookieFor(s), mirrorQueueForm("main", "@1", "later")); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("queue: %d %q", resp.StatusCode, body)
	}
	if got := q.all(); len(got) != 1 || got[0].AgentSession != claudeQueuedIn {
		t.Fatalf("stored %+v; want the row queued for %s", got, claudeQueuedIn)
	}
}

// TestAQueuedClaudeRowIsNeverDeliveredIntoARestartedSession: the window @4
// holds claudeRestarted now. A row queued in claudeQueuedIn is refused with
// the moved words and nothing is pasted; the controls — a row for the session
// held, and a legacy row naming none (queued before this) — are delivered.
func TestAQueuedClaudeRowIsNeverDeliveredIntoARestartedSession(t *testing.T) {
	wins := shownWindows()
	wins[2].ClaudeSession = claudeRestarted
	fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}, windows: wins, statusReport: true, openCodeSend: true}
	s, _ := viewServer(t, fm)
	now := time.Now()
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaQueue})
	row := func(id int64, shown string) sendqueue.Item {
		return sendqueue.Item{ID: id, Owner: 1, Body: "queued", StepUpAt: now, CreatedAt: now, AgentSession: shown,
			Target: sendqueue.Target{Host: "ws", Session: "main", Window: "@4"}}
	}
	out := s.sendQueueDeliver(ctx, row(91, claudeQueuedIn))
	if out.Sent || out.Superseded || out.MayHaveDelivered || out.Pasted || !strings.Contains(out.Detail, mirror.ClaudeMovedText) {
		t.Fatalf("a row queued in another session: %+v; want failed, saying %q", out, mirror.ClaudeMovedText)
	}
	if _, pastes := fm.v2.snapshot(); len(pastes) != 0 {
		t.Fatalf("a refused row was pasted: %+v", pastes)
	}
	for _, it := range []sendqueue.Item{row(92, claudeRestarted), row(93, "")} {
		if out := s.sendQueueDeliver(ctx, it); !out.Sent {
			t.Fatalf("the control queued for %q: %+v", it.AgentSession, out)
		}
	}
	if _, pastes := fm.v2.snapshot(); len(pastes) != 2 {
		t.Fatalf("pasted %+v; want the two controls", pastes)
	}
}
