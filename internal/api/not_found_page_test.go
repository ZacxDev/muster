package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// visibleText is a page's text as a person reads it: tags dropped, entities
// for an apostrophe undone, whitespace collapsed.
func visibleText(page string) string {
	page = regexp.MustCompile(`(?s)<head>.*</head>`).ReplaceAllString(page, "")
	page = regexp.MustCompile(`(?s)<script.*?</script>`).ReplaceAllString(page, "")
	page = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(page, " ")
	page = strings.NewReplacer("&#39;", "'", "&#x27;", "'").Replace(page)
	return strings.Join(strings.Fields(page), " ")
}

// TestABrowserNeverSeesGosBareNotFoundText (jargon audit F096): a page load
// of a path muster has no route for, and of a route whose handler calls
// http.NotFound (/auth/claim on a password-mode server), is answered with the
// not-found page — its whole text pinned — and never with
// "404 page not found". The status stays 404.
func TestABrowserNeverSeesGosBareNotFoundText(t *testing.T) {
	_, h, _, cookie := brandingServer(t)
	const want = "We couldn't find that page. The link may be old, or the page may have moved. Back to muster"
	for _, tc := range []struct {
		what, path string
		cookie     *http.Cookie
	}{
		{"no route, signed out", "/nonexistent", nil},
		{"no route, signed in", "/nonexistent", cookie},
		{"a handler's http.NotFound", "/auth/claim", nil},
	} {
		rec := get(h, tc.path, tc.cookie, true)
		body := rec.Body.String()
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", tc.what, rec.Code)
		}
		if strings.Contains(body, "404 page not found") {
			t.Errorf("%s: the bare Go text reached the page: %q", tc.what, body)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("%s: Content-Type %q, want text/html", tc.what, ct)
		}
		if got := visibleText(body); got != want {
			t.Errorf("%s: page text\n got %q\nwant %q", tc.what, got, want)
		}
		if !strings.Contains(body, `href="/"`) {
			t.Errorf("%s: no link back to muster's home", tc.what)
		}
	}
}

// TestNotFoundStaysPlainForAnythingButAPageLoad: an API client, a script or
// curl (no HTML in Accept) keeps the plain 404 text, a POST is never swapped,
// and a plain-text 404 whose body only STARTS with Go's text passes through
// byte for byte even to a browser.
// INVARIANT GUARD: these held before the not-found page existed; they pin
// that the swap is no broader than a browser's page load of Go's own text.
func TestNotFoundStaysPlainForAnythingButAPageLoad(t *testing.T) {
	_, h, _, _ := brandingServer(t)
	rec := get(h, "/nonexistent", nil, false)
	if rec.Code != http.StatusNotFound || rec.Body.String() != "404 page not found\n" {
		t.Errorf("non-browser GET: %d %q, want the plain 404", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodPost, "/nonexistent", nil)
	req.Header.Set("Accept", "text/html")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if strings.Contains(rr.Body.String(), NotFoundHeading) {
		t.Errorf("a POST got the page: %q", rr.Body.String())
	}

	s, _, _, _ := brandingServer(t)
	// Longer than Go's text (released while writing) and shorter (decided
	// at the end): both are someone else's words, kept as written.
	for _, body := range []string{"404 page not found\nand more", "404 no such run\n"} {
		own := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(body))
		})
		rr = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("Accept", "text/html")
		s.plainNotFound(own).ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound || rr.Body.String() != body {
			t.Errorf("another plain 404 was changed: %d %q, want %q", rr.Code, rr.Body.String(), body)
		}
	}
}
