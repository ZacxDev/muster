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
	"renderAsksPage",        // /attention (the Asks page)
	"renderPluginsPage",     // /plugins
	"renderFlowsPage",       // /flows
	"writeAdminDocument",    // /admin/users, /admin/invites
	"renderNotice",          // a notice, when its reader is signed in (it is in apiBareWriters too)
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
	"renderNotice": "a one-message notice, when its reader has NO browser session — a refused or expired " +
		"link, a sign-in that is not configured, a sign-out that did not finish (its session is already revoked): " +
		"every nav link would answer with the sign-in form. A signed-in reader's notice has the nav " +
		"(TestANoticeHasTheNavOnlyForASignedInReader)",
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
		// The nav opens no stream: /attention holds exactly the one its own
		// list script opens, every other page none — as before the nav.
		if n, want := streamCount(body), map[bool]int{true: 1, false: 0}[p.path == "/attention"]; n != want {
			t.Errorf("GET %s opens %d /events stream(s), want %d", p.path, n, want)
		}
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

// TestTheFrameableViewsDrawTheNavFramedOrNot: the views the view swap opens in
// its frame draw their nav whether the browser loads them as the page or into
// the frame (Sec-Fetch-Dest says which) — the frame covers the whole page, so
// its own nav is the only one reachable while it is on screen — and open no
// /events stream for it.
func TestTheFrameableViewsDrawTheNavFramedOrNot(t *testing.T) {
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
			if roots, opens := navCount(body); roots != 1 || opens != 1 {
				t.Errorf("%s loaded as %s draws %d side nav(s) and %d hamburger(s), want one of each", c.name, dest, roots, opens)
			}
			if n := streamCount(body); n != 0 {
				t.Errorf("%s loaded as %s opens %d /events stream(s); it opened none before the side nav", c.name, dest, n)
			}
		}
	}
}

// streamCount counts the /events streams a response's markup opens: an htmx
// sse-connect attribute, or a script constructing an EventSource on /events.
// Both are spelled here as the CODE spells them (attribute with its value, the
// call with its argument), which a comment describing them does not.
func streamCount(body string) int {
	return strings.Count(body, `sse-connect="/events"`) + strings.Count(body, `new EventSource('/events')`)
}

// TestANoticeHasTheNavOnlyForASignedInReader: renderNotice draws the side nav
// for a reader with a browser session (a member refused an admin page, a
// passkeys failure) and none for a signed-out one (a link opened at the wrong
// address) — decided by the request's principal, not by the caller.
func TestANoticeHasTheNavOnlyForASignedInReader(t *testing.T) {
	f := newGoTrueFixtureWith(t, nil)
	f.s.gt.Accounts = &memAccounts{rows: f.store}
	_ = f.member("admin@example.com", authz.OperatorUserID, authz.RoleAdmin)
	msub := f.member("member@example.com", 7, authz.RoleMember)
	at, rt, _ := f.fake.Mint(msub)

	signedIn := []struct{ what, path, says string }{
		{"a member opening an admin page", "/admin/users", "Admins only"},
		{"passkeys switched off in GoTrue", "/account/passkeys", "Passkeys are not available"},
	}
	for _, c := range signedIn {
		w := do(f.edge, "GET", c.path, nil, withCookies(at, rt), asNavigation)
		body := w.Body.String()
		if !strings.Contains(body, c.says) {
			t.Fatalf("%s: GET %s = %d, not the notice %q: %.300s", c.what, c.path, w.Code, c.says, body)
		}
		if roots, opens := navCount(body); roots != 1 || opens != 1 {
			t.Errorf("%s (signed in): the notice draws %d side nav(s) and %d hamburger(s), want one of each", c.what, roots, opens)
		}
		assertDocHeaders(t, c.what, w.Header())
	}

	// Signed out: a claim link opened on the machine port is refused with a
	// notice, and the reader has no session.
	w := do(f.machine, "GET", ClaimPath, nil, asNavigation)
	body := w.Body.String()
	if !strings.Contains(body, "Open this link at the public address") {
		t.Fatalf("GET %s on the machine port = %d, not the notice: %.300s", ClaimPath, w.Code, body)
	}
	if roots, opens := navCount(body); roots != 0 || opens != 0 {
		t.Errorf("the signed-out notice draws %d side nav(s) — it is bare", roots)
	}
	assertDocHeaders(t, "the signed-out notice", w.Header())
}
