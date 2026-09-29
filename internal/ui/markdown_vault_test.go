package ui

import (
	"strings"
	"testing"
	"time"
)

// Regression tests for the inline-substitution ordering bug: every inline pass
// used to run over the OUTPUT of the previous ones, so the renderer's own markup
// (href, target="_blank", the code-span class) was re-parsed as markdown.
//
// These are written against markdownHTML (the real entry point), not the internal
// helpers, so they keep holding if the implementation changes again.

func TestTwoLinksInOneParagraphKeepTheirAttributes(t *testing.T) {
	// The headline bug, and entirely independent of any producer: the `_` in the
	// FIRST anchor's target="_blank" paired with the `_` in the SECOND one's,
	// yielding target="<em>blank" … target="</em>blank" and destroying both.
	got := markdownHTML("[a](https://a.test/1) and [b](https://a.test/2)")

	if n := strings.Count(got, `target="_blank"`); n != 2 {
		t.Errorf("want 2 intact target=\"_blank\", got %d\n%s", n, got)
	}
	if strings.Contains(got, "<em>") {
		t.Errorf("emphasis injected into anchor attributes:\n%s", got)
	}
	for _, href := range []string{`href="https://a.test/1"`, `href="https://a.test/2"`} {
		if !strings.Contains(got, href) {
			t.Errorf("missing %s in:\n%s", href, got)
		}
	}
}

func TestUnderscoresInHrefAndLinkTextSurvive(t *testing.T) {
	// Two `_` inside one URL used to pair with each other; a single one paired with
	// target="_blank". Both the href and the link label must come through verbatim.
	cases := []struct{ name, src, wantHref, wantText string }{
		{
			"multi-param UTM",
			"Source: [a.test](https://a.test/p?utm_source=x&utm_medium=y) — News",
			`href="https://a.test/p?utm_source=x&amp;utm_medium=y"`,
			"a.test",
		},
		{
			"underscored host used as the label",
			"Source: [my_host.example.com](https://my_host.example.com/p)",
			`href="https://my_host.example.com/p"`,
			"my_host.example.com",
		},
		{
			"underscored path",
			"[en.wikipedia.org](https://en.wikipedia.org/wiki/List_of_Turing_Award_laureates)",
			`href="https://en.wikipedia.org/wiki/List_of_Turing_Award_laureates"`,
			"en.wikipedia.org",
		},
		{
			"asterisks in a query value",
			"[a.test](https://a.test/x?a=*b*)",
			`href="https://a.test/x?a=*b*"`,
			"a.test",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := markdownHTML(tc.src)
			if strings.Contains(got, "<em>") {
				t.Errorf("emphasis injected:\n%s", got)
			}
			if !strings.Contains(got, tc.wantHref) {
				t.Errorf("want href %s in:\n%s", tc.wantHref, got)
			}
			if !strings.Contains(got, ">"+tc.wantText+"</a>") {
				t.Errorf("want link text %q in:\n%s", tc.wantText, got)
			}
			if !strings.Contains(got, `target="_blank"`) {
				t.Errorf("target=\"_blank\" corrupted:\n%s", got)
			}
		})
	}
}

func TestCodeSpanContentStaysLiteral(t *testing.T) {
	// The old comment claimed "inline code first so emphasis inside a code span is
	// left literal". It was false — emphasis ran over the emitted <code> content.
	for _, src := range []string{"`a*b*c`", "`a_b_c`", "`**not bold**`"} {
		got := markdownHTML(src)
		if strings.Contains(got, "<em>") || strings.Contains(got, "<strong>") {
			t.Errorf("markup applied inside a code span for %q:\n%s", src, got)
		}
	}
}

func TestEmphasisInsideLinkTextStillWorks(t *testing.T) {
	// Long-standing behaviour, deliberately preserved. The WHOLE anchor is vaulted
	// (so emphasis cannot cross an anchor boundary); the label still italicizes
	// because mdLinkify runs mdEmphasis over it in isolation before stashing. Drop
	// that call and this test fails — which is the point of keeping it.
	got := markdownHTML("[*x*](https://a.test/)")
	if !strings.Contains(got, "<em>x</em></a>") {
		t.Errorf("emphasis inside link text was lost:\n%s", got)
	}
}

func TestAuthorTextCannotForgeAVaultPlaceholder(t *testing.T) {
	// The placeholder delimiter is NUL, which is stripped from the input, so author
	// text cannot fabricate a token and cause an out-of-range or injected restore.
	for _, src := range []string{
		"\x000\x00",
		"\x000\x00 [a](https://a.test/1)",
		"before \x0099\x00 after",
	} {
		got := markdownHTML(src)
		if strings.Contains(got, "\x00") {
			t.Errorf("NUL survived into the output for %q:\n%q", src, got)
		}
		if strings.Contains(got, "<a href") && !strings.Contains(src, "[a](") {
			t.Errorf("author text forged an anchor for %q:\n%s", src, got)
		}
	}
}

func TestVaultTokensAreUnambiguousPastTenItems(t *testing.T) {
	// Guards the NEW mechanism (it passes on the pre-vault renderer by construction,
	// since there are no tokens there): token 1 must not match inside token 11.
	// Interleaves code spans and links so both stash into the same index space and
	// cross the 1/11 boundary — an all-links version never gets there.
	var b strings.Builder
	const n = 60
	for i := 0; i < n; i++ {
		b.WriteString("`c` [l](https://a.test/x) ")
	}
	got := markdownHTML(b.String())

	if c := strings.Count(got, "<code"); c != n {
		t.Errorf("want %d code spans, got %d", n, c)
	}
	if a := strings.Count(got, `href="https://a.test/x"`); a != n {
		t.Errorf("want %d anchors, got %d", n, a)
	}
	if strings.Contains(got, "\x00") {
		t.Errorf("an unrestored placeholder leaked:\n%q", got)
	}
}

func TestTwoLinksWithUnderscoredLabelsDoNotCross(t *testing.T) {
	// The intersection the first version of this suite missed: two links AND
	// underscored labels. Vaulting only the opening tag left the labels exposed, so
	// the `_` in the first paired with the `_` in the second ACROSS the anchor
	// boundary, emitting crossing <em>/</a> — malformed HTML the browser reparses.
	got := markdownHTML("Source: [my_host.example.com](https://my_host.example.com/p) " +
		"and [other_host.example.com](https://other_host.example.com/q)")

	if strings.Contains(got, "<em>") {
		t.Errorf("emphasis crossed an anchor boundary:\n%s", got)
	}
	for _, want := range []string{
		">my_host.example.com</a>",
		">other_host.example.com</a>",
		`href="https://my_host.example.com/p"`,
		`href="https://other_host.example.com/q"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if c := strings.Count(got, `target="_blank"`); c != 2 {
		t.Errorf("want 2 intact target=\"_blank\", got %d", c)
	}
}

func TestVaultTokenCannotLeakIntoAnHref(t *testing.T) {
	// A code span inside a URL had its PLACEHOLDER captured by mdLink's url group.
	// Code spans stash at a lower index than the anchor containing them, so the
	// ascending restore had already passed by the time the anchor was restored: the
	// raw NUL token ended up inside the emitted href and the code text vanished.
	// mdLink now excludes NUL, so such input is not a MARKDOWN LINK — as it was
	// pre-vault.
	//
	// ⚠ THE ASSERTION MOVED FROM "no anchor at all" TO "no token in any href",
	// and the reason is a behaviour change rather than a weakening. Bare-URL
	// autolinking now runs after mdLinkify, so the `https://x.test/?q=` PREFIX —
	// the part before the code span, which is a perfectly ordinary bare URL — does
	// become an anchor. That is safe and is not what this test is about: the
	// hazard is a vault PLACEHOLDER reaching an href as a raw control byte. So the
	// check now reads every emitted href and looks in it, which is a strictly
	// tighter statement of the same property than counting anchors was.
	for _, src := range []string{
		"[a](https://x.test/?q=`y`z)",
		"see `cfg` then [a](https://x.test/p?k=`v`)",
	} {
		got := markdownHTML(src)
		if strings.Contains(got, "\x00") {
			t.Errorf("NUL leaked into the output for %q:\n%q", src, got)
		}
		for _, href := range hrefsIn(got) {
			if strings.ContainsAny(href, "\x00") {
				t.Errorf("a vault placeholder reached an href for %q: %q\n%s", src, href, got)
			}
		}
		// Still not a markdown link: the literal `[a](` survives, so the input
		// degraded to text-plus-autolink rather than being parsed as `[text](url)`.
		if strings.Contains(got, `>a</a>`) {
			t.Errorf("a URL containing a code span was parsed as a markdown link for %q:\n%s", src, got)
		}
		if !strings.Contains(got, "<code") {
			t.Errorf("the code span was swallowed for %q:\n%s", src, got)
		}
	}
}

// hrefsIn returns the value of every href attribute in an HTML fragment. Reading
// the ATTRIBUTE rather than grepping the whole document is what lets a test say
// "nothing forbidden reached an href" without also matching the same bytes in
// body text.
func hrefsIn(html string) []string {
	var out []string
	rest := html
	for {
		i := strings.Index(rest, `href="`)
		if i < 0 {
			return out
		}
		rest = rest[i+len(`href="`):]
		j := strings.Index(rest, `"`)
		if j < 0 {
			return out
		}
		out = append(out, rest[:j])
		rest = rest[j:]
	}
}

func TestRestoreIsSinglePassAtScale(t *testing.T) {
	// Complexity guard, not a benchmark. Restoring with one ReplaceAll per vaulted
	// item is quadratic, and markdownHTML batches a whole paragraph block into ONE
	// mdInline call, so the vault scales with the block. internal/ui/notes.go
	// renders every task's full body on the LIST page, so one such row would cost
	// every viewer tens of seconds of CPU per load.
	//
	// Measured on this input: ~0.2s single-pass, ~38s per-item. The 10s bound is a
	// ~50x margin over the good path and a ~4x margin under the bad one, so it
	// distinguishes the two on any plausible runner without being a flaky timing
	// assertion. A correctness-only check would NOT catch this — the quadratic
	// version returns the right HTML, just far too slowly.
	const n = 50000
	src := strings.Repeat("`a` ", n)

	start := time.Now()
	got := markdownHTML(src)
	elapsed := time.Since(start)

	if c := strings.Count(got, "<code"); c != n {
		t.Errorf("want %d code spans restored, got %d", n, c)
	}
	if strings.Contains(got, "\x00") {
		t.Error("an unrestored placeholder leaked at scale")
	}
	if elapsed > 10*time.Second {
		t.Errorf("rendering %d code spans took %v — restore looks quadratic again", n, elapsed)
	}
}

func TestEscapeFirstStillHolds(t *testing.T) {
	// The security property must be untouched by the vault: author text can never
	// emit raw tags, and a non-http(s) scheme never becomes an href.
	got := markdownHTML(`<script>alert(1)</script> [x](javascript:alert(1))`)
	if strings.Contains(got, "<script>") {
		t.Errorf("raw script tag emitted:\n%s", got)
	}
	if strings.Contains(got, "href=\"javascript:") {
		t.Errorf("javascript: URL became an href:\n%s", got)
	}
}
