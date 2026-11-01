package api

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/ui"
)

// THE TERMINAL VIEWS DRAW THE SESSION SWITCHER (operator ask: "Include the
// bottom bar in the agent terminal view as well as in the render view where it
// already is so that I can easily switch agents from the terminal view").
//
// Every Terminal view a person can switch from, served by its real route:
//
//   - a mirrored window WITH a Rendered view (main: a Claude pane), its
//     composer enabled;
//   - a mirrored window with NO Rendered view (chief: its terminal is its only
//     page), on a read-only host — its composer drawn DISABLED;
//   - a mirrored window on an OFFLINE host — no composer at all;
//   - a dispatched claude-code agent's Terminal view.
//
// Each draws #session-switcher once, its own session marked current (and the
// only one marked) in the All view and left out of Recent, with ONE pin toggle
// — the bar's — whose no-script "back" is the Terminal view itself, not the
// session's other view.
//
// Red at origin/main b60d5d99 (no Terminal view drew the bar; chief's page had
// its pin in the header instead); green on this branch.

var (
	switcherRowRe = func(key string) *regexp.Regexp {
		return regexp.MustCompile(`(?s)<div id="switcher-list-` + key + `"[^>]*>(.*?)</div>`)
	}
	switcherCurrentRe = regexp.MustCompile(`<span data-switcher-entry="(agent|mirror)" data-switcher-current="" aria-current="page"[^>]*data-switcher-code="([^"]*)"`)
	switcherPinBackRe = regexp.MustCompile(`(?s)<form [^>]*data-switcher-pin=""[^>]*>.*?<input type="hidden" name="back" value="([^"]*)">`)
)

// theTerminalDrawsTheSwitcher asserts page (a Terminal view) draws the bar with
// the page's own session (kind, code) current and its pin posting back to back.
func theTerminalDrawsTheSwitcher(t *testing.T, what, page, kind, code, back string) {
	t.Helper()
	if n := strings.Count(page, `id="`+ui.SwitcherID+`"`); n != 1 {
		t.Fatalf("%s: %d #%s, want 1", what, n, ui.SwitcherID)
	}
	all := switcherRowRe("all").FindStringSubmatch(page)
	if all == nil {
		t.Fatalf("%s: the bar has no All row", what)
	}
	cur := switcherCurrentRe.FindAllStringSubmatch(all[1], -1)
	if len(cur) != 1 || cur[0][1] != kind || cur[0][2] != code {
		t.Errorf("%s: the All row's current entries are %q, want exactly the page's own %s %q", what, cur, kind, code)
	}
	// The control: the All row holds more than the page's own session, so the
	// one-current check above is not vacuous.
	if n := strings.Count(all[1], `data-switcher-entry="`); n < 2 {
		t.Errorf("%s: fixture: the All row holds %d entries, want another session to switch to", what, n)
	}
	recent := switcherRowRe("recent").FindStringSubmatch(page)
	if recent == nil {
		t.Fatalf("%s: the bar has no Recent row", what)
	}
	if strings.Contains(recent[1], "data-switcher-current") || strings.Contains(recent[1], `data-switcher-code="`+code+`"`) {
		t.Errorf("%s: Recent holds the page's own session:\n%s", what, recent[1])
	}
	if n := strings.Count(page, `id="`+ui.SwitcherPinID+`"`); n != 1 {
		t.Errorf("%s: %d pin toggles, want the bar's one", what, n)
	}
	if m := switcherPinBackRe.FindStringSubmatch(page); m == nil || m[1] != back {
		t.Errorf("%s: the pin's back is %q, want the Terminal view %q", what, m, back)
	}
	// The bar is the column's foot: after the key bar, and inside the
	// composer when there is one.
	if strings.Index(page, `id="`+ui.SwitcherID+`"`) < strings.Index(page, `id="term-keys"`) {
		t.Errorf("%s: the bar is drawn above the key bar", what)
	}
}

func TestTheMirroredTerminalViewsDrawTheSessionSwitcher(t *testing.T) {
	for _, c := range []struct {
		name     string
		fm       *fakeMirror
		path     string
		code     string
		composer string // "enabled", "disabled" or "none"
	}{
		{"a window with a Rendered view", &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}}, mainPage, "main", "enabled"},
		{"a terminal-only window on a read-only host", &fakeMirror{online: true, v2: &fakeMirrorV2{}}, chiefTermPage, "chief", "disabled"},
		{"a window on an offline host", &fakeMirror{online: false}, chiefTermPage, "chief", "none"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, ts := viewServer(t, c.fm)
			resp, page := mirrorGet(t, s, ts, c.path)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s = %d", c.path, resp.StatusCode)
			}
			input := tagOf(page, "mv-input")
			switch {
			case c.composer == "none" && input != "":
				t.Fatalf("fixture: an offline host's page draws a composer: %s", input)
			case c.composer == "disabled" && !disabledAttr.MatchString(input):
				t.Fatalf("fixture: the composer is not drawn disabled: %s", input)
			case c.composer == "enabled" && (input == "" || disabledAttr.MatchString(input)):
				t.Fatalf("fixture: the composer is not drawn enabled: %s", input)
			}
			theTerminalDrawsTheSwitcher(t, c.path, page, "mirror", c.code, strings.ReplaceAll(ui.MirrorTerminalPath("ws", c.code), "&", "&amp;"))
		})
	}
}

func TestAnAgentsTerminalViewDrawsTheSessionSwitcher(t *testing.T) {
	s, ts := sendServer(t, newSendAgents(), &turnGateway{})
	resp, page := mirrorGet(t, s, ts, "/agents/keen-wren/terminal")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the agent's terminal page = %d", resp.StatusCode)
	}
	theTerminalDrawsTheSwitcher(t, "an agent's terminal", page, "agent", "keen-wren", ui.AgentTerminalPath("keen-wren"))
}
