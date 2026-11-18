package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

// staticScriptTag matches a <script> tag loading one of the pages' generated
// static scripts (internal/ui static_script.go): its versioned URL, and every
// other attribute it carries.
var staticScriptTag = regexp.MustCompile(`<script src="(/static/js/[A-Za-z0-9._-]+\.js\?v=[0-9a-f]+)"([^>]*)></script>`)

// expandStaticScripts returns html with every generated static script's tag
// replaced by an inline <script> holding the bytes GET <its URL> answers — the
// code the browser runs at that place in the document. A test that reads a
// page's JavaScript reads it through here, so it reads what is SERVED: the
// request goes through the real /static/ route, and anything but a 200 marked
// immutable fails the test (a 404 is a script the page never runs).
func expandStaticScripts(t *testing.T, html string) string {
	t.Helper()
	mux := http.NewServeMux()
	RegisterRoutes(mux)
	return staticScriptTag.ReplaceAllStringFunc(html, func(tag string) string {
		m := staticScriptTag.FindStringSubmatch(tag)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, m[1], nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s → %d: the page links a script that is not served", m[1], rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
			t.Fatalf("GET %s → Cache-Control %q: the URL a page links must be the current, immutable one", m[1], cc)
		}
		return "<script" + m[2] + ">" + rec.Body.String() + "</script>"
	})
}
