package pages_test

import (
	"encoding/json"
	"regexp"
	"testing"

	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/ui"
)

// dot is a 1x1 PNG, base64. The renderer does not sniff a data: image's
// bytes (the frame's browser decodes them), so each type's probe carries it.
const dot = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

// codeOpen is the renderer's inline code tag.
const codeOpen = `<code class="rounded bg-bg/60 px-1 py-0.5 font-mono text-[0.85em]">`

// blocksOf is the page's block markup, as the content document carries it
// (the SDK's BLOCKS string).
func blocksOf(t *testing.T, doc string) string {
	t.Helper()
	m := regexp.MustCompile(`BLOCKS=("(?:[^"\\]|\\.)*")`).FindStringSubmatch(doc)
	if m == nil {
		t.Fatalf("no BLOCKS in the content document")
	}
	var out string
	if err := json.Unmarshal([]byte(m[1]), &out); err != nil {
		t.Fatalf("BLOCKS is not a JSON string: %v", err)
	}
	return out
}

// TestAMarkdownDataImageIsDrawnOnlyWhenItIsARasterImage: a page's markdown
// block draws ![alt](data:image/png|jpeg|webp|gif;base64,...) as an <img>
// (a page an agent published on a server with uploads off was a wall of
// base64 text without it), and nothing else that looks like one: an SVG, any
// other media type, base64 that does not decode, and every attempt to leave
// the src or alt attribute stay the renderer's escaped text. Inside a code
// span the syntax is shown as written. Each case pins the WHOLE block.
func TestAMarkdownDataImageIsDrawnOnlyWhenItIsARasterImage(t *testing.T) {
	const open, close = `<section class="mp-block mp-markdown"><p class="my-1">`, `</p></section>` + "\n"
	for _, c := range []struct{ name, md, want string }{
		{"png", "![a dot](data:image/png;base64," + dot + ")", `<img src="data:image/png;base64,` + dot + `" alt="a dot">`},
		{"jpeg", "![j](data:image/jpeg;base64," + dot + ")", `<img src="data:image/jpeg;base64,` + dot + `" alt="j">`},
		{"webp", "![w](data:image/webp;base64," + dot + ")", `<img src="data:image/webp;base64,` + dot + `" alt="w">`},
		{"gif", "![g](data:image/gif;base64," + dot + ")", `<img src="data:image/gif;base64,` + dot + `" alt="g">`},
		{"in a sentence", "see ![d](data:image/png;base64,AAAA) here", `see <img src="data:image/png;base64,AAAA" alt="d"> here`},
		{"svg is text", "![s](data:image/svg+xml;base64,PHN2Zz4=)", `![s](data:image/svg+xml;base64,PHN2Zz4=)`},
		{"text/html is text", "![h](data:text/html;base64,PGI+)", `![h](data:text/html;base64,PGI+)`},
		{"not base64 is text", "![m](data:image/png;base64,AAAAA)", `![m](data:image/png;base64,AAAAA)`},
		{"not base64 encoded is text", "![m](data:image/png,rawbytes)", `![m](data:image/png,rawbytes)`},
		{"a quote cannot leave src", `![q](data:image/png;base64,AAAA" onerror="alert(1))`,
			`![q](data:image/png;base64,AAAA&quot; onerror=&quot;alert(1))`},
		{"a paren ends the image, the rest is text", "![p](data:image/png;base64,AAAA)onerror=alert(1)//)",
			`<img src="data:image/png;base64,AAAA" alt="p">onerror=alert(1)//)`},
		{"a quote cannot leave alt", `![" onload="x](data:image/png;base64,AAAA)`,
			`<img src="data:image/png;base64,AAAA" alt="&#34; onload=&#34;x">`},
		{"a tag in alt is text", `![<b>x</b>](data:image/png;base64,AAAA)`,
			`<img src="data:image/png;base64,AAAA" alt="&lt;b&gt;x&lt;/b&gt;">`},
		{"in a code span it is code", "`![c](data:image/png;base64,AAAA)`", codeOpen + `![c](data:image/png;base64,AAAA)</code>`},
	} {
		got := blocksOf(t, contentDoc(t, c.md))
		if want := open + c.want + close; got != want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, want)
		}
	}
}

// TestAMarkdownDataImageInACodeBlockIsCode: a fenced block shows the syntax
// as written, escaped, and draws nothing.
func TestAMarkdownDataImageInACodeBlockIsCode(t *testing.T) {
	got := blocksOf(t, contentDoc(t, "```\n![c](data:image/png;base64,AAAA)\n```"))
	want := `<section class="mp-block mp-markdown"><details class="group/code my-1 rounded-lg bg-bg/60 ring-1 ring-inset ring-line">` +
		`<summary class="flex cursor-pointer list-none items-center gap-1.5 px-2 py-1 text-xs font-medium text-muted transition hover:text-fg marker:content-['']">` +
		`<span class="transition group-open/code:rotate-90">▸</span>code</summary>` +
		`<pre class="overflow-auto rounded-b-lg px-2 pb-2 font-mono text-[0.85em] leading-relaxed"><code>![c](data:image/png;base64,AAAA)</code></pre></details></section>` + "\n"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// TestTaskAndChatMarkdownDrawNoDataImage is an INVARIANT GUARD, not
// regression coverage: the data: image is the PAGE's (markdownWithAssets),
// and ui.MarkdownHTML — which task bodies, comments and chat messages share —
// still draws the same input as the text it always was.
func TestTaskAndChatMarkdownDrawNoDataImage(t *testing.T) {
	md := "![a dot](data:image/png;base64,AAAA)"
	if got, want := ui.MarkdownHTML(md), `<p class="my-1">![a dot](data:image/png;base64,AAAA)</p>`; got != want {
		t.Errorf("ui.MarkdownHTML:\n got %s\nwant %s", got, want)
	}
}

// TestMarkdownImageDrawnIsTheRendererRule pins the predicate the phone check
// reads (ui.PagePhoneNotes) to what is drawn above.
func TestMarkdownImageDrawnIsTheRendererRule(t *testing.T) {
	for m, want := range map[string]bool{
		"![a](asset:AbCdEfGhIjKlMnOpQrStUv)":       true,
		"![a](data:image/png;base64," + dot + ")":  true,
		"![a](data:image/gif;base64,AAAA)":         true,
		"![a](data:image/svg+xml;base64,PHN2Zz4=)": false,
		"![a](data:image/png;base64,AAAAA)":        false,
		"![a](https://example.com/x.png)":          false,
		"![a](asset:short)":                        false,
		"x ![a](data:image/png;base64,AAAA)":       false, // one image, whole
		"![a](data:image/png;base64,AAAA)x":        false,
		"![a](data:image/jpeg;base64,AAAA)":        true,
		"![a](data:image/webp;base64,AAAA)":        true,
		"![a](data:image/bmp;base64,AAAA)":         false,
		"![a](data:image/png;base64,AAAA\" x=\"y)": false,
		"![](data:x)": false,
	} {
		if got := pages.MarkdownImageDrawn(m); got != want {
			t.Errorf("MarkdownImageDrawn(%q) = %v, want %v", m, got, want)
		}
	}
}
