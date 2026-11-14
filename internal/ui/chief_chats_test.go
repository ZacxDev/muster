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
// the chat pane on BOTH surfaces (the page manages chats in the header sheet's
// Chats tab, the panel under its thread list; the chip is in the header / the
// panel's title bar). Nor does a chief's page draw the session drawer: nothing
// opens it there.
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

// TestTheChiefPanelOffersItsChatActionsUnderItsList: the chief PANEL keeps
// every action the bar had — "New side chat" with visible text on every chat,
// and the open chat's own one — under its thread list, each answered into the
// panel body; and its title bar carries the context chip for the panel's chat.
func TestTheChiefPanelOffersItsChatActionsUnderItsList(t *testing.T) {
	for _, c := range chiefKinds {
		chat := chiefChatView(ChatSurfaceChiefPanel, c.bar, []SessionView{{ID: c.bar.SessionID, Active: true}})
		p := ChiefPanelView{Configured: true, Chat: chat}
		html := renderString(t, chiefThreadList(p))
		if !strings.Contains(html, chiefChatKindAttr+`="`+c.kind+`"`) {
			t.Errorf("%s: the panel's actions do not name the chat's kind", c.kind)
		}
		side := regexp.MustCompile(`<button type="submit" ` + chiefSideChatAttr + `="" class="[^"]*"><span aria-hidden="true">\+</span>New side chat</button>`)
		if !side.MatchString(html) {
			t.Errorf("%s: no New side chat button with visible text", c.kind)
		}
		if !strings.Contains(html, `hx-post="/agents/42/sessions/side?surface=chief-panel"`) || !strings.Contains(html, `hx-target="#`+chiefPanelBodyID+`"`) {
			t.Errorf("%s: New side chat is not answered into the panel body", c.kind)
		}
		if !strings.Contains(html, c.act+`=""`) {
			t.Errorf("%s: no %s action", c.kind, c.act)
		}
		for _, n := range c.notActions {
			if strings.Contains(html, n+`=""`) {
				t.Errorf("%s: carries %s", c.kind, n)
			}
		}
		if (c.kind == ChiefChatKindPrimary) != strings.Contains(html, `hx-confirm="`) {
			t.Errorf("%s: a confirm on the wrong chat (only Fresh primary asks)", c.kind)
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
		if !strings.Contains(html, `aria-label="`+chatIndexChatsLabel+`"`) {
			t.Errorf("compact=%v: the chat index button does not name its Chats tab", compact)
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

// TestTheChiefSheetHasChatsMessagesAndFocusHistory: the chief's page draws the
// mirrored session's sheet with three tabs IN THIS ORDER — Chats, Your
// messages, Focus history — and not the agent page's separate focus dialog.
func TestTheChiefSheetHasChatsMessagesAndFocusHistory(t *testing.T) {
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
		if strings.Join(got, ",") != "chats,messages,focus" {
			t.Errorf("%s: the sheet's tabs are %v, want chats, messages, focus", kind, got)
		}
		for _, label := range []string{">Chats</button>", ">Your messages</button>", ">Focus history</button>"} {
			if !strings.Contains(sheet, label) {
				t.Errorf("%s: no %s tab", kind, label)
			}
		}
		if !strings.Contains(sheet, `id="chat-index-tabs-panel-chats"`) || !strings.Contains(sheet, chiefChatsAttr) {
			t.Errorf("%s: the Chats tab has no chats", kind)
		}
		if strings.Contains(html, `id="focus-history"`) || strings.Contains(html, `id="focus-history-open"`) {
			t.Errorf("%s: the chief's page still draws the separate focus history dialog or its button", kind)
		}
		if !strings.Contains(sheet, chatIndexPartialPage) || strings.Contains(sheet, "muster-mirror") {
			t.Errorf("%s: the messages tab's note speaks of a mirrored host", kind)
		}
	}
	// The control: a worker's page keeps the focus dialog and has no sheet.
	w := renderString(t, AgentDetailPage(AgentDetailView{ID: 3, Name: "w", Kind: "claude-code", FocusHistory: "/ui/focus/history?agent=3"}, Features{}))
	if !strings.Contains(w, `id="focus-history"`) || strings.Contains(w, `<div id="chat-index"`) {
		t.Error("a worker's page changed: it should keep its focus dialog and have no chat index sheet")
	}
}

// TestTheChatsTabListsSwitchesAndManagesChats: the Chats tab's rows and
// actions, on the page surface (HX-Redirect answers, no ?surface=):
//   - every chat, a link to it, the open one marked current (aria-current and
//     "Current"), with the Primary badge and the unread dot;
//   - the archived chats in a collapsed <details>;
//   - a ⋯ menu on every row but the primary's: Make primary (when offered) and
//     Archive on a side chat, Unarchive on an archived one, each posting to its
//     route;
//   - "+ New side chat" on every chat; "Fresh primary", with its confirm, ONLY
//     on the primary.
func TestTheChatsTabListsSwitchesAndManagesChats(t *testing.T) {
	for _, c := range chiefKinds {
		html := renderString(t, chiefChatsPanel(chiefChatView(ChatSurfacePage, c.bar, chiefSheetSessions(c.bar.SessionID))))
		if !strings.Contains(html, chiefChatKindAttr+`="`+c.kind+`"`) {
			t.Errorf("%s: the Chats tab does not name the open chat's kind", c.kind)
		}
		for _, id := range []int64{5, 6, 7, 8} {
			sid := strconv.FormatInt(id, 10)
			row := regexp.MustCompile(`<a href="` + regexp.QuoteMeta(AgentSessionHref("chiefy", id)) + `" ` + chiefChatRowAttr + `="` + sid + `"( aria-current="page")?`).FindStringSubmatch(html)
			if row == nil {
				t.Errorf("%s: no row linking to chat %d", c.kind, id)
				continue
			}
			if (row[1] != "") != (id == c.bar.SessionID) {
				t.Errorf("%s: chat %d current=%v, want %v", c.kind, id, row[1] != "", id == c.bar.SessionID)
			}
		}
		if n := strings.Count(html, chiefChatCurrentAttr+`=""`); n != 1 {
			t.Errorf("%s: %d chats marked Current, want 1", c.kind, n)
		}
		if n := strings.Count(html, sessionDefaultAttr+`=""`); n != 1 {
			t.Errorf("%s: %d Primary badges, want 1", c.kind, n)
		}
		if n := strings.Count(html, chiefUnreadAttr+`=""`); n != 1 {
			t.Errorf("%s: %d unread dots, want 1", c.kind, n)
		}
		// The archived group: collapsed, and holding the archived chat unless
		// it is the one open.
		d := strings.Index(html, `<details `+chiefArchivedAttr)
		put := strings.Index(html, `>Put away<`)
		if c.bar.Archived {
			if d >= 0 {
				t.Errorf("%s: the open archived chat is hidden in the collapsed group", c.kind)
			}
		} else if d < 0 || put < d || strings.Contains(html[d:d+80], " open") {
			t.Errorf("%s: the archived chat is not in a collapsed Archived group (details %d, row %d)", c.kind, d, put)
		}
		// The row menus: none on the primary; Make primary and Archive on side
		// chat 6, Archive alone on side chat 8 (no default path), Unarchive on 7.
		menus := map[string][]string{
			"5": nil,
			"6": {`hx-post="/agents/42/sessions/6/default" hx-swap="none"`, `hx-post="/agents/42/sessions/6/archive" hx-swap="none"`},
			"8": {`hx-post="/agents/42/sessions/8/archive" hx-swap="none"`},
			"7": {`hx-post="/agents/42/sessions/7/unarchive" hx-swap="none"`},
		}
		for sid, want := range menus {
			m := regexp.MustCompile(`(?s)<div id="chief-chat-menu-` + sid + `".*?</div>`).FindString(html)
			if want == nil {
				if m != "" || strings.Contains(html, `popovertarget="chief-chat-menu-`+sid+`"`) {
					t.Errorf("%s: the primary has a ⋯ menu", c.kind)
				}
				continue
			}
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
		if strings.Contains(html, "surface=chief-panel") {
			t.Errorf("%s: a page action asks for the panel's answer", c.kind)
		}
		// The footer.
		side := regexp.MustCompile(`<form hx-post="/agents/42/sessions/side" hx-swap="none" class="contents"><button type="submit" ` + chiefSideChatAttr + `="" class="[^"]*"><span aria-hidden="true">\+</span>New side chat</button>`)
		if !side.MatchString(html) {
			t.Errorf("%s: no + New side chat posting to the side chat route", c.kind)
		}
		fresh := regexp.MustCompile(`<form hx-post="/agents/42/sessions/fresh-primary" hx-swap="none" hx-confirm="[^"]+" class="contents"><button type="submit" ` + chiefFreshPrimaryAttr + `=""`)
		if (c.kind == ChiefChatKindPrimary) != fresh.MatchString(html) {
			t.Errorf("%s: Fresh primary (with its confirm) shown=%v, want only on the primary", c.kind, fresh.MatchString(html))
		}
		if c.kind != ChiefChatKindPrimary && strings.Contains(html, chiefFreshPrimaryAttr) {
			t.Errorf("%s: Fresh primary offered off the primary", c.kind)
		}
	}
	// Not a chief's chat: no Chats tab content at all.
	if chiefChatsPanel(AgentDetailView{ID: 1, Name: "w"}) != nil {
		t.Error("a worker's chat has a Chats tab")
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
// page's Chats tab and the panel's thread list) draw the rows in the order given —
// the server's agents.SwitcherOrder — with the Primary badge on the primary,
// an unread dot only on an unread chat, and the archived ones under Archived.
func TestTheChiefSwitcherListsThePrimaryFirstWithUnreadDots(t *testing.T) {
	now := time.Now()
	sessions := []SessionView{
		{ID: 9, Title: "Main line", IsDefault: true, LastActive: now.Add(-time.Hour)},
		{ID: 2, Title: "Newest side", Unread: true, LastActive: now.Add(-time.Minute)},
		{ID: 4, Title: "Older side", LastActive: now.Add(-30 * time.Minute)},
		{ID: 7, Title: "Put away", Archived: true, LastActive: now.Add(-2 * time.Hour)},
	}
	pos := func(html, s string) int { return strings.Index(html, s) }
	check := func(name, html string) {
		order := []string{"Main line", "Newest side", "Older side", chiefArchivedLabel, "Put away"}
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
		if p := pos(html, `>Primary<`); p < 0 || p > pos(html, "Newest side") {
			t.Errorf("%s: the Primary badge is not on the primary row", name)
		}
	}
	v := chiefChatView(ChatSurfacePage, ChiefChatBar{SessionID: 9, Primary: true}, sessions)
	var b bytes.Buffer
	if err := chiefChatsPanel(v).Render(&b); err != nil {
		t.Fatal(err)
	}
	check("page Chats tab", b.String())
	b.Reset()
	if err := chiefThreadRows(ChiefThreadsView{Threads: sessions}).Render(&b); err != nil {
		t.Fatal(err)
	}
	check("panel list", b.String())
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
