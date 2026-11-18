package ui

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// THE COMPOSER'S TWO ROWS (operator ask): the box on its own full-width row
// ABOVE the button row; the paperclip a button of that row, never placed
// inside the box, in both the collapsed and the open state; every button of
// the row a 40px target. Read structurally — by id, parent and attribute —
// from the parsed page, on every surface that draws the composer.

// composerSurfaces are the pages that draw mirrorComposer, each with
// everything the row can hold that the surface offers.
func composerSurfaces(t *testing.T) map[string]string {
	t.Helper()
	list, mirrors := taggedFixture()
	cur := SwitcherCurrent{Mirror: MirrorRef{Host: "tower", Session: "w", Window: "@1"}}
	return map[string]string{
		// The Rendered view: chief switch, paperclip, keyboard, mic, split Send,
		// the chief's Send, the ⋯.
		"the Rendered view": renderString(t, MirrorViewPage(MirrorViewView{Host: "tower", Session: "w", Window: "@1", Interactive: true,
			Chief: true, Upload: UploadPath, Dictate: TranscribePath, DraftUser: 1,
			Queue:    &ComposerQueue{Src: SendQueuePath},
			Switcher: NewSwitcher(list, mirrors, nil, cur)})),
		// The Rendered view on a server with attachments and dictation off:
		// the muted paperclip and mic.
		"the Rendered view, features off": renderString(t, MirrorViewPage(MirrorViewView{Host: "tower", Session: "w", Window: "@1", Interactive: true,
			UploadOff: true, DictateOff: true, DraftUser: 1})),
		// A Terminal view's composer: the same component, not collapsible.
		"a Terminal view": renderString(t, AgentTerminalPage(AgentTerminalView{Name: "w",
			Composer: &ComposerView{SendPath: MirrorTerminalSendPath, Host: "tower", Session: "w", Upload: UploadPath, Dictate: TranscribePath}})),
	}
}

// elementChildren is n's element children, hidden inputs left out (they are
// not drawn).
func elementChildren(n *html.Node) []*html.Node {
	var out []*html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode {
			continue
		}
		if c.Data == "input" && attrOf(c, "type") == "hidden" {
			continue
		}
		out = append(out, c)
	}
	return out
}

func idsOf(ns []*html.Node) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.Data+"#"+attrOf(n, "id"))
	}
	return out
}

// TestTheBoxHasItsOwnRowAboveTheButtonRow: the form draws exactly two rows, in
// this order — the box's (#mv-input-row, holding the textarea and nothing
// else) and the buttons' (#mv-button-row, holding every control and no box).
// The Rendered view's composer only: a Terminal view's is one row
// (TestTheTerminalComposerIsOneRow).
// 🔴 A REGRESSION GUARD: at the base the form is ONE row, the box a sibling
// of the mic and Send.
func TestTheBoxHasItsOwnRowAboveTheButtonRow(t *testing.T) {
	for name, page := range composerSurfaces(t) {
		if name == "a Terminal view" {
			continue
		}
		doc := parseFragment(t, page)
		form := byID(doc, "mv-form")
		if form == nil {
			t.Fatalf("%s: no composer form", name)
		}
		rows := idsOf(elementChildren(form))
		if strings.Join(rows, " ") != "div#"+composerInputRowID+" div#"+composerButtonRowID {
			t.Errorf("%s: the form's rows are %v, want the box's row then the button row", name, rows)
			continue
		}
		inputRow, buttonRow := byID(form, composerInputRowID), byID(form, composerButtonRowID)
		// The box — and, with dictation, the recording's waveform, which is
		// drawn hidden and takes the box's place while the mic records
		// (dictate.go).
		wantRow := "textarea#mv-input"
		wave := byID(form, "mv-dictate-wave")
		if wave != nil {
			wantRow += " div#mv-dictate-wave"
		}
		if got := idsOf(elementChildren(inputRow)); strings.Join(got, " ") != wantRow {
			t.Errorf("%s: the box's row holds %v, want %s", name, got, wantRow)
		}
		box := byID(doc, "mv-input")
		if box == nil || within(box, buttonRow) {
			t.Errorf("%s: the box is in the button row", name)
		}
		// Every control of the form is in the button row — but the waveform's
		// ×, which is the box's place while recording.
		var controls []string
		walkNodes(form, func(n *html.Node) bool {
			if n.Type == html.ElementNode && (n.Data == "button" || n.Data == "a" || n.Data == "label") && !within(n, buttonRow) &&
				(wave == nil || !within(n, wave)) {
				controls = append(controls, n.Data+"#"+attrOf(n, "id"))
			}
			return true
		})
		if len(controls) > 0 {
			t.Errorf("%s: controls outside the button row: %v", name, controls)
		}
		if send := byID(buttonRow, "mv-send"); send == nil {
			t.Errorf("%s: CONTROL: Send is not in the button row", name)
		}
	}
}

// TestTheTerminalComposerIsOneRow: on a Terminal view — a mirrored session's
// and a dispatched agent's — the composer is ONE row (operator decision: the
// terminal keeps its height): the form's only row is #mv-button-row, and it
// holds, in order, the paperclip, the box, the mic and Send — the paperclip a
// sibling of the box, never inside it. No box row is drawn.
func TestTheTerminalComposerIsOneRow(t *testing.T) {
	for name, page := range map[string]string{
		"a mirrored session's terminal": renderString(t, AgentTerminalPage(AgentTerminalView{Name: "w",
			Composer: &ComposerView{SendPath: MirrorTerminalSendPath, Host: "tower", Session: "w", Upload: UploadPath, Dictate: TranscribePath}})),
		"an agent's terminal": renderString(t, AgentTerminalPage(AgentTerminalView{Name: "keen-wren", RenderedHref: AgentPath("keen-wren"),
			Composer: &ComposerView{SendPath: AgentSendPath("keen-wren"), Upload: UploadPath, Dictate: TranscribePath}})),
	} {
		doc := parseFragment(t, page)
		form := byID(doc, "mv-form")
		if form == nil {
			t.Fatalf("%s: no composer form", name)
		}
		if rows := idsOf(elementChildren(form)); strings.Join(rows, " ") != "div#"+composerButtonRowID {
			t.Errorf("%s: the form's rows are %v, want the one row #%s", name, rows, composerButtonRowID)
			continue
		}
		var got []string
		for _, c := range elementChildren(byID(form, composerButtonRowID)) {
			got = append(got, c.Data+"#"+attrOf(c, "id"))
		}
		// The waveform beside the box (it takes the box's place while
		// recording) and the ↑ before Send (Send's place) — dictate.go.
		if want := "input#mv-file button#mv-attach textarea#mv-input div#mv-dictate-wave button#mv-dictate button#mv-dictate-send button#mv-send"; strings.Join(got, " ") != want {
			t.Errorf("%s: the row is %v, want %s", name, got, want)
		}
		if byID(doc, composerInputRowID) != nil {
			t.Errorf("%s: a box row is drawn", name)
		}
	}
}

// TestThePaperclipIsAButtonOfTheRowInBothStates: the paperclip (#mv-attach,
// or the muted #mv-attach-off) is a CHILD of the button row — not inside the
// box's row, which is the one thing the collapsed state hides — and is not
// positioned out of the row's flow. The collapsed state's rules hide the box's
// row and show "Type a message"; none of them names the paperclip, so it is
// drawn the same, in the same row, collapsed and open.
// 🔴 A REGRESSION GUARD: at the base the paperclip is absolutely placed inside
// the box's field (data-mv-field) and the collapsed rules restyle it.
func TestThePaperclipIsAButtonOfTheRowInBothStates(t *testing.T) {
	want := map[string]string{"the Rendered view": "mv-attach", "the Rendered view, features off": "mv-attach-off", "a Terminal view": "mv-attach"}
	for name, page := range composerSurfaces(t) {
		doc := parseFragment(t, page)
		clip := byID(doc, want[name])
		if clip == nil {
			t.Errorf("%s: no #%s", name, want[name])
			continue
		}
		if clip.Parent == nil || attrOf(clip.Parent, "id") != composerButtonRowID {
			t.Errorf("%s: #%s's parent is %s#%s, want the button row", name, want[name], clip.Parent.Data, attrOf(clip.Parent, "id"))
		}
		for p := clip; p != nil; p = p.Parent {
			if p.Type == html.ElementNode {
				if _, ok := nodeAttr(p, "data-mv-field"); ok {
					t.Errorf("%s: the paperclip is inside the collapsible field", name)
				}
			}
		}
		for _, pos := range []string{"absolute", "fixed", "sticky"} {
			if hasClass(attrOf(clip, "class"), pos) {
				t.Errorf("%s: the paperclip is %s-positioned: %q", name, pos, attrOf(clip, "class"))
			}
		}
		// The only collapsible field is the box's row (the Rendered view's).
		fields := allAttr(doc, "data-mv-field")
		if name == "a Terminal view" {
			if len(fields) != 0 {
				t.Errorf("%s: a field is marked collapsible", name)
			}
		} else if len(fields) != 1 || attrOf(fields[0], "id") != composerInputRowID {
			t.Errorf("%s: the collapsible field is %v, want the box's row alone", name, idsOf(fields))
		}
	}
	css, err := os.ReadFile("../../web/css/input.css")
	if err != nil {
		t.Fatal(err)
	}
	rules := regexp.MustCompile(`([^{}]*\[data-mv-collapsed\][^{}]*)\{([^}]*)\}`).FindAllStringSubmatch(string(css), -1)
	got := map[string]string{}
	for _, r := range rules {
		got[strings.TrimSpace(r[1])] = strings.Join(strings.Fields(r[2]), " ")
	}
	wantRules := map[string]string{
		"#mv-form[data-mv-collapsed] [data-mv-field]": "display: none;",
		"#mv-form[data-mv-collapsed] #mv-text":        "display: inline-flex;",
	}
	if len(got) != len(wantRules) {
		t.Errorf("the collapsed state's rules are %v, want exactly %v", got, wantRules)
	}
	for sel, decl := range wantRules {
		if got[sel] != decl {
			t.Errorf("collapsed rule %q = %q, want %q", sel, got[sel], decl)
		}
	}
}

// TestComposerButtonsAre40px: every control of the button row — and the agent
// chat's mic and Send, which wear the same classes — is a 40px target
// (min-h/min-w 40px, or h-10 w-10 for the ⋯), and none is still 44px. The
// set checked is pinned, so a control that drops out of the row (or one
// that is never checked) fails rather than passing unseen. The bar's own ⋯
// (a Terminal view's) keeps its 44px.
func TestComposerButtonsAre40px(t *testing.T) {
	is40 := func(cls string) bool {
		return (hasClass(cls, "min-h-[40px]") && hasClass(cls, "min-w-[40px]")) || (hasClass(cls, "h-10") && hasClass(cls, "w-10"))
	}
	is44 := func(cls string) bool {
		for _, c := range []string{"min-h-[44px]", "min-w-[44px]", "h-11", "w-11"} {
			if hasClass(cls, c) {
				return true
			}
		}
		return false
	}
	wantIDs := map[string]string{
		"the Rendered view":               "label#mv-to button#mv-attach button#mv-text button#mv-dictate button#mv-dictate-send button#mv-send button#mv-send-more button#mv-send-chief button#switcher-more-btn",
		"the Rendered view, features off": "a#mv-attach-off button#mv-text a#mv-dictate-off button#mv-send",
		"a Terminal view":                 "button#mv-attach button#mv-dictate-discard button#mv-dictate button#mv-dictate-send button#mv-send",
	}
	for name, page := range composerSurfaces(t) {
		doc := parseFragment(t, page)
		row := byID(doc, composerButtonRowID)
		if row == nil {
			t.Errorf("%s: no button row", name)
			continue
		}
		var seen []string
		walkNodes(row, func(n *html.Node) bool {
			if n.Type != html.ElementNode {
				return true
			}
			// A popover's items are a menu's rows, not the row's targets.
			if _, ok := nodeAttr(n, "popover"); ok {
				return false
			}
			if n.Data == "button" || n.Data == "a" || n.Data == "label" {
				seen = append(seen, n.Data+"#"+attrOf(n, "id"))
				cls := attrOf(n, "class")
				if !is40(cls) || is44(cls) {
					t.Errorf("%s: %s#%s is not a 40px target: %q", name, n.Data, attrOf(n, "id"), cls)
				}
			}
			return true
		})
		if got := strings.Join(seen, " "); got != wantIDs[name] {
			t.Errorf("%s: the row's controls are\n  %s\nwant\n  %s", name, got, wantIDs[name])
		}
	}
	// The agent chat's form: the same Send and mic classes.
	chat := parseFragment(t, renderString(t, agentChatPane(AgentDetailView{Name: "a", Status: "running", ActiveSessionID: 1, Dictate: TranscribePath})))
	for _, id := range []string{"chat-send", "chat-dictate", "chat-dictate-send", "chat-dictate-discard"} {
		n := byID(chat, id)
		if n == nil || !is40(attrOf(n, "class")) || is44(attrOf(n, "class")) {
			t.Errorf("the agent chat's #%s is not a 40px target", id)
		}
	}
	// The bar's ⋯ (a Terminal view draws it in the bar) keeps its 44px.
	list, mirrors := taggedFixture()
	term := parseFragment(t, renderString(t, AgentTerminalPage(AgentTerminalView{Name: "w",
		Switcher: NewSwitcher(list, mirrors, nil, SwitcherCurrent{Mirror: MirrorRef{Host: "tower", Session: "w", Window: "@1"}}),
		Composer: &ComposerView{SendPath: MirrorTerminalSendPath, Host: "tower", Session: "w"}})))
	if more := byID(term, switcherMoreBtnID); more == nil || !hasClass(attrOf(more, "class"), "h-11") || within(more, byID(term, composerButtonRowID)) {
		t.Error("CONTROL: the Terminal view's bar ⋯ is not the bar's 44px button outside the composer's row")
	}
}
