package ui

import (
	"io"
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/sessionname"
)

// A SESSION'S NAME (internal/sessionname; docs/focus.md#a-sessions-name): the
// stable, human name every surface heads a session with, resolved ONCE on the
// server (internal/api names.go) and carried on the views as SessionName.
//
//   - DRAWN through sessionTitle — the switcher's pill and the sidebar's row
//     (switcherName), the agent page's header and <title> (agentTitle,
//     agentDocTitle), the Rendered view's and the terminal page's header and
//     <title> (mirrorDisplayName) — so they cannot disagree.
//   - RENAMED from the session's ⋯ menu in the switcher bar, "Rename…"
//     (sessionRenameMenuItem), on every session page, and from the agent
//     page's own ⋯ menu: one dialog (renamePopover). A name the person types
//     is never overwritten by anything automatic; an empty one hands the
//     session back to its automatic name.

// The rename route for a MIRRORED session (internal/api session_names.go);
// an agent is renamed at its own POST /agents/{id}/name.
const SessionNamePath = "/sessions/name"

// The switcher ⋯ menu's rename item and the dialog it opens.
const (
	SessionRenameID        = "session-rename"
	SessionRenameItemID    = "switcher-rename"
	SessionRenameMenuLabel = "Rename…"
)

// sessionTitle is a session's name as a ui surface draws it: n.Text, which
// the server resolved and plained (internal/sessionname) — or, for a view
// built without a resolved name (a test's minimal view), displayName plained,
// else name as written: what every surface drew before the rule existed.
//
// 🔴 n.Text IS NEVER PLAINED HERE. The resolver applied the title rule once
// already, and the rule is not idempotent (textplain: "applied once").
func sessionTitle(n sessionname.Name, displayName, name string) string {
	if n.Text != "" {
		return n.Text
	}
	return plainOr(displayName, name)
}

// renameDialog is one rename dialog's content: its element id, where it
// posts, its heading, the hidden fields naming the session, the name to
// prefill (what the session is called now), a placeholder, and a line under
// the input.
type renameDialog struct {
	id, action, heading string
	fields              [][2]string
	current             string
	placeholder, note   string
	// oob draws the dialog as an out-of-band swap of the one on the page
	// (the drift offer's in-place answer, RenderMirrorDriftRenamed): the
	// prefill then reads the session's new name without a reload.
	oob bool
}

// renamePopover is the rename dialog: a popover (top layer, out of flow), a
// form posted by htmx whose answer reloads the page (HX-Refresh) so every
// surface reads the new name — and, without script, a plain form post the
// server answers with a redirect back. Placed near the top of the screen, like
// the palette, so a phone's keyboard does not cover it.
func renamePopover(d renameDialog) g.Node {
	hidden := make([]g.Node, 0, len(d.fields))
	for _, f := range d.fields {
		hidden = append(hidden, Input(Type("hidden"), Name(f[0]), Value(f[1])))
	}
	return Div(
		ID(d.id),
		g.If(d.oob, hx("hx-swap-oob", "outerHTML")),
		g.Attr("popover", "auto"),
		g.Attr("role", "dialog"),
		g.Attr("aria-label", d.heading),
		// NOT BOOSTED (as the tag editor): a post from inside a boosted page is
		// a boosted one, whose answer would swap the whole body.
		hx("hx-boost", "false"),
		Class("inset-x-0 bottom-auto top-[max(0.5rem,10vh)] mx-auto my-0 w-[min(28rem,calc(100vw-1rem))] rounded-xl border-0 bg-s1 p-0 text-fg shadow-2xl ring-1 ring-line backdrop:bg-black/40"),
		Form(
			Method("post"),
			Action(d.action),
			g.Attr("data-session-rename", ""),
			Class("flex flex-col gap-3 p-4"),
			hx("hx-post", d.action),
			hx("hx-swap", "none"),
			g.Group(hidden),
			Label(
				Class("flex flex-col gap-1 text-xs font-medium text-muted"),
				Span(g.Text(d.heading)),
				Input(
					Type("text"),
					Name("name"),
					Value(d.current),
					g.Attr("data-session-rename-input", ""),
					g.Attr("aria-label", "Session name"),
					g.Attr("autocomplete", "off"),
					g.Attr("maxlength", strconv.Itoa(SessionNameMaxRunes)),
					g.Attr("enterkeyhint", "done"),
					Placeholder(d.placeholder),
					// text-base: 16px, so a phone does not zoom the page on focus.
					Class("w-full rounded-lg border-0 bg-bg px-3 py-2 text-base text-fg ring-1 ring-inset ring-edge placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus"),
				),
			),
			g.If(d.note != "", Span(Class("break-all text-[11px] text-muted"), g.Text(d.note))),
			Div(
				Class("flex items-center gap-2"),
				Button(
					Type("submit"),
					g.Attr("data-session-rename-save", ""),
					Class("press inline-flex min-h-[44px] items-center justify-center rounded-xl bg-accent px-4 text-sm font-semibold text-on-accent transition hover:bg-accent/90 active:scale-[0.98] disabled:opacity-60"),
					g.Text("Save"),
				),
				Button(
					Type("button"),
					g.Attr("popovertarget", d.id),
					g.Attr("popovertargetaction", "hide"),
					Class("press inline-flex min-h-[44px] items-center justify-center rounded-xl px-4 text-sm font-medium text-fg2 ring-1 ring-inset ring-edge transition hover:bg-s2"),
					g.Text("Cancel"),
				),
			),
		),
	)
}

// SessionNameMaxRunes is the store's bound on a typed name
// (focus.MaxNameRunes — this package does not import the store); internal/api
// pins the two equal.
const SessionNameMaxRunes = 60

// renameNote is the line under a rename input: an empty name hands the session
// back to muster's automatic name; technical, when known, is the session's
// technical id (its slug, or its tmux address and host), so the dialog says
// which session it renames.
func renameNote(technical string) string {
	n := "Leave it empty to let muster name it."
	if technical != "" {
		n += " " + technical
	}
	return n
}

// sessionRenameMenuItem is the switcher ⋯ menu's "Rename…": it opens the
// session's rename dialog (switcherRenameDialog). A popover toggle, so it works
// without script; the menu closes when the dialog opens (popover="auto").
func sessionRenameMenuItem() g.Node {
	return Button(
		ID(SessionRenameItemID),
		Type("button"),
		g.Attr("role", "menuitem"),
		g.Attr("popovertarget", SessionRenameID),
		g.Attr("aria-haspopup", "dialog"),
		g.Attr("aria-controls", SessionRenameID),
		Class("press flex min-h-[44px] w-full items-center gap-2 rounded-lg px-3 text-left text-sm font-semibold text-fg transition hover:bg-s2 focus:bg-s2 focus:outline-none"),
		g.Raw(`<svg class="h-4 w-4 shrink-0" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true"><path d="M13.586 3.586a2 2 0 1 1 2.828 2.828l-.793.793-2.828-2.828.793-.793ZM11.379 5.793 3 14.172V17h2.828l8.38-8.379-2.83-2.828Z"/></svg>`),
		Span(g.Text(SessionRenameMenuLabel)),
	)
}

// switcherRenameDialog is the ⋯ menu's rename dialog for the page's own
// session: an agent is renamed at its own route, a mirrored session at
// SessionNamePath (by its window, whose conversation the name is for).
func switcherRenameDialog(p *switcherPin) g.Node { return renamePopover(switcherRenameFields(p)) }

// switcherRenameFields is switcherRenameDialog's content.
func switcherRenameFields(p *switcherPin) renameDialog {
	d := renameDialog{id: SessionRenameID, heading: "Rename session", current: p.name.Text,
		note: renameNote(p.name.Technical)}
	if p.rawManual != "" {
		// A manual name is edited as it was TYPED, not as it draws (the
		// agent page's own dialog does the same).
		d.current = p.rawManual
	}
	if !p.name.Human() {
		// A technical name is not prefilled: typing over "mango:1" is not
		// naming the session.
		d.current, d.placeholder = "", p.name.Text
	}
	if p.tagTarget.AgentID > 0 {
		d.action = "/agents/" + strconv.FormatInt(p.tagTarget.AgentID, 10) + "/name"
	} else {
		d.action, d.fields = SessionNamePath, append(p.tagTarget.fields(), [2]string{"back", p.back})
	}
	return d
}

// nameIfHuman is n's text when it is a human name, "" otherwise: a palette
// title word that is not the codename again.
func nameIfHuman(n sessionname.Name) string {
	if n.Human() {
		return n.Text
	}
	return ""
}

// sessionNameTitle is a human name as a header draws it: the name (already
// plained), its technical id as the tooltip — where the codename, tmux address
// and host the main UI no longer spells are found. extra is what a caller adds
// to the span: the Rendered view's header gives it its id (MirrorNameID), which
// the drift offer's in-place answer swaps.
func sessionNameTitle(n sessionname.Name, class string, extra ...g.Node) g.Node {
	return Span(
		g.Group(extra),
		g.If(class != "", Class(class)),
		g.Attr("data-mirror-name", ""),
		g.Attr("data-session-name", string(n.Source)),
		g.If(n.Technical != "", g.Attr("title", n.Technical)),
		g.Text(n.Text),
	)
}

// SessionDriftDismissPath is "Keep the name" (internal/api name_drift.go).
const SessionDriftDismissPath = "/sessions/name/drift-dismiss"

// The drift offer's elements and the field its Rename posts.
//
//   - SessionDriftID is the banner; SessionDriftStatusID the polite live
//     region beside it, which says what a tap did once the banner is gone.
//   - SessionDriftParam=1 is sent (hx-vals, so only by htmx) with the
//     banner's Rename: the rename routes then answer IN PLACE
//     (RenderAgentDriftRenamed, RenderMirrorDriftRenamed) rather than with the
//     reload (HX-Refresh) the rename dialog's Save is answered with.
const (
	SessionDriftID       = "session-drift"
	SessionDriftStatusID = "session-drift-status"
	SessionDriftParam    = "drift"
)

// driftExplain is the offer's tooltip: what the banner is, which the one-line
// banner no longer spells out.
const driftExplain = "This session's work has moved on from its name. Rename it to what it is about now, or keep the name: muster offers again only when the focus changes."

// SessionDrift is a session page's rename OFFER (internal/focus DriftOffer):
// its focus has moved on from its name, so the page offers the current focus
// title as the new name — one tap — or to keep the name. muster never renames
// on its own. Offer is the title offered; RenameAction the session's rename
// route; Fields the hidden fields naming the session (both forms post them);
// Back the page to return to without script.
type SessionDrift struct {
	Offer        string
	RenameAction string
	Fields       [][2]string
	Back         string
}

// driftBanner is the offer under a session page's header, nothing for no
// offer. Server-drawn with the page, so it moves nothing in after paint.
//
// 🔴 ONE LINE (operator: the stacked three-row card took too much height). A
// short muted label, the offered name (truncated with an ellipsis, whole in
// its tooltip), then the primary "Rename" and a quiet "Keep" text link (no
// fill, no ring, underlined on hover and keyboard focus). Each button is a
// 44px-tall target, so the banner is one 44px row; the label and the name
// shrink, the buttons never do. flex-wrap is a floor for a very narrow screen:
// when the name's box would fall under basis-40 the buttons take a second row,
// never the page's width (e2e/tests/session-names.spec.ts measures 1280 and
// 390 wide).
//
// Both are forms, so they work without script (a POST answered with a 303
// back). With htmx they post with hx-swap="none" and the answer is out of
// band: the banner is deleted, the status region says what happened, and — on
// a Rename — every place the page names the session is redrawn with the new
// name (RenderAgentDriftRenamed, RenderMirrorDriftRenamed). Nothing else on the
// page is touched: no reload, so the composer's draft, the stream and the
// scroll position stay as they were.
func driftBanner(d *SessionDrift) g.Node {
	if d == nil || d.Offer == "" {
		return nil
	}
	hidden := func(extra ...[2]string) g.Node {
		nodes := []g.Node{Input(Type("hidden"), Name("back"), Value(d.Back))}
		for _, f := range append(append([][2]string{}, d.Fields...), extra...) {
			nodes = append(nodes, Input(Type("hidden"), Name(f[0]), Value(f[1])))
		}
		return g.Group(nodes)
	}
	return g.Group{
		Div(
			ID(SessionDriftID),
			g.Attr("data-session-drift", d.Offer),
			g.Attr("role", "region"),
			g.Attr("aria-label", "Rename suggestion"),
			hx("hx-boost", "false"),
			Class("flex flex-wrap items-center gap-x-2 border-b border-line bg-s1 px-3 text-xs text-fg2"),
			P(Class("flex min-w-0 flex-1 basis-40 items-center gap-1.5"),
				Span(Class("shrink-0 cursor-help text-muted"), g.Attr("title", driftExplain), g.Text("Suggested name")),
				Span(g.Attr("data-session-drift-offer", ""), Class("min-w-0 truncate font-semibold text-fg"), g.Attr("title", d.Offer), g.Text(d.Offer)),
			),
			Div(Class("ml-auto flex shrink-0 items-center"),
				Form(Method("post"), Action(d.RenameAction), hx("hx-post", d.RenameAction), hx("hx-swap", "none"),
					hx("hx-vals", `{"`+SessionDriftParam+`":"1"}`), hx("hx-disabled-elt", "find button"),
					hidden([2]string{"name", d.Offer}),
					Button(Type("submit"), g.Attr("data-session-drift-accept", ""), g.Attr("aria-label", "Rename to “"+d.Offer+"”"),
						Class("press group inline-flex h-11 items-center rounded-md px-1 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus disabled:opacity-60"),
						Span(Class("rounded-md bg-accent px-2.5 py-1 font-semibold text-on-accent transition group-hover:bg-accent/90"), g.Text("Rename")))),
				Form(Method("post"), Action(SessionDriftDismissPath), hx("hx-post", SessionDriftDismissPath), hx("hx-swap", "none"),
					hx("hx-disabled-elt", "find button"),
					hidden([2]string{"title", d.Offer}),
					Button(Type("submit"), g.Attr("data-session-drift-dismiss", ""), g.Attr("aria-label", "Keep the current name"),
						Class("inline-flex h-11 min-w-[44px] items-center justify-center rounded-md px-2 font-semibold text-fg2 underline-offset-2 hover:text-fg hover:underline focus-visible:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus disabled:opacity-60"),
						g.Text("Keep"))),
			),
		),
		P(ID(SessionDriftStatusID), g.Attr("role", "status"), g.Attr("aria-live", "polite"), Class("sr-only")),
	}
}

// driftAnswered is what every in-place answer to the offer carries, out of
// band: the banner deleted, and status in the live region beside it.
func driftAnswered(status string) g.Node {
	return g.Group{
		Div(ID(SessionDriftID), hx("hx-swap-oob", "delete")),
		P(ID(SessionDriftStatusID), hx("hx-swap-oob", "innerHTML"), g.Text(status)),
	}
}

// RenderDriftKept is "Keep"'s answer for htmx: the banner goes, the live
// region says so. The page's name is unchanged, so nothing else is drawn.
func RenderDriftKept(w io.Writer) error { return driftAnswered("Kept the name.").Render(w) }

// switcherRenamed is the switcher's part of an in-place rename: its rows and
// counts (each pill names the session) and its ⋯ menu's rename dialog, whose
// prefill is the name. Nothing without a switcher.
func switcherRenamed(s *Switcher) g.Node {
	if s == nil {
		return nil
	}
	out := g.Group{switcherRefresh(s)}
	if s.pin != nil {
		d := switcherRenameFields(s.pin)
		d.oob = true
		out = append(out, renamePopover(d))
	}
	return out
}

// RenderAgentDriftRenamed is the offer's Rename answered in place on an
// agent's page, v the page's view with its NEW name (v.SessionName): out of
// band, the banner gone (driftAnswered), the header's title (agentTitle), the
// agent menu's rename dialog (v.Card) and the switcher (v.Switcher); in band,
// the document's <title> (htmx sets it from the answer). The same helpers the
// page is drawn with, so the in-place name cannot differ from a reload's.
func RenderAgentDriftRenamed(w io.Writer, v AgentDetailView) error {
	out := g.Group{
		driftAnswered("Renamed to “" + sessionTitle(v.SessionName, v.DisplayName, v.Name) + "”."),
		agentTitle(v, hx("hx-swap-oob", "outerHTML")),
		TitleEl(g.Text(agentDocTitle(v))),
	}
	if v.Card.ID != 0 {
		d := agentRenameFields(v.Card)
		d.oob = true
		out = append(out, renamePopover(d))
	}
	return append(out, switcherRenamed(v.Switcher)).Render(w)
}

// RenderMirrorDriftRenamed is the offer's Rename answered in place on a
// window's Rendered view, v the page's view with its NEW name: out of band,
// the banner gone, the header's name (MirrorNameID) and its full-title
// popover, and the switcher; in band, the document's <title>.
func RenderMirrorDriftRenamed(w io.Writer, v MirrorViewView) error {
	out := g.Group{
		driftAnswered("Renamed to “" + mirrorViewName(v) + "”."),
		// Only a human name is drawn by the span MirrorNameID names (a
		// rename always gives one).
		g.If(v.SessionName.Human(), sessionNameTitle(v.SessionName, "", ID(MirrorNameID), hx("hx-swap-oob", "outerHTML"))),
		fullTitlePopover(mirrorTitlePopoverID, mirrorViewHeading(v), hx("hx-swap-oob", "outerHTML")),
		TitleEl(g.Text(mirrorViewDocTitle(v))),
		switcherRenamed(v.Switcher),
	}
	return out.Render(w)
}

// rawManual is raw — a manual name as typed — when n is a manual name, ""
// otherwise.
func rawManual(n sessionname.Name, raw string) string {
	if n.Source != sessionname.SourceManual {
		return ""
	}
	return raw
}
