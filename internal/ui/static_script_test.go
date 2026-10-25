package ui

import (
	"bytes"
	"encoding/json"
	"html"
	"io/fs"
	"regexp"
	"strings"
	"testing"

	g "maragu.dev/gomponents"

	"github.com/ZacxDev/muster/web"
)

// staticScriptTag matches a <script> tag that loads a generated static script
// (static_script.go): its path, its ?v= hash, and every other attribute.
var staticScriptTag = regexp.MustCompile(`<script src="(/static/js/[A-Za-z0-9._-]+\.js)\?v=([0-9a-f]+)"([^>]*)></script>`)

// expandStaticScripts returns html with every generated static script's tag
// replaced by the inline <script> the browser in effect runs: the tag's other
// attributes, then the file's bytes as /static/ serves them (web.Files), then
// </script>. It is how this package's tests keep reading a document's
// JavaScript after the scripts moved out of the document — the shape they
// asserted on (an inline <script> body at the place the code runs) is exactly
// what a loaded tag amounts to.
//
// 🔴 IT RESOLVES THE URL, IT DOES NOT TRUST IT. A tag naming a path nothing
// registered, or a ?v= that is not the served file's content hash, fails the
// test: either is a document whose script the browser would never run (a 404)
// or would run stale (an old hash the cache still holds) — and inlining
// whatever the registry holds would hide both.
func expandStaticScripts(t testing.TB, html string) string {
	t.Helper()
	return staticScriptTag.ReplaceAllStringFunc(html, func(tag string) string {
		m := staticScriptTag.FindStringSubmatch(tag)
		path, v, attrs := m[1], m[2], m[3]
		b, err := fs.ReadFile(web.Files, strings.TrimPrefix(path, "/"))
		if err != nil {
			t.Fatalf("%s is linked but not served: %v", path, err)
		}
		if h := web.AssetHash(path); h != v {
			t.Fatalf("%s is linked at ?v=%s but the served file's hash is %s", path, v, h)
		}
		return "<script" + attrs + ">" + string(b) + "</script>"
	})
}

// renderRaw renders a node exactly as the server sends it — static scripts as
// tags, not expanded (renderString expands them).
func renderRaw(t testing.TB, n g.Node) string {
	t.Helper()
	var b bytes.Buffer
	if err := n.Render(&b); err != nil {
		t.Fatalf("render: %v", err)
	}
	return b.String()
}

// appNavValues returns the router registry appScript's tag hands its static
// code — TABS, HEADINGS and DEFAULT_TAB as the JSON text the browser parses
// (navRegistryJS, carried in appNavAttr) — after checking that the code does
// take each of the three from that attribute. The values are the browser's
// copy of the registry; the code check is what makes them so.
func appNavValues(t *testing.T, feat Features) (tabs, headings, def string) {
	t.Helper()
	tag := renderRaw(t, appScript(feat))
	m := regexp.MustCompile(` ` + appNavAttr + `="([^"]*)"`).FindStringSubmatch(tag)
	if m == nil {
		t.Fatalf("appScript's tag carries no %s: %s", appNavAttr, tag)
	}
	var nav struct {
		Tabs     json.RawMessage `json:"tabs"`
		Headings json.RawMessage `json:"headings"`
		Default  json.RawMessage `json:"default"`
	}
	if err := json.Unmarshal([]byte(html.UnescapeString(m[1])), &nav); err != nil {
		t.Fatalf("appScript's %s is not the registry's JSON: %v", appNavAttr, err)
	}
	js := mustStripJSComments(t, jsSource(t, appScript(feat)))
	for _, want := range []string{
		`var APP_TAG = document.currentScript;`,
		`var NAV = JSON.parse(APP_TAG.getAttribute('` + appNavAttr + `'));`,
		`var TABS = NAV.tabs;`,
		`var HEADINGS = NAV.headings;`,
		`var DEFAULT_TAB = NAV.default;`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("appScript's code does not read the router's registry from its tag: it lacks %q", want)
		}
	}
	return string(nav.Tabs), string(nav.Headings), string(nav.Default)
}
