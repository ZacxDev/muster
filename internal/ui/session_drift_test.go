package ui

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	g "maragu.dev/gomponents"

	"github.com/ZacxDev/muster/internal/sessionname"
)

// THE RENAME OFFER'S MARKUP (driftBanner) AND ITS IN-PLACE ANSWERS. The
// layout — one row: a label, the offered name, "Rename", "Keep" — is measured
// in a browser by e2e/tests/session-names.spec.ts (heights and boxes at
// 390×844 and 1280×800, and the no-reload click path); these pin the markup
// that layout comes from, that the two forms still post what they posted
// before, and that the answers redraw the page's name with the page's own
// helpers.

// driftFixture is a synthetic offer whose text needs escaping (a quote, an
// ampersand) and a page with four hidden fields and a query-string Back.
func driftFixture() *SessionDrift {
	return &SessionDrift{Offer: `Fixing the "Stripe" webhook retry & refunds`, RenameAction: "/sessions/name/rename",
		Fields: [][2]string{{"kind", "mirror"}, {"host", "atlas"}, {"session", "main"}, {"window", "@1"}},
		Back:   "/mirror/view?host=atlas&session=main&window=%401"}
}

// driftGolden is the whole one-line banner and its status region, byte for
// byte.
const driftGolden = `<div id="session-drift" data-session-drift="Fixing the &#34;Stripe&#34; webhook retry &amp; refunds" role="region" aria-label="Rename suggestion" hx-boost="false" class="flex flex-wrap items-center gap-x-2 border-b border-line bg-s1 px-3 text-xs text-fg2">` +
	`<p class="flex min-w-0 flex-1 basis-40 items-center gap-1.5"><span class="shrink-0 cursor-help text-muted" title="This session&#39;s work has moved on from its name. Rename it to what it is about now, or keep the name: muster offers again only when the focus changes.">Suggested name</span>` +
	`<span data-session-drift-offer="" class="min-w-0 truncate font-semibold text-fg" title="Fixing the &#34;Stripe&#34; webhook retry &amp; refunds">Fixing the &#34;Stripe&#34; webhook retry &amp; refunds</span></p>` +
	`<div class="ml-auto flex shrink-0 items-center">` +
	`<form method="post" action="/sessions/name/rename" hx-post="/sessions/name/rename" hx-swap="none" hx-vals="{&#34;drift&#34;:&#34;1&#34;}" hx-disabled-elt="find button">` +
	`<input type="hidden" name="back" value="/mirror/view?host=atlas&amp;session=main&amp;window=%401"><input type="hidden" name="kind" value="mirror"><input type="hidden" name="host" value="atlas"><input type="hidden" name="session" value="main"><input type="hidden" name="window" value="@1"><input type="hidden" name="name" value="Fixing the &#34;Stripe&#34; webhook retry &amp; refunds">` +
	`<button type="submit" data-session-drift-accept="" aria-label="Rename to “Fixing the &#34;Stripe&#34; webhook retry &amp; refunds”" class="press group inline-flex h-11 items-center rounded-md px-1 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus disabled:opacity-60"><span class="rounded-md bg-accent px-2.5 py-1 font-semibold text-on-accent transition group-hover:bg-accent/90">Rename</span></button></form>` +
	`<form method="post" action="/sessions/name/drift-dismiss" hx-post="/sessions/name/drift-dismiss" hx-swap="none" hx-disabled-elt="find button">` +
	`<input type="hidden" name="back" value="/mirror/view?host=atlas&amp;session=main&amp;window=%401"><input type="hidden" name="kind" value="mirror"><input type="hidden" name="host" value="atlas"><input type="hidden" name="session" value="main"><input type="hidden" name="window" value="@1"><input type="hidden" name="title" value="Fixing the &#34;Stripe&#34; webhook retry &amp; refunds">` +
	`<button type="submit" data-session-drift-dismiss="" aria-label="Keep the current name" class="inline-flex h-11 min-w-[44px] items-center justify-center rounded-md px-2 font-semibold text-fg2 underline-offset-2 hover:text-fg hover:underline focus-visible:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus disabled:opacity-60">Keep</button></form>` +
	`</div></div>` +
	`<p id="session-drift-status" role="status" aria-live="polite" class="sr-only"></p>`

// TestTheDriftOfferIsOneLine: the whole banner is the golden — one wrapping
// ROW (flex-wrap, not the stacked flex-col it was), no vertical padding (the
// 44px buttons are its height), the offer truncated with its whole text as
// its tooltip, "Keep" with no ring and no fill, and a polite status region
// beside it for the in-place answers.
func TestTheDriftOfferIsOneLine(t *testing.T) {
	if got := renderRaw(t, driftBanner(driftFixture())); got != driftGolden {
		t.Fatalf("the drift banner:\n got  %s\n want %s", got, driftGolden)
	}
	if driftBanner(nil) != nil || driftBanner(&SessionDrift{}) != nil {
		t.Fatal("no offer draws a banner")
	}
}

// driftInputsBefore is each form's hidden fields as drawn BEFORE the in-place
// change (main at 476d594d, this fixture): what the forms post, in order.
var driftInputsBefore = [2]string{
	`<input type="hidden" name="back" value="/mirror/view?host=atlas&amp;session=main&amp;window=%401"><input type="hidden" name="kind" value="mirror"><input type="hidden" name="host" value="atlas"><input type="hidden" name="session" value="main"><input type="hidden" name="window" value="@1"><input type="hidden" name="name" value="Fixing the &#34;Stripe&#34; webhook retry &amp; refunds">`,
	`<input type="hidden" name="back" value="/mirror/view?host=atlas&amp;session=main&amp;window=%401"><input type="hidden" name="kind" value="mirror"><input type="hidden" name="host" value="atlas"><input type="hidden" name="session" value="main"><input type="hidden" name="window" value="@1"><input type="hidden" name="title" value="Fixing the &#34;Stripe&#34; webhook retry &amp; refunds">`,
}

var (
	formOpen = regexp.MustCompile(`<form [^>]*>`)
	hiddenIn = regexp.MustCompile(`<input type="hidden"[^>]*>`)
)

// TestTheDriftOfferPostsWhatItPostedBefore: the two forms post exactly the
// hidden fields they posted before, to the same routes, by the same method,
// with and without htmx — the server's semantics are unchanged (accept is an
// ordinary rename, keep a dismissal). The ONLY addition is the Rename's
// hx-vals marker, which htmx alone sends; a plain form post carries none.
func TestTheDriftOfferPostsWhatItPostedBefore(t *testing.T) {
	got := renderRaw(t, driftBanner(driftFixture()))
	forms := strings.Split(got, "</form>")
	if len(forms) != 3 {
		t.Fatalf("want two forms in:\n%s", got)
	}
	wantOpen := [2]string{
		`<form method="post" action="/sessions/name/rename" hx-post="/sessions/name/rename" hx-swap="none" hx-vals="{&#34;drift&#34;:&#34;1&#34;}" hx-disabled-elt="find button">`,
		`<form method="post" action="/sessions/name/drift-dismiss" hx-post="/sessions/name/drift-dismiss" hx-swap="none" hx-disabled-elt="find button">`,
	}
	for i := 0; i < 2; i++ {
		if open := formOpen.FindString(forms[i]); open != wantOpen[i] {
			t.Errorf("form %d opens\n got  %s\n want %s", i, open, wantOpen[i])
		}
		if in := strings.Join(hiddenIn.FindAllString(forms[i], -1), ""); in != driftInputsBefore[i] {
			t.Errorf("form %d posts\n got  %s\n want %s", i, in, driftInputsBefore[i])
		}
	}
	if strings.Count(got, `name="`+SessionDriftParam+`"`) != 0 {
		t.Error("the in-place marker is a form field: a plain post would carry it")
	}
}

// TestKeepIsAnsweredInPlace: "Keep"'s answer deletes the banner and fills
// the status region, out of band — and holds nothing that could reload or
// navigate the page.
func TestKeepIsAnsweredInPlace(t *testing.T) {
	var b bytes.Buffer
	if err := RenderDriftKept(&b); err != nil {
		t.Fatal(err)
	}
	const want = `<div id="session-drift" hx-swap-oob="delete"></div><p id="session-drift-status" hx-swap-oob="innerHTML">Kept the name.</p>`
	if b.String() != want {
		t.Fatalf("keep's answer:\n got  %s\n want %s", b.String(), want)
	}
}

// withoutOOB is html with its hx-swap-oob attributes removed: an answer's
// copy of an element as the page itself draws it.
func withoutOOB(html string) string {
	return regexp.MustCompile(` hx-swap-oob="[^"]*"`).ReplaceAllString(html, "")
}

// renderedName is the new name every renamed fixture carries: pairwise
// distinct from the old name, the slug, the technical id and every constant
// the answers spell.
const renamedName = "Wiring the refund flow"

// TestARenamedMirrorIsRedrawnInPlace: the Rendered view's answer to the
// offer's Rename carries, out of band, the banner deleted, the header's name
// span EXACTLY as the page draws it (same id, so it lands on the page's span;
// the new name, manual), the full-title popover as the page draws it, the
// switcher's rows and its rename dialog prefilled with the new name; and the
// document's <title> as the page draws it.
func TestARenamedMirrorIsRedrawnInPlace(t *testing.T) {
	n := sessionname.Name{Text: renamedName, Source: sessionname.SourceManual, Technical: "tmux main:1 @ atlas"}
	mirrors := []MirrorCardView{{Host: "atlas", Session: "main", Window: "@1", SessionName: n, ManualName: renamedName}}
	v := MirrorViewView{Host: "atlas", Session: "main", Window: "@1", SessionName: n,
		Switcher: NewSwitcher(nil, mirrors, nil, SwitcherCurrent{Mirror: MirrorRef{Host: "atlas", Session: "main", Window: "@1"}})}
	var b bytes.Buffer
	if err := RenderMirrorDriftRenamed(&b, v); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	page := renderRaw(t, mirrorViewTitle(v))
	nameSpan := renderRaw(t, mirrorViewEntryTitle(v))
	for _, want := range []string{
		`<div id="session-drift" hx-swap-oob="delete"></div>`,
		`<p id="session-drift-status" hx-swap-oob="innerHTML">Renamed to “` + renamedName + `”.</p>`,
		`<title>` + renamedName + ` · Mirrored · muster</title>`,
		`<span id="` + MirrorNameID + `" hx-swap-oob="outerHTML" data-mirror-name="" data-session-name="manual"`,
		`id="` + SessionRenameID + `" hx-swap-oob="outerHTML"`,
		`value="` + renamedName + `" data-session-rename-input`,
		`id="` + switcherRowID(SwitcherAll) + `" hx-swap-oob="innerHTML"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the answer lacks %s:\n%s", want, got)
		}
	}
	// The page's own span is what lands: the same element, the same id.
	if !strings.Contains(withoutOOB(got), nameSpan) || !strings.Contains(page, `id="`+MirrorNameID+`"`) {
		t.Errorf("the answer's name is not the header's span:\n page %s\n answer %s", nameSpan, got)
	}
	if pop := renderRaw(t, fullTitlePopover(mirrorTitlePopoverID, mirrorViewHeading(v))); !strings.Contains(withoutOOB(got), pop) {
		t.Errorf("the answer's full title is not the page's:\n%s", pop)
	}
	if !strings.Contains(mirrorViewHeading(v), renamedName) {
		t.Error("control: the heading names the new name")
	}
}

// TestARenamedAgentIsRedrawnInPlace: the agent page's answer carries the
// header's title box EXACTLY as the page draws it (AgentTitleID), the agent
// menu's rename dialog and the switcher's, both prefilled with the new name,
// and the page's <title>.
func TestARenamedAgentIsRedrawnInPlace(t *testing.T) {
	n := sessionname.Name{Text: renamedName, Source: sessionname.SourceManual, Technical: "agent-7"}
	card := AgentCardView{ID: 7, Name: "agent-7", DisplayName: renamedName, SessionName: n}
	v := AgentDetailView{ID: 7, Name: "agent-7", DisplayName: renamedName, SessionName: n, Card: card,
		Switcher: NewSwitcher([]AgentCardView{card}, nil, nil, SwitcherCurrent{AgentID: 7})}
	var b bytes.Buffer
	if err := RenderAgentDriftRenamed(&b, v); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	for _, want := range []string{
		`<div id="session-drift" hx-swap-oob="delete"></div>`,
		`<p id="session-drift-status" hx-swap-oob="innerHTML">Renamed to “` + renamedName + `”.</p>`,
		`<title>` + renamedName + ` · muster</title>`,
		`<div id="` + AgentTitleID + `" hx-swap-oob="outerHTML" data-agent-title=""`,
		`id="` + AgentRenameID + `" hx-swap-oob="outerHTML"`,
		`id="` + SessionRenameID + `" hx-swap-oob="outerHTML"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the answer lacks %s:\n%s", want, got)
		}
	}
	if title := renderRaw(t, agentTitle(v)); !strings.Contains(withoutOOB(got), title) {
		t.Errorf("the answer's title is not the header's:\n page %s\n answer %s", title, got)
	}
	if strings.Count(got, `value="`+renamedName+`" data-session-rename-input`) != 2 {
		t.Errorf("both rename dialogs are not prefilled with the new name:\n%s", got)
	}
	// Without a switcher (a page drawn without one) the rest still goes.
	v.Switcher = nil
	b.Reset()
	if err := RenderAgentDriftRenamed(&b, v); err != nil || strings.Contains(b.String(), `id="`+SessionRenameID+`"`) || !strings.Contains(b.String(), `id="`+AgentTitleID+`"`) {
		t.Fatalf("no switcher: %v\n%s", err, b.String())
	}
}

// TestTheRenameDialogIsOutOfBandOnlyWhenAsked: the dialogs a page draws carry
// no hx-swap-oob (a page's element must never be an out-of-band swap itself).
func TestTheRenameDialogIsOutOfBandOnlyWhenAsked(t *testing.T) {
	for name, n := range map[string]g.Node{
		"agent":    agentRenamePopover(AgentCardView{ID: 7, Name: "agent-7"}),
		"switcher": renamePopover(renameDialog{id: SessionRenameID, action: SessionNamePath, heading: "Rename session"}),
	} {
		if strings.Contains(renderRaw(t, n), "hx-swap-oob") {
			t.Errorf("%s: the page's dialog is drawn out of band", name)
		}
	}
}
