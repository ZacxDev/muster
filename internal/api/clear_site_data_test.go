package api

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
)

// clearSiteCachesCallers are the responses that send Clear-Site-Data: "cache"
// (clearSiteCaches): the two sign-outs, on their own response, and the sign-in
// pages' background request. A SIGN-IN is not among them: the browser holds
// the redirect until the clear finishes (TestNoSignInWaitsOnClearingTheCaches).
var clearSiteCachesCallers = []string{
	"handleClearCaches",  // the sign-in pages' background clear (clearCachesHTML)
	"handleGoTrueLogout", // GoTrue sign-out
	"handleLogout",       // password sign-out
}

// TestEverySignInAndSignOutClearsTheCaches: structural — exactly these
// functions call clearSiteCaches, failing when the set grows or shrinks — and
// behavioural, through the handler chain: the password sign-out sends the
// header (GoTrue's is TestTheGoTrueSignOutClearsTheCaches), every page a
// sign-in starts from starts the background clear, and that request answers
// with the header.
func TestEverySignInAndSignOutClearsTheCaches(t *testing.T) {
	got := callersOf(t, ".", "clearSiteCaches")
	if len(got) == 0 {
		t.Fatal("the source scan found no caller at all — it is not observing this package")
	}
	if strings.Join(got, ",") != strings.Join(clearSiteCachesCallers, ",") {
		t.Errorf("clearSiteCaches is called from %v; the ledger names %v.\n"+
			"A sign-out clears on its response; a sign-in must not (its page clears in the background).", got, clearSiteCachesCallers)
	}

	_, h := pwaServer(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin") // muster's own form (the central origin check)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Clear-Site-Data") != `"cache"` {
		t.Errorf("password sign-out = %d, Clear-Site-Data %q; want \"cache\"", rec.Code, rec.Header().Get("Clear-Site-Data"))
	}

	// Every page a sign-in starts from carries the background clear: both
	// modes' sign-in pages, the claim, an invite and a reset.
	const clearScript = `fetch('/auth/clear-caches',{method:'POST',keepalive:true,credentials:'same-origin'})`
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), clearScript) {
		t.Errorf("password mode GET /login = %d, and its page does not start the background clear", rec.Code)
	}
	f := newGoTrueFixture(t)
	f.s.gt.Accounts = &memAccounts{rows: f.store} // registers the invite and reset pages
	for _, path := range []string{"/login", ClaimPath, InvitePath, ResetPath} {
		w := do(f.edge, "GET", path, nil, asNavigation)
		if !strings.Contains(w.Body.String(), clearScript) {
			t.Errorf("gotrue mode GET %s = %d, and its page does not start the background clear", path, w.Code)
		}
	}

	// The request itself, in both modes: same-origin is answered 204 with the
	// header and no-store; cross-site and origin-less ones are refused without
	// it (the negative control: the header is not on every answer).
	for _, mode := range []struct {
		name string
		h    http.Handler
	}{{"password", h}, {"gotrue", f.edge}} {
		w := do(mode.h, "POST", "/auth/clear-caches", nil)
		if w.Code != http.StatusNoContent || w.Header().Get("Clear-Site-Data") != `"cache"` || w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: same-origin POST /auth/clear-caches = %d, Clear-Site-Data %q, Cache-Control %q; want 204, \"cache\", no-store",
				mode.name, w.Code, w.Header().Get("Clear-Site-Data"), w.Header().Get("Cache-Control"))
		}
		for _, o := range []string{"https://elsewhere.example", ""} {
			w := do(mode.h, "POST", "/auth/clear-caches", nil, func(r *http.Request) {
				if o == "" {
					r.Header.Del("Origin")
				} else {
					r.Header.Set("Origin", o)
				}
			})
			if w.Code != http.StatusForbidden || w.Header().Get("Clear-Site-Data") != "" {
				t.Errorf("%s: POST /auth/clear-caches with Origin %q = %d, Clear-Site-Data %q; want 403 and none",
					mode.name, o, w.Code, w.Header().Get("Clear-Site-Data"))
			}
		}
	}
}

// TestNoSignInWaitsOnClearingTheCaches: no response that STARTS a session
// carries Clear-Site-Data. A browser holds a redirect that carries it until
// the clear has finished, and Chromium's clear walks the whole disk cache,
// every site's: measured between the 303 arriving and the next request, 1–2
// ms with an empty cache, 350–500 ms with 20,000 entries and 10.7 s for the
// first clear after the browser started — the sign-in page frozen on screen
// all the while. Behavioural for every sign-in through the handler chain: the
// password and GoTrue forms, a passkey, an invite's new account, a reset and
// the admin claim — each with its success checked, so a refused sign-in
// cannot pass for one that sent no header.
func TestNoSignInWaitsOnClearingTheCaches(t *testing.T) {
	check := func(what string, w *httptest.ResponseRecorder, wantCode int) {
		t.Helper()
		if w.Code != wantCode {
			t.Fatalf("%s = %d, want %d (the sign-in must succeed for this to mean anything): %.300s", what, w.Code, wantCode, w.Body.String())
		}
		if got := w.Header().Get("Clear-Site-Data"); got != "" {
			t.Errorf("%s sent Clear-Site-Data %q: the browser waits for the whole clear before it leaves the sign-in page", what, got)
		}
	}

	_, h := pwaServer(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(url.Values{"password": {testUIPassword}, "next": {"/"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	check("password sign-in", rec, http.StatusSeeOther)
	if setCookies(rec)[sessionCookieName] == nil {
		t.Error("password sign-in set no session cookie")
	}

	f := newGoTrueFixture(t)
	f.member("m@example.com", 7, authz.RoleMember)
	w := do(f.edge, "POST", "/login", url.Values{"email": {"m@example.com"}, "password": {"correct horse battery"}})
	check("GoTrue sign-in", w, http.StatusSeeOther)
	if setCookies(w)[accessCookieName] == nil {
		t.Error("GoTrue sign-in set no access cookie")
	}

	c := newGoTrueFixture(t)
	w = do(c.edge, "POST", ClaimPath, claimForm(mintClaim(t, c.store), "op@example.com", "a-strong-password", "a-strong-password"))
	check("the admin claim's sign-in", w, http.StatusSeeOther)
	if setCookies(w)[accessCookieName] == nil {
		t.Error("the claim set no access cookie")
	}

	pw := newPasskeyWorld(t)
	w = do(pw.edge, "POST", InvitePath, redeemForm(pw.createInvite(""), "pk-csd@invite.test", "", goodPW, goodPW))
	check("an invite's sign-in", w, http.StatusSeeOther)
	if setCookies(w)[accessCookieName] == nil {
		t.Error("the invite set no access cookie")
	}
	var sub string
	if err := pw.pool.QueryRow(authz.System(context.Background()), `SELECT gotrue_sub::text FROM users WHERE email = $1`, "pk-csd@invite.test").Scan(&sub); err != nil {
		t.Fatal(err)
	}
	pw.fake.AddPasskey(sub, "cred-csd")
	w = pw.passkeySignIn("cred-csd", gotruetest.FlagUPUV)
	check("a passkey sign-in", w, http.StatusOK)
	if setCookies(w)[accessCookieName] == nil {
		t.Error("the passkey sign-in set no access cookie")
	}
	w = do(pw.edge, "POST", ResetPath, pw.resetForm(sub, "a-new-long-password", false))
	check("a reset's sign-in", w, http.StatusSeeOther)
	if setCookies(w)[accessCookieName] == nil {
		t.Error("the reset set no access cookie")
	}
}

// TestTheClearCachesPathMatchesItsRoute: the route literal in
// registerLoginRoutes is the constant the pages' script posts to.
func TestTheClearCachesPathMatchesItsRoute(t *testing.T) {
	if clearCachesPath != "/auth/clear-caches" {
		t.Fatalf("clearCachesPath = %q; the route registered is POST /auth/clear-caches", clearCachesPath)
	}
	src, err := os.ReadFile("login.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `mux.HandleFunc("POST /auth/clear-caches", s.handleClearCaches)`) {
		t.Fatal("registerLoginRoutes no longer registers POST /auth/clear-caches → handleClearCaches")
	}
}

// callersOf is every function in dir's non-test files that calls fn, sorted.
func callersOf(t *testing.T, dir, fn string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	set := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if id, ok := c.Fun.(*ast.Ident); ok && id.Name == fn {
						set[fd.Name.Name] = true
					}
				}
				return true
			})
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestTheGoTrueSignOutClearsTheCaches: GoTrue's sign-out, through the handler
// chain, sends Clear-Site-Data: "cache" — this browser's and everywhere's.
func TestTheGoTrueSignOutClearsTheCaches(t *testing.T) {
	f := newGoTrueFixture(t)
	f.member("m@example.com", 7, authz.RoleMember)
	for _, scope := range []string{"", "global"} {
		in := do(f.edge, "POST", "/login", url.Values{"email": {"m@example.com"}, "password": {"correct horse battery"}})
		ck := setCookies(in)
		if in.Code != http.StatusSeeOther || ck[accessCookieName] == nil || ck[refreshCookieName] == nil {
			t.Fatalf("fixture: sign-in = %d", in.Code)
		}
		form := url.Values{}
		if scope != "" {
			form.Set("scope", scope)
		}
		w := do(f.edge, "POST", "/logout", form, withCookies(ck[accessCookieName].Value, ck[refreshCookieName].Value))
		if w.Code != http.StatusSeeOther {
			t.Fatalf("sign-out (scope %q) = %d", scope, w.Code)
		}
		if got := w.Header().Get("Clear-Site-Data"); got != `"cache"` {
			t.Errorf("GoTrue sign-out (scope %q): Clear-Site-Data = %q, want \"cache\"", scope, got)
		}
	}
}
