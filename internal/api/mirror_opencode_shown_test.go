package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE SESSION A MESSAGE WAS WRITTEN FOR, PAGE AND QUEUE HALF
// (ui.AgentSessionField, mirror.ShownSession): the view of a gated window
// carries the session it shows in its composer; a send, and the queued row it
// becomes, name it; and a message written for one session is never delivered
// while the window shows another. The fake mirror plays the hub's own rule
// (mirror.ShownSessionCheck).
//
// 🔴 REGRESSION TEST. At 2afa5ab8 nothing carried the session the page showed:
// a queued message written for X was delivered to Y after the pane switched.

const (
	shownX = "ses_0fixtureShownX1"
	shownY = "ses_0fixtureShownY1"
)

// shownWindows: @1 launcher-bound, @3 a hand-opened pane its host verified,
// whose list now names Y; @4 Claude Code.
func shownWindows() []mirror.Window {
	return []mirror.Window{
		{Session: "main", ID: "@1", Index: 0, Active: true, Command: "opencode", Runtime: mirror.RuntimeOpenCode, AgentSession: "ses_0fixture1a2b",
			AgentSend: true, Status: mirror.StatusIdle, StatusSince: 7},
		{Session: "main", ID: "@3", Index: 2, Command: "opencode", Runtime: mirror.RuntimeOpenCode, AgentSession: shownY,
			AgentBind: mirror.AgentBindVerified, Status: mirror.StatusIdle, StatusSince: 9},
		{Session: "main", ID: "@4", Index: 3, Claude: true, ClaudeSession: "5a6b7c8d-9e0f-4a1b-8c2d-3e4f5a6b7c8d", Status: mirror.StatusIdle, StatusSince: 10},
	}
}

// TestAGatedViewCarriesTheSessionItShows: the verified window's composer
// names Y (its list's session, which the page's script then keeps equal to
// the stream's); a Claude Code window's names none.
func TestAGatedViewCarriesTheSessionItShows(t *testing.T) {
	fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}, windows: shownWindows(), statusReport: true, openCodeSend: true}
	s, ts := viewServer(t, fm)
	_, page := mirrorGet(t, s, ts, ui.MirrorRef{Host: "ws", Session: "main", Window: "@3"}.ViewPath())
	if !strings.Contains(page, `name="`+ui.AgentSessionField+`" value="`+shownY+`"`) {
		t.Fatalf("the verified window's composer does not name %s", shownY)
	}
	// The script line, pinned whole: on every batch the field takes the
	// session the stream is drawing (its gen), so a view that started over on
	// another session sends naming that one.
	if !strings.Contains(page, `if (m.gen) { var shownEl = document.querySelector('input[name="`+ui.AgentSessionField+`"]'); if (shownEl) shownEl.value = m.gen; }`) {
		t.Fatal("the page's script does not keep the field equal to the stream's session")
	}
	_, claude := mirrorGet(t, s, ts, ui.MirrorRef{Host: "ws", Session: "main", Window: "@4"}.ViewPath())
	if strings.Contains(claude, `name="`+ui.AgentSessionField+`" value=`) {
		t.Fatal("a Claude Code window's composer names an agent session")
	}
}

// TestASendFromAPageShowingAnotherSessionIsRefused: the page's stream still
// shows X while the hub lists Y: refused, in the moved words, nothing pasted;
// a page naming no session (an older page) is refused for the verified pane;
// the page naming Y is sent, and names Y.
func TestASendFromAPageShowingAnotherSessionIsRefused(t *testing.T) {
	fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}, windows: shownWindows(), statusReport: true, openCodeSend: true}
	s, ts := viewServer(t, fm)
	cookie := sessionCookieFor(s)
	form := func(nonce, shown string) url.Values {
		f := url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@3"}, "text": {"for the session shown"}, "nonce": {nonce}}
		if shown != "" {
			f.Set(ui.AgentSessionField, shown)
		}
		return f
	}
	for _, c := range []struct {
		name, nonce, shown, body string
	}{
		{"a page still showing X", "nonce-0123456789abcd01", shownX, mirror.PasteRefusedOpenCodeMovedText},
		{"an older page naming none", "nonce-0123456789abcd02", "", mirror.PasteRefusedOpenCodeUnnamedText},
	} {
		code, body, _ := postSendNonce(t, ts.URL, cookie, form(c.nonce, c.shown))
		if code != http.StatusConflict || body != c.body {
			t.Fatalf("%s: %d %q; want 409 %q", c.name, code, body, c.body)
		}
	}
	_, pastes := fm.v2.snapshot()
	if len(pastes) != 0 {
		t.Fatalf("a refused send was pasted: %+v", pastes)
	}
	if code, body, _ := postSendNonce(t, ts.URL, cookie, form("nonce-0123456789abcd03", shownY)); code != http.StatusNoContent {
		t.Fatalf("the control, a page showing Y: %d %q", code, body)
	}
	fm.mu.Lock()
	shown := append([]string(nil), fm.shown...)
	fm.mu.Unlock()
	if len(shown) != 3 || shown[0] != shownX || shown[1] != "" || shown[2] != shownY {
		t.Fatalf("the sends named %q; want X, none, Y", shown)
	}
}

// TestAQueuedRowWrittenForXIsNeverDeliveredToY: the owner queued for X; the
// pane now shows Y. Delivery refuses it — failed, saying it moved, nothing
// pasted — never superseded to wait and land in Y. A row naming no session
// (from before the column) is refused for the verified pane too; a row for Y
// is delivered; a launcher-bound pane's row naming none still goes.
func TestAQueuedRowWrittenForXIsNeverDeliveredToY(t *testing.T) {
	fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}, windows: shownWindows(), statusReport: true, openCodeSend: true}
	s, _ := viewServer(t, fm)
	now := time.Now()
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaQueue})
	row := func(id int64, window, shown string) sendqueue.Item {
		return sendqueue.Item{ID: id, Owner: 1, Body: "queued", StepUpAt: now, CreatedAt: now, AgentSession: shown,
			Target: sendqueue.Target{Host: "ws", Session: "main", Window: window}}
	}
	for _, c := range []struct {
		name string
		it   sendqueue.Item
		why  string
	}{
		{"written for X, the pane on Y", row(81, "@3", shownX), mirror.PasteRefusedOpenCodeMovedText},
		{"an older row naming none", row(82, "@3", ""), mirror.PasteRefusedOpenCodeUnnamedText},
	} {
		out := s.sendQueueDeliver(ctx, c.it)
		if out.Sent || out.MayHaveDelivered || out.Pasted || !strings.Contains(out.Detail, c.why) {
			t.Fatalf("%s: %+v; want refused, saying %q", c.name, out, c.why)
		}
	}
	if _, pastes := fm.v2.snapshot(); len(pastes) != 0 {
		t.Fatalf("a refused row was pasted: %+v", pastes)
	}
	for _, it := range []sendqueue.Item{row(83, "@3", shownY), row(84, "@1", "")} {
		if out := s.sendQueueDeliver(ctx, it); !out.Sent {
			t.Fatalf("the control %s/%q: %+v", it.Target.Window, it.AgentSession, out)
		}
	}
}

// TestQueueingStoresTheSessionThePageShowed: the queue route stores the
// form's agent_session with the row (the store keeps it: migration 0106, and
// internal/sendqueue's TestAQueuedRowKeepsTheSessionItWasWrittenFor).
func TestQueueingStoresTheSessionThePageShowed(t *testing.T) {
	q := &memQueue{}
	fm := queueMirror()
	s, ts := queueServer(t, fm, q, &turnGateway{})
	form := mirrorQueueForm("main", "@1", "later, for X")
	form.Set(ui.AgentSessionField, shownX)
	if resp, body := postQueue(t, ts, sessionCookieFor(s), form); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("queue: %d %q", resp.StatusCode, body)
	}
	if got := q.all(); len(got) != 1 || got[0].AgentSession != shownX {
		t.Fatalf("stored %+v; want the row written for %s", got, shownX)
	}
}
