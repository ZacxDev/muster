package api

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// TestMirrorLabelsReachEverySurfaceAndNeverTheAddress: a host's labels (the
// plugin's HostView.Labels) title the sidebar's row (where the retired Agents
// list's card went), the terminal page and the rendered view, through the real
// mux — while every href and socket path still
// names the SESSION. Its control is the same host with no labels: raw names.
func TestMirrorLabelsReachEverySurfaceAndNeverTheAddress(t *testing.T) {
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}, labels: map[string]string{"main": "mango:1"}}
	s, ts := viewServer(t, fm)

	_, list := mirrorGet(t, s, ts, ui.NavAgentsPath)
	// The labelled row: named by its label, found by its raw session name too
	// (the card's "tmux session main" tooltip is the palette's word now), and
	// linking the session by its NAME. The unlabelled one: its raw name.
	main := cardFor(t, list, ui.MirrorViewPath("ws", "main"))
	for _, want := range []string{`title="mango:1"`, `truncate">mango:1</span>`, `data-switcher-words="claude code main ws"`} {
		if !strings.Contains(main, want) {
			t.Errorf("the labelled row lacks %q:\n%s", want, main)
		}
	}
	if chief := cardFor(t, list, ui.MirrorTerminalPath("ws", "chief")); !strings.Contains(chief, `truncate">chief</span>`) {
		t.Errorf("the unlabelled row is not its raw name:\n%s", chief)
	}
	if strings.Contains(list, "session=mango") {
		t.Error("a link addresses the session by its label")
	}

	_, term := mirrorGet(t, s, ts, ui.MirrorTerminalPath("ws", "main"))
	if !strings.Contains(term, "<title>Terminal · mango:1 · muster</title>") || !strings.Contains(term, `title="tmux session main"`) {
		t.Error("the terminal page is not titled by the label with the raw name")
	}
	if !strings.Contains(term, amp(ui.MirrorTerminalWSPath("ws", "main"))) {
		t.Error("the terminal's socket does not address the session by name")
	}
	_, chiefTerm := mirrorGet(t, s, ts, ui.MirrorTerminalPath("ws", "chief"))
	if !strings.Contains(chiefTerm, "<title>Terminal · chief · muster</title>") || strings.Contains(chiefTerm, "data-mirror-session") {
		t.Error("an unlabelled session's terminal is not titled by its raw name alone")
	}

	_, view := mirrorGet(t, s, ts, ui.MirrorViewPath("ws", "main"))
	if !strings.Contains(view, "<title>mango:1 · Mirrored · muster</title>") {
		t.Error("the rendered view is not titled by the label")
	}

	// Control: no labels — every surface is the raw name.
	s2, ts2 := viewServer(t, &fakeMirror{online: true, v2: &fakeMirrorV2{}})
	_, plain := mirrorGet(t, s2, ts2, ui.NavAgentsPath)
	if row := cardFor(t, plain, ui.MirrorViewPath("ws", "main")); strings.Contains(row, "mango") || !strings.Contains(row, `truncate">main</span>`) {
		t.Errorf("an unlabelled host's row is not its raw name:\n%s", row)
	}
}

// TestASharedLabelIsQualifiedByItsHostOnEverySurface: two hosts both list
// "mango:1" (the measured bug: two cards titled exactly that, the operator
// opened the wrong one). Through the real mux, the sidebar's row (where the
// retired list's card went), the terminal page and the rendered view all call
// ws's entry "mango:1 · ws" — header AND <title> — and the other host's row
// "mango:1 · tower". A label only ONE host lists ("solo" below) is left as it
// is.
func TestASharedLabelIsQualifiedByItsHostOnEverySurface(t *testing.T) {
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}, labels: map[string]string{"main": "mango:1", "chief": "solo"},
		others: []mirror.HostView{{Host: "tower", Online: true, Sessions: []string{"mango"}, Labels: map[string]string{"mango": "mango:1"}}}}
	s, ts := viewServer(t, fm)

	_, list := mirrorGet(t, s, ts, ui.NavAgentsPath)
	for _, c := range []struct{ href, name string }{
		{ui.MirrorViewPath("ws", "main"), "mango:1 · ws"},
		{ui.MirrorTerminalPath("tower", "mango"), "mango:1 · tower"},
		// Unqualified, and with no raw session name beside it.
		{ui.MirrorTerminalPath("ws", "chief"), "solo"},
	} {
		if row := cardFor(t, list, c.href); !strings.Contains(row, `title="`+c.name+`"`) || !strings.Contains(row, `truncate">`+c.name+`</span>`) {
			t.Errorf("the row of %s is not named %q:\n%s", c.href, c.name, row)
		}
	}
	if n := strings.Count(list, `mango:1 · `); n != 4 {
		t.Errorf("%d qualified names, want 4 (the two mango:1 rows' title and text, not solo)", n)
	}

	_, term := mirrorGet(t, s, ts, ui.MirrorTerminalPath("ws", "main"))
	for _, want := range []string{"<title>Terminal · mango:1 · ws · muster</title>",
		`<span data-mirror-label="">mango:1</span><span data-mirror-qualifier=""> · ws</span>`} {
		if !strings.Contains(term, want) {
			t.Errorf("the terminal page lacks %q", want)
		}
	}
	_, view := mirrorGet(t, s, ts, ui.MirrorViewPath("ws", "main"))
	for _, want := range []string{"<title>mango:1 · ws · Mirrored · muster</title>",
		`<span data-mirror-label="">mango:1</span><span data-mirror-qualifier=""> · ws</span>`} {
		if !strings.Contains(view, want) {
			t.Errorf("the rendered view lacks %q", want)
		}
	}
	_, other := mirrorGet(t, s, ts, ui.MirrorTerminalPath("tower", "mango"))
	if !strings.Contains(other, "<title>Terminal · mango:1 · tower · muster</title>") {
		t.Error("the other host's terminal page is not titled mango:1 · tower")
	}
	_, unique := mirrorGet(t, s, ts, ui.MirrorTerminalPath("ws", "chief"))
	if !strings.Contains(unique, "<title>Terminal · solo · muster</title>") || strings.Contains(unique, "data-mirror-qualifier") {
		t.Error("a label only one host lists was qualified")
	}

	// Control: the clashing host gone — ws's entry is plain "mango:1" again.
	fm.mu.Lock()
	fm.others = nil
	fm.mu.Unlock()
	_, alone := mirrorGet(t, s, ts, ui.NavAgentsPath)
	if row := cardFor(t, alone, ui.MirrorViewPath("ws", "main")); strings.Contains(row, " · ws") || !strings.Contains(row, `truncate">mango:1</span>`) {
		t.Errorf("with no clash left, the row is still qualified:\n%s", row)
	}
	_, aloneTerm := mirrorGet(t, s, ts, ui.MirrorTerminalPath("ws", "main"))
	if !strings.Contains(aloneTerm, "<title>Terminal · mango:1 · muster</title>") {
		t.Error("with no clash left, the terminal page is still qualified")
	}
}
