package api

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/web"
)

// THE PUBLIC PAGES LISTENER'S SURFACE (pages_public.go). Every path and
// token here is invented.

// pagesPublicLedger is, written out by hand, every REGISTERED route the public
// pages listener passes on. 🔴 It is not derived from PagesPublicRoutes: it is
// what that list is checked against. Growing the public surface means editing
// both, on purpose, in review.
var pagesPublicLedger = []string{
	"GET /brand/icon-192.png",
	"GET /brand/logo.png",
	"GET /s/{token}",
	"GET /s/{token}/assets/{asset}",
	"GET /s/{token}/content",
	"POST /s/{token}/feedback",
	"POST /s/{token}/submit",
	"POST /s/{token}/upload",
}

// pagesPublicStaticLedger is every path under a registered subtree ("GET
// /static/") the listener passes on: the shell's icon on an unbranded server.
var pagesPublicStaticLedger = []string{"/static/icons/icon-192.png"}

var routeWildcard = regexp.MustCompile(`\{[^}]*\}`)

// concreteRequest is a request that the registered pattern pat matches: its
// method (GET when the pattern names none), each wildcard filled in, a subtree
// pattern given one segment more.
func concreteRequest(pat string) *http.Request {
	method, path := http.MethodGet, pat
	if m, p, ok := strings.Cut(pat, " "); ok {
		method, path = m, p
	}
	path = routeWildcard.ReplaceAllString(strings.TrimSuffix(path, "{$}"), "w0rd")
	if strings.HasSuffix(path, "/") && path != "/" {
		path += "w0rd"
	}
	return httptest.NewRequest(method, path, nil)
}

// reachesInner reports whether PagesPublicSurface passed r on to the handler
// behind it.
func reachesInner(r *http.Request) (bool, *httptest.ResponseRecorder) {
	reached := false
	h := PagesPublicSurface(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusTeapot)
	}), "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return reached, w
}

// TestThePublicPagesListenerServesOnlyItsAllowlist walks every route muster
// registers (RegisterRoutes, the one registration site) and sends a request
// each pattern matches to the public listener's surface: exactly the
// ledgered ones reach muster, and every other one is the listener's 404. The
// set that gets through must EQUAL the ledger, so a route added to
// PagesPublicRoutes fails it, and so does a ledgered route muster no longer
// registers (or the list no longer names).
func TestThePublicPagesListenerServesOnlyItsAllowlist(t *testing.T) {
	routes := recordRoutes(t)
	// Positive control: the walk sees muster's routes, the UI root and the
	// API among them, so a zero below is not a walk over nothing.
	have := map[string]bool{}
	for _, p := range routes {
		have[p] = true
	}
	for _, must := range []string{"GET /{$}", "GET /health", "GET /api/whoami", "GET /pages", "GET /static/", "POST /mcp"} {
		if !have[must] {
			t.Fatalf("instrument check: RegisterRoutes recorded %d route(s) without %q", len(routes), must)
		}
	}
	var through []string
	for _, p := range routes {
		ok, w := reachesInner(concreteRequest(p))
		if ok {
			through = append(through, p)
			continue
		}
		if w.Code != http.StatusNotFound || w.Body.String() != pagesPublicNotFound || len(w.Header().Values("Set-Cookie")) != 0 {
			t.Errorf("%s: refused with %d %q, want the listener's one 404", p, w.Code, w.Body.String())
		}
	}
	sort.Strings(through)
	want := append([]string(nil), pagesPublicLedger...)
	sort.Strings(want)
	if strings.Join(through, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the public pages listener passes on these registered routes:\n  %s\nbut its ledger is:\n  %s\n"+
			"A route was added to (or dropped from) PagesPublicRoutes, or muster stopped registering one: the public "+
			"surface changed. Update pagesPublicLedger only if that is meant",
			strings.Join(through, "\n  "), strings.Join(want, "\n  "))
	}
	// Every PagesPublicRoutes entry is accounted for: a registered route in the
	// ledger, or a ledgered static file that ships.
	ledgered := map[string]bool{}
	for _, p := range pagesPublicLedger {
		ledgered[p] = true
	}
	for _, p := range pagesPublicStaticLedger {
		ledgered["GET "+p] = true
	}
	for _, p := range PagesPublicRoutes {
		if !ledgered[p] {
			t.Errorf("PagesPublicRoutes names %q, which no ledger holds", p)
		}
	}
	if len(PagesPublicRoutes) != len(pagesPublicLedger)+len(pagesPublicStaticLedger) {
		t.Errorf("PagesPublicRoutes has %d entries, the ledgers %d", len(PagesPublicRoutes), len(pagesPublicLedger)+len(pagesPublicStaticLedger))
	}
}

// TestThePublicPagesListenerServesOnlyTheShellsOwnStaticFiles: of everything
// under /static/, only the icon the shell links passes, and it ships; the
// app's own stylesheet and scripts do not.
func TestThePublicPagesListenerServesOnlyTheShellsOwnStaticFiles(t *testing.T) {
	icon, _, _ := strings.Cut(ui.Brand{}.Icon192(), "?")
	if icon != pagesPublicStaticLedger[0] {
		t.Fatalf("an unbranded shell links its icon at %q, the ledger names %q", icon, pagesPublicStaticLedger[0])
	}
	for _, p := range pagesPublicStaticLedger {
		if _, err := fs.Stat(web.Files, strings.TrimPrefix(p, "/")); err != nil {
			t.Errorf("%s is ledgered but does not ship: %v", p, err)
		}
		if ok, _ := reachesInner(httptest.NewRequest(http.MethodGet, p+"?v=0123456789ab", nil)); !ok {
			t.Errorf("GET %s is refused", p)
		}
	}
	var others int
	_ = fs.WalkDir(web.Files, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || "/"+p == pagesPublicStaticLedger[0] {
			return err
		}
		others++
		if ok, _ := reachesInner(httptest.NewRequest(http.MethodGet, "/"+p, nil)); ok {
			t.Errorf("GET /%s passes the public pages listener", p)
		}
		return nil
	})
	if others < 10 {
		t.Fatalf("instrument check: only %d other static file(s) walked", others)
	}
}

// TestThePublicPagesListenerRefusesEveryOtherShape: a method a ledgered path
// does not take, a path with a segment more or less, and a path that is not
// canonical are all the one 404, never a 405 or a redirect that would say
// what exists.
func TestThePublicPagesListenerRefusesEveryOtherShape(t *testing.T) {
	for _, c := range []struct{ method, path string }{
		{http.MethodDelete, "/s/w0rd"},
		{http.MethodPost, "/s/w0rd"},
		{http.MethodGet, "/s/w0rd/submit"},
		{http.MethodPut, "/s/w0rd/upload"},
		{http.MethodGet, "/s/"},
		{http.MethodGet, "/s/w0rd/"},
		{http.MethodGet, "/s/w0rd/content/more"},
		{http.MethodGet, "/s//w0rd"},
		{http.MethodGet, "/s/w0rd/../../api/whoami"},
		{http.MethodGet, "/s/w0rd/./content"},
		{http.MethodGet, "/brand/icon-512.png"},
		{http.MethodGet, "/static/app.css"},
		{http.MethodGet, "/"},
		{http.MethodGet, "/metrics"},
		{http.MethodGet, "/health"},
		{http.MethodPost, "/mcp"},
		{http.MethodGet, "/plugin/v1/w0rd"},
		{http.MethodGet, "/login"},
		{http.MethodOptions, "/s/w0rd/submit"},
	} {
		r := httptest.NewRequest(c.method, "/", nil)
		r.URL.Path = c.path
		ok, w := reachesInner(r)
		if ok || w.Code != http.StatusNotFound || w.Header().Get("Location") != "" || w.Header().Get("Allow") != "" {
			t.Errorf("%s %s: passed=%v %d (Location %q, Allow %q), want the one 404", c.method, c.path, ok, w.Code,
				w.Header().Get("Location"), w.Header().Get("Allow"))
		}
	}
	// Positive control: the same surface does pass a ledgered shape.
	if ok, _ := reachesInner(httptest.NewRequest(http.MethodHead, "/s/w0rd", nil)); !ok {
		t.Fatal("HEAD /s/w0rd is refused: the surface refuses everything")
	}
}
