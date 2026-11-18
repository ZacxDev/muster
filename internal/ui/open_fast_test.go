package ui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/web"
)

// THE OPEN'S CRITICAL PATH, as the documents spell it (e2e/tests/open-perf
// .spec.ts measures it in a browser).

// scriptSrc is a <script> tag's own src attribute (not a data-* one).
var scriptSrc = regexp.MustCompile(`(?:^|\s)src="([^"]*)"`)

// TestNoDocumentLoadsTheFaroBundlesInItsHead: with the browser telemetry ON,
// no document carries a <script src> for the vendored Faro SDK or its tracing
// bundle — they were parser-blocking in every head, ~170 KB read and parsed
// before the body — and the initializer's tag names both versioned URLs, so it
// can load them itself after the open. Red before this change: every document
// below linked both bundles in its head.
func TestNoDocumentLoadsTheFaroBundlesInItsHead(t *testing.T) {
	restore := withFaro(t)
	defer restore()
	docs := scriptedDocuments(t, Features{SnapUser: 7, SnapBuild: "b1"})
	docs["mirror view"] = renderRaw(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Window: "@3", StepUp: stepUpFixture()}))
	docs["terminal"] = renderRaw(t, AgentTerminalPage(AgentTerminalView{Name: "main", StepUp: stepUpFixture()}))
	sdk, tracing := web.Asset("/static/vendor/faro-web-sdk.iife.js"), web.Asset("/static/vendor/faro-web-tracing.iife.js")
	if !strings.Contains(sdk, "?v=") || !strings.Contains(tracing, "?v=") {
		t.Fatalf("CONTROL: the bundles are not versioned assets: %q %q", sdk, tracing)
	}
	for name, doc := range docs {
		inits := 0
		for _, m := range anyScript.FindAllStringSubmatch(doc, -1) {
			if src := scriptSrc.FindStringSubmatch(m[1]); src != nil && strings.HasPrefix(src[1], "/static/vendor/faro-") {
				t.Errorf("%s: a <script src> for a Faro bundle: %s", name, m[1])
			}
			if strings.Contains(m[1], `src="/static/js/faro-init.js`) {
				inits++
				for _, want := range []string{`data-faro-sdk="` + sdk + `"`, `data-faro-tracing-src="` + tracing + `"`} {
					if !strings.Contains(m[1], want) {
						t.Errorf("%s: the init's tag lacks %s: %s", name, want, m[1])
					}
				}
			}
		}
		if inits != 1 {
			t.Errorf("%s: %d faro-init.js tags, want 1", name, inits)
		}
	}
}

// TestTheRenderedViewReadsItsSnapshotWhileTheDocumentLoads: on the rendered
// view the snapshot store's tag is parser-BLOCKING (no defer) and comes before
// the view's own script, so the view starts its snapshot read — and its socket
// — while the document is still loading, not at DOMContentLoaded. Every other
// page keeps the deferred tag. Red before this change: the view's tag was
// deferred.
func TestTheRenderedViewReadsItsSnapshotWhileTheDocumentLoads(t *testing.T) {
	page := renderRaw(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Nav: NavChrome{Feat: Features{SnapUser: 9, SnapBuild: "v9"}}}))
	tag := regexp.MustCompile(`<script src="/static/mirror-snap\.js\?v=[0-9a-f]+"[^>]*>`).FindStringIndex(page)
	if tag == nil {
		t.Fatal("no snapshot store on the view")
	}
	if attrs := page[tag[0]:tag[1]]; regexp.MustCompile(`\sdefer[\s>=]`).MatchString(attrs) {
		t.Errorf("the view's snapshot store is deferred: %s", attrs)
	}
	view := strings.Index(page, "function whenParsed()")
	if view < 0 {
		t.Fatal("CONTROL: the view's script is not on the page")
	}
	if tag[0] > view {
		t.Error("the snapshot store comes after the view's script")
	}
	if strings.Index(page, `id="mv-log"`) > tag[0] {
		t.Error("the snapshot store comes before the log it reads")
	}
	if html := renderString(t, SnapScript(Features{SnapUser: 1, SnapBuild: "b"})); !strings.Contains(html, " defer") {
		t.Errorf("SnapScript (every other page) lost its defer: %s", html)
	}
}
