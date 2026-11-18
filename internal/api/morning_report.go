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
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE MORNING REPORT ("while you were away"; docs/features.md#morning-report).
//
// Once a day, at the server's report time (MUSTER_MORNING_REPORT_AT in
// MUSTER_MORNING_REPORT_TZ, 08:00 America/Chicago by default), every enabled
// person gets one push saying what happened overnight — only on a day when
// something did (morning.Report.Quiet) — and the push opens the report's page:
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
// 🔴 ONE REPORT PER OWNER PER DAY, IN THE DATABASE (morning_reports, 0153):
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
//     "question" items), read live on the page; the job counts them.
//   - Done: pull requests the owner's sessions created (session_prs) that
//     GitHub says merged in the window, and tasks that are complete and were
//     last changed in the window (notes.updated_at).
//   - Stuck: the stall facts (stallInputs, the nudge sweep's own read) —
//     sessions idle past the stall threshold or stopped on an error — and
//     dispatched agents whose outcome turned "failed" in the window.
//   - Usage: the chief's turns in the window (chief_turn_usage) and each
//     Claude account's latest reading (cc_account_usage) when the usage poll
//     is on; otherwise each says plainly that it is not tracked.
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
	// reads; morningPRReadsDeadline all of those reads together.
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
		rep, gaps := s.buildMorningReport(octx, day)
		row = morning.Row{Date: day.Date, Since: day.Since, Until: day.Slot, Quiet: rep.Quiet(), Questions: len(rep.Questions),
			Body: morning.Body{Done: capLines(rep.Done), Stuck: capLines(rep.Stuck), Usage: rep.Usage, Gaps: gaps}}
		inserted, err := st.Insert(octx, row)
		if err != nil {
			return false, fmt.Errorf("record: %w", err)
		}
		if !inserted {
			// Another tick or replica recorded it between the read and the
			// insert: it pushes, not this one.
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
		Body:  morning.PushBody(row.Questions, len(row.Body.Done), len(row.Body.Stuck)),
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

// buildMorningReport is octx's owner's report of day, and what it could not
// read.
func (s *Server) buildMorningReport(octx context.Context, day morning.Day) (morning.Report, []morning.Gap) {
	rep := morning.Report{Since: day.Since, Until: day.Slot}
	for _, it := range s.morningQuestions(octx) {
		rep.Questions = append(rep.Questions, morning.Question{AskID: it.AskID, Text: it.Text, AskedAt: it.Since})
	}
	var gaps []morning.Gap
	merged, g := s.morningMerged(octx, day)
	gaps = append(gaps, g...)
	rep.Done = append(merged, s.morningTasksDone(octx, day)...)
	rep.Stuck = s.morningStuck(octx, day)
	rep.Usage = s.morningUsage(octx, day)
	return rep, gaps
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

// The report's gaps, in plain words (the setting is an admin's ⓘ).
var (
	gapPRsNotRecorded = morning.Gap{Text: "Merged pull requests aren't tracked on this server.", Setting: "MUSTER_SESSION_ROUTING=1 and MUSTER_PR_MAIL_SENDERS"}
	gapPRsNoGitHub    = morning.Gap{Text: "Whether pull requests merged can't be checked on this server.", Setting: "the GitHub App (MUSTER_GITHUB_APP_ID)"}
	gapPRsRead        = morning.Gap{Text: "Some pull requests could not be checked just now."}
	gapTasksRead      = morning.Gap{Text: "Finished tasks could not be read just now."}
)

// morningMerged is the pull requests octx's owner's sessions created that
// GitHub says merged in day's window, each read through the fleet's PR-state
// cache within morningPRsRead reads.
func (s *Server) morningMerged(octx context.Context, day morning.Day) ([]morning.Done, []morning.Gap) {
	lister, ok := s.ext.PRSessions.(recentPRLister)
	if !ok {
		return nil, []morning.Gap{gapPRsNotRecorded}
	}
	reader := s.fleetPRReader(octx)
	if reader == nil {
		return nil, []morning.Gap{gapPRsNoGitHub}
	}
	prs, err := lister.RecentPRs(octx, day.Since.Add(-morningPRsLookback), morningPRsRead)
	if err != nil {
		s.logger.Printf("morning report: recent pull requests: %v", err)
		return nil, []morning.Gap{gapPRsRead}
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
		out = append(out, morning.Done{Kind: morning.DoneMerged, Label: p.Repo + "#" + strconv.FormatInt(p.PR, 10),
			Href: "https://github.com/" + p.Repo + "/pull/" + strconv.FormatInt(p.PR, 10), At: st.ClosedAt})
	}
	var gaps []morning.Gap
	if unread > 0 {
		gaps = append(gaps, gapPRsRead)
	}
	return out, gaps
}

// morningTasksDone is octx's owner's own tasks (never a team's) that are
// complete and were last changed in day's window. ⚠ muster keeps no time a
// task was completed: updated_at is when it last changed, so a complete task
// edited overnight is listed again, and one completed overnight and edited
// after the report time is not.
func (s *Server) morningTasksDone(octx context.Context, day morning.Day) []morning.Done {
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
		title := strings.TrimSpace(n.Title)
		if title == "" {
			title = strings.TrimSpace(n.Directory)
		}
		out = append(out, morning.Done{Kind: morning.DoneTask, Label: cutRunes(title, 200),
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
			if a.Outcome != agentoutcome.Failed || a.OutcomeAt == nil || a.OutcomeAt.Before(day.Since) {
				continue
			}
			failed[a.ID] = true
			why := "failed"
			if r := strings.TrimSpace(a.OutcomeReason); r != "" {
				why = "failed: " + cutRunes(r, 160)
			}
			out = append(out, morning.Stuck{Kind: morning.StuckFailed, Name: firstNonEmpty(a.DisplayName, a.Name), Why: why,
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

// stuckName is a stalled session's name as the report says it: its stable
// name, else its window's label, else the agent's name.
func stuckName(c nudgeCandidate) string {
	return firstNonEmpty(c.Name, c.Label, c.agent.DisplayName, c.agent.Name, "A session")
}

// firstNonEmpty is the first of vs that is not blank, trimmed ("" for none).
func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
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

// The usage section's plain "not tracked" sentences.
const (
	morningChiefUntracked = "Your chief's working time isn't tracked on this server."
	morningPlanUntracked  = "Usage isn't tracked on this server."
	morningPlanSetting    = "MUSTER_CC_USAGE_POLL=1"
)

// morningUsage is day's usage for octx's owner: the chief's turns in the
// window, and each Claude account's latest reading when the poll is on.
func (s *Server) morningUsage(octx context.Context, day morning.Day) morning.Usage {
	var u morning.Usage
	if w, ok := s.ext.ChiefUsage.(chiefUsageWindower); ok {
		if got, err := w.Window(octx, day.Since, day.Slot); err != nil {
			s.logger.Printf("morning report: chief usage: %v", err)
			u.Chief.Off = "Your chief's working time could not be read just now."
		} else {
			u.Chief = morning.ChiefUsage{Tracked: true, Turns: got.Turns, Busy: got.Busy, Tokens: got.Tokens.Total()}
		}
	} else {
		u.Chief.Off = morningChiefUntracked
	}
	// 🔴 OFF IS SAID, NEVER A ZERO: with the poll off no reading is taken, so
	// an account's numbers would be whatever was last read, or none.
	if s.ext.CCUsage == nil || s.ext.CCUsageInterval == 0 {
		u.Plan = morning.PlanUsage{Off: morningPlanUntracked, Setting: morningPlanSetting}
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
		pa := morning.PlanAccount{Name: a.Account}
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
// open questions as the queue's own cards (their forms return here).
func (s *Server) morningReportView(ctx context.Context, row morning.Row, loc *time.Location, v ui.MorningReportView) ui.MorningReportView {
	since, until := row.Since.In(loc), row.Until.In(loc)
	v.Heading = until.Format("Monday, January 2")
	v.Window = "From " + clockText(since) + " " + since.Format("Monday") + " to " + clockText(until) + " " + until.Format("Monday") + "."
	v.Quiet = row.Quiet
	items := s.morningQuestions(ctx)
	s.withFocus(ctx, items)
	s.withAskBoxes(ctx, items, true)
	v.QuestionCount = len(items)
	if len(items) > 0 {
		v.QuestionsHTML = attentionItemsHTMLTo(items, s.now(), morningDatePath(row.Date))
	}
	for _, d := range row.Body.Done {
		line := ui.MorningLine{Text: d.Label, Href: d.Href, External: d.Kind == morning.DoneMerged}
		switch d.Kind {
		case morning.DoneMerged:
			line.Detail = "Pull request merged at " + clockText(d.At.In(loc))
		case morning.DoneTask:
			line.Detail = "Task finished"
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
	if u.Chief.Tracked {
		v.Usage = append(v.Usage, ui.MorningUsageLine{Text: chiefUsageText(u.Chief)})
	} else if u.Chief.Off != "" {
		v.Usage = append(v.Usage, ui.MorningUsageLine{Text: u.Chief.Off, Off: true, Setting: u.Chief.Setting})
	}
	if u.Plan.Tracked {
		if len(u.Plan.Accounts) == 0 {
			v.Usage = append(v.Usage, ui.MorningUsageLine{Text: "No Claude accounts are set up."})
		}
		for _, a := range u.Plan.Accounts {
			v.Usage = append(v.Usage, ui.MorningUsageLine{Text: planUsageText(a, loc)})
		}
	} else if u.Plan.Off != "" {
		v.Usage = append(v.Usage, ui.MorningUsageLine{Text: u.Plan.Off, Off: true, Setting: u.Plan.Setting})
	}
	for _, g := range row.Body.Gaps {
		v.Gaps = append(v.Gaps, ui.MorningUsageLine{Text: g.Text, Off: true, Setting: g.Setting})
	}
	return v
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
	return "Your chief worked " + durationText(c.Busy) + " over " + turns + "."
}

// durationText is d in hours and minutes ("1h 12m", "4m", "under a minute").
func durationText(d time.Duration) string {
	if d < time.Minute {
		return "under a minute"
	}
	h, m := int(d.Hours()), int(d.Minutes())%60
	switch {
	case h == 0:
		return strconv.Itoa(m) + "m"
	case m == 0:
		return strconv.Itoa(h) + "h"
	}
	return strconv.Itoa(h) + "h " + strconv.Itoa(m) + "m"
}

// planUsageText is one account's line: its latest reading and when it was
// taken — never an overnight figure, which muster does not keep.
func planUsageText(a morning.PlanAccount, loc *time.Location) string {
	pct := func(p *float64) string {
		if p == nil {
			return "not read"
		}
		return strconv.Itoa(int(*p+0.5)) + "% used"
	}
	t := a.Name + ": this 5-hour period " + pct(a.FiveHour) + ", this week " + pct(a.Week)
	if a.ReadAt.IsZero() {
		return t + " (not read yet)."
	}
	return t + " (as of " + clockText(a.ReadAt.In(loc)) + ")."
}
