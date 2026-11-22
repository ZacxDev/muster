package ui

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// updateSidebarGolden rewrites testdata/sidebar-*.golden.html from the code.
var updateSidebarGolden = flag.Bool("update-sidebar-golden", false, "rewrite the sidebar goldens")

// sidebarGoldenCases are the sidebars the golden pins: every nav entry's
// gate on and off, an admin and a member, each kind of entry marked current.
var sidebarGoldenCases = []struct {
	name               string
	active, current    string
	hasPanels, overlay bool
	feat               Features
}{
	{name: "bare", active: "tasks", current: "tasks", hasPanels: true},
	{name: "member-all-on", active: NavPages, current: NavPages,
		feat: Features{Connectors: true, Attention: true, Pages: true, Officers: true,
			Apps: []AppTab{{ID: 7, Label: "Board", Icon: "B"}, {ID: 9, Label: "Ledger", Icon: "L"}}}},
	{name: "admin-connectors-off", active: NavAdmin, current: NavAdmin, overlay: true,
		feat: Features{Admin: true, Setup: true, Attention: true, Officers: true}},
	{name: "setup-asks-current", active: NavAttention, current: NavAttention,
		feat: Features{Setup: true, Attention: true, Pages: true, Apps: []AppTab{{ID: 3, Label: "Notes", Icon: "N"}}}},
	{name: "app-current", active: NavAppEntry(9), current: NavAppEntry(9), hasPanels: true,
		feat: Features{Connectors: true, Admin: true, Setup: true, Attention: true, Pages: true, Officers: true,
			Apps: []AppTab{{ID: 9, Label: "Ledger", Icon: "L"}}}},
}

// sidebarNavHTML is sidebarIn's render up to the command palette: the
// backdrop and the whole #sidebar panel, every nav link in it.
func sidebarNavHTML(t *testing.T, active, current string, hasPanels bool, feat Features, overlay bool) string {
	t.Helper()
	h := renderString(t, sidebarIn(active, current, hasPanels, feat, overlay))
	i := strings.Index(h, `<div id="`+paletteID+`"`)
	if i < 0 {
		t.Fatal("the sidebar draws no palette")
	}
	return h[:i]
}

// TestTheSidebarIsUnchangedByTheNavList pins the sidebar's markup, byte for
// byte, across feature sets, roles and the entry marked current: the nav list
// (nav_pages.go) the sidebar and the palette now both read must draw exactly
// the links the hand-written sidebar drew. The goldens were written by the
// sidebar BEFORE the list existed. An invariant guard (it cannot go red on the
// base, which wrote it): it is what keeps the refactor a refactor.
func TestTheSidebarIsUnchangedByTheNavList(t *testing.T) {
	for _, c := range sidebarGoldenCases {
		got := sidebarNavHTML(t, c.active, c.current, c.hasPanels, c.feat, c.overlay)
		path := filepath.Join("testdata", "sidebar-"+c.name+".golden.html")
		if *updateSidebarGolden {
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Errorf("%s: the sidebar changed (rewrite only for an intended change: -update-sidebar-golden)\n got: %s\nwant: %s", c.name, got, want)
		}
	}
}
