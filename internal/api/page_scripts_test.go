package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// THE SHELL DOCUMENTS SEND THEIR CODE BY REFERENCE (internal/ui
// static_script.go): GET / is small, and each script it runs is a URL the
// browser keeps for a year.

// The inline-script budget is internal/ui's TestTheShellCarriesNoInlineScriptOverTheBudget
// (it renders the board-drawn shell, the case that still carries inline code);
// every link's ?v= is checked against its file by TestEveryStaticLinkCarriesItsContentHash.

var (
	pageScriptSrc  = regexp.MustCompile(`src="(/static/js/[^"?]+)\?v=([0-9a-f]+)"`)
	immutableCache = "public, max-age=31536000, immutable"
)

// TestThePagesScriptsAreServedImmutableUnderTheirContentHash: every script
// GET /tasks (the shell; `/` opens the chief's chat now) links is served as
// JavaScript, and only the URL the shell links it at is cached for a year.
func TestThePagesScriptsAreServedImmutableUnderTheirContentHash(t *testing.T) {
	s, h := pinServer(t, &fakeMirror{online: true, windows: pinWindows()}, &memPins{})
	doc := getOwed(t, s, h, "/tasks").Body.String()
	links := pageScriptSrc.FindAllStringSubmatch(doc, -1)
	if len(links) < 10 {
		t.Fatalf("CONTROL: GET /tasks links %d page scripts", len(links))
	}
	get := func(url string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		return rec
	}
	for _, m := range links {
		path, v := m[1], m[2]
		rec := get(path + "?v=" + v)
		if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Errorf("GET %s?v=%s → %d (%d bytes)", path, v, rec.Code, rec.Body.Len())
			continue
		}
		// The SERVED body, hashed here: what the worker's namesItsBytes checks
		// (checkVersioned hashes web.Files, not the response).
		if sum := sha256.Sum256(rec.Body.Bytes()); hex.EncodeToString(sum[:])[:12] != v {
			t.Errorf("%s is linked at ?v=%s but the served bytes hash to %x", path, v, sum[:6])
		}
		if cc := rec.Header().Get("Cache-Control"); cc != immutableCache {
			t.Errorf("GET %s?v=%s: Cache-Control %q, want %q", path, v, cc, immutableCache)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
			t.Errorf("GET %s: Content-Type %q, want text/javascript", path, ct)
		}
		// NEGATIVE CONTROLS: the bare path and another hash are revalidated.
		for _, other := range []string{path, path + "?v=000000000000"} {
			if cc := get(other).Header().Get("Cache-Control"); cc == immutableCache {
				t.Errorf("GET %s is immutable too; only the URL naming the current bytes may be", other)
			}
		}
	}
}

// TestAMissingStaticFileIsNeverCached: a 404 under /static/ says no-store. A
// cache in front (an edge) that kept a script's 404 for minutes would stretch
// the seconds two builds overlap in a rollout into an app with no code.
// Without an explicit header Go's file server sends none (it drops the
// handler's on its error path). The present file and the directory listing
// are the controls: neither may come back no-store.
func TestAMissingStaticFileIsNeverCached(t *testing.T) {
	_, h := pinServer(t, &fakeMirror{online: true, windows: pinWindows()}, &memPins{})
	for _, p := range []string{"/static/js/definitely-not-here.js", "/static/js/definitely-not-here.js?v=0123456789ab", "/static/nope.css"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s → %d, want 404", p, rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("GET %s → 404 with Cache-Control %q, want no-store", p, cc)
		}
	}
	for _, p := range []string{"/static/app.css", "/static/"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("CONTROL: GET %s → %d, %q, want 200 no-cache", p, rec.Code, rec.Header().Get("Cache-Control"))
		}
	}
}

// TestTheWorkerPrecachesEveryScriptTheShellLinks: offline, the shell document
// comes from the worker's cache — so must every script it runs, under the URL
// it links them by, or the offline shell opens inert.
func TestTheWorkerPrecachesEveryScriptTheShellLinks(t *testing.T) {
	sw, err := ServiceWorkerSource()
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`const SHELL_JS = (\[[^\]]*\]);`).FindSubmatch(sw)
	if m == nil {
		t.Fatal("sw.js as served has no SHELL_JS array")
	}
	s, h := pinServer(t, &fakeMirror{online: true, windows: pinWindows()}, &memPins{})
	// The worker installs from the board-less "/" (offlineShellParam).
	doc := getOwed(t, s, h, "/?"+offlineShellParam+"=1").Body.String()
	links := pageScriptSrc.FindAllStringSubmatch(doc, -1)
	if len(links) < 10 {
		t.Fatalf("CONTROL: the offline shell links %d page scripts", len(links))
	}
	for _, l := range links {
		if url := l[1] + "?v=" + l[2]; !strings.Contains(string(m[1]), `"`+url+`"`) {
			t.Errorf("the offline shell links %s but the worker does not precache it", url)
		}
	}
	if !strings.Contains(string(sw), "const SHELL = ['/', APP_CSS, '/manifest.webmanifest'].concat(SHELL_JS);") {
		t.Error("SHELL_JS is not part of the precached SHELL")
	}
}
