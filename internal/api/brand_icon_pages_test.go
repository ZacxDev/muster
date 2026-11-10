package api

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/branding"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/web"
)

// THE FAVICON ON EVERY DOCUMENT — the api half (internal/ui's
// TestEveryDocumentLinksTheBrandIcon is the other).
//
// 🔴 THE DEFECTS THESE PIN: (1) writeAuthDocument, the head of every sign-in,
// claim, invite, reset and notice page, wrote no rel=icon at all, so a
// signed-out visitor to a branded instance saw no favicon on /login; and (2)
// the four handlers that hand a Brand to an internal/ui form page could drop
// it with every test green — the ui tests pass the Brand themselves.

// apiTestBrandETag is an ETag no other string in a page can produce.
const apiTestBrandETag = "zq81-api-brand-etag"

// brandedIcon is the favicon href a branded page must carry.
var brandedIcon = BrandIcon192Path + "?v=" + apiTestBrandETag

// brandedStore is a branding store holding a logo (HasLogo is ETag != "").
func brandedStore() *memBranding {
	return &memBranding{b: branding.Branding{Name: "Branded Instance", ETag: apiTestBrandETag}}
}

var (
	apiLinkTag  = regexp.MustCompile(`<link [^>]*>`)
	apiRelAttr  = regexp.MustCompile(` rel="([^"]*)"`)
	apiHrefAttr = regexp.MustCompile(` href="([^"]*)"`)
)

// pageIconLinks is each rel's hrefs on page, in document order.
func pageIconLinks(page string) map[string][]string {
	out := map[string][]string{}
	for _, tag := range apiLinkTag.FindAllString(page, -1) {
		rel, href := apiRelAttr.FindStringSubmatch(tag), apiHrefAttr.FindStringSubmatch(tag)
		if rel == nil || href == nil {
			continue
		}
		out[rel[1]] = append(out[rel[1]], strings.ReplaceAll(href[1], "&amp;", "&"))
	}
	return out
}

// wantBrandIcon asserts page is a document carrying exactly one favicon and
// one touch icon, both at want, and exactly one manifest link.
func wantBrandIcon(t *testing.T, what, page, want string) {
	t.Helper()
	if !strings.Contains(strings.ToLower(page), "<!doctype html") {
		t.Errorf("%s: not a document: %.300s", what, page)
		return
	}
	links := pageIconLinks(page)
	for _, rel := range []string{"icon", "apple-touch-icon"} {
		if got := links[rel]; len(got) != 1 || got[0] != want {
			t.Errorf("%s: rel=%s links %v, want exactly [%s]", what, rel, got, want)
		}
	}
	if got := links["manifest"]; len(got) != 1 {
		t.Errorf("%s: %d rel=manifest links, want 1: %v", what, len(got), got)
	}
}

// TestTheSignInPageLinksTheBrandIcon: GET /login, signed out, on a
// password-mode server with a logo set, links the logo's icon as its favicon
// and touch icon — and muster's own with none set.
func TestTheSignInPageLinksTheBrandIcon(t *testing.T) {
	s, h, mem, _ := brandingServer(t)
	mem.b = brandedStore().b
	s.brandingStored(mem.b)
	wantBrandIcon(t, "GET /login (password mode, branded)", get(h, "/login", nil, true).Body.String(), brandedIcon)

	// CONTROL: with no logo the same page links muster's own icon, which ships.
	s.brandingStored(branding.Branding{})
	def := web.Asset("/static/icons/icon-192.png")
	if web.AssetHash("/static/icons/icon-192.png") == "" {
		t.Fatal("muster's icon is not in the embed")
	}
	page := get(h, "/login", nil, true).Body.String()
	wantBrandIcon(t, "GET /login (password mode, no logo)", page, def)
	if strings.Contains(page, "/brand/icon") {
		t.Error("GET /login with no logo links a brand icon")
	}
}

// TestEveryBarePageLinksTheBrandIcon: the pages writeAuthDocument writes,
// requested signed out on a branded GoTrue-mode server, link the logo's icon:
// the four bare forms TestEverySignedInPageDrawsTheSideNav requests signed out
// (sign-in, claim, invite, reset), and a signed-out notice (renderNotice) — a
// claim link opened on the machine port, as
// TestANoticeHasTheNavOnlyForASignedInReader reaches it.
func TestEveryBarePageLinksTheBrandIcon(t *testing.T) {
	f := newGoTrueFixtureWith(t, &Extensions{Branding: brandedStore()})
	// The invite and reset routes are registered with an accounts store.
	f.s.gt.Accounts = &memAccounts{rows: f.store}
	for _, path := range []string{"/login", ClaimPath, InvitePath, ResetPath} {
		w := do(f.edge, "GET", path, nil, asNavigation)
		wantBrandIcon(t, "GET "+path+" (gotrue mode, signed out, branded)", w.Body.String(), brandedIcon)
	}
	w := do(f.machine, "GET", ClaimPath, nil, asNavigation)
	if !strings.Contains(w.Body.String(), "Open this link at the public address") {
		t.Fatalf("GET %s on the machine port = %d, not the notice: %.300s", ClaimPath, w.Code, w.Body.String())
	}
	wantBrandIcon(t, "the signed-out notice", w.Body.String(), brandedIcon)
}

// TestEveryHandWrittenDocumentDrawsDocHead is the structural half: the
// functions in this package that spell a <!doctype> themselves are exactly
// handWrittenDocuments — failing when the set grows or shrinks — and each
// draws its head links through ui.DocHeadHTML, the string form of the one
// helper every internal/ui document head calls.
func TestEveryHandWrittenDocumentDrawsDocHead(t *testing.T) {
	handWrittenDocuments := []string{"writeAuthDocument"}
	_, _, doctype := apiDocumentWriters(t, ".")
	if strings.Join(doctype, ",") != strings.Join(handWrittenDocuments, ",") {
		t.Fatalf("a <!doctype> is spelled in %v; the ledger names %v", doctype, handWrittenDocuments)
	}
	callers := selectorCallers(t, ".", "ui", "DocHeadHTML")
	// POSITIVE CONTROL: the same scan finds ThemeHeadHTML in writeAuthDocument.
	if got := selectorCallers(t, ".", "ui", "ThemeHeadHTML"); !contains(got, "writeAuthDocument") {
		t.Fatalf("the scan found ui.ThemeHeadHTML called from %v — it is not observing this package", got)
	}
	for _, fn := range handWrittenDocuments {
		if !contains(callers, fn) {
			t.Errorf("%s writes a document without ui.DocHeadHTML (called from %v): its page has no favicon, "+
				"touch icon or manifest link", fn, callers)
		}
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// selectorCallers is the sorted names of the functions in dir's non-test files
// that call pkg.name.
func selectorCallers(t *testing.T, dir, pkg, name string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen := map[string]bool{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(x ast.Node) bool {
				if c, ok := x.(*ast.CallExpr); ok {
					if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
						if id, ok := sel.X.(*ast.Ident); ok && id.Name == pkg {
							seen[fd.Name.Name] = true
						}
					}
				}
				return true
			})
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// THE FORM PAGES' BRAND, THROUGH THEIR HANDLERS. Each case reaches one of the
// four sites that hand a Brand to an internal/ui form page and reads the
// favicon off the served page.

// TestTheMirrorConfirmPageLinksTheBrandIcon: a stale send the browser submits
// (no script) is answered with the plain confirm page (mirrorStepUpPage).
func TestTheMirrorConfirmPageLinksTheBrandIcon(t *testing.T) {
	s, ts := viewServer(t, &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}})
	s.ext.Branding = brandedStore()
	form := url.Values{"host": {"ws"}, "session": {"main"}, "text": {"hello"}, "nonce": {"0123456789abcdef0123456789abcdef"}}
	resp, page := navPost(t, ts.URL, ui.MirrorSendPath, staleCookieFor(s), form)
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, `id="stepup-form"`) {
		t.Fatalf("a stale no-script send = %d; want the confirm page: %.300s", resp.StatusCode, page)
	}
	wantBrandIcon(t, "the mirror confirm page", page, brandedIcon)
}

// TestTheSendAgainPageLinksTheBrandIcon: a no-script re-send of a message that
// may have landed is answered with the Send again page (sendAgainPage).
func TestTheSendAgainPageLinksTheBrandIcon(t *testing.T) {
	fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}, statusReport: true,
		windows: queueWindows()}
	s, ts := viewServer(t, fm)
	s.ext.Branding = brandedStore()
	cookie := sessionCookieFor(s)
	form := url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}, "text": {"once"}, "nonce": {"nonce-aaaaaaaaaaaaaaaa"}}
	fm.v2.pasteErr = fmt.Errorf("mirror: the host did not answer: %w", mirror.ErrPasteUnconfirmed)
	navPost(t, ts.URL, ui.MirrorSendPath, cookie, form)
	fm.v2.pasteErr = nil
	form.Set("nonce", "nonce-bbbbbbbbbbbbbbbb")
	resp, page := navPost(t, ts.URL, ui.MirrorSendPath, cookie, form)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(page, `id="send-again"`) {
		t.Fatalf("a no-script re-send of a held message = %d; want the Send again page: %.300s", resp.StatusCode, page)
	}
	wantBrandIcon(t, "the Send again page", page, brandedIcon)
}

// TestTheQueueConfirmPageLinksTheBrandIcon: a stale queue post the browser
// submits is answered with the plain confirm page (sendQueueStepUpPage).
func TestTheQueueConfirmPageLinksTheBrandIcon(t *testing.T) {
	s, ts := queueServer(t, queueMirror(), &memQueue{}, nil)
	s.ext.Branding = brandedStore()
	resp, page := navPost(t, ts.URL, ui.SendQueuePath, staleCookieFor(s), mirrorQueueForm("main", "@1", "later"))
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, `id="stepup-form"`) {
		t.Fatalf("a stale no-script queue post = %d; want the confirm page: %.300s", resp.StatusCode, page)
	}
	wantBrandIcon(t, "the queue confirm page", page, brandedIcon)
}

// TestTheRetryConfirmPageLinksTheBrandIcon: a no-script Retry of a message
// that may already have been sent is answered with the retry confirm page
// (SendQueueRetryConfirmPage).
func TestTheRetryConfirmPageLinksTheBrandIcon(t *testing.T) {
	q := &memQueue{items: []sendqueue.Item{{ID: 1, Owner: authz.OperatorUserID, Target: queueTarget, Body: "did it land",
		State: sendqueue.Failed, MayHaveDelivered: true, CreatedAt: time.Now()}}}
	s, ts := queueServer(t, queueMirror(), q, nil)
	s.ext.Branding = brandedStore()
	resp, page := navPost(t, ts.URL, ui.SendQueueRetryPath(1), sessionCookieFor(s), url.Values{})
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, `id="sq-retry-confirm"`) {
		t.Fatalf("an unconfirmed no-script Retry = %d; want the retry confirm page: %.300s", resp.StatusCode, page)
	}
	wantBrandIcon(t, "the retry confirm page", page, brandedIcon)
}
