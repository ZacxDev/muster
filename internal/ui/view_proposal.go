package ui

import (
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE CHIEF'S PROPOSAL CARD (docs/design/chief-in-agent-view.md, phase 2):
// ONE card for every kind of proposal (internal/api chief_proposal.go) — a
// succession now, ending a session next. Its heading, its settings and its
// body are the kind's; its buttons are the card's stored options (the kind's,
// then Not now), and they ARE the authorisation: nothing the chief proposed
// happens until the owner taps one. A real form (POST MirrorProposalPath) that
// the view's script submits in place (successionScript's wire).

// MirrorProposalPath is where a proposal card's buttons post the owner's tap.
const MirrorProposalPath = "/mirror/view/proposal"

// ProposalView is one proposal card as a view draws it.
type ProposalView struct {
	ID   int64
	Kind string
	// Heading is the question the card asks ("Start a new session that
	// continues this one?").
	Heading string
	// Settings are its label/value rows, drawn in order; a blank value is
	// left out.
	Settings [][2]string
	// BodyLabel and Body: what the action will carry, verbatim (a
	// succession's kickoff), in an open disclosure; "" for none.
	BodyLabel, Body string
	// Options are the buttons, in order; the first is the primary one (two,
	// when the card asks a choice of three).
	Options []string
}

// The classes of a proposal card's body (its kickoff) on the Attention view
// and the ask page, which internal/api draws (proposalBodyHTML). They live
// HERE because the stylesheet scans internal/ui, not that file: a class
// written only there would never be built.
const (
	ProposalBodyClass     = "flex min-w-0 flex-col gap-1"
	ProposalBodyNoteClass = "text-xs text-fg2"
	ProposalKickoffClass  = "max-h-80 overflow-auto whitespace-pre-wrap break-words rounded-xl bg-s1 px-3 py-2 font-mono text-xs text-fg ring-1 ring-line"
)

// The classes of what a builtin officer's ask is about (attention.ItemContext)
// on the Attention view and the ask page, which internal/api draws
// (officerContextHTML): muster's record of the item, and the source message
// in its own ringed box, apart from the officer's note. HERE for the same
// reason as the proposal's.
const (
	OfficerContextClass     = "flex min-w-0 flex-col gap-2"
	OfficerContextNoteClass = "break-words text-xs text-fg2"
	OfficerSourceClass      = "flex min-w-0 flex-col gap-1 rounded-xl bg-s1 px-3 py-2 ring-1 ring-line"
	OfficerSourceBodyClass  = "max-h-80 overflow-auto whitespace-pre-wrap break-words font-mono text-xs text-fg"
)

// proposalButtonClass styles the card's buttons.
const proposalButtonClass = "press inline-flex min-h-[44px] items-center rounded-lg px-3 text-sm font-semibold transition "

// ProposalCard is proposal p's card on the view at ref, extra being the
// attributes its kind's region reads (data-succession…).
func ProposalCard(ref MirrorRef, p ProposalView, extra ...g.Node) g.Node {
	hidden := func(n, v string) g.Node { return Input(Type("hidden"), Name(n), Value(v)) }
	var buttons []g.Node
	primary := 1
	if len(p.Options) > 2 {
		primary = 2
	}
	for i, o := range p.Options {
		cls := proposalButtonClass + "text-fg2 ring-1 ring-inset ring-edge hover:bg-s2"
		if i < primary {
			cls = proposalButtonClass + "bg-accent text-on-accent hover:bg-accent/90"
		}
		buttons = append(buttons, Button(Type("submit"), Name("choice"), Value(o), g.Attr("data-proposal-choice", o), Class(cls), g.Text(o)))
	}
	var settings []g.Node
	for _, kv := range p.Settings {
		if kv[1] == "" {
			continue
		}
		settings = append(settings, Div(Class("flex gap-2"), Span(Class("w-16 shrink-0 text-muted"), g.Text(kv[0])),
			Span(Class("min-w-0 break-words text-fg"), g.Text(kv[1]))))
	}
	var body g.Node
	if p.Body != "" {
		body = g.El("details", g.Attr("open", ""), Class("mt-1"),
			g.El("summary", Class("press cursor-pointer py-1 font-semibold text-fg2"), g.Text(p.BodyLabel)),
			Pre(ID("mv-proposal-body"), Class("max-h-[30dvh] overflow-auto whitespace-pre-wrap break-words rounded-lg bg-s2 p-2 font-mono text-[12px] text-fg"),
				g.Text(p.Body)))
	}
	return Section(append(extra,
		ID("mv-proposal-card"),
		g.Attr("data-proposal", strconv.FormatInt(p.ID, 10)),
		g.Attr("data-proposal-kind", p.Kind),
		g.Attr("aria-label", "Your chief's proposal"),
		Class("border-b border-line bg-s1 px-3 py-2 text-xs text-fg2"),
		P(Class("text-sm font-semibold text-fg"), g.Text(p.Heading)),
		P(Class("text-muted"), g.Text("Your chief proposed it. Nothing happens until you tap.")),
		Div(Class("mt-1 flex flex-col gap-0.5"), g.Group(settings)),
		body,
		Form(Method("post"), Action(MirrorProposalPath), ID("mv-proposal-form"), Class("mt-2 flex flex-wrap gap-2"),
			hidden("id", strconv.FormatInt(p.ID, 10)), hidden("host", ref.Host), hidden("session", ref.Session), hidden("window", ref.Window),
			g.Group(buttons)),
		P(ID("mv-proposal-error"), g.Attr("role", "alert"), g.Attr("hidden", ""), Class("mt-1 text-danger")),
	)...)
}
