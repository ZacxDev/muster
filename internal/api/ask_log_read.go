package api

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/asklog"
	"github.com/ZacxDev/muster/internal/features"
	"github.com/ZacxDev/muster/internal/ui"
	g "maragu.dev/gomponents"
)

// ---------------------------------------------------------------------------
// READING THE ASK LOG (docs/asks.md, "Reading the log"): what the owner's
// agents asked them, how they answered and how long it took — so the
// questions an agent could have answered itself can be found.
//
//   - GET /api/asks/log      the owner's asks, newest first, filtered, paged
//   - GET /api/asks/summary  counts, time to answer, the most repeated questions
//   - GET /attention/log     the Asks page's Log tab, as a fragment
//   - /attention?tab=log     the Asks page drawn on its Log tab
//
// 🔴 THE OWNER'S OWN ROWS, AND ONLY BY ROW-LEVEL SECURITY. Every read runs as
// the request's principal on asks' forced policy (asklog.PG.Page/Summarize);
// nothing here names an owner or takes the system scope, and there is no
// admin read. The JSON routes take a PERSONAL token only (requireAPIToken):
// the shared hook token acts as the machine user, never as a person, so it
// is refused rather than answering the machine user's (empty) log as if it
// were the caller's. The tab is the signed-in person's.
// ---------------------------------------------------------------------------

const (
	asksTabLog = "log"
	// askLogPanelPath is the Log tab's fragment (the tab is drawn with a
	// placeholder unless the page was opened on it, so the Asks page's own
	// render reads nothing from the log).
	askLogPanelPath = attentionPath + "/log"
	apiAsksLogPath  = "/api/asks/log"
	apiAsksSumPath  = "/api/asks/summary"
	// askLogParamBefore is the page cursor: the id of the last row shown.
	askLogParamBefore = "before"
)

// askLogFilterParams are the query parameters a filter is read from, in the
// order the tab's links carry them.
var askLogFilterParams = []string{"kind", "runtime", "outcome", "answered_by", "host", "repo", "session", "since", "until"}

func (s *Server) registerAskLogReadRoutes(mux Mux) {
	mux.HandleFunc("GET "+apiAsksLogPath, s.requireAPIToken(s.handleAPIAsksLog))
	mux.HandleFunc("GET "+apiAsksSumPath, s.requireAPIToken(s.handleAPIAsksSummary))
	mux.HandleFunc("GET "+askLogPanelPath, s.requireSession(s.handleAskLogPanel))
}

// askLogStore is the log, nil while MUSTER_ASK_LOG is off.
func (s *Server) askLogStore() asklog.Store {
	if s.askLog == nil {
		return nil
	}
	return s.askLog.cfg.Store
}

// askLogTextTTL is how long the log keeps text.
func (s *Server) askLogTextTTL() time.Duration {
	if s.askLog == nil || s.askLog.cfg.TextTTL <= 0 {
		return asklog.DefaultTextTTL
	}
	return s.askLog.cfg.TextTTL
}

// parseAskLogWhen reads a since/until value: "" (open), an RFC 3339 instant,
// or an age before now — a Go duration ("36h") or whole days ("7d").
func parseAskLogWhen(v string, now time.Time) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" || v == "all" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if n, ok := strings.CutSuffix(v, "d"); ok {
		if d, err := strconv.Atoi(n); err == nil && d > 0 && d <= 3650 {
			return now.Add(-time.Duration(d) * 24 * time.Hour), nil
		}
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, errors.New(`want an RFC 3339 time, a duration ("36h") or days ("7d")`)
}

// parseAskLogQuery is q as a filter, a cursor and a page size.
func parseAskLogQuery(q url.Values, now time.Time) (f asklog.Filter, before int64, limit int, err error) {
	g := func(k string) string { return strings.TrimSpace(q.Get(k)) }
	f = asklog.Filter{Kind: g("kind"), Runtime: g("runtime"), Outcome: g("outcome"), AnsweredBy: g("answered_by"),
		Host: g("host"), Repo: g("repo"), Session: g("session")}
	if f.Since, err = parseAskLogWhen(g("since"), now); err != nil {
		return f, 0, 0, errors.New("since: " + err.Error())
	}
	if f.Until, err = parseAskLogWhen(g("until"), now); err != nil {
		return f, 0, 0, errors.New("until: " + err.Error())
	}
	if v := g(askLogParamBefore); v != "" {
		if before, err = strconv.ParseInt(v, 10, 64); err != nil || before <= 0 {
			return f, 0, 0, errors.New("before: want the id of a row")
		}
	}
	if v := g("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit <= 0 || limit > asklog.MaxPageSize {
			return f, 0, 0, errors.New("limit: want 1 to " + strconv.Itoa(asklog.MaxPageSize))
		}
	}
	if err := f.Validate(); err != nil {
		return f, 0, 0, errors.New(strings.TrimPrefix(err.Error(), asklog.ErrBadAsk.Error()+": "))
	}
	return f, before, limit, nil
}

// askLogRow is one ask as the API answers it.
type askLogRow struct {
	ID            int64           `json:"id"`
	Kind          string          `json:"kind"`
	Runtime       string          `json:"runtime"`
	Host          string          `json:"host"`
	Session       string          `json:"session"`
	Window        string          `json:"window"`
	ClaudeSession string          `json:"claude_session"`
	AgentID       int64           `json:"agent_id,omitempty"`
	Repo          string          `json:"repo"`
	Cwd           string          `json:"cwd"`
	Question      string          `json:"question"`
	QuestionNorm  string          `json:"question_norm"`
	Options       json.RawMessage `json:"options"`
	Answer        string          `json:"answer"`
	AnsweredBy    string          `json:"answered_by"`
	Provenance    string          `json:"provenance"`
	Outcome       string          `json:"outcome"`
	AskedAt       time.Time       `json:"asked_at"`
	AnsweredAt    *time.Time      `json:"answered_at"`
	AnswerSecs    *float64        `json:"answer_secs"`
	// TextExpired: the retention sweep cleared the row's text (question,
	// answer, options); the rest is kept.
	TextExpired bool `json:"text_expired"`
}

func askLogRowOf(a asklog.Ask) askLogRow {
	r := askLogRow{ID: a.ID, Kind: a.Kind, Runtime: a.Runtime, Host: a.Host, Session: a.TmuxSession, Window: a.Window,
		ClaudeSession: a.ClaudeSession, AgentID: a.AgentID, Repo: a.Repo, Cwd: a.Cwd, Question: a.Question, QuestionNorm: a.QuestionNorm,
		Options: a.Options, Answer: a.Answer, AnsweredBy: a.AnsweredBy, Provenance: a.Provenance, Outcome: a.Outcome,
		AskedAt: a.AskedAt.UTC(), TextExpired: !a.TextPurgedAt.IsZero()}
	if len(r.Options) == 0 {
		r.Options = json.RawMessage("[]")
	}
	if r.Outcome == "" {
		r.Outcome = asklog.OutcomeOpen
	}
	if r.AnsweredBy == "" {
		r.AnsweredBy = asklog.AnsweredByNone
	}
	if !a.AnsweredAt.IsZero() {
		at := a.AnsweredAt.UTC()
		secs := a.Latency().Seconds()
		r.AnsweredAt, r.AnswerSecs = &at, &secs
	}
	return r
}

// askLogRead answers the error a read failed with: 400 for a filter the
// reader refuses, else 500 (logged).
func (s *Server) askLogReadFailed(w http.ResponseWriter, what string, err error) {
	if errors.Is(err, asklog.ErrBadAsk) {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": strings.TrimPrefix(err.Error(), asklog.ErrBadAsk.Error()+": ")})
		return
	}
	s.logger.Printf("ask log: %s: %v", what, err)
	s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the ask log could not be read"})
}

// handleAPIAsksLog is GET /api/asks/log: the caller's asks, newest first.
func (s *Server) handleAPIAsksLog(w http.ResponseWriter, r *http.Request) {
	st := s.askLogStore()
	if st == nil {
		s.writeFeatureDisabled(w, r, features.AskLog)
		return
	}
	f, before, limit, err := parseAskLogQuery(r.URL.Query(), s.now())
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	rows, next, err := st.Page(r.Context(), f, before, limit)
	if err != nil {
		s.askLogReadFailed(w, "page", err)
		return
	}
	out := make([]askLogRow, 0, len(rows))
	for _, a := range rows {
		out = append(out, askLogRowOf(a))
	}
	body := map[string]any{"asks": out, "next": nil, "text_ttl_secs": int64(s.askLogTextTTL().Seconds())}
	if next > 0 {
		body["next"] = strconv.FormatInt(next, 10)
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, body)
}

// askLogTopRow is one repeated question as the API answers it.
type askLogTopRow struct {
	QuestionNorm    string    `json:"question_norm"`
	Question        string    `json:"question"`
	Count           int       `json:"count"`
	AnsweredByOwner int       `json:"answered_by_owner"`
	MedianSecs      float64   `json:"median_answer_secs"`
	TopAnswer       string    `json:"top_answer"`
	TopAnswerCount  int       `json:"top_answer_count"`
	LastAskedAt     time.Time `json:"last_asked_at"`
}

// handleAPIAsksSummary is GET /api/asks/summary: the caller's asks counted.
func (s *Server) handleAPIAsksSummary(w http.ResponseWriter, r *http.Request) {
	st := s.askLogStore()
	if st == nil {
		s.writeFeatureDisabled(w, r, features.AskLog)
		return
	}
	f, _, _, err := parseAskLogQuery(r.URL.Query(), s.now())
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	top := 0
	if v := strings.TrimSpace(r.URL.Query().Get("top")); v != "" {
		if top, err = strconv.Atoi(v); err != nil || top <= 0 || top > 50 {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "top: want 1 to 50"})
			return
		}
	}
	sum, err := st.Summarize(r.Context(), f, top)
	if err != nil {
		s.askLogReadFailed(w, "summary", err)
		return
	}
	tops := make([]askLogTopRow, 0, len(sum.Top))
	for _, q := range sum.Top {
		tops = append(tops, askLogTopRow{QuestionNorm: q.Norm, Question: q.Question, Count: q.Count, AnsweredByOwner: q.ByOwner, MedianSecs: q.MedianAnswer.Seconds(),
			TopAnswer: q.TopAnswer, TopAnswerCount: q.TopAnswerCount, LastAskedAt: q.LastAsked.UTC()})
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{
		"total":              sum.Total,
		"answered":           sum.Answered,
		"by_kind":            sum.ByKind,
		"by_outcome":         sum.ByOutcome,
		"by_answered_by":     sum.ByAnsweredBy,
		"median_answer_secs": sum.MedianAnswer.Seconds(),
		"p90_answer_secs":    sum.P90Answer.Seconds(),
		"top_questions":      tops,
		"text_ttl_secs":      int64(s.askLogTextTTL().Seconds()),
	})
}

// handleAskLogPanel is GET /attention/log: the Log tab's panel, for the page's
// script to put in place when the tab is first opened.
func (s *Server) handleAskLogPanel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(s.askLogPanelHTML(r.Context(), r.URL.Query())))
}

// askLogQuery is q's filter parameters (and before, when given), encoded: the
// query a Log link keeps.
func askLogQuery(q url.Values, before int64) url.Values {
	out := url.Values{}
	for _, k := range askLogFilterParams {
		if v := strings.TrimSpace(q.Get(k)); v != "" {
			out.Set(k, v)
		}
	}
	if before > 0 {
		out.Set(askLogParamBefore, strconv.FormatInt(before, 10))
	}
	return out
}

// askLogHref is the Asks page on its Log tab with q.
func askLogHref(q url.Values) string {
	v := url.Values{}
	for k, vs := range q {
		v[k] = vs
	}
	v.Set(asksTabParam, asksTabLog)
	return attentionPath + "?" + v.Encode()
}

// askLogLazyHTML is the Log tab's panel before it is opened: a placeholder the
// page's script fills from GET /attention/log, and a plain link to the page
// on the tab for a browser that runs no script.
func askLogLazyHTML(q url.Values) string {
	src := askLogPanelPath
	if e := askLogQuery(q, 0).Encode(); e != "" {
		src += "?" + e
	}
	return `<div class="flex flex-col gap-3" data-asklog-lazy data-src="` + html.EscapeString(src) + `">
<p class="px-1 text-sm text-fg2" data-asklog-loading><a class="underline" href="` + html.EscapeString(askLogHref(askLogQuery(q, 0))) + `">Open the ask log</a></p></div>`
}

// The labels the tab draws.
var (
	askKindLabels = map[string]string{
		// Plain words (the design review: "Picker" and "Attention ask" were
		// muster's own names for its kinds, not the reader's).
		asklog.KindPlainText: "Question in chat", asklog.KindAskUserQuestion: "Question with choices", asklog.KindAttention: "Agent's question",
		asklog.KindOpenCodeQuestion: "opencode question", asklog.KindOpenCodePermission: "opencode permission request",
	}
	// 🔴 THE LOG NEVER SAYS "Open". The page heading's "N open" is what waits
	// on you NOW (THE ONE OPEN COUNT, asks_count.go); a log row with no outcome
	// is history the log never saw end — a phone screenshot read "Asks · 0
	// open" over the log's "Open 3". So that row is "No answer": what the log
	// holds for it, in words that cannot be read as "waiting". The stored
	// value and the filter's URL value stay "open" (asklog.OutcomeOpen).
	askOutcomeLabels = map[string]string{
		asklog.OutcomeOpen: "No answer", asklog.OutcomeAnswered: "Answered", asklog.OutcomeAnsweredInSession: "Answered in session",
		asklog.OutcomeDeclined: "Declined", asklog.OutcomeMovedOn: "Moved on", asklog.OutcomeExpired: "Expired",
		asklog.OutcomeCancelled: "Cancelled", asklog.OutcomeSuperseded: "Superseded", asklog.OutcomeGone: "Gone",
		asklog.OutcomeUnknown: "Unknown",
	}
	// askOutcomeMeanings is each outcome in a sentence a newcomer gets: the
	// "How it ended" ⓘ lists them.
	askOutcomeMeanings = map[string]string{
		asklog.OutcomeOpen:              "The log never saw it end, and holds no answer. Not the same as waiting on you now.",
		asklog.OutcomeAnswered:          "It got an answer: from you, your chief or another session.",
		asklog.OutcomeAnsweredInSession: "You answered it in the session itself, not in muster.",
		asklog.OutcomeDeclined:          "You declined to answer.",
		asklog.OutcomeMovedOn:           "The session carried on without an answer.",
		asklog.OutcomeExpired:           "It timed out before anyone answered.",
		asklog.OutcomeCancelled:         "The agent withdrew it.",
		asklog.OutcomeSuperseded:        "A newer question replaced it before an answer.",
		asklog.OutcomeGone:              "Its session closed before any answer.",
		asklog.OutcomeUnknown:           "muster restarted before it saw how it ended.",
	}
	askByLabels = map[string]string{
		// "Someone else", not "Other": the outcomes' fold is "Other" (askOutcomeRows).
		asklog.ByOwner: "You", asklog.ByChief: "The chief", asklog.ByOther: "Someone else", asklog.AnsweredByNone: "Nobody",
	}
	// askByInline is who answered, inside a sentence ("by you").
	askByInline      = map[string]string{asklog.ByOwner: "you", asklog.ByChief: "the chief", asklog.ByOther: "someone else"}
	askRuntimeLabels = map[string]string{asklog.RuntimeClaudeCode: "Claude Code", asklog.RuntimeOpenCode: "opencode", asklog.RuntimeAgent: "Agent"}
	askSinceChoices  = []struct{ v, label string }{{"24h", "Last 24 hours"}, {"7d", "Last 7 days"}, {"30d", "Last 30 days"}, {"", "All kept"}}
)

func labelOr(m map[string]string, k string) string {
	if v, ok := m[k]; ok {
		return v
	}
	return k
}

// askDuration is d as the tab reads it: "12s", "3m 20s", "1h 5m", "2d 3h".
func askDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d/time.Second)) + "s"
	case d < time.Hour:
		m, sec := int(d/time.Minute), int(d%time.Minute/time.Second)
		if sec == 0 {
			return strconv.Itoa(m) + "m"
		}
		return strconv.Itoa(m) + "m " + strconv.Itoa(sec) + "s"
	case d < 48*time.Hour:
		h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
		if m == 0 {
			return strconv.Itoa(h) + "h"
		}
		return strconv.Itoa(h) + "h " + strconv.Itoa(m) + "m"
	}
	days, h := int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour)
	if h == 0 {
		return strconv.Itoa(days) + "d"
	}
	return strconv.Itoa(days) + "d " + strconv.Itoa(h) + "h"
}

// askLogPanelHTML is the Log tab's panel for ctx's principal, filtered by q.
func (s *Server) askLogPanelHTML(ctx context.Context, q url.Values) string {
	esc := html.EscapeString
	st := s.askLogStore()
	var b strings.Builder
	b.WriteString(`<div class="flex flex-col gap-3" data-testid="asklog-panel">`)
	if st == nil {
		b.WriteString(`<div class="` + ui.AsksCardClass + `" data-asklog-off>
<p class="text-sm font-semibold text-fg">The ask log is off</p>
<p class="text-sm text-fg2">Turn it on with <code class="font-mono text-xs text-fg">MUSTER_ASK_LOG=1</code> and restart muster. It then records every question your agents ask you, your answer and how long it took (` +
			`<a class="underline" href="` + esc(features.DocsURL) + `#ask-log">docs</a>).</p></div></div>`)
		return b.String()
	}
	now := s.now()
	f, before, _, err := parseAskLogQuery(q, now)
	if err != nil {
		b.WriteString(problemNode("That filter is not one the log takes (" + err.Error() + ")."))
		f, before = asklog.Filter{}, 0
		q = url.Values{}
	}
	b.WriteString(askLogFormHTML(q))
	sum, err := st.Summarize(ctx, f, 0)
	if err != nil {
		s.logger.Printf("ask log: summary: %v", err)
		b.WriteString(problemNode("The ask log could not be read.") + `</div>`)
		return b.String()
	}
	filtered := f != (asklog.Filter{})
	if sum.Total == 0 {
		if filtered {
			b.WriteString(`<p class="px-1 text-sm text-fg2" data-asklog-empty="filtered">No asks match these filters. <a class="underline" href="` +
				esc(askLogHref(url.Values{})) + `">Clear the filters</a></p></div>`)
		} else {
			b.WriteString(`<p class="px-1 text-sm text-fg2" data-asklog-empty="none">No asks recorded yet. Every question your agents ask you lands here, with your answer and how long it took.</p></div>`)
		}
		return b.String()
	}
	// 🔴 "How it ended" is the LOG's own breakdown — it adds up to its Total —
	// so its "No answer" is the log's rows with no outcome yet, not THE ONE
	// OPEN COUNT (the header, More and the ☰ say that one; audit of #787), and
	// it never says "Open" (askOutcomeLabels).
	b.WriteString(askLogSummaryHTML(sum, s.askLogTextTTL()))
	rows, next, err := st.Page(ctx, f, before, asklog.DefaultPageSize)
	if err != nil {
		s.logger.Printf("ask log: page: %v", err)
		b.WriteString(problemNode("The ask log could not be read.") + `</div>`)
		return b.String()
	}
	// No count beside it: the "asks" tile above already says how many.
	b.WriteString(ui.AsksSectionLabel("asklog-asks", "Every ask, newest first") + `
<ul class="` + ui.AsksGroupListClass + `" aria-labelledby="asklog-asks" data-testid="asklog-list">`)
	for _, a := range rows {
		b.WriteString(askLogCardHTML(a, now, s.askLogTextTTL()))
	}
	if len(rows) == 0 {
		b.WriteString(ui.AsksEmptyHTML(` data-asks-empty="log-page" data-asklog-empty="page"`, "", "", "No older asks.", ""))
	}
	b.WriteString(`</ul>`)
	var pager []string
	if before > 0 {
		pager = append(pager, `<a class="`+ui.AsksSecondaryButtonClass+`" href="`+esc(askLogHref(askLogQuery(q, 0)))+`" data-asklog-newest>Newest</a>`)
	}
	if next > 0 {
		pager = append(pager, `<a class="`+ui.AsksSecondaryButtonClass+`" href="`+esc(askLogHref(askLogQuery(q, next)))+`" data-asklog-older>Older</a>`)
	}
	if len(pager) > 0 {
		b.WriteString(`<nav class="` + ui.AsksActionsClass + `" aria-label="Ask log pages">` + strings.Join(pager, "") + `</nav>`)
	}
	b.WriteString(`</div>`)
	return b.String()
}

// askLogFormHTML is the filter form: kind, outcome, who answered and the time
// window on top; runtime, host, repository and session behind "More filters"
// (open when one of them is set). A GET to the Asks page on its Log tab.
func askLogFormHTML(q url.Values) string {
	esc := html.EscapeString
	sel := func(name, label string, choices []struct{ v, label string }) string {
		cur := strings.TrimSpace(q.Get(name))
		var b strings.Builder
		b.WriteString(`<label class="` + ui.AsksLogFieldClass + `">` + esc(label) + `<select name="` + name + `" class="` + ui.AsksLogInputClass + `" data-asklog-filter="` + name + `">`)
		for _, c := range choices {
			b.WriteString(`<option value="` + esc(c.v) + `"`)
			if c.v == cur {
				b.WriteString(` selected`)
			}
			b.WriteString(`>` + esc(c.label) + `</option>`)
		}
		b.WriteString(`</select></label>`)
		return b.String()
	}
	anyChoice := struct{ v, label string }{"", "Any"}
	kinds := []struct{ v, label string }{anyChoice}
	for _, k := range asklog.Kinds {
		kinds = append(kinds, struct{ v, label string }{k, askKindLabels[k]})
	}
	outcomes := []struct{ v, label string }{anyChoice}
	for _, o := range asklog.FilterOutcomes {
		outcomes = append(outcomes, struct{ v, label string }{o, askOutcomeLabels[o]})
	}
	bys := []struct{ v, label string }{anyChoice}
	for _, w := range []string{asklog.ByOwner, asklog.ByChief, asklog.ByOther, asklog.AnsweredByNone} {
		bys = append(bys, struct{ v, label string }{w, askByLabels[w]})
	}
	runtimes := []struct{ v, label string }{anyChoice}
	for _, rt := range []string{asklog.RuntimeClaudeCode, asklog.RuntimeOpenCode, asklog.RuntimeAgent} {
		runtimes = append(runtimes, struct{ v, label string }{rt, askRuntimeLabels[rt]})
	}
	since := append([]struct{ v, label string }{}, askSinceChoices...)
	if cur := strings.TrimSpace(q.Get("since")); cur != "" && labelOrSince(cur) == "" {
		since = append(since, struct{ v, label string }{cur, "Since " + cur})
	}
	text := func(name, label, ph string) string {
		return `<label class="` + ui.AsksLogFieldClass + `">` + esc(label) + `<input type="text" name="` + name + `" value="` + esc(strings.TrimSpace(q.Get(name))) +
			`" placeholder="` + esc(ph) + `" autocomplete="off" autocapitalize="off" spellcheck="false" class="` + ui.AsksLogInputClass + `" data-asklog-filter="` + name + `"></label>`
	}
	// 🔴 EVERY FILTER IS BEHIND ONE "Filters" TOGGLE, SHUT (the design review:
	// eight fields filled the tab's first screen). The toggle says how many
	// are set, so a filtered log never reads as the whole log.
	set := 0
	for _, k := range askLogFilterParams {
		if strings.TrimSpace(q.Get(k)) != "" {
			set++
		}
	}
	label := "Filters"
	if set > 0 {
		label += " (" + strconv.Itoa(set) + ")"
	}
	return `<details class="` + ui.AsksDetailsClass + `" data-asklog-filters><summary class="` + ui.AsksSecondaryButtonClass + ` cursor-pointer list-none [&::-webkit-details-marker]:hidden" data-asklog-filters-toggle>` + esc(label) + `</summary>
<form method="get" action="` + attentionPath + `" class="flex flex-col gap-2 pt-2" data-asklog-form aria-label="Filter the ask log">
<input type="hidden" name="` + asksTabParam + `" value="` + asksTabLog + `">
<div class="` + ui.AsksLogFormClass + `">` + sel("kind", "Kind", kinds) + sel("outcome", "Outcome", outcomes) + sel("answered_by", "Answered by", bys) +
		sel("since", "When", since) + sel("runtime", "Runtime", runtimes) + text("host", "Host", "any") + text("repo", "Repository", "owner/name") +
		text("session", "Session", "tmux or session id") + `</div>
<div class="` + ui.AsksActionsClass + `"><button type="submit" class="` + ui.AsksPrimaryButtonClass + `">Apply</button>` +
		`<a class="` + ui.AsksSecondaryButtonClass + `" href="` + esc(askLogHref(url.Values{})) + `">Clear</a></div>
</form></details>`
}

// labelOrSince is the picker's label for a since value it offers, "" for one
// it does not (an RFC 3339 time typed into the address).
func labelOrSince(v string) string {
	for _, c := range askSinceChoices {
		if c.v == v {
			return c.label
		}
	}
	return ""
}

// askLogSummaryHTML is the summary: four tiles, the counts, and the questions
// asked most often.
func askLogSummaryHTML(sum asklog.Summary, ttl time.Duration) string {
	esc := html.EscapeString
	var b strings.Builder
	tile := func(key, value, label string) {
		b.WriteString(`<div class="` + ui.AsksLogStatClass + `" data-asklog-stat="` + key + `"><span class="` + ui.AsksLogStatValueClass + `">` + esc(value) +
			`</span><span class="` + ui.AsksSubClass + `">` + esc(label) + `</span></div>`)
	}
	lat := func(d time.Duration) string {
		if sum.Answered == 0 {
			return "—"
		}
		return askDuration(d)
	}
	b.WriteString(`<section class="flex flex-col gap-2" aria-label="Summary" data-asklog-summary><div class="` + ui.AsksLogStatsClass + `">`)
	tile("total", strconv.Itoa(sum.Total), "asks")
	tile("owner", strconv.Itoa(sum.ByAnsweredBy[asklog.ByOwner]), "answered by you")
	tile("median", lat(sum.MedianAnswer), "median to answer")
	tile("p90", lat(sum.P90Answer), "90th percentile")
	b.WriteString(`</div>`)
	// THE BREAKDOWNS, AS PLAIN STATISTICS: a heading (the page's one heading
	// style) over "label … N" rows. No pill: nothing here is a control.
	counts := func(key, label string, rows []askCountRow, info string) {
		head := ui.AsksSectionLabel("asklog-counts-"+key, label)
		if info != "" {
			head = `<div class="` + ui.AsksLogSectionHeadClass + `">` + head + info + `</div>`
		}
		b.WriteString(`<div class="flex min-w-0 flex-col gap-1" data-asklog-counts="` + key + `">` + head +
			`<dl class="` + ui.AsksLogCountsClass + `" aria-labelledby="asklog-counts-` + key + `">`)
		for _, r := range rows {
			b.WriteString(`<div class="` + ui.AsksLogCountRowClass + `" data-count="` + esc(r.key) + `"><dt class="` + ui.AsksLogCountLabelClass + `">` + esc(r.label) +
				`</dt><dd class="` + ui.AsksLogCountValueClass + `">` + strconv.Itoa(r.n) + `</dd></div>`)
		}
		b.WriteString(`</dl></div>`)
	}
	outcomes, folded := askOutcomeRows(sum.ByOutcome, sum.Total)
	b.WriteString(`<div class="` + ui.AsksLogBreakdownsClass + `">`)
	counts("kind", "Kind of question", askCountRows(asklog.Kinds, sum.ByKind, askKindLabels), "")
	counts("outcome", "How it ended", outcomes, askOutcomeSheetHTML(sum.ByOutcome, folded))
	counts("answered_by", "Who answered", askCountRows([]string{asklog.ByOwner, asklog.ByChief, asklog.ByOther, asklog.AnsweredByNone}, sum.ByAnsweredBy, askByLabels), "")
	b.WriteString(`</div></section>`)

	b.WriteString(ui.AsksSectionLabel("asklog-top", "Asked most often") + `
<ul class="` + ui.AsksGroupListClass + `" aria-labelledby="asklog-top" data-testid="asklog-top">`)
	if len(sum.Top) == 0 {
		b.WriteString(ui.AsksEmptyHTML(` data-asks-empty="log-top" data-asklog-top-empty`, "", "", `No question asked twice in the last `+esc(askDuration(ttl))+` (question text is kept that long).`, ""))
	}
	for _, t := range sum.Top {
		// The question as the owner saw it (the newest of its rows), its last
		// paragraph — what was ASKED, and the part its normal form is made of
		// (asklog.Normalise), not the report before it. Grouped by the normal
		// form, which is never shown: lower-cased and stripped of its
		// punctuation it read as a typo.
		text := asklog.LastParagraph(t.Question)
		if text == "" {
			text = t.Norm
		}
		facts := make([]string, 0, 4)
		for _, f := range askTopFacts(t) {
			facts = append(facts, `<span class="`+ui.AsksLogFactClass+`">`+esc(f)+`</span>`)
		}
		b.WriteString(`<li class="` + ui.AsksCardClass + `"` + ui.AsksFixedCardAttrs + ` tabindex="-1" data-asklog-top-row data-count="` + strconv.Itoa(t.Count) +
			`" data-norm="` + esc(t.Norm) + `"><p class="` + ui.AsksLogClampClass + `" data-asklog-top-question>` +
			esc(text) + `</p><p class="` + ui.AsksLogFactsClass + `" data-asklog-top-facts>` + strings.Join(facts, " · ") + `</p></li>`)
	}
	b.WriteString(`</ul>`)
	return b.String()
}

// askTopFacts is a repeated question's facts line, short enough for a phone:
// how many times; who answered, only when it was not you every time; its
// usual answer; the median time to one.
func askTopFacts(t asklog.TopQuestion) []string {
	facts := []string{strconv.Itoa(t.Count) + " times"}
	if t.ByOwner < t.Count {
		facts = append(facts, "you answered "+strconv.Itoa(t.ByOwner))
	}
	if t.TopAnswer != "" {
		switch {
		case t.TopAnswerCount >= t.Count:
			facts = append(facts, `always “`+t.TopAnswer+`”`)
		case 2*t.TopAnswerCount > t.Count:
			facts = append(facts, `usually “`+t.TopAnswer+`”`)
		default:
			facts = append(facts, `most often “`+t.TopAnswer+`”`)
		}
	}
	if t.MedianAnswer > 0 {
		facts = append(facts, "median "+askDuration(t.MedianAnswer))
	}
	return facts
}

// askCountRow is one row of a breakdown: its key (data-count), label and count.
type askCountRow struct {
	key, label string
	n          int
}

// askCountRows is m's non-zero counts, in order, labelled.
func askCountRows(order []string, m map[string]int, labels map[string]string) []askCountRow {
	var rows []askCountRow
	for _, k := range order {
		if n := m[k]; n > 0 {
			rows = append(rows, askCountRow{k, labelOr(labels, k), n})
		}
	}
	return rows
}

// askOutcomeOther is the key of the outcomes' fold ("Other").
const askOutcomeOther = "other"

// askOutcomeShownPercent is the share of the asks an outcome other than Answered and
// No answer must hold to get a row of its own (one in ten).
const askOutcomeShownPercent = 10

// askOutcomeRows is "How it ended" at a glance, most first: Answered and No
// answer always have their own rows; any other outcome has one when it holds
// at least askOutcomeShownPercent of total, and the rest — the rare and
// jargony ones (Superseded, Moved on, …) — FOLD INTO ONE "Other" row, last,
// whose breakdown is in the ⓘ (askOutcomeSheetHTML). folded is the outcomes
// in it. A fold of one outcome is drawn by that outcome's own name: "Other 1"
// would hide a word to save none.
func askOutcomeRows(m map[string]int, total int) (rows []askCountRow, folded []string) {
	other := 0
	for _, k := range asklog.FilterOutcomes {
		n := m[k]
		if n <= 0 {
			continue
		}
		if k == asklog.OutcomeAnswered || k == asklog.OutcomeOpen || n*100 >= total*askOutcomeShownPercent {
			rows = append(rows, askCountRow{k, askOutcomeLabels[k], n})
			continue
		}
		folded = append(folded, k)
		other += n
	}
	switch len(folded) {
	case 0:
	case 1:
		rows = append(rows, askCountRow{folded[0], askOutcomeLabels[folded[0]], other})
		folded = nil
	}
	// Most first; ties keep FilterOutcomes' order.
	slices.SortStableFunc(rows, func(a, b askCountRow) int { return b.n - a.n })
	if len(folded) > 0 {
		rows = append(rows, askCountRow{askOutcomeOther, "Other", other})
	}
	return rows, folded
}

// askOutcomeSheetHTML is the "How it ended" ⓘ: what each outcome the log holds
// means, and which of them "Other" counts.
func askOutcomeSheetHTML(m map[string]int, folded []string) string {
	esc := html.EscapeString
	in := map[string]bool{}
	for _, k := range folded {
		in[k] = true
	}
	var b strings.Builder
	b.WriteString(`<dl class="flex flex-col gap-2" data-asklog-outcome-meanings>`)
	for _, k := range asklog.FilterOutcomes {
		n := m[k]
		if n <= 0 {
			continue
		}
		label := askOutcomeLabels[k]
		if in[k] {
			label += " (in Other)"
		}
		b.WriteString(`<div data-meaning="` + esc(k) + `"`)
		if in[k] {
			b.WriteString(` data-folded`)
		}
		b.WriteString(`><dt class="flex justify-between gap-3 text-sm font-medium text-fg"><span>` + esc(label) + `</span><span class="tabular-nums">` + strconv.Itoa(n) +
			`</span></dt><dd class="text-sm text-fg2">` + esc(askOutcomeMeanings[k]) + `</dd></div>`)
	}
	b.WriteString(`</dl>`)
	return ui.InfoSheetHTML(ui.InfoSheet{ID: "asklog-outcome-info", Label: "What these outcomes mean", Heading: "How asks ended",
		Lead: "Every ask in the log ends one of these ways.", Extra: g.Raw(b.String()), Inline: true})
}

// askLogWhere is where an ask was asked, as the card's sub line names it.
func askLogWhere(a asklog.Ask) string {
	var parts []string
	switch {
	case a.Host != "" && a.TmuxSession != "":
		parts = append(parts, a.TmuxSession+" @ "+a.Host)
	case a.Host != "":
		parts = append(parts, "@ "+a.Host)
	case a.AgentID > 0:
		parts = append(parts, "agent "+strconv.FormatInt(a.AgentID, 10))
	}
	if a.Repo != "" {
		parts = append(parts, a.Repo)
	}
	return strings.Join(parts, " · ")
}

// askLogCardHTML is one ask's card: what kind and where, its outcome as a
// pill; the question (or that its text expired); the answer, who gave it and
// how long it took.
func askLogCardHTML(a asklog.Ask, now time.Time, ttl time.Duration) string {
	esc := html.EscapeString
	row := askLogRowOf(a)
	id := strconv.FormatInt(a.ID, 10)
	var b strings.Builder
	b.WriteString(`<li id="asklog-` + id + `" class="` + ui.AsksCardClass + `"` + ui.AsksFixedCardAttrs + ` tabindex="-1" data-asklog-row data-ask-id="` + id + `" data-kind="` + esc(row.Kind) +
		`" data-outcome="` + esc(row.Outcome) + `" data-answered-by="` + esc(row.AnsweredBy) + `"`)
	if row.TextExpired {
		b.WriteString(` data-text-expired`)
	}
	sub := askLogWhere(a)
	if sub != "" {
		sub += " · "
	}
	b.WriteString(`>
<div class="flex min-w-0 items-start gap-2"><div class="` + ui.AsksHeadTextClass + `"><span class="` + ui.AsksTitleClass + `">` + esc(labelOr(askKindLabels, row.Kind)) +
		`</span><span class="` + ui.AsksSubClass + `">` + esc(sub) + `<time datetime="` + row.AskedAt.Format(time.RFC3339) + `">` + esc(attentionAge(now.Sub(a.AskedAt))) +
		`</time></span></div><span class="` + ui.AsksPillClass + `" data-asklog-outcome>` + esc(labelOr(askOutcomeLabels, row.Outcome)) + `</span></div>
`)
	switch {
	case row.TextExpired:
		b.WriteString(`<p class="` + ui.AsksLogExpiredClass + `" data-asklog-expired>Text expired: question and answer text is kept ` + esc(askDuration(ttl)) + `.</p>
`)
	case strings.TrimSpace(a.Question) != "":
		b.WriteString(`<p class="` + ui.AsksLogClampClass + `" data-asklog-question>` + esc(strings.TrimSpace(a.Question)) + `</p>
`)
	}
	var ans []string
	if strings.TrimSpace(a.Answer) != "" {
		ans = append(ans, "“"+truncate(strings.TrimSpace(firstLine(strings.TrimSpace(a.Answer))), 120)+"”")
	}
	if row.AnsweredBy != asklog.AnsweredByNone {
		ans = append(ans, "by "+labelOr(askByInline, row.AnsweredBy))
	}
	if row.AnswerSecs != nil {
		ans = append(ans, "after "+askDuration(a.Latency()))
	}
	if len(ans) > 0 {
		b.WriteString(`<p class="` + ui.AsksLogFactsClass + `" data-asklog-answer>` + esc(strings.Join(ans, " · ")) + `</p>
`)
	}
	b.WriteString(`</li>`)
	return b.String()
}
