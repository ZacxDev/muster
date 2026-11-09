package ui

import (
	"regexp"
	"strings"
	"testing"
)

// THE RENAME OFFER'S MARKUP (driftBanner). The layout — copy, then the Rename
// button, then "Keep the name" as a text link, one per row — is checked in a
// browser by e2e/tests/session-names.spec.ts (bounding boxes at 390×844 and
// 1280×800); these pin the markup that layout comes from, and that the two
// forms still post exactly what they posted before the layout changed.

// driftFixture is a synthetic offer whose text needs escaping (a quote, an
// ampersand) and a page with four hidden fields and a query-string Back.
func driftFixture() *SessionDrift {
	return &SessionDrift{Offer: `Fixing the "Stripe" webhook retry & refunds`, RenameAction: "/sessions/name/rename",
		Fields: [][2]string{{"kind", "mirror"}, {"host", "atlas"}, {"session", "main"}, {"window", "@1"}},
		Back:   "/mirror/view?host=atlas&session=main&window=%401"}
}

// driftGolden is the whole stacked banner, byte for byte.
const driftGolden = `<div id="session-drift" data-session-drift="Fixing the &#34;Stripe&#34; webhook retry &amp; refunds" hx-boost="false" class="flex flex-col items-start gap-1 border-b border-line bg-s1 px-3 py-2 text-xs text-fg2">` +
	`<p class="max-w-full break-words">This session has moved on to “Fixing the &#34;Stripe&#34; webhook retry &amp; refunds”.</p>` +
	`<form method="post" action="/sessions/name/rename" hx-post="/sessions/name/rename" hx-swap="none" class="max-w-full">` +
	`<input type="hidden" name="back" value="/mirror/view?host=atlas&amp;session=main&amp;window=%401"><input type="hidden" name="kind" value="mirror"><input type="hidden" name="host" value="atlas"><input type="hidden" name="session" value="main"><input type="hidden" name="window" value="@1"><input type="hidden" name="name" value="Fixing the &#34;Stripe&#34; webhook retry &amp; refunds">` +
	`<button type="submit" data-session-drift-accept="" class="press inline-flex min-h-[44px] max-w-full items-center rounded-lg bg-accent px-3 py-1 text-left text-xs font-semibold break-words text-on-accent transition hover:bg-accent/90">Rename to “Fixing the &#34;Stripe&#34; webhook retry &amp; refunds”</button></form>` +
	`<form method="post" action="/sessions/name/drift-dismiss" hx-post="/sessions/name/drift-dismiss" hx-swap="none" class="max-w-full">` +
	`<input type="hidden" name="back" value="/mirror/view?host=atlas&amp;session=main&amp;window=%401"><input type="hidden" name="kind" value="mirror"><input type="hidden" name="host" value="atlas"><input type="hidden" name="session" value="main"><input type="hidden" name="window" value="@1"><input type="hidden" name="title" value="Fixing the &#34;Stripe&#34; webhook retry &amp; refunds">` +
	`<button type="submit" data-session-drift-dismiss="" class="inline-flex min-h-[44px] min-w-[44px] items-center px-1 text-xs font-semibold text-fg2 underline-offset-2 hover:text-fg hover:underline focus-visible:underline">Keep the name</button></form>` +
	`</div>`

// TestTheDriftOfferIsStacked: the whole banner is the golden — a column
// (flex-col), the copy its own block, the Rename button, and "Keep the name"
// with no ring and no fill, underlined on hover and focus, 44px tall.
func TestTheDriftOfferIsStacked(t *testing.T) {
	if got := renderRaw(t, driftBanner(driftFixture())); got != driftGolden {
		t.Fatalf("the drift banner:\n got  %s\n want %s", got, driftGolden)
	}
	if driftBanner(nil) != nil || driftBanner(&SessionDrift{}) != nil {
		t.Fatal("no offer draws a banner")
	}
}

// driftFormsBefore is the two forms as drawn BEFORE the stacked layout
// (captured from main at 0df73e27 with this fixture), every class attribute
// removed: what they post, where, and the ids and data- attributes the page
// and the specs find them by.
const driftFormsBefore = `<form method="post" action="/sessions/name/rename" hx-post="/sessions/name/rename" hx-swap="none"><input type="hidden" name="back" value="/mirror/view?host=atlas&amp;session=main&amp;window=%401"><input type="hidden" name="kind" value="mirror"><input type="hidden" name="host" value="atlas"><input type="hidden" name="session" value="main"><input type="hidden" name="window" value="@1"><input type="hidden" name="name" value="Fixing the &#34;Stripe&#34; webhook retry &amp; refunds"><button type="submit" data-session-drift-accept="">Rename to “Fixing the &#34;Stripe&#34; webhook retry &amp; refunds”</button></form>` +
	`<form method="post" action="/sessions/name/drift-dismiss" hx-post="/sessions/name/drift-dismiss" hx-swap="none"><input type="hidden" name="back" value="/mirror/view?host=atlas&amp;session=main&amp;window=%401"><input type="hidden" name="kind" value="mirror"><input type="hidden" name="host" value="atlas"><input type="hidden" name="session" value="main"><input type="hidden" name="window" value="@1"><input type="hidden" name="title" value="Fixing the &#34;Stripe&#34; webhook retry &amp; refunds"><button type="submit" data-session-drift-dismiss="">Keep the name</button></form>`

var classAttr = regexp.MustCompile(` class="[^"]*"`)

// TestTheDriftOfferPostsWhatItPostedBefore: with the classes (the layout)
// removed, the two forms are exactly the pre-layout forms — the same methods,
// actions, hx attributes, hidden fields in the same order, data- attributes
// and labels — and the banner keeps its id, data-session-drift and hx-boost.
func TestTheDriftOfferPostsWhatItPostedBefore(t *testing.T) {
	got := classAttr.ReplaceAllString(renderRaw(t, driftBanner(driftFixture())), "")
	i := strings.Index(got, "<form")
	if i < 0 || !strings.HasSuffix(got, "</div>") {
		t.Fatalf("no forms in:\n%s", got)
	}
	if forms := strings.TrimSuffix(got[i:], "</div>"); forms != driftFormsBefore {
		t.Fatalf("the forms changed:\n got  %s\n want %s", forms, driftFormsBefore)
	}
	const head = `<div id="session-drift" data-session-drift="Fixing the &#34;Stripe&#34; webhook retry &amp; refunds" hx-boost="false">`
	if !strings.HasPrefix(got, head) {
		t.Fatalf("the banner's own attributes changed:\n%s", got)
	}
}
