// Package sessionname is THE RULE for what a session is called: one stable,
// human name for a dispatched agent or a mirrored Claude Code / opencode
// session, the same on every surface that names it — its sidebar row and
// switcher pill, its page's header and <title>, the terminal page, a push, an
// attention item, a dispatch card, and every chief tool that lists or names
// sessions (docs/focus.md#a-sessions-name).
//
// 🔴 ONE RULE, ONE PLACE. Resolve is the only place the precedence below is
// spelled; Agent and Mirror are its only two entry points, and every surface
// reaches them through internal/api's two adapters (names.go there), which a
// ledger test pins (TestTheSessionNameCallerLedger). A surface that reads an
// agent's DisplayName or a window's Label to NAME it is the bug this package
// exists to prevent: each such copy drifts from the others.
//
// The precedence, first non-empty wins:
//
//  1. MANUAL — the name the person gave the session: an agent's display name
//     they set (agents.NameManual), a mirrored session's name_manual. Never
//     overwritten by anything automatic.
//  2. SYSTEM — a name muster pinned for a role (the chief, an officer:
//     agents.NameSystem). It is what the rest of muster finds the role by, so
//     the auto name never replaces it.
//  3. DISPATCH — the name an agent was dispatched with from a task
//     ("#42 · Fix the login bug"). It IS the agent's stable name (operator
//     decision, #504 round 0 Q1): it never changes on its own — the auto name
//     never replaces it — and only a manual rename does.
//  4. AUTO — the name frozen from the session's first good focus title
//     (internal/focus names.go). Frozen once, never moved by a later title.
//     A task-dispatched agent's auto name is still frozen and stored; it is
//     simply never shown while the dispatch name ranks above it.
//  5. FALLBACK — the session's technical id: an agent's slug, a mirrored
//     window's host label or tmux address. Only a session with nothing above
//     is called by it.
//
// Every level but FALLBACK is plained with the title rule (textplain.Title),
// and one that plains to nothing falls through to the next. The fallback is
// an identifier the operator types and is never plained (ui.plainOr's rule).
package sessionname

import (
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/textplain"
)

// Source is which level of the precedence a Name came from.
type Source string

const (
	SourceManual   Source = "manual"
	SourceSystem   Source = "system"
	SourceAuto     Source = "auto"
	SourceDispatch Source = "dispatch"
	SourceFallback Source = "fallback"
)

// Name is a session's resolved name: Text, what every surface calls it, and
// Source, where it came from. Technical is the session's technical id, for the
// places that show one — the detail page, a tooltip, a chief tool that needs
// an address: an agent's slug; a mirrored window's "tmux <session>:<index> @
// <host>" (mirror.Where), led by its host label when it has one.
type Name struct {
	Text      string
	Source    Source
	Technical string
}

// Human reports whether n is a human name rather than a technical id: any
// source but SourceFallback.
func (n Name) Human() bool { return n.Source != SourceFallback && n.Text != "" }

// Parts are a session's candidate names, one per level ("" = none at that
// level), and its technical id.
type Parts struct {
	Manual, System, Auto, Dispatch string
	// Fallback is the technical name the session is called by when it has no
	// other: never plained.
	Fallback string
	// Technical is the session's technical id (Name.Technical).
	Technical string
}

// Resolve is the precedence (the package comment): the first level whose
// text plains to something, else the fallback as is.
func Resolve(p Parts) Name {
	for _, l := range []struct {
		text string
		src  Source
	}{{p.Manual, SourceManual}, {p.System, SourceSystem}, {p.Dispatch, SourceDispatch}, {p.Auto, SourceAuto}} {
		if t := textplain.Title(l.text); t != "" {
			return Name{Text: t, Source: l.src, Technical: p.Technical}
		}
	}
	return Name{Text: strings.TrimSpace(p.Fallback), Source: SourceFallback, Technical: p.Technical}
}

// Agent is dispatched agent a's name, given its stored name halves (its
// agent_focus row's; the zero value for none).
//
// a.DisplayName counts as MANUAL or SYSTEM by its DisplayNameSource
// (migration 0088), as DISPATCH when it was derived from a task, and is
// otherwise the slug copied — the fallback, as a.Name is.
func Agent(a agents.Agent, stored focus.Name) Name {
	// The fallback is the display name as stored — for a default source,
	// the slug copied — else the slug.
	p := Parts{Auto: stored.Auto, Fallback: a.DisplayName, Technical: a.Name}
	switch agents.ResolveDisplayNameSource(a.DisplayNameSource) {
	case agents.NameManual:
		p.Manual = a.DisplayName
	case agents.NameSystem:
		p.System = a.DisplayName
	case agents.NameDispatch:
		p.Dispatch = a.DisplayName
	default:
		// A default-sourced display name is the slug copied — the fallback.
		// One that is NOT the slug came from a writer that sets no source
		// (the e2e seed, a fixture, any future insert path): it is a title
		// like a task's, so it ranks as one: it heads the agent PLAINED, and
		// the auto name never replaces it. (Round 0 of #504 proposed deleting this arm as
		// unreachable; CI's e2e reached it at once — a markdown task name
		// drawn raw.)
		if a.DisplayName != a.Name {
			p.Dispatch = a.DisplayName
		}
	}
	if p.Fallback == "" {
		p.Fallback = a.Name
	}
	return Resolve(p)
}

// Mirror is mirrored entry e's name, given its conversation's stored name
// halves (the agent_focus row keyed by e.SessionKey(); the zero value when it
// has none, or its host reports no conversation).
//
// 🔴 THE HOST'S LABEL IS A FALLBACK, NOT A NAME. A window's Label is the
// codename its host's operator configured for the tmux SESSION
// (muster-mirror -labels-file) plus the window's index — "mango:1" — a
// technical address like the tmux one it stands for, shared by every
// conversation that window ever holds. So it ranks with the tmux address,
// below the conversation's own auto name, and is shown only while the
// conversation has no human name (decision recorded in docs/focus.md).
//
// The fallback is e.DisplayTitle(): the title WITH the list's qualifier when
// it needs one — how two "mango:1" windows on two hosts are told apart. A
// human name carries none (its tooltip has the place). The ui draws a
// fallback name from the view's own fields (Label, Title, Qualifier), never
// from this text, so it is not qualified twice.
func Mirror(e mirror.SessionView, stored focus.Name) Name {
	return Resolve(Parts{Manual: stored.Manual, Auto: stored.Auto, Fallback: e.DisplayTitle(), Technical: e.Where()})
}
