package ui

import (
	"html"
	"strings"
	"testing"
)

// TestTheViewDrawsTheChiefModeOnlyWhenOffered: with Chief set the Rendered
// view's composer draws the "<agent> | Chief" switch (two radios, the agent
// checked), the chief's Send — a submit whose formaction is the chief's route
// with the window's address, so a browser submit reaches the chief's door — and
// the strip pointed at the window's chief log; and it does so on a window whose
// agent composer is DISABLED (an opencode window that is not launcher-bound),
// where the agent's Send stays disabled and the chief's is not. Without Chief
// none of it is drawn (the composer as it was).
func TestTheViewDrawsTheChiefModeOnlyWhenOffered(t *testing.T) {
	base := MirrorViewView{Host: "ws", Session: "main", Window: "@3", Interactive: true, ViewOnly: "opencode",
		ViewOnlyWhy: "this host does not send to opencode", DraftUser: 1}
	off := renderString(t, MirrorViewPage(base))
	for _, id := range []string{`id="mv-to"`, `id="mv-send-chief"`, `id="mv-chief"`, `id="mv-chief-log"`, ViewChiefPlaceholder} {
		if strings.Contains(off, id) {
			t.Errorf("without Chief the view draws %s", id)
		}
	}

	on := base
	on.Chief = true
	page := renderString(t, MirrorViewPage(on))
	for _, want := range []string{
		`<input id="mv-to-agent" type="radio" name="to" value="agent" checked>opencode`,
		`<input id="mv-to-chief" type="radio" name="to" value="chief">Chief`,
		`id="mv-send-chief" type="submit" formaction="/mirror/view/chief?host=ws&amp;session=main&amp;window=%403"`,
		`id="mv-chief-log" data-src="/mirror/view/chief/log?host=ws&amp;session=main&amp;window=%403"`,
		`data-mode-key="` + html.EscapeString(MirrorDraftKey(1, ViewToChief, "ws", "main", "@3")) + `"`,
		// The stylesheet that follows the checked radio, with or without script.
		`#mv-compose:has(#mv-to-chief:checked) #mv-send-chief { display: inline-flex; }`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the chief mode lacks %s", want)
		}
	}
	// The agent's own Send is still drawn disabled: the chief mode changes
	// nothing about what the pane can be sent.
	if !strings.Contains(page, `id="mv-send" type="submit" disabled`) {
		t.Errorf("the disabled agent composer's Send is no longer disabled")
	}
	if strings.Contains(page, `id="mv-send-chief" type="submit" disabled`) {
		t.Errorf("the chief's Send is drawn disabled on a view whose agent composer is")
	}
	// A Claude Code window names its agent segment "Claude".
	cc := renderString(t, MirrorViewPage(MirrorViewView{Host: "ws", Session: "main", Window: "@1", Interactive: true, Chief: true}))
	if !strings.Contains(cc, `value="agent" checked>Claude`) {
		t.Errorf("a Claude Code window's agent segment is not named Claude")
	}
}
