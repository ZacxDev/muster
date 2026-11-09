package ui

import (
	"html"
	"regexp"
	"strings"
	"testing"
)

// TestTheViewDrawsTheChiefModeOnlyWhenOffered: with Chief set the Rendered
// view's composer draws the chief toggle (one button, on the agent), the
// chief's Send — a submit whose formaction is the chief's route with the
// window's address, so a browser submit reaches the chief's door — and the
// strip pointed at the window's chief log; and it does so on a window whose
// agent composer is DISABLED (an opencode window that is not launcher-bound),
// where the agent's Send stays disabled and the chief's is not. Without Chief
// none of it is drawn (the composer as it was).
func TestTheViewDrawsTheChiefModeOnlyWhenOffered(t *testing.T) {
	base := MirrorViewView{Host: "ws", Session: "main", Window: "@3", Interactive: true, ViewOnly: "opencode",
		ViewOnlyWhy: "this host does not send to opencode", DraftUser: 1}
	off := renderString(t, MirrorViewPage(base))
	for _, id := range []string{`id="mv-to"`, `id="mv-to-chief"`, `id="mv-send-chief"`, `id="mv-chief"`, `id="mv-chief-log"`, ViewChiefPlaceholder} {
		if strings.Contains(off, id) {
			t.Errorf("without Chief the view draws %s", id)
		}
	}

	on := base
	on.Chief = true
	page := renderString(t, MirrorViewPage(on))
	for _, want := range []string{
		// The toggle: a real button, not pressed (the agent), named by the
		// current target and the action, carrying both labels for the script.
		`<button id="mv-to" type="button" aria-pressed="false" aria-label="Sending to opencode — switch to the chief" title="Sending to opencode — switch to the chief"` +
			` data-label-agent="Sending to opencode — switch to the chief" data-label-chief="Sending to the chief — switch to opencode"`,
		`id="mv-send-chief" type="submit" formaction="/mirror/view/chief?host=ws&amp;session=main&amp;window=%403"`,
		`id="mv-chief-log" data-src="/mirror/view/chief/log?host=ws&amp;session=main&amp;window=%403"`,
		`data-mode-key="` + html.EscapeString(MirrorDraftKey(1, ViewToChief, "ws", "main", "@3")) + `"`,
		// The stylesheet that follows the mode, with script (data-to) or
		// without (the <noscript> checkbox).
		`#mv-compose[data-to="chief"] #mv-send-chief, #mv-compose:has(#mv-to-chief:checked) #mv-send-chief { display: inline-flex; }`,
		// The chief's colour, from the theme's tokens: the box, the composer's edge.
		`#mv-compose[data-to="chief"] #mv-input, #mv-compose:has(#mv-to-chief:checked) #mv-input { background-color: rgb(var(--mu-st-review-bg)); box-shadow: inset 0 0 0 2px rgb(var(--mu-st-review-fg)); }`,
		`#mv-compose[data-to="chief"], #mv-compose:has(#mv-to-chief:checked) { border-top-color: rgb(var(--mu-st-review-fg));`,
		// The no-script equivalent: the button hidden, a checkbox in its place.
		`<noscript><style>#mv-to { display: none !important; }</style><label id="mv-to-ns"`,
		`<input id="mv-to-chief" type="checkbox" aria-label="Send to the chief">`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the chief mode lacks %s", want)
		}
	}
	// The old two-radio switch is gone, and the mode is never a form field.
	for _, gone := range []string{`role="radiogroup"`, `id="mv-to-agent"`, `id="mv-to-row"`, `name="to"`} {
		if strings.Contains(page, gone) {
			t.Errorf("the chief mode still draws %s", gone)
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
	// A Claude Code window names its agent "Claude".
	cc := renderString(t, MirrorViewPage(MirrorViewView{Host: "ws", Session: "main", Window: "@1", Interactive: true, Chief: true}))
	if !strings.Contains(cc, `aria-label="Sending to Claude — switch to the chief"`) || !strings.Contains(cc, `data-label-chief="Sending to the chief — switch to Claude"`) {
		t.Errorf("a Claude Code window's toggle does not name Claude")
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
