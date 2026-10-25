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

// shellInlineScriptBudget is the most one inline <script> in a served shell
// document may hold; shellInlineScriptTotal, all of them together. What is
// still inline must run inside a boosted swap's own task (tasksRefilterGuard,
// about half a kilobyte); anything larger is code re-sent with every
// navigation.
const (
	shellInlineScriptBudget = 1024
	shellInlineScriptTotal  = 2048
)

var (
	servedScript   = regexp.MustCompile(`(?s)<script([^>]*)>(.*?)</script>`)
	pageScriptSrc  = regexp.MustCompile(`src="(/static/js/[^"?]+)\?v=([0-9a-f]+)"`)
	immutableCache = "public, max-age=31536000, immutable"
)

// TestServedShellDocumentsCarryNoInlineScriptOverTheBudget drives the real
// handler. Red before the scripts moved: GET / carried ~230 KB of inline
// JavaScript in 17 scripts, the largest ~72 KB.
func TestServedShellDocumentsCarryNoInlineScriptOverTheBudget(t *testing.T) {
	s, h := pinServer(t, &fakeMirror{online: true, windows: pinWindows()}, &memPins{})
	for _, path := range []string{"/", "/agents", "/tasks?new=1"} {
		doc := getOwed(t, s, h, path).Body.String()
		total, linked := 0, 0
		for _, m := range servedScript.FindAllStringSubmatch(doc, -1) {
			if strings.Contains(m[1], " src=") {
				if strings.Contains(m[1], `src="/static/js/`) {
					linked++
				}
				continue
			}
			n := len(m[2])
			total += n
			if n > shellInlineScriptBudget {
				head := strings.TrimSpace(m[2])
				if len(head) > 60 {
					head = head[:60]
				}
				t.Errorf("GET %s: an inline <script> of %d bytes (budget %d): %q…", path, n, shellInlineScriptBudget, head)
			}
		}
		if total > shellInlineScriptTotal {
			t.Errorf("GET %s: %d bytes of inline JavaScript (budget %d)", path, total, shellInlineScriptTotal)
		}
		// POSITIVE CONTROL: the code moved, it did not vanish.
		if linked < 10 {
			t.Errorf("GET %s links %d page scripts; a document that runs nothing meets any budget", path, linked)
		}
		t.Logf("GET %s: %d bytes, %d bytes inline JS, %d page scripts", path, len(doc), total, linked)
	}
}

// TestThePagesScriptsAreServedImmutableUnderTheirContentHash: every script
// GET / links is served, its ?v= is the SHA-256 of the bytes served (computed
// here, not asked of web.Asset), and only that URL is cached for a year.
func TestThePagesScriptsAreServedImmutableUnderTheirContentHash(t *testing.T) {
	s, h := pinServer(t, &fakeMirror{online: true, windows: pinWindows()}, &memPins{})
	doc := getOwed(t, s, h, "/").Body.String()
	links := pageScriptSrc.FindAllStringSubmatch(doc, -1)
	if len(links) < 10 {
		t.Fatalf("CONTROL: GET / links %d page scripts", len(links))
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
		sum := sha256.Sum256(rec.Body.Bytes())
		if got := hex.EncodeToString(sum[:])[:12]; got != v {
			t.Errorf("%s is linked at ?v=%s but the served bytes hash to %s", path, v, got)
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
