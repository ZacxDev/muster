package ui

import (
	"bytes"
	"os"
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

// driftGoldenMarkup is the whole one-line banner and its status region, byte
// for byte (then renameScript, the one script every rename surface carries).
const driftGoldenMarkup = `<div id="session-drift" data-session-drift="Fixing the &#34;Stripe&#34; webhook retry &amp; refunds" role="region" aria-label="Rename suggestion" hx-boost="false" class="flex flex-wrap items-center gap-x-2 border-b border-line bg-s1 px-3 text-xs text-fg2">` +
	`<p class="flex min-w-0 flex-1 basis-40 items-center gap-1.5"><span class="inline-flex shrink-0 cursor-help items-center text-muted" title="This session&#39;s work has moved on from its name. Rename it to what it is about now, or keep the name: muster offers again only when the focus changes.">` +
	`<svg class="h-3.5 w-3.5 sm:hidden" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true"><path d="M13.586 3.586a2 2 0 1 1 2.828 2.828l-.793.793-2.828-2.828.793-.793ZM11.379 5.793 3 14.172V17h2.828l8.38-8.379-2.83-2.828Z"/></svg>` +
	`<span class="sr-only sm:not-sr-only">Suggested name</span></span>` +
	`<span data-session-drift-offer="" class="min-w-0 truncate font-semibold text-fg" title="Fixing the &#34;Stripe&#34; webhook retry &amp; refunds">Fixing the &#34;Stripe&#34; webhook retry &amp; refunds</span></p>` +
	`<div class="ml-auto flex shrink-0 items-center">` +
	`<form method="post" action="/sessions/name/rename" hx-post="/sessions/name/rename" hx-swap="none" hx-disabled-elt="find button">` +
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
	driftGolden := driftGoldenMarkup + renderRaw(t, renameScript())
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
// ordinary rename, keep a dismissal), and nothing marks the offer's Rename
// apart from the dialog's Save: every htmx rename is answered in place.
func TestTheDriftOfferPostsWhatItPostedBefore(t *testing.T) {
	got := renderRaw(t, driftBanner(driftFixture()))
	forms := strings.Split(got, "</form>")
	if len(forms) != 3 {
		t.Fatalf("want two forms in:\n%s", got)
	}
	wantOpen := [2]string{
		`<form method="post" action="/sessions/name/rename" hx-post="/sessions/name/rename" hx-swap="none" hx-disabled-elt="find button">`,
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

// renamedName is the new name every renamed fixture carries: pairwise
// distinct from the old name, the slug, the label, the technical id and every
// constant the answers spell.
const renamedName = "Wiring the refund flow"

// expectRenamed checks an in-place answer's shared contract: the page's own
// element (page, as the page draws it) is in the body out of band, unchanged
// but for hx-swap-oob; the body holds no <title> (htmx would set the tab from
// it, decoding the name once more) — the title and focus are rn's.
func expectRenamed(t *testing.T, rn Renamed, page, title, focus, swapped string) string {
	t.Helper()
	body := renderRaw(t, rn.Body)
	if !strings.Contains(withoutOOB(body), page) {
		t.Errorf("the answer does not carry the page's own element:\n page   %s\n answer %s", page, body)
	}
	if !strings.Contains(body, `id="`+swapped+`" hx-swap-oob="outerHTML"`) {
		t.Errorf("the element %s is not swapped out of band:\n%s", swapped, body)
	}
	if strings.Contains(body, "<title") {
		t.Errorf("the answer carries a <title> htmx would decode:\n%s", body)
	}
	if rn.Title != title || rn.Focus != focus {
		t.Errorf("title %q focus %q, want %q %q", rn.Title, rn.Focus, title, focus)
	}
	return body
}

// TestARenamedMirrorIsRedrawnInPlace: on the Rendered view the answer swaps
// the WHOLE title button exactly as the page draws it — a human name, and,
// with the name cleared, the label the page falls back to — its full-title
// popover, the switcher and its dialog; the banner goes; the tab's title is
// the page's and focus the title button.
func TestARenamedMirrorIsRedrawnInPlace(t *testing.T) {
	ref := MirrorRef{Host: "atlas", Session: "main", Window: "@1"}
	for name, n := range map[string]sessionname.Name{
		"named":   {Text: renamedName, Source: sessionname.SourceManual, Technical: "tmux main:1 @ atlas"},
		"cleared": {Text: "mango:1", Source: sessionname.SourceFallback},
	} {
		mirrors := []MirrorCardView{{Host: "atlas", Session: "main", Window: "@1", Label: "mango:1", SessionName: n}}
		v := MirrorViewView{Host: "atlas", Session: "main", Window: "@1", Label: "mango:1", Title: "mango:1", SessionName: n, HostIcon: "server",
			Switcher: NewSwitcher(nil, mirrors, nil, SwitcherCurrent{Mirror: ref})}
		want := mirrorViewName(v)
		body := expectRenamed(t, MirrorViewRenamed(v), renderRaw(t, mirrorViewTitleButton(v)), want+" · Mirrored · muster", MirrorHeadingID, MirrorTitleID)
		for _, part := range []string{
			`<div id="session-drift" hx-swap-oob="delete"></div>`,
			`<p id="session-drift-status" hx-swap-oob="innerHTML">Renamed to “` + want + `”.</p>`,
			`id="` + SessionRenameID + `" hx-swap-oob="outerHTML"`,
			`id="` + switcherRowID(SwitcherAll) + `" hx-swap-oob="innerHTML"`,
		} {
			if !strings.Contains(body, part) {
				t.Errorf("%s: the answer lacks %s", name, part)
			}
		}
		if pop := renderRaw(t, fullTitlePopover(mirrorTitlePopoverID, mirrorViewHeading(v))); !strings.Contains(withoutOOB(body), pop) {
			t.Errorf("%s: the answer's full title is not the page's", name)
		}
		if name == "named" && !strings.Contains(body, `data-session-name="manual"`) {
			t.Errorf("named: the button does not draw the manual name:\n%s", body)
		}
		if name == "cleared" && (strings.Contains(body, renamedName) || !strings.Contains(body, ">mango:1<")) {
			t.Errorf("cleared: the button does not fall back to the label:\n%s", body)
		}
	}
}

// TestARenamedAgentIsRedrawnInPlace: on an agent's page the answer swaps the
// header's whole title box exactly as the page draws it — the new name, and,
// cleared, the slug — the agent menu's dialog and the switcher's, both
// prefilled with the name; the tab's title is the page's, focus the title.
func TestARenamedAgentIsRedrawnInPlace(t *testing.T) {
	for name, tc := range map[string]struct {
		n       sessionname.Name
		display string
	}{
		"named":   {sessionname.Name{Text: renamedName, Source: sessionname.SourceManual, Technical: "agent-7"}, renamedName},
		"cleared": {sessionname.Name{Text: "agent-7", Source: sessionname.SourceFallback}, "agent-7"},
	} {
		card := AgentCardView{ID: 7, Name: "agent-7", DisplayName: tc.display, SessionName: tc.n}
		v := AgentDetailView{ID: 7, Name: "agent-7", DisplayName: tc.display, SessionName: tc.n, Card: card,
			Switcher: NewSwitcher([]AgentCardView{card}, nil, nil, SwitcherCurrent{AgentID: 7})}
		body := expectRenamed(t, AgentPageRenamed(v), renderRaw(t, agentTitle(v)), agentDocTitle(v), AgentTitleID, AgentTitleID)
		for _, part := range []string{`id="` + AgentRenameID + `" hx-swap-oob="outerHTML"`, `id="` + SessionRenameID + `" hx-swap-oob="outerHTML"`,
			`<div id="session-drift" hx-swap-oob="delete"></div>`} {
			if !strings.Contains(body, part) {
				t.Errorf("%s: the answer lacks %s", name, part)
			}
		}
		if name == "named" && strings.Count(body, `value="`+renamedName+`" data-session-rename-input`) != 2 {
			t.Errorf("named: both rename dialogs are not prefilled with the new name:\n%s", body)
		}
		if name == "cleared" && strings.Contains(body, renamedName) {
			t.Errorf("cleared: the old name is still drawn:\n%s", body)
		}
	}
	if !strings.HasPrefix(agentDocTitle(AgentDetailView{Name: "agent-7", DisplayName: renamedName,
		SessionName: sessionname.Name{Text: renamedName, Source: sessionname.SourceManual}}), renamedName) {
		t.Fatal("control: the agent's tab is titled by its name")
	}
}

// TestARenamedTerminalIsRedrawnInPlace: a Terminal page (an agent's, a
// mirrored window's) has no #agent-title and no #mv-title: its answer swaps
// ITS header title (#term-title) exactly as the page draws it, sets ITS tab
// title ("Terminal · … · muster") and focuses it; no drift offer.
func TestARenamedTerminalIsRedrawnInPlace(t *testing.T) {
	named := sessionname.Name{Text: renamedName, Source: sessionname.SourceManual, Technical: "tmux main:1 @ atlas"}
	for name, v := range map[string]AgentTerminalView{
		"agent":          {Name: "agent-7", DisplayName: renamedName, SessionName: named},
		"mirror":         {Name: "main", DisplayName: "mango:1", RawName: "main", SessionName: named, Badge: "Mirrored · atlas", BadgeHost: "atlas"},
		"mirror cleared": {Name: "main", DisplayName: "mango:1", RawName: "main", Badge: "Mirrored · atlas", BadgeHost: "atlas"},
	} {
		page := renderRaw(t, AgentTerminalPage(v))
		head := renderRaw(t, terminalTitle(v))
		if !strings.Contains(page, head) || !strings.Contains(page, "<title>"+strings.ReplaceAll(terminalDocTitle(v), "&", "&amp;")+"</title>") {
			t.Fatalf("%s: control: the page draws terminalTitle and terminalDocTitle", name)
		}
		body := expectRenamed(t, TerminalRenamed(v), head, terminalDocTitle(v), TermTitleID, TermTitleID)
		if !strings.HasPrefix(terminalDocTitle(v), "Terminal · ") {
			t.Errorf("%s: the tab title %q is not the Terminal page's", name, terminalDocTitle(v))
		}
		for _, other := range []string{`id="` + AgentTitleID + `"`, `id="` + MirrorTitleID + `"`, `session-drift`} {
			if strings.Contains(body, other) {
				t.Errorf("%s: a Terminal page's answer swaps %s, which it has not", name, other)
			}
		}
		if name == "mirror cleared" && (strings.Contains(body, renamedName) || !strings.Contains(body, "mango:1")) {
			t.Errorf("cleared: the heading does not fall back to the label:\n%s", body)
		}
	}
}

// TestTheTitleIsTheNameExactly: a name holding a literal "&amp;" titles the
// tab as "&amp;", not "&": the title travels as a string (Renamed.Title, the
// muster:renamed event), and the answer holds no <title> for htmx to decode.
func TestTheTitleIsTheNameExactly(t *testing.T) {
	n := sessionname.Name{Text: "R&amp;D sync", Source: sessionname.SourceManual}
	rn := MirrorViewRenamed(MirrorViewView{Host: "atlas", Session: "main", Window: "@1", SessionName: n})
	if rn.Title != "R&amp;D sync · Mirrored · muster" {
		t.Fatalf("title %q", rn.Title)
	}
	if body := renderRaw(t, rn.Body); strings.Contains(body, "<title") {
		t.Fatalf("a <title> in the answer:\n%s", body)
	}
	if !strings.Contains(renderRaw(t, renameScript()), "document.title = d.title") {
		t.Fatal("the script does not set the title from the event's string")
	}
}

// TestEveryRenameSurfaceCarriesTheScript: the rename dialog and the drift
// offer each carry renameScript (whichever a page draws, the title and focus
// follow), bound once per document.
func TestEveryRenameSurfaceCarriesTheScript(t *testing.T) {
	script := renderRaw(t, renameScript())
	for name, n := range map[string]g.Node{
		"dialog": agentRenamePopover(AgentCardView{ID: 7, Name: "agent-7"}),
		"offer":  driftBanner(driftFixture()),
	} {
		if !strings.Contains(renderRaw(t, n), script) {
			t.Errorf("the %s does not carry the rename script", name)
		}
	}
	if !strings.Contains(script, "window.__musterRenamed") {
		t.Error("the script is not bound once")
	}
}

// TestTheFocusTargetsAreFocusable: where a rename moves focus is focusable
// by script on every page, whatever the title's state — the Rendered view's
// title BUTTON is disabled while it fits, so the heading around it is the
// target; each target carries tabindex="-1".
func TestTheFocusTargetsAreFocusable(t *testing.T) {
	n := sessionname.Name{Text: renamedName, Source: sessionname.SourceManual}
	for name, tc := range map[string]struct {
		html, id string
	}{
		"Rendered view": {renderRaw(t, mirrorViewTitle(MirrorViewView{Host: "atlas", SessionName: n})), MirrorHeadingID},
		"agent page":    {renderRaw(t, agentTitle(AgentDetailView{Name: "agent-7", SessionName: n})), AgentTitleID},
		"Terminal page": {renderRaw(t, terminalTitle(AgentTerminalView{Name: "agent-7", SessionName: n})), TermTitleID},
	} {
		open := regexp.MustCompile(`<[a-z0-9]+ id="` + tc.id + `"[^>]*>`).FindString(tc.html)
		if !strings.Contains(open, `tabindex="-1"`) || strings.Contains(open, "disabled") {
			t.Errorf("%s: the focus target %s is not script-focusable: %q", name, tc.id, open)
		}
	}
	if MirrorHeadingID == MirrorTitleID {
		t.Fatal("the Rendered view focuses the button it disables")
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

// TestThePencilIsDrawnOnce: the rename pencil's path is spelled in ONE place
// (pencilIcon), which the ⋯ menu's Rename… and the offer's label both use.
func TestThePencilIsDrawnOnce(t *testing.T) {
	src, err := os.ReadFile("session_name.go")
	if err != nil {
		t.Fatal(err)
	}
	const path = `M13.586 3.586a2 2 0 1 1 2.828 2.828`
	if n := strings.Count(string(src), path); n != 1 {
		t.Fatalf("the pencil's path is spelled %d times in session_name.go, want once (pencilIcon)", n)
	}
	for name, n := range map[string]g.Node{"menu item": sessionRenameMenuItem(), "offer": driftBanner(driftFixture())} {
		if !strings.Contains(renderRaw(t, n), path) {
			t.Errorf("control: the %s draws no pencil", name)
		}
	}
}
