package api

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// idleCardIDs is the ids of the cards carrying data-idle-section, and whether
// the Idle header was drawn, with the count it says.
func idleCardIDs(body string) (ids []string, head string) {
	for _, m := range regexp.MustCompile(`<article id="([^"]+)"([^>]*)>`).FindAllStringSubmatch(body, -1) {
		if strings.Contains(m[2], "data-idle-section") {
			ids = append(ids, m[1])
		}
	}
	if m := regexp.MustCompile(`data-idle-count="" class="tabular-nums">\(([0-9]+)\)`).FindStringSubmatch(body); m != nil {
		head = m[1]
	}
	return ids, head
}

// TestTheIdleSectionFollowsTheServersClockAndTheLiveRefresh: with the server's
// clock injected 20 days after the fixture (t0 = 2026-05-06T07:08:09Z) and the
// threshold at 14 days, the document and the partial draw every unpinned entry
// in the Idle section (the pinned agent 6 never), with the count; a window
// active again leaves the section on the very next /ui/agents, and the count
// follows. Threshold 0 draws no section at all.
func TestTheIdleSectionFollowsTheServersClockAndTheLiveRefresh(t *testing.T) {
	s, h, _ := listServer(t)
	t0 := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	now := t0.Add(20 * 24 * time.Hour)
	s.now = func() time.Time { return now }
	s.ext.AgentsIdleAfter = 14 * 24 * time.Hour
	fm := s.ext.Mirror.(*fakeMirror)
	mainID, chiefID := "mirror-b0a314c33976", "mirror-dd10d8b1345a"

	for _, path := range []string{"/agents", "/ui/agents"} {
		ids, head := idleCardIDs(getOwed(t, s, h, path).Body.String())
		want := []string{"agent-5", mainID, "agent-7", chiefID}
		if strings.Join(ids, ",") != strings.Join(want, ",") || head != "4" {
			t.Errorf("%s: idle cards %v with count %q, want %v with count 4 (pinned agent-6 never idle)", path, ids, head, want)
		}
	}

	// main:@1 active a minute ago: out of the section on the next refresh.
	fm.mu.Lock()
	fm.windows[0].Activity = now.Add(-time.Minute).Unix()
	fm.mu.Unlock()
	body := getOwed(t, s, h, "/ui/agents").Body.String()
	ids, head := idleCardIDs(body)
	if strings.Join(ids, ",") != "agent-5,agent-7,"+chiefID || head != "3" {
		t.Errorf("after main:@1 became active: idle cards %v with count %q, want [agent-5 agent-7 %s] with 3", ids, head, chiefID)
	}
	if order := listArticle.FindAllStringSubmatch(body, 2); len(order) < 2 || order[1][1] != mainID {
		t.Errorf("the reactivated window is not drawn right after the pinned agent: %v", order)
	}

	// Boundary: chief:@2 (t0+2h) idle EXACTLY 14 days is not idle; a second more is.
	chiefAt := time.Unix(fm.windows[1].Activity, 0)
	now = chiefAt.Add(14 * 24 * time.Hour)
	if ids, _ := idleCardIDs(getOwed(t, s, h, "/ui/agents").Body.String()); strings.Contains(strings.Join(ids, ","), chiefID) {
		t.Errorf("a window idle exactly the threshold is in the section: %v", ids)
	}
	now = now.Add(time.Second)
	if ids, _ := idleCardIDs(getOwed(t, s, h, "/ui/agents").Body.String()); !strings.Contains(strings.Join(ids, ","), chiefID) {
		t.Errorf("a window idle the threshold and a second is not in the section: %v", ids)
	}

	// Off.
	s.ext.AgentsIdleAfter = 0
	if ids, head := idleCardIDs(getOwed(t, s, h, "/agents").Body.String()); len(ids) != 0 || head != "" {
		t.Errorf("threshold 0 drew a section: %v %q", ids, head)
	}
}
