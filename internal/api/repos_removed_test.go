package api

import (
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/auth"
)

// ---------------------------------------------------------------------------
// THE REPOS TAB AND THE GITHUB OAUTH CONNECTION ARE GONE.
//
// The GitHub App (Extensions.GitHubApp, the `github` connector) is muster's
// GitHub identity; the per-user OAuth connection it superseded, and the Repos
// tab that showed it, were removed. These tests drive the REAL mux, so they see
// the route set and the shell a deployment serves — not a renderer in isolation.
// ---------------------------------------------------------------------------

// operatorRequest is a request carrying the human session this fixture's
// password issues, so what a route answers is its OWN behaviour and not the
// session gate's 401.
func operatorRequest(s *Server, method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.AddCookie(&http.Cookie{
		Name:  sessionCookieName,
		Value: auth.SignSession(s.auth.uiSessionSecret(), time.Now(), sessionTTL),
	})
	return r
}

// TestTheReposSurfaceAndTheOAuthRoutesAreGone: with a GitHub App configured —
// the shape that used to draw the Repos tab whatever the OAuth store held — the
// shell draws no Repos panel or link and links to no OAuth flow, and every route
// the tab and the flow served answers 404 (the shell is registered on exact
// paths, so nothing catches them).
func TestTheReposSurfaceAndTheOAuthRoutesAreGone(t *testing.T) {
	s := New(nil, AuthConfig{UIPassword: testUIPassword}, log.New(os.Stderr, "", 0))
	s.UseExtensions(Extensions{GitHubApp: &listingApp{repos: []string{"example-org/widget"}}})
	h := s.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, operatorRequest(s, http.MethodGet, "/tasks"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /tasks answered %d, want 200; nothing below is a claim about the shell", rec.Code)
	}
	body := rec.Body.String()
	// The attributes the BROWSER keys on (the panel id appScript toggles, the
	// data-tab the router reads, the hrefs), not a label a copy change moves.
	for _, gone := range []string{`id="panel-repos"`, `data-tab="repos"`, `href="/repos"`, `/ui/repos`, `/github/connect`} {
		if strings.Contains(body, gone) {
			t.Errorf("the shell still carries %q", gone)
		}
	}
	// Positive control: the same probe sees the tabs that remain.
	if !strings.Contains(body, `data-tab="tasks"`) || !strings.Contains(body, `id="panel-tasks"`) {
		t.Fatal("control: the probe does not see the Tasks tab, so its silence about Repos proves nothing")
	}

	for _, rt := range []struct{ method, path string }{
		{http.MethodGet, "/repos"},
		{http.MethodGet, "/ui/repos"},
		{http.MethodGet, "/github/connect"},
		{http.MethodGet, "/github/callback?state=x&code=y"},
		{http.MethodPost, "/github/disconnect"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, operatorRequest(s, rt.method, rt.path))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s answered %d, want 404 (the route is removed)", rt.method, rt.path, rec.Code)
		}
	}
}

// TestADeepLinkToAHiddenTabStillRendersAShell: the tab routes stay registered
// whatever is wired, so /connectors is served on a build with no connector
// store and must land somewhere.
//
// 🔴 A SHELL WITH NO PANEL SHOWN IS A BLANK PAGE UNDER A CORRECT-LOOKING URL.
// The clamp sends it to the default tab, which is what an unrouted path already
// does. (This guarded /repos while that tab existed; /connectors is the hidden
// tab that remains.)
func TestADeepLinkToAHiddenTabStillRendersAShell(t *testing.T) {
	s := New(nil, AuthConfig{UIPassword: testUIPassword}, log.New(os.Stderr, "", 0))
	s.UseExtensions(Extensions{})
	if s.ext.Connectors != nil {
		t.Fatal("the fixture wired a connector store, so /connectors is not a hidden tab here")
	}
	h := s.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, operatorRequest(s, http.MethodGet, "/connectors"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /connectors with no connector store answered %d, want 200 — the route is "+
			"registered unconditionally, so it must serve something", rec.Code)
	}
	body := rec.Body.String()
	// The default tab's panel must be the VISIBLE one. `hidden` is the class
	// every inactive panel carries, so its absence on this panel is the state
	// "this is the tab being shown".
	i := strings.Index(body, `id="panel-`+defaultTabForTest+`"`)
	if i < 0 {
		t.Fatalf("GET /connectors rendered no #panel-%s at all", defaultTabForTest)
	}
	rest := body[i:]
	end := strings.Index(rest, ">")
	if end < 0 {
		t.Fatal("could not read the panel's opening tag")
	}
	if strings.Contains(rest[:end], "hidden") {
		t.Errorf("GET /connectors with no connector store left #panel-%s hidden, so every panel in "+
			"the document is hidden and the page is blank. The active tab must clamp to one "+
			"this build draws.\n    tag: %s", defaultTabForTest, rest[:end])
	}
	if strings.Contains(body, `id="panel-connectors"`) {
		t.Error("the shell drew the Connectors panel with no connector store behind it")
	}
}

// defaultTabForTest is the tab an unknown or undrawable tab clamps to. It is
// internal/ui's `defaultTab`, which is unexported; spelled here because the two
// packages are separate and this is the only thing this file needs from it.
const defaultTabForTest = "tasks"
