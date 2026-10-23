package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE SIDE NAV ON EVERY SIGNED-IN PAGE — the api half.
//
// internal/api writes some documents itself, as strings: through
// writeAuthDocument (BARE — no navigation) or writeNavDocument (inside
// ui.NavDocument, the app's navigation). The ui package's ledger
// (TestEveryDocumentIsNavOrNamedBare) cannot see these; this one does.

// apiNavWriters are the functions that write a signed-in page through
// writeNavDocument — each draws the side nav.
var apiNavWriters = []string{
	"handleAccountPage",     // /account
	"renderAccountTokens",   // /account/tokens
	"renderCredentials",     // /account/credentials
	"renderAccountPasskeys", // /account/passkeys
	"renderAttentionPage",   // /attention
	"renderPluginsPage",     // /plugins
	"renderFlowsPage",       // /flows
	"writeAdminDocument",    // /admin/users, /admin/invites
}

// apiBareWriters are the functions that write through writeAuthDocument — no
// navigation — and why each is bare.
var apiBareWriters = map[string]string{
	"renderLoginPage":   "the password sign-in form: shown to someone who is not signed in",
	"renderGoTrueLogin": "the GoTrue sign-in form: shown to someone who is not signed in",
	"renderClaimForm":   "the admin-claim form: its reader has no account yet",
	"handleInvitePage":  "the invite page: its reader has no account yet",
	"renderInviteForm":  "the create-your-account form an invite opens: no account yet",
	"renderResetForm":   "the set-a-new-password form a reset link opens: its reader cannot sign in",
	"renderNotice": "a one-message notice — a refused or expired link, a failed sign-in, \"admins only\". " +
		"Most are shown to someone signed out or mid-way through signing in, where a nav would offer pages " +
		"that answer with the sign-in form; the few a signed-in person can see are dead ends with the " +
		"browser's Back, kept bare rather than split by caller",
}

// TestEveryAPIDocumentIsNavOrNamedBare reads this package's source: every
// function calling writeNavDocument must be in apiNavWriters, every function
// calling writeAuthDocument must be in apiBareWriters — failing when either set
// grows or shrinks — and no other function may spell a document itself.
func TestEveryAPIDocumentIsNavOrNamedBare(t *testing.T) {
	nav, bare, doctype := apiDocumentWriters(t, ".")
	// POSITIVE CONTROL: a scan wired to nothing would report two empty sets.
	if len(nav) < 2 || len(bare) < 2 {
		t.Fatalf("the source scan found %d nav and %d bare writers — it is not observing this package", len(nav), len(bare))
	}
	want := append([]string(nil), apiNavWriters...)
	sort.Strings(want)
	if strings.Join(nav, ",") != strings.Join(want, ",") {
		t.Errorf("writeNavDocument is called from %v; the ledger names %v.\n"+
			"A signed-in page added or removed without updating apiNavWriters.", nav, want)
	}
	var wantBare []string
	for fn := range apiBareWriters {
		wantBare = append(wantBare, fn)
	}
	sort.Strings(wantBare)
	if strings.Join(bare, ",") != strings.Join(wantBare, ",") {
		t.Errorf("writeAuthDocument (no side nav) is called from %v; apiBareWriters names %v.\n"+
			"🔴 A page behind requireSession written this way has no way back to the rest of the app: write it "+
			"with writeNavDocument. A genuinely signed-out page goes in apiBareWriters with its reason.", bare, wantBare)
	}
	if strings.Join(doctype, ",") != "writeAuthDocument" {
		t.Errorf("a <!doctype> is spelled in %v; only writeAuthDocument may write a document by hand — every "+
			"other one goes through it or through internal/ui, where its ledger sees it", doctype)
	}
}

// apiDocumentWriters scans dir's non-test Go files and returns, sorted, the
// functions that call writeNavDocument, those that call writeAuthDocument, and
// those whose body holds a "<!doctype" string literal.
func apiDocumentWriters(t *testing.T, dir string) (nav, bare, doctype []string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	navSet, bareSet, docSet := map[string]bool{}, map[string]bool{}, map[string]bool{}
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
				switch x := n.(type) {
				case *ast.CallExpr:
					switch fn := x.Fun.(type) {
					case *ast.Ident:
						if fn.Name == "writeAuthDocument" {
							bareSet[fd.Name.Name] = true
						}
					case *ast.SelectorExpr:
						if fn.Sel.Name == "writeNavDocument" {
							navSet[fd.Name.Name] = true
						}
					}
				case *ast.BasicLit:
					if x.Kind == token.STRING && strings.Contains(strings.ToLower(x.Value), "<!doctype") {
						docSet[fd.Name.Name] = true
					}
				}
				return true
			})
		}
	}
	keys := func(m map[string]bool) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	return keys(navSet), keys(bareSet), keys(docSet)
}

// navCount is how many side navs (and hamburgers) a response body draws.
func navCount(body string) (roots, opens int) {
	return strings.Count(body, `id="sidebar-root"`), strings.Count(body, `id="sidebar-open"`)
}

// assertDocHeaders: both document writers send no-store and same-origin
// referrers — the split must not have dropped them from either half.
func assertDocHeaders(t *testing.T, what string, h http.Header) {
	t.Helper()
	if got := h.Get("Cache-Control"); got != "no-store" {
		t.Errorf("%s: Cache-Control = %q, want no-store", what, got)
	}
	if got := h.Get("Referrer-Policy"); got != "same-origin" {
		t.Errorf("%s: Referrer-Policy = %q, want same-origin (the page's forms are refused without it)", what, got)
	}
}

// TestEverySignedInPageDrawsTheSideNav requests each signed-in server-authored
// page as a signed-in admin and reads the nav off the response — and each bare
// page signed out, which must draw none.
func TestEverySignedInPageDrawsTheSideNav(t *testing.T) {
	f := newGoTrueFixtureWith(t, &Extensions{
		Attention:   emptyAttention{},
		Plugins:     &countingPlugins{},
		Credentials: &memCreds{},
		// No API-token or flows store: those two pages then render their
		// "this server has no database" state — still the page, which is all
		// this asks about.
	})
	f.fake.EnablePasskeys()
	f.s.gt.Accounts = &memAccounts{rows: f.store}
	sub := f.member("admin@example.com", authz.OperatorUserID, authz.RoleAdmin)
	at, rt, _ := f.fake.Mint(sub)

	// Each page, and the nav entry it must mark as current (the link's id), or
	// "" for a page no entry names.
	pages := []struct{ path, current string }{
		{"/attention", "sidebar-attention"},
		{"/account", "sidebar-account"},
		{"/account/tokens", "sidebar-account"},
		{ui.CredentialsPath, "sidebar-account"},
		{"/account/passkeys", "sidebar-account"},
		{"/plugins", ""},
		{"/flows", ""},
		{"/admin/users", "sidebar-admin"},
		{"/admin/invites", "sidebar-admin"},
	}
	for _, p := range pages {
		w := do(f.edge, "GET", p.path, nil, withCookies(at, rt), asNavigation)
		body := w.Body.String()
		if w.Code != http.StatusOK || !strings.Contains(body, "<!doctype") && !strings.Contains(body, "<!DOCTYPE") {
			t.Errorf("GET %s = %d, not a document: %.300s", p.path, w.Code, body)
			continue
		}
		if roots, opens := navCount(body); roots != 1 || opens != 1 {
			t.Errorf("GET %s draws %d side nav(s) and %d hamburger(s), want one of each", p.path, roots, opens)
			continue
		}
		assertDocHeaders(t, "GET "+p.path, w.Header())
		nav := body[strings.Index(body, `id="sidebar"`):]
		nav = nav[:strings.Index(nav, "</nav>")]
		if n := strings.Count(nav, `aria-current="page"`); n != map[bool]int{true: 1, false: 0}[p.current != ""] {
			t.Errorf("GET %s: %d nav links carry aria-current, want %v", p.path, n, p.current)
		}
		if p.current != "" {
			i := strings.Index(nav, `id="`+p.current+`"`)
			if i < 0 {
				t.Errorf("GET %s: the nav has no #%s", p.path, p.current)
				continue
			}
			tag := nav[strings.LastIndex(nav[:i], "<"):]
			if tag = tag[:strings.Index(tag, ">")]; !strings.Contains(tag, `aria-current="page"`) {
				t.Errorf("GET %s: #%s is not the current entry: %s", p.path, p.current, tag)
			}
		}
	}

	// BARE: the pages a signed-out caller is shown. No nav — and still the
	// no-store / same-origin headers.
	for _, path := range []string{"/login", ClaimPath, InvitePath, ResetPath} {
		w := do(f.edge, "GET", path, nil, asNavigation)
		body := w.Body.String()
		if !strings.Contains(body, "<!doctype") {
			t.Errorf("GET %s signed out = %d, not a document: %.300s", path, w.Code, body)
			continue
		}
		if roots, opens := navCount(body); roots != 0 || opens != 0 {
			t.Errorf("GET %s (signed out) draws %d side nav(s) — it is a bare page", path, roots)
		}
		assertDocHeaders(t, "GET "+path, w.Header())
	}
}

// TestTheViewSwapFrameIsServedWithoutTheNav: the four frameable views draw the
// nav when loaded as a page, and NONE when the browser loads them into the
// view-swap frame (Sec-Fetch-Dest: iframe) — and say they vary on it.
func TestTheViewSwapFrameIsServedWithoutTheNav(t *testing.T) {
	ms, mts := viewServer(t, &fakeMirror{online: true, v2: &fakeMirrorV2{}})
	tsrv, tts := terminalServer(t, newFakeTerminal())
	cases := []struct {
		name string
		s    *Server
		base string
		path string
	}{
		{"the mirrored Rendered view", ms, mts.URL, ui.MirrorViewPath("ws", "main")},
		{"the mirrored terminal", ms, mts.URL, ui.MirrorTerminalPath("ws", "main")},
		{"an agent's terminal", tsrv, tts.URL, ui.AgentTerminalPath("keen-wren")},
	}
	for _, c := range cases {
		for _, dest := range []string{"document", "iframe"} {
			req, _ := http.NewRequest(http.MethodGet, c.base+c.path, nil)
			req.Header.Set("Cookie", sessionCookieFor(c.s))
			req.Header.Set("Sec-Fetch-Dest", dest)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body := string(raw)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s (%s) = %d: %.300s", c.name, dest, resp.StatusCode, body)
			}
			want := map[string]int{"document": 1, "iframe": 0}[dest]
			if roots, opens := navCount(body); roots != want || opens != want {
				t.Errorf("%s loaded as %s draws %d side nav(s) and %d hamburger(s), want %d", c.name, dest, roots, opens, want)
			}
			if !strings.Contains(strings.Join(resp.Header.Values("Vary"), ","), "Sec-Fetch-Dest") {
				t.Errorf("%s: the response does not say it varies on Sec-Fetch-Dest (Vary: %v)", c.name, resp.Header.Values("Vary"))
			}
		}
	}
}
