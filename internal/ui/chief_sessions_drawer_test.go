package ui

import (
	"regexp"
	"strings"
	"testing"
)

// THE SESSIONS BUTTON AND ITS DRAWER (chiefChatStrip, chiefSessionsButton,
// chiefChipsScript), which replaced the strip of chips. e2e
// chief-sessions-drawer.spec.ts drives them in a browser.

// sessionsButton is the bar's button in html, its opening tag to its close.
func sessionsButton(t *testing.T, html string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<button type="button" ` + chiefSessionsOpenAttr + `=""[^>]*>.*?</button>`).FindString(html)
	if m == "" {
		t.Fatalf("no Sessions button:\n%s", html)
	}
	return m
}

// TestTheSessionsButtonNamesTheChatOnScreen: the bar is ONE button — named
// "Sessions: <the chat on screen>", ★ only when that is the primary, its model
// tag only when it has one — controlling a drawer of that id that is a modal
// dialog titled "Sessions", shut (inert) as drawn, with its ✕. On both
// surfaces, each with its own drawer id.
func TestTheSessionsButtonNamesTheChatOnScreen(t *testing.T) {
	for _, surface := range []ChatSurface{ChatSurfacePage, ChatSurfaceChiefPanel} {
		drawer := "chief-sessions"
		if surface == ChatSurfaceChiefPanel {
			drawer = "chief-panel-sessions"
		}
		for _, c := range []struct {
			on         int64
			model      string
			name       string
			star, tagd bool
		}{
			{5, "", "Sessions: Primary", true, false},
			{8, "", "Sessions: Side two", false, false},
			{8, "sonnet", "Sessions: Side two", false, true},
			{7, "", "Sessions: Put away", false, false},
		} {
			sessions := chiefSheetSessions(c.on)
			for i := range sessions {
				if sessions[i].ID == c.on {
					sessions[i].Model = c.model
				}
			}
			html := renderString(t, chiefChatStrip(&ChiefChatBar{AgentID: 42, SessionID: c.on, Primary: c.on == 5, Archived: c.on == 7}, "chiefy", sessions, surface))
			if n := strings.Count(html, chiefSessionsOpenAttr+`=""`); n != 1 {
				t.Errorf("%s on %d: %d Sessions buttons, want 1", surface, c.on, n)
			}
			b := sessionsButton(t, html)
			for _, want := range []string{`aria-haspopup="dialog"`, `aria-expanded="false"`, `aria-controls="` + drawer + `"`, `aria-label="` + c.name + `"`} {
				if !strings.Contains(b, want) {
					t.Errorf("%s on %d: the button lacks %s:\n%s", surface, c.on, want, b)
				}
			}
			if got := strings.Contains(b, sessionDefaultAttr); got != c.star {
				t.Errorf("%s on %d: ★ on the button %v, want %v", surface, c.on, got, c.star)
			}
			if got := strings.Contains(b, chiefChipModelAttr+`="sonnet"`); got != c.tagd {
				t.Errorf("%s on %d: model tag on the button %v, want %v", surface, c.on, got, c.tagd)
			}
			d := regexp.MustCompile(`<div id="` + drawer + `" ` + chiefSessionsDrawerAttr + `="" role="dialog" aria-modal="true" aria-labelledby="` + drawer + `-title" inert="" data-open="0" class="[^"]*translate-x-full`).FindString(html)
			if d == "" {
				t.Errorf("%s on %d: no shut modal drawer %q", surface, c.on, drawer)
			}
			if !strings.Contains(html, `<h2 id="`+drawer+`-title"`) || !strings.Contains(html, `>Sessions</h2>`) {
				t.Errorf("%s on %d: the drawer is not titled Sessions", surface, c.on)
			}
			if !strings.Contains(html, chiefSessionsCloseAttr+`="" aria-label="Close sessions"`) {
				t.Errorf("%s on %d: the drawer has no ✕", surface, c.on)
			}
			if !strings.Contains(html, chiefSessionsBackdropAttr+`="`+drawer+`" hidden=""`) {
				t.Errorf("%s on %d: no hidden backdrop for the drawer", surface, c.on)
			}
		}
	}
}

// TestTheSessionsButtonDotsAnUnreadReplyElsewhere: the button carries a dot
// when a chat OTHER than the one on screen has an unread reply — the rows are
// in the shut drawer, so it is what says so — and none when the only unread
// chat is the one on screen, or none is.
func TestTheSessionsButtonDotsAnUnreadReplyElsewhere(t *testing.T) {
	for _, c := range []struct {
		on     int64
		unread []int64
		dot    bool
	}{
		{5, []int64{6}, true},
		{5, []int64{7}, true}, // an archived chat's reply too
		{6, []int64{6}, false},
		{5, nil, false},
	} {
		sessions := chiefSheetSessions(c.on)
		for i := range sessions {
			sessions[i].Unread = false
			for _, u := range c.unread {
				if sessions[i].ID == u {
					sessions[i].Unread = true
				}
			}
		}
		b := sessionsButton(t, renderString(t, chiefChatStrip(&ChiefChatBar{AgentID: 42, SessionID: c.on, Primary: c.on == 5}, "chiefy", sessions, ChatSurfacePage)))
		if got := strings.Contains(b, chiefSessionsUnreadAttr); got != c.dot {
			t.Errorf("on %d, unread %v: dot %v, want %v", c.on, c.unread, got, c.dot)
		}
	}
}

// TestTheDrawerSlidesFromItsSurfacesRightEdge: on the page the drawer is fixed
// to the screen's right edge, full height; in the panel it is ABSOLUTE in the
// panel body's column — never fixed, which inside the transformed panel would
// be placed by the panel (chiefChatStrip's header) — and the column it lands
// in is the relative, overflow-hidden one that clips it while it is slid away.
func TestTheDrawerSlidesFromItsSurfacesRightEdge(t *testing.T) {
	cls := func(html, id string) string {
		m := regexp.MustCompile(`<div id="` + id + `" [^>]*class="([^"]*)"`).FindStringSubmatch(html)
		if m == nil {
			t.Fatalf("no drawer %s", id)
		}
		return " " + m[1] + " "
	}
	page := cls(renderString(t, chiefChatStrip(&ChiefChatBar{AgentID: 42, SessionID: 5, Primary: true}, "chiefy", chiefSheetSessions(5), ChatSurfacePage)), "chief-sessions")
	for _, want := range []string{" fixed ", " inset-y-0 ", " right-0 "} {
		if !strings.Contains(page, want) {
			t.Errorf("the page's drawer lacks %q: %s", want, page)
		}
	}
	chat := chiefChatView(ChatSurfaceChiefPanel, ChiefChatBar{SessionID: 5, Primary: true}, chiefSheetSessions(5))
	body := renderString(t, chiefPanelBody(ChiefPanelView{Configured: true, Chat: chat}))
	panel := cls(body, "chief-panel-sessions")
	for _, want := range []string{" absolute ", " inset-y-0 ", " right-0 "} {
		if !strings.Contains(panel, want) {
			t.Errorf("the panel's drawer lacks %q: %s", want, panel)
		}
	}
	if strings.Contains(panel, " fixed ") {
		t.Errorf("the panel's drawer is fixed: %s", panel)
	}
	// Its containing block: the column carrying the active-session mark.
	col := regexp.MustCompile(`<div class="([^"]*)" `+chiefActiveSessionAttr+`=`).FindStringSubmatch(body)
	if col == nil || !strings.Contains(" "+col[1]+" ", " relative ") || !strings.Contains(" "+col[1]+" ", " overflow-hidden ") {
		t.Errorf("the panel's drawer is not inside the relative, clipping column: %v", col)
	}
	if strings.Index(body, `id="chief-panel-sessions"`) < strings.Index(body, chiefActiveSessionAttr+`=`) {
		t.Error("the panel's drawer is outside the column")
	}
}

// TestTheDrawersEscapeIsCapturedAndStepsAsideForAPopover: the drawer's
// keydown listener is a CAPTURE listener on document (so it runs before the
// chief panel's, which would shut the whole panel), it returns while a popover
// (a row's ⋯ menu) is open, and only then takes the key — preventDefault (the
// panel's handler skips a prevented Escape) and stopPropagation — and shuts the
// drawer with the focus back on its button. e2e drives the keys.
func TestTheDrawersEscapeIsCapturedAndStepsAsideForAPopover(t *testing.T) {
	js := renderString(t, chiefChipsScript())
	start := strings.Index(js, `document.addEventListener('keydown', function (e) {`)
	if start < 0 {
		t.Fatal("no keydown listener")
	}
	body := js[start:]
	end := strings.Index(body, "}, true);")
	if end < 0 {
		t.Fatal("the keydown listener is not a capture listener")
	}
	body = body[:end]
	esc := strings.Index(body, `if (e.key === 'Escape') {`)
	guard := strings.Index(body, `try { if (document.querySelector(':popover-open')) return; } catch (err) {}`)
	pd := strings.Index(body, `e.preventDefault();`)
	sp := strings.Index(body, `e.stopPropagation();`)
	shut := strings.Index(body, `setOpen(d, false, true);`)
	if !(esc >= 0 && esc < guard && guard < pd && pd < shut && guard < sp && sp < shut) {
		t.Errorf("Escape: want the popover guard, then preventDefault and stopPropagation, then the shut (%d %d %d %d %d)", esc, guard, pd, sp, shut)
	}
	// And the panel skips an Escape someone took.
	if !strings.Contains(renderString(t, chiefPanelScript()), `if (e.key !== 'Escape' || e.defaultPrevented) return;`) {
		t.Error("the panel's Escape does not skip a prevented one")
	}
}

// TestOnlyAChiefsChatKeepsItsScroll: a chief's #chat-log carries the scroll
// memory's mark (agentChatScript reads it), on the page and in the panel; a
// worker's does not — its chat opens at its end, as before.
func TestOnlyAChiefsChatKeepsItsScroll(t *testing.T) {
	logTag := regexp.MustCompile(`<div id="chat-log"[^>]*>`)
	for _, surface := range []ChatSurface{ChatSurfacePage, ChatSurfaceChiefPanel} {
		tag := logTag.FindString(renderPane(t, chiefChatView(surface, ChiefChatBar{SessionID: 5, Primary: true}, chiefSheetSessions(5))))
		if !strings.Contains(tag, ChatScrollMemoryAttr+`=""`) {
			t.Errorf("%s: a chief's chat log has no scroll memory mark: %s", surface, tag)
		}
	}
	if tag := logTag.FindString(renderPane(t, AgentDetailView{ID: 1, Name: "w"})); tag == "" || strings.Contains(tag, ChatScrollMemoryAttr) {
		t.Errorf("a worker's chat log carries the scroll memory mark (or has no log): %s", tag)
	}
}
