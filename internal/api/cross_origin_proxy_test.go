package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
)

// TestTheCheckBehindAHostRewritingProxy: a proxy that rewrites Host hands
// muster an internal Host while the browser's Origin (and Referer) is the
// public one, MUSTER_PUBLIC_URL. With no Sec-Fetch-Site (an older browser, or
// plain http), that Origin is admitted only because the public origin is
// trusted; any other origin is refused.
func TestTheCheckBehindAHostRewritingProxy(t *testing.T) {
	s, h, seen := newPrincipalTestServer(t)
	s.ext.PublicURL = "https://Muster.Example.org"
	const internalHost = "muster-internal.svc:8080"
	drive := func(hdr headerCase) (reached bool, code int) {
		*seen = nil
		req := writeRequestFor("POST /tasks")
		req.Host = internalHost
		req.Header.Set("Cookie", sessionCookieFor(s))
		hdr.apply(req)
		aborted, code := serve(t, h, req)
		return aborted && len(*seen) == 1 && (*seen)[0].Via == authz.ViaSession, code
	}
	for _, c := range []struct {
		hdr   headerCase
		admit bool
	}{
		{headerCase{name: "public Origin", origin: "https://muster.example.org"}, true},
		{headerCase{name: "public Referer only", referer: "https://muster.example.org/tasks"}, true},
		{headerCase{name: "the internal Host as Origin", origin: "http://" + internalHost}, true},
		{headerCase{name: "public host, other scheme", origin: "http://muster.example.org"}, false},
		{headerCase{name: "public host, other scheme, Referer", referer: "http://muster.example.org/tasks"}, false},
		{headerCase{name: "a sibling Origin", origin: "https://meet.example.org"}, false},
		{headerCase{name: "a sibling Referer", referer: "https://meet.example.org/x"}, false},
	} {
		reached, code := drive(c.hdr)
		if reached != c.admit {
			t.Errorf("%s: reached the handler = %v (status %d); want %v", c.hdr.name, reached, code, c.admit)
		}
	}
}

// TestAPlainHTTPNativeFormIsAdmitted: on a plain-http, non-loopback
// deployment a browser sends no Sec-Fetch-Site (measured in Chromium). A
// document muster serves now carries Referrer-Policy: same-origin by default,
// so its native form POST carries muster's real Origin, and the check admits
// it. (Under the old no-referrer default it carried `Origin: null`: refused.)
func TestAPlainHTTPNativeFormIsAdmitted(t *testing.T) {
	s, h, seen := newPrincipalTestServer(t)
	const host = "192.0.2.10:30306"
	// The default policy, on a response no handler overrides, through the real
	// middleware chain.
	rec := httptest.NewRecorder()
	get := httptest.NewRequest(http.MethodGet, "/health", nil)
	get.Host = host
	s.Handler().ServeHTTP(rec, get)
	if got := rec.Header().Get("Referrer-Policy"); got != "same-origin" {
		t.Fatalf("the default Referrer-Policy is %q; want same-origin, or a native form POST carries Origin: null", got)
	}
	// The form's POST: no Sec-Fetch-Site, the page's real Origin.
	*seen = nil
	req := writeRequestFor("POST /tasks")
	req.Host = host
	req.Header.Set("Cookie", sessionCookieFor(s))
	req.Header.Set("Origin", "http://"+host)
	if aborted, code := serve(t, h, req); !aborted || len(*seen) != 1 {
		t.Fatalf("a plain-http native form POST with its real Origin: status %d, saw %v; want admitted", code, *seen)
	}
}
