package ui

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	g "maragu.dev/gomponents"
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

// TestThePanelsSessionsTabIsAnsweredIntoItsBody: the chief PANEL's sheet has
// the page's Sessions tab — a row switches with an hx-get of the panel body
// (never a navigation), the "+" and every ⋯ action post with
// ?surface=chief-panel into #chief-panel-body — and its thread list keeps no
// chat action but "Continue in a new session"; its title bar carries no
// context chip.
func TestThePanelsSessionsTabIsAnsweredIntoItsBody(t *testing.T) {
	for _, c := range chiefKinds {
		chat := chiefChatView(ChatSurfaceChiefPanel, c.bar, chiefSheetSessions(c.bar.SessionID))
		p := ChiefPanelView{Configured: true, Chat: chat}
		body := renderString(t, chiefPanelBody(p))
		// The Sessions tab: from its panel to the next tab's.
		strip := ""
		if i := strings.Index(body, `id="chief-panel-index-tabs-panel-sessions"`); i >= 0 {
			if j := strings.Index(body[i:], `id="chief-panel-index-tabs-panel-messages"`); j >= 0 {
				strip = body[i : i+j]
			}
		}
		if strip == "" {
			t.Fatalf("%s: the panel body has no Sessions tab", c.kind)
		}
		if !strings.Contains(strip, chiefChatKindAttr+`="`+c.kind+`"`) {
			t.Errorf("%s: the tab does not name the chat's kind", c.kind)
		}
		if strings.Contains(strip, "<a ") || strings.Contains(strip, "href=") {
			t.Errorf("%s: a panel row navigates", c.kind)
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
				t.Errorf("%s: the panel's Sessions tab lacks %s", c.kind, want)
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
			t.Errorf("%s: the panel's Sessions tab does not bring floatingMenuScript", c.kind)
		}
		// One list of the chats: the panel's thread list is gone (folded into
		// the Sessions tab).
		for _, banned := range []string{`for="chief-threads"`, `id="chief-thread-search"`, "data-chief-thread=", "data-session-picker"} {
			if strings.Contains(body, banned) {
				t.Errorf("%s: the panel still draws its thread list (%s)", c.kind, banned)
			}
		}
		// No token chip in the panel's title bar (operator decision,
		// #871): the page says it in its ⋯ menu, when it is high.
		bar := renderString(t, chiefPanelActions(p))
		if strings.Contains(bar, ChiefContextPath("chiefy", c.bar.SessionID)) || strings.Contains(bar, `id="`+chiefContextID+`"`) {
			t.Errorf("%s: the panel's title bar still carries the context chip slot: %s", c.kind, bar)
		}
	}
}

// TestTheChiefHeaderHasTheChatIndexButtonOnly: a chief's header has the one
// list icon a mirrored session's has (chiefIndexButton, onto #chat-log, its
// sheet's first tab the chief's Sessions) — and no ＋, no Chat history
// (clock), no Focus history (target) — in both headers
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
		if !strings.Contains(html, `aria-label="`+chiefIndexLabel+`"`) || !strings.Contains(html, `aria-controls="chat-index"`) {
			t.Errorf("compact=%v: the chat index button does not name the chief's sheet", compact)
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
		// A worker's: New chat, Chat history and What it worked on, in either
		// header.
		for _, want := range []string{`aria-label="New chat session"`, `aria-label="Chat history"`, `id="focus-history-open"`} {
			if !strings.Contains(wh, want) {
				t.Errorf("compact=%v: a worker's header tools lack %s", compact, want)
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
		html := renderString(t, sessionsPanel(chiefChatView(ChatSurfacePage, c.bar, nil).ChiefChat, "chiefy", chiefSheetSessions(c.bar.SessionID), ChatSurfacePage))
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
		// One ★ among the rows (the button carries its own while the primary is
		// the chat on screen: TestTheSessionsButtonNamesTheChatOnScreen).
		rows := html[strings.Index(html, `<nav aria-label="Chief sessions">`):]
		if n := strings.Count(rows, sessionDefaultAttr+`=""`); n != 1 {
			t.Errorf("%s: %d ★ among the rows, want 1", c.kind, n)
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
		// "+ New session", at the drawer's top, before the rows.
		plus := regexp.MustCompile(`<form hx-post="/agents/42/sessions/side" hx-swap="none" class="contents"><button type="submit" ` + chiefSideChatAttr + `="" title="New session"[^>]*>.*?<span>New session</span></button></form><nav aria-label="Chief sessions">`)
		if !plus.MatchString(html) {
			t.Errorf("%s: no New session posting to the side chat route above the rows", c.kind)
		}
	}
	// Not a chief's chat: no strip.
	if chiefCurrentChat(nil, nil, ChatSurfacePage) != nil || chiefPageStrip(AgentDetailView{ID: 1, Name: "w"}) != nil || chiefIndexSheet(AgentDetailView{ID: 1, Name: "w"}) != nil {
		t.Error("a worker's chat has a chief's bar or sheet")
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
		m := toggle.FindStringSubmatch(renderString(t, sessionsPanel(bar, "chiefy", sessions, ChatSurfacePage)))
		if m == nil || m[1] != want {
			t.Errorf("the archived chip reads %v, want %q", m, want)
		}
	}
	// The open archived chat is with the others, not counted.
	open := append([]SessionView{}, sessions...)
	open[1].Active, open[0].Active = true, false
	m := toggle.FindStringSubmatch(renderString(t, sessionsPanel(&ChiefChatBar{AgentID: 42, SessionID: open[1].ID, Archived: true}, "chiefy", open, ChatSurfacePage)))
	if m == nil || m[1] != "Archived (2)" {
		t.Errorf("with one archived chat open, the chip reads %v, want \"Archived (2)\"", m)
	}
	if html := renderString(t, sessionsPanel(bar, "chiefy", sessions[:1], ChatSurfacePage)); strings.Contains(html, chiefArchivedAttr+`=""`) {
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
	shared := renderString(t, sessionsPanel(&ChiefChatBar{AgentID: 42, SessionID: 5, Primary: true, SharedRoom: true}, "chiefy", sessions, ChatSurfacePage))
	owner := renderString(t, sessionsPanel(&ChiefChatBar{AgentID: 42, SessionID: 5, Primary: true}, "chiefy", sessions, ChatSurfacePage))
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

// TestTheChiefHeaderHasNoTokenChipButItsMenuHasTheMemorySlot: the raw
// token-count chip is gone from the chief page's header (operator decision,
// #871) — outside the ⋯ menu the header has no slot at all — and the ⋯
// menu holds the memory line's slot instead: loading its own chat's line,
// refreshed when a turn of the chief starts or ends or a reply is stored, its
// own target. Claude-code (folded) and gateway chiefs alike. A worker's header
// has none.
func TestTheChiefHeaderHasNoTokenChipButItsMenuHasTheMemorySlot(t *testing.T) {
	for _, kind := range []string{"claude-code", "gateway"} {
		for _, c := range chiefKinds {
			v := chiefChatView(ChatSurfacePage, c.bar, nil)
			v.Kind = kind
			v.Card = AgentCardView{ID: 42, Name: "chiefy", IsChief: true}
			head := renderString(t, agentDetailHeader(v))
			if !strings.HasPrefix(head, "<header") {
				t.Fatalf("%s: not a header: %.80s", kind, head)
			}
			menu := regexp.MustCompile(`(?s)<div id="` + AgentMenuID + `".*`).FindString(head)
			if menu == "" {
				t.Fatalf("%s %s: no ⋯ menu in the header", kind, c.kind)
			}
			outside := strings.TrimSuffix(head, menu)
			for _, banned := range []string{`id="` + chiefContextID + `"`, "data-chief-context-row", "chief-context-chip", "data-ctx-count"} {
				if strings.Contains(outside, banned) {
					t.Errorf("%s %s: the header (outside its ⋯ menu) carries %s", kind, c.kind, banned)
				}
			}
			slot := regexp.MustCompile(`<div id="` + chiefContextID + `"[^>]*>`).FindString(menu)
			if slot == "" {
				t.Errorf("%s %s: no memory slot in the ⋯ menu", kind, c.kind)
				continue
			}
			for _, want := range []string{`hx-get="` + ChiefContextPath("chiefy", c.bar.SessionID) + `"`, `hx-target="this"`,
				"sse:chat.turn[", "sse:chat.reply[", "load", "empty:hidden"} {
				if !strings.Contains(slot, want) {
					t.Errorf("%s %s: the slot lacks %s: %s", kind, c.kind, want, slot)
				}
			}
		}
		w := renderString(t, agentDetailHeader(AgentDetailView{ID: 3, Name: "w", Kind: kind, Card: AgentCardView{ID: 3, Name: "w"}}))
		if strings.Contains(w, chiefContextID) {
			t.Errorf("%s: a worker's header has the chief's memory slot", kind)
		}
	}
}

// TestTheChiefSwitcherListsThePrimaryFirstWithUnreadDots: the Sessions tab, on
// both surfaces, draws the chats in the order given — the server's
// agents.SwitcherOrder — with the primary marked on the primary, an unread dot
// only on an unread chat, and the archived ones last.
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
	for _, surface := range []ChatSurface{ChatSurfacePage, ChatSurfaceChiefPanel} {
		tab := renderString(t, sessionsPanel(&ChiefChatBar{AgentID: 42, SessionID: 9, Primary: true}, "chiefy", sessions, surface))
		check(string(surface)+" tab", tab, "★", []string{"Primary", "Newest side", "Older side", "Archived (1)", "Put away"})
	}
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

// TestTheMemoryLineShowsOnlyAboveTheThreshold: the ⋯ menu's line says
// "Memory used: <n>k" in plain words only past ChiefMemoryShowAbove (350k):
// at 350,612 tokens it is there ("351k": rounded, not cut), at 349,876 it is
// not. Neither fixture equals the threshold or a round thousand, so a mutant
// moving the threshold, dropping the check or cutting instead of rounding
// moves one of them.
func TestTheMemoryLineShowsOnlyAboveTheThreshold(t *testing.T) {
	for _, c := range []struct {
		tokens int64
		want   string // "" = nothing drawn
	}{
		{350_612, "Memory used: 351k"},
		{362_480, "Memory used: 362k"},
		{349_876, ""},
		{45_000, ""},
	} {
		var b bytes.Buffer
		if err := RenderChiefMemoryLine(&b, c.tokens); err != nil {
			t.Fatal(err)
		}
		html := b.String()
		if c.want == "" {
			if html != "" {
				t.Errorf("%d tokens: drew %q, want nothing", c.tokens, html)
			}
			continue
		}
		if !strings.Contains(html, ChiefMemoryAttr+`="`) || !strings.Contains(html, ">"+c.want+"<") {
			t.Errorf("%d tokens: drew %q, want the line %q", c.tokens, html, c.want)
		}
		if strings.Contains(html, "token") || strings.Contains(html, "context") {
			t.Errorf("%d tokens: the line is not in plain words: %q", c.tokens, html)
		}
	}
}

// TestTheContinuePickerIsOnASideChatOnly: "Continue in a new session" is in the
// Sessions tab, on BOTH surfaces, when the chat on screen is a SIDE chat — one
// form per runtime the bar offers (ChiefChatBar.Runtimes), the Claude Code one
// with the "+" menu's models (NewChatModels), each posting runtime + model to
// that chat's continue route the way its surface answers (HX-Redirect on the
// page; the panel body in the panel), with a confirm and its controls disabled
// while the request runs — and on no other chat, nor anywhere when no runtime
// is offered, nor in a team's shared room. RED before the fold: Continue was
// under the panel's thread list, never in the tab, never on the page.
func TestTheContinuePickerIsOnASideChatOnly(t *testing.T) {
	runtimes := []SessionRuntimeView{{Runtime: "claude-code", Label: "Claude Code"}, {Runtime: "opencode", Label: "OpenRouter"}}
	models := []string{"opus", "sonnet", "haiku"}
	for _, surface := range []ChatSurface{ChatSurfacePage, ChatSurfaceChiefPanel} {
		post := `hx-post="/agents/42/sessions/6/continue" hx-swap="none"`
		if surface == ChatSurfaceChiefPanel {
			post = `hx-post="/agents/42/sessions/6/continue\?surface=chief-panel" hx-target="#chief-panel-body" hx-swap="innerHTML"`
		}
		for _, c := range chiefKinds {
			bar := c.bar
			bar.AgentID, bar.Runtimes, bar.NewChatModels = 42, runtimes, models
			html := renderString(t, sessionsPanel(&bar, "chiefy", chiefSheetSessions(bar.SessionID), surface))
			has := strings.Contains(html, chiefContinueAttr+`=""`)
			if has != (c.kind == ChiefChatKindSide) {
				t.Errorf("%s %s: Continue shown=%v, want only on a side chat", surface, c.kind, has)
			}
			if !has {
				continue
			}
			for _, rt := range []string{"claude-code", "opencode"} {
				form := regexp.MustCompile(`<form ` + chiefContinueFormAttr + `="` + rt + `" ` + post + ` hx-confirm="[^"]+" hx-disabled-elt="find button, find select"[^>]*>` +
					`<input type="hidden" name="runtime" value="` + rt + `">`)
				if !form.MatchString(html) {
					t.Errorf("%s side: no %s continue form posting to the chat's continue route:\n%s", surface, rt, html)
				}
			}
			for _, m := range models {
				if !strings.Contains(html, `<option value="`+m+`">`+m+`</option>`) {
					t.Errorf("%s side: the continue picker lacks model %s", surface, m)
				}
			}
			if !strings.Contains(html, ">Continue in a new session</summary>") {
				t.Errorf("%s side: the continue group is not labelled", surface)
			}
			bare := bar
			bare.Runtimes = nil
			if strings.Contains(renderString(t, sessionsPanel(&bare, "chiefy", chiefSheetSessions(6), surface)), chiefContinueAttr) {
				t.Errorf("%s side: Continue offered with no runtime offered", surface)
			}
			shared := bar
			shared.SharedRoom = true
			if strings.Contains(renderString(t, sessionsPanel(&shared, "chiefy", chiefSheetSessions(6), surface)), chiefContinueAttr) {
				t.Errorf("%s side: Continue offered in a shared room", surface)
			}
		}
	}
}

// TestContinueOffersExactlyTheNewSessionChoices: on a side chat the Continue
// forms offer exactly what the "+ New session" menu does — the Claude Code
// models in order (NewChatModels), and OpenRouter when the menu offers it.
func TestContinueOffersExactlyTheNewSessionChoices(t *testing.T) {
	models := []string{"opus", "sonnet", "haiku", "claude-opus-4-1[1m]"}
	bar := &ChiefChatBar{AgentID: 42, SessionID: 6, NewChatModels: models, OpenRouter: true,
		Runtimes: []SessionRuntimeView{{Runtime: "claude-code", Label: "Claude Code"}, {Runtime: "opencode", Label: "OpenRouter"}}}
	html := renderString(t, sessionsPanel(bar, "chiefy", chiefSheetSessions(6), ChatSurfacePage))
	var cont []string
	for _, f := range regexp.MustCompile(`<form `+chiefContinueFormAttr+`="[^"]*"[\s\S]*?</form>`).FindAllString(html, -1) {
		rt := regexp.MustCompile(`name="runtime" value="([^"]*)"`).FindStringSubmatch(f)[1]
		if rt == "opencode" {
			cont = append(cont, "opencode · search")
			continue
		}
		for _, o := range regexp.MustCompile(`<option value="([^"]*)">`).FindAllStringSubmatch(f, -1) {
			cont = append(cont, rt+" · "+o[1])
		}
	}
	var plus []string
	for _, m := range newChatEntryRE.FindAllStringSubmatch(html, -1) {
		if m[4] != "" {
			plus = append(plus, "claude-code · "+m[4])
		}
	}
	if strings.Contains(html, chiefNewChatOpenRouterAttr) {
		plus = append(plus, "opencode · search")
	}
	if len(plus) != len(models)+1 {
		t.Fatalf("control: the + menu offers %v", plus)
	}
	if strings.Join(cont, "\n") != strings.Join(plus, "\n") {
		t.Errorf("Continue offers %v, + New session offers %v: they must be the same choices", cont, plus)
	}
}

// TestEachChipsMenuIsNamedForItsChat: every chip's ⋯ (and its menu) is
// "Actions for <the chip's label>" — "Primary" on the primary, the chat's title
// on the others — and "Archived (n)" controls exactly the archived chips it
// shows and hides, by their ids, on both surfaces.
func TestEachChipsMenuIsNamedForItsChat(t *testing.T) {
	for _, surface := range []ChatSurface{ChatSurfacePage, ChatSurfaceChiefPanel} {
		html := renderString(t, sessionsPanel(&ChiefChatBar{AgentID: 42, SessionID: 5, Primary: true}, "chiefy", chiefSheetSessions(5), surface))
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

// sessionsPanel is the Sessions tab's panel for b on surface (in that
// surface's sheet).
func sessionsPanel(b *ChiefChatBar, name string, sessions []SessionView, surface ChatSurface) g.Node {
	return chiefSessionsTab(b, name, sessions, surface, chiefIndexID(surface)).Panel
}

// TestTheSessionsScriptReadsTextNotMarkup: the Sessions tab's filter reads a
// row's words by textContent and an attribute's value — never innerHTML or
// innerText's markup-sensitive cousins — and a ⋯ action's answer reopens the
// sheet the action was in (its closest sheet), on the Sessions tab. A spelled
// guard on the script's source, paired with e2e's behaviour; a title carrying
// markup is escaped where it is drawn (TestFocusTextIsEscapedEverywhereItIsDrawn's
// rule, textplain).
func TestTheSessionsScriptReadsTextNotMarkup(t *testing.T) {
	js := renderString(t, chiefSessionsScript())
	for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "eval("} {
		if strings.Contains(js, banned) {
			t.Errorf("the Sessions script uses %s", banned)
		}
	}
	for _, want := range []string{"a.textContent", "el.closest('[' + SHEET + ']')", `[data-tab="' + SESSIONS_TAB + '"]`, "window.muIndex.open(d)"} {
		if !strings.Contains(js, want) {
			t.Errorf("the Sessions script lacks %s", want)
		}
	}
	// A title with markup in it is text in the row and in the bar.
	sessions := chiefSheetSessions(8)
	sessions[2].Title = `<svg onload="alert(1)">x`
	for surface, markup := range chiefSurfaces(t, 8, sessions) {
		if strings.Contains(markup, `<svg onload`) {
			t.Errorf("%s: a title's markup is drawn as markup", surface)
		}
	}
}
