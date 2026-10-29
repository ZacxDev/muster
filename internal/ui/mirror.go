package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"time"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/mirror"
)

// MirrorCardView is one MIRRORED entry on the Agents list: a tmux WINDOW (or,
// from a muster-mirror that registers per session, a whole session) on one of
// the operator's own machines, reported live by that machine's muster-mirror
// (internal/mirror). It is NOT an agent — there is no agents row, no chat,
// nothing to start, stop or delete — so it is its own card, not an
// AgentCardView with fields switched off.
type MirrorCardView struct {
	Host    string
	Session string
	// Window is the tmux window id the card is ("@12"); "" = a per-session
	// host's session. With Session it is the card's ADDRESS: what it links,
	// keys and ids by. Never the window's index, which tmux renumbers.
	Window string
	// Title is what the card is called when the host sent no Label:
	// "<session>:<window index>" for a window. "" = the session name.
	Title string
	// Label is the host's display name for the entry ("mango:1"), "" for
	// none. DISPLAY ONLY: the card shows Session beside the label so the real
	// name stays findable.
	Label string
	// Qualifier is what tells this entry apart from another listed entry with
	// the same title — its host, and/or its tmux address — "" when the title is
	// already unique. Decided in ONE place over the whole list
	// (mirror.SessionView.Qualifier); shown as part of the title.
	Qualifier string
	// Kind is what runs in the pane, as the list names it ("Claude Code",
	// "opencode", "shell", or a command); "" = unknown. Shown as a chip and
	// searched; Claude decides where the card opens.
	Kind string
	// Note is the host's line about the window (mirror.SessionView.AgentNote):
	// why it has no rendered view, or what its credential needs. "" for none.
	Note string
	// LiveCopy is the warning that the window's conversation also runs
	// elsewhere under another session id (mirror.SessionView.LiveCopyWarning),
	// "" for none. Drawn under the title (livecopy.go).
	LiveCopy string
	// Online: the host is connected and still lists the session.
	Online bool
	// Interactive: the host ANNOUNCED -interactive, so its terminal page offers
	// the Interactive switch (still behind a fresh sign-in: internal/stepup).
	// The card does not badge it; it is a filter field (mode:interactive).
	Interactive bool
	// ActiveAt is when the host last saw the entry active
	// (mirror.SessionView.Activity: Unix seconds, floored to a minute); zero =
	// the host did not say. The footer's "active … ago" prints it — the same
	// number the list is ordered by (mirror.Ordered).
	ActiveAt time.Time
	// Claude: the host reports Claude Code in the session's pane, so the card
	// opens the RENDERED view by default (MirrorViewPath); otherwise it opens the
	// terminal.
	Claude bool
	// AgentView: the host reports a non-Claude runtime with a view (Hermes)
	// in the pane AND names the conversation it shows
	// (mirror.SessionView.AgentView), so the card opens
	// its read-only rendered view by default, as a Claude Code card does.
	AgentView bool
	// Pinned: the viewing user pinned this entry's address (internal/pins). The
	// list draws a pinned entry first, as a label-only CHIP instead of its card
	// (pinned_chips.go); a card is only ever drawn for an unpinned entry.
	Pinned bool
	// Focus is what the Claude Code session in the window is working on
	// (internal/focus, keyed by its Claude session id), nil for none.
	Focus *FocusView
	// Repo is the short name of the repository the window works in
	// (mirror.SessionView.Repo: its git top level's name, else its cwd's),
	// "" when the host did not say (an older muster-mirror). A chip, and found
	// by the filter (`repo:` and free text). The card's NAME is unchanged.
	Repo string
	// Attention is the card's attention badge (cards_attention.go), nil for
	// none.
	Attention *CardAttention
	// Work is what the window's Claude Code session has running now
	// (mirror.SessionView.Work): its chip (work_chip.go), display only. The zero value — nothing running, or a host that did not
	// say — draws no chip.
	Work mirror.WindowWork
	// Color is the window's tmux identity colour (mirror.SessionView.Color,
	// lowercase #rrggbb), "" for none: the card's border and title are tinted
	// with it (identity_color.go). None — or anything that is not a wire
	// colour — draws the card exactly as before.
	Color string
	// HostIcon is the key of Host's icon (HostIconFor, composed in
	// internal/api from the host's registration rank); "" draws the hash
	// fallback.
	HostIcon string
	// Idle: drawn in the list's Idle section (idle_section.go), composed by
	// MarkIdle. Its card carries data-idle-section.
	Idle bool
}

// MirrorCardHref is where a mirrored card opens: the rendered view for a
// Claude Code pane or a Hermes one with a known session, the terminal for
// anything else.
func MirrorCardHref(m MirrorCardView) string {
	ref := MirrorRef{Host: m.Host, Session: m.Session, Window: m.Window}
	if m.Claude || m.AgentView {
		return ref.ViewPath()
	}
	return ref.TerminalPath()
}

// MirrorRef addresses one mirrored entry: a host, a tmux session on it, and —
// for a host that registers per window — the window's tmux id. Every URL of a
// mirrored entry is built from one, so the pages, their sockets and their
// composer cannot name different windows.
//
// Window == "" is "the session's ACTIVE window": the only address a
// per-session host has, and what every URL from before windows were
// addressable means.
type MirrorRef struct {
	Host    string
	Session string
	Window  string
}

// query is the ref as URL parameters. `window` is present only when set, so a
// per-session entry's URLs are byte-identical to what they always were.
func (r MirrorRef) query() string {
	v := url.Values{"host": {r.Host}, "session": {r.Session}}
	if r.Window != "" {
		v.Set("window", r.Window)
	}
	return v.Encode()
}

// TerminalPath is the entry's terminal page.
func (r MirrorRef) TerminalPath() string { return "/mirror/terminal?" + r.query() }

// TerminalWSPath is that page's socket, before the page adds its mode and size.
func (r MirrorRef) TerminalWSPath() string { return "/mirror/terminal/ws?" + r.query() }

// ScrollForm is the entry's address as the body of a scroll request
// (MirrorTerminalScrollPath), before the page adds `lines`.
func (r MirrorRef) ScrollForm() string { return r.query() }

// ViewPath is the entry's rendered view.
func (r MirrorRef) ViewPath() string { return "/mirror/view?" + r.query() }

// ViewWSPath is that page's transcript socket, before the page adds its resume
// point.
func (r MirrorRef) ViewWSPath() string { return "/mirror/view/ws?" + r.query() }

// MirrorTerminalPath is a mirrored session's terminal page (its active window).
func MirrorTerminalPath(host, session string) string {
	return MirrorRef{Host: host, Session: session}.TerminalPath()
}

// MirrorTerminalWSPath is that page's socket, before the page adds its mode and
// size.
func MirrorTerminalWSPath(host, session string) string {
	return MirrorRef{Host: host, Session: session}.TerminalWSPath()
}

// mirrorCardID is a stable element id for (host, session, window): session
// names are arbitrary text, so they are hashed rather than spelled into an id.
// A per-session card's id is what it always was.
func mirrorCardID(m MirrorCardView) string {
	key := m.Host + "\x00" + m.Session
	if m.Window != "" {
		key += "\x00" + m.Window
	}
	sum := sha256.Sum256([]byte(key))
	return "mirror-" + hex.EncodeToString(sum[:6])
}

// mirrorSessionTitle is a mirrored session's name as a page title: the
// host's label when it sent one, with the raw tmux session name as a small
// secondary text AND the tooltip; the session name alone otherwise.
//
// 🔴 THE RAW NAME STAYS VISIBLE, NOT ONLY IN A TOOLTIP: it is what an operator
// types into `tmux attach`, and a phone has no hover.
func mirrorSessionTitle(label, session, class string) g.Node {
	return mirrorEntryTitle(label, "", session, "", class)
}

// mirrorEntryTitle is mirrorSessionTitle for an entry that may be a WINDOW:
// title is what an entry with no label is called ("<session>:<window index>";
// "" = the session name). A window with no label needs no secondary text — its
// title already spells the session.
//
// qualifier ("" = none) is what tells the entry apart from another listed
// entry of the same name (mirror.SessionView.Qualifier). It is drawn AS PART
// OF THE TITLE, right after the name — "mango:1 · laptop" — not in a chip, so
// two "mango:1" cards read as two different titles. The rendered view and the
// terminal page draw their title here; the card through mirrorCardTitle, the
// same title without the raw session name.
func mirrorEntryTitle(label, title, session, qualifier, class string) g.Node {
	return entryTitle(label, title, session, qualifier, class, true)
}

// entryTitle is mirrorEntryTitle, drawing the muted session name beside a
// label only when withSession is set.
func entryTitle(label, title, session, qualifier, class string, withSession bool) g.Node {
	q := g.If(qualifier != "", Span(g.Attr("data-mirror-qualifier", ""), g.Text(mirrorQualifierSep+qualifier)))
	name, labelled := mirrorDisplayName(label, title, session)
	if !labelled {
		return Span(Class(class), g.Attr("data-mirror-name", ""), g.Text(name), q)
	}
	return Span(
		Class(class),
		g.Attr("data-mirror-name", ""),
		g.Attr("title", "tmux session "+session),
		Span(g.Attr("data-mirror-label", ""), g.Text(name)),
		q,
		g.If(withSession, Span(g.Attr("data-mirror-session", ""), Class("ml-1.5 text-xs text-muted"), g.Text(session))),
	)
}

// mirrorDisplayName is THE rule for what a mirrored entry is called, plained:
// its label when it has one that is not just the session's name (labelled),
// else its title, else its session. entryTitle draws it; the rendered view's
// <title> and its full-title popover (mirrorViewTitle) spell it from here too,
// so the three cannot drift apart.
func mirrorDisplayName(label, title, session string) (name string, labelled bool) {
	if label == "" || label == session {
		return plainOr(title, session), false
	}
	return plainOr(label, session), true
}

// mirrorQualifierSep joins a mirrored entry's name and its qualifier — the
// same separator as mirror.QualifierSep (this package does not import the
// plugin), so the titles here spell mirror.SessionView.DisplayTitle exactly.
const mirrorQualifierSep = " · "

// mirrorQualified is a mirrored entry's name as plain text (a <title>, the
// filter's haystack): the name, then its qualifier when it has one.
func mirrorQualified(name, qualifier string) string {
	if qualifier == "" {
		return name
	}
	return name + mirrorQualifierSep + qualifier
}

// mirrorBadge is the "Mirrored · <host>" badge beside a mirrored entry's
// title on both its pages (the card carries mirroredMark instead). It is NOT the generic grey chip:
// a muted chip among the kind/mode chips was easy to miss on a phone, and the
// host is what tells two same-named windows apart — so it is tinted, bold, and
// leads with the HOST'S icon (host_icons.go: the one its cards draw), hostIcon
// being its key and host what it names.
func mirrorBadge(text, host, hostIcon string) g.Node {
	d := hostIconDefOf(hostIcon, host)
	return Span(
		g.Attr("data-host-icon", d.Key),
		Class("inline-flex items-center gap-1 rounded-full bg-st-progress-bg px-2.5 py-1 text-xs font-semibold text-st-progress-fg ring-1 ring-inset ring-st-progress-fg/40"),
		g.Raw(hostIconSVG(d, "h-3.5 w-3.5")),
		g.Text(text),
	)
}

// podHost is a dispatched agent's host: the literal "cluster" — muster has no
// cluster name to put there, and naming it after a machine would merge "my
// tmux on that machine" with "pods on that cluster".
const podHost = "cluster"

// mirrorKind is what runs in a mirrored window's pane, as the palette finds it
// by: "Claude Code" for a Claude Code pane, else the host's reading of the
// command ("" = unknown).
func mirrorKind(m MirrorCardView) string {
	if m.Claude {
		return "Claude Code"
	}
	return m.Kind
}
