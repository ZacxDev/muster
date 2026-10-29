package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
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
	if got, err := s.homeTarget(r, homeWorkerID); err != nil || got != chiefHref {
		t.Errorf("home except the worker = %q, %v; want the chief %q", got, err, chiefHref)
	}
	if got, err := s.homeTarget(r, homeChiefID); err != nil || got != ui.AgentListHref("brave-otter", 55) {
		t.Errorf("home except the chief = %q, %v; want the worker", got, err)
	}
	noChief := &homeAgents{rows: homeRows(false), latest: map[int64]int64{homeWorkerID: 55}}
	s, _ = homeServer(t, noChief)
	r = asOperator(httptest.NewRequest(http.MethodGet, "/", nil))
	if got, err := s.homeTarget(r, 0); err != nil || got != ui.AgentListHref("brave-otter", 55) {
		t.Fatalf("CONTROL: home = %q, %v; want the worker", got, err)
	}
	if got, err := s.homeTarget(r, homeWorkerID); err != nil || got != "" {
		t.Errorf("home except the only agent = %q, %v; want nothing (the onboarding page)", got, err)
	}
}

// asOperator is r as the signed-in operator would reach a handler (the
// principal the session middleware attaches).
func asOperator(r *http.Request) *http.Request {
	return r.WithContext(authz.WithPrincipal(r.Context(), authz.Principal{UserID: authz.OperatorUserID, Role: authz.RoleAdmin, Via: authz.ViaSession}))
}
