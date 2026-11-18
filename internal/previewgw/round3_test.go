package previewgw

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/preview/wire"
)

// replaceRE finds the navigation in /__muster/check's page.
var replaceRE = regexp.MustCompile(`location\.replace\(("(?:[^"\\]|\\.)*")\)`)

// checkTarget is where /__muster/check's page sends the browser, or "".
func checkTarget(body string) string {
	m := replaceRE.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	var s string
	if json.Unmarshal([]byte(m[1]), &s) != nil {
		return ""
	}
	return s
}

// showHere walks muster's Show here chain for preview id as a browser does:
// w. (same-site) navigates muster's frame to the entry URL; the redirect to
// /__muster/check stays in that same-site chain; a further redirect would
// too, while a navigation the p- page starts itself (location.replace) is a
// new, same-origin one. It returns the last answer and the hops taken.
func (e *gwEnv) showHere(t *testing.T, id, entryCode string) (answer, []string) {
	t.Helper()
	nav := func(site, target, cookie string) answer {
		h := http.Header{"Sec-Fetch-Site": {site}, "Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"iframe"}}
		if cookie != "" {
			h.Set("Cookie", OwnerCookie+"="+cookie)
		}
		return e.do(t, http.MethodGet, pHost(id), target, h, "")
	}
	target := enterPath + "?" + url.Values{"g": {entryCode}}.Encode()
	a := nav("same-site", target, "")
	hops := []string{target}
	cookie := ownerCookieOf(t, a)
	site := "same-site"
	for range 4 {
		switch {
		case a.status >= 300 && a.status < 400:
			target = a.header.Get("Location") // still the same-site chain
		case a.status == http.StatusOK && checkTarget(a.body) != "":
			target, site = checkTarget(a.body), "same-origin" // the p- page's own navigation
		default:
			return a, hops
		}
		hops = append(hops, site+" "+target)
		a = nav(site, target, cookie)
	}
	return a, hops
}

// 🔴 An iframe navigation passes only on the gateway's own /__muster/check,
// which never reaches the app. Preview S's script hiding an iframe of
// p-V/admin, with the victim's Strict cookie for V, is refused cross_site and
// V's app sees nothing. Positive control (M26b): muster's Show here chain
// (w. → entry → check → the app) still reaches V's app, because check's page
// navigates on to the app itself (same-origin) instead of redirecting.
func TestASiblingsHiddenIframeIsRefusedAndShowHereStillWorks(t *testing.T) {
	e := newGW(t, gwOpts{tokens: []string{"mpl_owner1", "mpl_owner2"}})
	var victimApp recorder
	e.serve(4211, victimApp.start(t, nil))
	e.serve(4212, appServer(t))
	e.hostFor(t, 1, "alpha", wire.Listener{Port: 4211, Process: "vite", Window: "@1"})
	e.hostFor(t, 2, "beta", wire.Listener{Port: 4212, Process: "evil", Window: "@9"})
	v, vCookie, vTok := e.admitIn(t, 1, "alpha", "@1", 4211)
	_, sCookie, _ := e.admitIn(t, 2, "beta", "@9", 4212)
	jar := OwnerCookie + "=" + sCookie + "; " + OwnerCookie + "=" + vCookie

	before := victimApp.n.Load()
	for _, site := range []string{"same-site", "cross-site"} {
		h := http.Header{"Cookie": {jar}, "Sec-Fetch-Site": {site}, "Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"iframe"}}
		if a := e.do(t, "GET", pHost(v), "/admin", h, ""); a.status != http.StatusForbidden || a.header.Get(ErrorCodeHeader) != codeCrossSite {
			t.Errorf("a %s hidden iframe of p-V/admin: HTTP %d code %q; want 403 %s", site, a.status, a.header.Get(ErrorCodeHeader), codeCrossSite)
		}
	}
	if n := victimApp.n.Load() - before; n != 0 {
		t.Errorf("V's app saw %d requests from a hidden iframe", n)
	}

	u, _, oa := e.openURL(t, vTok, `{"preview":"`+v+`"}`)
	if u == "" {
		t.Fatalf("open V again: HTTP %d %s", oa.status, oa.body)
	}
	before = victimApp.n.Load()
	a, hops := e.showHere(t, v, entryCodeOf(t, u, v))
	if a.status != http.StatusOK || a.body != "ok" || a.header.Get(ErrorCodeHeader) != "" {
		t.Fatalf("Show here: HTTP %d %q code %q after %q; want V's app's 200", a.status, a.body, a.header.Get(ErrorCodeHeader), hops)
	}
	if n := victimApp.n.Load() - before; n != 1 {
		t.Fatalf("Show here: V's app saw %d requests; want 1", n)
	}
}

// 🔴 Origin is rewritten on WebSocket upgrades only. A plain POST reaches the
// app with the browser's own Origin, matching X-Forwarded-Host and
// X-Forwarded-Proto; a WebSocket upgrade gets Origin http://localhost:<port>
// (dev servers compare it with Host).
func TestOriginIsRewrittenOnlyOnWebSockets(t *testing.T) {
	e := newGW(t, gwOpts{})
	var rc recorder
	ws := wsEchoApp(t, nil)
	e.serve(4214, ws.port)
	fh, id, cookie, _ := e.admitted(t, 4213, rc.start(t, nil))
	if a := e.do(t, "POST", pHost(id), "/form", own(id, cookie), "x=1"); a.status != http.StatusOK {
		t.Fatalf("POST: HTTP %d %s", a.status, a.header.Get(ErrorCodeHeader))
	}
	r := rc.last(t)
	if got := r.Header.Get("Origin"); got != "https://"+pHost(id) {
		t.Errorf("POST: the app saw Origin %q; want the browser's own %q", got, "https://"+pHost(id))
	}
	if xfh, xfp := r.Header.Get("X-Forwarded-Host"), r.Header.Get("X-Forwarded-Proto"); xfh != pHost(id) || xfp != "https" {
		t.Errorf("POST: X-Forwarded-Host %q, X-Forwarded-Proto %q; want %q, https (the Origin's host and scheme)", xfh, xfp, pHost(id))
	}

	e.relist(t, fh, 1, "alpha", wire.Listener{Port: 4213, Process: "vite", Window: "@1"},
		wire.Listener{Port: 4214, Process: "node", Window: "@1"})
	wsID, wsCookie, _ := e.admitIn(t, 1, "alpha", "@1", 4214)
	c, resp, err := e.wsDial(t, wsID, "/hmr", own(wsID, wsCookie), nil)
	if err != nil {
		t.Fatalf("WebSocket: %v HTTP %d", err, statusOf(resp))
	}
	wsEcho(t, c, "hello")
	c.Close(websocket.StatusNormalClosure, "")
	u := ws.last(t)
	if got := u.Header.Get("Origin"); got != "http://localhost:4214" {
		t.Errorf("WebSocket: the app saw Origin %q; want http://localhost:4214", got)
	}
	if u.Host != "localhost:4214" {
		t.Errorf("WebSocket: the app saw Host %q", u.Host)
	}
	if xfh, xfp := u.Header.Get("X-Forwarded-Host"), u.Header.Get("X-Forwarded-Proto"); xfh != pHost(wsID) || xfp != "https" {
		t.Errorf("WebSocket: X-Forwarded-Host %q, X-Forwarded-Proto %q; want %q, https", xfh, xfp, pHost(wsID))
	}
}

// 🔴 A compressed event stream gets no keepalive comment: a gzip
// text/event-stream that is quiet for several keepalive intervals before its
// first byte still decodes. Positive control: the same quiet stream with
// Content-Encoding: identity does get the comment.
func TestACompressedEventStreamGetsNoKeepalive(t *testing.T) {
	e := newGW(t, gwOpts{keepalive: 40 * time.Millisecond})
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = io.WriteString(zw, "data: seven\n\n")
	zw.Close()
	var rc recorder
	_, id, cookie, _ := e.admitted(t, 4215, rc.start(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		enc := r.URL.Query().Get("enc")
		w.Header().Set("Content-Encoding", enc)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select { // quiet for several intervals before the first byte
		case <-time.After(300 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		if enc == "gzip" {
			_, _ = w.Write(gz.Bytes())
		} else {
			_, _ = io.WriteString(w, "data: seven\n\n")
		}
	}))
	get := func(enc string) []byte {
		t.Helper()
		h := own(id, cookie)
		h.Set("Accept-Encoding", "gzip") // set by hand: the client hands the body over undecoded
		resp := e.send(t, context.Background(), "GET", pHost(id), "/events?enc="+enc, h, nil)
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("%s stream: %v", enc, err)
		}
		return b
	}
	raw := get("gzip")
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the gzip stream no longer decodes (%v): %q", err, raw)
	}
	got, err := io.ReadAll(zr)
	if err != nil || string(got) != "data: seven\n\n" {
		t.Fatalf("the gzip stream decoded to %q, %v; want the app's one event", got, err)
	}
	if b := string(get("identity")); !strings.HasPrefix(b, ":\n\n") || !strings.HasSuffix(b, "data: seven\n\n") {
		t.Fatalf("positive control: a quiet identity stream got %q; want a comment before the event", b)
	}
}

// streamsOf is how many upstream streams t holds (in use or pooled idle).
func streamsOf(t *Tunnel) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.streams
}

// 🔴 Idle pooled connections never hold the slots another preview needs. A
// host with maxConns 3 serves previews A and B; three concurrent requests to A
// leave three idle pooled connections, which take every slot. A request to B
// then succeeds (no 429). So does one sent while A's three are still in
// flight and that go idle only while B waits (B's wait is 2 s; A's are
// released after 200 ms). Closing A releases its pooled connections at once
// (within 2 s; the pool would keep them 30 s).
func TestIdlePooledConnectionsDoNotStarveAnotherPreview(t *testing.T) {
	e := newGW(t, gwOpts{dialWait: 2 * time.Second})
	var aApp, bApp recorder
	release := make(chan struct{})
	triple := tripleApp()
	e.serve(4216, aApp.start(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			select {
			case <-release:
			case <-time.After(5 * time.Second):
			}
		}
		triple(w, r)
	}))
	e.serve(4217, bApp.start(t, nil))
	fh := e.mustDial(t, dialOpts{listener: "public", token: e.mint(t, 1, "alpha"), hostName: "alpha", maxConns: 3})
	fh.offers <- wire.Offer{Type: wire.TypeConfig, Config: &wire.Config{Ports: []string{"4000-4999"}, Offer: true, MaxOpen: 7, MaxConns: 3}}
	waitFor(t, "the config", func() bool { return e.tunnelFor(1, "alpha").stateMaxConns() == 3 })
	e.relist(t, fh, 1, "alpha", wire.Listener{Port: 4216, Process: "vite", Window: "@1"},
		wire.Listener{Port: 4217, Process: "node", Window: "@1"})
	a, aCookie, aTok := e.admitIn(t, 1, "alpha", "@1", 4216)
	b, bCookie, _ := e.admitIn(t, 1, "alpha", "@1", 4217)
	tn := e.tunnelFor(1, "alpha")
	three := func(path string) func() {
		t.Helper()
		var wg sync.WaitGroup
		var bad atomic.Int64
		for range 3 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req, _ := http.NewRequest("GET", e.public.URL+path, nil)
				req.Host = pHost(a)
				req.Header = own(a, aCookie)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					bad.Add(1)
					return
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 || string(body) != "triple" {
					bad.Add(1)
				}
			}()
		}
		return func() {
			t.Helper()
			wg.Wait()
			if bad.Load() != 0 {
				t.Fatalf("%d of three concurrent requests to A failed", bad.Load())
			}
		}
	}
	three("/")()
	if n := streamsOf(tn); n != 3 {
		t.Fatalf("setup: %d upstream streams after three concurrent requests to A; want its 3 pooled connections", n)
	}
	if r := e.do(t, "GET", pHost(b), "/b", own(b, bCookie), ""); r.status != http.StatusOK || r.body != "ok" {
		t.Fatalf("B beside A's idle pool: HTTP %d %q code %q; want B's 200", r.status, r.body, r.header.Get(ErrorCodeHeader))
	}
	held := three("/hold")
	waitFor(t, "A's three held requests", func() bool { return streamsOf(tn) == 3 && aApp.n.Load() >= 6 })
	time.AfterFunc(200*time.Millisecond, func() { close(release) })
	if r := e.do(t, "GET", pHost(b), "/b2", own(b, bCookie), ""); r.status != http.StatusOK || r.body != "ok" {
		t.Fatalf("B while A's connections went idle during its wait: HTTP %d %q code %q; want B's 200", r.status, r.body, r.header.Get(ErrorCodeHeader))
	}
	held()
	three("/")()
	if r := e.do(t, "POST", webHost(), closePath, bearer(aTok), `{"preview":"`+a+`"}`); r.status != http.StatusOK {
		t.Fatalf("Close A: HTTP %d %s", r.status, r.body)
	}
	// At once: well inside the 30 s the pool would otherwise keep them.
	for deadline := time.Now().Add(2 * time.Second); streamsOf(tn) != 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("closing A left %d pooled upstream streams holding the host's slots 2 s later; want 0", streamsOf(tn))
		}
	}
}

// tripleApp answers 200 "triple" once three requests are in, so three
// requests hold three upstream connections at once.
func tripleApp() http.HandlerFunc {
	var mu sync.Mutex
	n := 0
	all := make(chan struct{})
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		ch := all
		if n%3 == 0 {
			close(all)
			all = make(chan struct{})
		}
		mu.Unlock()
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
		}
		_, _ = io.WriteString(w, "triple")
	}
}

// 🔴 A cookie bomb is reset wherever its cookies can live: for a request
// under /app/x, every cookie it names is expired at /, /app and /app/x,
// host-only and at Domain=<zone>, each also Partitioned (a cookie tossed
// inside muster's frame is partitioned); a __Host- cookie only host-only at /.
func TestACookieBombIsResetAtEveryPathPrefix(t *testing.T) {
	e := newGW(t, gwOpts{})
	var rc recorder
	_, id, cookie, _ := e.admitted(t, 4218, rc.start(t, nil))
	h := own(id, cookie)
	base := "__Host-k=1; deep="
	h.Set("Cookie", base+strings.Repeat("z", 9000))
	a := e.do(t, "GET", pHost(id), "/app/x", h, "")
	if a.status != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("HTTP %d; want 431", a.status)
	}
	var want []string
	for _, p := range []string{"/", "/app", "/app/x"} {
		if p == "/" {
			want = append(want, "__Host-k=; Path=/; Max-Age=0; Secure", "__Host-k=; Path=/; Max-Age=0; Secure; Partitioned")
		}
		want = append(want,
			"deep=; Path="+p+"; Max-Age=0; Secure",
			"deep=; Path="+p+"; Max-Age=0; Secure; Partitioned",
			"deep=; Path="+p+"; Domain="+testDomain+"; Max-Age=0; Secure",
			"deep=; Path="+p+"; Domain="+testDomain+"; Max-Age=0; Secure; Partitioned")
	}
	if got := a.header.Values("Set-Cookie"); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the reset under /app/x:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// An app cannot speak for the gateway, or for the zone: X-Muster-* headers
// (a forged X-Muster-Preview-Error among them) and Clear-Site-Data (which a
// browser applies to the whole registrable domain, every preview and muster's
// w. included) are removed from its responses; its other headers pass.
func TestAppResponsesCannotSpeakForTheGateway(t *testing.T) {
	e := newGW(t, gwOpts{})
	var rc recorder
	_, id, cookie, _ := e.admitted(t, 4219, rc.start(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(ErrorCodeHeader, codeHostOffline)
		w.Header().Set("X-Muster-Owner", "2")
		w.Header().Set("Clear-Site-Data", `"cookies", "storage"`)
		w.Header().Set("X-App-Build", "b17")
		_, _ = io.WriteString(w, "app body")
	}))
	a := e.do(t, "GET", pHost(id), "/", own(id, cookie), "")
	if a.status != http.StatusOK || a.body != "app body" || a.header.Get("X-App-Build") != "b17" {
		t.Fatalf("HTTP %d %q, X-App-Build %q; want the app's response", a.status, a.body, a.header.Get("X-App-Build"))
	}
	for _, k := range []string{ErrorCodeHeader, "X-Muster-Owner"} {
		if v := a.header.Values(k); len(v) != 0 {
			t.Errorf("the app's %s: %q reached the browser", k, v)
		}
	}
	if v := a.header.Values("Clear-Site-Data"); len(v) != 0 {
		t.Errorf("the app's Clear-Site-Data %q reached the browser", v)
	}
}

// 🔴 Nor as a TRAILER: ReverseProxy copies an app's trailers into the
// response after the body, past the header rules. An app declaring
// X-Muster-Preview-Error (and Clear-Site-Data, Set-Cookie) in Trailer:, or
// sending them undeclared (net/http's TrailerPrefix), gets none of them to
// the browser — not in the declared list, not as a value. Its own trailer
// (X-App-Trailer, declared and undeclared) is the positive control: it
// passes, so the browser leg does read trailers.
func TestAppTrailersCannotSpeakForTheGateway(t *testing.T) {
	forged := []string{ErrorCodeHeader, "X-Muster-Owner", "Clear-Site-Data", "Set-Cookie"}
	for _, declared := range []bool{true, false} {
		e := newGW(t, gwOpts{})
		var rc recorder
		_, id, cookie, _ := e.admitted(t, 4373, rc.start(t, func(w http.ResponseWriter, r *http.Request) {
			names := append([]string{"X-App-Trailer"}, forged...)
			if declared {
				w.Header().Set("Trailer", strings.Join(names, ", "))
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "app body")
			w.(http.Flusher).Flush()
			for _, k := range names {
				v := codePreviewClosed
				if k == "X-App-Trailer" {
					v = "t17"
				}
				if !declared {
					k = http.TrailerPrefix + k
				}
				w.Header().Set(k, v)
			}
		}))
		req, err := http.NewRequest("GET", e.public.URL+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = pHost(id)
		for k, vs := range own(id, cookie) {
			req.Header[k] = vs
		}
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		announced := make([]string, 0, len(resp.Trailer))
		for k := range resp.Trailer { // the Trailer: line, before the body
			announced = append(announced, k)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(b) != "app body" {
			t.Fatalf("declared=%v: HTTP %d %q; want the app's response", declared, resp.StatusCode, b)
		}
		if got := resp.Trailer.Get("X-App-Trailer"); got != "t17" {
			t.Fatalf("declared=%v: the app's own trailer %q; want t17 (the browser leg must carry trailers)", declared, got)
		}
		for _, k := range forged {
			for _, a := range announced {
				if strings.EqualFold(a, k) {
					t.Errorf("declared=%v: the app's %s trailer was announced to the browser (Trailer: %v)", declared, k, announced)
				}
			}
			if v := resp.Trailer.Values(k); len(v) != 0 {
				t.Errorf("declared=%v: the app's %s trailer %q reached the browser", declared, k, v)
			}
			if v := resp.Header.Values(k); len(v) != 0 {
				t.Errorf("declared=%v: the app's %s trailer %q reached the browser as a header", declared, k, v)
			}
		}
	}
}

// 🔴 A browser leg that stops answering pings is closed, and with it the
// app's leg: the browser here never reads, so it never answers a ping, and
// the app sees its connection end. (TestAQuietWebSocketIsPingedOnTheBrowserLeg
// is the positive control: a leg that answers stays open.)
func TestABrowserLegThatStopsAnsweringPingsIsClosed(t *testing.T) {
	e := newGW(t, gwOpts{keepalive: 50 * time.Millisecond})
	ended := make(chan struct{})
	var once sync.Once
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			if _, _, err := c.Read(context.Background()); err != nil {
				once.Do(func() { close(ended) })
				return
			}
		}
	}))
	t.Cleanup(app.Close)
	_, id, cookie, _ := e.admitted(t, 4220, app.Listener.Addr().(*net.TCPAddr).Port)
	c, resp, err := e.wsDial(t, id, "/", own(id, cookie), nil)
	if err != nil {
		t.Fatalf("dial: %v HTTP %d", err, statusOf(resp))
	}
	defer c.CloseNow()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the app's leg was still open 5 s after the browser stopped answering pings")
	}
}
