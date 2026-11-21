package api

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
)

// THE STEP-UP CARD'S POST PREVIEW (host_stepup_card.go), without a database:
// the logo per venue and its fallback, an X thread drawn part by part, the
// text's paragraphs and its neutralised characters, and Details' call in
// words. The door-to-every-face case is stepup_post_preview_pg_test.go.

// stepUpPostCard is a draft card on venue platform (target kind target) as
// Clean stores it.
//
// Its title is the one muster gives that card: "Post to <venue>? · #9" for an
// outbound draft, "Send this reply on <venue>? · #9" for a reply draft (no
// platform).
func stepUpPostCard(t *testing.T, platform, venue, target, handle, body string, media int) attention.Ask {
	t.Helper()
	title := "Post to " + venue + "? · #9"
	if platform == "" {
		title = "Send this reply on " + venue + "? · #9"
	}
	c := &attention.StepUpCard{Tool: pluginDraftDecideTool, Title: title, Why: "Marketing: launch", Asker: "main:1",
		Preview: body, Venue: venue, Platform: platform, Target: target, Handle: handle, DraftID: 9, Details: "D"}
	for i := range media {
		c.Media = append(c.Media, attention.StepUpMedium{N: i, Ref: "r/" + string(rune('a'+i)), Type: "image/png", SHA256: strings.Repeat("c", 64)})
	}
	return stepUpAsk(t, c)
}

// stepUpAsk is step-up card c as Clean stores it, on an open ask.
func stepUpAsk(t *testing.T, c *attention.StepUpCard) attention.Ask {
	t.Helper()
	n, err := attention.Clean(attention.New{Asker: attention.Asker{System: true, Label: hostDecisionLabel}, StepUp: c,
		Options: []string{hostDecisionConfirm, hostDecisionRefuse}})
	if err != nil {
		t.Fatal(err)
	}
	return attention.Ask{ID: 5, Asker: n.Asker, Question: n.Question, Options: n.Options, StepUp: n.StepUp, Status: attention.Open, CreatedAt: time.Now()}
}

// TestAStepUpPostPreviewHasItsVenuesLogo: X and Reddit drafts carry their own
// glyph on the page and the feed; a venue muster has no glyph for, and a reply
// draft (whose venue is its plugin's label), carry the neutral one — never
// another venue's.
func TestAStepUpPostPreviewHasItsVenuesLogo(t *testing.T) {
	cases := []struct{ platform, venue, target, logo, kind string }{
		{"x", "X", "post", "x", "New post"},
		{"x", "X", "reply", "x", "Reply"},
		{"reddit", "Reddit", "reply", "reddit", "Reply"},
		{"example.social", "Example.social", "post", "other", "New post"},
		{"", "Example inbox", "", "other", "Reply to a message"},
	}
	for _, c := range cases {
		a := stepUpPostCard(t, c.platform, c.venue, c.target, "", "Hello there.", 0)
		page, feed := askQuestionHTML(a), askFeedCardHTML(askItem(a), time.Now())
		for face, html := range map[string]string{"page": page, "feed": feed} {
			if n := strings.Count(html, `data-venue-logo="`); n != 1 || !strings.Contains(html, `data-venue-logo="`+c.logo+`"`) ||
				!strings.Contains(html, `data-logo="`+c.logo+`"`) {
				t.Errorf("%s %q: want exactly one %q logo (%d logos):\n%s", face, c.platform, c.logo, n, html)
			}
			if !strings.Contains(html, `data-stepup-venue>`+c.venue+`</span>`) || !strings.Contains(html, `data-stepup-post-kind>`+c.kind+`</span>`) {
				t.Errorf("%s %q: the venue %q or kind %q is not drawn:\n%s", face, c.platform, c.venue, c.kind, html)
			}
		}
	}
	// The glyphs are distinct shapes, drawn in currentColor.
	x, reddit, other := askQuestionHTML(stepUpPostCard(t, "x", "X", "post", "", "a", 0)),
		askQuestionHTML(stepUpPostCard(t, "reddit", "Reddit", "post", "", "a", 0)),
		askQuestionHTML(stepUpPostCard(t, "example.social", "E", "post", "", "a", 0))
	if !strings.Contains(x, `<path fill="currentColor" d="M18.244 2.25h3.308`) || !strings.Contains(reddit, `<ellipse cx="12" cy="14.5" rx="8.5" ry="5.8"/>`) ||
		!strings.Contains(other, `d="M4 5.5A2.5 2.5 0 0 1 6.5 3h11`) || strings.Contains(other, "M18.244") || strings.Contains(other, "ellipse") {
		t.Errorf("the glyphs:\nx: %s\nreddit: %s\nother: %s", x, reddit, other)
	}
	// A DM names who it reaches.
	if dm := askQuestionHTML(stepUpPostCard(t, "x", "X", "dm", "@someone", "Hi.", 0)); !strings.Contains(dm, `data-stepup-post-kind>Direct message to @someone</span>`) {
		t.Errorf("a DM's kind line:\n%s", dm)
	}
	if f := askQuestionHTML(stepUpPostCard(t, "x", "X", "follow", "someone", "Follow someone", 0)); !strings.Contains(f, `data-stepup-post-kind>Follow @someone</span>`) {
		t.Errorf("a follow's kind line:\n%s", f)
	}
}

// TestAnXThreadIsDrawnPartByPart: an X post whose body has ---8<--- lines is
// drawn as the thread it becomes — each part numbered, medium i under part
// i+1 — on the page; the feed shows the first part and how many follow.
func TestAnXThreadIsDrawnPartByPart(t *testing.T) {
	a := stepUpPostCard(t, "x", "X", "post", "", "First part.\nStill first.\n---8<---\nSecond part.", 2)
	page := askQuestionHTML(a)
	want := `<span class="truncate text-sm text-fg2" data-stepup-post-kind>Thread · 2 posts</span></span></header>
<div class="flex min-w-0 flex-col gap-3" data-stepup-part="1">
<p class="text-xs font-semibold text-fg2" data-stepup-part-n>1/2</p>
<div class="flex min-w-0 flex-col gap-3 text-[15px] leading-normal text-fg" data-stepup-preview-text><p class="whitespace-pre-wrap break-words">First part.
Still first.</p></div>
<div class="grid min-w-0 gap-0.5 overflow-hidden rounded-xl border border-line" data-stepup-media><img alt="Attached image 1" loading="lazy" class="block h-auto max-h-[32rem] w-full bg-s1 object-cover" src="/plugins/drafts/9/media/0" data-stepup-medium data-stepup-image></div>
</div>
<div class="flex min-w-0 flex-col gap-3 border-t border-line pt-3" data-stepup-part="2">
<p class="text-xs font-semibold text-fg2" data-stepup-part-n>2/2</p>
<div class="flex min-w-0 flex-col gap-3 text-[15px] leading-normal text-fg" data-stepup-preview-text><p class="whitespace-pre-wrap break-words">Second part.</p></div>
<div class="grid min-w-0 gap-0.5 overflow-hidden rounded-xl border border-line" data-stepup-media><img alt="Attached image 2" loading="lazy" class="block h-auto max-h-[32rem] w-full bg-s1 object-cover" src="/plugins/drafts/9/media/1" data-stepup-medium data-stepup-image></div>
</div>
</article>
`
	if !strings.Contains(page, want) || strings.Contains(page, "---8&lt;---") {
		t.Errorf("the thread on the page, want\n%s\nin\n%s", want, page)
	}
	feed := askFeedCardHTML(askItem(a), time.Now())
	if !strings.Contains(feed, `data-stepup-preview-text>First part.
Still first.</p>
<p class="text-xs text-fg2" data-stepup-thread-more>+1 more post in this thread</p>`) {
		t.Errorf("the thread on the feed:\n%s", feed)
	}
	// One post with two images: both on it, side by side.
	two := askQuestionHTML(stepUpPostCard(t, "x", "X", "post", "", "One post.", 2))
	if !strings.Contains(two, `<div class="grid min-w-0 gap-0.5 overflow-hidden rounded-xl border border-line grid-cols-2" data-stepup-media>`) ||
		strings.Count(two, `class="block aspect-square h-full w-full bg-s1 object-cover"`) != 2 || strings.Contains(two, "data-stepup-part-n") {
		t.Errorf("one post with two images:\n%s", two)
	}
}

// TestAStepUpPreviewsTextIsSafeAndReadable: the officer's text is escaped,
// its bidi and control characters are U+FFFD (they cannot reorder the text),
// a tab is a space, a carriage return a line break — on the page and the
// feed, whether the card came through Clean or not. Nothing is spelled out.
func TestAStepUpPreviewsTextIsSafeAndReadable(t *testing.T) {
	const evil = "<script>alert(1)</script> \u202Egnp.exe\u202C & done\tnow\rnext"
	const want = `<p class="whitespace-pre-wrap break-words">&lt;script&gt;alert(1)&lt;/script&gt; ` + "\uFFFD" + `gnp.exe` + "\uFFFD" + ` &amp; done now
next</p>`
	cards := map[string]attention.Ask{}
	// An X post (whose thread split folds a CR itself) and a reply draft
	// (whose text only the renderer reads).
	for _, platform := range []string{"x", ""} {
		stored := stepUpPostCard(t, platform, "X", "post", "", evil, 0)
		raw := stored
		rc := *stored.StepUp
		rc.Preview = evil // as if stored without Clean: the renderer's own guarantee
		raw.StepUp = &rc
		cards["cleaned "+platform], cards["raw "+platform] = stored, raw
	}
	for name, a := range cards {
		page, feed := askQuestionHTML(a), askFeedCardHTML(askItem(a), time.Now())
		if !strings.Contains(page, want) {
			t.Errorf("%s: the page's text, want %s in\n%s", name, want, page)
		}
		for face, html := range map[string]string{"page": page, "feed": feed} {
			for _, never := range []string{"<script>", "\u202E", "\u202C", "\t", "\r", `\u{`, `\t`, `\r`, `\n`} {
				if strings.Contains(html, never) {
					t.Errorf("%s %s carries %q:\n%s", name, face, never, html)
				}
			}
		}
	}
	// Paragraphs: blank lines (however many, however spaced) between, a line
	// break inside one kept.
	if got := strings.Join(stepUpParagraphs("a\nb\n\n \n\nc\r\nd  \ne\rf\n"), "|"); got != "a\nb|c\nd\ne\nf" {
		t.Errorf("stepUpParagraphs: %q", got)
	}
}

// TestStepUpDetailsSayTheCallInWords: Details quotes a call's arguments as
// words in the call's order — never JSON, never a hash, a media type or a
// size, a string's line breaks folded — and says so when it cannot read them.
func TestStepUpDetailsSayTheCallInWords(t *testing.T) {
	cases := []struct{ args, want string }{
		{`{"id":484,"decision":"approve","body_sha256":"` + strings.Repeat("a", 64) + `","media_sha256":["` + strings.Repeat("b", 64) + `"]}`,
			`with id 484, decision “approve”`},
		{`{"route_id":"r1","body":"See you Friday.\nBye.\n\n\tThanks"}`, `with route_id “r1”, body “See you Friday. Bye. Thanks”`},
		{`{"batch_id":"b-1","batch_sha256":"s","approve_ids":[1,2,3]}`, `with batch_id “b-1”, approve_ids 1, 2, 3`},
		{`{"id":4,"media":[{"ref":"a/b","sha256":"x","content_type":"image/png","size":42322}],"done":true,"note":null,"ids":[]}`,
			`with id 4, media (ref “a/b”), done true, note none, ids none`},
		{`{}`, `with no arguments shown`},
		{``, `with no arguments`},
		{`{"id":`, `with arguments muster could not read`},
	}
	for _, c := range cases {
		if got := stepUpCallWords(c.args); got != c.want {
			t.Errorf("stepUpCallWords(%s)\n got %q\nwant %q", c.args, got, c.want)
		}
	}
	const want = "A session on host-a (main:1) asks to approve a draft.\nThe call: plugin_draft_decide, with id 7.\n\nThis machine runs host officer acme:main, " +
		"and its sessions are named by a header any process there can set — so this needs you. Confirm on your paired phone to let it happen " +
		"(a signed action; a browser answer is refused), or decide it in muster's web UI instead."
	if got := stepUpDetails("host-a", "main:1", "approve a draft", pluginDraftDecideTool, `{"id":7,"body_sha256":"`+strings.Repeat("f", 64)+`"}`,
		"This machine runs host officer acme:main"); got != want {
		t.Errorf("stepUpDetails\n got %q\nwant %q", got, want)
	}
}

// The classes the look-alike case is drawn with (internal/ui stepup_post.go),
// as literals: what a reader of the page gets, not what the code computes.
const (
	stepUpMarkOpen = `<mark class="rounded-sm bg-danger/15 text-fg underline decoration-danger decoration-wavy underline-offset-2" data-stepup-lookalike>`
	stepUpNoteLine = `Contains look-alike letters — check the highlighted words.</p>`
)

// TestLookAlikeLettersAreFlaggedOnTheStepUpPreview: the preview draws a
// post's text as it reads, so a look-alike letter would read as the real one.
// A mixed-script word (a Cyrillic а in "pаypal.com", a Greek ο in "gοogle"),
// a non-ASCII letter in a domain or an @handle (all-Cyrillic "аррӏе.com",
// "@аррӏе"), and a combining overlay mark (U+0336) are each marked, on the
// page and the feed, with one line under the preview — outside the post's
// mock — saying so. "café", a pure-Cyrillic word, Japanese, emoji and
// ordinary punctuation are not, and carry no line.
//
// RED at 037fe5724: no word was marked and no line drawn — "pаypal.com" read
// exactly as "paypal.com".
func TestLookAlikeLettersAreFlaggedOnTheStepUpPreview(t *testing.T) {
	flagged := []struct{ body, word string }{
		{"Pay at p\u0430ypal.com today.", "p\u0430ypal.com"},
		{"Search on g\u03BFogle now.", "g\u03BFogle"},
		{"Visit \u0430\u0440\u0440\u04CF\u0435.com for more.", "\u0430\u0440\u0440\u04CF\u0435.com"},
		{"Follow @\u0430\u0440\u0440\u04CF\u0435 there.", "@\u0430\u0440\u0440\u04CF\u0435"},
		{"Big S\u0336A\u0336L\u0336E\u0336 today.", "S\u0336A\u0336L\u0336E\u0336"},
	}
	for _, c := range flagged {
		for _, platform := range []string{"x", ""} {
			a := stepUpPostCard(t, platform, "X", "post", "", c.body, 0)
			page, feed := askQuestionHTML(a), askFeedCardHTML(askItem(a), time.Now())
			for face, h := range map[string]string{"page": page, "feed": feed} {
				if n := strings.Count(h, "data-stepup-lookalike>"); n != 1 || !strings.Contains(h, stepUpMarkOpen+c.word+`</mark>`) {
					t.Errorf("%s %q (%q): want %q marked once (%d marks):\n%s", face, c.body, platform, c.word, n, h)
				}
				if strings.Count(h, stepUpNoteLine) != 1 {
					t.Errorf("%s %q (%q): want the look-alike line once:\n%s", face, c.body, platform, h)
				}
			}
			// The line is under the post, outside its mock.
			if end, note := strings.Index(page, "</article>"), strings.Index(page, stepUpNoteLine); end < 0 || note < end {
				t.Errorf("page %q: the look-alike line is not after the post's mock:\n%s", c.body, page)
			}
		}
	}
	for _, body := range []string{
		"Caf\u00E9 and na\u00EFve r\u00E9sum\u00E9 \u2014 it\u2019s here. \u2615 \U0001F389\U0001F44D\U0001F3FD",
		"\u041F\u0440\u0438\u0432\u0435\u0442 \u043C\u0438\u0440, see example.com and @someone.",
		"\u65E5\u672C\u8A9E\u306E\u30C6\u30AD\u30B9\u30C8 iPhone\u3092 (English/\u0440\u0443\u0441\u0441\u043A\u0438\u0439).",
	} {
		a := stepUpPostCard(t, "x", "X", "post", "", body, 0)
		for face, h := range map[string]string{"page": askQuestionHTML(a), "feed": askFeedCardHTML(askItem(a), time.Now())} {
			if strings.Contains(h, "data-stepup-lookalike") || strings.Contains(h, "look-alike") {
				t.Errorf("%s %q: an ordinary text is flagged:\n%s", face, body, h)
			}
		}
	}
}

// TestAnOldDraftCardStillSaysItsVenue: a draft card filed before cards
// carried their platform and target (both empty) is drawn with the venue and
// kind its title names — an X post's card says X and New post, with X's logo —
// rather than as a reply to a message with the neutral logo. A reply draft's
// card (its title "Send this reply on …") and a card whose title names no
// outbound kind stay neutral.
//
// RED at 037fe5724: every such card read "Reply to a message", neutral logo.
func TestAnOldDraftCardStillSaysItsVenue(t *testing.T) {
	cases := []struct{ title, venue, logo, kind string }{
		{"Post to X? · #9", "X", "x", "New post"},
		{"Post to X?", "X", "x", "New post"},
		{"Reply on X? · #9", "X", "x", "Reply"},
		{"Send a DM on X?", "X", "x", "Direct message"},
		{"Post to Reddit? · #9", "Reddit", "reddit", "New post"},
		{"Send this reply on X? · #9", "X", "other", "Reply to a message"},
		{"Reject this draft? · #9", "X", "other", "Reply to a message"},
	}
	for _, c := range cases {
		a := stepUpAsk(t, &attention.StepUpCard{Tool: pluginDraftDecideTool, Title: c.title, Why: "Marketing: launch", Asker: "main:1",
			Preview: "Hello there.", Venue: c.venue, DraftID: 9, Details: "D"})
		for face, h := range map[string]string{"page": askQuestionHTML(a), "feed": askFeedCardHTML(askItem(a), time.Now())} {
			if !strings.Contains(h, `data-venue-logo="`+c.logo+`"`) || !strings.Contains(h, `data-stepup-post-kind>`+c.kind+`</span>`) {
				t.Errorf("%s %q: want logo %q and kind %q:\n%s", face, c.title, c.logo, c.kind, h)
			}
		}
	}
	// An old X post's card is a thread like a new one's.
	a := stepUpAsk(t, &attention.StepUpCard{Tool: pluginDraftDecideTool, Title: "Post to X? · #9", Why: "W", Preview: "First.\n---8<---\nSecond.", Venue: "X", DraftID: 9, Details: "D"})
	if page := askQuestionHTML(a); !strings.Contains(page, `data-stepup-post-kind>Thread · 2 posts</span>`) || strings.Contains(page, "8&lt;") {
		t.Errorf("an old X thread's card:\n%s", page)
	}
}
