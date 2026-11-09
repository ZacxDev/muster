package api

import (
	"context"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/ui"
)

// TestTheSidebarPartialDrawsThePlaceOnlyForANamedWindow drives the real
// sidebar partial (GET ui.NavAgentsPath — what every htmx/SSE refresh of the
// sidebar re-fetches, and what the command palette builds its rows from on a
// page with no bar) with two windows of one host labelled alike ("kiwi:1"),
// so the host qualifies both (mirror.qualify: "kiwi:1 · main:1", "kiwi:1 ·
// main:2" — its server-side fallback name, sessionname.Mirror's DisplayTitle;
// the row draws "kiwi:1"). @1 is called by a human name ("Billing
// migration"); @2 by that fallback, which is not the bare codename — so only
// mirrorCards' human-name gate, not tmuxPlace's same-name skip, keeps @2 from
// gaining a place.
//
//   - switch off (no row): neither row carries a place;
//   - another user's switch on: the operator's rows are unchanged;
//   - the operator's on: @1's row gains data-switcher-tmux and its line,
//     "kiwi:1 @ ws"; @2's gains nothing.
func TestTheSidebarPartialDrawsThePlaceOnlyForANamedWindow(t *testing.T) {
	ws := claudeWindows()
	ws[0].Label, ws[1].Label = "kiwi:1", "kiwi:1"
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}, windows: ws}
	s, ts := focusMirrorServer(t, fm, namedFocusStore(), nil) // @1: "Billing migration"
	prefs := &memNotifyPrefs{}
	s.ext.NotifyPrefs = prefs
	rows := func() string { t.Helper(); _, b := mirrorGet(t, s, ts, ui.NavAgentsPath); return b }

	off := rows()
	named, plain := cardFor(t, off, ref("main", "@1").ViewPath()), cardFor(t, off, ref("main", "@2").ViewPath())
	if !strings.Contains(named, `data-switcher-name="Billing migration"`) || !strings.Contains(plain, `data-switcher-name="kiwi:1"`) {
		t.Fatalf("fixture: the rows are not named as the test needs:\n%s\n%s", named, plain)
	}
	if strings.Contains(off, "data-switcher-tmux") || strings.Contains(off, "data-nav-tmux") {
		t.Fatalf("off: the rows carry a place:\n%s", off)
	}
	_ = prefs.SetTmuxAddress(context.Background(), 2, true)
	if r := rows(); r != off {
		t.Fatalf("another user's switch changed the operator's rows:\n got  %s\n want %s", r, off)
	}
	_ = prefs.SetTmuxAddress(context.Background(), 1, true)
	on := rows()
	named, plain = cardFor(t, on, ref("main", "@1").ViewPath()), cardFor(t, on, ref("main", "@2").ViewPath())
	if !strings.Contains(named, `data-switcher-tmux="kiwi:1 @ ws"`) || !strings.Contains(named, `<span data-nav-tmux="" class="min-w-0 truncate text-xs font-normal text-muted">kiwi:1 @ ws</span>`) {
		t.Fatalf("on: the named row has no place:\n%s", named)
	}
	if strings.Contains(plain, "data-switcher-tmux") || strings.Contains(plain, "data-nav-tmux") {
		t.Fatalf("on: the window called by its (qualified) codename gained a place:\n%s", plain)
	}
}
