package ui

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// askAnchor is the anchor every link in ask text is: mdAnchor's, the same
// attributes every rendered link carries (href already escaped).
func askAnchor(href, label string) string { return mdAnchor(href, label) }

// askHost is the real-host label drawn after a link whose words are not its
// address (linkHost).
func askHost(host string) string {
	return `<span class="` + AskLinkHostClass + `" data-ask-link-host>(` + host + `)</span>`
}

// TestAskTextNamesTheHostADeceptiveLinkOpens: a link whose words are not its
// address — words that are another address, words that are not one, an
// address with a user part — has the host it really opens after it; a bare
// URL (its words ARE its address) has none.
//
// RED at the previous head (5ec49bc): such a link was drawn as its words
// alone, so "[https://good.example](https://evil.example)" read as a link to
// good.example.
func TestAskTextNamesTheHostADeceptiveLinkOpens(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"[https://good.example](https://evil.example/x)",
			askAnchor("https://evil.example/x", "https://good.example") + askHost("evil.example")},
		{"https://good.example@evil.example/login",
			askAnchor("https://good.example@evil.example/login", "https://good.example@evil.example/login") + askHost("evil.example")},
		{"[sign in](https://good.example@EVIL.example/)",
			askAnchor("https://good.example@EVIL.example/", "sign in") + askHost("evil.example")},
		// Its words are its address: nothing more to say.
		{"https://good.example/path", askAnchor("https://good.example/path", "https://good.example/path")},
		{"[https://good.example/a](https://good.example/a)", askAnchor("https://good.example/a", "https://good.example/a")},
	} {
		if got := AskTextHTML(c.in); got != c.want {
			t.Errorf("%q:\n got  %s\nwant %s", c.in, got, c.want)
		}
	}
	// A host holding a hidden code point is no link at all (the hidden rule).
	if got := AskTextHTML("[ok](https://ev\u202eil.example/)"); strings.Contains(got, "<a ") || strings.Contains(got, "data-ask-link-host") {
		t.Errorf("a host with U+202E: %s", got)
	}
	// The chat's renderer draws no host label: ask text's alone.
	if got := markdownHTML("[x](https://evil.example)"); strings.Contains(got, "data-ask-link-host") {
		t.Errorf("the chat drew a host label: %s", got)
	}
}

// TestAskTextLinksABareURL: a bare http(s) URL in ask text is a link, with the
// attributes every rendered link has (mdAnchor: target, rel, class).
//
// RED before ask text was markdown: an ask's question was escaped text, the
// URL inert.
func TestAskTextLinksABareURL(t *testing.T) {
	got := AskTextHTML("Watch https://example.com/talk then answer.")
	want := "Watch " + askAnchor("https://example.com/talk", "https://example.com/talk") + " then answer."
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	got = AskTextHTML("Is [the clip](https://example.com/v) **really** it?")
	want = "Is " + askAnchor("https://example.com/v", "the clip") + askHost("example.com") + " <strong>really</strong> it?"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// TestAskTextLeavesOtherSchemesInert: javascript:, data: and vbscript: never
// become an anchor, in either link shape (mdHTTPScheme, and mdBareURL's own
// pattern), and their text is drawn as written.
func TestAskTextLeavesOtherSchemesInert(t *testing.T) {
	for _, in := range []string{
		"[tap](javascript:alert(1))",
		"[tap](JavaScript:alert(1))",
		"[tap](data:text/html,hi)",
		"[tap](vbscript:msgbox)",
		"javascript:alert(1)",
		"data:text/html,<b>x</b>",
		"vbscript:msgbox",
	} {
		got := AskTextHTML(in)
		if strings.Contains(got, "<a ") || strings.Contains(got, "href") {
			t.Errorf("%q became a link: %s", in, got)
		}
		if strings.Contains(got, "<b>") {
			t.Errorf("%q wrote markup: %s", in, got)
		}
	}
	// The positive control in the same harness: an https link IS one.
	if got := AskTextHTML("[tap](https://example.com)"); !strings.Contains(got, `<a href="https://example.com"`) {
		t.Fatalf("the control did not link: %s", got)
	}
}

// TestAskTextKeepsTheWatchTagVerbatim: a watch officer's machine tag at the
// start of a question is drawn exactly as written, even when its video id
// holds an `_` that markdown would otherwise open emphasis on.
//
// The fixture is chosen so the tag is NOT inert markdown: "-_abc" opens `_`
// emphasis (a `_` after a non-word character) and "this_" closes it, so
// without the tag's protection the output is "[watch:-<em>abc#2] … this</em>".
func TestAskTextKeepsTheWatchTagVerbatim(t *testing.T) {
	for _, c := range []struct{ in, tag string }{
		{"[watch:-_abc#2] what about this_ part?", "[watch:-_abc#2]"},
		{"[watch:dQw4w9WgXcQ#7] does this allegation change your view? https://youtu.be/dQw4w9WgXcQ?t=120", "[watch:dQw4w9WgXcQ#7]"},
		{"[watch:a_b#12] _why_?", "[watch:a_b#12]"},
	} {
		got := AskTextHTML(c.in)
		if !strings.HasPrefix(got, c.tag+" ") {
			t.Errorf("%q: the tag is not drawn as written:\n%s", c.in, got)
		}
		// Its text, read back as a browser reads it, starts with the tag.
		if txt := htmlText(t, got); !strings.HasPrefix(txt, c.tag+" ") {
			t.Errorf("%q: its text reads %q", c.in, txt)
		}
	}
	// The rest of the question is still markdown.
	if got := AskTextHTML("[watch:a_b#12] _why_?"); !strings.Contains(got, "<em>why</em>") {
		t.Errorf("the question after the tag is not markdown: %s", got)
	}
}

// TestAskTextFlagsHiddenCodePoints: a hidden code point is its visible flag,
// never written raw — in plain text, in a link's text — and a URL holding one
// is NOT a link (its text stays text, the code point flagged).
func TestAskTextFlagsHiddenCodePoints(t *testing.T) {
	got := AskTextHTML("open https://example.com/a\u202eb now")
	if strings.Contains(got, "<a ") {
		t.Errorf("a URL holding U+202E became a link: %s", got)
	}
	if !strings.Contains(got, `data-stepup-hidden="U+202E"`) || strings.ContainsRune(got, '\u202e') {
		t.Errorf("U+202E is not its flag: %s", got)
	}
	if txt := htmlText(t, got); txt != "open https://example.com/aU+202Eb now" {
		t.Errorf("its text reads %q", txt)
	}

	got = AskTextHTML("[see\u200bthis](https://example.com/s\u200bt)")
	if strings.Contains(got, "<a ") || strings.ContainsRune(got, '\u200b') {
		t.Errorf("a link whose href holds U+200B: %s", got)
	}

	got = AskTextHTML("[see\u200bthis](https://example.com/ok)")
	want := askAnchor("https://example.com/ok", "see"+hiddenFlag('\u200b')+"this") + askHost("example.com")
	if got != want {
		t.Errorf("a hidden code point in a link's text:\n got  %s\nwant %s", got, want)
	}
	// Control: the same URL without it IS a link.
	if got := AskTextHTML("open https://example.com/ab now"); !strings.Contains(got, "<a ") {
		t.Fatalf("the control did not link: %s", got)
	}
}

// TestAskTextKeepsItsLineBreaks: plain ask text reads as it did before it was
// markdown — its line breaks and blank lines real breaks in a pre-wrap box,
// no <p>, no <br> — and a list is a block of its own.
func TestAskTextKeepsItsLineBreaks(t *testing.T) {
	for in, want := range map[string]string{
		"Ship it?\nTonight or Monday": "Ship it?\nTonight or Monday",
		"one\n\ntwo\n":                "one\n\ntwo\n",
		"\nlead":                      "\nlead",
		"a\r\nb\rc":                   "a\nb\nc",
		"tab\there":                   "tab here",
		"Pick:\n- one\n- two\nthanks": `Pick:<ul class="my-1 list-disc space-y-0.5 pl-5"><li>one</li><li>two</li></ul>thanks`,
	} {
		if got := AskTextHTML(in); got != want {
			t.Errorf("%q:\n got  %q\nwant %q", in, got, want)
		}
	}
}

// TestAskTextClipRange: a YouTube link carrying a start and/or an end draws
// its range after the anchor, small and muted; an unreadable value, an end not
// after its start, and any other link draw none.
func TestAskTextClipRange(t *testing.T) {
	clip := func(s string) string { return `<span class="` + AskClipClass + `" data-ask-clip>` + s + `</span>` }
	cases := []struct{ url, want string }{
		{"https://youtu.be/abc?t=120", "2:00"},
		{"https://youtu.be/abc?t=1m30s", "1:30"},
		{"https://youtu.be/abc?t=90s", "1:30"},
		{"https://youtu.be/abc?t=7", "0:07"},
		{"https://www.youtube.com/watch?v=abc&t=3725", "1:02:05"},
		{"https://youtube.com/watch?v=abc&start=120&end=165", "2:00–2:45"},
		{"https://m.youtube.com/watch?v=abc&t=1h2m3s", "1:02:03"},
		{"https://www.youtube.com/shorts/abc?start=5&end=1m", "0:05–1:00"},
		{"https://youtu.be/abc?end=45", "0:00–0:45"},
		// Malformed or out of order: no range.
		{"https://youtu.be/abc?t=abc", ""},
		{"https://youtu.be/abc?t=1x", ""},
		{"https://youtu.be/abc?t=-5", ""},
		{"https://youtu.be/abc?start=90&end=30", ""},
		{"https://youtu.be/abc?start=90&end=90", ""},
		{"https://youtu.be/abc?start=10&end=zz", ""},
		// No time at all.
		{"https://youtu.be/abc", ""},
		// Not a YouTube video link.
		{"https://example.com/watch?v=abc&t=120", ""},
		{"https://www.youtube.com/channel/x?t=120", ""},
		{"https://www.youtube.com/watch?t=120", ""},
		{"https://youtu.be/?t=120", ""},
		{"https://notyoutube.com/watch?v=abc&t=120", ""},
	}
	for _, c := range cases {
		got := AskTextHTML(c.url)
		href := mdEscape(c.url)
		want := askAnchor(href, href)
		if c.want != "" {
			want += clip(c.want)
		}
		if got != want {
			t.Errorf("%s:\n got  %s\nwant %s", c.url, got, want)
		}
	}
	// A markdown link's range is after its anchor too.
	got := AskTextHTML("[the clip](https://youtu.be/abc?start=120&end=165) — does it hold?")
	want := askAnchor("https://youtu.be/abc?start=120&amp;end=165", "the clip") + clip("2:00–2:45") + askHost("youtu.be") + " — does it hold?"
	if got != want {
		t.Errorf("markdown link:\n got  %s\nwant %s", got, want)
	}
}

// TestTheChatRendererIsUnchangedByAskText: the chat's markdown (markdownHTML,
// the one renderer) still wraps a paragraph in <p>, joins its lines with <br>
// and draws no clip range: ask text's differences are ask text's alone.
// An invariant guard (the chat never differed): it pins that the ask mode did
// not leak into the shared renderer's default.
func TestTheChatRendererIsUnchangedByAskText(t *testing.T) {
	got := markdownHTML("see https://youtu.be/abc?t=120\nnext")
	want := `<p class="my-1">see ` + mdAnchor("https://youtu.be/abc?t=120", "https://youtu.be/abc?t=120") + `<br>next</p>`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// TestTheChatAskCardDrawsAskText: the chief's question card in the chat draws
// its question — waiting, answered and closed — and its answer through ask
// text: a link is a link, a hidden code point its flag.
//
// RED before: the card drew g.Text(ask.Question), the URL inert.
func TestTheChatAskCardDrawsAskText(t *testing.T) {
	ask := OwnerAsk{Question: "Is this the moment? https://youtu.be/abc?t=120", Options: []OwnerAskOption{{Label: "Yes"}, {Label: "No"}}}
	link := askAnchor("https://youtu.be/abc?t=120", "https://youtu.be/abc?t=120")
	for _, c := range []struct {
		name string
		st   OwnerCardState
	}{
		{"waiting", OwnerCardState{AskID: 5, Status: OwnerCardOpen}},
		{"answered", OwnerCardState{AskID: 5, Status: OwnerCardAnswered, Answer: "see https://example.com/notes"}},
		{"closed", OwnerCardState{AskID: 5, Status: OwnerCardClosed, Closed: "expired"}},
	} {
		out := nodeString(storedOwnerAskCard("call-1", ask, c.st))
		q := askBetween(t, out, "data-owner-ask-question", "</div>")
		if !strings.Contains(q, link) {
			t.Errorf("%s: the question has no link:\n%s", c.name, q)
		}
		if c.name == "answered" {
			a := askBetween(t, out, "data-owner-ask-answer", "</div>")
			if !strings.Contains(a, askAnchor("https://example.com/notes", "https://example.com/notes")) {
				t.Errorf("answered: the answer has no link:\n%s", a)
			}
		}
	}
	hidden := nodeString(storedOwnerAskCard("call-2", OwnerAsk{Question: "ok\u202e?", Options: []OwnerAskOption{{Label: "a"}}}, OwnerCardState{Status: OwnerCardOpen}))
	if strings.ContainsRune(hidden, '\u202e') || !strings.Contains(hidden, `data-stepup-hidden="U+202E"`) {
		t.Errorf("a hidden code point in the chat card's question is not its flag")
	}
}

// askBetween is the part of s from the first mark to the next end after it.
func askBetween(t *testing.T, s, mark, end string) string {
	t.Helper()
	i := strings.Index(s, mark)
	if i < 0 {
		t.Fatalf("no %s in:\n%s", mark, s)
	}
	j := strings.Index(s[i:], end)
	if j < 0 {
		t.Fatalf("no %s after %s", end, mark)
	}
	return s[i : i+j]
}

// htmlText is frag's text as a browser reads it.
func htmlText(t *testing.T, frag string) string {
	t.Helper()
	root, err := html.Parse(strings.NewReader("<div>" + frag + "</div>"))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return b.String()
}
