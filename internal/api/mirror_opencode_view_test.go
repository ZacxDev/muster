package api

import (
	"html"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// AN OPENCODE WINDOW's RENDERED VIEW (docs/design/opencode.md): its sidebar
// row opens the rendered view when the host names the session its pane title matched,
// the page draws the conversation socket like a Claude Code window's, and its
// composer is DISABLED, saying why, with no send queue — on an interactive
// host too: muster views opencode and sends nothing to it.
//
// 🔴 REGRESSION TEST. On origin/main an opencode window is not on the wire: its
// card opens the Terminal, and /mirror/view redirects there.
func TestAnOpenCodeWindowsCardOpensItsReadOnlyRenderedView(t *testing.T) {
	windows := []mirror.Window{
		{Session: "main", ID: "@1", Index: 0, Active: true, Command: "opencode", Runtime: mirror.RuntimeOpenCode, AgentSession: "ses_0fixture1a2b",
			Status: mirror.StatusIdle, StatusSince: 7},
		// An opencode window whose title matched no session: no view.
		{Session: "main", ID: "@2", Index: 1, Command: "opencode", Runtime: mirror.RuntimeOpenCode},
	}
	fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}, windows: windows, statusReport: true}
	s, ts := viewServer(t, fm)
	// The sidebar's rows (where the retired Agents list's cards went).
	_, list := mirrorGet(t, s, ts, ui.NavAgentsPath)
	view := ui.MirrorRef{Host: "ws", Session: "main", Window: "@1"}
	none := ui.MirrorRef{Host: "ws", Session: "main", Window: "@2"}
	if !strings.Contains(list, `href="`+amp(view.ViewPath())+`"`) {
		t.Fatalf("the opencode row with a session does not open its rendered view:\n%s", list)
	}
	if !strings.Contains(list, `href="`+amp(none.TerminalPath())+`"`) {
		t.Fatal("the opencode row with no session does not open its terminal")
	}
	// Found by the kind (the palette's words; the card's filter field has no
	// successor on the row).
	if n := strings.Count(list, `data-switcher-words="opencode main ws"`); n != 2 {
		t.Errorf("%d rows are found as opencode, want 2:\n%s", n, list)
	}

	resp, page := mirrorGet(t, s, ts, view.ViewPath())
	if resp.StatusCode != 200 {
		t.Fatalf("the opencode view page = %d", resp.StatusCode)
	}
	for _, want := range []string{`id="mv-log"`, `data-ws="` + amp(view.ViewWSPath()) + `"`, "opencode is working…",
		// The note, pinned literally (ui.MirrorViewOnlyNote spells it).
		html.EscapeString("Sending to opencode isn't supported yet: muster shows this session read-only. Type in the terminal.")} {
		if !strings.Contains(page, want) {
			t.Errorf("the opencode view lacks %q", want)
		}
	}
	i := strings.Index(page, `id="mv-input"`)
	tag := page[strings.LastIndex(page[:i], "<"):]
	tag = tag[:strings.Index(tag, ">")]
	if !disabledAttr.MatchString(tag) {
		t.Fatalf("the opencode view's composer is enabled: %s", tag)
	}
	if strings.Contains(page, ui.SendQueuePath) || strings.Contains(page, "Claude is working") {
		t.Fatal("the opencode view offers a send queue, or calls the agent Claude")
	}

	resp, _ = mirrorGet(t, s, ts, none.ViewPath())
	if resp.StatusCode != 303 || resp.Header.Get("Location") != none.TerminalPath() {
		t.Fatalf("the unmatched opencode window's view = %d → %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// TestAnOpenCodeWindowsCardSaysWhyItHasNoView: the host's reason for each
// state the person must act on (mirror.Window.AgentNote, spelled by
// internal/mirrorhost) is drawn, as it was sent, on the page the window's
// entry opens — its terminal, since none of them has a view (the retired
// Agents list drew it on the window's card) — and each page says its OWN
// window's reason only.
func TestAnOpenCodeWindowsCardSaysWhyItHasNoView(t *testing.T) {
	reasons := []string{
		"opencode is not paired on this host: run muster-mirror opencode-pair",
		"the opencode server refused muster-mirror's token (it expired, or the server's password was rotated): pair the host again with `muster-mirror opencode-pair`",
		`2 opencode sessions match the title "New session": ambiguous, so none is shown`,
		`no opencode session is titled "Something else"`,
		"this opencode runs on its own server (--standalone or --server), not the host's",
		"opencode token expires in 2 days — renewal failing: renew the opencode token: the opencode server refused the credential (401)",
	}
	var windows []mirror.Window
	for i, r := range reasons {
		windows = append(windows, mirror.Window{Session: "main", ID: "@" + strconv.Itoa(i+1), Index: i, Command: "opencode",
			Runtime: mirror.RuntimeOpenCode, AgentNote: r})
	}
	fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}, windows: windows, statusReport: true}
	s, ts := viewServer(t, fm)
	for i, r := range reasons {
		w := ui.MirrorRef{Host: "ws", Session: "main", Window: "@" + strconv.Itoa(i+1)}
		resp, page := mirrorGet(t, s, ts, w.TerminalPath())
		if resp.StatusCode != 200 {
			t.Fatalf("%s = %d", w.TerminalPath(), resp.StatusCode)
		}
		if !strings.Contains(page, html.EscapeString(r)) {
			t.Errorf("%s does not say %q", w.TerminalPath(), r)
		}
		for j, other := range reasons {
			if j != i && strings.Contains(page, html.EscapeString(other)) {
				t.Errorf("%s says another window's reason %q", w.TerminalPath(), other)
			}
		}
	}
}
