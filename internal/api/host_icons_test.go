package api

import (
	"regexp"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// rowHostIcon is a mirrored sidebar row's host (its link's host=) and the icon
// it draws (data-host-icon), in the order the sidebar draws them.
var rowHostIcon = regexp.MustCompile(`(?s)<a id="nav-mirror-[0-9a-f]+" href="/mirror/[a-z]+\?host=([^&"]+)&amp;[^"]*"[^>]*>.*?data-host-icon="([a-z]+)"`)

// TestEachHostDrawsTheIconOfItsRegistrationRank: through the real mux, the
// sidebar (where the retired Agents list's cards went) draws each host's icon
// by the plugin's registration ranks (Mirror.HostRanks → ui.HostIconFor) — two
// hosts, two icons — and a host's window's terminal page draws the same one its
// rows do. The ranks are given in the OPPOSITE order to the list's host order
// and to the names', so an icon picked by position on the list or by name
// would be red.
func TestEachHostDrawsTheIconOfItsRegistrationRank(t *testing.T) {
	ranks := map[string]int{"tower": 0, "ws": 1}
	fm := &fakeMirror{online: true, ranks: ranks,
		others: []mirror.HostView{{Host: "tower", Online: true, Sessions: []string{"mango"}}}}
	s, ts := viewServer(t, fm)

	_, list := mirrorGet(t, s, ts, ui.NavAgentsPath)
	got := map[string]map[string]bool{}
	for _, m := range rowHostIcon.FindAllStringSubmatch(list, -1) {
		if got[m[1]] == nil {
			got[m[1]] = map[string]bool{}
		}
		got[m[1]][m[2]] = true
	}
	for host, r := range ranks {
		want := ui.HostIconFor(map[string]int{host: r}, host)
		if len(got[host]) != 1 || !got[host][want] {
			t.Errorf("host %s's rows draw %v; want only %q (rank %d)", host, got[host], want, r)
		}
	}
	if ui.HostIconFor(ranks, "tower") == ui.HostIconFor(ranks, "ws") {
		t.Fatal("control: the two ranks name one icon")
	}

	_, term := mirrorGet(t, s, ts, ui.MirrorTerminalPath("tower", "mango"))
	badge := regexp.MustCompile(`data-term-badge=""[^>]*><span data-host-icon="([a-z]+)"`).FindStringSubmatch(term)
	if badge == nil || badge[1] != ui.HostIconFor(ranks, "tower") {
		t.Errorf("tower's terminal page badge draws %v, want %q", badge, ui.HostIconFor(ranks, "tower"))
	}
}
