package api

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
)

// The central origin check for cookie-authenticated writes (cross_origin.go,
// core PR 1g), driven over EVERY write route a browser session admits, through
// each route's real registered wrapper chain. The principal probe
// (newPrincipalTestServer) panics at admit's hand-off, so "reached the
// handler" is observed, not inferred, and no handler body runs.
//
// 🔴 THE NEGATIVE SWEEP IS RED AT THE BASE THIS PR STARTS FROM: before the
// check, POST /tasks, /tasks/merge, /agents/{id}/start and /chief/provision
// (and every other route not wrapped in requireSameOrigin) handed a sibling
// page's request, carrying the owner's cookie, to the handler.

// testSiblingOrigin is a sibling host of the test requests' Host
// (httptest.NewRequest's example.com): same-site, cross-origin — the
// meetings subdomain's shape.
const testSiblingOrigin = "http://meet.example.com"

// isWriteForTest is the test's own reading of "a write": every method but
// GET, HEAD and OPTIONS, and a METHOD-LESS pattern (which serves every
// method). Written here, not borrowed from the implementation.
func isWriteForTest(pattern string) bool {
	i := strings.IndexByte(pattern, ' ')
	if i < 0 {
		return true
	}
	switch pattern[:i] {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// writeRequestFor builds a request matching pattern; a method-less pattern is
// driven as a POST.
func writeRequestFor(pattern string) *http.Request {
	method, path := concretePath(pattern)
	if !strings.Contains(pattern, " ") {
		method = http.MethodPost
	}
	return httptest.NewRequest(method, path, strings.NewReader(""))
}

// cookieWriteRoutes are the write routes a browser session admits: every
// write in routeVias[ViaSession] (which TestEveryRouteIsClassifiedForItsPrincipal
// pins to routes.golden both ways, and TestEveryAuthenticatedRouteAttachesAPrincipal
// drives behaviourally).
func cookieWriteRoutes(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, p := range routeVias[authz.ViaSession] {
		if isWriteForTest(p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	// Instrument check: the four routes the design names as unguarded today
	// must be in the walk, or the walk is not the one that matters.
	for _, must := range []string{"POST /tasks", "POST /tasks/merge", "POST /agents/{id}/start", "POST /chief/provision"} {
		found := false
		for _, p := range out {
			found = found || p == must
		}
		if !found {
			t.Fatalf("instrument check: %s is not among the cookie write routes", must)
		}
	}
	if len(out) < 150 {
		t.Fatalf("instrument check: only %d cookie write routes", len(out))
	}
	return out
}

// bearerOnlyWriteRoutes are the write routes no browser session admits:
// writes behind any other door, and in no ViaSession list.
func bearerOnlyWriteRoutes(t *testing.T) map[string]authz.Via {
	t.Helper()
	cookie := map[string]bool{}
	for _, p := range routeVias[authz.ViaSession] {
		cookie[p] = true
	}
	out := map[string]authz.Via{}
	for via, ps := range routeVias {
		if via == authz.ViaSession {
			continue
		}
		for _, p := range ps {
			if isWriteForTest(p) && !cookie[p] {
				out[p] = via
			}
		}
	}
	if len(out) < 60 {
		t.Fatalf("instrument check: only %d bearer-only write routes", len(out))
	}
	return out
}

// headerCase is one set of the three headers the check reads.
type headerCase struct {
	name                  string
	site, origin, referer string
	userAgent             string
}

func (c headerCase) apply(r *http.Request) {
	for k, v := range map[string]string{"Sec-Fetch-Site": c.site, "Origin": c.origin, "Referer": c.referer, "User-Agent": c.userAgent} {
		if v != "" {
			r.Header.Set(k, v)
		}
	}
}

// foreignCases are what a page that is not muster's sends (or what no
// browser page sends at all). Every one must be refused before the handler.
// Each isolates ONE branch of the rule, so a mutant that loosens one branch
// turns exactly its case red.
func foreignCases(own string) []headerCase {
	return []headerCase{
		// A sibling subdomain's page, as browsers send it (spike S7).
		{name: "sibling page (Sec-Fetch-Site same-site)", site: "same-site", origin: testSiblingOrigin, referer: testSiblingOrigin + "/"},
		// same-site with muster's OWN Origin: not something a browser sends,
		// but it isolates the Sec-Fetch-Site branch — a rule that ignored the
		// header and read Origin would admit it.
		{name: "Sec-Fetch-Site same-site, own Origin", site: "same-site", origin: own},
		{name: "Sec-Fetch-Site cross-site", site: "cross-site", origin: "http://attacker.example.net"},
		// none, with muster's own Origin: the standard library admits "none";
		// this check does not.
		{name: "Sec-Fetch-Site none, own Origin", site: "none", origin: own},
		// An older browser with no Fetch Metadata, on a sibling page.
		{name: "no Sec-Fetch-Site, sibling Origin", origin: testSiblingOrigin},
		{name: "no Sec-Fetch-Site, Origin null", origin: "null"},
		{name: "no Sec-Fetch-Site or Origin, sibling Referer", referer: testSiblingOrigin + "/page"},
		{name: "none of the three headers"},
	}
}

// ownCases are muster's own pages. Every one must reach the handler.
func ownCases(own string) []headerCase {
	return []headerCase{
		// 🔴 muster's own native form under Referrer-Policy: no-referrer
		// (measured, spike S7): Origin null, Sec-Fetch-Site same-origin. Red if
		// the rule reads Origin before Sec-Fetch-Site.
		{name: "native form (Sec-Fetch-Site same-origin, Origin null)", site: "same-origin", origin: "null"},
		{name: "htmx / fetch (Sec-Fetch-Site same-origin, own Origin)", site: "same-origin", origin: own},
		{name: "no Sec-Fetch-Site, own Origin", origin: own},
		{name: "only a same-origin Referer", referer: own + "/tasks"},
		// ⚠ UNVERIFIED: the Android muster app's WebView. By documentation it is
		// Chromium and sends Sec-Fetch-Site; the headers here are that claim,
		// not a capture from a device (spike S7's operator steps capture it).
		{name: "Android WebView (UNVERIFIED headers)", site: "same-origin", origin: own,
			userAgent: "Mozilla/5.0 (Linux; Android 15; Pixel 9 Build/AP4A; wv) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/131.0.0.0 Mobile Safari/537.36"},
	}
}

func TestEveryCookieWriteRefusesAForeignRequest(t *testing.T) {
	s, h, seen := newPrincipalTestServer(t)
	routes := cookieWriteRoutes(t)
	for _, pattern := range routes {
		for _, c := range foreignCases("http://example.com") {
			t.Run(pattern+"/"+c.name, func(t *testing.T) {
				*seen = nil
				req := writeRequestFor(pattern)
				req.Header.Set("Cookie", sessionCookieFor(s))
				c.apply(req)
				rec, aborted := serveRec(t, h, req)
				code := rec.Code
				if aborted || len(*seen) != 0 {
					t.Fatalf("a %s request with the session cookie reached the handler as %v", c.name, *seen)
				}
				if code != http.StatusForbidden || !strings.Contains(rec.Body.String(), CrossOriginRefusal) {
					t.Fatalf("status %d body %q; want 403 with %q", code, rec.Body.String(), CrossOriginRefusal)
				}
			})
		}
	}
}

func TestEveryCookieWriteAdmitsMusterOwnPages(t *testing.T) {
	s, h, seen := newPrincipalTestServer(t)
	for _, pattern := range cookieWriteRoutes(t) {
		for _, c := range ownCases("http://example.com") {
			t.Run(pattern+"/"+c.name, func(t *testing.T) {
				*seen = nil
				req := writeRequestFor(pattern)
				req.Header.Set("Cookie", sessionCookieFor(s))
				c.apply(req)
				aborted, code := serve(t, h, req)
				if !aborted || len(*seen) != 1 || (*seen)[0].Via != authz.ViaSession {
					t.Fatalf("muster's own %s did not reach the handler (status %d, saw %v)", c.name, code, *seen)
				}
			})
		}
	}
}

// TestBearerOnlyWritesAreNotOpenedByTheCookie: a route the check exempts
// because no cookie admits it must really admit no cookie. Driven with the
// session cookie alone, from a sibling page AND from muster's own page; if a
// route that IS cookie-admitted were filed as bearer-only, the own-page case
// would reach its handler here.
func TestBearerOnlyWritesAreNotOpenedByTheCookie(t *testing.T) {
	s, h, seen := newPrincipalTestServer(t)
	routes := bearerOnlyWriteRoutes(t)
	patterns := make([]string, 0, len(routes))
	for p := range routes {
		patterns = append(patterns, p)
	}
	sort.Strings(patterns)
	sawMethodless := false
	for _, pattern := range patterns {
		sawMethodless = sawMethodless || !strings.Contains(pattern, " ")
		for _, c := range []headerCase{
			{name: "sibling page", site: "same-site", origin: testSiblingOrigin},
			{name: "own page", site: "same-origin", origin: "http://example.com"},
		} {
			t.Run(pattern+"/"+c.name, func(t *testing.T) {
				*seen = nil
				req := writeRequestFor(pattern)
				req.Header.Set("Cookie", sessionCookieFor(s))
				c.apply(req)
				if aborted, _ := serve(t, h, req); aborted || len(*seen) != 0 {
					t.Fatalf("a cookie-only request reached a bearer-only route's handler as %v", *seen)
				}
			})
		}
	}
	if !sawMethodless {
		t.Fatal("instrument check: no method-less pattern (/connect/...) among the bearer-only writes")
	}
}

// TestBearerWritesIgnoreTheOriginHeaders: every write behind a bearer door,
// with that door's credential and the most hostile headers, reaches its
// handler as that door. A cross-site page cannot make a browser attach these.
func TestBearerWritesIgnoreTheOriginHeaders(t *testing.T) {
	s, h, seen := newPrincipalTestServer(t)
	checked := 0
	for via, ps := range routeVias {
		if via == authz.ViaSession {
			continue
		}
		for _, pattern := range ps {
			if !isWriteForTest(pattern) {
				continue
			}
			t.Run(string(via)+" "+pattern, func(t *testing.T) {
				*seen = nil
				req := writeRequestFor(pattern)
				credential(s, req, via)
				req.Header.Set("Sec-Fetch-Site", "cross-site")
				req.Header.Set("Origin", "http://attacker.example.net")
				if aborted, code := serve(t, h, req); !aborted || len(*seen) != 1 || (*seen)[0].Via != via {
					t.Fatalf("a %s request was not admitted (status %d, saw %v)", via, code, *seen)
				}
			})
			checked++
		}
	}
	if checked < 60 {
		t.Fatalf("instrument check: drove only %d bearer write routes", checked)
	}
}

// TestACookieAndABearerAreJudgedByTheDoorThatAdmitted: a request carrying
// both is checked as whichever credential its route's door read.
func TestACookieAndABearerAreJudgedByTheDoorThatAdmitted(t *testing.T) {
	s, h, seen := newPrincipalTestServer(t)
	hostile := func(r *http.Request) {
		r.Header.Set("Cookie", sessionCookieFor(s))
		r.Header.Set("Authorization", "Bearer "+pAPIToken)
		r.Header.Set("Sec-Fetch-Site", "same-site")
		r.Header.Set("Origin", testSiblingOrigin)
	}
	// A personal-token route: the token admitted it, so it is not checked.
	*seen = nil
	req := writeRequestFor("POST /api/tasks")
	hostile(req)
	if aborted, code := serve(t, h, req); !aborted || len(*seen) != 1 || (*seen)[0].Via != authz.ViaToken {
		t.Fatalf("POST /api/tasks with a token and a cookie: status %d, saw %v; want admitted by the token", code, *seen)
	}
	// A session route: the cookie admitted it (the bearer is not read there),
	// so it is checked and refused.
	*seen = nil
	req = writeRequestFor("POST /tasks")
	hostile(req)
	if aborted, code := serve(t, h, req); aborted || len(*seen) != 0 || code != http.StatusForbidden {
		t.Fatalf("POST /tasks with a cookie and a token from a sibling: status %d, saw %v; want 403", code, *seen)
	}
}

// TestAGoTrueSessionWriteIsOriginChecked: the GoTrue mode's session door
// (requireGoTrueSession) is a cookie door too, on the browser listener.
func TestAGoTrueSessionWriteIsOriginChecked(t *testing.T) {
	f := newGoTrueFixture(t)
	sub := f.member("m@example.com", 7, authz.RoleMember)
	at, rt, _ := f.fake.Mint(sub)
	for _, target := range []string{"/tasks", "/tasks/merge", "/account/suggest"} {
		for _, c := range []headerCase{
			{name: "sibling page", site: "same-site", origin: testSiblingOrigin},
			{name: "Origin null, no Sec-Fetch-Site", origin: "null"},
			{name: "none of the three headers"},
		} {
			t.Run(target+"/"+c.name, func(t *testing.T) {
				f.mu.Lock()
				f.seen = nil
				f.mu.Unlock()
				w := do(f.edge, http.MethodPost, target, nil, withCookies(at, rt), func(r *http.Request) {
					r.Header.Del("Origin")
					c.apply(r)
				})
				if ps := f.principals(); len(ps) != 0 || w.Code != http.StatusForbidden {
					t.Fatalf("GoTrue session, %s: status %d, principals %v; want 403 before the handler", c.name, w.Code, ps)
				}
			})
		}
		t.Run(target+"/native form", func(t *testing.T) {
			f.mu.Lock()
			f.seen = nil
			f.mu.Unlock()
			do(f.edge, http.MethodPost, target, nil, withCookies(at, rt), func(r *http.Request) {
				r.Header.Set("Origin", "null")
				r.Header.Set("Sec-Fetch-Site", "same-origin")
			})
			if ps := f.principals(); len(ps) != 1 || ps[0].Via != authz.ViaSession || ps[0].UserID != 7 {
				t.Fatalf("GoTrue session, muster's own native form: principals %v; want user 7 via session", ps)
			}
		})
	}
}
