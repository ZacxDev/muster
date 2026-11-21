package api

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// THE STEP-UP CARD'S POST PREVIEW ON EVERY FACE, THROUGH THE REAL MIRROR DOOR
// (host_stepup_card.go stepUpPostHTML / stepUpFeedHTML). The operator's
// feedback on the card, verbatim: "show it as a styled preview with the logo
// of the platform, also render the text, dont show raw '\n' dont show the
// mimi, size, sha. do show the id in the title".
//
// RED at the base (asks-ui-followup c8473fefb): the page drew a "Post on X"
// pill over one box of VisibleEscape'd text (an em dash read \u{2014}), the
// title had no id, and Details quoted the call's JSON with its body_sha256,
// its media_sha256 and the call's hash.

// stepUpXLogo is X's glyph as the card draws it (ui.VenueLogoSVG "x").
const stepUpXLogo = `<svg viewBox="0 0 24 24" aria-hidden="true" focusable="false" class="h-5 w-5" data-venue-logo="x">` +
	`<path fill="currentColor" d="M18.244 2.25h3.308l-7.227 8.26 8.502 11.24H16.17l-5.214-6.817L4.99 21.75H1.68l7.73-8.835L1.254 2.25H8.08l4.713 6.231zm-1.161 17.52h1.833L7.084 4.126H5.117z"/></svg>`

// stepUpRegion is the step-up card's own markup in page: from its card to
// the end of its Details.
func stepUpRegion(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, "data-stepup-card")
	j := -1
	if i >= 0 {
		j = strings.Index(page[i:], "</details>")
	}
	if i < 0 || j < 0 {
		t.Fatalf("no step-up card on the page:\n%s", page)
	}
	return page[i : i+j]
}

// TestAStepUpPostPreviewOnEveryFace: an X post with two paragraphs (a line
// break inside the first, non-ASCII punctuation) and one image is drawn on
// its page as a mock of the post — X's logo, "X", "New post", the text as
// paragraphs with real line breaks, the image below — and on the feed as the
// compact mock; the title carries the draft id on the page heading, the top
// bar, the feed and the push; and no face (page, Details, feed, push, the
// phone's question) shows the media's type, size or hash, the body's hash, or
// a spelled escape.
func TestAStepUpPostPreviewOnEveryFace(t *testing.T) {
	const body = "Launch day — it’s here.\nTwo lines in one paragraph.\n\nA second paragraph: github.com/example/repo"
	r := newStepUpDraftRig(t, body)
	a := r.decideCard(t, map[string]any{"decision": "approve", "body_sha256": sha(body), "media_sha256": []string{sha("launch/hero-1")}})
	id := strconv.FormatInt(r.draft.ID, 10)
	title := "Post to X? · #" + id

	page := r.askPage(t, a.ID)
	card := stepUpRegion(t, page)
	post := `<article class="flex min-w-0 flex-col gap-3 rounded-2xl bg-bg p-4 ring-1 ring-inset ring-edge" data-stepup-preview data-stepup-post data-logo="x">
<header class="flex min-w-0 items-center gap-3" data-stepup-post-head><span class="flex h-10 w-10 shrink-0 items-center justify-center rounded-full bg-fg text-bg" data-stepup-logo>` +
		stepUpXLogo + `</span><span class="flex min-w-0 flex-col leading-tight"><span class="truncate text-base font-semibold text-fg" data-stepup-venue>X</span>` +
		`<span class="truncate text-sm text-fg2" data-stepup-post-kind>New post</span></span></header>
<div class="flex min-w-0 flex-col gap-3" data-stepup-part="1">
<div class="flex min-w-0 flex-col gap-3 text-[15px] leading-normal text-fg" data-stepup-preview-text>` +
		`<p class="whitespace-pre-wrap break-words">Launch day — it’s here.
Two lines in one paragraph.</p><p class="whitespace-pre-wrap break-words">A second paragraph: github.com/example/repo</p></div>
<div class="grid min-w-0 gap-0.5 overflow-hidden rounded-xl border border-line" data-stepup-media>` +
		`<img alt="Attached image 1" loading="lazy" class="block h-auto max-h-[32rem] w-full bg-s1 object-cover" src="/plugins/drafts/` + id + `/media/0" data-stepup-medium data-stepup-image></div>
</div>
</article>
`
	if !strings.Contains(card, post) {
		t.Errorf("the page's post preview, want exactly\n%s\nin\n%s", post, card)
	}
	if !strings.Contains(card, `data-stepup-title>`+title+`</h2>`) {
		t.Errorf("the card's heading lacks the id %q:\n%s", title, card)
	}
	if got := askPageH1(page); got != title {
		t.Errorf("the top bar's heading %q, want %q", got, title)
	}

	feed := askFeedCardHTML(askItem(a), time.Now())
	feedPost := `<div class="flex min-w-0 flex-col gap-2" data-attention-text data-stepup-feed>
<p class="break-words text-[15px] font-semibold leading-snug text-fg" data-stepup-title>` + title + `</p>
<p class="asks-clamp-1 break-words text-sm text-fg2" data-stepup-why>Marketing: rebrand announcement</p>
<p class="asks-clamp-1 break-words text-sm text-fg2" data-stepup-about>Asked by work:2</p>
<div class="flex min-w-0 flex-col gap-2 rounded-xl bg-bg p-3 ring-1 ring-inset ring-edge" data-stepup-preview data-stepup-post data-logo="x">
<div class="flex min-w-0 items-center gap-2" data-stepup-post-head><span class="flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-fg text-bg" data-stepup-logo>` +
		strings.Replace(stepUpXLogo, `class="h-5 w-5"`, `class="h-4 w-4"`, 1) +
		`</span><span class="min-w-0 truncate text-sm text-fg2"><span class="font-semibold text-fg" data-stepup-venue>X</span> · <span data-stepup-post-kind>New post</span></span></div>
<p class="asks-clamp whitespace-pre-wrap break-words text-[15px] leading-snug text-fg" data-stepup-preview-text>Launch day — it’s here.
Two lines in one paragraph.

A second paragraph: github.com/example/repo</p>
<div class="flex min-w-0 flex-wrap gap-1" data-stepup-media>` +
		`<img alt="Attached image 1" loading="lazy" class="h-20 w-auto max-w-[12rem] rounded-lg border border-line bg-s1 object-cover" src="/plugins/drafts/` + id + `/media/0" data-stepup-medium data-stepup-image></div>
</div>
</div>
`
	if !strings.Contains(feed, feedPost) {
		t.Errorf("the feed's card, want exactly\n%s\nin\n%s", feedPost, feed)
	}

	n := attentionNotification(askItem(a), false)
	if n.Title != title {
		t.Errorf("the push title %q, want %q", n.Title, title)
	}
	if want := "Marketing: rebrand announcement · Asked by work:2 · Launch day — it’s here. Two lines in one paragraph. A second paragraph: github.com/example/repo"; n.Body != want {
		t.Errorf("the push body\n%q\nwant\n%q", n.Body, want)
	}

	// No face shows the media's type, size or hash, the body's hash, the
	// call's JSON, or a spelled escape. 10 B is the rig's upload size.
	// The feed's face: its card from the head to the phone line (the rest is
	// the card's controls, whose hx-vals are JSON).
	fi, fj := strings.Index(feed, "<li "), strings.Index(feed, "data-asks-stepup-row")
	if fi < 0 || fj < fi {
		t.Fatalf("the feed card has no head or no phone line:\n%s", feed)
	}
	faces := map[string]string{"page": card, "feed": feed[fi:fj], "push": n.Title + "\n" + n.Body, "question": a.Question, "details": a.StepUp.Details}
	for face, text := range faces {
		for _, never := range []string{"image/webp", "webp", "10 B", "sha256", "sha ", sha(body), sha("launch/hero-1"), sha("launch/hero-1")[:12], sha(body)[:12],
			`\n`, `\t`, `\r`, `\u{`, "{&#34;", `{"`} {
			if strings.Contains(text, never) {
				t.Errorf("the %s face shows %q:\n%s", face, never, text)
			}
		}
	}
}
