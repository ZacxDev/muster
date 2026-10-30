package api

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/recents"
	"github.com/ZacxDev/muster/internal/ui"
)

// listServer is the pins fixture (three agents, two mirrored windows, agent 6
// pinned, a recent entry) with a provisioner that counts its reads.
func listServer(t *testing.T) (*Server, http.Handler, *countingInstances) {
	t.Helper()
	mem := &memRecents{}
	mem.put(recents.Entry{AgentID: 5})
	mp := &memPins{}
	s, _ := pinServer(t, &fakeMirror{online: true, windows: pinWindows()}, mp)
	mp.pinFor(1).Agents[6] = true
	prov := &countingInstances{Provisioner: newFakeTerminal()}
	s.ext.Recents = mem
	s.ext.Provisioner = prov
	return s, s.Handler(), prov
}

// navRowID is a sidebar row's id.
var navRowID = regexp.MustCompile(`<a id="(nav-[^"]+)"`)

// idleRowIDs is the ids of the sidebar's rows inside its Idle group
// (ui.NavIdleID), in order, the count its summary says, and every row id of
// the partial.
func idleRowIDs(body string) (ids []string, count string, all []string) {
	for _, m := range navRowID.FindAllStringSubmatch(body, -1) {
		all = append(all, m[1])
	}
	i := strings.Index(body, `<details id="`+ui.NavIdleID+`"`)
	if i < 0 {
		return nil, "", all
	}
	group := body[i:]
	group = group[:strings.Index(group, "</details>")]
	for _, m := range navRowID.FindAllStringSubmatch(group, -1) {
		ids = append(ids, m[1])
	}
	if m := regexp.MustCompile(`data-nav-idle-count="" class="tabular-nums">\(([0-9]+)\)`).FindStringSubmatch(group); m != nil {
		count = m[1]
	}
	return ids, count, all
}

// TestTheIdleSectionFollowsTheServersClockAndTheLiveRefresh: with the server's
// clock injected 20 days after the fixture (t0 = 2026-05-06T07:08:09Z) and the
// threshold at 14 days, the sidebar (GET ui.NavAgentsPath) draws every
// unpinned entry in its collapsed Idle group (the pinned agent 6 never), with
// the count; a window active again leaves the group on the very next refresh,
// and the count follows. Threshold 0 draws no group at all.
//
// It is the retired Agents list's Idle section, moved into the sidebar; the
// same InIdleSection rule, read off the same build (agentsListCardsLive).
func TestTheIdleSectionFollowsTheServersClockAndTheLiveRefresh(t *testing.T) {
	s, h, _ := listServer(t)
	t0 := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	now := t0.Add(20 * 24 * time.Hour)
	s.now = func() time.Time { return now }
	s.ext.AgentsIdleAfter = 14 * 24 * time.Hour
	fm := s.ext.Mirror.(*fakeMirror)
	mainID, chiefID := "nav-mirror-b0a314c33976", "nav-mirror-dd10d8b1345a"

	ids, count, all := idleRowIDs(getOwed(t, s, h, ui.NavAgentsPath).Body.String())
	want := []string{"nav-agent-5", mainID, "nav-agent-7", chiefID}
	if strings.Join(ids, ",") != strings.Join(want, ",") || count != "4" {
		t.Errorf("idle rows %v with count %q, want %v with count 4 (pinned agent 6 never idle)", ids, count, want)
	}
	// CONTROL: the pinned agent is drawn, above the group.
	if len(all) != 5 || all[0] != "nav-agent-6" {
		t.Errorf("CONTROL: the sidebar rows are %v; want five, the pinned agent 6 first and outside the group", all)
	}

	// main:@1 active a minute ago: out of the group on the next refresh.
	fm.mu.Lock()
	fm.windows[0].Activity = now.Add(-time.Minute).Unix()
	fm.mu.Unlock()
	ids, count, all = idleRowIDs(getOwed(t, s, h, ui.NavAgentsPath).Body.String())
	if strings.Join(ids, ",") != "nav-agent-5,nav-agent-7,"+chiefID || count != "3" {
		t.Errorf("after main:@1 became active: idle rows %v with count %q, want [nav-agent-5 nav-agent-7 %s] with 3", ids, count, chiefID)
	}
	if len(all) < 1 || all[0] != mainID {
		t.Errorf("the reactivated window is not the sidebar's first row (the most recently active): %v", all)
	}

	// Boundary: chief:@2 (t0+2h) idle EXACTLY 14 days is not idle; a second more is.
	chiefAt := time.Unix(fm.windows[1].Activity, 0)
	now = chiefAt.Add(14 * 24 * time.Hour)
	if ids, _, _ := idleRowIDs(getOwed(t, s, h, ui.NavAgentsPath).Body.String()); strings.Contains(strings.Join(ids, ","), chiefID) {
		t.Errorf("a window idle exactly the threshold is in the group: %v", ids)
	}
	now = now.Add(time.Second)
	if ids, _, _ := idleRowIDs(getOwed(t, s, h, ui.NavAgentsPath).Body.String()); !strings.Contains(strings.Join(ids, ","), chiefID) {
		t.Errorf("a window idle the threshold and a second is not in the group: %v", ids)
	}

	// Off.
	s.ext.AgentsIdleAfter = 0
	if ids, count, all := idleRowIDs(getOwed(t, s, h, ui.NavAgentsPath).Body.String()); len(ids) != 0 || count != "" || len(all) != 5 {
		t.Errorf("threshold 0 drew a group: %v %q (rows %v)", ids, count, all)
	}
}

// TestAClaudeWindowsIdleIsItsLastMessage: a Claude window whose tmux activity
// is a minute old (a tmux restart) but whose host reports its last message 30
// days ago is in the Idle group, ordered by the last message — one number for
// both. Without a reported last message (an older host) the same window is
// working, by tmux's time.
func TestAClaudeWindowsIdleIsItsLastMessage(t *testing.T) {
	s, h, _ := listServer(t)
	now := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.ext.AgentsIdleAfter = 14 * 24 * time.Hour
	fm := s.ext.Mirror.(*fakeMirror)
	last := now.Add(-30*24*time.Hour - 7*time.Minute)
	fm.mu.Lock()
	fm.windows[0].Claude = true
	fm.windows[0].Activity = now.Add(-time.Minute).Unix()
	fm.windows[0].LastMessage = last.Unix()
	fm.mu.Unlock()
	mainID := "nav-mirror-b0a314c33976"
	// Ordered by the last message (30 days) it is the NEWEST idle row — the
	// rest are months old; by tmux's minute it would not be idle at all.
	if ids, _, _ := idleRowIDs(getOwed(t, s, h, ui.NavAgentsPath).Body.String()); len(ids) == 0 || ids[0] != mainID {
		t.Errorf("a Claude window silent for 30 days (tmux: a minute) is not the newest idle row: %v", ids)
	}
	// An older host: no last message, tmux's minute — working.
	fm.mu.Lock()
	fm.windows[0].LastMessage = 0
	fm.mu.Unlock()
	if ids, _, _ := idleRowIDs(getOwed(t, s, h, ui.NavAgentsPath).Body.String()); strings.Contains(strings.Join(ids, ","), mainID) {
		t.Errorf("without a last message the window is idle by something else: %v", ids)
	}
}
