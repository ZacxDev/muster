package api

import (
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
)

// clearSiteCachesCallers are the responses that start or end a session, each
// of which sends Clear-Site-Data: "cache" (clearSiteCaches).
var clearSiteCachesCallers = []string{
	"handleClaimSubmit",         // the admin claim's sign-in
	"handleGoTrueLoginSubmit",   // GoTrue sign-in
	"handleGoTrueLogout",        // GoTrue sign-out
	"handleLoginSubmit",         // password sign-in
	"handleLogout",              // password sign-out
	"handlePasskeySignInVerify", // a passkey sign-in
	"signInAfterCutoff",         // the sign-in after a password reset
	"signInNewLogin",            // the sign-in an invite's new account makes
}

// TestEverySignInAndSignOutClearsTheCaches: structural — exactly these
// functions call clearSiteCaches, failing when the set grows or shrinks — and
// behavioural, through the handler chain, for the password sign-in and both
// modes' sign-in (GoTrue's sign-out is TestTheGoTrueSignOutClearsTheCaches).
func TestEverySignInAndSignOutClearsTheCaches(t *testing.T) {
	got := callersOf(t, ".", "clearSiteCaches")
	if len(got) == 0 {
		t.Fatal("the source scan found no caller at all — it is not observing this package")
	}
	if strings.Join(got, ",") != strings.Join(clearSiteCachesCallers, ",") {
		t.Errorf("clearSiteCaches is called from %v; the ledger names %v.\n"+
			"A response that starts or ends a session must clear the caches; a new one goes in the ledger.", got, clearSiteCachesCallers)
	}

	// Password mode: a sign-in clears; a refused one does not (the negative
	// control: the header is the sign-in's, not every POST /login's).
	_, h := pwaServer(t, nil)
	post := func(form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if w := post(url.Values{"password": {"not the password"}}); w.Header().Get("Clear-Site-Data") != "" {
		t.Errorf("a refused sign-in (%d) sent Clear-Site-Data %q", w.Code, w.Header().Get("Clear-Site-Data"))
	}
	if w := post(url.Values{"password": {testUIPassword}, "next": {"/"}}); w.Code != http.StatusSeeOther || w.Header().Get("Clear-Site-Data") != `"cache"` {
		t.Errorf("password sign-in = %d, Clear-Site-Data %q; want 303 and \"cache\"", w.Code, w.Header().Get("Clear-Site-Data"))
	}
	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Clear-Site-Data") != `"cache"` {
		t.Errorf("password sign-out = %d, Clear-Site-Data %q; want \"cache\"", rec.Code, rec.Header().Get("Clear-Site-Data"))
	}

	// GoTrue mode: a sign-in clears.
	f := newGoTrueFixture(t)
	f.member("m@example.com", 7, authz.RoleMember)
	w := do(f.edge, "POST", "/login", url.Values{"email": {"m@example.com"}, "password": {"correct horse battery"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Clear-Site-Data") != `"cache"` {
		t.Errorf("GoTrue sign-in = %d, Clear-Site-Data %q; want 303 and \"cache\"", w.Code, w.Header().Get("Clear-Site-Data"))
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
