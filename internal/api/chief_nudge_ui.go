package api

import (
	"context"
	"html"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/textplain"
)

// THE "CHIEF NUDGES" TAB of the Asks page (docs/chief-nudges.md): every
// decision the stall sweep recorded for the viewer, newest first, with the
// repeat stallers on top. It is drawn while the feature is on.
//
// ⚠ ONLY CLASSES THE STYLESHEET ALREADY EMITS. This file is not one of
// Tailwind's content globs (tailwind.config.js), so every class here is copied
// from markup that is — answeredAsksHTML's card and the flows constants — and
// a new class would ship unstyled with css-check green.

const (
	asksTabNudges = "nudges"
	// chiefNudgesShown is how many decisions the tab lists.
	chiefNudgesShown = 50
)

// chiefNudgesPanelHTML is the tab's panel for ctx's principal.
func (s *Server) chiefNudgesPanelHTML(ctx context.Context, now time.Time) string {
	esc := html.EscapeString
	var b strings.Builder
	owner := requestOwner(ctx)
	if s.chiefNudgeLive(owner) {
		b.WriteString(`<p class="text-xs text-fg2" data-nudges-mode="live">Live: your chief nudges a stalled session only with a labelled message quoting one of your own messages to it, or asks you here and relays your answer word for word.</p>`)
	} else {
		b.WriteString(`<p class="text-xs text-fg2" data-nudges-mode="shadow">Shadow mode: your chief's decisions about your stalled sessions are recorded here. Nothing is sent and nothing is asked.</p>`)
	}
	stallers, err := s.ext.ChiefNudges.Stallers(ctx, now.Add(-chiefnudge.StallerWindow), chiefnudge.StallerThreshold)
	if err != nil {
		s.logger.Printf("chief nudges: stallers: %v", err)
	}
	if len(stallers) > 0 {
		b.WriteString(`<p class="pt-2 text-xs font-semibold uppercase tracking-wide text-fg2">Repeat stallers</p>
<ul class="flex flex-col gap-2" data-testid="nudges-stallers">`)
		for _, st := range stallers {
			b.WriteString(`<li class="` + flowsCardClass + `" data-nudge-staller="` + esc(st.Kind+":"+st.Key) + `"><p class="break-words text-sm text-fg"><span class="font-semibold">` +
				esc(nudgeWhere(textplain.Title(st.Title), st.Kind, st.Key, st.Host)) + `</span> · ` + strconv.Itoa(st.Count) + ` decisions in the last day</p></li>`)
		}
		b.WriteString(`</ul>`)
	}
	list, err := s.ext.ChiefNudges.List(ctx, chiefNudgesShown)
	if err != nil {
		s.logger.Printf("chief nudges: list: %v", err)
		b.WriteString(problemNode("The chief's nudge decisions could not be read."))
	}
	b.WriteString(`<p class="pt-2 text-xs font-semibold uppercase tracking-wide text-fg2">Decisions</p>
<ul class="flex flex-col gap-2" data-testid="nudges-list">`)
	if len(list) == 0 {
		b.WriteString(`<li class="text-sm text-fg2" data-nudges-empty>No decisions yet. The chief looks at a session once it has been idle for ` +
			esc(s.chiefNudgeIdle().String()) + `.</li>`)
	}
	for _, d := range list {
		b.WriteString(chiefNudgeCardHTML(d, now))
	}
	b.WriteString(`</ul>`)
	return b.String()
}

// nudgeSessionName is a session as the tab names it: its focus title and where.
func nudgeWhere(title, kind, key, host string) string {
	where := "agent " + key
	if kind == chiefnudge.KindMirror {
		where = "@ " + host
	}
	if strings.TrimSpace(title) == "" {
		return where
	}
	return title + " (" + where + ")"
}

// chiefNudgeCardHTML is one decision's card.
func chiefNudgeCardHTML(d chiefnudge.Decision, now time.Time) string {
	esc := html.EscapeString
	id := strconv.FormatInt(d.ID, 10)
	mode := "Shadow"
	if d.Mode == chiefnudge.ModeLive {
		mode = "Live"
	}
	var b strings.Builder
	b.WriteString(`<li id="nudge-` + id + `" class="` + flowsCardClass + `" data-nudge data-nudge-id="` + id + `" data-decision="` + esc(d.Decision) +
		`" data-outcome="` + esc(d.Outcome) + `" data-mode="` + esc(d.Mode) + `">
<span class="text-xs text-fg2"><span class="font-semibold" data-nudge-mode>` + mode + `</span> · <span data-nudge-decision>` + esc(nudgeDecisionLabel(d.Decision)) +
		`</span> · <time datetime="` + d.DecidedAt.UTC().Format(time.RFC3339) + `">` + esc(attentionAge(now.Sub(d.DecidedAt))) + `</time></span>
<p class="break-words text-sm text-fg"><span class="font-semibold" data-nudge-session>` + esc(nudgeWhere(textplain.Title(d.Title), d.Kind, d.Key, d.Host)) + `</span></p>
`)
	switch {
	case d.Message != "":
		label := "Would send: "
		if d.Mode == chiefnudge.ModeLive {
			label = "Message: "
		}
		b.WriteString(`<p class="whitespace-pre-wrap break-words text-sm text-fg" data-nudge-message>` + label + esc(d.Message) + `</p>
`)
	case d.Question != "":
		q := d.Question
		if len(d.Options) > 0 {
			q += "\nOptions: " + strings.Join(d.Options, " · ")
		}
		b.WriteString(`<p class="whitespace-pre-wrap break-words text-sm text-fg" data-nudge-question>Question: ` + esc(q) + `</p>
`)
	}
	if d.QuoteText != "" {
		when := ""
		if !d.QuoteAt.IsZero() {
			when = " · " + d.QuoteAt.UTC().Format(time.RFC3339)
		}
		b.WriteString(`<p class="break-words text-sm text-fg2" data-nudge-quote>Quote [` + esc(d.QuoteID) + esc(when) + `]: "` + esc(d.QuoteText) + `"</p>
`)
	}
	if d.Covered != "" {
		b.WriteString(`<p class="break-words text-sm text-fg2" data-nudge-covers>Covers: ` + esc(d.Covered) + `</p>
`)
	}
	if d.Reason != "" {
		b.WriteString(`<p class="break-words text-sm text-fg2" data-nudge-reason>Reason: ` + esc(d.Reason) + `</p>
`)
	}
	out := nudgeOutcomeLabel(d.Outcome)
	if d.Detail != "" {
		out += " — " + d.Detail
	}
	b.WriteString(`<p class="break-words text-sm text-fg" data-nudge-outcome>` + esc(out) + `</p>
</li>
`)
	return b.String()
}

func nudgeDecisionLabel(decision string) string {
	switch decision {
	case chiefnudge.DecisionNudge:
		return "Nudge"
	case chiefnudge.DecisionAsk:
		return "Ask you"
	case chiefnudge.DecisionLeave:
		return "Leave it"
	}
	return "Unreadable answer"
}

// nudgeOutcomeLabel is an outcome in words.
func nudgeOutcomeLabel(outcome string) string {
	if g, ok := strings.CutPrefix(outcome, chiefnudge.SuppressedPrefix); ok {
		switch g {
		case chiefnudge.GuardQuote:
			return "Suppressed: the quote is not in one of your messages to this session"
		case chiefnudge.GuardHourlyCap:
			return "Suppressed: the hourly cap was reached"
		case chiefnudge.GuardInvalid:
			return "Suppressed: the chief's answer could not be read"
		case chiefnudge.GuardNoQuestion:
			return "Suppressed: the chief asked nothing"
		case chiefnudge.GuardMovedOn:
			return "Suppressed: the session moved on first"
		case chiefnudge.GuardAskCap:
			return "Suppressed: your open asks are at their cap"
		case chiefnudge.GuardAskFailed:
			return "Suppressed: the ask could not be raised"
		}
		return "Suppressed: " + g
	}
	switch outcome {
	case chiefnudge.OutcomeWouldSend:
		return "Would have been sent (shadow mode)"
	case chiefnudge.OutcomeWouldAsk:
		return "Would have asked you (shadow mode)"
	case chiefnudge.OutcomeLeft:
		return "Left alone"
	case chiefnudge.OutcomeQueued:
		return "Queued for the session"
	case chiefnudge.OutcomeSent:
		return "Sent"
	case chiefnudge.OutcomeAsked:
		return "Asked you; waiting for your answer"
	case chiefnudge.OutcomeRelayQueued:
		return "Your answer is queued for the session"
	case chiefnudge.OutcomeRelayed:
		return "Your answer was sent into the session"
	case chiefnudge.OutcomeFailed:
		return "Not sent"
	case chiefnudge.OutcomeAskClosed:
		return "The ask closed"
	}
	return outcome
}
