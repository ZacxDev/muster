package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// THE WEB APP'S SERVER SWITCHER, the server half
// (docs/design/web-server-switcher.md): another muster's page may READ this
// server's instance document and brand images, and this server's pages may
// fetch exactly those two paths on another origin.

// TestTheInstanceDocumentAndBrandImagesAreReadableFromAnyOrigin: GET
// /api/instance and every /brand/ image answer Access-Control-Allow-Origin "*"
// — with no logo (the 404) and with one, on a 200 and on a 304 — and never
// Access-Control-Allow-Credentials, so a credentialed read stays unreadable.
//
// 🔴 REGRESSION: on main neither route sends the header, and the switcher on
// another origin cannot read a server's name or logo.
func TestTheInstanceDocumentAndBrandImagesAreReadableFromAnyOrigin(t *testing.T) {
	_, h, mem, cookie := brandingServer(t)
	check := func(what string, hd http.Header) {
		t.Helper()
		if got := hd.Values("Access-Control-Allow-Origin"); len(got) != 1 || got[0] != "*" {
			t.Errorf("%s: Access-Control-Allow-Origin %q, want exactly [*]", what, got)
		}
		if got := hd.Get("Access-Control-Allow-Credentials"); got != "" {
			t.Errorf("%s: Access-Control-Allow-Credentials %q, want none", what, got)
		}
	}
	paths := []string{BrandLogoPath, BrandIcon192Path, BrandIcon512Path}
	check("instance, nothing set", get(h, InstancePath, nil, false).Header())
	for _, p := range paths {
		rec := get(h, p, nil, false)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s with no logo: %d (fixture)", p, rec.Code)
		}
		check(p+" 404", rec.Header())
	}
	if rec := putLogo(t, h, who{cookie: cookie}, "Team Example", testLogoPNG(t, 24, 12)); rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	check("instance, logo set", get(h, InstancePath, nil, false).Header())
	for _, p := range paths {
		rec := get(h, p+"?v="+mem.b.ETag, nil, false)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", p, rec.Code)
		}
		check(p+" 200", rec.Header())
		req, _ := http.NewRequest(http.MethodGet, p, nil)
		req.Header.Set("If-None-Match", `"`+mem.b.ETag+`"`)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotModified {
			t.Fatalf("%s If-None-Match: %d", p, rr.Code)
		}
		check(p+" 304", rr.Header())
	}
}

// TestOnlyThePublicBrandRoutesAreOpenToOtherOrigins: the header is the two
// public routes' own, not the middleware's — a signed-in page, an API route
// and the manifest carry none.
func TestOnlyThePublicBrandRoutesAreOpenToOtherOrigins(t *testing.T) {
	_, h, _, cookie := brandingServer(t)
	for _, p := range []string{"/tasks", "/api/tasks", "/manifest.webmanifest", "/account"} {
		if got := get(h, p, cookie, p == "/tasks" || p == "/account").Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("%s: Access-Control-Allow-Origin %q, want none", p, got)
		}
	}
}

// TestThePagesMayFetchOnlyTheSwitchersPathsOnOtherOrigins: the CSP's
// connect-src is 'self' plus, on any https origin (and the two loopback http
// hosts), /api/instance exactly and the /brand/ prefix — and nothing else: no
// bare scheme source (https:), no "*", no other path.
func TestThePagesMayFetchOnlyTheSwitchersPathsOnOtherOrigins(t *testing.T) {
	csp := contentSecurityPolicy("'none'", "")
	var connect string
	for _, d := range strings.Split(csp, ";") {
		if d = strings.TrimSpace(d); strings.HasPrefix(d, "connect-src ") {
			connect = d
		}
	}
	want := []string{"connect-src", "'self'",
		"https://*:*/api/instance", "https://*:*/brand/",
		"http://localhost:*/api/instance", "http://localhost:*/brand/",
		"http://127.0.0.1:*/api/instance", "http://127.0.0.1:*/brand/"}
	if got := strings.Fields(connect); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("connect-src\n %q\nwant\n %q", got, want)
	}
}
