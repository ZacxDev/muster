package ui

import (
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
func switcherRenameDialog(p *switcherPin) g.Node {
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
	return renamePopover(d)
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
// and host the main UI no longer spells are found.
func sessionNameTitle(n sessionname.Name, class string) g.Node {
	return Span(
		g.If(class != "", Class(class)),
		g.Attr("data-mirror-name", ""),
		g.Attr("data-session-name", string(n.Source)),
		g.If(n.Technical != "", g.Attr("title", n.Technical)),
		g.Text(n.Text),
	)
}

// SessionDriftDismissPath is "Keep the name" (internal/api name_drift.go).
const SessionDriftDismissPath = "/sessions/name/drift-dismiss"

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

// driftBanner is the offer under a session page's header: what the session is
// about now, "Rename to “<offer>”" (a rename to it — the name becomes the
// person's) and "Keep the name" (dismissed until the focus changes again).
// Server-drawn with the page, so it moves nothing in after paint; nothing for
// no offer.
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
	const btn = "press inline-flex min-h-[44px] items-center rounded-lg px-3 text-xs font-semibold transition "
	return Div(
		ID("session-drift"),
		g.Attr("data-session-drift", d.Offer),
		hx("hx-boost", "false"),
		Class("flex flex-wrap items-center gap-x-2 gap-y-1 border-b border-line bg-s1 px-3 py-1 text-xs text-fg2"),
		Span(Class("min-w-0 flex-1 break-words"), g.Text("This session has moved on to “"+d.Offer+"”.")),
		Form(Method("post"), Action(d.RenameAction), hx("hx-post", d.RenameAction), hx("hx-swap", "none"),
			hidden([2]string{"name", d.Offer}),
			Button(Type("submit"), g.Attr("data-session-drift-accept", ""), Class(btn+"bg-accent text-on-accent hover:bg-accent/90"),
				g.Text("Rename to “"+d.Offer+"”"))),
		Form(Method("post"), Action(SessionDriftDismissPath), hx("hx-post", SessionDriftDismissPath), hx("hx-swap", "none"),
			hidden([2]string{"title", d.Offer}),
			Button(Type("submit"), g.Attr("data-session-drift-dismiss", ""), Class(btn+"text-fg2 ring-1 ring-inset ring-edge hover:bg-s2"),
				g.Text("Keep the name"))),
	)

// rawManual is raw — a manual name as typed — when n is a manual name, ""
// otherwise.
func rawManual(n sessionname.Name, raw string) string {
	if n.Source != sessionname.SourceManual {
		return ""
	}
	return raw
}
