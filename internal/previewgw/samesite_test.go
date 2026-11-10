package previewgw

import (
	"net/http"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/preview/wire"
)

// 🔴 A sibling preview cannot act on the victim's. The victim (owner 1) has
// preview V open. The attacker (owner 2) mints an entry link to their own
// preview S and gets the victim's browser to follow it (login CSRF): the
// victim's browser now holds a valid __Host-mp for S AS WELL AS its own for V.
// S's script is the attacker's. Every way S's script can reach V carrying
// V's cookie is refused (403 cross_site) and V's app sees none of it:
// same-site fetch (cors), subresource (no-cors), WebSocket, form POST; and,
// for a browser with no Fetch Metadata, a POST and a WebSocket with S's
// Origin. S's cookie alone admits nothing on V. Positive controls on V
// itself: same-origin, a typed URL (none), a top-level navigation, an
// iframe navigation, and its own WebSocket all pass.
func TestASiblingPreviewTheVictimWasLoggedIntoIsRefused(t *testing.T) {
	e := newGW(t, gwOpts{tokens: []string{"mpl_owner1", "mpl_owner2"}})
	var victimApp recorder
	ws := wsEchoApp(t, nil)
	e.serve(4191, victimApp.start(t, nil))
	e.serve(4192, ws.port)
	e.serve(4193, appServer(t))
	e.hostFor(t, 1, "alpha", wire.Listener{Port: 4191, Process: "vite", Window: "@1"},
		wire.Listener{Port: 4192, Process: "node", Window: "@1"})
	e.hostFor(t, 2, "beta", wire.Listener{Port: 4193, Process: "evil", Window: "@9"})
	v, vCookie, _ := e.admitIn(t, 1, "alpha", "@1", 4191)
	vws, vwsCookie, _ := e.admitIn(t, 1, "alpha", "@1", 4192)
	// The attacker's own entry link, spent by the victim's browser.
	s, sCookie, _ := e.admitIn(t, 2, "beta", "@9", 4193)
	if a := e.do(t, "GET", pHost(s), "/", own(s, sCookie), ""); a.status != http.StatusOK {
		t.Fatalf("setup: the victim's browser is not logged into the attacker's preview (HTTP %d)", a.status)
	}
	jar := OwnerCookie + "=" + sCookie + "; " + OwnerCookie + "=" + vCookie // the victim's browser sends both names
	sOrigin := "https://" + pHost(s)
	from := func(kv ...string) http.Header {
		h := http.Header{"Cookie": {jar}}
		for i := 0; i < len(kv); i += 2 {
			h.Set(kv[i], kv[i+1])
		}
		return h
	}
	before := victimApp.n.Load()
	refused := []struct {
		name, method string
		hdr          http.Header
	}{
		{"same-site fetch", "GET", from("Sec-Fetch-Site", "same-site", "Sec-Fetch-Mode", "cors", "Sec-Fetch-Dest", "empty", "Origin", sOrigin)},
		{"same-site subresource", "GET", from("Sec-Fetch-Site", "same-site", "Sec-Fetch-Mode", "no-cors", "Sec-Fetch-Dest", "image")},
		{"same-site form POST", "POST", from("Sec-Fetch-Site", "same-site", "Sec-Fetch-Mode", "navigate", "Sec-Fetch-Dest", "document", "Origin", sOrigin)},
		{"cross-site fetch", "GET", from("Sec-Fetch-Site", "cross-site", "Sec-Fetch-Mode", "cors", "Origin", "https://evil.example")},
		{"no Fetch Metadata, POST with the sibling's Origin", "POST", from("Origin", sOrigin)},
		{"no Fetch Metadata, POST with no Origin", "POST", from()},
	}
	for _, c := range refused {
		a := e.do(t, c.method, pHost(v), "/act", c.hdr, "x=1")
		if a.status != http.StatusForbidden || a.header.Get(ErrorCodeHeader) != codeCrossSite {
			t.Errorf("%s: HTTP %d code %q; want 403 %s", c.name, a.status, a.header.Get(ErrorCodeHeader), codeCrossSite)
		}
	}
	for name, h := range map[string]http.Header{
		"WebSocket from the sibling":                     from("Sec-Fetch-Site", "same-site", "Sec-Fetch-Mode", "websocket", "Origin", sOrigin),
		"WebSocket, no Fetch Metadata, sibling Origin":   from("Origin", sOrigin),
		"WebSocket claiming same-origin, sibling Origin": from("Sec-Fetch-Site", "same-origin", "Origin", sOrigin),
	} {
		h.Set("Cookie", OwnerCookie+"="+sCookie+"; "+OwnerCookie+"="+vwsCookie)
		if _, resp, err := e.wsDial(t, vws, "/", h, nil); err == nil || statusOf(resp) != http.StatusForbidden {
			t.Errorf("%s: %v HTTP %d; want 403", name, err, statusOf(resp))
		}
	}
	if n := victimApp.n.Load() - before; n != 0 {
		t.Errorf("the victim's app saw %d requests from the sibling", n)
	}
	if a := e.do(t, "GET", pHost(v), "/", own(v, sCookie), ""); a.status != http.StatusForbidden ||
		a.header.Get(ErrorCodeHeader) != codeSessionExpired {
		t.Errorf("the sibling's cookie alone on V: HTTP %d %q; want 403 %s", a.status, a.header.Get(ErrorCodeHeader), codeSessionExpired)
	}

	// Positive controls: V's own requests, with the same jar.
	for _, c := range []struct {
		name, method string
		hdr          http.Header
	}{
		{"same-origin fetch", "GET", from("Sec-Fetch-Site", "same-origin", "Sec-Fetch-Mode", "cors")},
		{"same-origin POST", "POST", from("Sec-Fetch-Site", "same-origin", "Sec-Fetch-Mode", "cors", "Origin", "https://"+pHost(v))},
		{"typed URL", "GET", from("Sec-Fetch-Site", "none", "Sec-Fetch-Mode", "navigate", "Sec-Fetch-Dest", "document")},
		{"top-level navigation", "GET", from("Sec-Fetch-Site", "same-site", "Sec-Fetch-Mode", "navigate", "Sec-Fetch-Dest", "document")},
		{"muster's frame", "GET", from("Sec-Fetch-Site", "same-site", "Sec-Fetch-Mode", "navigate", "Sec-Fetch-Dest", "iframe")},
		{"no Fetch Metadata, GET", "GET", from()},
		{"no Fetch Metadata, POST with its own Origin", "POST", from("Origin", "https://"+pHost(v))},
	} {
		if a := e.do(t, c.method, pHost(v), "/ok", c.hdr, "x=1"); a.status != http.StatusOK {
			t.Errorf("positive control %s: HTTP %d %s", c.name, a.status, a.header.Get(ErrorCodeHeader))
		}
	}
	h := from("Sec-Fetch-Site", "same-origin", "Sec-Fetch-Mode", "websocket", "Origin", "https://"+pHost(vws))
	h.Set("Cookie", OwnerCookie+"="+sCookie+"; "+OwnerCookie+"="+vwsCookie)
	c, resp, err := e.wsDial(t, vws, "/", h, nil)
	if err != nil {
		t.Fatalf("positive control: V's own WebSocket: %v HTTP %d", err, statusOf(resp))
	}
	wsEcho(t, c, "own socket")
	c.Close(websocket.StatusNormalClosure, "")
}

// A Cookie header past 8 KiB (cookie bombing) gets the reset page: 431
// too_many_cookies, expiring every cookie it names host-only and at
// Domain=<zone> (a __Host- cookie host-only only); the app is not reached.
// 8192 bytes exactly is still proxied.
func TestACookieBombGetsTheResetPage(t *testing.T) {
	e := newGW(t, gwOpts{})
	var rc recorder
	_, id, cookie, _ := e.admitted(t, 4194, rc.start(t, nil))
	base := OwnerCookie + "=" + cookie + "; __Host-x=1; tossed="
	at := func(n int) http.Header {
		h := own(id, cookie)
		h.Set("Cookie", base+strings.Repeat("a", n-len(base)))
		return h
	}
	if a := e.do(t, "GET", pHost(id), "/", at(8192), ""); a.status != http.StatusOK {
		t.Fatalf("an 8192-byte Cookie header: HTTP %d %s; want proxied", a.status, a.header.Get(ErrorCodeHeader))
	}
	before := rc.n.Load()
	a := e.do(t, "GET", pHost(id), "/", at(8193), "")
	if a.status != http.StatusRequestHeaderFieldsTooLarge || a.header.Get(ErrorCodeHeader) != codeTooManyCookies {
		t.Fatalf("an 8193-byte Cookie header: HTTP %d %q; want 431 %s", a.status, a.header.Get(ErrorCodeHeader), codeTooManyCookies)
	}
	want := []string{
		OwnerCookie + "=; Path=/; Max-Age=0; Secure",
		"__Host-x=; Path=/; Max-Age=0; Secure",
		"tossed=; Path=/; Max-Age=0; Secure",
		"tossed=; Path=/; Domain=" + testDomain + "; Max-Age=0; Secure",
	}
	if got := a.header.Values("Set-Cookie"); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the reset page's Set-Cookie:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if n := rc.n.Load() - before; n != 0 {
		t.Fatalf("the app saw %d requests past the cookie bound", n)
	}
}
