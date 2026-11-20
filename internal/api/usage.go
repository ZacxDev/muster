package api

import (
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/chiefusage"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE USAGE PAGE (docs/usage.md): what the owner's chief cost over the last 7
// or 30 days and what muster spared it — daily totals, the messages by source
// and by who handled them (a routing rule, one of muster's own paths, or a
// chief turn), the chief's tokens by what started its turns, the costliest
// senders, and each rule's hits with an estimate of the tokens it saved
// (chiefusage.RuleImpact). Server-rendered, read-only, the owner's own rows.

const usagePath = "/usage"

// usageWindows are the windows the page offers, in days; the first is the
// default.
var usageWindows = []int{7, 30}

func (s *Server) registerUsageRoutes(mux Mux) {
	mux.HandleFunc("GET "+usagePath, s.requireSession(s.handleUsagePage))
}

// usageDays is the request's window (?days=7|30), the default for anything else.
func usageDays(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("days"))
	for _, d := range usageWindows {
		if n == d {
			return d
		}
	}
	return usageWindows[0]
}

// num is n with thousands separators ("12,345").
// UsageTokensNote is what the totals' ⓘ says about the token counts: the
// footnote the page used to end with (the jargon audit's F070).
const UsageTokensNote = "A token total is a count, not a cost: a cache read costs far less than an input token. " +
	"\"Estimated tokens avoided\" is an estimate, not a measurement: it multiplies a rule's hits by the average of your chief's turns " +
	"for the same plugin and sender before the rule was approved."

// replyWord and messageWord are "reply"/"replies" and "message"/"messages"
// for n.
func replyWord(n int64) string {
	if n == 1 {
		return "reply"
	}
	return "replies"
}

func messageWord(n int64) string {
	if n == 1 {
		return "message"
	}
	return "messages"
}

func num(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// usageTriggerText names a turn trigger for the page.
func usageTriggerText(row chiefusage.TokenRow) string {
	switch row.Trigger {
	case chiefusage.TriggerPlugin:
		return "plugin " + row.Plugin
	case chiefusage.TriggerTaskEvent:
		return "task events (" + row.Plugin + ")"
	case chiefusage.TriggerOwnerChat:
		return "your chat lines"
	case chiefusage.TriggerView:
		return "your lines on a session's view"
	case chiefusage.TriggerAnswer:
		return "your answers to its questions"
	case chiefusage.TriggerHandBack:
		return "routes handed back"
	case chiefusage.TriggerSessionQuestion:
		return "your sessions' questions"
	case chiefusage.TriggerMachine:
		return "the machine message door"
	case chiefusage.TriggerKickoff:
		return "its first turn"
	case chiefusage.TriggerNudgeCheck:
		return "stall checks"
	}
	return "other"
}

// tokensText is t as one line.
func tokensText(t chiefusage.Tokens) string {
	return num(t.Total()) + " tokens (in " + num(t.Input) + " · out " + num(t.Output) + " · cache read " + num(t.CacheRead) +
		" · cache write " + num(t.CacheCreation) + ")"
}

func (s *Server) handleUsagePage(w http.ResponseWriter, r *http.Request) {
	if s.ext.ChiefUsage == nil {
		http.NotFound(w, r)
		return
	}
	days := usageDays(r)
	since := time.Now().UTC().AddDate(0, 0, -(days - 1))
	since = time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, time.UTC)
	rep, err := s.ext.ChiefUsage.Report(r.Context(), since)
	problem := ""
	if err != nil {
		s.logger.Printf("usage: report: %v", err)
		problem = "The usage could not be read."
	}
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<section class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line" data-testid="usage-page" data-days="` + strconv.Itoa(days) + `">
<h1 class="text-balance text-base font-semibold text-fg">Usage</h1>
<p class="text-sm text-fg2">What your chief cost, and how many plugin messages were placed without it. Tokens are the numbers its runtime reported for each turn.</p>
<nav class="flex flex-wrap gap-2" aria-label="Window">
`)
	for _, d := range usageWindows {
		cur := ""
		if d == days {
			cur = ` aria-current="page"`
		}
		b.WriteString(`<a href="` + usagePath + `?days=` + strconv.Itoa(d) + `"` + cur + ` class="inline-flex min-h-[44px] items-center text-sm text-accent underline" data-usage-window="` +
			strconv.Itoa(d) + `">Last ` + strconv.Itoa(d) + ` days</a>
`)
	}
	b.WriteString("</nav>\n")
	b.WriteString(problemNode(problem))
	t := rep.Totals
	b.WriteString(`<div class="` + flowsCardClass + `" data-testid="usage-totals" data-messages="` + strconv.FormatInt(t.Messages, 10) +
		`" data-by-rule="` + strconv.FormatInt(t.ByRule, 10) + `" data-by-muster="` + strconv.FormatInt(t.ByMuster, 10) +
		`" data-by-chief="` + strconv.FormatInt(t.ByChief, 10) + `" data-turns="` + strconv.FormatInt(t.Turns, 10) +
		`" data-tokens="` + strconv.FormatInt(t.Tokens.Total(), 10) + `">
<span class="text-sm font-semibold text-fg">` + num(t.Messages) + ` plugin messages · ` + strconv.FormatInt(t.WithoutChiefPercent(), 10) + `% without your chief</span>
<span class="text-sm text-fg">` + num(t.ByRule) + ` by a rule · ` + num(t.ByMuster) + ` by muster's own paths · ` + num(t.ByChief) + ` by a chief turn` +
		ruleFallbackText(t.Fallbacks) + `</span>
<div class="flex items-center gap-1"><span class="text-sm text-fg" data-usage-replies>` + num(t.Turns) + ` chief ` + replyWord(t.Turns) + `</span>` +
		ui.InfoSheetHTML(ui.InfoSheet{ID: "usage-tokens", Label: "Your chief's token counts", Heading: "Token counts",
			Lead: UsageTokensNote, Code: num(t.Turns) + " chief " + replyWord(t.Turns) + " · " + tokensText(t.Tokens), Inline: true}) + `</div>
`)
	if t.UnknownTurns > 0 {
		b.WriteString(`<span class="text-xs text-fg2">` + num(t.UnknownTurns) + ` of those turns reported no usage (counted as unknown, not free).</span>
`)
	}
	b.WriteString("</div>\n")

	section := func(title, testid string) {
		b.WriteString(`<h2 class="text-sm font-semibold text-fg">` + title + `</h2>
<ul class="flex flex-col gap-2" data-testid="` + testid + `">
`)
	}
	empty := func(n int, what string) {
		if n == 0 {
			b.WriteString(`<li class="text-sm text-fg2">` + what + `</li>`)
		}
	}

	section("By day", "usage-days")
	for i := len(rep.Days) - 1; i >= 0; i-- {
		d := rep.Days[i]
		b.WriteString(`<li class="` + flowsCardClass + `" data-usage-day="` + d.Day.Format("2006-01-02") + `">
<span class="text-sm font-semibold text-fg">` + d.Day.Format("Mon 2 Jan") + `</span>
<div class="flex items-center gap-1"><span class="text-xs text-fg2" data-usage-day-messages>` + num(d.Messages) + ` ` + messageWord(d.Messages) + `</span>` +
			ui.InfoSheetHTML(ui.InfoSheet{ID: "usage-day-" + d.Day.Format("2006-01-02"), Label: "Breakdown of " + d.Day.Format("Mon 2 Jan"), Heading: d.Day.Format("Mon 2 Jan"),
				Lead: "Placed by a rule, by muster's own paths and by your chief; your chief's replies and their tokens:",
				Code: num(d.ByRule) + " rule · " + num(d.ByMuster) + " muster · " + num(d.ByChief) + " chief · " + num(d.Turns) + " " + replyWord(d.Turns) + " · " + num(d.Tokens.Total()) + " tokens", Inline: true}) + `</div>
</li>
`)
	}
	b.WriteString("</ul>\n")

	section("Messages by source", "usage-sources")
	empty(len(rep.Sources), "No plugin messages in this window.")
	for _, src := range rep.Sources {
		b.WriteString(`<li class="` + flowsCardClass + `" data-usage-source="` + esc(src.Plugin) + `">
<span class="break-words text-sm font-semibold text-fg">` + esc(src.Plugin) + ` · ` + num(src.Messages) + `</span>
<span class="text-xs text-fg2">` + num(src.ByRule) + ` by a rule · ` + num(src.ByMuster) + ` by muster · ` + num(src.ByChief) + ` by a chief turn</span>
</li>
`)
	}
	b.WriteString("</ul>\n")

	section("Chief tokens by source", "usage-by-source")
	empty(len(rep.BySource), "No chief turns in this window.")
	for _, row := range rep.BySource {
		b.WriteString(`<li class="` + flowsCardClass + `" data-usage-trigger="` + esc(row.Trigger) + `" data-usage-plugin="` + esc(row.Plugin) + `">
<span class="break-words text-sm font-semibold text-fg">` + esc(usageTriggerText(row)) + ` · ` + num(row.Turns) + ` turns</span>
<span class="text-xs text-fg2">` + esc(tokensText(row.Tokens)) + `</span>
</li>
`)
	}
	b.WriteString("</ul>\n")

	section("Costliest senders", "usage-senders")
	empty(len(rep.TopSenders), "No plugin message reached your chief in this window.")
	for _, row := range rep.TopSenders {
		b.WriteString(`<li class="` + flowsCardClass + `" data-usage-sender="` + esc(row.Sender) + `">
<span class="break-words text-sm font-semibold text-fg">` + esc(row.Sender) + ` <span class="font-mono text-xs text-fg2">` + esc(row.Plugin) + `</span></span>
<span class="text-xs text-fg2">` + num(row.Turns) + ` turns · ` + esc(tokensText(row.Tokens)) + `</span>
</li>
`)
	}
	b.WriteString("</ul>\n")

	section("Rules", "usage-rules")
	empty(len(rep.Rules), "No approved routing rules.")
	for _, ri := range rep.Rules {
		est := "estimated tokens avoided: none to estimate from (no chief turn for these messages before the rule)"
		if ri.BeforeTurns > 0 {
			est = "estimated tokens avoided: about " + num(ri.Avoided) + " (an estimate, not a measurement: " + num(ri.AvgBefore) +
				" a turn, averaged over " + num(ri.BeforeTurns) + " turns before it)"
		}
		b.WriteString(`<li class="` + flowsCardClass + `" data-usage-rule="` + strconv.FormatInt(ri.RuleID, 10) + `" data-avoided="` + strconv.FormatInt(ri.Avoided, 10) + `">
<span class="break-words text-sm font-semibold text-fg">Rule #` + strconv.FormatInt(ri.RuleID, 10) + ` <span class="font-mono text-xs text-fg2">` + esc(ri.Plugin) + `</span> · ` + esc(ri.State) + `</span>
<span class="text-xs text-fg2">` + num(ri.Hits) + hitWord(ri.Hits) + ` in this window (` + num(ri.HitsAll) + ` in all) · ` + esc(est) + `</span>
</li>
`)
	}
	b.WriteString("</ul>\n")
	s.writeChiefSessionsUsage(r, &b, since)
	b.WriteString(`<a href="` + routingRulesPath + `" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Routing rules</a>
<a href="` + answersPath + `" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Answers</a>
</section>`)
	s.writeNavDocument(w, r, http.StatusOK, "Usage", "", false, b.String())
}

// writeChiefSessionsUsage writes the "Chief sessions on OpenRouter" section
// (docs/usage.md): today's spend against the daily cap, the window by day, and
// each session's model, turns, tokens and USD — from chief_session_llm_usage,
// the proxy's own booking of the provider-reported cost. Nothing when the
// server does not serve those sessions.
func (s *Server) writeChiefSessionsUsage(r *http.Request, b *strings.Builder, since time.Time) {
	cs := s.chiefSessions
	if cs == nil {
		return
	}
	rep, err := cs.store.SessionLLMReport(r.Context(), since)
	b.WriteString(`<h2 class="text-sm font-semibold text-fg">Chief sessions on OpenRouter</h2>
<div class="flex flex-col gap-2" data-testid="usage-chief-sessions">
`)
	if err != nil {
		s.logger.Printf("usage: chief sessions report: %v", err)
		b.WriteString(problemNode("The chief sessions' spend could not be read."))
		b.WriteString("</div>\n")
		return
	}
	capText := "no per-session cap"
	if cs.sessionUSD > 0 {
		capText = "a session may spend " + usd(cs.sessionUSD)
	}
	b.WriteString(`<div class="` + flowsCardClass + `" data-testid="usage-chief-sessions-today" data-spent="` + strconv.FormatFloat(rep.Today, 'f', 6, 64) +
		`" data-cap="` + strconv.FormatFloat(cs.dailyUSD, 'f', 2, 64) + `">
<span class="text-sm font-semibold text-fg">Today ` + usd(rep.Today) + ` of ` + usd(cs.dailyUSD) + ` (UTC day; resets at 00:00 UTC)</span>
<span class="text-xs text-fg2">` + capText + `. Booked from the cost OpenRouter reported for each request.</span>
</div>
<ul class="flex flex-col gap-2" data-testid="usage-chief-sessions-days">
`)
	for i := len(rep.Days) - 1; i >= 0; i-- {
		d := rep.Days[i]
		b.WriteString(`<li class="` + flowsCardClass + `" data-chief-sessions-day="` + d.Day.Format("2006-01-02") + `" data-usd="` +
			strconv.FormatFloat(d.CostUSD, 'f', 6, 64) + `">
<span class="text-sm text-fg">` + d.Day.Format("Mon 2 Jan") + ` · ` + usd(d.CostUSD) + ` · ` + num(d.Requests) + ` requests</span>
</li>
`)
	}
	b.WriteString(`</ul>
<ul class="flex flex-col gap-2" data-testid="usage-chief-sessions-list">
`)
	if len(rep.Sessions) == 0 {
		b.WriteString(`<li class="text-sm text-fg2">No chief session spent on OpenRouter in this window.</li>`)
	}
	esc := html.EscapeString
	for _, row := range rep.Sessions {
		b.WriteString(`<li class="` + flowsCardClass + `" data-chief-session="` + strconv.FormatInt(row.SessionID, 10) + `" data-usd="` +
			strconv.FormatFloat(row.CostUSD, 'f', 6, 64) + `" data-turns="` + strconv.FormatInt(row.Turns, 10) + `">
<span class="break-words text-sm font-semibold text-fg">Session #` + strconv.FormatInt(row.SessionID, 10) + ` <span class="font-mono text-xs text-fg2">` +
			esc(row.Model) + `</span> · ` + usd(row.CostUSD) + `</span>
<span class="text-xs text-fg2">` + num(row.Turns) + ` turns · ` + num(row.Requests) + ` requests · ` + esc(tokensText(row.Tokens)) + `</span>
</li>
`)
	}
	b.WriteString("</ul>\n</div>\n")
}

// usd is a dollar amount for the page: cents, or four places under a dollar.
func usd(v float64) string {
	if v < 1 {
		return "$" + strconv.FormatFloat(v, 'f', 4, 64)
	}
	return "$" + strconv.FormatFloat(v, 'f', 2, 64)
}

// ruleFallbackText is the totals line's note on rule fallbacks ("" for none).
func ruleFallbackText(n int64) string {
	if n == 0 {
		return ""
	}
	return " (" + num(n) + " matched a rule whose session was unavailable)"
}

// hitWord is " hit" for one, " hits" for any other count.
func hitWord(n int64) string {
	if n == 1 {
		return " hit"
	}
	return " hits"
}
