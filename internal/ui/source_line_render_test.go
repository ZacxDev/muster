package ui

import (
	"strings"
	"testing"
)

// Round-trip guard for the Brave capture extension's "Source: …" line.
//
// WHY THIS EXISTS, in Go rather than in the extension's own suite: the extension
// appends a source line built for THIS renderer, and its JS tests can only assert
// the string it emits — never the HTML that comes out. That blind spot let the
// same defect ship twice before anyone rendered the output.
//
// The extension used to percent-encode `_`/`*` in the URL to survive the renderer
// applying emphasis to its own emitted HTML. 0.7.77 fixed that at the source (see
// markdown_vault_test.go), so the encoding was removed and these cases assert the
// RAW URLs the extension now emits. If this test starts failing, the extension's
// source line and this renderer have diverged again — fix the renderer, not the
// extension: a client-side workaround for this class has been wrong three times.
func TestExtensionSourceLineRendersIntact(t *testing.T) {
	// Exactly what lib.js `formatSourceLine` emits, for realistic page URLs.
	lines := []struct {
		name string
		line string
		// wantHref is the href the anchor must carry (percent-encoded form).
		wantHref string
	}{
		{
			name:     "multi-param UTM (two underscores pair with each other)",
			line:     "Source: [a.test](https://a.test/p?utm_source=x&utm_medium=y) — News",
			wantHref: "https://a.test/p?utm_source=x&amp;utm_medium=y",
		},
		{
			name:     "wikipedia underscore path",
			line:     "Source: [en.wikipedia.org](https://en.wikipedia.org/wiki/List_of_Turing_Award_laureates)",
			wantHref: "https://en.wikipedia.org/wiki/List_of_Turing_Award_laureates",
		},
		{
			name:     "asterisks in a query value",
			line:     "Source: [a.test](https://a.test/x?a=*b*)",
			wantHref: "https://a.test/x?a=*b*",
		},
		{
			name:     "backtick in the path",
			line:     "Source: [a.test](https://a.test/back`tick)",
			wantHref: "https://a.test/back`tick",
		},
		{
			name:     "clean URL, unencoded",
			line:     "Source: [a.test](https://a.test/p?id=7&q=x) — Title",
			wantHref: "https://a.test/p?id=7&amp;q=x",
		},
	}

	for _, tc := range lines {
		t.Run(tc.name, func(t *testing.T) {
			got := markdownHTML(tc.line)

			// 1. No emphasis may be injected anywhere. This is the assertion the
			//    extension's JS tests structurally cannot make.
			if strings.Contains(got, "<em>") || strings.Contains(got, "</em>") {
				t.Errorf("emphasis injected into a source line:\n  in:  %s\n  out: %s", tc.line, got)
			}
			// 2. The anchor must survive with the exact intended href.
			wantAnchor := `href="` + tc.wantHref + `"`
			if !strings.Contains(got, wantAnchor) {
				t.Errorf("anchor href missing or altered:\n  want: %s\n  out:  %s", wantAnchor, got)
			}
			// 3. target="_blank" must be intact — its `_` is the thing an unencoded
			//    URL underscore used to pair with.
			if !strings.Contains(got, `target="_blank"`) {
				t.Errorf("target=\"_blank\" corrupted:\n  out: %s", got)
			}
		})
	}
}
