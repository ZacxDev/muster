package api

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// TestAMirroredCardPrintsItsWindowsActivity: the host-reported activity
// (mirror.Window.Activity, Unix seconds floored to a minute) reaches the
// mirrored window's sidebar row (where the retired list card went) as the same
// recency dot an agent's row carries — per WINDOW, not per host. The two
// windows' times are pairwise distinct (pinWindows), so a mutant that stamps
// one time on every row, or drops the field, is red. Red before: mirrorCards
// dropped SessionView.Activity and the card had no footer.
func TestAMirroredCardPrintsItsWindowsActivity(t *testing.T) {
	s, h := pinServer(t, &fakeMirror{online: true, windows: pinWindows()}, nil)
	body := getOwed(t, s, h, ui.NavAgentsPath).Body.String()
	for _, w := range pinWindows() {
		row := rowWith(t, body, "session="+w.Session+"&amp;window=%40"+w.ID[1:])
		active := regexp.MustCompile(`data-recency-ts="(\d+)"`).FindStringSubmatch(row)
		if active == nil {
			t.Errorf("%s%s: no recency dot in the row:\n%s", w.Session, w.ID, row)
			continue
		}
		// The hub floors a window's activity to the bucket (mirror.Normalize…).
		if want := (w.Activity - w.Activity%mirror.ActivityBucket) * 1000; active[1] != strconv.FormatInt(want, 10) {
			t.Errorf("%s%s: data-recency-ts = %s, want %d (the window's own activity)", w.Session, w.ID, active[1], want)
		}
	}
}

// TestAMirroredCardWithNoActivityPrintsNone: an older host that reports no
// activity (0) gets NO recency dot, rather than one dated from the Unix epoch.
func TestAMirroredCardWithNoActivityPrintsNone(t *testing.T) {
	ws := pinWindows()
	for i := range ws {
		ws[i].Activity = 0
	}
	s, h := pinServer(t, &fakeMirror{online: true, windows: ws}, nil)
	body := getOwed(t, s, h, ui.NavAgentsPath).Body.String()
	row := rowWith(t, body, "session=main&amp;window=%401")
	if strings.Contains(row, "data-recency") {
		t.Errorf("a window with no reported activity carries one:\n%s", row)
	}
	// Control: an agent of the same answer, with an activity, carries its dot.
	if agent := rowWith(t, body, `id="nav-agent-5"`); !strings.Contains(agent, "data-recency-ts=") {
		t.Errorf("instrument check FAILED: an agent's row with an activity has no dot either:\n%s", agent)
	}
}

// rowWith is the sidebar row (<a id="nav-…"> … </a>) in body that contains
// marker.
func rowWith(t *testing.T, body, marker string) string {
	t.Helper()
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no row with %q in:\n%s", marker, body)
	}
	start := strings.LastIndex(body[:i+len(marker)], `<a id="nav-`)
	if start < 0 {
		t.Fatalf("%q is in no sidebar row:\n%s", marker, body)
	}
	rest := body[start:]
	if j := strings.Index(rest, "</a>"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}
