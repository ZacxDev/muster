package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agentoutcome"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/chiefusage"
	"github.com/ZacxDev/muster/internal/morning"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/prroute"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/redact"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE MORNING REPORT ("while you were away"; docs/features.md#morning-report).
//
// Once a day, at 08:00 (morning.ReportHour) in MUSTER_MORNING_REPORT_TZ
// (America/Chicago by default), every enabled person gets one push saying what
// happened overnight — only on a day when something did (morning.Report.Quiet)
// — and the push opens the report's page:
//
//	GET /morning          the newest report
//	GET /morning/{date}   the report of a local date (2006-01-02)
//
// 🔴 READ-ONLY. The job and the page read muster's own rows and never act:
// no nudge, no message, no ask, no task write. The page's open questions are
// the Asks queue's own cards (attentionItemsHTMLTo, the SAME function /attention
// draws them with), so the owner answers one in place through the queue's own
// answer route, and the answer lands as it would on /attention.
//
// 🔴 ONE REPORT PER OWNER PER DAY, IN THE DATABASE (morning_reports, 0155):
// the day's row is inserted before anything is pushed, and the push is
// claimed by one conditional update before it is sent, so a restart, a second
// tick or a rolling update's second replica sends nothing twice.
//
// 🔴 EVERY READ IS THE OWNER'S OWN: each owner's report is built under
// attentionPrincipal (row-level security), and the questions are narrowed to
// the owner's own asks (a member's queue can hold a team's, read-only).
//
// WHERE EACH SECTION COMES FROM (no model, no transcript):
//   - Questions: the Asks queue's open questions (attentionItems: "ask" and
//     "question" items), read live on the page; the job counts them, and how
//     many were asked in the window.
//   - Done: pull requests the owner's sessions created (session_prs) that
//     GitHub says merged in the window, by their titles; and tasks that are
//     complete and last CHANGED in the window (notes.updated_at — muster keeps
//     no completion time, so the page says "changed overnight").
//   - Stuck: the stall facts (stallInputs, the nudge sweep's own read) —
//     sessions idle past the stall threshold or stopped on an error — and
//     dispatched agents whose outcome turned "failed" in the window.
//   - Usage and cost: the chief's turns in the window (chief_turn_usage), each
//     Claude account's latest reading (cc_account_usage) when the usage poll
//     is on, and the chief's OpenRouter spend (chief_session_llm_usage, whole
//     UTC days) when those sessions are on; otherwise each says plainly that
//     it is not tracked.
// ---------------------------------------------------------------------------

// MorningReport is the morning report's wiring (MUSTER_MORNING_REPORT=1);
// nil in Extensions: off — no job, and the page answers 404.
type MorningReport struct {
	Store    morning.Store
	Schedule morning.Schedule
}

// MorningReportInterval is how often the job looks for a due report.
const MorningReportInterval = time.Minute

// The report's page.
const (
	morningPath       = "/morning"
	morningPathPrefix = morningPath + "/"
)

// morningDatePath is the page of the report of date.
func morningDatePath(date string) string { return morningPathPrefix + date }

// Bounds on what one report reads and keeps.
const (
	// morningPRsRead bounds the pull requests whose GitHub state one report
	// reads; morningPRReadsDeadline all of those reads together. A report that
	// meets the bound says so and is never quiet (morning.Report.Unchecked).
	morningPRsRead         = 30
	morningPRReadsDeadline = 20 * time.Second
	// morningPRsLookback is how far back a pull request's last report may be
	// for it to be checked: one opened a week ago can merge overnight.
	morningPRsLookback = 7 * 24 * time.Hour
	// morningLinesMax bounds each stored section.
	morningLinesMax = 50
)

func (s *Server) registerMorningReportRoutes(mux Mux) {
	mux.HandleFunc("GET "+morningPath, s.requireSession(s.handleMorningReport))
	mux.HandleFunc("GET "+morningPathPrefix+"{date}", s.requireSession(s.handleMorningReport))
}

// RunMorningReportSweep runs MorningReportPass every interval (<= 0:
// MorningReportInterval) until ctx ends. It does nothing with the feature off.
func (s *Server) RunMorningReportSweep(ctx context.Context, interval time.Duration) {
	if s.ext.MorningReport == nil {
		return
	}
	if interval <= 0 {
		interval = MorningReportInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := s.MorningReportPass(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Printf("morning report: %v", err)
		}
	}
}

// MorningReportPass is one look for a due report: when today's report time
// has passed (within morning.CatchUp), every owner without today's report gets
// one, and a push when it is not a quiet day. It returns how many pushes it
// sent.
func (s *Server) MorningReportPass(ctx context.Context) (int, error) {
	mr := s.ext.MorningReport
	if mr == nil {
		return 0, nil
	}
	day, ok := mr.Schedule.Due(s.now())
	if !ok {
		return 0, nil
	}
	owners, err := mr.Store.Owners(ctx)
	if err != nil {
		return 0, fmt.Errorf("owners: %w", err)
	}
	sent := 0
	for _, o := range owners {
		if ctx.Err() != nil {
			return sent, ctx.Err()
		}
		pushed, err := s.morningReportFor(s.attentionPrincipal(ctx, o), o, day)
		if err != nil {
			s.logger.Printf("morning report: owner %d, %s: %v", o, day.Date, err)
		}
		if pushed {
			sent++
		}
	}
	return sent, nil
}

// morningReportFor is owner's report of day, under octx (the owner's
// principal): built and stored once, and pushed once when it is not quiet.
// True when this call sent the push.
func (s *Server) morningReportFor(octx context.Context, owner int64, day morning.Day) (bool, error) {
	st := s.ext.MorningReport.Store
	row, have, err := st.Get(octx, day.Date)
	if err != nil {
		return false, fmt.Errorf("read: %w", err)
	}
	if !have {
		rep := s.buildMorningReport(octx, day)
		rep.Done, rep.Stuck = capLines(rep.Done), capLines(rep.Stuck)
		row = morning.Row{Date: day.Date, Since: day.Since, Until: day.Slot, Quiet: rep.Quiet(), Questions: rep.Questions, Body: rep.Body}
		inserted, err := st.Insert(octx, row)
		if err != nil {
			return false, fmt.Errorf("record: %w", err)
		}
		if !inserted {
			// An OPTIMISATION, not the guard: another tick or replica recorded
			// the day between the read and the insert, and it pushes. Were this
			// return missing, ClaimPush below would still refuse the second push.
			return false, nil
		}
	}
	// 🔴 ONE RULE, ONE PLACE: whether this day pushes is ClaimPush's, in SQL —
	// never a quiet day, and never a day already pushed — so a restart, a
	// second tick and a second replica all ask the same row the same question.
	claimed, err := st.ClaimPush(octx, day.Date, s.now())
	if err != nil || !claimed {
		return false, err
	}
	s.goNotify(owner, push.EventMorningReport, RouterNotification{
		Type:  "morning_report",
		Title: morning.PushTitle,
		Body:  morning.PushBody(row.Questions, row.Body.Merged(), len(row.Body.Stuck)),
		Tag:   morning.PushTag,
		Data:  map[string]string{"url": morningDatePath(day.Date)},
	}, nil)
	return true, nil
}

// capLines is ls cut to morningLinesMax.
func capLines[T any](ls []T) []T {
	if len(ls) > morningLinesMax {
		return ls[:morningLinesMax]
	}
	return ls
}

// buildMorningReport is octx's owner's report of day.
func (s *Server) buildMorningReport(octx context.Context, day morning.Day) morning.Report {
	rep := morning.Report{Since: day.Since, Until: day.Slot}
	for _, it := range s.morningQuestions(octx) {
		rep.Questions++
		// The night is [Since, Slot): a question asked after the report time
		// is tomorrow's news, never today's too.
		if !it.Since.Before(day.Since) && it.Since.Before(day.Slot) {
			rep.NewQuestions++
		}
	}
	merged, gaps, unchecked := s.morningMerged(octx, day)
	rep.Unchecked = unchecked
	rep.Gaps = gaps
	rep.Done = append(merged, s.morningTasksChanged(octx, day)...)
	rep.Stuck = s.morningStuck(octx, day)
	rep.Usage = s.morningUsage(octx, day)
	return rep
}

// morningQuestions is octx's owner's open questions: the Asks queue's "ask"
// and "question" items (never a notice, a waiting window, a draft or a task to
// route), the owner's OWN only — a member's queue can list a team's asks to
// watch, which are not theirs to answer — newest first, as the queue orders
// them.
func (s *Server) morningQuestions(octx context.Context) []attentionItem {
	items, err := s.attentionItems(octx)
	if err != nil {
		s.logger.Printf("morning report: the questions: %v", err)
	}
	owner := requestOwner(octx)
	var out []attentionItem
	for _, it := range items {
		switch {
		case it.Kind == attentionKindAsk && it.owner == owner:
		case it.Kind == attentionKindQuestion:
			// A held question is read from the owner's own hosts only.
		default:
			continue
		}
		out = append(out, it)
	}
	sortAttentionItems(out)
	return out
}

// recentPRLister is the pull-request store's recent-creations read
// (plugins.PGStore.RecentPRs); a store without it lists none.
type recentPRLister interface {
	RecentPRs(ctx context.Context, since time.Time, limit int) ([]plugins.PRSession, error)
}

// The report's gaps, in plain words. The setting behind each is named only on
// the page, for an admin (morningSettingFor), never stored.
var (
	gapPRsNotRecorded = morning.Gap{Text: "Merged pull requests aren't tracked on this server."}
	gapPRsNoGitHub    = morning.Gap{Text: "Whether pull requests merged can't be checked on this server."}
	gapPRsRead        = morning.Gap{Text: "Some pull requests could not be checked just now."}
	gapPRsCap         = morning.Gap{Text: "Only your " + strconv.Itoa(morningPRsRead) + " most recent pull requests were checked."}
)

// The usage part's plain "not tracked" sentences.
const (
	morningChiefUntracked = "Your chief's working time isn't tracked on this server."
	morningPlanUntracked  = "Usage isn't tracked on this server."
	morningCostUntracked  = "Cost isn't tracked for Claude plans."
)

// morningSettingFor is the setting behind an "isn't tracked" sentence, for an
// admin's ⓘ ("" when it is not a setting).
var morningSettingFor = map[string]string{
	gapPRsNotRecorded.Text: "MUSTER_SESSION_ROUTING=1 and MUSTER_PR_MAIL_SENDERS",
	gapPRsNoGitHub.Text:    "the GitHub App (MUSTER_GITHUB_APP_ID)",
	morningPlanUntracked:   "MUSTER_CC_USAGE_POLL=1",
}

// morningMerged is the pull requests octx's owner's sessions created that
// GitHub says merged in day's window, each read through the fleet's PR-state
// cache within morningPRsRead reads; the gaps it met; and whether any pull
// request went unchecked (a state not read, or the read bound met), which
// keeps the day from being quiet.
func (s *Server) morningMerged(octx context.Context, day morning.Day) ([]morning.Done, []morning.Gap, bool) {
	lister, ok := s.ext.PRSessions.(recentPRLister)
	if !ok {
		return nil, []morning.Gap{gapPRsNotRecorded}, false
	}
	reader := s.fleetPRReader(octx)
	if reader == nil {
		return nil, []morning.Gap{gapPRsNoGitHub}, false
	}
	// One more than the bound is read, so "more than the bound" is known,
	// never guessed from exactly the bound.
	prs, err := lister.RecentPRs(octx, day.Since.Add(-morningPRsLookback), morningPRsRead+1)
	if err != nil {
		s.logger.Printf("morning report: recent pull requests: %v", err)
		return nil, []morning.Gap{gapPRsRead}, true
	}
	capped := len(prs) > morningPRsRead
	if capped {
		prs = prs[:morningPRsRead]
	}
	rctx, cancel := context.WithTimeout(withPRReadBudget(octx, morningPRsRead), morningPRReadsDeadline)
	defer cancel()
	var out []morning.Done
	unread := 0
	for _, p := range prs {
		st, known, _ := s.fleetPRs.read(rctx, reader, prroute.Ref{Repo: p.Repo, PR: p.PR}, s.now(), fleetPRReadTimeout, fleetPRTTL, fleetPRTTL)
		if !known {
			unread++
			continue
		}
		if !st.Merged || st.ClosedAt.Before(day.Since) || !st.ClosedAt.Before(day.Slot) {
			continue
		}
		ref := p.Repo + "#" + strconv.FormatInt(p.PR, 10)
		title := cutRunes(strings.Join(strings.Fields(redact.Secrets(st.Title)), " "), 200)
		if title == "" {
			title = "A pull request"
		}
		out = append(out, morning.Done{Kind: morning.DoneMerged, Label: title, Ref: ref,
			Href: "https://github.com/" + p.Repo + "/pull/" + strconv.FormatInt(p.PR, 10), At: st.ClosedAt})
	}
	var gaps []morning.Gap
	if unread > 0 {
		gaps = append(gaps, gapPRsRead)
	}
	if capped {
		gaps = append(gaps, gapPRsCap)
	}
	return out, gaps, unread > 0 || capped
}

// morningTasksChanged is octx's owner's own tasks (never a team's) that are
// complete and were last CHANGED in day's window. ⚠ muster keeps no time a
// task was completed: updated_at moves on any change (a tag, a synced
// snapshot), so these lines say "changed overnight" and never make a day loud
// (morning.Report.Quiet).
func (s *Server) morningTasksChanged(octx context.Context, day morning.Day) []morning.Done {
	if s.ext.Notes == nil {
		return nil
	}
	list, err := s.ext.Notes.ListSummaries(octx)
	if err != nil {
		s.logger.Printf("morning report: tasks: %v", err)
		return nil
	}
	owner := requestOwner(octx)
	var out []morning.Done
	for _, n := range list {
		if n.Status != notes.StatusComplete || n.OwnerID != owner || n.UpdatedAt.Before(day.Since) || !n.UpdatedAt.Before(day.Slot) {
			continue
		}
		title := firstNonBlank(strings.TrimSpace(n.Title), strings.TrimSpace(n.Directory))
		out = append(out, morning.Done{Kind: morning.DoneTaskChanged, Label: cutRunes(title, 200),
			Href: "/tasks/" + strconv.FormatInt(n.ID, 10), At: n.UpdatedAt})
	}
	return out
}

// morningStuck is what is stuck for octx's owner: from the stall facts (the
// nudge sweep's own read, so the two agree on who is stalled), every session
// that stopped on an error, and every one idle past the stall threshold with
// nothing running and no question of its own open (that one is already a
// question); then every dispatched agent whose outcome turned "failed" in
// day's window. The chief and officers are never listed: officers escalate
// through their own channel, and the chief is the one reporting.
func (s *Server) morningStuck(octx context.Context, day morning.Day) []morning.Stuck {
	var out []morning.Stuck
	failed := map[int64]bool{}
	owner := requestOwner(octx)
	if s.ext.Agents != nil {
		list, err := s.ext.Agents.List(octx)
		if err != nil {
			s.logger.Printf("morning report: agents: %v", err)
		}
		for _, a := range ownedBy(agents.ProvisionedOnly(list), owner) {
			if a.Outcome != agentoutcome.Failed || a.OutcomeAt == nil || a.OutcomeAt.Before(day.Since) || !a.OutcomeAt.Before(day.Slot) {
				continue
			}
			failed[a.ID] = true
			why := "failed"
			if r := strings.TrimSpace(a.OutcomeReason); r != "" {
				why = "failed: " + cutRunes(r, 160)
			}
			out = append(out, morning.Stuck{Kind: morning.StuckFailed, Name: firstNonBlank(strings.TrimSpace(a.DisplayName), a.Name), Why: why,
				Href: ui.AgentSessionHref(a.Name, 0), Since: *a.OutcomeAt})
		}
	}
	idle := s.chiefNudgeIdle()
	seenAgent := map[int64]bool{}
	for _, c := range s.stallInputs(octx, nil) {
		if c.Chief || c.Officer || c.OtherUser {
			continue
		}
		if c.Kind == chiefnudge.KindAgent {
			if failed[c.AgentID] || seenAgent[c.AgentID] {
				continue
			}
		}
		var k, why string
		switch {
		case c.TurnError:
			k, why = morning.StuckError, "stopped on an error"
		case c.Status == "idle" && !c.Shell && !c.OpenAsk && c.IdleFor >= idle:
			k, why = morning.StuckIdle, "idle, nothing running"
		default:
			continue
		}
		if c.Kind == chiefnudge.KindAgent {
			seenAgent[c.AgentID] = true
		}
		out = append(out, morning.Stuck{Kind: k, Name: stuckName(c), Why: why, Href: stuckHref(c), Since: c.IdleSince})
	}
	return out
}

// stuckName is a stalled session's name as the report says it: its HUMAN
// name, else (an agent) the agent's display name or slug, else "A session".
// 🔴 NEVER A WINDOW'S LABEL: a mirrored window's label is its tmux place
// ("Gold:2"), which is an address, not a name, and the page's top layer names
// no address (the ⓘ rule).
func stuckName(c nudgeCandidate) string {
	if n := strings.TrimSpace(c.Name); n != "" {
		return n
	}
	if c.Kind == chiefnudge.KindAgent {
		if n := firstNonBlank(strings.TrimSpace(c.agent.DisplayName), c.agent.Name); n != "" {
			return n
		}
	}
	return "A session"
}

// stuckHref is where a stalled session opens.
func stuckHref(c nudgeCandidate) string {
	if c.Kind == chiefnudge.KindAgent {
		return ui.AgentSessionHref(c.agent.Name, c.ChatSessionID)
	}
	return ui.MirrorRef{Host: c.Host, Session: c.TmuxSession, Window: c.Window}.ViewPath()
}

// chiefUsageWindower is the chief usage store's window read
// (chiefusage.PGStore.Window); a store without it reads none.
type chiefUsageWindower interface {
	Window(ctx context.Context, since, until time.Time) (chiefusage.Window, error)
}

// daySpender is the chief OpenRouter sessions' store's per-day spend
// (chiefusage.PGStore.DaySpent).
type daySpender interface {
	DaySpent(ctx context.Context, day time.Time) (float64, error)
}

// morningUsage is day's usage and cost for octx's owner: the chief's turns in
// the window, each Claude account's latest reading when the poll is on, and the
// chief's OpenRouter spend over the UTC days the window touches when those
// sessions are on.
func (s *Server) morningUsage(octx context.Context, day morning.Day) morning.Usage {
	var u morning.Usage
	if w, ok := s.ext.ChiefUsage.(chiefUsageWindower); ok {
		if got, err := w.Window(octx, day.Since, day.Slot); err != nil {
			s.logger.Printf("morning report: chief usage: %v", err)
			u.Chief.Off = "Your chief's working time could not be read just now."
		} else {
			u.Chief = morning.ChiefUsage{Tracked: true, Turns: got.Turns, Busy: got.Busy}
		}
	} else {
		u.Chief.Off = morningChiefUntracked
	}
	u.Cost = s.morningCost(octx, day)
	// 🔴 OFF IS SAID, NEVER A ZERO: with the poll off no reading is taken, so
	// an account's numbers would be whatever was last read, or none.
	if s.ext.CCUsage == nil || s.ext.CCUsageInterval == 0 {
		u.Plan = morning.PlanUsage{Off: morningPlanUntracked}
		return u
	}
	accts, err := s.ccUsageAccounts(octx, requestOwner(octx), s.now())
	if err != nil {
		s.logger.Printf("morning report: plan usage: %v", err)
		u.Plan = morning.PlanUsage{Off: "Usage could not be read just now."}
		return u
	}
	u.Plan.Tracked = true
	for _, a := range accts {
		var pa morning.PlanAccount
		if a.FiveHour != nil {
			pa.FiveHour = a.FiveHour.Utilization
		}
		if a.SevenDay != nil {
			pa.Week = a.SevenDay.Utilization
		}
		if a.ReadAt != nil {
			pa.ReadAt, _ = time.Parse(time.RFC3339, *a.ReadAt)
		}
		u.Plan.Accounts = append(u.Plan.Accounts, pa)
	}
	return u
}

// morningCost is the chief's OpenRouter spend on the last whole UTC day
// before day's report time (morning.CostUsage); "not tracked for Claude
// plans" when those sessions are off.
func (s *Server) morningCost(octx context.Context, day morning.Day) morning.CostUsage {
	var sp daySpender
	if s.chiefSessions != nil {
		sp, _ = s.chiefSessions.store.(daySpender)
	}
	if sp == nil {
		return morning.CostUsage{Off: morningCostUntracked}
	}
	slot := day.Slot.UTC()
	whole := time.Date(slot.Year(), slot.Month(), slot.Day()-1, 0, 0, 0, 0, time.UTC)
	usd, err := sp.DaySpent(octx, whole)
	if err != nil {
		s.logger.Printf("morning report: chief spend: %v", err)
		return morning.CostUsage{Off: "Your chief's cost could not be read just now."}
	}
	return morning.CostUsage{Tracked: true, USD: usd, Day: whole.Format("2006-01-02")}
}

// --- the page ------------------------------------------------------------------

// handleMorningReport serves GET /morning and GET /morning/{date}: the
// principal's report of that day (the newest without a date), its open
// questions read now and answerable in place.
func (s *Server) handleMorningReport(w http.ResponseWriter, r *http.Request) {
	mr := s.ext.MorningReport
	if mr == nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	date := r.PathValue("date")
	var row morning.Row
	var have bool
	var err error
	if date == "" {
		row, have, err = mr.Store.Latest(ctx)
	} else {
		if _, ok := mr.Schedule.ParseDate(date); !ok {
			http.NotFound(w, r)
			return
		}
		row, have, err = mr.Store.Get(ctx, date)
	}
	if err != nil {
		s.logger.Printf("morning report: page: %v", err)
		http.Error(w, "the report could not be read", http.StatusInternalServerError)
		return
	}
	feat := s.shellFeatures(ctx)
	v := ui.MorningReportView{Setup: feat.Setup}
	v.Notice, _ = composerNotice(r)
	if !have {
		v.Missing = true
	} else {
		v = s.morningReportView(ctx, row, mr.Schedule.Loc, v)
	}
	var b strings.Builder
	if err := ui.RenderMorningReport(&b, v); err != nil {
		s.logger.Printf("morning report: render: %v", err)
	}
	s.writeNavDocument(w, r, http.StatusOK, "Morning report", "", false, b.String())
}

// morningReportView is row as its page draws it, in loc, with the owner's
// open questions as the queue's own cards (their forms return here), each
// with how long it has left.
func (s *Server) morningReportView(ctx context.Context, row morning.Row, loc *time.Location, v ui.MorningReportView) ui.MorningReportView {
	since, until := row.Since.In(loc), row.Until.In(loc)
	now := s.now()
	v.Heading = until.Format("Monday, January 2")
	v.Window = "From " + clockText(since) + " " + since.Format("Monday") + " to " + clockText(until) + " " + until.Format("Monday") + "."
	v.Quiet = row.Quiet
	items := s.morningQuestions(ctx)
	s.withFocus(ctx, items)
	s.withAskBoxes(ctx, items, true)
	expires := s.askExpiries(ctx)
	for _, it := range items {
		q := ui.MorningQuestion{CardHTML: attentionItemHTML(it, now, morningDatePath(row.Date))}
		if at, ok := expires[it.AskID]; ok && it.AskID > 0 {
			q.Left = "Answer within " + askDuration(at.Sub(now).Truncate(time.Minute))
			if at.Sub(now) < time.Minute {
				q.Left = "Expires in under a minute"
			}
		}
		v.Questions = append(v.Questions, q)
	}
	for _, d := range row.Body.Done {
		line := ui.MorningLine{Text: d.Label, Href: d.Href, External: d.Kind == morning.DoneMerged}
		switch d.Kind {
		case morning.DoneMerged:
			line.Detail = "Pull request merged at " + clockText(d.At.In(loc))
		case morning.DoneTaskChanged:
			line.Detail = "Complete task, changed overnight"
		}
		v.Done = append(v.Done, line)
	}
	for _, st := range row.Body.Stuck {
		line := ui.MorningLine{Text: st.Name, Href: st.Href, Detail: st.Why}
		if !st.Since.IsZero() {
			line.Detail += " · since " + clockText(st.Since.In(loc)) + " " + st.Since.In(loc).Format("Monday")
		}
		v.Stuck = append(v.Stuck, line)
	}
	u := row.Body.Usage
	off := func(text string) ui.MorningUsageLine {
		return ui.MorningUsageLine{Text: text, Off: true, Setting: morningSettingFor[text]}
	}
	switch {
	case u.Chief.Tracked:
		v.Usage = append(v.Usage, ui.MorningUsageLine{Text: chiefUsageText(u.Chief)})
	case u.Chief.Off != "":
		v.Usage = append(v.Usage, off(u.Chief.Off))
	}
	switch {
	case u.Plan.Tracked && len(u.Plan.Accounts) == 0:
		v.Usage = append(v.Usage, ui.MorningUsageLine{Text: "No Claude plans are set up."})
	case u.Plan.Tracked:
		for i, a := range u.Plan.Accounts {
			v.Usage = append(v.Usage, ui.MorningUsageLine{Text: planUsageText(i+1, a, loc)})
		}
	case u.Plan.Off != "":
		v.Usage = append(v.Usage, off(u.Plan.Off))
	}
	switch {
	case u.Cost.Tracked:
		v.Usage = append(v.Usage, ui.MorningUsageLine{Text: costText(u.Cost)})
	case u.Cost.Off != "":
		v.Usage = append(v.Usage, off(u.Cost.Off))
	}
	for _, g := range row.Body.Gaps {
		v.Gaps = append(v.Gaps, off(g.Text))
	}
	return v
}

// askExpiries is ctx's principal's open asks' expiry times, by ask id (empty
// on a read error, logged: the page then shows no time left).
func (s *Server) askExpiries(ctx context.Context) map[int64]time.Time {
	out := map[int64]time.Time{}
	if s.ext.Attention == nil {
		return out
	}
	open, err := s.ext.Attention.Open(ctx)
	if err != nil {
		s.logger.Printf("morning report: ask expiries: %v", err)
		return out
	}
	for _, a := range open {
		out[a.ID] = a.ExpiresAt
	}
	return out
}

// clockText is t's wall-clock time as the report says it ("8:00 AM").
func clockText(t time.Time) string { return t.Format("3:04 PM") }

// chiefUsageText is the chief's line: "Your chief worked 1h 12m over 34
// turns." — or that it did not.
func chiefUsageText(c morning.ChiefUsage) string {
	if c.Turns == 0 {
		return "Your chief did no work overnight."
	}
	turns := "1 turn"
	if c.Turns != 1 {
		turns = strconv.FormatInt(c.Turns, 10) + " turns"
	}
	return "Your chief worked " + askDuration(c.Busy) + " over " + turns + "."
}

// planUsageText is the n-th Claude plan's line: its latest reading and when it
// was taken — never an overnight figure, which muster does not keep, and never
// the account's configured name.
func planUsageText(n int, a morning.PlanAccount, loc *time.Location) string {
	pct := func(p *float64) string {
		if p == nil {
			return "not read"
		}
		return strconv.Itoa(int(*p+0.5)) + "% used"
	}
	t := "Claude plan " + strconv.Itoa(n) + ": this 5-hour period " + pct(a.FiveHour) + ", this week " + pct(a.Week)
	if a.ReadAt.IsZero() {
		return t + " (not read yet)."
	}
	return t + " (as of " + clockText(a.ReadAt.In(loc)) + ")."
}

// costText is the chief's OpenRouter cost line: the last whole UTC day's
// spend, and which day that is.
func costText(c morning.CostUsage) string {
	return fmt.Sprintf("Your chief's OpenRouter cost on %s (UTC, the last whole day): $%.2f.", c.Day, c.USD)
}
