package ui

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// chiefChatView is a chief's chat view on surface, on chat sid.
func chiefChatView(surface ChatSurface, bar ChiefChatBar, sessions []SessionView) AgentDetailView {
	bar.AgentID, bar.AgentName = 42, "chiefy"
	return AgentDetailView{ID: 42, Name: "chiefy", ActiveSessionID: bar.SessionID, Surface: surface, ChiefChat: &bar, Sessions: sessions}
}

func renderPane(t *testing.T, v AgentDetailView) string {
	t.Helper()
	var b bytes.Buffer
	if err := agentChatPane(v).Render(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// chiefKinds are the three chats a chief can be on, with the one action of its
// own each has (Fresh primary, Archive, Unarchive) and the two it must not.
var chiefKinds = []struct {
	bar        ChiefChatBar
	kind, act  string
	notActions []string
}{
	{ChiefChatBar{SessionID: 5, Primary: true}, ChiefChatKindPrimary, chiefFreshPrimaryAttr, []string{chiefArchiveAttr, chiefUnarchiveAttr}},
	{ChiefChatBar{SessionID: 6}, ChiefChatKindSide, chiefArchiveAttr, []string{chiefFreshPrimaryAttr, chiefUnarchiveAttr}},
	{ChiefChatBar{SessionID: 7, Archived: true}, ChiefChatKindArchived, chiefUnarchiveAttr, []string{chiefFreshPrimaryAttr, chiefArchiveAttr}},
}

// TestTheChiefChatHasNoChatBar: the chat bar that sat over a chief's transcript
// — its kind, its context chip and its three labelled buttons — is gone from
// the chat pane on BOTH surfaces (both manage chats on the strip of chips,
// drawn outside the pane; the chip is in the header / the panel's title bar).
// Nor does a chief's page draw the session drawer: nothing opens it there.
//
// Replaces TestTheChiefChatBarSaysWhichChatAndOffersItsActions, which pinned the
// bar (operator decision: one consolidated view, not three buttons).
func TestTheChiefChatHasNoChatBar(t *testing.T) {
	for _, surface := range []ChatSurface{ChatSurfacePage, ChatSurfaceChiefPanel} {
		for _, c := range chiefKinds {
			html := renderPane(t, chiefChatView(surface, c.bar, []SessionView{{ID: c.bar.SessionID, Active: true}}))
			for _, banned := range []string{"data-chief-chat-bar", chiefChatsAttr, chiefSideChatAttr, chiefFreshPrimaryAttr,
				chiefArchiveAttr, chiefUnarchiveAttr, `id="` + chiefContextID + `"`} {
				if strings.Contains(html, banned) {
					t.Errorf("%s %s: the chat pane carries %s", surface, c.kind, banned)
				}
			}
			if surface == ChatSurfacePage && strings.Contains(html, `id="`+sessionDrawerID+`"`) {
				t.Errorf("%s: the chief's page still draws the session drawer", c.kind)
			}
		}
	}
	// The control: a worker's page keeps its drawer.
	if !strings.Contains(renderPane(t, AgentDetailView{ID: 1, Name: "w"}), `id="`+sessionDrawerID+`"`) {
		t.Error("a worker's page lost its session drawer")
	}
}

// TestTheChiefPanelDrawsTheStripAnsweredIntoItsBody: the chief PANEL draws the
// page's strip of chips over its transcript — a chip switches with an hx-get
// of the panel body (never a navigation), the "+" and every ⋯ action post with
// ?surface=chief-panel into #chief-panel-body — and its thread list keeps no
// chat action but "Continue in a new session"; its title bar carries the
// context chip for the panel's chat.
func TestTheChiefPanelDrawsTheStripAnsweredIntoItsBody(t *testing.T) {
	for _, c := range chiefKinds {
		chat := chiefChatView(ChatSurfaceChiefPanel, c.bar, chiefSheetSessions(c.bar.SessionID))
		p := ChiefPanelView{Configured: true, Chat: chat}
		body := renderString(t, chiefPanelBody(p))
		strip := regexp.MustCompile(`(?s)<nav ` + chiefChatStripAttr + `="".*?</nav>`).FindString(body)
		if strip == "" {
			t.Fatalf("%s: the panel body has no chip strip", c.kind)
		}
		if !strings.Contains(strip, chiefChatKindAttr+`="`+c.kind+`"`) {
			t.Errorf("%s: the strip does not name the chat's kind", c.kind)
		}
		if strings.Contains(strip, "<a ") || strings.Contains(strip, "href=") {
			t.Errorf("%s: a panel chip navigates", c.kind)
		}
		for _, id := range []string{"5", "6", "7", "8"} {
			if !regexp.MustCompile(`<button type="button" ` + chiefChatRowAttr + `="` + id + `"[^>]*hx-get="/ui/chief/panel\?session=` + id + `" hx-target="#` + chiefPanelBodyID + `" hx-swap="innerHTML"`).MatchString(strip) {
				t.Errorf("%s: chip %s does not switch the panel body", c.kind, id)
			}
		}
		if !strings.Contains(strip, `<form hx-post="/agents/42/sessions/side?surface=chief-panel" hx-target="#`+chiefPanelBodyID+`" hx-swap="innerHTML" class="contents"><button type="submit" `+chiefSideChatAttr+`=""`) {
			t.Errorf("%s: the panel's + is not answered into the panel body:\n%s", c.kind, strip)
		}
		cur := "&amp;current=" + strconv.FormatInt(c.bar.SessionID, 10)
		for _, want := range []string{
			`hx-post="/agents/42/sessions/fresh-primary?surface=chief-panel` + cur + `" hx-target="#chief-panel-body"`,
			`hx-post="/agents/42/sessions/6/default?surface=chief-panel` + cur + `" hx-target="#chief-panel-body"`,
			`hx-post="/agents/42/sessions/6/archive?surface=chief-panel` + cur + `" hx-target="#chief-panel-body"`,
			`hx-post="/agents/42/sessions/7/unarchive?surface=chief-panel` + cur + `" hx-target="#chief-panel-body"`,
		} {
			if !strings.Contains(strip, want) {
				t.Errorf("%s: the panel strip lacks %s", c.kind, want)
			}
		}
		if strings.Contains(strip, `hx-swap="none"`) {
			t.Errorf("%s: a panel action answers with a redirect (hx-swap none)", c.kind)
		}
		if !strings.Contains(strip, `popovertarget="chief-panel-chat-menu-6"`) || strings.Contains(strip, `"chief-chat-menu-`) {
			t.Errorf("%s: the panel's menus are not its own ids", c.kind)
		}
		// The menus are placed: the shell the panel lives in has no
		// floatingMenuScript of its own.
		if !strings.Contains(strip, "__musterFloatingMenu") {
			t.Errorf("%s: the panel strip does not bring floatingMenuScript", c.kind)
		}
		// The thread list: no chat action but Continue.
		list := renderString(t, chiefThreadList(p))
		for _, banned := range []string{chiefSideChatAttr, chiefFreshPrimaryAttr, chiefArchiveAttr + `=""`, chiefUnarchiveAttr, chiefChatsAttr} {
			if strings.Contains(list, banned) {
				t.Errorf("%s: the thread list still carries %s", c.kind, banned)
			}
		}
		bar := renderString(t, chiefPanelActions(p))
		if !strings.Contains(bar, `hx-get="`+ChiefContextPath("chiefy", c.bar.SessionID)+`"`) {
			t.Errorf("%s: the panel's title bar has no context chip slot for its own chat", c.kind)
		}
		// The slot names its own target: in the panel an inherited
		// hx-target="this" (#chief-panel-body) swapped the chip's answer over
		// the whole chat (caught by the e2e suite).
		if !regexp.MustCompile(`<span id="` + chiefContextID + `"[^>]*hx-target="this"`).MatchString(bar) {
			t.Errorf("%s: the chip slot has no hx-target of its own", c.kind)
		}
	}
}

// TestTheChiefHeaderHasTheChatIndexButtonOnly: a chief's header has the one
// list icon a mirrored session's has (chatIndexButton, onto #chat-log) — and
// no ＋, no Chat history (clock), no Focus history (target) — in both headers
// (compact: a claude-code chief's folded row; not: a gateway chief's). Every
// other agent keeps its three.
//
// Replaces TestTheChiefHeaderHasNoIconOnlyNewChat.
func TestTheChiefHeaderHasTheChatIndexButtonOnly(t *testing.T) {
	render := func(v AgentDetailView, compact bool) string {
		var b bytes.Buffer
		if err := agentHeaderToolsWith(v, compact).Render(&b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	for _, compact := range []bool{false, true} {
		v := chiefChatView(ChatSurfacePage, ChiefChatBar{SessionID: 1, Primary: true}, nil)
		v.FocusHistory = "/ui/focus/history?agent=42"
		html := render(v, compact)
		if !strings.Contains(html, `id="chat-index-open"`) || !strings.Contains(html, `data-chat-index-log="chat-log"`) {
			t.Errorf("compact=%v: the chief's header has no chat index button onto #chat-log:\n%s", compact, html)
		}
		if !strings.Contains(html, `aria-label="`+chatIndexLabel+`"`) {
			t.Errorf("compact=%v: the chat index button is not the mirrored session's", compact)
		}
		for _, banned := range []string{`aria-label="New chat session"`, `aria-label="Chat history"`, `id="focus-history-open"`, `for="` + sessionDrawerID + `"`} {
			if strings.Contains(html, banned) {
				t.Errorf("compact=%v: the chief's header still carries %s", compact, banned)
			}
		}
		if n := strings.Count(html, "<button"); n != 1 {
			t.Errorf("compact=%v: %d buttons in the chief's header tools, want 1", compact, n)
		}
		if !strings.Contains(html, " w-11") {
			t.Errorf("compact=%v: the tools' box is not one control wide", compact)
		}
		w := AgentDetailView{ID: 3, Name: "w", FocusHistory: "/ui/focus/history?agent=3"}
		wh := render(w, compact)
		for _, want := range []string{`aria-label="New chat session"`, `aria-label="Chat history"`, `id="focus-history-open"`} {
			if !strings.Contains(wh, want) {
				t.Errorf("compact=%v: a worker's header lost %s", compact, want)
			}
		}
		if strings.Contains(wh, `id="chat-index-open"`) {
			t.Errorf("compact=%v: a worker's header grew the chat index button", compact)
		}
	}
}

// chiefPage renders a chief's whole page with sessions, on chat bar, of kind
// (a claude-code chief's folded header, or a gateway chief's two rows).
func chiefPage(t *testing.T, kind string, bar ChiefChatBar, sessions []SessionView) string {
	t.Helper()
	v := chiefChatView(ChatSurfacePage, bar, sessions)
	v.Kind = kind
	v.FocusHistory = "/ui/focus/history?agent=42"
	return renderString(t, AgentDetailPage(v, Features{}))
}

// chiefSheetSessions are a chief's chats: the primary, two side chats (one
// unread, one the deployment may make primary), and one archived.
func chiefSheetSessions(active int64) []SessionView {
	now := time.Now()
	s := []SessionView{
		{ID: 5, Title: "Main line", IsDefault: true, LastActive: now.Add(-time.Hour)},
		{ID: 6, Title: "Side one", Unread: true, LastActive: now.Add(-time.Minute), DefaultPath: "/agents/42/sessions/6/default"},
		{ID: 8, Title: "Side two", LastActive: now.Add(-30 * time.Minute)},
		{ID: 7, Title: "Put away", Archived: true, LastActive: now.Add(-2 * time.Hour)},
	}
	for i := range s {
		s[i].Active = s[i].ID == active
	}
	return s
}

// TestTheChiefSheetHasNoChatsTab: the chief's page draws the mirrored
// session's sheet with its two tabs IN THIS ORDER — Your messages, Focus
// history — and no Chats tab: the chats are the strip under the header, drawn
// once, outside the sheet. Not the agent page's separate focus dialog either.
//
// Replaces TestTheChiefSheetHasChatsMessagesAndFocusHistory (operator
// decision: no modal for the chats).
func TestTheChiefSheetHasNoChatsTab(t *testing.T) {
	for _, kind := range []string{"claude-code", "gateway"} {
		html := chiefPage(t, kind, ChiefChatBar{SessionID: 5, Primary: true}, chiefSheetSessions(5))
		i := strings.Index(html, `<div id="chat-index"`)
		if i < 0 {
			t.Fatalf("%s: the chief's page has no chat index sheet", kind)
		}
		sheet := html[i:]
		tabs := regexp.MustCompile(`role="tab" data-tab="([a-z]+)"`).FindAllStringSubmatch(sheet, -1)
		var got []string
		for _, m := range tabs {
			got = append(got, m[1])
		}
		if strings.Join(got, ",") != "messages,focus" {
			t.Errorf("%s: the sheet's tabs are %v, want messages, focus", kind, got)
		}
		if strings.Contains(sheet, chiefChatsAttr) || strings.Contains(sheet, ">Chats</button>") || strings.Contains(sheet, chiefSideChatAttr) {
			t.Errorf("%s: the sheet still holds the chats", kind)
		}
		// The strip: once, before the sheet, under the header.
		if n := strings.Count(html, `<nav `+chiefChatStripAttr+`=""`); n != 1 {
			t.Errorf("%s: %d chip strips on the page, want 1", kind, n)
		}
		head := strings.Index(html, "</header>")
		strip := strings.Index(html, `<nav `+chiefChatStripAttr)
		log := strings.Index(html, `id="chat-log"`)
		if !(head >= 0 && head < strip && strip < log) {
			t.Errorf("%s: the strip is not between the header and the transcript (header end %d, strip %d, log %d)", kind, head, strip, log)
		}
		if strings.Contains(html, `id="focus-history"`) || strings.Contains(html, `id="focus-history-open"`) {
			t.Errorf("%s: the chief's page still draws the separate focus history dialog or its button", kind)
		}
		if !strings.Contains(sheet, chatIndexPartialPage) || strings.Contains(sheet, "muster-mirror") {
			t.Errorf("%s: the messages tab's note speaks of a mirrored host", kind)
		}
	}
	// The control: a worker's page keeps the focus dialog and has no sheet and
	// no strip.
	w := renderString(t, AgentDetailPage(AgentDetailView{ID: 3, Name: "w", Kind: "claude-code", FocusHistory: "/ui/focus/history?agent=3"}, Features{}))
	if !strings.Contains(w, `id="focus-history"`) || strings.Contains(w, `<div id="chat-index"`) || strings.Contains(w, chiefChatStripAttr) {
		t.Error("a worker's page changed: it should keep its focus dialog and have no chat index sheet and no chip strip")
	}
}

// chipIDs is the chat ids of html's chips, in document order.
func chipIDs(html string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`<li `+chiefChipAttr+`="(\d+)"`).FindAllStringSubmatch(html, -1) {
		out = append(out, m[1])
	}
	return out
}

// TestTheStripSwitchesAndManagesChats: the page's strip, on each kind of chat
// (HX-Redirect answers, no ?surface=):
//   - a chip per chat, in the order given, each a link to its chat, the open
//     one aria-current="true" and no other; the ★ on the primary alone; the
//     unread dot on the unread side chat alone;
//   - the archived chats behind "Archived (n)" (hidden until it expands),
//     unless the archived chat is the one open;
//   - a ⋯ menu on EVERY chip: Fresh primary (with its confirm) on the
//     primary; Make primary (when offered) and Archive on a side chat;
//     Unarchive on an archived one — each naming the open chat (?current=);
//   - "+" posting to the side chat route, after the chips' scroller.
func TestTheStripSwitchesAndManagesChats(t *testing.T) {
	for _, c := range chiefKinds {
		html := renderString(t, chiefChatStrip(chiefChatView(ChatSurfacePage, c.bar, nil).ChiefChat, "chiefy", chiefSheetSessions(c.bar.SessionID), ChatSurfacePage))
		if !strings.Contains(html, chiefChatKindAttr+`="`+c.kind+`"`) {
			t.Errorf("%s: the strip does not name the open chat's kind", c.kind)
		}
		if got := strings.Join(chipIDs(html), ","); got != "5,6,8,7" {
			t.Errorf("%s: chips %s, want 5,6,8,7 (the order given, archived last)", c.kind, got)
		}
		for _, id := range []int64{5, 6, 7, 8} {
			sid := strconv.FormatInt(id, 10)
			row := regexp.MustCompile(`<a href="` + regexp.QuoteMeta(AgentSessionHref("chiefy", id)) + `" ` + chiefChatRowAttr + `="` + sid + `"[^>]*?( aria-current="true")?( class=)`).FindStringSubmatch(html)
			if row == nil {
				t.Errorf("%s: no chip linking to chat %d", c.kind, id)
				continue
			}
			if (row[1] != "") != (id == c.bar.SessionID) {
				t.Errorf("%s: chat %d current=%v, want %v", c.kind, id, row[1] != "", id == c.bar.SessionID)
			}
		}
		if n := strings.Count(html, `aria-current="true" class=`); n != 1 {
			t.Errorf("%s: %d chips current, want 1", c.kind, n)
		}
		if n := strings.Count(html, sessionDefaultAttr+`=""`); n != 1 {
			t.Errorf("%s: %d ★, want 1", c.kind, n)
		}
		if !regexp.MustCompile(`(?s)<li ` + chiefChipAttr + `="5" ` + chiefChipKindAttr + `="primary".*?★.*?Primary</span>`).MatchString(html) {
			t.Errorf("%s: the primary chip is not ★ Primary", c.kind)
		}
		// The unread dot: chat 6's chip and no other (unread is cleared on the
		// open chat by the server, not here).
		if n := strings.Count(html, chiefUnreadAttr+`=""`); n != 1 {
			t.Errorf("%s: %d unread dots, want 1", c.kind, n)
		}
		if !strings.Contains(regexp.MustCompile(`(?s)<li `+chiefChipAttr+`="6".*?</li>`).FindString(html), chiefUnreadAttr) {
			t.Errorf("%s: the unread dot is not on chat 6's chip", c.kind)
		}
		// The archived chip: hidden behind its toggle unless open.
		archived := regexp.MustCompile(`<li ` + chiefChipAttr + `="7" ` + chiefChipKindAttr + `="archived"( ` + chiefArchivedChipAttr + `="" hidden)?`).FindStringSubmatch(html)
		toggle := strings.Contains(html, `<button type="button" `+chiefArchivedAttr+`="" aria-expanded="false"`)
		if c.bar.Archived {
			if archived == nil || archived[1] != "" || toggle {
				t.Errorf("%s: the open archived chat is hidden behind Archived (toggle %v)", c.kind, toggle)
			}
		} else if archived == nil || archived[1] == "" || !toggle {
			t.Errorf("%s: the archived chat is not hidden behind a collapsed Archived (n) (chip %v, toggle %v)", c.kind, archived, toggle)
		}
		// The menus.
		cur := "?current=" + strconv.FormatInt(c.bar.SessionID, 10)
		menus := map[string][]string{
			"5": {`hx-post="/agents/42/sessions/fresh-primary` + cur + `" hx-swap="none" hx-confirm="` + FreshPrimaryConfirm + `"`},
			"6": {`hx-post="/agents/42/sessions/6/default` + cur + `" hx-swap="none"`, `hx-post="/agents/42/sessions/6/archive` + cur + `" hx-swap="none"`},
			"8": {`hx-post="/agents/42/sessions/8/archive` + cur + `" hx-swap="none"`},
			"7": {`hx-post="/agents/42/sessions/7/unarchive` + cur + `" hx-swap="none"`},
		}
		for sid, want := range menus {
			m := regexp.MustCompile(`(?s)<div id="chief-chat-menu-` + sid + `".*?</div>`).FindString(html)
			if m == "" || !strings.Contains(html, `popovertarget="chief-chat-menu-`+sid+`"`) {
				t.Errorf("%s: chat %s has no ⋯ menu", c.kind, sid)
				continue
			}
			if got := strings.Count(m, `role="menuitem"`); got != len(want) {
				t.Errorf("%s: chat %s's menu has %d items, want %d", c.kind, sid, got, len(want))
			}
			for _, w := range want {
				if !strings.Contains(m, w) {
					t.Errorf("%s: chat %s's menu lacks %s:\n%s", c.kind, sid, w, m)
				}
			}
		}
		if n := strings.Count(html, chiefFreshPrimaryAttr+`=""`); n != 1 {
			t.Errorf("%s: %d Fresh primary actions, want 1 (the primary's menu)", c.kind, n)
		}
		if strings.Contains(html, "surface=chief-panel") {
			t.Errorf("%s: a page action asks for the panel's answer", c.kind)
		}
		// The "+", after the scroller.
		plus := regexp.MustCompile(`</ul><form hx-post="/agents/42/sessions/side" hx-swap="none" class="contents"><button type="submit" ` + chiefSideChatAttr + `="" aria-label="New side chat"`)
		if !plus.MatchString(html) {
			t.Errorf("%s: no + posting to the side chat route at the strip's end", c.kind)
		}
	}
	// Not a chief's chat: no strip.
	if chiefChatStrip(nil, "w", nil, ChatSurfacePage) != nil || chiefPageStrip(AgentDetailView{ID: 1, Name: "w"}) != nil {
		t.Error("a worker's chat has a chip strip")
	}
}

// TestTheArchivedChipCountsItsChats: "Archived (n)" with the literal count
// of the archived chats behind it — 1, 2, 3 — and no such chip with none.
// The open archived chat is not behind it, so it is not counted.
func TestTheArchivedChipCountsItsChats(t *testing.T) {
	bar := &ChiefChatBar{AgentID: 42, AgentName: "chiefy", SessionID: 1, Primary: true}
	sessions := []SessionView{{ID: 1, IsDefault: true, Active: true}}
	toggle := regexp.MustCompile(`<button type="button" ` + chiefArchivedAttr + `="" aria-expanded="false"[^>]*>([^<]*)<svg`)
	for _, want := range []string{"Archived (1)", "Archived (2)", "Archived (3)"} {
		sessions = append(sessions, SessionView{ID: int64(len(sessions) + 10), Title: "old " + want, Archived: true})
		m := toggle.FindStringSubmatch(renderString(t, chiefChatStrip(bar, "chiefy", sessions, ChatSurfacePage)))
		if m == nil || m[1] != want {
			t.Errorf("the archived chip reads %v, want %q", m, want)
		}
	}
	// The open archived chat is with the others, not counted.
	open := append([]SessionView{}, sessions...)
	open[1].Active, open[0].Active = true, false
	m := toggle.FindStringSubmatch(renderString(t, chiefChatStrip(&ChiefChatBar{AgentID: 42, SessionID: open[1].ID, Archived: true}, "chiefy", open, ChatSurfacePage)))
	if m == nil || m[1] != "Archived (2)" {
		t.Errorf("with one archived chat open, the chip reads %v, want \"Archived (2)\"", m)
	}
	if html := renderString(t, chiefChatStrip(bar, "chiefy", sessions[:1], ChatSurfacePage)); strings.Contains(html, chiefArchivedAttr+`=""`) {
		t.Error("an Archived chip with no archived chat")
	}
}

// TestASharedRoomsStripOnlySwitches: a member on a team's chief
// (ChiefChatBar.SharedRoom) gets the chips — every chat, switching, the
// current one marked, Archived (n) to expand — and no "+", no ⋯ and no action
// at all (the server refuses every one: internal/api teamAgentRoutes). The
// owner's strip, the same chats, has them (the control).
func TestASharedRoomsStripOnlySwitches(t *testing.T) {
	sessions := chiefSheetSessions(5)
	shared := renderString(t, chiefChatStrip(&ChiefChatBar{AgentID: 42, SessionID: 5, Primary: true, SharedRoom: true}, "chiefy", sessions, ChatSurfacePage))
	owner := renderString(t, chiefChatStrip(&ChiefChatBar{AgentID: 42, SessionID: 5, Primary: true}, "chiefy", sessions, ChatSurfacePage))
	for name, html := range map[string]string{"shared": shared, "owner": owner} {
		if got := strings.Join(chipIDs(html), ","); got != "5,6,8,7" {
			t.Errorf("%s: chips %s, want 5,6,8,7", name, got)
		}
		if n := strings.Count(html, `aria-current="true" class=`); n != 1 {
			t.Errorf("%s: %d current chips, want 1", name, n)
		}
		if !strings.Contains(html, `>Archived (1)<`) {
			t.Errorf("%s: no Archived (1) chip", name)
		}
	}
	for _, attr := range []string{chiefSideChatAttr + `=""`, chiefFreshPrimaryAttr + `=""`, chiefMakePrimaryAttr + `=""`, chiefArchiveAttr + `=""`, chiefUnarchiveAttr + `=""`, chiefChatMenuAttr + `=""`, "popovertarget=", "hx-post="} {
		if strings.Contains(shared, attr) {
			t.Errorf("the shared room's strip carries %s", attr)
		}
		if !strings.Contains(owner, attr) {
			t.Errorf("control: the owner's strip lacks %s", attr)
		}
	}
}

// TestTheChiefContextChipIsInTheHeader: the chief chat's context chip slot is
// in the page HEADER — under the title in a claude-code chief's folded row, in
// a gateway chief's second row — loading its own chat's chip, refreshed when a
// turn of the chief starts or ends or a reply is stored, and its own target. A
// worker's header has none.
func TestTheChiefContextChipIsInTheHeader(t *testing.T) {
	for _, kind := range []string{"claude-code", "gateway"} {
		for _, c := range chiefKinds {
			v := chiefChatView(ChatSurfacePage, c.bar, nil)
			v.Kind = kind
			head := renderString(t, agentDetailHeader(v))
			if !strings.HasPrefix(head, "<header") {
				t.Fatalf("%s: not a header: %.80s", kind, head)
			}
			slot := regexp.MustCompile(`<span id="` + chiefContextID + `"[^>]*>`).FindString(head)
			if slot == "" {
				t.Errorf("%s %s: no context chip slot in the header", kind, c.kind)
				continue
			}
			for _, want := range []string{`hx-get="` + ChiefContextPath("chiefy", c.bar.SessionID) + `"`, `hx-target="this"`,
				"sse:chat.turn[", "sse:chat.reply[", "load"} {
				if !strings.Contains(slot, want) {
					t.Errorf("%s %s: the slot lacks %s: %s", kind, c.kind, want, slot)
				}
			}
			if !strings.Contains(head, `data-chief-context-row=""`) {
				t.Errorf("%s %s: no context row", kind, c.kind)
			}
		}
		w := renderString(t, agentDetailHeader(AgentDetailView{ID: 3, Name: "w", Kind: kind}))
		if strings.Contains(w, chiefContextID) {
			t.Errorf("%s: a worker's header has the chief's context chip", kind)
		}
	}
}

// TestTheChiefSwitcherListsThePrimaryFirstWithUnreadDots: both lists (the
// strip and the panel's thread list) draw the chats in the order given — the
// server's agents.SwitcherOrder — with the primary marked on the primary, an
// unread dot only on an unread chat, and the archived ones last.
func TestTheChiefSwitcherListsThePrimaryFirstWithUnreadDots(t *testing.T) {
	now := time.Now()
	sessions := []SessionView{
		{ID: 9, Title: "Main line", IsDefault: true, Active: true, LastActive: now.Add(-time.Hour)},
		{ID: 2, Title: "Newest side", Unread: true, LastActive: now.Add(-time.Minute)},
		{ID: 4, Title: "Older side", LastActive: now.Add(-30 * time.Minute)},
		{ID: 7, Title: "Put away", Archived: true, LastActive: now.Add(-2 * time.Hour)},
	}
	pos := func(html, s string) int { return strings.Index(html, s) }
	check := func(name, html, primary string, order []string) {
		last := -1
		for _, s := range order {
			p := pos(html, s)
			if p < 0 || p < last {
				t.Errorf("%s: %q out of order (at %d, after %d)", name, s, p, last)
			}
			last = p
		}
		if n := strings.Count(html, chiefUnreadAttr+`=""`); n != 1 {
			t.Errorf("%s: %d unread dots, want 1", name, n)
		}
		dot := pos(html, chiefUnreadAttr)
		if dot < pos(html, "Newest side") || dot > pos(html, "Older side") {
			t.Errorf("%s: the unread dot is not on the unread chat", name)
		}
		if p := pos(html, primary); p < 0 || p > pos(html, "Newest side") {
			t.Errorf("%s: the primary mark is not on the primary", name)
		}
	}
	strip := renderString(t, chiefChatStrip(&ChiefChatBar{AgentID: 42, SessionID: 9, Primary: true}, "chiefy", sessions, ChatSurfacePage))
	check("page strip", strip, "★", []string{"Primary", "Newest side", "Older side", "Archived (1)", "Put away"})
	var b bytes.Buffer
	if err := chiefThreadRows(ChiefThreadsView{Threads: sessions}).Render(&b); err != nil {
		t.Fatal(err)
	}
	check("panel list", b.String(), ">Primary<", []string{"Main line", "Newest side", "Older side", chiefArchivedLabel, "Put away"})
}

// TestTheComposerIsFocusedOnlyWhenTheServerAsks: #chat-input carries
// autofocus, and the pane the focus call, exactly when FocusComposer is set —
// on the page (which also drops ComposerFocusParam from its address) and in
// the panel — and neither without it.
func TestTheComposerIsFocusedOnlyWhenTheServerAsks(t *testing.T) {
	for _, surface := range []ChatSurface{ChatSurfacePage, ChatSurfaceChiefPanel} {
		for _, focus := range []bool{false, true} {
			v := chiefChatView(surface, ChiefChatBar{SessionID: 6}, []SessionView{{ID: 6, Active: true}})
			v.FocusComposer = focus
			html := renderPane(t, v)
			auto := regexp.MustCompile(`<textarea id="chat-input" autofocus`).MatchString(html)
			call := strings.Contains(html, `var i = document.getElementById('chat-input'); if (i) i.focus({ preventScroll: true });`)
			drop := strings.Contains(html, `u.searchParams.delete('`+ComposerFocusParam+`')`)
			if auto != focus || call != focus {
				t.Errorf("%s focus=%v: autofocus %v, focus call %v", surface, focus, auto, call)
			}
			if drop != (focus && surface == ChatSurfacePage) {
				t.Errorf("%s focus=%v: drops the focus parameter from the address %v", surface, focus, drop)
			}
		}
	}
}

// TestTheChiefContextChipTints: the chief's chip is #601's contextView — model
// then used/window, amber from 70%, red from 90%, untinted below and with no
// window — drawn on the server. contextView tints on the ROUNDED share (one
// number for the detail and the tint), so the edges are the tokens where the
// rounded share reaches 70 and 90: 139,000 and 179,000 of 200,000, one token
// either side.
func TestTheChiefContextChipTints(t *testing.T) {
	for _, c := range []struct {
		tokens, window int64
		level, count   string
	}{
		{138_999, 200_000, "", "139k/200k"},
		{139_000, 200_000, ContextLevelWarn, "139k/200k"},
		{178_999, 200_000, ContextLevelWarn, "179k/200k"},
		{179_000, 200_000, ContextLevelDanger, "179k/200k"},
		{179_000, 0, "", "179k"},
	} {
		v := ChiefContextView(c.tokens, c.window, "claude-sonnet-4-6")
		var b bytes.Buffer
		if err := RenderChiefContextChip(&b, v); err != nil {
			t.Fatal(err)
		}
		html := b.String()
		if !regexp.MustCompile(`data-ctx-count=""[^>]*>` + regexp.QuoteMeta(c.count) + `<`).MatchString(html) {
			t.Errorf("%d/%d: count, want %s in %s", c.tokens, c.window, c.count, html)
		}
		gotLevel := regexp.MustCompile(`data-level="([a-z]+)"`).FindStringSubmatch(html)
		if (c.level == "") != (gotLevel == nil) || (gotLevel != nil && gotLevel[1] != c.level) {
			t.Errorf("%d/%d: level %v, want %q", c.tokens, c.window, gotLevel, c.level)
		}
		if !regexp.MustCompile(`data-ctx-name=""[^>]*>claude-sonnet-4-6<`).MatchString(html) {
			t.Errorf("the chip does not name the model first: %s", html)
		}
		if !strings.Contains(html, `popovertarget="`+chiefContextPopID+`"`) || !strings.Contains(html, `data-ctx-detail=""`) {
			t.Errorf("no full detail on tap: %s", html)
		}
	}
	var b bytes.Buffer
	if err := RenderChiefContextChip(&b, nil); err != nil || b.Len() != 0 {
		t.Errorf("no readout draws %q (%v), want nothing", b.String(), err)
	}
}

// TestTheContinuePickerIsOnASideChatOnly: "Continue in a new session" is under
// the panel's list on a SIDE chat, with the New session picker's runtime and
// models, posting runtime + model to that chat's continue route with a confirm
// and its controls disabled while the request runs — and on no other chat, nor
// anywhere when no runtime is offered.
func TestTheContinuePickerIsOnASideChatOnly(t *testing.T) {
	runtimes := []SessionRuntimeView{{Runtime: "claude-code", Label: "Claude Code"}}
	models := []string{"opus", "sonnet", "haiku"}
	for _, c := range chiefKinds {
		chat := chiefChatView(ChatSurfaceChiefPanel, c.bar, []SessionView{{ID: c.bar.SessionID, Active: true}})
		html := renderString(t, chiefThreadList(ChiefPanelView{Configured: true, Chat: chat, SessionRuntimes: runtimes, ClaudeModels: models}))
		has := strings.Contains(html, chiefContinueAttr+`=""`)
		if has != (c.kind == ChiefChatKindSide) {
			t.Errorf("%s: Continue shown=%v, want only on a side chat", c.kind, has)
		}
		if !has {
			continue
		}
		form := regexp.MustCompile(`<form ` + chiefContinueFormAttr + `="claude-code" hx-post="/agents/42/sessions/6/continue\?surface=chief-panel" ` +
			`hx-target="#chief-panel-body" hx-swap="innerHTML" hx-confirm="[^"]+" hx-disabled-elt="find button, find select"[^>]*>` +
			`<input type="hidden" name="runtime" value="claude-code">`)
		if !form.MatchString(html) {
			t.Errorf("side: no continue form posting to the chat's continue route:\n%s", html)
		}
		for _, m := range models {
			if !strings.Contains(html, `<option value="`+m+`">`+m+`</option>`) {
				t.Errorf("side: the continue picker lacks model %s", m)
			}
		}
		if !strings.Contains(html, ">Continue in a new session</summary>") {
			t.Error("side: the continue group is not labelled")
		}
		// No runtime offered: no picker.
		bare := renderString(t, chiefThreadList(ChiefPanelView{Configured: true, Chat: chat, ClaudeModels: models}))
		if strings.Contains(bare, chiefContinueAttr) {
			t.Error("side: Continue offered with no runtime offered")
		}
	}
}

// TestContinueOffersExactlyTheNewSessionChoices: on a side chat, where both
// pickers are drawn, the Continue forms offer exactly the runtimes and models
// the New session forms do — runtime by runtime, model by model, in order.
func TestContinueOffersExactlyTheNewSessionChoices(t *testing.T) {
	runtimes := []SessionRuntimeView{{Runtime: "claude-code", Label: "Claude Code"}}
	models := []string{"opus", "sonnet", "haiku", "claude-opus-4-1[1m]"}
	chat := chiefChatView(ChatSurfaceChiefPanel, ChiefChatBar{SessionID: 6}, []SessionView{{ID: 6, Active: true}})
	html := renderString(t, chiefThreadList(ChiefPanelView{Configured: true, Chat: chat, SessionRuntimes: runtimes, ClaudeModels: models}))
	choices := func(attr string) []string {
		forms := regexp.MustCompile(`<form `+attr+`="[^"]*"[\s\S]*?</form>`).FindAllString(html, -1)
		var out []string
		for _, f := range forms {
			rt := regexp.MustCompile(`name="runtime" value="([^"]*)"`).FindStringSubmatch(f)
			if rt == nil {
				out = append(out, "<no runtime>")
				continue
			}
			for _, o := range regexp.MustCompile(`<option value="([^"]*)">`).FindAllStringSubmatch(f, -1) {
				out = append(out, rt[1]+" · "+o[1])
			}
		}
		return out
	}
	start, cont := choices(newSessionAttr), choices(chiefContinueFormAttr)
	if len(start) != len(models) {
		t.Fatalf("control: the New session forms offer %v, want each of %v", start, models)
	}
	if strings.Join(cont, "\n") != strings.Join(start, "\n") {
		t.Errorf("Continue offers %v, New session offers %v: they must be the same choices", cont, start)
	}
}

// TestEachChipsMenuIsNamedForItsChat: every chip's ⋯ (and its menu) is
// "Actions for <the chip's label>" — "Primary" on the primary, the chat's title
// on the others — and "Archived (n)" controls exactly the archived chips it
// shows and hides, by their ids, on both surfaces.
func TestEachChipsMenuIsNamedForItsChat(t *testing.T) {
	for _, surface := range []ChatSurface{ChatSurfacePage, ChatSurfaceChiefPanel} {
		html := renderString(t, chiefChatStrip(&ChiefChatBar{AgentID: 42, SessionID: 5, Primary: true}, "chiefy", chiefSheetSessions(5), surface))
		var names []string
		for _, m := range regexp.MustCompile(`aria-haspopup="menu" aria-expanded="false" aria-label="([^"]*)"`).FindAllStringSubmatch(html, -1) {
			names = append(names, m[1])
		}
		if got := strings.Join(names, " | "); got != "Actions for Primary | Actions for Side one | Actions for Side two | Actions for Put away" {
			t.Errorf("%s: the ⋯ names are %q", surface, got)
		}
		prefix := "chief-chip-"
		if surface == ChatSurfaceChiefPanel {
			prefix = "chief-panel-chip-"
		}
		if !strings.Contains(html, `aria-controls="`+prefix+`7"`) || !regexp.MustCompile(`<li `+chiefChipAttr+`="7" `+chiefChipKindAttr+`="archived" `+chiefArchivedChipAttr+`="" hidden(="")? id="`+prefix+`7"`).MatchString(html) {
			t.Errorf("%s: Archived (n) does not control the archived chip by its id", surface)
		}
	}
}

// TestThePanelsEscapeStepsAsideForAnOpenPopover: the panel's Escape handler
// returns while a popover (a chip's ⋯ menu) is open, so that Escape closes the
// menu and not the panel. e2e chief-ux drives the keys.
func TestThePanelsEscapeStepsAsideForAnOpenPopover(t *testing.T) {
	js := renderString(t, chiefPanelScript())
	esc := strings.Index(js, `if (e.key !== 'Escape' || e.defaultPrevented) return;`)
	guard := strings.Index(js, `try { if (document.querySelector(':popover-open')) return; } catch (err) {}`)
	shut := -1
	if guard > esc && esc >= 0 {
		shut = strings.Index(js[guard:], `setOpen(false);`)
	}
	if esc < 0 || guard < esc || shut < 0 || shut > 400 {
		t.Errorf("the panel's Escape handler has no :popover-open guard before it shuts the panel (%d, %d, %d)", esc, guard, shut)
	}
}
