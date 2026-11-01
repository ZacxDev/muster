package ui

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// TestATerminalViewsSwitcherIsTheColumnsFootAndInPlaceBeforeTheTerminalMeasures:
// a Terminal view's session switcher (AgentTerminalView.Switcher), with a
// composer (enabled or disabled) and with none:
//
//   - 🔴 THE BAR IS IN THE DOCUMENT BEFORE THE TERMINAL'S SCRIPT, so the
//     terminal's first measure of its box already has the bar's room taken —
//     a bar parsed after it would shrink the box under a sized terminal and
//     send a second, smaller size;
//   - its container is its containing block (relative) and reserves exactly
//     its room, the pinned one with the safe-area inset (switcherRoom(true)):
//     an in-flow composer used to be positioned only when its `/` menu or
//     suggestions row made it so, which would have placed the absolute bar
//     against the page instead;
//   - the safe-area inset is the bar's alone: the key bar and the composer's
//     form above it no longer carry it.
//
// The control is the same page with no switcher, where the last row carries
// the inset, as before.
func TestATerminalViewsSwitcherIsTheColumnsFootAndInPlaceBeforeTheTerminalMeasures(t *testing.T) {
	sw := NewSwitcher(navAgentFixture(), navMirrorFixture(), nil, SwitcherCurrent{Mirror: MirrorRef{Host: "laptop", Session: "s-fresh", Window: "@8"}})
	const safe = "env(safe-area-inset-bottom)"
	classOf := func(n *html.Node) string { c, _ := pinAttr(n, "class"); return c }
	byID := func(doc *html.Node, id string) *html.Node {
		ns := pinFind(doc, func(n *html.Node) bool { v, _ := pinAttr(n, "id"); return v == id })
		if len(ns) != 1 {
			t.Fatalf("%d #%s, want 1", len(ns), id)
		}
		return ns[0]
	}
	for _, c := range []struct {
		name     string
		composer *MirrorComposerView
	}{
		{"with a composer", &MirrorComposerView{SendPath: MirrorTerminalSendPath, Host: "laptop", Session: "s-fresh", Window: "@8"}},
		{"with a disabled composer", &MirrorComposerView{SendPath: MirrorTerminalSendPath, Host: "laptop", Session: "s-fresh", Disabled: MirrorReadOnlyNote}},
		{"with no composer", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := AgentTerminalView{Name: "s-fresh", WSPath: "/mirror/terminal/ws?host=laptop&session=s-fresh", Composer: c.composer, StepUp: &StepUpView{}, Switcher: sw}
			page := renderString(t, AgentTerminalPage(v))
			doc := pinParse(t, page)
			bar := byID(doc, SwitcherID)
			// Before the terminal's script (its first line looks #term up).
			if b, s := strings.Index(page, `id="`+SwitcherID+`"`), strings.Index(page, "var host = document.getElementById('term');"); s < 0 || b > s {
				t.Errorf("the bar (at %d) is not before the terminal's script (at %d)", b, s)
			}
			box := bar.Parent
			if cls := classOf(box); !strings.Contains(" "+cls+" ", " relative ") || !strings.Contains(" "+cls+" ", " "+switcherRoom(true)+" ") {
				t.Errorf("the bar's container does not hold it in its reserved room: class=%q", cls)
			}
			if c.composer != nil {
				if id, _ := pinAttr(box, "id"); id != "mv-compose" {
					t.Errorf("the bar is not the composer's last row: its container is #%s", id)
				}
				if f := classOf(byID(doc, "mv-form")); strings.Contains(f, safe) {
					t.Errorf("the composer's form still carries the safe-area inset: %q", f)
				}
			}
			if cls := classOf(bar); !strings.Contains(cls, "pb-["+safe+"]") {
				t.Errorf("the bar does not carry the safe-area inset: %q", cls)
			}
			if k := classOf(byID(doc, "term-keys")); strings.Contains(k, safe) {
				t.Errorf("the key bar still carries the safe-area inset: %q", k)
			}
			if n := strings.Count(page, `id="`+SwitcherPinID+`"`); n != 1 {
				t.Errorf("%d pin toggles, want the bar's one", n)
			}

			// The control: no switcher, and the last row carries the inset.
			v.Switcher = nil
			ctl := pinParse(t, renderString(t, AgentTerminalPage(v)))
			last := byID(ctl, "term-keys")
			if c.composer != nil {
				last = byID(ctl, "mv-form")
			}
			if !strings.Contains(classOf(last), safe) {
				t.Errorf("control: with no switcher the last row does not carry the safe-area inset: %q", classOf(last))
			}
		})
	}
}
