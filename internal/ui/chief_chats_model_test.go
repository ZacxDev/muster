package ui

import (
	"regexp"
	"strings"
	"testing"
)

// THE "+" MODEL MENU AND THE CHIP'S MODEL TAG (chiefNewChatChip,
// chiefChipModelTag).

var (
	newChatEntryRE = regexp.MustCompile(`<form hx-post="([^"]+)"[^>]*class="contents">(<input type="hidden" name="model" value="([^"]*)">)?<button type="submit" ` +
		`data-chief-new-chat-model="([^"]*)" role="menuitem"[^>]*>([^<]*)</button></form>`)
)

// TestThePlusOpensAModelMenuWhereAModelMayBePicked: with NewChatModels the "+"
// is a popover trigger (not a submit) for a menu of Default and each model, in
// order, each entry a form posting its model (none for Default) to the side
// chat route on the strip's surface. The models are the bar's own list (here
// one the renderer cannot know: a mutant hardcoding a list fails).
func TestThePlusOpensAModelMenuWhereAModelMayBePicked(t *testing.T) {
	for _, c := range []struct {
		surface ChatSurface
		path    string
		menuID  string
	}{
		{ChatSurfacePage, "/agents/42/sessions/side", "chief-new-chat-menu"},
		{ChatSurfaceChiefPanel, "/agents/42/sessions/side?surface=chief-panel", "chief-panel-new-chat-menu"},
	} {
		t.Run(c.menuID, func(t *testing.T) {
			bar := &ChiefChatBar{AgentID: 42, SessionID: 5, Primary: true, NewChatModels: []string{"sonnet", "claude-test-model-9", "haiku"}}
			html := renderString(t, chiefChatStrip(bar, "chiefy", chiefSheetSessions(5), c.surface))
			plus := regexp.MustCompile(`<button type="button" data-chief-side-chat="" popovertarget="` + c.menuID + `" aria-haspopup="menu"[^>]*aria-label="New side chat"`)
			if !plus.MatchString(html) {
				t.Errorf("the \"+\" is not the menu's trigger:\n%s", html)
			}
			if strings.Contains(html, `<button type="submit" data-chief-side-chat`) {
				t.Error("the \"+\" still opens a chat at once")
			}
			if !strings.Contains(html, `<div id="`+c.menuID+`" data-chief-new-chat-menu="" popover="auto" role="menu"`) {
				t.Errorf("no popover menu %q", c.menuID)
			}
			var got []string
			for _, m := range newChatEntryRE.FindAllStringSubmatch(html, -1) {
				if m[1] != c.path {
					t.Errorf("entry %q posts to %q, want %q", m[5], m[1], c.path)
				}
				if m[3] != m[4] {
					t.Errorf("entry %q posts model %q but is marked %q", m[5], m[3], m[4])
				}
				got = append(got, m[4]+"="+m[5])
			}
			want := []string{"=Default", "sonnet=Sonnet", "claude-test-model-9=claude-test-model-9", "haiku=Haiku"}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("the menu's entries are %q, want %q", got, want)
			}
		})
	}
}

// TestThePlusOpensAChatAtOnceWithoutModels: no NewChatModels (the runtimes
// gate off) is today's "+": a submit, no menu, no entries.
func TestThePlusOpensAChatAtOnceWithoutModels(t *testing.T) {
	html := renderString(t, chiefChatStrip(&ChiefChatBar{AgentID: 42, SessionID: 5, Primary: true}, "chiefy", chiefSheetSessions(5), ChatSurfacePage))
	if !strings.Contains(html, `<form hx-post="/agents/42/sessions/side" hx-swap="none" class="contents"><button type="submit" data-chief-side-chat=""`) {
		t.Errorf("the \"+\" is not today's one-tap form:\n%s", html)
	}
	for _, banned := range []string{"data-chief-new-chat-menu", "data-chief-new-chat-model", `popovertarget="chief-new-chat-menu"`} {
		if strings.Contains(html, banned) {
			t.Errorf("gate off: the strip draws %s", banned)
		}
	}
}

// TestAChipTagsItsChatsModel: a chat opened on a model carries a compact tag
// naming it; a chat on the chief's own model (Model "") none.
func TestAChipTagsItsChatsModel(t *testing.T) {
	sessions := chiefSheetSessions(5)
	for i := range sessions {
		if sessions[i].ID == 6 {
			sessions[i].Model = "sonnet"
		}
	}
	html := renderString(t, chiefChatStrip(&ChiefChatBar{AgentID: 42, SessionID: 5, Primary: true}, "chiefy", sessions, ChatSurfacePage))
	chip := func(id string) string {
		i := strings.Index(html, `<li data-chief-chip="`+id+`"`)
		if i < 0 {
			t.Fatalf("no chip %s", id)
		}
		rest := html[i:]
		return rest[:strings.Index(rest, "</li>")]
	}
	if c := chip("6"); !regexp.MustCompile(`<span data-chief-chip-model="sonnet"[^>]*>sonnet</span>`).MatchString(c) {
		t.Errorf("the sonnet chat's chip has no sonnet tag:\n%s", c)
	}
	for _, id := range []string{"5", "8", "7"} {
		if strings.Contains(chip(id), "data-chief-chip-model") {
			t.Errorf("chip %s (no model) has a model tag", id)
		}
	}
}
