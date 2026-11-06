package api

import (
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE OPENCODE API SEND, PAGE HALF (docs/design/opencode-first-class.md).

// ocWindows: @1 a launcher-bound opencode window (AgentSend), @2 a
// title-matched one; both idle with a session.
func ocWindows() []mirror.Window {
	return []mirror.Window{
		{Session: "main", ID: "@1", Index: 0, Active: true, Command: "opencode", Runtime: mirror.RuntimeOpenCode, AgentSession: "ses_0fixture1a2b",
			AgentSend: true, Status: mirror.StatusIdle, StatusSince: 7},
		{Session: "main", ID: "@2", Index: 1, Command: "opencode", Runtime: mirror.RuntimeOpenCode, AgentSession: "ses_0fixture1a2c",
			Status: mirror.StatusIdle, StatusSince: 8},
	}
}

// composerTag is the composer input's opening tag on page.
func composerTag(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, `id="mv-input"`)
	if i < 0 {
		t.Fatal("no composer")
	}
	tag := page[strings.LastIndex(page[:i], "<"):]
	return tag[:strings.Index(tag, ">")]
}

// TestABoundOpenCodeWindowOnAnOptedInHostCanBeSentTo: the composer is
// enabled and names opencode; no suggested replies (Claude Code's). Its queue
// is mirrorQueueOff's (TestTheQueueIsOpenForASendableOpenCodeWindowOnly).
func TestABoundOpenCodeWindowOnAnOptedInHostCanBeSentTo(t *testing.T) {
	fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}, windows: ocWindows(), statusReport: true, openCodeSend: true}
	s, ts := viewServer(t, fm)
	resp, page := mirrorGet(t, s, ts, ui.MirrorRef{Host: "ws", Session: "main", Window: "@1"}.ViewPath())
	if resp.StatusCode != 200 {
		t.Fatalf("view = %d", resp.StatusCode)
	}
	if tag := composerTag(t, page); disabledAttr.MatchString(tag) {
		t.Fatalf("the bound window's composer is disabled: %s", tag)
	}
	for _, want := range []string{"Message opencode…", "opencode is working…"} {
		if !strings.Contains(page, want) {
			t.Errorf("the bound window's view lacks %q", want)
		}
	}
	if strings.Contains(page, `id="mv-readonly"`) || strings.Contains(page, "Claude is working") {
		t.Fatal("the bound window's view says it is read-only, or calls the agent Claude")
	}
}

// TestAnOpenCodeComposerSaysWhyItCannotSend: on a host that did not opt in,
// and for a window not started by the launcher, the composer is disabled with
// no queue, saying which — each in its own words, pinned literally.
func TestAnOpenCodeComposerSaysWhyItCannotSend(t *testing.T) {
	for _, c := range []struct {
		name   string
		sends  bool
		window string
		want   string
	}{
		{"the host did not opt in", false, "@1", "Not sent: this host's muster-mirror does not send to opencode (it needs -interactive and -opencode-send). Type it in the terminal."},
		{"not launcher-bound", true, "@2", "Not sent: muster can't be sure which session this opencode shows, because it wasn't started with `muster-mirror opencode`. Type it in the terminal, or restart it that way."},
	} {
		t.Run(c.name, func(t *testing.T) {
			fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}, windows: ocWindows(), statusReport: true, openCodeSend: c.sends}
			s, ts := viewServer(t, fm)
			_, page := mirrorGet(t, s, ts, ui.MirrorRef{Host: "ws", Session: "main", Window: c.window}.ViewPath())
			if !strings.Contains(page, html.EscapeString(c.want)) {
				t.Fatalf("the composer does not say %q", c.want)
			}
			if tag := composerTag(t, page); !disabledAttr.MatchString(tag) {
				t.Fatalf("enabled: %s", tag)
			}
			if strings.Contains(page, ui.SendQueuePath) {
				t.Fatal("a send queue is offered")
			}
		})
	}
}

// TestASendAgainOfAMaybeLandedMessageReusesItsFirstKey: 🔴 a send that may
// have landed (unconfirmed) is HELD under the key it was sent with; the
// person's Send again — a NEW nonce — is sent under that FIRST key, so a gated
// send posts the same opencode message id and lands at most once. A confirmed
// send releases it: the same words sent afresh get their own key.
func TestASendAgainOfAMaybeLandedMessageReusesItsFirstKey(t *testing.T) {
	fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}, windows: ocWindows(), statusReport: true, openCodeSend: true}
	s, ts := viewServer(t, fm)
	cookie := sessionCookieFor(s)
	form := url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}, "text": {"do the thing"}, "nonce": {"nonce-0123456789abcdef"}}
	fm.v2.pasteErr = fmt.Errorf("mirror: the host did not answer: %w", mirror.ErrPasteUnconfirmed)
	if code, _, again := postSendNonce(t, ts.URL, cookie, form); code < 400 || !again {
		t.Fatalf("the unconfirmed send: %d, send-again %v", code, again)
	}
	fm.v2.pasteErr = nil
	form.Set("nonce", "nonce-fedcba9876543210") // what Send again mints
	form.Set("again", "1")
	if code, body, _ := postSendNonce(t, ts.URL, cookie, form); code != http.StatusNoContent {
		t.Fatalf("Send again: %d %q", code, body)
	}
	// Confirmed: released. The same words again, afresh.
	form.Del("again")
	form.Set("nonce", "nonce-aaaabbbbccccdddd")
	if code, body, _ := postSendNonce(t, ts.URL, cookie, form); code != http.StatusNoContent {
		t.Fatalf("a fresh send of the same words: %d %q", code, body)
	}
	fm.v2.mu.Lock()
	keys := append([]string(nil), fm.v2.keys...)
	fm.v2.mu.Unlock()
	want := []string{"nonce-0123456789abcdef", "nonce-0123456789abcdef", "nonce-aaaabbbbccccdddd"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("the sends' keys %q, want %q", keys, want)
	}
}

// TestAnOpenCodeRefusalSaysItsOwnWords: the hub's and the host's refusals of
// a gated send come back to the composer in their own words (409), never as a
// plain failure.
func TestAnOpenCodeRefusalSaysItsOwnWords(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{&mirror.OpenCodeNotIdleError{Status: mirror.StatusBusy}, mirror.PasteRefusedOpenCodeBusyText},
		{&mirror.PasteRefusedError{Why: mirror.PasteRefusedOpenCodeSlashText, Refusal: mirror.PasteRefusalOpenCodeSlash}, mirror.PasteRefusedOpenCodeSlashText},
		{&mirror.PasteRefusedError{Why: mirror.PasteRefusedOpenCodeUnboundText, Refusal: mirror.PasteRefusalOpenCodeUnbound, Cause: mirror.ErrOpenCodeUnbound}, mirror.PasteRefusedOpenCodeUnboundText},
		{&mirror.PasteRefusedError{Why: mirror.PasteRefusedOpenCodeGoneText, Refusal: mirror.PasteRefusalOpenCodeGone}, mirror.PasteRefusedOpenCodeGoneText},
	} {
		fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}, windows: ocWindows(), statusReport: true, openCodeSend: true}
		s, ts := viewServer(t, fm)
		fm.v2.pasteErr = c.err
		form := url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}, "text": {"hi"}, "nonce": {"nonce-0123456789abcdef"}}
		if code, body, _ := postSendNonce(t, ts.URL, sessionCookieFor(s), form); code != http.StatusConflict || body != c.want {
			t.Fatalf("%v: %d %q, want 409 %q", c.err, code, body, c.want)
		}
	}
}

// TestTheQueueIsOpenForASendableOpenCodeWindowOnly: the send queue takes a
// message for a launcher-bound opencode window on an opted-in host — and says
// why not for the others, in the gated send's own words.
func TestTheQueueIsOpenForASendableOpenCodeWindowOnly(t *testing.T) {
	ws := ocWindows()
	entry := func(i int) mirror.SessionView {
		return mirror.SessionView{Host: "ws", Session: "main", Window: ws[i].ID, Runtime: ws[i].Runtime, AgentSession: ws[i].AgentSession, AgentSend: ws[i].AgentSend}
	}
	on := mirror.HostView{Host: "ws", Online: true, StatusReport: true, Windows: ws, Interactive: true, OpenCodeSend: true}
	off := on
	off.OpenCodeSend = false
	if why := mirrorQueueOff(on, entry(0)); why != "" {
		t.Fatalf("a sendable opencode window's queue is off: %q", why)
	}
	if why := mirrorQueueOff(on, entry(1)); why != mirror.PasteRefusedOpenCodeUnboundText {
		t.Fatalf("an unbound window's queue: %q", why)
	}
	if why := mirrorQueueOff(off, entry(0)); why != mirror.PasteRefusedOpenCodeOffText {
		t.Fatalf("a host that did not opt in: %q", why)
	}
	if why := mirrorQueueOff(on, mirror.SessionView{Host: "ws", Session: "main", Window: "@9"}); why != SendQueueNotClaude {
		t.Fatalf("a shell window's queue: %q", why)
	}
}
