package ui

import (
	"bytes"
	"regexp"
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

// TestTheChiefChatBarSaysWhichChatAndOffersItsActions: the bar names the chat
// (primary, side, archived), carries a "New side chat" control with VISIBLE
// text on every chat, and the one action each kind has — Fresh primary (with a
// confirm) on the primary, Archive on a side chat, Unarchive on an archived
// one — on both surfaces, the panel's answered into the panel body.
func TestTheChiefChatBarSaysWhichChatAndOffersItsActions(t *testing.T) {
	cases := []struct {
		bar        ChiefChatBar
		kind, act  string
		notActions []string
	}{
		{ChiefChatBar{SessionID: 5, Primary: true}, ChiefChatKindPrimary, chiefFreshPrimaryAttr, []string{chiefArchiveAttr, chiefUnarchiveAttr}},
		{ChiefChatBar{SessionID: 6}, ChiefChatKindSide, chiefArchiveAttr, []string{chiefFreshPrimaryAttr, chiefUnarchiveAttr}},
		{ChiefChatBar{SessionID: 7, Archived: true}, ChiefChatKindArchived, chiefUnarchiveAttr, []string{chiefFreshPrimaryAttr, chiefArchiveAttr}},
	}
	for _, surface := range []ChatSurface{ChatSurfacePage, ChatSurfaceChiefPanel} {
		for _, c := range cases {
			html := renderPane(t, chiefChatView(surface, c.bar, nil))
			if !strings.Contains(html, chiefChatKindAttr+`="`+c.kind+`"`) {
				t.Errorf("%s %s: the bar does not name its kind", surface, c.kind)
			}
			side := regexp.MustCompile(`<button type="submit" ` + chiefSideChatAttr + `="" class="[^"]*">New side chat</button>`)
			if !side.MatchString(html) {
				t.Errorf("%s %s: no New side chat button with visible text", surface, c.kind)
			}
			if !strings.Contains(html, c.act+`=""`) {
				t.Errorf("%s %s: no %s action", surface, c.kind, c.act)
			}
			for _, n := range c.notActions {
				if strings.Contains(html, n+`=""`) {
					t.Errorf("%s %s: carries %s", surface, c.kind, n)
				}
			}
			if c.kind == ChiefChatKindPrimary && !strings.Contains(html, `hx-confirm="`) {
				t.Errorf("%s: Fresh primary has no confirm", surface)
			}
			panelAnswer := strings.Contains(html, `hx-post="/agents/42/sessions/side?surface=chief-panel"`) &&
				strings.Contains(html, `hx-target="#`+chiefPanelBodyID+`"`)
			if (surface == ChatSurfaceChiefPanel) != panelAnswer {
				t.Errorf("%s %s: the side chat's answer is not the surface's (panel answer %v)", surface, c.kind, panelAnswer)
			}
			if !strings.Contains(html, `hx-get="`+ChiefContextPath("chiefy", c.bar.SessionID)+`"`) {
				t.Errorf("%s %s: no context chip slot for its own chat", surface, c.kind)
			}
			// The slot names its own target: in the panel an inherited
			// hx-target="this" (#chief-panel-body) swapped the chip's answer over
			// the whole chat (caught by the e2e suite).
			if !regexp.MustCompile(`<span id="` + chiefContextID + `"[^>]*hx-target="this"`).MatchString(html) {
				t.Errorf("%s %s: the chip slot has no hx-target of its own", surface, c.kind)
			}
		}
	}
	// A worker's chat: no bar.
	var b bytes.Buffer
	_ = agentChatPane(AgentDetailView{ID: 1, Name: "w"}).Render(&b)
	if strings.Contains(b.String(), chiefChatBarAttr) {
		t.Error("a worker's chat draws the chief's bar")
	}
}

// TestTheChiefHeaderHasNoIconOnlyNewChat: on the chief's page the header's
// icon-only ＋ is gone (its new chat is the bar's labelled button); every other
// agent keeps it.
func TestTheChiefHeaderHasNoIconOnlyNewChat(t *testing.T) {
	render := func(v AgentDetailView) string {
		var b bytes.Buffer
		if err := agentHeaderTools(v).Render(&b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	if strings.Contains(render(chiefChatView(ChatSurfacePage, ChiefChatBar{SessionID: 1, Primary: true}, nil)), `aria-label="New chat session"`) {
		t.Error("the chief's header still has the icon-only New chat session button")
	}
	if !strings.Contains(render(AgentDetailView{ID: 3, Name: "w"}), `aria-label="New chat session"`) {
		t.Error("a worker's header lost its New chat button")
	}
}

// TestTheChiefSwitcherListsThePrimaryFirstWithUnreadDots: both switchers (the
// page's drawer and the panel's thread list) draw the rows in the order given —
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
	if err := sessionDrawer(v).Render(&b); err != nil {
		t.Fatal(err)
	}
	check("page drawer", b.String())
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
