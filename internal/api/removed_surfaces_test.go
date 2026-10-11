package api

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// THE PRIVILEGE AND RUNBOOK SURFACES ARE GONE, AND THIS PINS IT BY BEHAVIOUR.
//
// Privileges were superseded by connectors and runbooks were unused, so both
// subsystems were removed whole: routes, stores, views, tools and tables. The
// route golden already records the pattern set; this test is the BEHAVIOURAL
// half, because a golden is a list of strings and says nothing about what a
// request to a removed path actually gets back. A catch-all, a shell route or a
// prefix pattern re-admitting one of these paths would leave the golden clean
// and answer 200.
//
// Each request carries a VALID SESSION (and, for the agent tier, a token), so
// a 404 here cannot be an auth refusal in disguise. The positive controls are
// still-registered routes asked the same way: they must NOT 404, or this test
// is observing a server that 404s everything.
// ---------------------------------------------------------------------------

// removedRoutes is every (method, concrete path) the removal took away. Paths
// carry concrete ids so a `{id}` pattern would match them if it still existed.
var removedRoutes = []struct{ method, path string }{
	// privileges: profiles registry, per-agent grants, request queue, picker
	{http.MethodGet, "/privileges"},
	{http.MethodGet, "/ui/privileges"},
	{http.MethodPost, "/privileges"},
	{http.MethodDelete, "/privileges/7"},
	{http.MethodGet, "/ui/privilege-requests"},
	{http.MethodPost, "/ui/privilege-requests/7/approve"},
	{http.MethodPost, "/ui/privilege-requests/7/deny"},
	{http.MethodGet, "/ui/agents/7/grants"},
	{http.MethodPost, "/agents/7/grants"},
	{http.MethodDelete, "/agents/7/grants/3"},
	{http.MethodGet, "/ui/agents/profiles"},
	{http.MethodPost, "/agent/privilege/request"},
	// runbooks: registry, tab shell, dispatch
	{http.MethodGet, "/runbooks"},
	{http.MethodGet, "/ui/runbooks"},
	{http.MethodPost, "/runbooks"},
	{http.MethodDelete, "/runbooks/7"},
	{http.MethodPost, "/runbooks/7/dispatch"},
}

func removedSurfaceServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	return s, s.Handler()
}

func serveAs(s *Server, h http.Handler, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.Header.Set("Cookie", sessionCookieFor(s))
	req.Header.Set("Authorization", "Bearer "+testHookToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRemovedPrivilegeAndRunbookRoutesAnswer404(t *testing.T) {
	s, h := removedSurfaceServer(t)

	// POSITIVE CONTROLS: the same request shape against routes that still exist
	// must not 404 — otherwise every assertion below passes over a server that
	// 404s everything (an unwired handler, a wrong prefix, a broken session).
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/tasks"},
		{http.MethodGet, "/agents"},
		{http.MethodGet, "/health"},
	} {
		if rec := serveAs(s, h, c.method, c.path); rec.Code == http.StatusNotFound {
			t.Fatalf("control %s %s answered 404 — the harness is not reaching registered routes, "+
				"so the 404s below would prove nothing", c.method, c.path)
		}
	}

	for _, c := range removedRoutes {
		rec := serveAs(s, h, c.method, c.path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s answered %d, want 404: the privilege/runbook surface is removed and "+
				"no route may answer for it", c.method, c.path, rec.Code)
		}
	}
}

// TestNoRegisteredPatternNamesARemovedSurface is the structural half: the
// recorded pattern set (the same one the golden pins) carries no pattern whose
// path mentions the removed vocabulary. It catches a re-added route under a new
// path the behavioural list above does not name.
func TestNoRegisteredPatternNamesARemovedSurface(t *testing.T) {
	got := recordRoutes(t)
	if len(got) == 0 {
		t.Fatal("no routes recorded; the scan below would pass over nothing")
	}
	for _, p := range got {
		for _, word := range []string{"privilege", "runbook", "/grants", "/profiles"} {
			if strings.Contains(p, word) {
				t.Errorf("registered pattern %q mentions %q — a removed surface is still routed", p, word)
			}
		}
	}
}
