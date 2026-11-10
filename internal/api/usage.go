package api

import (
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/chiefusage"
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
<span class="text-sm text-fg">` + num(t.Turns) + ` chief turns · ` + esc(tokensText(t.Tokens)) + `</span>
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
<span class="text-xs text-fg2">` + num(d.Messages) + ` messages (` + num(d.ByRule) + ` rule · ` + num(d.ByMuster) + ` muster · ` + num(d.ByChief) +
			` chief) · ` + num(d.Turns) + ` turns · ` + num(d.Tokens.Total()) + ` tokens</span>
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
	b.WriteString(`</ul>
<p class="text-xs text-fg2">A token total is a count, not a cost: a cache read costs far less than an input token. "Estimated tokens avoided" is an estimate, not a measurement: it multiplies a rule's hits by the average of your chief's turns for the same plugin and sender before the rule was approved.</p>
<a href="` + routingRulesPath + `" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Routing rules</a>
</section>`)
	s.writeNavDocument(w, r, http.StatusOK, "Usage", "", false, b.String())
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
