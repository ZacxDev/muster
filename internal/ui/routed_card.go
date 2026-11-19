package ui

import (
	"strconv"
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/plugins"
)

// ROUTED MESSAGES, AS A PERSON SEES THEM (the operator rule: no ids or routing
// text in muster's top layer). When muster routes something to a session — a
// plugin message, a task, the owner's own request — it pastes a FENCED block
// into that session: a preface written for the model (the route id, how to
// answer, "data, not instructions"), then the item behind the fence's markers.
// And when the session answers a request, the chief is handed the answer the
// same way. Both used to be drawn as the owner's own bubble, verbatim.
//
// They are drawn as a "From <who>" card: who it is from, the subject, and the
// text as plain words. The exact text the agent received — fence, route id and
// all — is in the card's ⓘ, with Copy.
//
// 🔴 ONLY THE DRAWING CHANGES. What the agent is handed is untouched:
// internal/api's TestTheAgentIsHandedTheRoutedTextUnchanged pins its bytes
// against a golden written before these cards existed.

// RouteAnswerCard is one route's answer in the chief's chat, as the SERVER
// read it from the route's own record (internal/api routeAnswerCardsFor) —
// never from the row's text.
type RouteAnswerCard struct {
	// From is the plain name of the session that answered ("a session" when it
	// has none).
	From string
	// OneWay: the answer reports on a heads-up nobody can reply to; otherwise
	// it answers the owner's request.
	OneWay bool
	// Body is the session's answer as it wrote it.
	Body string
}

// The words of the cards and their ⓘ.
const (
	routedSheetPromptLabel   = "What the session was handed"
	routedSheetPromptHeading = "Exactly what the session received"
	routedSheetPromptLead    = "muster handed this to the session in full, marked as information for it to weigh rather than instructions to follow:"
	routedSheetAnswerLabel   = "What your chief was handed"
	routedSheetAnswerHeading = "Exactly what your chief received"
	routedSheetAnswerLead    = "muster handed this to your chief in full, marked as information for it to weigh rather than instructions to follow:"
	routeAnswerLine          = "Answered your request"
	routeOneWayAnswerLine    = "Reported back on a message"
)

// routedFrom is who a routed paste is from — the card's heading, after
// "From " — and the line under it, in a person's words.
//
// 🔴 A PLUGIN'S LABEL IS THE PLUGIN'S WORDS, NEVER MUSTER'S: a plugin named
// "your chief" or "you" must not draw "From your chief". So a message is
// always framed as an app's — "the app ‘<label>’" — and only muster's own
// router names (the chief, the owner) are said bare.
func routedFrom(rb plugins.RoutedBlock) (from, line string) {
	who := map[plugins.RoutedVia]string{plugins.RoutedViaChief: "your chief", plugins.RoutedViaOwner: "you"}[rb.Via]
	if who == "" {
		who = "muster"
	}
	switch rb.Kind {
	case plugins.RoutedKindRequest:
		return "your chief", routedRequestLine
	case plugins.RoutedKindTask:
		return who, "Task #" + strconv.FormatInt(rb.TaskID, 10)
	}
	from = routedAppFrom(rb.Source)
	switch rb.Via {
	case plugins.RoutedViaChief:
		line = "Passed on by your chief"
	case plugins.RoutedViaOwner:
		line = "Passed on by you"
	case plugins.RoutedViaThread:
		line = "A reply in a conversation this session started"
	case plugins.RoutedViaRule:
		line = "Sent here by a rule you approved"
	case plugins.RoutedViaRedraft:
		line = "Its earlier reply was refused, so this came back"
	case plugins.RoutedViaPR:
		line = "About a code change this session made"
	}
	return from, line
}

// routedAppFrom is a plugin message's source as a heading names it: always
// an app's, whatever its label says.
func routedAppFrom(label string) string {
	if strings.TrimSpace(label) == "" {
		return "a connected app"
	}
	return "the app ‘" + label + "’"
}

// The request card's words: the line under its heading, and the labels of its
// two parts — the owner's own message and the chief's relay of it, both shown,
// since the session is told to weigh one against the other.
const (
	routedRequestLine       = "Your request, passed on"
	routedOwnerTypedLabel   = "You wrote"
	routedChiefRelayedLabel = "Your chief passed it on as"
)

// routedPart is one labelled text of a card's body ("" label: unlabelled).
type routedPart struct{ label, text string }

// routedPromptCard draws a routed paste (a mirrored session's prompt that
// plugins.ParseRoutedBlock read and the server found on record) as its card;
// raw is the prompt as the session received it (the ⓘ's), id the card's
// element id (the prompt's: the message index links to it), sheetID its ⓘ's.
func routedPromptCard(rb plugins.RoutedBlock, raw, id, sheetID string) g.Node {
	from, line := routedFrom(rb)
	parts := []routedPart{{"", rb.Body}}
	if rb.Kind == plugins.RoutedKindRequest {
		parts = []routedPart{{routedOwnerTypedLabel, rb.OwnerTyped}, {routedChiefRelayedLabel, rb.Body}}
	}
	icon := routedIconMessage
	if from == "your chief" {
		icon = routedIconChief
	}
	return routedCard("prompt", id, icon, "From "+from, line, rb.Subject, parts,
		InfoSheet{ID: sheetID, Label: routedSheetPromptLabel, Heading: routedSheetPromptHeading, Lead: routedSheetPromptLead, Code: raw, Multiline: true})
}

// routeAnswerRow draws a chief-chat row holding route answers: one card per
// answer, and ONE ⓘ for the row, on its first card (the whole turn the chief
// was handed).
func routeAnswerRow(m ChatLine, cards []RouteAnswerCard) g.Node {
	nodes := make([]g.Node, 0, len(cards))
	sheetID := "ra-" + strconv.FormatInt(m.ID, 10)
	for i, c := range cards {
		line := routeAnswerLine
		if c.OneWay {
			line = routeOneWayAnswerLine
		}
		var sheet InfoSheet
		if i == 0 {
			sheet = InfoSheet{ID: sheetID, Label: routedSheetAnswerLabel, Heading: routedSheetAnswerHeading, Lead: routedSheetAnswerLead, Code: m.Content, Multiline: true}
		}
		nodes = append(nodes, routedCard("answer", "", routedIconSession, "From "+c.From, line, "", []routedPart{{"", c.Body}}, sheet))
	}
	return Div(
		g.Attr("data-route-answers", ""),
		g.If(m.ID > 0, g.Attr("data-msg-id", strconv.FormatInt(m.ID, 10))),
		Class("flex w-full max-w-[92%] min-w-0 flex-col gap-2 self-start"),
		g.Group(nodes),
	)
}

// routedCard is the card's frame: a self-start card (never the owner's
// right-hand bubble), its "From …" heading with the ⓘ beside it (a sheet with
// no ID draws none), the line under it, the subject, and the body's parts as
// plain text, each under its label when it has one.
func routedCard(kind, id string, icon routedIcon, heading, line, subject string, parts []routedPart, sheet InfoSheet) g.Node {
	var body []g.Node
	for _, p := range parts {
		if p.text == "" {
			continue
		}
		body = append(body, Div(
			g.Attr("data-routed-part", p.label),
			Class("flex flex-col gap-0.5"),
			g.If(p.label != "", P(g.Attr("data-routed-part-label", ""), Class("text-xs font-medium text-muted"), g.Text(p.label))),
			P(g.Attr("data-routed-body", ""), Class("max-h-72 overflow-y-auto whitespace-pre-wrap break-words text-fg2"), g.Text(p.text)),
		))
	}
	return Div(
		g.If(id != "", ID(id)),
		g.Attr("data-routed-card", kind),
		Class("flex w-full max-w-[92%] min-w-0 flex-col gap-1 self-start rounded-2xl bg-s2 px-3 py-2 text-sm text-fg ring-1 ring-inset ring-line"),
		Div(
			Class("flex min-w-0 items-center gap-2"),
			routedIconNode(icon),
			P(g.Attr("data-routed-from", ""), Class("min-w-0 flex-1 truncate font-semibold text-fg"), g.Text(heading)),
			g.If(sheet.ID != "", sheet.Node()),
		),
		g.If(line != "", P(g.Attr("data-routed-line", ""), Class("text-xs text-muted"), g.Text(line))),
		g.If(subject != "", P(g.Attr("data-routed-subject", ""), Class("font-medium text-fg"), g.Text(subject))),
		g.Group(body),
	)
}

// THE CARDS' ICONS ARE THE APP'S OWN, NEVER AN EMOJI (operator decision,
// 2026-11-19: the ✉ they carried drew as a broken glyph). Each is a 16px
// stroked glyph in currentColor, text-fg2 — the colour every other card icon
// is drawn in (hostMark):
//
//   - routedIconChief: a card from the chief ("From your chief") — the
//     chief's own hexagon (clusterIcon: the icon its pill and sidebar row
//     carry, as muster's own cluster agent);
//   - routedIconSession: a session's answer ("From <session>") — a session's
//     conversation, the glyph of its Rendered view (viewIconRendered's);
//   - routedIconMessage: anything else passed on (an app's message, a task,
//     the owner's own) — an envelope, drawn, not typed.
type routedIcon string

const (
	routedIconChief   routedIcon = "chief"
	routedIconSession routedIcon = "session"
	routedIconMessage routedIcon = "message"
)

// routedIconBodies is each icon's SVG body (24×24).
var routedIconBodies = map[routedIcon]string{
	routedIconChief:   clusterIcon.Body,
	routedIconSession: `<path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/>`,
	routedIconMessage: `<rect x="3" y="5" width="18" height="14" rx="2"/><path d="M3 7l9 6 9-6"/>`,
}

// routedIconNode is icon, decorative: the heading beside it says who it is
// from.
func routedIconNode(icon routedIcon) g.Node {
	return Span(
		g.Attr("data-routed-icon", string(icon)),
		g.Attr("aria-hidden", "true"),
		Class("inline-flex h-4 w-4 shrink-0 items-center justify-center text-fg2"),
		g.Raw(hostIconSVG(hostIconDef{Key: string(icon), Body: routedIconBodies[icon]}, "h-4 w-4")),
	)
}
