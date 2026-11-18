package ui

import (
	"strconv"

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

// routedFrom is who a routed paste is from, and the line under it, in a
// person's words.
func routedFrom(rb plugins.RoutedBlock) (from, line string) {
	who := map[plugins.RoutedVia]string{plugins.RoutedViaChief: "your chief", plugins.RoutedViaOwner: "you"}[rb.Via]
	if who == "" {
		who = "muster"
	}
	switch rb.Kind {
	case plugins.RoutedKindRequest:
		return "your chief", "Your request, passed on"
	case plugins.RoutedKindTask:
		return who, "Task #" + strconv.FormatInt(rb.TaskID, 10)
	}
	from = rb.Source
	if from == "" {
		from = "a connected app"
	}
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
	}
	return from, line
}

// routedPromptCard draws a routed paste (a mirrored session's prompt that
// plugins.ParseRoutedBlock read) as its card; raw is the prompt as the session
// received it (the ⓘ's), id the card's element id (the prompt's: the message
// index links to it), sheetID its ⓘ's.
func routedPromptCard(rb plugins.RoutedBlock, raw, id, sheetID string) g.Node {
	from, line := routedFrom(rb)
	return routedCard("prompt", id, "From "+from, line, rb.Subject, rb.Body,
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
		nodes = append(nodes, routedCard("answer", "", "From "+c.From, line, "", c.Body, sheet))
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
// no ID draws none), the line under it, the subject, and the body as plain
// text.
func routedCard(kind, id, heading, line, subject, body string, sheet InfoSheet) g.Node {
	return Div(
		g.If(id != "", ID(id)),
		g.Attr("data-routed-card", kind),
		Class("flex w-full max-w-[92%] min-w-0 flex-col gap-1 self-start rounded-2xl bg-s2 px-3 py-2 text-sm text-fg ring-1 ring-inset ring-line"),
		Div(
			Class("flex min-w-0 items-center gap-2"),
			Span(Class("shrink-0"), g.Attr("aria-hidden", "true"), g.Text("✉")),
			P(g.Attr("data-routed-from", ""), Class("min-w-0 flex-1 truncate font-semibold text-fg"), g.Text(heading)),
			g.If(sheet.ID != "", sheet.Node()),
		),
		g.If(line != "", P(g.Attr("data-routed-line", ""), Class("text-xs text-muted"), g.Text(line))),
		g.If(subject != "", P(g.Attr("data-routed-subject", ""), Class("font-medium text-fg"), g.Text(subject))),
		g.If(body != "", P(g.Attr("data-routed-body", ""), Class("max-h-72 overflow-y-auto whitespace-pre-wrap break-words text-fg2"), g.Text(body))),
	)
}
