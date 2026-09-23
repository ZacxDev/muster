package api

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/web"
)

// ---------------------------------------------------------------------------
// EVERY /static/ PATH THE UI EMITS MUST RESOLVE TO A FILE THAT SHIPS.
//
// 🔴 THIS CLOSES A SEAM WHOSE WHOLE CHARACTER IS SILENCE. A missing
// `<script src>` does not error the page: it renders, styled, and then does
// nothing — no htmx means every panel stays a skeleton because no hx-get ever
// fires, no idiomorph means every morph swap quietly degrades to a destructive
// one, no sse.js means the page is write-only. Nothing in a renderer can
// observe that, because the renderers emit markup and the markup is correct.
//
// 🔴 THE PATH SET IS DERIVED BY SCANNING internal/ui's SOURCES, NOT LISTED
// HERE, AND THAT IS THE WHOLE DESIGN. A hand-written list passes unchanged when
// a view starts loading a seventh asset — which is exactly the drift this
// extraction keeps finding. Deriving it means adding a new /static/ reference
// to any view fails this test until the file is vendored.
// ---------------------------------------------------------------------------

// staticRef matches a /static/... path in Go source, stopping at the closing
// quote. It deliberately requires an extension, so the bare prefix `/static/`
// (the ROUTE pattern, which resolves to a directory) is not mistaken for a file.
var staticRef = regexp.MustCompile(`/static/[A-Za-z0-9._/-]+\.[A-Za-z0-9]+`)

// emittedStaticPaths returns every distinct /static/… file path internal/ui's
// NON-TEST sources mention.
//
// ⚠ IT SCANS COMMENTS TOO, AND THAT IS THE SAFE DIRECTION. A path named only in
// a comment is a file this test will insist ships — a false demand, costing one
// vendored file. The opposite bias would let a real emission through. If a
// comment ever needs to name a path that must NOT ship, this is the function to
// narrow, and narrowing it means parsing Go rather than scanning text.
func emittedStaticPaths(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join("..", "ui")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read internal/ui: %v", err)
	}
	seen := map[string]bool{}
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		scanned++
		for _, m := range staticRef.FindAllString(string(b), -1) {
			seen[m] = true
		}
	}
	// 🔴 POSITIVE CONTROL ON THE SCANNER. A wrong directory, a suffix filter that
	// stopped matching, or a regexp that compiles but matches nothing each
	// produce an EMPTY set — and an empty set trivially satisfies every
	// assertion below. Both numbers are checked: the files reached, and the
	// paths found.
	if scanned < 10 {
		t.Fatalf("the scanner read only %d source file(s) from internal/ui; it is pointed "+
			"at the wrong place and a clean result below would mean nothing", scanned)
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	if len(out) < 5 {
		t.Fatalf("the scanner found only %d /static/ path(s) across %d files: %v\n"+
			"The shell alone loads more than that, so this is a broken pattern rather "+
			"than a lean UI.", len(out), scanned, out)
	}
	return out
}

// TestEveryStaticPathTheUIEmitsResolves is the closing condition the UI chunk's
// seam note named.
func TestEveryStaticPathTheUIEmitsResolves(t *testing.T) {
	wantEmbedded := emittedStaticPaths(t)

	sub, err := fs.Sub(web.Static, "static")
	if err != nil {
		t.Fatalf("the embedded filesystem has no 'static' directory: %v", err)
	}

	var missing []string
	for _, p := range wantEmbedded {
		rel := strings.TrimPrefix(p, "/static/")
		if _, err := fs.Stat(sub, rel); err != nil {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d of %d /static/ path(s) the UI emits do NOT exist in the embed:\n  %s\n\n"+
			"Each is a 404 the browser reports and the page does not: a missing script "+
			"renders a styled page that then does nothing. Vendor the file into "+
			"web/static/ — do not remove it from the view to make this pass.",
			len(missing), len(wantEmbedded), strings.Join(missing, "\n  "))
	}
}

// TestStaticRouteServesTheEmbeddedFiles is the BEHAVIOURAL half.
//
// 🔴 THE TEST ABOVE PROVES THE BYTES ARE IN THE BINARY; THIS ONE PROVES A
// REQUEST REACHES THEM. They are different claims and the gap between them is a
// real failure mode: a correct embed served under a wrong prefix, or a route
// registered on a pattern that does not match, 404s every asset while the embed
// test stays green.
func TestStaticRouteServesTheEmbeddedFiles(t *testing.T) {
	mux := http.NewServeMux()
	RegisterRoutes(mux)

	for _, p := range emittedStaticPaths(t) {
		rel := strings.TrimPrefix(p, "/static/")
		if _, err := fs.Stat(mustSub(t), rel); err != nil {
			continue // covered, and failed, by the test above
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s → %d, want 200. The file is embedded but the route does not "+
				"serve it — check the pattern and the StripPrefix in staticHandler.", p, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("GET %s → 200 with an EMPTY body", p)
		}
	}

	// 🔴 NEGATIVE CONTROL. A handler that answered 200 to everything would pass
	// every assertion above. Ask for something that is not there.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/vendor/definitely-not-here.js", nil))
	if rec.Code == http.StatusOK {
		t.Error("the static route answered 200 for a file that does not exist, so every " +
			"200 above is evidence about the handler rather than about the embed")
	}
}

func mustSub(t *testing.T) fs.FS {
	t.Helper()
	sub, err := fs.Sub(web.Static, "static")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	return sub
}

// TestRootScopedPWADocumentsAreServed covers the two files served from the
// document root rather than under /static/, because the service worker's scope
// is decided by the path it is fetched from.
func TestRootScopedPWADocumentsAreServed(t *testing.T) {
	mux := http.NewServeMux()
	RegisterRoutes(mux)

	for _, tc := range []struct {
		path     string
		wantType string
		wantBody string
	}{
		{"/sw.js", "text/javascript", "addEventListener"},
		{"/manifest.webmanifest", "application/manifest+json", `"icons"`},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s → %d, want 200", tc.path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, tc.wantType) {
			t.Errorf("GET %s → Content-Type %q, want a %s", tc.path, ct, tc.wantType)
		}
		if !strings.Contains(rec.Body.String(), tc.wantBody) {
			t.Errorf("GET %s body does not contain %q; it may be the wrong file", tc.path, tc.wantBody)
		}
	}

	// The worker must be allowed to claim the whole origin, or it controls only
	// pages under its own path — which is every page except the root.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sw.js", nil))
	if got := rec.Header().Get("Service-Worker-Allowed"); got != "/" {
		t.Errorf("GET /sw.js → Service-Worker-Allowed %q, want \"/\"", got)
	}
}

// TestEveryManifestIconShips closes the transitive half: the manifest is served
// from root and names four icons the scanner above cannot see, because no Go
// source mentions them.
//
// 🔴 THE PATHS ARE READ OUT OF THE MANIFEST ITSELF, NOT LISTED HERE. A fifth
// icon added to the manifest is then covered automatically; a hardcoded list
// would not be.
func TestEveryManifestIconShips(t *testing.T) {
	b, err := web.Static.ReadFile("static/manifest.webmanifest")
	if err != nil {
		t.Fatalf("read the manifest: %v", err)
	}
	refs := staticRef.FindAllString(string(b), -1)
	if len(refs) < 2 {
		t.Fatalf("found only %d icon reference(s) in the manifest; the pattern is not "+
			"matching and a clean result would be meaningless", len(refs))
	}
	sub := mustSub(t)
	for _, p := range refs {
		rel := strings.TrimPrefix(p, "/static/")
		if _, err := fs.Stat(sub, rel); err != nil {
			t.Errorf("the manifest names %s and it is not in the embed: %v\n"+
				"An installed PWA with a missing icon falls back to a screenshot of the page.", p, err)
		}
	}
}

// Keep the regexp import meaningful if this file is ever trimmed.
var _ = regexp.MustCompile
