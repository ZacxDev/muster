package ui

import (
	"io"
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// ---------------------------------------------------------------------------
// THE MORNING REPORT'S PAGE (internal/api morning_report.go; docs/features.md
// "Morning report"): what happened while the owner was away, in four parts —
// the questions waiting on them (the Asks queue's own cards, answerable here),
// what got done, what is stuck, and usage.
//
// 🔴 PLAIN WORDS IN THE TOP LAYER (the operator rule, info_sheet.go): a part
// whose source is off on this server says so in a sentence, and the setting
// behind it is in an ⓘ — withheld from anyone who is not an admin
// (Features.Setup). No variable, id or address is ever drawn outside one.
//
// Phone first: one column, the questions on top so they are above the fold,
// every link and button at least 44px tall.
// ---------------------------------------------------------------------------

// MorningReportView is one report as its page draws it.
type MorningReportView struct {
	// Missing: there is no report for this day (or none yet).
	Missing bool
	// Heading is the day ("Wednesday, November 18"); Window the night it
	// covers, in words.
	Heading, Window string
	// Quiet: nothing happened, so no push was sent.
	Quiet bool
	// QuestionsHTML is the open questions: the Asks queue's own <li> cards
	// (internal/api attentionItemsHTMLTo), "" for none; QuestionCount how many.
	QuestionsHTML string
	QuestionCount int
	Done, Stuck   []MorningLine
	Usage, Gaps   []MorningUsageLine
	// Notice is a refused answer's plain line ("" for none).
	Notice string
	// Setup: the viewer may see setup details (Features.Setup).
	Setup bool
}

// MorningLine is one line of Done or Stuck.
type MorningLine struct {
	Text, Detail, Href string
	// External: Href leaves muster (a pull request on GitHub).
	External bool
}

// MorningUsageLine is one usage or coverage sentence. Off: its source is off
// or unread; Setting is what turns it on, drawn in an ⓘ for an admin only.
type MorningUsageLine struct {
	Text    string
	Off     bool
	Setting string
}

// The page's test hooks.
const (
	MorningSectionQuestions = "morning-questions"
	MorningSectionDone      = "morning-done"
	MorningSectionStuck     = "morning-stuck"
	MorningSectionUsage     = "morning-usage"
)

// RenderMorningReport writes v's page body (the nav document's main).
func RenderMorningReport(w io.Writer, v MorningReportView) error {
	return morningReport(v).Render(w)
}

func morningReport(v MorningReportView) g.Node {
	if v.Missing {
		return Section(Class("flex flex-col gap-3 p-4"), g.Attr("data-testid", "morning-report"),
			H1(Class("text-base font-semibold text-fg"), g.Text("Morning report")),
			P(Class("text-sm text-fg2"), g.Attr("data-testid", "morning-missing"),
				g.Text("There is no report for this day. A report is written each morning when something happened overnight.")),
		)
	}
	intro := v.Window
	if v.Quiet {
		intro += " Nothing happened, so no notification was sent."
	}
	return Section(Class("mx-auto flex w-full max-w-2xl flex-col gap-4 p-4"), g.Attr("data-testid", "morning-report"),
		Div(Class("flex flex-col gap-1"),
			H1(Class("text-balance text-base font-semibold text-fg"), g.Text("While you were away")),
			P(Class("text-sm text-fg2"), g.Text(v.Heading+". "+intro)),
		),
		g.If(v.Notice != "", P(Class("rounded-lg bg-s2 px-3 py-2 text-sm text-fg"), g.Attr("role", "status"), g.Text(v.Notice))),
		morningQuestions(v),
		morningLines(MorningSectionDone, "Done overnight", "Nothing was finished overnight.", v.Done),
		morningLines(MorningSectionStuck, "Stuck", "Nothing is stuck.", v.Stuck),
		morningUsage(v),
	)
}

func morningHeading(text string, n int) g.Node {
	return H2(Class("flex items-center gap-2 text-sm font-semibold text-fg"), g.Text(text),
		g.If(n > 0, Span(Class("rounded-full bg-s2 px-2 text-xs font-medium text-fg2"), g.Text(strconv.Itoa(n)))))
}

func morningQuestions(v MorningReportView) g.Node {
	body := g.Node(P(Class("text-sm text-fg2"), g.Attr("data-testid", "morning-questions-none"), g.Text("Nothing is waiting on you.")))
	if v.QuestionsHTML != "" {
		body = Ul(Class(AsksFeedClass), g.Attr("data-asks-feed", ""), g.Attr("data-testid", "morning-question-cards"), g.Raw(v.QuestionsHTML))
	}
	return Section(Class("flex flex-col gap-2"), g.Attr("data-testid", MorningSectionQuestions), g.Attr("aria-label", "Waiting on you"),
		morningHeading("Waiting on you", v.QuestionCount), body)
}

func morningLines(id, heading, none string, lines []MorningLine) g.Node {
	var body g.Node = P(Class("text-sm text-fg2"), g.Text(none))
	if len(lines) > 0 {
		items := make([]g.Node, 0, len(lines))
		for _, l := range lines {
			items = append(items, morningLine(l))
		}
		body = Ul(Class("flex flex-col divide-y divide-line rounded-2xl bg-s1 ring-1 ring-inset ring-line"), g.Group(items))
	}
	return Section(Class("flex flex-col gap-2"), g.Attr("data-testid", id), g.Attr("aria-label", heading),
		morningHeading(heading, len(lines)), body)
}

func morningLine(l MorningLine) g.Node {
	inner := g.Group{
		Span(Class("min-w-0 truncate text-sm font-medium text-fg"), g.Text(l.Text)),
		g.If(l.Detail != "", Span(Class("min-w-0 truncate text-xs text-muted"), g.Text(l.Detail))),
	}
	cls := "flex min-h-[44px] min-w-0 flex-col justify-center px-3 py-2"
	if l.Href == "" {
		return Li(Div(Class(cls), inner))
	}
	return Li(A(Href(l.Href), Class(cls+" transition hover:bg-s2"),
		g.If(l.External, g.Group{Target("_blank"), Rel("noopener noreferrer")}), inner))
}

func morningUsage(v MorningReportView) g.Node {
	var items []g.Node
	for i, u := range append(append([]MorningUsageLine{}, v.Usage...), v.Gaps...) {
		items = append(items, morningUsageLine("morning-info-"+strconv.Itoa(i), u, v.Setup))
	}
	return Section(Class("flex flex-col gap-2"), g.Attr("data-testid", MorningSectionUsage), g.Attr("aria-label", "Usage"),
		morningHeading("Usage", 0),
		Ul(Class("flex flex-col gap-1"), g.Group(items)))
}

func morningUsageLine(id string, u MorningUsageLine, setup bool) g.Node {
	cls := "text-sm text-fg"
	if u.Off {
		cls = "text-sm text-fg2"
	}
	return Li(Class("flex min-h-[44px] items-center gap-1"), g.If(u.Off, g.Attr("data-morning-off", "")),
		Span(Class("min-w-0 flex-1 "+cls), g.Text(u.Text)),
		g.If(u.Setting != "", InfoSheet{ID: id, Label: "How to turn this on", Heading: "How to turn this on",
			Lead: "An admin turns this on in the server's settings:", Code: u.Setting, Withheld: !setup}.Node()),
	)
}
