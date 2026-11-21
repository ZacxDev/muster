package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ZacxDev/muster/internal/metrics"
)

// 🔴 THE "unauthenticated" CLASS IS ONLY SAFE WHILE NO PUBLIC ROUTE READS THE
// SESSION. The central check runs in admit, and a public route never reaches
// admit: a public handler that read the browser's session cookie itself would
// act on a signed-in owner's behalf with no origin check at all (POST /logout
// did, until it applied the check itself).
//
// So the functions that read a browser session are a LEDGER: every function in
// this package that references one of them (a call or a method value) is
// listed with why that read is safe. It fails when the set grows or shrinks,
// so a new public handler calling hasValidSession is red until someone writes
// down why.
//
// ⚠ IT COVERS THE THREE SESSION READERS, NOT RAW COOKIE READS: a handler that
// read the cookie by name directly (r.Cookie(sessionCookieName)) would not be
// seen here. passwordSession and resolveGoTrueSession are the only such reads
// today (sessionReaders lists both).

var sessionReaderNames = map[string]bool{"hasValidSession": true, "passwordSession": true, "resolveGoTrueSession": true}

// looksSignedIn (signin_app.go) is in sessionReaders below: it reads the
// session cookies without resolving them, only to choose between a sign-in
// link and requireOperatorWrite, which then runs the central check.

var sessionReaders = map[string]string{
	"hasValidSession":       "the password-mode reader itself (passwordSession)",
	"requireSession":        "the session door: hands on through admit, which runs the central check",
	"requireGoTrueSession":  "the GoTrue session door: hands on through admit, which runs the central check",
	"handleLoginPage":       "GET /login: a redirect for a signed-in browser; GET changes nothing",
	"handleGoTrueLoginPage": "GET /login in GoTrue mode: a redirect (and a cookie refresh); GET changes nothing",
	"handleLogout":          "POST /logout: refuseCrossOriginWrite before the read",
	"looksSignedIn": "POST /auth/qr: isSameOrigin (appSignInRefusal) before the read, which only picks the sign-in link " +
		"or requireOperatorWrite; nothing is written on it",
	"handleGoTrueLogout": "POST /logout in GoTrue mode: isSameOrigin before the read",
	"passwordGate": "POST /login and the step-up confirm: a signed-in browser is exempt from the sign-in throttle. " +
		"⚠ POST /login is not origin-checked in password mode, so a sibling page can use that exemption",
}

func sessionReaderCallers(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if sel, ok := n.(*ast.SelectorExpr); ok && sessionReaderNames[sel.Sel.Name] {
						out[fn.Name.Name] = true
					}
					return true
				})
			}
		}
	}
	return out
}

func TestEverySessionReadIsLedgered(t *testing.T) {
	got := sessionReaderCallers(t)
	// Positive control: the scanner sees the doors it must see.
	for _, must := range []string{"requireSession", "requireGoTrueSession", "handleLogout"} {
		if !got[must] {
			t.Fatalf("instrument check: the scanner did not see %s read the session", must)
		}
	}
	var grew, shrank []string
	for fn := range got {
		if sessionReaders[fn] == "" {
			grew = append(grew, fn)
		}
	}
	for fn := range sessionReaders {
		if !got[fn] {
			shrank = append(shrank, fn)
		}
	}
	sort.Strings(grew)
	sort.Strings(shrank)
	if len(grew) > 0 {
		t.Errorf("functions that read a browser session and are not in sessionReaders (a public route that reads the "+
			"session bypasses the central origin check; say why each is safe):\n  %s", strings.Join(grew, "\n  "))
	}
	if len(shrank) > 0 {
		t.Errorf("sessionReaders lists functions that no longer read a session:\n  %s", strings.Join(shrank, "\n  "))
	}
}

// TestAPasswordSignOutFromASiblingIsRefused: POST /logout reads the session
// (and forgets the browser's push subscription with it), so a sibling page's
// post is refused before that, and muster's own form still signs out.
func TestAPasswordSignOutFromASiblingIsRefused(t *testing.T) {
	s, _, _ := newPrincipalTestServer(t)
	h := s.Handler()
	counter := metrics.CSRFRefusals.WithLabelValues("logout", csrfReasonFetchSite)
	before := testutil.ToFloat64(counter)

	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.Header.Set("Cookie", sessionCookieFor(s))
	req.Header.Set("Sec-Fetch-Site", "same-site")
	req.Header.Set("Origin", testSiblingOrigin)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || len(rec.Result().Cookies()) != 0 || !strings.Contains(rec.Body.String(), CrossOriginRefusal) {
		t.Fatalf("a sibling's POST /logout: %d, cookies %v, body %q; want 403 and nothing cleared", rec.Code, rec.Result().Cookies(), rec.Body)
	}
	if after := testutil.ToFloat64(counter); after != before+1 {
		t.Fatalf("muster_csrf_refusals_total{logout,sec-fetch-site} moved %v→%v; want +1", before, after)
	}

	// muster's own sign-out form (native, no-referrer: Origin null).
	req = httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.Header.Set("Cookie", sessionCookieFor(s))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", "null")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	cleared := false
	for _, c := range rec.Result().Cookies() {
		cleared = cleared || (c.Name == sessionCookieName && c.MaxAge < 0)
	}
	if rec.Code != http.StatusSeeOther || !cleared {
		t.Fatalf("muster's own sign-out: %d, cookies %v; want 303 and the session cleared", rec.Code, rec.Result().Cookies())
	}
}
