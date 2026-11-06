package api

import (
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// TestAHostsGuardRefusalReadsTheSameWithAndWithoutScript: when the host's
// paste guard refuses a Terminal-view send (a multiplexer, an unchecked pane,
// opencode), the person reads the guard's own words on BOTH
// paths — the composer's script (the response body:
// exactly the host's sentence, with no "mirror: the host refused the
// message:" in front of it) and a form the browser posted itself (a redirect
// to the view with a notice code whose text is the same sentence), and a
// queued message (Queue, Send now) refused so says it in its row. Each of
// the guard's kinds; and a host refusal that is NOT the guard's keeps the
// generic answer (the control: the new branch keys on the kind, not on any
// refusal) — an older host's removed "hermes" kind among them.
func TestAHostsGuardRefusalReadsTheSameWithAndWithoutScript(t *testing.T) {
	fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}}
	s, ts := viewServer(t, fm)
	cookie := sessionCookieFor(s)
	form := url.Values{"host": {"ws"}, "session": {"chief"}, "text": {"4"}}
	nav := func() string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+ui.MirrorTerminalSendPath, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Cookie", cookie)
		req.Header.Set("Origin", ts.URL)
		req.Header.Set("Sec-Fetch-Dest", "document")
		c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("a browser-submitted send answered %d, want a redirect back to the view", resp.StatusCode)
		}
		return resp.Header.Get("Location")
	}
	for _, c := range []struct{ refusal, text, notice string }{
		{mirror.PasteRefusalMultiplexer, mirror.PasteRefusedMultiplexerText, "multiplexer"},
		{mirror.PasteRefusalUnchecked, mirror.PasteRefusedUncheckedText, "unchecked"},
		{mirror.PasteRefusalOpenCode, mirror.PasteRefusedOpenCodeText, "opencode"},
	} {
		fm.v2.pasteErr = &mirror.PasteRefusedError{Why: c.text, Refusal: c.refusal}
		code, body, _ := postSend(t, ts.URL, ui.MirrorTerminalSendPath, cookie, "", form)
		if code != http.StatusConflict || body != c.text {
			t.Errorf("%s, with script: %d %q; want 409 and exactly the host's words %q", c.refusal, code, body, c.text)
		}
		loc := nav()
		if !strings.HasSuffix(loc, "notice="+c.notice) {
			t.Errorf("%s, without script: redirected to %q, want the notice %q", c.refusal, loc, c.notice)
			continue
		}
		if _, page := mirrorGet(t, s, ts, loc); !strings.Contains(page, html.EscapeString(c.text)) {
			t.Errorf("%s, without script: the page after the redirect does not say %q", c.refusal, c.text)
		}
		if out := sendQueueFailure(fm.v2.pasteErr); out.Sent || out.MayHaveDelivered || out.Pasted ||
			!strings.HasPrefix(out.Detail, "Not sent: ") || !strings.Contains(out.Detail, c.text) {
			t.Errorf("%s, a queued message: %+v; want not sent, saying %q", c.refusal, out, c.text)
		}
	}
	// The words changed with the opencode API send (it IS supported now, from
	// the Rendered view): keys are still never typed, and it says where to
	// send instead.
	if !strings.HasPrefix(mirror.PasteRefusedOpenCodeText, "Not sent from here: opencode runs in this pane, and muster never types into it") ||
		!strings.Contains(mirror.PasteRefusedOpenCodeText, "Rendered view") {
		t.Fatalf("the opencode refusal no longer says keys are never typed, and where to send: %q", mirror.PasteRefusedOpenCodeText)
	}

	// Control: another host refusal, with no kind — or with the kind an
	// older host's Hermes guard names, which muster no longer knows — is the
	// generic answer.
	for _, pr := range []*mirror.PasteRefusedError{
		{Why: "session \"chief\" is not in this host's latest list"},
		{Why: "Not sent: Hermes runs in this pane. Type it in the terminal.", Refusal: "hermes"},
	} {
		fm.v2.pasteErr = pr
		if code, body, _ := postSend(t, ts.URL, ui.MirrorTerminalSendPath, cookie, "", form); code != http.StatusBadGateway || body != "mirror: the host refused the message: "+pr.Why {
			t.Errorf("a refusal that is not the guard's (kind %q): %d %q; want the generic 502 with the host's reason after the prefix", pr.Refusal, code, body)
		}
		if loc := nav(); !strings.HasSuffix(loc, "notice=failed") {
			t.Errorf("a refusal that is not the guard's (kind %q), without script: %q, want notice=failed", pr.Refusal, loc)
		}
	}
}
