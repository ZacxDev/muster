package ui

import (
	"regexp"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// THE CHIEF'S SESSIONS TAB (chiefSessionsTab, chiefCurrentChat,
// chiefIndexButton, chiefSessionsScript), which replaced the Sessions drawer:
// the first tab of the sheet the list icon opens, on the chief's page and in
// the chief panel. e2e chief-sessions-tab.spec.ts drives them in a browser.
//
// 🔴 THE PAGE-LEVEL TESTS HERE NAME ONLY WHAT main HAD TOO (AgentDetailPage,
// chiefPanelBody, MirrorViewPage) AND SPELL EVERY MARK AS A LITERAL, so each
// compiles against the code before this change and is shown red there (the
// PR's matrix), and a renamed constant cannot carry its assertion along.

// chiefSurfaces renders a chief on chat on, with sessions, on each surface:
// the page (a gateway chief's two-row header and a claude-code chief's folded
// one) and the panel body.
func chiefSurfaces(t *testing.T, on int64, sessions []SessionView) map[string]string {
	t.Helper()
	bar := ChiefChatBar{SessionID: on, Primary: on == 5, Archived: on == 7}
	out := map[string]string{}
	for _, kind := range []string{"gateway", "claude-code"} {
		v := chiefChatView(ChatSurfacePage, bar, sessions)
		v.Kind = kind
		v.FocusHistory = "/ui/focus/history?subject=agent%3A42"
		out["page/"+kind] = renderString(t, AgentDetailPage(v, Features{}))
	}
	v := chiefChatView(ChatSurfaceChiefPanel, bar, sessions)
	v.FocusHistory = "/ui/focus/history?subject=agent%3A42"
	out["panel"] = renderString(t, chiefPanelBody(ChiefPanelView{Configured: true, Chat: v}))
	return out
}

// sheetOf is surface's sheet and its id: the page's #chat-index, the panel's
// #chief-panel-index.
func chiefSheetOf(t *testing.T, surface, markup string) (*html.Node, string) {
	t.Helper()
	id := "chat-index"
	if surface == "panel" {
		id = "chief-panel-index"
	}
	s := byID(parseFragment(t, markup), id)
	if s == nil {
		t.Fatalf("%s: no sheet #%s", surface, id)
	}
	return s, id
}

// tabIDs is the data-tab of each tab under n, in order.
func tabIDs(n *html.Node) []string {
	var out []string
	for _, tab := range allAttr(n, "data-tab") {
		if attrOf(tab, "role") == "tab" {
			out = append(out, attrOf(tab, "data-tab"))
		}
	}
	return out
}

// TestTheChiefsSheetHasSessionsFirst: on the chief's page (both headers) and
// in the chief panel, the list icon's sheet has THREE tabs in this order —
// sessions, messages, focus — the first selected as drawn (a stored choice
// wins on load: tabs.go), and the Sessions tab holds every chat (5, 6, 8, 7 in
// the order given), New session, a LABELLED search box and the filter chips —
// All pressed, the others toggles not pressed — and the "No sessions match"
// line, hidden. RED at main: the sheet had two tabs, messages and focus.
func TestTheChiefsSheetHasSessionsFirst(t *testing.T) {
	for surface, markup := range chiefSurfaces(t, 5, chiefSheetSessions(5)) {
		sheet, id := chiefSheetOf(t, surface, markup)
		if got := strings.Join(tabIDs(sheet), ","); got != "sessions,messages,focus" {
			t.Errorf("%s: the sheet's tabs are %q, want sessions,messages,focus", surface, got)
		}
		first := byID(sheet, id+"-tabs-tab-sessions")
		if first == nil || attrOf(first, "aria-selected") != "true" || !strings.HasPrefix(focusNodeText(first), "Sessions") {
			t.Errorf("%s: the Sessions tab is not drawn selected: %v", surface, first)
		}
		panel := byID(sheet, id+"-tabs-panel-sessions")
		if panel == nil {
			t.Fatalf("%s: no Sessions panel", surface)
		}
		var rows []string
		for _, li := range allAttr(panel, "data-chief-chip") {
			rows = append(rows, attrOf(li, "data-chief-chip"))
		}
		if strings.Join(rows, ",") != "5,6,8,7" {
			t.Errorf("%s: the Sessions tab's rows are %v, want 5,6,8,7", surface, rows)
		}
		if findAttr(panel, "data-chief-side-chat") == nil {
			t.Errorf("%s: the Sessions tab has no New session", surface)
		}
		search := findAttr(panel, "data-chief-sessions-search")
		if search == nil || attrOf(search, "type") != "search" {
			t.Fatalf("%s: no search box", surface)
		}
		var label *html.Node
		for _, l := range allAttr(panel, "for") {
			if attrOf(l, "for") == attrOf(search, "id") && l.Data == "label" {
				label = l
			}
		}
		if attrOf(search, "id") == "" || label == nil || focusNodeText(label) != "Search sessions" {
			t.Errorf("%s: the search box has no label of its own", surface)
		}
		var chips []string
		for _, c := range allAttr(panel, "data-chief-filter") {
			chips = append(chips, attrOf(c, "data-chief-filter")+"="+focusNodeText(c)+":"+attrOf(c, "aria-pressed"))
			if c.Data != "button" || attrOf(c, "type") != "button" {
				t.Errorf("%s: chip %q is not a button", surface, focusNodeText(c))
			}
		}
		if got := strings.Join(chips, " "); got != "all=All:true unread=Unread:false archived=Archived:false" {
			t.Errorf("%s: the chips are %q", surface, got)
		}
		none := findAttr(panel, "data-chief-sessions-none")
		if none == nil || !hasAttr(none, "hidden") || focusNodeText(none) != "No sessions match" {
			t.Errorf("%s: no hidden \"No sessions match\" line: %v", surface, none)
		}
	}
}

// TestAModelChipPerModelAChatWasOpenedOn: a chip per DISTINCT OpenRouter model
// the chats were opened on (the operator's option: "a per-model chip when you
// have OpenRouter sessions"), in the order the chats first name them, after
// Archived — named by the model's own name (an OpenRouter id's last part) and
// filtering by the whole id. A chat on a CLAUDE model (sonnet, beside them in
// the fixture) gets no chip; none for a chief whose chats are all on its own
// model, and no Archived chip with no archived chat. RED at main: no chips at
// all; red before the OpenRouter-only fix: a sonnet chip.
func TestAModelChipPerModelAChatWasOpenedOn(t *testing.T) {
	sessions := chiefSheetSessions(5)
	sessions[0].Model = "openrouter/vendor-j/jasper-3b" // 5, the primary
	sessions[1].Model = "openrouter/vendor-q/quartz-9b" // 6
	sessions[2].Model = "sonnet"                        // 8: a Claude model
	sessions[3].Model = "openrouter/vendor-q/quartz-9b" // 7, the same again
	for surface, markup := range chiefSurfaces(t, 5, sessions) {
		sheet, _ := chiefSheetOf(t, surface, markup)
		var chips []string
		for _, c := range allAttr(sheet, "data-chief-filter") {
			chips = append(chips, attrOf(c, "data-chief-filter")+"="+focusNodeText(c))
		}
		want := "all=All unread=Unread archived=Archived model:openrouter/vendor-j/jasper-3b=jasper-3b model:openrouter/vendor-q/quartz-9b=quartz-9b"
		if got := strings.Join(chips, " "); got != want {
			t.Errorf("%s: chips %q, want %q", surface, got, want)
		}
	}
	plain := chiefSheetSessions(5)[:3]
	for surface, markup := range chiefSurfaces(t, 5, plain) {
		sheet, _ := chiefSheetOf(t, surface, markup)
		var chips []string
		for _, c := range allAttr(sheet, "data-chief-filter") {
			chips = append(chips, attrOf(c, "data-chief-filter"))
		}
		if got := strings.Join(chips, " "); got != "all unread" {
			t.Errorf("%s, no models and nothing archived: chips %q, want \"all unread\"", surface, got)
		}
	}
}

// TestTheMirroredViewsSheetHasNoSessionsTab: a mirrored session's sheet keeps
// its two tabs and carries none of the chief's chats. An invariant guard (the
// change did not touch the mirrored view's tabs): green at main and here.
func TestTheMirroredViewsSheetHasNoSessionsTab(t *testing.T) {
	page := renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Window: "@2"}))
	sheet := byID(parseFragment(t, page), "chat-index")
	if sheet == nil {
		t.Fatal("no index sheet")
	}
	if got := strings.Join(tabIDs(sheet), ","); got != "messages,focus" {
		t.Errorf("the mirrored view's tabs are %q, want messages,focus", got)
	}
	for _, banned := range []string{"data-chief-chats", "data-chief-chip", "data-chief-filter", "data-chief-sessions-search", "data-chief-side-chat"} {
		if strings.Contains(page, banned) {
			t.Errorf("the mirrored view carries %s", banned)
		}
	}
}

// TestNoSessionsDrawerIsLeft: the drawer is gone from the chief's page and
// panel — no Sessions button, no drawer, no ✕ or backdrop of its own, neither
// of its ids — and the bar over the transcript holds no control at all: the
// chat on screen as plain text. RED at main: the bar was the drawer's button.
func TestNoSessionsDrawerIsLeft(t *testing.T) {
	for _, on := range []int64{5, 8} {
		for surface, markup := range chiefSurfaces(t, on, chiefSheetSessions(on)) {
			for _, banned := range []string{"data-chief-sessions-open", "data-chief-sessions-drawer", "data-chief-sessions-close",
				"data-chief-sessions-backdrop", `id="chief-sessions"`, `id="chief-panel-sessions"`} {
				if strings.Contains(markup, banned) {
					t.Errorf("%s on %d: still carries %s", surface, on, banned)
				}
			}
			bars := allAttr(parseFragment(t, markup), "data-chief-chat-strip")
			if len(bars) != 1 {
				t.Fatalf("%s on %d: %d bars, want 1", surface, on, len(bars))
			}
			for _, banned := range []string{"button", "a", "input", "form"} {
				if findTag(bars[0], banned) != nil {
					t.Errorf("%s on %d: the bar holds a <%s>: it is plain text", surface, on, banned)
				}
			}
		}
	}
}

// findTag is the first element named tag under n (n included).
func findTag(n *html.Node, tag string) *html.Node {
	var out *html.Node
	walkNodes(n, func(c *html.Node) bool {
		if out == nil && c.Type == html.ElementNode && c.Data == tag {
			out = c
		}
		return out == nil
	})
	return out
}

// TestTheBarNamesTheChatOnScreen: the bar's text is the chat on screen —
// "★Primary" on the primary (the ★ hidden from a screen reader, which hears
// "Session: Primary"), else its title, "New chat" while it has none — and the
// model tag only where the chat has a model. Pairwise-distinct titles, so a bar
// naming the wrong chat reads differently. RED at main: the bar's text was in a
// button named "Sessions: …".
func TestTheBarNamesTheChatOnScreen(t *testing.T) {
	for _, c := range []struct {
		on    int64
		model string
		text  string
		star  bool
	}{
		{5, "", "Session: ★Primary", true},
		{8, "", "Session: Side two", false},
		{8, "sonnet", "Session: Side twosonnet", false},
		{7, "", "Session: Put away", false},
		{9, "", "Session: New chat", false},
	} {
		sessions := append(chiefSheetSessions(c.on), SessionView{ID: 9, Active: c.on == 9})
		for i := range sessions {
			if sessions[i].ID == c.on {
				sessions[i].Model = c.model
			}
		}
		for surface, markup := range chiefSurfaces(t, c.on, sessions) {
			bars := allAttr(parseFragment(t, markup), "data-chief-chat-strip")
			if len(bars) != 1 {
				t.Fatalf("%s on %d: %d bars", surface, c.on, len(bars))
			}
			if got := focusNodeText(bars[0]); got != c.text {
				t.Errorf("%s on %d: the bar reads %q, want %q", surface, c.on, got, c.text)
			}
			star := findAttr(bars[0], "data-session-default")
			if (star != nil) != c.star || (star != nil && attrOf(star, "aria-hidden") != "true") {
				t.Errorf("%s on %d: ★ %v, want %v (aria-hidden)", surface, c.on, star != nil, c.star)
			}
			if tag := findAttr(bars[0], "data-chief-chip-model"); (tag != nil) != (c.model != "") {
				t.Errorf("%s on %d: model tag %v, want %v", surface, c.on, tag != nil, c.model != "")
			}
		}
	}
}

// TestTheListIconCarriesTheUnreadDotAndTheTabCountsThem: the list icon (the
// page's header's #chat-index-open, the panel title bar's
// #chief-panel-index-open) opens its own sheet and carries the unread dot — and
// says so in its name — exactly when a chat OTHER than the one on screen has an
// unread reply; the Sessions tab's label counts those chats (2 is not 1, so a
// count stuck at one is caught). The one on screen unread counts for nothing.
// RED at main: the dot was on the Sessions button; the tab did not exist.
func TestTheListIconCarriesTheUnreadDotAndTheTabCountsThem(t *testing.T) {
	for _, c := range []struct {
		on     int64
		unread []int64
		count  string // "" = no count
	}{
		{5, nil, ""},
		{6, []int64{6}, ""},
		{5, []int64{6}, "1"},
		{5, []int64{6, 7}, "2"},
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
		for surface, markup := range chiefSurfaces(t, c.on, sessions) {
			doc := parseFragment(t, markup)
			sheet, id := chiefSheetOf(t, surface, markup)
			icon := byID(doc, id+"-open")
			if icon == nil || attrOf(icon, "aria-controls") != id || attrOf(icon, "aria-haspopup") != "dialog" {
				t.Fatalf("%s: no list icon opening #%s: %v", surface, id, icon)
			}
			if surface != "panel" && !insideTag(icon, "header") {
				t.Errorf("%s: the list icon is not in the header", surface)
			}
			dot := findAttr(icon, "data-chief-sessions-unread") != nil
			if dot != (c.count != "") {
				t.Errorf("%s on %d, unread %v: dot %v, want %v", surface, c.on, c.unread, dot, c.count != "")
			}
			wantName := "Sessions, your messages and what it worked on"
			if c.count != "" {
				wantName += ", unread replies in other sessions"
			}
			if got := attrOf(icon, "aria-label"); got != wantName {
				t.Errorf("%s on %d: the icon is named %q, want %q", surface, c.on, got, wantName)
			}
			tab := byID(sheet, id+"-tabs-tab-sessions")
			if tab == nil {
				t.Fatalf("%s: no Sessions tab", surface)
			}
			cnt := findAttr(tab, "data-chief-sessions-count")
			got := ""
			if cnt != nil {
				got = attrOf(cnt, "data-chief-sessions-count")
			}
			if got != c.count {
				t.Errorf("%s on %d, unread %v: the tab counts %q, want %q", surface, c.on, c.unread, got, c.count)
			}
			if c.count != "" && focusNodeText(tab) != "Sessions"+c.count+", "+c.count+" unread" {
				t.Errorf("%s: the tab reads %q", surface, focusNodeText(tab))
			}
		}
	}
}

// TestThePanelsSheetIsInThePanelsColumn: the panel's sheet is ABSOLUTE in the
// panel body's column — never fixed, which inside the transformed panel would
// be placed by the panel — and the column it lands in is the relative,
// overflow-hidden one carrying the active-session mark; the page's sheet is
// fixed over the screen. Both a fixed height. The panel's title bar holds the
// icon (out of band) and no thread-list launcher. RED at main: the panel had no
// sheet.
func TestThePanelsSheetIsInThePanelsColumn(t *testing.T) {
	ms := chiefSurfaces(t, 5, chiefSheetSessions(5))
	page, _ := chiefSheetOf(t, "page/gateway", ms["page/gateway"])
	panel, _ := chiefSheetOf(t, "panel", ms["panel"])
	if !nodeHasClass(page, "fixed") || nodeHasClass(page, "absolute") {
		t.Errorf("the page's sheet is not fixed: %s", attrOf(page, "class"))
	}
	if !nodeHasClass(panel, "absolute") || nodeHasClass(panel, "fixed") {
		t.Errorf("the panel's sheet is not absolute: %s", attrOf(panel, "class"))
	}
	col := panel.Parent
	if col == nil || !hasAttr(col, "data-chief-active-session") || !nodeHasClass(col, "relative") || !nodeHasClass(col, "overflow-hidden") {
		t.Errorf("the panel's sheet is not a child of the relative, clipping column: %v", col)
	}
	for name, s := range map[string]*html.Node{"page": page, "panel": panel} {
		box := findAttr(s, "data-chat-index-close").NextSibling
		if !nodeHasClass(box, "h-[70dvh]") {
			t.Errorf("%s: the sheet is not a fixed height", name)
		}
	}
	slot := byID(parseFragment(t, ms["panel"]), "chief-panel-actions")
	if slot == nil || attrOf(slot, "hx-swap-oob") != "true" || byID(slot, "chief-panel-index-open") == nil || findAttr(slot, "for") != nil {
		t.Errorf("the panel's title bar does not carry the list icon alone (no thread-list launcher): %v", slot)
	}
}

// TestInvariantTwoSheetsOnOneDocumentEachOpenTheirOwn — 🔴 AN INVARIANT GUARD,
// NOT REGRESSION COVERAGE: no page muster serves today holds both sheets (the
// chief page and the mirrored view are their own documents; the panel lives
// in the home shell only), so no served path has ever hit the hazard. It pins
// the opener-scoped lookup so a FUTURE document holding both works: the page's
// sheet and the panel's share NO id — not the sheet's, not a tab's, not a
// part's — each opener controls its own (aria-controls), and the script finds a
// sheet's parts INSIDE the sheet its opener names, never by a document-wide id
// (the old chatIndexScript looked up 'chat-index', 'chat-index-open' and
// 'chat-index-list' by id). e2e's "two sheets" case is the same guard in a
// browser. (It fails at main only because the panel had no sheet.)
func TestInvariantTwoSheetsOnOneDocumentEachOpenTheirOwn(t *testing.T) {
	ms := chiefSurfaces(t, 5, chiefSheetSessions(5))
	ids := func(markup, sheetID string) map[string]bool {
		s := byID(parseFragment(t, markup), sheetID)
		if s == nil {
			t.Fatalf("no sheet #%s", sheetID)
		}
		out := map[string]bool{sheetID: true}
		for _, n := range allAttr(s, "id") {
			out[attrOf(n, "id")] = true
		}
		return out
	}
	page, panel := ids(ms["page/gateway"], "chat-index"), ids(ms["panel"], "chief-panel-index")
	if len(page) < 10 || len(panel) < 10 {
		t.Fatalf("control: too few ids to compare (%d, %d)", len(page), len(panel))
	}
	for id := range page {
		if panel[id] {
			t.Errorf("both sheets carry id %q", id)
		}
	}
	js := renderString(t, chatIndexScript())
	for _, banned := range []string{"getElementById('chat-index", `getElementById("chat-index`, "#chat-index"} {
		if strings.Contains(js, banned) {
			t.Errorf("the sheet script finds a part by the page's id: %s", banned)
		}
	}
}

// TestTheSheetsEscapeIsCapturedAndStepsAsideForAPopover: the sheet's keydown
// listener is a CAPTURE listener on document (so it runs before the chief
// panel's, which would shut the whole panel), it returns while a popover (a
// row's ⋯ menu) is open, and only then takes the key — preventDefault (the
// panel's handler skips a prevented Escape) and stopPropagation — and shuts
// the sheet. RED at main: the sheet's Escape was a bubbling listener that shut
// it whatever was open. e2e drives the keys.
func TestTheSheetsEscapeIsCapturedAndStepsAsideForAPopover(t *testing.T) {
	js := renderString(t, chatIndexScript())
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
	shut := strings.Index(body, `close();`)
	if !(esc >= 0 && esc < guard && guard < pd && pd < shut && guard < sp && sp < shut) {
		t.Errorf("Escape: want the popover guard, then preventDefault and stopPropagation, then the shut (%d %d %d %d %d)", esc, guard, pd, sp, shut)
	}
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
