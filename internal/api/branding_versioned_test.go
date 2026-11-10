package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ZacxDev/muster/internal/branding"
)

// distinctiveETag is a logo ETag no upload in these tests produces, so an
// assertion naming it cannot pass on a value the server derived by itself.
const distinctiveETag = "e7a9c0ffee5b1d42"

// brandedWith is a branding server whose row (and cache) holds a real logo
// under etag.
func brandedWith(t *testing.T, etag string) (http.Handler, branding.Branding) {
	t.Helper()
	s, h, mem, cookie := brandingServer(t)
	if rec := putLogo(t, h, who{cookie: cookie}, "Team Example", testLogoPNG(t, 24, 12)); rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	mem.mu.Lock()
	mem.b.ETag = etag
	b := mem.b
	mem.mu.Unlock()
	s.brandingStored(b)
	return h, b
}

// TestTheInstanceDocumentLinksVersionedImages: the instance document's three
// image URLs carry ?v=<etag>, the same etag the document names, so a client
// fetching them gets a URL a CDN cannot answer with an older logo.
func TestTheInstanceDocumentLinksVersionedImages(t *testing.T) {
	h, _ := brandedWith(t, distinctiveETag)
	var doc InstanceDoc
	if err := json.Unmarshal(get(h, InstancePath, nil, false).Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Logo == nil {
		t.Fatal("logo is null")
	}
	if doc.Logo.ETag != distinctiveETag {
		t.Fatalf("etag %q, want %q", doc.Logo.ETag, distinctiveETag)
	}
	want := map[string]string{
		"url":     "/brand/logo.png?v=" + distinctiveETag,
		"icon192": "/brand/icon-192.png?v=" + distinctiveETag,
		"icon512": "/brand/icon-512.png?v=" + distinctiveETag,
	}
	got := map[string]string{"url": doc.Logo.URL, "icon192": doc.Logo.Icon192, "icon512": doc.Logo.Icon512}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
}

const (
	wantImmutable = "public, max-age=2592000, immutable"
	wantNoCache   = "no-cache"
)

// TestTheBrandImagesAreCachedByTheirURL: /brand/* is immutable for 30 days
// only at the URL that names the current logo (?v=<current etag>); with no v,
// or a stale one, it is the current image under no-cache, so a CDN or browser
// revalidates (and the ETag still answers 304). Each case is checked on a
// 200 and on a 304.
func TestTheBrandImagesAreCachedByTheirURL(t *testing.T) {
	h, b := brandedWith(t, distinctiveETag)
	stored := map[string][]byte{BrandLogoPath: b.Logo, BrandIcon192Path: b.Icon192, BrandIcon512Path: b.Icon512}
	cases := []struct {
		name, query, want string
	}{
		{"no v", "", wantNoCache},
		{"empty v", "?v=", wantNoCache},
		{"stale v", "?v=0123456789abcdef", wantNoCache},
		{"current v", "?v=" + distinctiveETag, wantImmutable},
	}
	for p, body := range stored {
		for _, c := range cases {
			rec := get(h, p+c.query, nil, false)
			if rec.Code != http.StatusOK || string(rec.Body.Bytes()) != string(body) {
				t.Errorf("%s %s: %d, want 200 and the current image", p, c.name, rec.Code)
			}
			if cc := rec.Header().Get("Cache-Control"); cc != c.want {
				t.Errorf("%s %s: Cache-Control %q, want %q", p, c.name, cc, c.want)
			}
			req := httptest.NewRequest(http.MethodGet, p+c.query, nil)
			req.Header.Set("If-None-Match", `"`+distinctiveETag+`"`)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusNotModified {
				t.Errorf("%s %s If-None-Match current: %d, want 304", p, c.name, rr.Code)
			}
			if cc := rr.Header().Get("Cache-Control"); cc != c.want {
				t.Errorf("%s %s 304: Cache-Control %q, want %q", p, c.name, cc, c.want)
			}
		}
	}
}
