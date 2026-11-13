package previewgw

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/preview/wire"
)

// ------------------------------------------------------------ helpers

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Timeout: 10 * time.Second}

type answer struct {
	status int
	header http.Header
	body   string
}

// do sends one request to the public listener with Host host.
func (e *gwEnv) do(t *testing.T, method, host, target string, hdr http.Header, body string) answer {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.public.URL+target, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	for k, vs := range hdr {
		req.Header[k] = vs
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatalf("%s %s%s: %v", method, host, target, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return answer{resp.StatusCode, resp.Header, string(b)}
}

func webHost() string               { return "w." + testDomain }
func pHost(id string) string        { return "p-" + id + "." + testDomain }
func bearer(tok string) http.Header { return http.Header{"Authorization": {"Bearer " + tok}} }

// enter opens the session page with a hand-off code for plugin pid.
func (e *gwEnv) enter(t *testing.T, code string, pid int64) answer {
	t.Helper()
	q := url.Values{"muster_code": {code}, "muster_plugin": {itoa(pid)}}
	return e.do(t, http.MethodGet, webHost(), enterPath+"?"+q.Encode(), nil, "")
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

var bootRE = regexp.MustCompile(`<script id="boot" type="application/json">(.*?)</script>`)

// pageToken is the page token a rendered session page carries.
func pageToken(t *testing.T, a answer) string {
	t.Helper()
	m := bootRE.FindStringSubmatch(a.body)
	if m == nil {
		t.Fatalf("no boot element in the page (HTTP %d):\n%s", a.status, a.body)
	}
	var boot struct{ Token string }
	if err := json.Unmarshal([]byte(m[1]), &boot); err != nil || boot.Token == "" {
		t.Fatalf("boot %q: %v", m[1], err)
	}
	return boot.Token
}

// session enters owner 1's session page for (alpha, window) and returns its
// page token.
func (e *gwEnv) session(t *testing.T, window string) string {
	t.Helper()
	a := e.enter(t, e.fm.handoff("mpl_owner1", 1, "alpha", "main", window), 10)
	if a.status != http.StatusOK {
		t.Fatalf("enter %s: HTTP %d\n%s", window, a.status, a.body)
	}
	return pageToken(t, a)
}

// openURL asks the page's open route for an entry URL.
func (e *gwEnv) openURL(t *testing.T, tok, body string) (string, string, answer) {
	t.Helper()
	a := e.do(t, http.MethodPost, webHost(), openPath, bearer(tok), body)
	if a.status != http.StatusOK {
		return "", "", a
	}
	var out struct{ URL, Preview string }
	if err := json.Unmarshal([]byte(a.body), &out); err != nil {
		t.Fatalf("open answer %q: %v", a.body, err)
	}
	return out.URL, out.Preview, a
}

// entryCodeOf is g=… of an entry URL, after checking the URL names id's host.
func entryCodeOf(t *testing.T, u, id string) string {
	t.Helper()
	p, err := url.Parse(u)
	if err != nil || p.Scheme != "https" || p.Host != pHost(id) || p.Path != enterPath {
		t.Fatalf("entry URL %q (preview %s)", u, id)
	}
	return p.Query().Get("g")
}

// useEntry spends an entry code on id's host, as a browser with dest would.
func (e *gwEnv) useEntry(t *testing.T, id, code, dest string) answer {
	t.Helper()
	h := http.Header{}
	if dest != "" {
		h.Set("Sec-Fetch-Dest", dest)
	}
	return e.do(t, http.MethodGet, pHost(id), enterPath+"?"+url.Values{"g": {code}}.Encode(), h, "")
}

func ownerCookieOf(t *testing.T, a answer) string {
	t.Helper()
	resp := http.Response{Header: a.header}
	for _, c := range resp.Cookies() {
		if c.Name == OwnerCookie {
			return c.Value
		}
	}
	t.Fatalf("no %s cookie set (HTTP %d, Set-Cookie %q)", OwnerCookie, a.status, a.header.Values("Set-Cookie"))
	return ""
}

func withCookie(v string) http.Header { return http.Header{"Cookie": {OwnerCookie + "=" + v}} }

// hostWithOffers dials alpha for owner 1 on the public listener and reports
// ls as its listeners, waiting until the gateway holds them.
func (e *gwEnv) hostWithOffers(t *testing.T, ls ...wire.Listener) *fakeHost {
	t.Helper()
	fh := e.mustDial(t, dialOpts{listener: "public", token: e.mint(t, 1, "alpha"), hostName: "alpha"})
	fh.offers <- wire.Offer{Type: wire.TypeListeners, Listeners: ls}
	waitFor(t, "the listeners", func() bool {
		tn := e.tunnelFor(1, "alpha")
		if tn == nil {
			return false
		}
		tn.mu.Lock()
		defer tn.mu.Unlock()
		return len(tn.state.Listeners) == len(ls)
	})
	return fh
}

// agentOpen opens a preview as an agent's preview_open would; it returns its id.
func agentOpen(t *testing.T, fh *fakeHost, id, window string, port int) string {
	t.Helper()
	fh.offers <- wire.Offer{Type: wire.TypeOpen, Request: &wire.Request{ID: id, Window: window, Port: port, Label: "api"}}
	for {
		select {
		case a := <-fh.answers:
			if a.a.RequestID != id {
				continue // an unsolicited answer (an owner's Open), not this one
			}
			if a.a.Error != "" || a.a.Preview == "" {
				t.Fatalf("open %s: %+v", id, a.a)
			}
			return a.a.Preview
		case <-time.After(5 * time.Second):
			t.Fatal("no answer to the open")
		}
	}
}

// openedPreview: a host with one offer (vite :5173 in @1), the owner's page
// for @1, Open clicked on the offer. It returns the preview id and its entry
// code.
func (e *gwEnv) openedPreview(t *testing.T) (*fakeHost, string, string, string) {
	t.Helper()
	fh := e.hostWithOffers(t, wire.Listener{Port: 5173, Process: "vite", Window: "@1", FirstSeen: "2000-01-02T03:04:05Z"})
	tok := e.session(t, "@1")
	u, id, a := e.openURL(t, tok, `{"port":5173}`)
	if u == "" {
		t.Fatalf("open the offer: HTTP %d %s", a.status, a.body)
	}
	return fh, tok, id, entryCodeOf(t, u, id)
}

// ------------------------------------------------------------ the tests

// 🔴 No w. response ever carries Set-Cookie: every route, in every outcome,
// walked. The p- entry's Set-Cookie at the end is the positive control (the
// check can see a cookie when one is set).
func TestNoWebResponseEverSetsACookie(t *testing.T) {
	e := newGW(t, gwOpts{})
	fh := e.hostWithOffers(t, wire.Listener{Port: 5173, Process: "vite", Window: "@1"})
	agentID := agentOpen(t, fh, "r1", "@1", 8000)
	page := e.enter(t, e.fm.handoff("mpl_owner1", 1, "alpha", "main", "@1"), 10)
	tok := pageToken(t, page)
	walk := map[string]answer{
		"GET /":                     e.do(t, "GET", webHost(), "/", nil, ""),
		"GET enter, no code":        e.do(t, "GET", webHost(), enterPath, nil, ""),
		"GET enter, unknown code":   e.enter(t, "nope", 10),
		"GET enter, unknown plugin": e.enter(t, e.fm.handoff("mpl_owner1", 1, "alpha", "main", "@1"), 99),
		"GET enter, the page":       page,
		"POST enter":                e.do(t, "POST", webHost(), enterPath, nil, "x"),
		"GET list":                  e.do(t, "GET", webHost(), listPath, bearer(tok), ""),
		"GET list, no token":        e.do(t, "GET", webHost(), listPath, nil, ""),
		"POST open, offer":          e.do(t, "POST", webHost(), openPath, bearer(tok), `{"port":5173}`),
		"POST open, preview":        e.do(t, "POST", webHost(), openPath, bearer(tok), `{"preview":"`+agentID+`"}`),
		"POST open, bad body":       e.do(t, "POST", webHost(), openPath, bearer(tok), `{}`),
		"POST open, no token":       e.do(t, "POST", webHost(), openPath, nil, `{"port":5173}`),
		"GET open":                  e.do(t, "GET", webHost(), openPath, bearer(tok), ""),
		"GET other path":            e.do(t, "GET", webHost(), "/metrics", nil, ""),
	}
	if walk["GET enter, the page"].status != 200 || walk["POST open, offer"].status != 200 || walk["GET list"].status != 200 {
		t.Fatalf("the walk did not reach the happy paths: page %d, open %d, list %d",
			walk["GET enter, the page"].status, walk["POST open, offer"].status, walk["GET list"].status)
	}
	for name, a := range walk {
		if sc := a.header.Values("Set-Cookie"); len(sc) != 0 {
			t.Errorf("w. %s (HTTP %d) set a cookie: %q", name, a.status, sc)
		}
	}
	// Positive control: the same check sees the owner cookie p- sets.
	_, _, id, code := e.openedPreview2(t, tok)
	if sc := e.useEntry(t, id, code, "document").header.Values("Set-Cookie"); len(sc) == 0 {
		t.Fatal("positive control: the p- entry set no cookie, so the check above proves nothing")
	}
}

// openedPreview2 is Open on the @1 offer with an existing page token.
func (e *gwEnv) openedPreview2(t *testing.T, tok string) (string, string, string, string) {
	t.Helper()
	u, id, a := e.openURL(t, tok, `{"port":5173}`)
	if u == "" {
		t.Fatalf("open: HTTP %d %s", a.status, a.body)
	}
	return u, tok, id, entryCodeOf(t, u, id)
}

// The entry route answers the page itself (no redirect), whose script strips
// the code; a reload (no code), or the zone's root, shows "open from muster".
func TestAReloadWithoutACodeShowsOpenFromMuster(t *testing.T) {
	e := newGW(t, gwOpts{})
	a := e.enter(t, e.fm.handoff("mpl_owner1", 1, "alpha", "main", "@1"), 10)
	if a.status != http.StatusOK || a.header.Get("Location") != "" || !strings.Contains(a.body, "history.replaceState(null,'','/')") {
		t.Fatalf("enter: HTTP %d, Location %q; want the page itself, stripping the code", a.status, a.header.Get("Location"))
	}
	for _, target := range []string{enterPath, "/", enterPath + "?muster_plugin=10"} {
		r := e.do(t, "GET", webHost(), target, nil, "")
		if !strings.Contains(r.body, "Open this from muster") || strings.Contains(r.body, `id="boot"`) {
			t.Fatalf("reload %s: HTTP %d, want the open-from-muster page:\n%s", target, r.status, r.body)
		}
	}
}

// A hand-off code spent once is refused the second time, with the
// open-from-muster page (muster's 404 is a refusal, not an outage).
func TestAReplayedHandoffCodeIsRefused(t *testing.T) {
	e := newGW(t, gwOpts{})
	code := e.fm.handoff("mpl_owner1", 1, "alpha", "main", "@1")
	if a := e.enter(t, code, 10); a.status != http.StatusOK {
		t.Fatalf("first use: HTTP %d", a.status)
	}
	a := e.enter(t, code, 10)
	if a.status != http.StatusForbidden || !strings.Contains(a.body, "Open this from muster") || strings.Contains(a.body, `id="boot"`) {
		t.Fatalf("replay: HTTP %d; want 403 open-from-muster:\n%s", a.status, a.body)
	}
}

// muster_plugin picks the token to redeem with; a code is redeemed only with
// its own plugin's token.
func TestMusterPluginPicksTheToken(t *testing.T) {
	e := newGW(t, gwOpts{tokens: []string{"mpl_owner1", "mpl_owner2"}})
	if a := e.enter(t, e.fm.handoff("mpl_owner2", 2, "beta", "s", "@4"), 20); a.status != http.StatusOK ||
		!strings.Contains(a.body, "beta") {
		t.Fatalf("owner 2's code with plugin 20: HTTP %d", a.status)
	}
	if a := e.enter(t, e.fm.handoff("mpl_owner2", 2, "beta", "s", "@4"), 10); a.status != http.StatusForbidden {
		t.Fatalf("owner 2's code with plugin 10: HTTP %d; want 403", a.status)
	}
}

// 🔴 A token whose /self lacks viewer is never used to redeem (G2's rule:
// that answer is a 401). The fake muster counts redeem calls; the positive
// control is the same flow with viewer, which counts one.
func TestATokenWithoutViewerCannotRedeem(t *testing.T) {
	e := newGW(t, gwOpts{})
	if a := e.enter(t, e.fm.handoff("mpl_owner1", 1, "alpha", "main", "@1"), 10); a.status != http.StatusOK || e.fm.redeems.Load() != 1 {
		t.Fatalf("positive control: HTTP %d, %d redeem calls; want 200 and 1", a.status, e.fm.redeems.Load())
	}
	e.fm.set("mpl_owner1", 200, Self{PluginID: 10, PluginName: "previews", OwnerID: 1, Capabilities: []string{"messages"}})
	e.g.CheckOwners(context.Background())
	a := e.enter(t, e.fm.handoff("mpl_owner1", 1, "alpha", "main", "@1"), 10)
	if n := e.fm.redeems.Load(); n != 1 {
		t.Fatalf("a token without viewer was used to redeem (%d calls)", n)
	}
	if a.status != http.StatusForbidden || !strings.Contains(a.body, "Open this from muster") {
		t.Fatalf("without viewer: HTTP %d; want 403 open-from-muster", a.status)
	}
}

// The entry code is single use, lasts 60 s, and is bound to its preview.
func TestTheEntryCodeIsSingleUseSixtySecondsAndBoundToItsPreview(t *testing.T) {
	e := newGW(t, gwOpts{})
	fh, tok, id, code := e.openedPreview(t)
	if a := e.useEntry(t, id, code, "document"); a.status != http.StatusSeeOther || a.header.Get("Location") != checkPath {
		t.Fatalf("first use: HTTP %d, Location %q", a.status, a.header.Get("Location"))
	}
	if a := e.useEntry(t, id, code, "document"); a.status != http.StatusForbidden || len(a.header.Values("Set-Cookie")) != 0 {
		t.Fatalf("replayed entry code: HTTP %d, Set-Cookie %q; want 403 and none", a.status, a.header.Values("Set-Cookie"))
	}

	_, _, _, code = e.openedPreview2(t, tok)
	e.clock.Advance(59 * time.Second) // literal: the contract, not the constant
	if a := e.useEntry(t, id, code, "document"); a.status != http.StatusSeeOther {
		t.Fatalf("at 59 s: HTTP %d; want accepted", a.status)
	}
	_, _, _, code = e.openedPreview2(t, tok)
	e.clock.Advance(60 * time.Second)
	if a := e.useEntry(t, id, code, "document"); a.status != http.StatusForbidden {
		t.Fatalf("at 60 s: HTTP %d; want 403", a.status)
	}

	// Bound to its preview: a code for this preview, spent on another's host.
	tok = e.session(t, "@1") // the first page token has expired by now (15 min)
	other := agentOpen(t, fh, "r2", "@1", 8000)
	_, _, _, code = e.openedPreview2(t, tok)
	if a := e.useEntry(t, other, code, "document"); a.status != http.StatusForbidden {
		t.Fatalf("a code spent on another preview's host: HTTP %d; want 403", a.status)
	}
}

// 🔴 The owner cookie is bound to its preview and its owner.
func TestTheOwnerCookieIsBoundToItsPreviewAndOwner(t *testing.T) {
	e := newGW(t, gwOpts{})
	fh, _, id, code := e.openedPreview(t)
	cookie := ownerCookieOf(t, e.useEntry(t, id, code, "document"))
	if a := e.do(t, "GET", pHost(id), "/", withCookie(cookie), ""); a.status != http.StatusOK || a.body != appBody+"/" {
		t.Fatalf("its own preview: HTTP %d %q; want admitted (the app's answer)", a.status, a.body)
	}
	other := agentOpen(t, fh, "r2", "@1", 8000)
	if a := e.do(t, "GET", pHost(other), "/", withCookie(cookie), ""); a.status != http.StatusForbidden {
		t.Fatalf("another preview's cookie: HTTP %d; want 403", a.status)
	}
	forged, err := e.g.sign.mintOwnerCookie(id, 2, e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if a := e.do(t, "GET", pHost(id), "/", withCookie(forged), ""); a.status != http.StatusForbidden {
		t.Fatalf("a cookie for this preview but another owner: HTTP %d; want 403", a.status)
	}
	pageTok := e.session(t, "@1")
	if a := e.do(t, "GET", pHost(id), "/", withCookie(pageTok), ""); a.status != http.StatusForbidden {
		t.Fatalf("a page token as the cookie: HTTP %d; want 403", a.status)
	}
}

// The owner cookie lasts 8 h on the gateway's clock: 8 h − 1 s admitted,
// 8 h + 1 s refused.
func TestTheOwnerCookieExpiresAtEightHours(t *testing.T) {
	// Idle and TTL past the test's 8 h, so only the cookie can end it.
	e := newGW(t, gwOpts{idle: 11 * time.Hour, ttl: 13 * time.Hour})
	_, _, id, code := e.openedPreview(t)
	cookie := ownerCookieOf(t, e.useEntry(t, id, code, "document"))
	e.clock.Advance(8*time.Hour - time.Second) // literal: the contract, not the constant
	if a := e.do(t, "GET", pHost(id), "/", withCookie(cookie), ""); a.status != http.StatusOK {
		t.Fatalf("at 8h-1s: HTTP %d; want admitted", a.status)
	}
	e.clock.Advance(2 * time.Second)
	if a := e.do(t, "GET", pHost(id), "/", withCookie(cookie), ""); a.status != http.StatusForbidden ||
		!strings.Contains(a.body, "Session expired") {
		t.Fatalf("at 8h+1s: HTTP %d; want 403 session expired", a.status)
	}
}

// SameSite follows Sec-Fetch-Dest: Strict for a document, None; Partitioned
// for an iframe; always Secure, HttpOnly, Path=/, host-only.
func TestTheOwnerCookieSameSiteFollowsSecFetchDest(t *testing.T) {
	e := newGW(t, gwOpts{})
	_, tok, id, code := e.openedPreview(t)
	for _, c := range []struct{ dest, want, not string }{
		{"document", "SameSite=Strict", "Partitioned"},
		{"iframe", "SameSite=None; Partitioned", "Strict"},
	} {
		if c.dest == "iframe" {
			_, _, _, code = e.openedPreview2(t, tok)
		}
		sc := strings.Join(e.useEntry(t, id, code, c.dest).header.Values("Set-Cookie"), "\n")
		for _, must := range []string{OwnerCookie + "=", c.want, "Secure", "HttpOnly", "Path=/", "Max-Age=28800"} {
			if !strings.Contains(sc, must) {
				t.Errorf("%s: Set-Cookie %q lacks %q", c.dest, sc, must)
			}
		}
		if strings.Contains(sc, c.not) || strings.Contains(sc, "Domain=") {
			t.Errorf("%s: Set-Cookie %q has %q or a Domain", c.dest, sc, c.not)
		}
	}
}

// frame-ancestors on w. and on the gateway's own p- pages is exactly
// MUSTER_PUBLIC_URL's origin (no path), with Referrer-Policy no-referrer.
func TestFrameAncestorsIsMusterPublicURLsOrigin(t *testing.T) {
	e := newGW(t, gwOpts{})
	_, _, id, code := e.openedPreview(t)
	for name, a := range map[string]answer{
		"w. page":    e.enter(t, e.fm.handoff("mpl_owner1", 1, "alpha", "main", "@1"), 10),
		"w. root":    e.do(t, "GET", webHost(), "/", nil, ""),
		"w. 404":     e.do(t, "GET", webHost(), "/x", nil, ""),
		"p- entry":   e.useEntry(t, id, code, "iframe"),
		"p- refused": e.do(t, "GET", pHost(id), "/", nil, ""),
	} {
		csp := a.header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "frame-ancestors "+testPublicOrigin) || strings.Contains(csp, "frame-ancestors "+testPublicOrigin+"/") ||
			strings.HasSuffix(csp, "/base") {
			t.Errorf("%s: CSP %q; want frame-ancestors %s", name, csp, testPublicOrigin)
		}
		if got := a.header.Get("Referrer-Policy"); got != "no-referrer" {
			t.Errorf("%s: Referrer-Policy %q", name, got)
		}
		if a.header.Get("X-Frame-Options") != "" {
			t.Errorf("%s: X-Frame-Options would forbid muster's frame", name)
		}
	}
}

// 🔴 The page shows only its target window's offers and previews; another
// window's can be neither seen nor opened with its token.
func TestThePageShowsOnlyItsTargetWindow(t *testing.T) {
	e := newGW(t, gwOpts{})
	fh := e.hostWithOffers(t,
		wire.Listener{Port: 5173, Process: "vite", Window: "@1"},
		wire.Listener{Port: 5174, Process: "hidden-proc", Window: "@2"})
	mine := agentOpen(t, fh, "r1", "@1", 8000)
	theirs := agentOpen(t, fh, "r2", "@2", 8001)
	page := e.enter(t, e.fm.handoff("mpl_owner1", 1, "alpha", "main", "@1"), 10)
	tok := pageToken(t, page)
	list := e.do(t, "GET", webHost(), listPath, bearer(tok), "")
	for name, body := range map[string]string{"page": page.body, "list": list.body} {
		for _, want := range []string{"vite · :5173", ":8000", mine} {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
		for _, absent := range []string{"hidden-proc", "5174", ":8001", theirs} {
			if strings.Contains(body, absent) {
				t.Errorf("%s shows another window's %q", name, absent)
			}
		}
	}
	if _, _, a := e.openURL(t, tok, `{"port":5174}`); a.status != http.StatusNotFound {
		t.Errorf("open another window's offer: HTTP %d; want 404", a.status)
	}
	if _, _, a := e.openURL(t, tok, `{"preview":"`+theirs+`"}`); a.status != http.StatusNotFound {
		t.Errorf("open another window's preview: HTTP %d; want 404", a.status)
	}
}

// 🔴 Offers expose nothing: with an offer present and no click, there is no
// preview, a p- request gets 404, and no stream is opened to the host. Open
// is what creates the preview, and tells the host unsolicited.
func TestOffersExposeNothingBeforeOpen(t *testing.T) {
	e := newGW(t, gwOpts{})
	fh := e.hostWithOffers(t, wire.Listener{Port: 5173, Process: "vite", Window: "@1"})
	if ps := e.g.Previews(1); len(ps) != 0 {
		t.Fatalf("an offer made previews: %+v", ps)
	}
	for _, id := range []string{"aaaaaaaaaaaaaaaaaaaaaaaaaa", "zzzzzzzzzzzzzzzzzzzzzzzzzz"} {
		for _, path := range []string{"/", enterPath + "?g=x", checkPath} {
			if a := e.do(t, "GET", pHost(id), path, nil, ""); a.status != http.StatusNotFound {
				t.Fatalf("p-%s%s before Open: HTTP %d; want 404", id, path, a.status)
			}
		}
	}
	if n := fh.connects.Load(); n != 0 {
		t.Fatalf("%d streams opened to the host before Open", n)
	}
	tok := e.session(t, "@1")
	_, id, a := e.openURL(t, tok, `{"port":5173}`)
	if id == "" {
		t.Fatalf("open: HTTP %d %s", a.status, a.body)
	}
	select {
	case got := <-fh.answers:
		if got.path != wire.OpenedPath || got.a != (wire.Answer{Preview: id, Window: "@1", Port: 5173}) {
			t.Fatalf("the host was told %s %+v", got.path, got.a)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the host was not told of the owner's open")
	}
	if a := e.do(t, "GET", pHost(id), "/", nil, ""); a.status != http.StatusForbidden {
		t.Fatalf("the opened preview without a cookie: HTTP %d; want 403 (it exists now)", a.status)
	}
	// Open's probe is the first stream (and the positive control for the
	// count above); one more dial is one more.
	if n := fh.connects.Load(); n != 1 {
		t.Fatalf("%d streams after Open; want 1 (its probe)", n)
	}
	c, err := e.g.Dial(context.Background(), 1, "alpha", echoServer(t))
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if n := fh.connects.Load(); n != 2 {
		t.Fatalf("%d streams after Open and one dial; want 2", n)
	}
}

// The page token lasts 15 minutes, and is refused without its owner served.
func TestThePageTokenExpiresAtFifteenMinutes(t *testing.T) {
	e := newGW(t, gwOpts{})
	tok := e.session(t, "@1")
	e.clock.Advance(15*time.Minute - time.Second) // literal: the contract, not the constant
	if a := e.do(t, "GET", webHost(), listPath, bearer(tok), ""); a.status != http.StatusOK {
		t.Fatalf("at 15m-1s: HTTP %d", a.status)
	}
	e.clock.Advance(2 * time.Second)
	if a := e.do(t, "GET", webHost(), listPath, bearer(tok), ""); a.status != http.StatusUnauthorized {
		t.Fatalf("at 15m+1s: HTTP %d; want 401", a.status)
	}
}

// A host on the public path is labelled slow; on the mesh path it is not.
func TestASlowPathHostIsLabelled(t *testing.T) {
	e := newGW(t, gwOpts{})
	tokHost := e.mint(t, 1, "alpha")
	e.mustDial(t, dialOpts{listener: "mesh", token: tokHost, hostName: "alpha"})
	tok := e.session(t, "@1")
	if a := e.do(t, "GET", webHost(), listPath, bearer(tok), ""); strings.Contains(a.body, SlowPathLabel) {
		t.Fatalf("mesh host labelled slow:\n%s", a.body)
	}
	e.mustDial(t, dialOpts{listener: "public", token: tokHost, hostName: "alpha"})
	waitFor(t, "the public tunnel to replace the mesh one", func() bool {
		tn := e.tunnelFor(1, "alpha")
		return tn != nil && tn.path == PathPublic
	})
	if a := e.do(t, "GET", webHost(), listPath, bearer(tok), ""); !strings.Contains(a.body, SlowPathLabel) {
		t.Fatalf("public host not labelled slow:\n%s", a.body)
	}
}

// 🔴 The session page's Open, in its own frame or tab, REPLACES the page and
// adds no history entry: inside muster's panel the page is a frame, and a
// frame navigation is an entry in the tab's joint session history, which the
// Android shell's Back (goBack) walked back to the page's spent entry URL — a
// white, broken frame. Read off the page as served, its script included.
// (e2e/tests/previews.spec.ts measures history.length across the real click.)
func TestOpenReplacesTheSessionPage(t *testing.T) {
	e := newGW(t, gwOpts{})
	e.hostWithOffers(t, wire.Listener{Port: 5173, Process: "vite", Window: "@1"})
	page := e.enter(t, e.fm.handoff("mpl_owner1", 1, "alpha", "main", "@1"), 10)
	if !strings.Contains(page.body, "if(tab)tab.location.href=a.url;else location.replace(a.url);") {
		t.Errorf("Open does not replace the page with location.replace(a.url)")
	}
	// No other way of navigating THIS document: an assign, or a location (or
	// location.href) assignment that is not the new tab's.
	selfNav := regexp.MustCompile(`location\.assign\(|(?:^|[^.\w])(?:window\.|document\.)?location(?:\.href)?\s*=[^=]`)
	if m := selfNav.FindString(page.body); m != "" {
		t.Errorf("the page navigates itself with an entry-adding %q", m)
	}
}
