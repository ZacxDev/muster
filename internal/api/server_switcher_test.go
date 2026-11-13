package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// THE WEB APP'S SERVER SWITCHER, the server half
// (docs/design/web-server-switcher.md). The operator's decision on review, "Logo
// via <img>, no CORS": no other origin may READ this server's instance
// document or brand images, and this server's pages read nothing on another
// origin; they may only DRAW another muster's /brand/icon-192.png.

// TestTheInstanceDocumentAndBrandImagesAreNotReadableFromOtherOrigins:
// GET /api/instance and every /brand/ image — 404 with no logo, 200 and 304
// with one — carry no Access-Control-Allow-Origin and no
// Access-Control-Allow-Credentials, so another origin's script cannot read
// them (an <img> can still draw an image: that needs no CORS).
func TestTheInstanceDocumentAndBrandImagesAreNotReadableFromOtherOrigins(t *testing.T) {
	_, h, mem, cookie := brandingServer(t)
	check := func(what string, hd http.Header) {
		t.Helper()
		for _, k := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Credentials"} {
			if got := hd.Values(k); len(got) != 0 {
				t.Errorf("%s: %s %q, want none", what, k, got)
			}
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
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.Header.Set("If-None-Match", `"`+mem.b.ETag+`"`)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotModified {
			t.Fatalf("%s If-None-Match: %d", p, rr.Code)
		}
		check(p+" 304", rr.Header())
	}
}

// TestTheSwitchersLogoPathIsServedUnversioned: the path the switcher draws
// (servers.js LOGO_PATH) with NO ?v= — it cannot know the etag without reading
// /api/instance — answers the current icon, revalidated (no-cache + ETag), and
// 404 with no logo, so the switcher's <img> falls back to colour and label.
func TestTheSwitchersLogoPathIsServedUnversioned(t *testing.T) {
	_, h, mem, cookie := brandingServer(t)
	if rec := get(h, "/brand/icon-192.png", nil, false); rec.Code != http.StatusNotFound {
		t.Fatalf("no logo: %d, want 404", rec.Code)
	}
	putLogo(t, h, who{cookie: cookie}, "", testLogoPNG(t, 24, 12))
	rec := get(h, "/brand/icon-192.png", nil, false)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" ||
		rec.Header().Get("Cache-Control") != "no-cache" || rec.Header().Get("ETag") != `"`+mem.b.ETag+`"` {
		t.Fatalf("unversioned icon: %d %v", rec.Code, rec.Header())
	}
}

// cspDirective is one directive of csp, as its fields ("" when absent).
func cspDirective(csp, name string) []string {
	for _, d := range strings.Split(csp, ";") {
		if f := strings.Fields(d); len(f) > 0 && f[0] == name {
			return f
		}
	}
	return nil
}

// TestThePagesReadNothingOnOtherOriginsAndDrawOnlyTheLogoPath: connect-src is
// main's ('self' alone), and img-src is main's ('self' data:) plus EXACTLY the
// switcher's logo path on any https origin and the two loopback http hosts —
// no bare scheme, no "*", no /brand/ prefix, no other path.
//
// 🔴 REGRESSION (against this PR's first revision, f191b0f8): connect-src
// carried /api/instance and /brand/ on any https origin.
func TestThePagesReadNothingOnOtherOriginsAndDrawOnlyTheLogoPath(t *testing.T) {
	for _, fa := range []string{"'none'", "'self'"} {
		csp := contentSecurityPolicy(fa, "")
		if got := strings.Join(cspDirective(csp, "connect-src"), " "); got != "connect-src 'self'" {
			t.Errorf("frame-ancestors %s: %q, want connect-src 'self'", fa, got)
		}
		want := "img-src 'self' data: https://*:*/brand/icon-192.png http://localhost:*/brand/icon-192.png http://127.0.0.1:*/brand/icon-192.png"
		if got := strings.Join(cspDirective(csp, "img-src"), " "); got != want {
			t.Errorf("frame-ancestors %s:\n %q\nwant\n %q", fa, got, want)
		}
	}
}
