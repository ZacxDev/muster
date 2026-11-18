package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/pins"
	"github.com/ZacxDev/muster/internal/recents"
	"github.com/ZacxDev/muster/internal/ui"
)

// GET /agents — THE RETIRED AGENTS LIST PAGE'S URL — IS HOME'S REDIRECT
// (nav_agents.go handleAgentsHome). Every link to the list that is out in the
// world (a bookmark, a PWA shortcut on a phone, a `?filter=` link the list
// once wrote, a notification) lands where home does; with nothing to open it
// draws the onboarding page instead of redirecting, so `/` → /agents cannot
// loop; a failed read says so.
//
// REGRESSION TEST: red at dd25e646, where GET /agents answered 200 with the
// list's shell document in every case.
func TestTheAgentsURLRedirectsHome(t *testing.T) {
	withChief := &homeAgents{rows: homeRows(true), latest: map[int64]int64{homeChiefID: homeChiefSess, homeWorkerID: 55}}
	noChief := &homeAgents{rows: homeRows(false), latest: map[int64]int64{homeWorkerID: 55}}
	for _, c := range []struct {
		name, path string
		store      *homeAgents
		want       string
	}{
		{"a chief", "/agents", withChief, chiefHref},
		{"a chief, an old filter link", "/agents?filter=host%3Aws&q=x", withChief, chiefHref},
		{"no chief: the sidebar's first row", "/agents", noChief, ui.AgentListHref("brave-otter", 55)},
		{"no chief, an old filter link", "/agents?filter=idle-1h", noChief, ui.AgentListHref("brave-otter", 55)},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, h := homeServer(t, c.store)
			rec := homeGet(s, h, c.path, nil)
			if rec.Code != http.StatusFound || rec.Header().Get("Location") != c.want {
				t.Fatalf("GET %s = %d → %q, want 302 → %q", c.path, rec.Code, rec.Header().Get("Location"), c.want)
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", cc)
			}
			hx := homeGet(s, h, c.path, map[string]string{"HX-Request": "true"})
			if hx.Code != http.StatusOK || hx.Header().Get("HX-Redirect") != c.want || hx.Body.Len() != 0 {
				t.Errorf("htmx GET %s = %d, HX-Redirect %q, %d body bytes; want 200, %q, none", c.path, hx.Code, hx.Header().Get("HX-Redirect"), hx.Body.Len(), c.want)
			}
		})
	}

	// Nothing to open: the onboarding page itself, inside the navigation —
	// not a redirect (which `/` would follow straight back here).
	for name, store := range map[string]*homeAgents{"no agents": {}, "no agent store": nil} {
		var s *Server
		var h http.Handler
		if store == nil {
			s, h = homeServer(t, nil)
		} else {
			s, h = homeServer(t, store)
		}
		rec := homeGet(s, h, "/agents?filter=x", nil)
		body := rec.Body.String()
		if rec.Code != http.StatusOK || !strings.Contains(body, `id="`+ui.HomeEmptyID+`"`) || !strings.Contains(body, `id="sidebar-root"`) {
			t.Errorf("%s: GET /agents = %d, want 200 with the onboarding page in the nav:\n%.400s", name, rec.Code, body)
		}
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Errorf("%s: the onboarding page redirects to %q", name, loc)
		}
		// And `/` goes there.
		if root := homeGet(s, h, "/", nil); root.Code != http.StatusFound || root.Header().Get("Location") != ui.HomeFallbackPath {
			t.Errorf("%s: GET / = %d → %q, want 302 → %s", name, root.Code, root.Header().Get("Location"), ui.HomeFallbackPath)
		}
	}

	// A failed read is said, never drawn as "No agents yet".
	s, h := homeServer(t, &homeAgents{rows: homeRows(true), listErr: errors.New("db down")})
	rec := homeGet(s, h, "/agents", nil)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), ui.HomeEmptyID) {
		t.Errorf("a failed read: GET /agents = %d %q, want 500 without the empty state", rec.Code, rec.Body.String())
	}
}

// TestHomeAfterADeleteLeavesTheDeletedAgentOut: the agent a DELETE is tearing
// down outlives the response by a moment, so home computed for the deleting
// page skips it (homeTarget's except) — the page must not land back on the
// agent it just deleted, even when that agent is the sidebar's first row.
func TestHomeAfterADeleteLeavesTheDeletedAgentOut(t *testing.T) {
	store := &homeAgents{rows: homeRows(true), latest: map[int64]int64{homeChiefID: homeChiefSess, homeWorkerID: 55}}
	// The chief is labelled " Chief " (primaryChief): deleting the worker
	// still goes to the chief; excluding the chief (it cannot be deleted, but
	// the rule is the rule) goes to the worker.
	s, _ := homeServer(t, store)
	r := asOperator(httptest.NewRequest(http.MethodGet, "/", nil))
	if got, err := s.homeTarget(r, homeExcept{agent: homeWorkerID}); err != nil || got != chiefHref {
		t.Errorf("home except the worker = %q, %v; want the chief %q", got, err, chiefHref)
	}
	if got, err := s.homeTarget(r, homeExcept{agent: homeChiefID}); err != nil || got != ui.AgentListHref("brave-otter", 55) {
		t.Errorf("home except the chief = %q, %v; want the worker", got, err)
	}
	noChief := &homeAgents{rows: homeRows(false), latest: map[int64]int64{homeWorkerID: 55}}
	s, _ = homeServer(t, noChief)
	r = asOperator(httptest.NewRequest(http.MethodGet, "/", nil))
	if got, err := s.homeTarget(r, homeExcept{}); err != nil || got != ui.AgentListHref("brave-otter", 55) {
		t.Fatalf("CONTROL: home = %q, %v; want the worker", got, err)
	}
	if got, err := s.homeTarget(r, homeExcept{agent: homeWorkerID}); err != nil || got != "" {
		t.Errorf("home except the only agent = %q, %v; want nothing (the onboarding page)", got, err)
	}
}

// asOperator is r as the signed-in operator would reach a handler (the
// principal the session middleware attaches).
func asOperator(r *http.Request) *http.Request {
	return r.WithContext(authz.WithPrincipal(r.Context(), authz.Principal{UserID: authz.OperatorUserID, Role: authz.RoleAdmin, Via: authz.ViaSession}))
}

// TestHomeLeavingAnAgentDoesNotOpenIt: `/?leave=<name>` (ui.HomeLeavingPath —
// where Mark complete sends the agent page) is home with that agent left out.
// With no chief, home is the session opened last, which is the page being
// left: bare `/` landed straight back on it.
//
// REGRESSION TEST for the retirement's own loop (e2e agents-list-ux's Mark
// complete cases found it): red before `leave` existed.
func TestHomeLeavingAnAgentDoesNotOpenIt(t *testing.T) {
	two := &homeAgents{
		rows:   append(homeRows(false), agents.Agent{ID: 31, Name: "calm-heron", DisplayName: "other", Status: agents.StatusRunning, OwnerID: authz.OperatorUserID}),
		latest: map[int64]int64{homeWorkerID: 55, 31: 77},
		// The worker is the more recently active: the sidebar's first row.
		last: map[int64]time.Time{homeWorkerID: time.Date(2000, 1, 2, 0, 0, 0, 0, time.UTC), 31: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	s, h := homeServer(t, two)
	// CONTROL: bare home opens the worker (the sidebar's first row here).
	if rec := homeGet(s, h, "/", nil); rec.Header().Get("Location") != ui.AgentListHref("brave-otter", 55) {
		t.Fatalf("CONTROL: GET / → %q, want the worker", rec.Header().Get("Location"))
	}
	if got, want := ui.HomeLeavingPath("brave-otter"), "/?leave=brave-otter"; got != want {
		t.Errorf("HomeLeavingPath = %q, want %q", got, want)
	}
	rec := homeGet(s, h, ui.HomeLeavingPath("brave-otter"), nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != ui.AgentListHref("calm-heron", 77) {
		t.Errorf("GET /?leave=brave-otter = %d → %q, want the other agent", rec.Code, rec.Header().Get("Location"))
	}
	// An unknown name leaves nothing out.
	if rec := homeGet(s, h, "/?leave=no-such-agent", nil); rec.Header().Get("Location") != ui.AgentListHref("brave-otter", 55) {
		t.Errorf("GET /?leave=no-such-agent → %q, want the worker", rec.Header().Get("Location"))
	}
}

// TestHomeLeavingAMirroredWindowDoesNotOpenIt: a mirrored window's Rendered
// view and terminal send their Home arrow to ui.HomeLeavingMirrorPath. Opening
// the view recorded a visit, so with no chief bare home IS that window; the
// leaving home opens another session. And when leaving leaves nothing, the
// onboarding fallback (/agents) carries the leave along and must not send the
// viewer back either.
//
// REGRESSION TEST (round-1 audit): red before the mirror half of `leave`.
func TestHomeLeavingAMirroredWindowDoesNotOpenIt(t *testing.T) {
	s, h, _ := listServer(t)
	mem := s.ext.Recents.(*memRecents)
	key := pins.MirrorKey{Host: "ws", Session: "main", Window: "@1"}
	mem.put(recents.Entry{Mirror: key})
	ref := ui.MirrorRef{Host: key.Host, Session: key.Session, Window: key.Window}
	window := ui.MirrorCardHref(ui.MirrorCardView{Host: key.Host, Session: key.Session, Window: key.Window})
	// CONTROL: bare home opens the window just visited.
	if rec := homeGet(s, h, "/", nil); rec.Header().Get("Location") != window {
		t.Fatalf("CONTROL: GET / → %q, want the visited window %q", rec.Header().Get("Location"), window)
	}
	leave := ui.HomeLeavingMirrorPath(ref)
	if want := "/?leave_host=ws&leave_session=main&leave_window=%401"; leave != want {
		t.Errorf("HomeLeavingMirrorPath = %q, want %q", leave, want)
	}
	rec := homeGet(s, h, leave, nil)
	if to := rec.Header().Get("Location"); rec.Code != http.StatusFound || to == window || to == "" || to == ui.HomeFallbackPath {
		t.Errorf("GET %s = %d → %q, want another session (agent-5, the next visit)", leave, rec.Code, to)
	} else if to != ui.AgentListHref("idle-newest", 0) && !strings.HasPrefix(to, "/agents/") {
		t.Errorf("GET %s → %q, want an agent's page", leave, to)
	}
}

// TestTheFallbackKeepsTheLeave: with nothing left once the session being left
// is left out, `/` falls back to /agents CARRYING the leave, and /agents draws
// the onboarding page instead of sending the viewer back to that session.
// (The delete path's own fallback is HomeLeavingPath of the deleted agent.)
func TestTheFallbackKeepsTheLeave(t *testing.T) {
	only := &homeAgents{rows: homeRows(false), latest: map[int64]int64{homeWorkerID: 55}}
	s, h := homeServer(t, only)
	rec := homeGet(s, h, ui.HomeLeavingPath("brave-otter"), nil)
	if to := rec.Header().Get("Location"); to != ui.HomeFallbackPath+"?leave=brave-otter" {
		t.Fatalf("GET /?leave=brave-otter → %q, want /agents carrying the leave", to)
	}
	rec = homeGet(s, h, ui.HomeFallbackPath+"?leave=brave-otter", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="`+ui.HomeEmptyID+`"`) {
		t.Errorf("GET /agents?leave=brave-otter = %d → %q, want the onboarding page", rec.Code, rec.Header().Get("Location"))
	}
	// CONTROL: without the leave, /agents opens the agent.
	if rec := homeGet(s, h, ui.HomeFallbackPath, nil); rec.Code != http.StatusFound {
		t.Errorf("CONTROL: GET /agents = %d, want a redirect to the agent", rec.Code)
	}
}

// TestADeleteWithNowhereToGoStillLeavesTheDeletedAgent: when home without the
// deleted agent finds nothing (or its read failed), the DELETE's redirect is
// home LEAVING that agent — never bare /agents, which would compute home again
// with the row still there.
func TestADeleteWithNowhereToGoStillLeavesTheDeletedAgent(t *testing.T) {
	if got := homeAfterDelete("", "brave-otter"); got != "/?leave=brave-otter" {
		t.Errorf("nowhere found: %q, want /?leave=brave-otter", got)
	}
	if got := homeAfterDelete("/agents/calm-heron?session=7", "brave-otter"); got != "/agents/calm-heron?session=7" {
		t.Errorf("a home found: %q, want it unchanged", got)
	}
}
