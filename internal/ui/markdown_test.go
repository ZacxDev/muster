package ui

import (
	"strings"
	"testing"
)

// TestMarkdownHTMLElements asserts each supported markdown element renders to the
// expected safe HTML. The renderer is escape-FIRST, so every case also implicitly
// guards that author text cannot inject tags.
func TestMarkdownHTMLElements(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string // substrings that must appear
		deny []string // substrings that must NOT appear
	}{
		{
			name: "bold",
			in:   "this is **bold** text",
			want: []string{"<strong>bold</strong>"},
			deny: []string{"**bold**"},
		},
		{
			name: "italic star",
			in:   "this is *em* text",
			want: []string{"<em>em</em>"},
		},
		{
			name: "italic underscore",
			in:   "this is _em_ text",
			want: []string{"<em>em</em>"},
		},
		{
			name: "inline code",
			in:   "call `foo()` now",
			want: []string{`<code class="rounded bg-slate-950/60 px-1 py-0.5 font-mono text-[0.85em]">foo()</code>`},
			deny: []string{"`foo()`"},
		},
		{
			name: "fenced json block renders as a collapsed <details>",
			in:   "before\n```json\n{\"a\": 1}\n```\nafter",
			// The fenced block is now a COLLAPSED disclosure (no `open` attr) with a
			// "json code" summary, so raw JSON never walls the card. The <pre><code>
			// content still lives inside, escaped.
			want: []string{`<details`, `<summary`, "json code", `<pre`, `<code>`, `{&quot;a&quot;: 1}`, "</code></pre>", "</details>", "before", "after"},
			deny: []string{"```", "<details open", `<details class="group/code my-1 rounded-lg bg-slate-950/60 ring-1 ring-inset ring-white/5" open`},
		},
		{
			name: "bare fenced block summary falls back to 'code'",
			in:   "```\nplain\n```",
			want: []string{`<details`, `<summary`, ">code<", "plain", "</details>"},
			deny: []string{"<details open"},
		},
		{
			name: "http link renders an anchor",
			in:   "see [Source ticket](https://example.com/t/1) now",
			want: []string{
				`<a href="https://example.com/t/1"`,
				`target="_blank"`,
				`rel="noopener"`,
				">Source ticket</a>",
			},
			deny: []string{"[Source ticket]", "](https://"},
		},
		{
			name: "javascript link is neutralized (no anchor; left as inert text)",
			in:   "[click](javascript:alert(1))",
			// Non-http scheme → NOT an anchor; the literal markdown text remains (inert,
			// escaped). The critical guarantee is no live <a href=...> is produced.
			want: []string{"[click]"},
			deny: []string{"<a ", "href="},
		},
		{
			name: "blockquote",
			in:   "> ⚠ safety: verify first",
			want: []string{"<blockquote", "⚠ safety: verify first", "</blockquote>"},
			deny: []string{"&gt; ⚠"}, // the leading '> ' marker is stripped, not escaped-literal
		},
		{
			name: "bullet list",
			in:   "- one\n- two\n- three",
			want: []string{"<ul", "<li>one</li>", "<li>two</li>", "<li>three</li>", "</ul>"},
		},
		{
			name: "ordered list",
			in:   "1. first\n2. second",
			want: []string{"<ol", "<li>first</li>", "<li>second</li>", "</ol>"},
		},
		{
			name: "heading",
			in:   "## A heading",
			want: []string{"font-semibold", "A heading"},
			deny: []string{"## A heading"},
		},
		{
			name: "paragraph with inline mix",
			in:   "plain **b** and `c` together",
			want: []string{"<p", "<strong>b</strong>", "<code", "</p>"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := markdownHTML(c.in)
			for _, w := range c.want {
				if !strings.Contains(out, w) {
					t.Errorf("markdownHTML(%q) missing %q\n---\n%s", c.in, w, out)
				}
			}
			for _, d := range c.deny {
				if strings.Contains(out, d) {
					t.Errorf("markdownHTML(%q) should NOT contain %q\n---\n%s", c.in, d, out)
				}
			}
		})
	}
}

// TestMarkdownLinks is the focused render + XSS test for `[text](url)` support
// (the drafter adds a "Source ticket" link). http(s) links become a safe new-tab
// anchor; every non-http(s) scheme is neutralized (left as inert literal text, no
// <a href> produced) so a `javascript:`/`data:`/`vbscript:` URL can never become a
// live link.
func TestMarkdownLinks(t *testing.T) {
	// http + https → anchor with target/rel and the link text.
	for _, ok := range []struct{ in, href, text string }{
		{"[Source ticket](https://civitai.com/t/42)", "https://civitai.com/t/42", "Source ticket"},
		{"[plain](http://example.com)", "http://example.com", "plain"},
		{"[caps](HTTPS://Example.com/X)", "HTTPS://Example.com/X", "caps"},
	} {
		out := markdownHTML(ok.in)
		if !strings.Contains(out, `<a href="`+ok.href+`"`) {
			t.Errorf("link %q: missing anchor href %q\n---\n%s", ok.in, ok.href, out)
		}
		if !strings.Contains(out, `target="_blank"`) || !strings.Contains(out, `rel="noopener"`) {
			t.Errorf("link %q: anchor must be a safe new-tab link (target=_blank rel=noopener)\n---\n%s", ok.in, out)
		}
		if !strings.Contains(out, ">"+ok.text+"</a>") {
			t.Errorf("link %q: missing link text %q\n---\n%s", ok.in, ok.text, out)
		}
	}

	// Dangerous / non-http schemes → NO anchor at all (no live href produced).
	for _, bad := range []string{
		"[x](javascript:alert(1))",
		"[x](JavaScript:alert(1))",
		"[x](data:text/html,<script>alert(1)</script>)",
		"[x](vbscript:msgbox(1))",
		"[x](/relative/path)",
		"[x](mailto:a@b.com)",
	} {
		out := markdownHTML(bad)
		if strings.Contains(out, "<a ") || strings.Contains(out, "href=") {
			t.Errorf("dangerous link %q produced an anchor — must be neutralized\n---\n%s", bad, out)
		}
	}

}

// TestMarkdownHTMLEscapesInjection is the security-critical test: NO author input
// can ever produce live HTML/JS. The renderer escapes BEFORE applying markup, so
// the only tags in the output are ones it emits itself.
func TestMarkdownHTMLEscapesInjection(t *testing.T) {
	cases := []string{
		`<script>alert(1)</script>`,
		`<img src=x onerror=alert(1)>`,
		`**<b>bold injection</b>**`,
		"`<i>code injection</i>`",
		"> <svg onload=alert(1)>",
		"```\n<script>evil()</script>\n```",
		`- <a href="javascript:alert(1)">x</a>`,
	}
	for _, in := range cases {
		out := markdownHTML(in)
		// No raw author-controlled OPENING tags should survive — all author '<' must
		// have been escaped to '&lt;'. We strip the renderer's OWN known-safe tags
		// first, then assert no '<' remains (which would mean an author tag leaked).
		stripped := out
		for _, ours := range []string{
			"<p ", "</p>", "<strong>", "</strong>", "<em>", "</em>", "<code ",
			"<code>", "</code>", "<pre ", "</pre>", "<ul ", "</ul>", "<ol ", "</ol>",
			"<li>", "</li>", "<blockquote ", "</blockquote>", "<div ", "</div>", "<br>",
			// Fenced blocks now render as a collapsed <details>/<summary> (with a
			// chevron <span>); links render as <a …>. All author-controlled.
			"<details ", "</details>", "<summary ", "</summary>", "<span ", "</span>",
			"<a ", "</a>",
		} {
			stripped = strings.ReplaceAll(stripped, ours, "")
		}
		if strings.Contains(stripped, "<") {
			t.Errorf("an author '<' tag leaked unescaped for input %q\n---\n%s\n--- after stripping our tags ---\n%s", in, out, stripped)
		}
		// The escaped form must be present (proof the content was kept, just neutered).
		if !strings.Contains(out, "&lt;") {
			t.Errorf("expected escaped < (&lt;) for input %q\n---\n%s", in, out)
		}
	}
}

// TestMarkdownSnippet asserts the collapsed-card preview strips markup to clean
// prose, drops fenced blocks, collapses whitespace, and truncates.
func TestMarkdownSnippet(t *testing.T) {
	in := "**🤖 task-drafter** · `id`\n> ⚠ verify first\n\n```json\n{\"goal\":\"\"}\n```\n- do a\n- do b"
	got := markdownSnippet(in, 200)
	for _, deny := range []string{"**", "`", ">", "```", "{", "- "} {
		if strings.Contains(got, deny) {
			t.Errorf("snippet should not contain markup %q: %q", deny, got)
		}
	}
	for _, want := range []string{"task-drafter", "verify first", "do a"} {
		if !strings.Contains(got, want) {
			t.Errorf("snippet missing %q: %q", want, got)
		}
	}
	// Truncation.
	long := strings.Repeat("word ", 100)
	if n := len([]rune(markdownSnippet(long, 30))); n > 31 { // 30 + ellipsis
		t.Errorf("snippet not truncated to ~30 runes: got %d", n)
	}
}
