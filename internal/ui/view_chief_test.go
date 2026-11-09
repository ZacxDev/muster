package ui

import (
	"html"
	"regexp"
	"strings"
	"testing"
)

// TestTheViewDrawsTheChiefModeOnlyWhenOffered: with Chief set the Rendered
// view's composer draws the chief switch (ONE always-present checkbox,
// role="switch", unchecked, in a label drawn as the icon button), the chief's
// Send — a submit whose formaction is the chief's route with the window's
// address, so a browser submit reaches the chief's door — and the strip
// pointed at the window's chief log; and it does so on a window whose agent
// composer is DISABLED (an opencode window that is not launcher-bound), where
// the agent's Send stays disabled and the chief's is not, and the box is drawn
// ENABLED but marked (data-agent-off) so the chief mode can send with no
// script. Without Chief none of it is drawn (the composer as it was, its box
// disabled).
func TestTheViewDrawsTheChiefModeOnlyWhenOffered(t *testing.T) {
	base := MirrorViewView{Host: "ws", Session: "main", Window: "@3", Interactive: true, ViewOnly: "opencode",
		ViewOnlyWhy: "this host does not send to opencode", DraftUser: 1}
	off := renderString(t, MirrorViewPage(base))
	for _, id := range []string{`id="mv-to"`, `id="mv-to-chief"`, `id="mv-send-chief"`, `id="mv-chief"`, `id="mv-chief-log"`, ViewChiefPlaceholder, `data-chief-path="`, `data-agent-off=""`} {
		if strings.Contains(off, id) {
			t.Errorf("without Chief the view draws %s", id)
		}
	}
	if !regexp.MustCompile(`<textarea id="mv-input"[^>]*\sdisabled[\s>=]`).MatchString(off) {
		t.Errorf("without Chief the disabled composer's box is no longer disabled")
	}

	on := base
	on.Chief = true
	page := renderString(t, MirrorViewPage(on))
	for _, want := range []string{
		// The switch: a label drawn as the button, a checkbox with the switch
		// role (unchecked: the agent), and the name for each mode as hidden text.
		`<label id="mv-to" title="Sending to opencode — switch to the chief"` +
			` data-label-agent="Sending to opencode — switch to the chief" data-label-chief="Sending to the chief — switch to opencode"`,
		`<input id="mv-to-chief" type="checkbox" role="switch" autocomplete="off" data-mode-key="` +
			html.EscapeString(MirrorDraftKey(1, ViewToChief, "ws", "main", "@3")) + `">`,
		`<span data-to-mode="agent" class="sr-only">Sending to opencode — switch to the chief</span>`,
		`<span data-to-mode="chief" class="sr-only">Sending to the chief — switch to opencode</span>`,
		`id="mv-send-chief" type="submit" formaction="/mirror/view/chief?host=ws&amp;session=main&amp;window=%403"`,
		`id="mv-chief-log" data-src="/mirror/view/chief/log?host=ws&amp;session=main&amp;window=%403"`,
		// The composer's script tells a chief line by the route, drawn by the
		// server (not set by the chief script, which may not run).
		`data-chief-path="/mirror/view/chief"`,
		// The ONE stylesheet path: the checkbox checked.
		`#mv-compose:has(#mv-to-chief:checked) #mv-send-chief { display: inline-flex; }`,
		`#mv-compose:has(#mv-to-chief:checked) #mv-to span[data-to-mode="chief"] { display: inline; }`,
		// The chief's colour, from the theme's tokens: the box, the composer's edge.
		`#mv-compose:has(#mv-to-chief:checked) #mv-input { background-color: rgb(var(--mu-st-review-bg)); box-shadow: inset 0 0 0 2px rgb(var(--mu-st-review-fg)); opacity: 1; }`,
		`#mv-compose:has(#mv-to-chief:checked) { border-top-color: rgb(var(--mu-st-review-fg));`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the chief mode lacks %s", want)
		}
	}
	// The old two-radio switch, the button toggle and its <noscript>
	// duplicate are gone; the mode is never a form field; one selector path.
	for _, gone := range []string{`role="radiogroup"`, `id="mv-to-agent"`, `id="mv-to-row"`, `name="to"`, `<noscript><style>#mv-to`,
		`mv-to-ns`, `aria-pressed`, `data-to="chief"`, `<button id="mv-to"`} {
		if strings.Contains(page, gone) {
			t.Errorf("the chief mode still draws %s", gone)
		}
	}
	// The box is enabled, marked; the agent's own Send is still drawn
	// disabled: the chief mode changes nothing about what the pane can be sent.
	box := regexp.MustCompile(`<textarea id="mv-input"[^>]*>`).FindString(page)
	if regexp.MustCompile(`\sdisabled[\s>=]`).MatchString(box) || !strings.Contains(box, `data-agent-off=""`) {
		t.Errorf("the box of a disabled composer with the chief mode is not enabled-and-marked: %s", box)
	}
	if !strings.Contains(page, `id="mv-send" type="submit" disabled`) {
		t.Errorf("the disabled agent composer's Send is no longer disabled")
	}
	if strings.Contains(page, `id="mv-send-chief" type="submit" disabled`) {
		t.Errorf("the chief's Send is drawn disabled on a view whose agent composer is")
	}
	// A Claude Code window names its agent "Claude", and its box is as it was.
	cc := renderString(t, MirrorViewPage(MirrorViewView{Host: "ws", Session: "main", Window: "@1", Interactive: true, Chief: true}))
	if !strings.Contains(cc, `<span data-to-mode="agent" class="sr-only">Sending to Claude — switch to the chief</span>`) || !strings.Contains(cc, `data-label-chief="Sending to the chief — switch to Claude"`) {
		t.Errorf("a Claude Code window's switch does not name Claude")
	}
	if strings.Contains(cc, `data-agent-off=""`) {
		t.Errorf("an enabled composer's box is marked agent-off")
	}
}

// formControlRE finds the composer form's controls (button, a,
// textarea, label) by id, in document order — read with <noscript> removed.
var formControlRE = regexp.MustCompile(`<(button|a|textarea|label)\b[^>]*\bid="([^"]+)"`)

// TestTheChiefToggleIsTheRowsFirstControl: the toggle is the composer row's
// FIRST control — before the paperclip (which sits inside the input's field),
// the input, the keyboard button, the mic, Send and the ⋯ — on a Claude Code
// window with uploads and dictation, and on a disabled opencode one (no
// paperclip there: a disabled composer draws none).
func TestTheChiefToggleIsTheRowsFirstControl(t *testing.T) {
	for name, v := range map[string]MirrorViewView{
		"claude":   {Host: "ws", Session: "main", Window: "@1", Interactive: true, Chief: true, Upload: "/uploads", Dictate: "/dictate", DraftUser: 1},
		"opencode": {Host: "ws", Session: "main", Window: "@3", Interactive: true, ViewOnly: "opencode", Chief: true, DraftUser: 1},
	} {
		page := renderString(t, MirrorViewPage(v))
		i := strings.Index(page, `<form id="mv-form"`)
		j := strings.Index(page[i:], `</form>`)
		if i < 0 || j < 0 {
			t.Fatalf("%s: no composer form", name)
		}
		form := page[i : i+j]
		// <noscript> holds the fallback, not a scripted control.
		form = regexp.MustCompile(`(?s)<noscript>.*?</noscript>`).ReplaceAllString(form, "")
		var ids []string
		for _, m := range formControlRE.FindAllStringSubmatch(form, -1) {
			ids = append(ids, m[2])
		}
		if len(ids) < 3 || ids[0] != "mv-to" {
			t.Fatalf("%s: the row's controls are %v, want mv-to first", name, ids)
		}
		if name == "claude" {
			if ids[1] != "mv-attach" || ids[2] != "mv-input" {
				t.Errorf("%s: the paperclip and the input do not follow the toggle: %v", name, ids)
			}
		} else if ids[1] != "mv-input" {
			t.Errorf("%s: the input does not follow the toggle: %v", name, ids)
		}
	}
}
