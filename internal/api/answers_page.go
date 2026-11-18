package api

import (
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/askattempt"
)

// THE ANSWERS PAGE (docs/mirroring.md#answer-attempts): how answering
// question cards has gone — the success rate overall, by runtime and by
// question shape over the last 24 hours and 7 days; every failure grouped by
// its reason, with counts; and each failure expandable to the pane snapshot
// the host read and the card's timeline (every attempt on the same card). A
// failed card links to its own record (answersCardPath). Server-rendered,
// read-only, the owner's own rows (row-level security), as /usage.

const answersPath = "/answers"

// answersCardPath is the Answers page's timeline of card id: where a failed
// card's "What happened" link goes.
func answersCardPath(card string) string {
	return answersPath + "?card=" + url.QueryEscape(card)
}

// answersWindows are the windows the page reports.
var answersWindows = []struct {
	label, id string
	d         time.Duration
}{{"Last 24 hours", "24h", 24 * time.Hour}, {"Last 7 days", "7d", 7 * 24 * time.Hour}}

// answersFailuresShown bounds the failures listed under one reason.
const answersFailuresShown = 20

func (s *Server) registerAnswersRoutes(mux Mux) {
	mux.HandleFunc("GET "+answersPath, s.requireSession(s.handleAnswersPage))
}

func (s *Server) handleAnswersPage(w http.ResponseWriter, r *http.Request) {
	if s.ext.AskAttempts == nil {
		http.NotFound(w, r)
		return
	}
	esc := html.EscapeString
	now := s.now()
	problem := ""
	var b strings.Builder
	if card := r.URL.Query().Get("card"); card != "" {
		rows, err := s.ext.AskAttempts.ForCard(r.Context(), card)
		if err != nil {
			s.logger.Printf("answers: card timeline: %v", err)
			problem = "The attempts could not be read."
		}
		b.WriteString(`<section class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line" data-testid="answers-card" data-card="` + esc(card) + `" data-attempts="` + strconv.Itoa(len(rows)) + `">
<h1 class="text-balance text-base font-semibold text-fg">Answers to one question card</h1>
<p class="text-sm text-fg2">Every Submit and Dismiss on this card, oldest first.</p>
`)
		b.WriteString(problemNode(problem))
		if len(rows) == 0 && problem == "" {
			b.WriteString(`<p class="text-sm text-fg2">No attempts on this card are recorded (they are kept 30 days).</p>`)
		}
		b.WriteString(`<ol class="flex flex-col gap-2">`)
		for _, a := range rows {
			b.WriteString(answersAttemptHTML(a, true))
		}
		b.WriteString(`</ol>
<a href="` + answersPath + `" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">All answers</a>
</section>`)
		s.writeNavDocument(w, r, http.StatusOK, "Answers", "", false, b.String())
		return
	}
	longest := answersWindows[len(answersWindows)-1].d
	rows, err := s.ext.AskAttempts.List(r.Context(), now.Add(-longest), askattempt.MaxList)
	if err != nil {
		s.logger.Printf("answers: list: %v", err)
		problem = "The attempts could not be read."
	}
	b.WriteString(`<section class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line" data-testid="answers-page">
<h1 class="text-balance text-base font-semibold text-fg">Answers</h1>
<p class="text-sm text-fg2">How answering question cards from muster has gone: every Submit and Dismiss, from a card, the Attention list or your phone. A success rate counts answers (not dismissals, nor the same answer submitted twice). Rows are kept 30 days and hold the question's shape, never its text; a failure keeps the terminal as the host read it then, with secrets redacted.</p>
`)
	b.WriteString(problemNode(problem))
	for _, win := range answersWindows {
		sum := askattempt.Summarise(rows, now.Add(-win.d))
		b.WriteString(`<h2 class="text-sm font-semibold text-fg">` + win.label + `</h2>
<div class="` + flowsCardClass + `" data-testid="answers-rate-` + win.id + `" data-ok="` + strconv.Itoa(sum.Overall.OK) + `" data-total="` + strconv.Itoa(sum.Overall.Total) + `">
<span class="text-sm font-semibold text-fg">` + answersRateText(sum.Overall) + `</span>
`)
		for _, g := range []struct {
			title, attr string
			rows        []askattempt.Keyed
		}{{"By runtime", "runtime", sum.ByRuntime}, {"By question shape", "shape", sum.ByShape}} {
			if len(g.rows) == 0 {
				continue
			}
			b.WriteString(`<span class="text-xs font-medium text-fg2">` + g.title + `</span><ul class="flex flex-col gap-1">`)
			for _, k := range g.rows {
				b.WriteString(`<li class="text-xs text-fg" data-answers-` + g.attr + `="` + esc(k.Key) + `" data-window="` + win.id + `" data-ok="` +
					strconv.Itoa(k.Rate.OK) + `" data-total="` + strconv.Itoa(k.Rate.Total) + `">` + esc(k.Key) + ` · ` + answersRateText(k.Rate) + `</li>`)
			}
			b.WriteString(`</ul>`)
		}
		b.WriteString("</div>\n")
	}

	// Failures by reason, over the longest window.
	sum := askattempt.Summarise(rows, now.Add(-longest))
	b.WriteString(`<h2 class="text-sm font-semibold text-fg">What went wrong (` + answersWindows[len(answersWindows)-1].label + `)</h2>
<ul class="flex flex-col gap-2" data-testid="answers-failures">
`)
	if len(sum.Failures) == 0 {
		b.WriteString(`<li class="text-sm text-fg2">Nothing failed or was refused in this window.</li>`)
	}
	for _, f := range sum.Failures {
		info := askattempt.Reasons[f.Reason]
		b.WriteString(`<li class="` + flowsCardClass + `" data-answers-reason="` + esc(f.Reason) + `" data-count="` + strconv.Itoa(f.Count) + `">
<span class="text-sm font-semibold text-fg"><span class="font-mono">` + esc(f.Reason) + `</span> · ` + strconv.Itoa(f.Count) + ` · ` + esc(info.Outcome) + `</span>
<span class="text-xs text-fg2">` + esc(info.Text) + ` ` + esc(answersRecoveryText(info.Recovery)) + `</span>
<ol class="flex flex-col gap-1">`)
		shown := 0
		for _, a := range rows {
			if a.Reason != f.Reason || shown >= answersFailuresShown {
				continue
			}
			shown++
			b.WriteString(answersAttemptHTML(a, false))
		}
		b.WriteString("</ol></li>\n")
	}
	b.WriteString(`</ul>
<a href="` + usagePath + `" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Usage</a>
</section>`)
	s.writeNavDocument(w, r, http.StatusOK, "Answers", "", false, b.String())
}

// answersRateText is a rate as the page says it.
func answersRateText(r askattempt.Rate) string {
	if r.Total == 0 {
		return "no answers"
	}
	return strconv.Itoa(r.Percent()) + "% answered (" + strconv.Itoa(r.OK) + " of " + strconv.Itoa(r.Total) + ")"
}

// answersRecoveryText is what a card offers after a reason.
func answersRecoveryText(rec string) string {
	switch rec {
	case askattempt.RecoverRetry:
		return "The card offers Try again."
	case askattempt.RecoverMessage:
		return "The card offers Send as a message."
	}
	return ""
}

// answersAttemptHTML is one attempt, expandable to its snapshot (and, off a
// card's own timeline, a link to it).
func answersAttemptHTML(a askattempt.Attempt, onCard bool) string {
	esc := html.EscapeString
	id := strconv.FormatInt(a.ID, 10)
	where := a.Host + "/" + a.Session + a.Window
	open := ""
	if onCard && a.Outcome != askattempt.OutcomeOK {
		open = " open"
	}
	var b strings.Builder
	b.WriteString(`<li id="attempt-` + id + `" data-attempt="` + id + `" data-attempt-number="` + strconv.Itoa(a.Number) + `" data-reason="` + esc(a.Reason) +
		`" data-method="` + esc(a.Method) + `" data-door="` + esc(a.Door) + `"><details class="rounded-md bg-s2/50 px-2 py-1"` + open + `>
<summary class="cursor-pointer text-xs text-fg">` + esc(a.At.UTC().Format("2 Jan 15:04:05")) + ` · attempt ` + strconv.Itoa(a.Number) + ` · ` + esc(a.Action) + ` (` +
		esc(a.Method) + `) · ` + esc(a.Reason) + ` · ` + esc(a.Runtime) + ` · ` + esc(a.Door) + ` · ` + esc(where) + `</summary>
<p class="text-xs text-fg2">` + esc(askattempt.ShapeKey(a.Shape)) + ` · ` + strconv.Itoa(a.Keys) + ` key(s) · ` + strconv.FormatInt(a.Duration.Milliseconds(), 10) + ` ms</p>
`)
	if a.Snapshot != "" {
		b.WriteString(`<pre class="mt-1 max-h-80 overflow-auto whitespace-pre rounded bg-bg px-2 py-1 font-mono text-[11px] text-fg2" data-attempt-snapshot>` + esc(a.Snapshot) + `</pre>
`)
	} else if a.Outcome == askattempt.OutcomeFailed {
		b.WriteString(`<p class="text-xs text-muted">No snapshot: the host sent no screen with this outcome.</p>
`)
	}
	if !onCard && a.CardID != "" {
		b.WriteString(`<a href="` + esc(answersCardPath(a.CardID)) + `#attempt-` + id + `" class="text-xs text-accent underline" data-attempt-timeline>This card's timeline</a>
`)
	}
	b.WriteString("</details></li>\n")
	return b.String()
}
