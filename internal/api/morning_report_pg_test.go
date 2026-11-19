package api

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ccusage"
	"github.com/ZacxDev/muster/internal/chiefusage"
	"github.com/ZacxDev/muster/internal/githubapp"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/morning"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE MORNING REPORT (morning_report.go) on a real Postgres: one report and
// one push per owner per day across a restart, nothing on a quiet day, every
// section filled from its own source with in-window and out-of-window rows
// pairwise distinct, an owner's report holding only its own items, and the
// usage section saying plainly when the poll is off. Every name, title, repo
// and number is invented.

// morningPush records each push with the owner it was sent to.
type morningPush struct {
	capturePush
	mu     sync.Mutex
	owners []int64
	prefs  memPush // nothing muted
}

func (o *morningPush) Store() push.Store { return &o.prefs }

func (o *morningPush) SendTo(ctx context.Context, owner int64, p push.Payload) push.Report {
	o.mu.Lock()
	o.owners = append(o.owners, owner)
	o.mu.Unlock()
	return o.capturePush.SendTo(ctx, owner, p)
}

// to is the pushes owner got.
func (o *morningPush) to(owner int64) []push.Payload {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.capturePush.mu.Lock()
	defer o.capturePush.mu.Unlock()
	var out []push.Payload
	for i, w := range o.owners {
		if w == owner {
			out = append(out, o.capturePush.got[i])
		}
	}
	return out
}

// morningObservedMirror is the fleet fixture's mirror with each idle window's status
// observed at a fixed instant, as a host that reports status stamps it (the
// stall facts read an idle streak from it).
type morningObservedMirror struct {
	*digestMirror
	at time.Time
}

func (m morningObservedMirror) AgentState(ctx context.Context, host, session, window string) mirror.AgentState {
	st := m.digestMirror.AgentState(ctx, host, session, window)
	if st.Status == "idle" {
		st.Observed = m.at
	}
	return st
}

type morningFixture struct {
	*fleetFixture
	push *morningPush
	day  morning.Day
	loc  *time.Location
}

// newMorningFixture is the fleet fixture with the morning report on (08:00
// in America/Chicago) and the clock five minutes past TODAY's report time —
// today's, because session_prs' retention is the database's own clock.
func newMorningFixture(t *testing.T) *morningFixture {
	t.Helper()
	f := &morningFixture{fleetFixture: newFleetFixture(t), push: &morningPush{}}
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	f.loc = loc
	sched := morning.Schedule{Hour: 8, Loc: loc}
	today := time.Now().In(loc)
	f.day = sched.On(today.Year(), today.Month(), today.Day())
	if time.Now().Before(f.day.Slot) {
		y := today.AddDate(0, 0, -1)
		f.day = sched.On(y.Year(), y.Month(), y.Day())
	}
	now := f.day.Slot.Add(5 * time.Minute)
	f.srv.now = func() time.Time { return now }
	f.srv.ext.MorningReport = &MorningReport{Store: morning.NewPG(f.pool), Schedule: sched}
	f.srv.ext.Push = f.push
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM morning_reports WHERE owner_id = $1`, `DELETE FROM chief_turn_usage WHERE owner_id = $1`,
			`DELETE FROM cc_account_usage WHERE owner_id = $1`} {
			_, _ = f.pool.Exec(sys, q, f.owner)
		}
	})
	return f
}

// ask files an open question to owner from its chief (asker).
func (f *morningFixture) ask(t *testing.T, owner, asker int64, q string) attention.Ask {
	t.Helper()
	a, err := f.srv.ext.Attention.Create(f.as(owner), attention.New{Asker: attention.Asker{AgentID: asker, Label: "chief"}, Question: q,
		Options: []string{"Ship it", "Hold"}})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// task files a task for owner in status, last changed at at.
func (f *morningFixture) task(t *testing.T, owner int64, title, status string, at time.Time) notes.Note {
	t.Helper()
	ctx := f.as(owner)
	n, err := f.notes.Create(ctx, notes.Note{Title: title, Directory: "example"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(authz.System(ctx), `UPDATE notes SET status = $2, updated_at = $3 WHERE id = $1`, n.ID, status, at); err != nil {
		t.Fatal(err)
	}
	return n
}

// askedAt moves ask id's creation to at.
func (f *morningFixture) askedAt(t *testing.T, id int64, at time.Time) {
	t.Helper()
	if _, err := f.pool.Exec(authz.System(f.ctx), `UPDATE attention_asks SET created_at = $2 WHERE id = $1`, id, at); err != nil {
		t.Fatal(err)
	}
}

// report builds and records owner's report of the fixture's day, as ctx.
func (f *morningFixture) report(t *testing.T, ctx context.Context, owner int64) (bool, morning.Row) {
	t.Helper()
	pushed, err := f.srv.morningReportFor(ctx, owner, f.day)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.WaitForPushes()
	row, ok, err := f.srv.ext.MorningReport.Store.Get(ctx, f.day.Date)
	if err != nil || !ok {
		t.Fatalf("no row for %s: %v, %v", f.day.Date, ok, err)
	}
	return pushed, row
}

// page is the report page of date as ctx.
func (f *morningFixture) page(t *testing.T, ctx context.Context, date string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, morningDatePath(date), nil).WithContext(ctx)
	req.SetPathValue("date", date)
	rec := httptest.NewRecorder()
	f.srv.handleMorningReport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("page: %d %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// TestTheMorningReportIsSentOncePerDayAcrossARestart: a loud day pushes once;
// the next tick, and a SECOND server on the same database (a restart, or a
// rolling update's second replica), push nothing more and build nothing new.
// The push is counts only and opens the day's page.
func TestTheMorningReportIsSentOncePerDayAcrossARestart(t *testing.T) {
	f := newMorningFixture(t)
	a := f.ask(t, f.owner, f.chief.ID, "Merge the exhibit-hall migration tonight?")
	f.askedAt(t, a.ID, f.day.Slot.Add(-time.Hour)) // inside the night
	if pushed, _ := f.report(t, f.asOwner, f.owner); !pushed {
		t.Fatal("the first run of a loud day sent no push")
	}
	if pushed, _ := f.report(t, f.asOwner, f.owner); pushed {
		t.Fatal("the second run of the same day pushed again")
	}
	// A restart: a new process on the same database, through the sweep's own
	// door (MorningReportPass, every owner under attentionPrincipal).
	s2 := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	s2.UseExtensions(f.srv.ext)
	s2.now = f.srv.now
	if _, err := s2.MorningReportPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	s2.WaitForPushes()
	got := f.push.to(f.owner)
	if len(got) != 1 {
		t.Fatalf("%d pushes to the owner across a restart; want exactly 1", len(got))
	}
	p := got[0]
	if p.Title != "While you were away" || p.Body != "1 question for you" || p.Tag != "morning-report" ||
		p.Data["url"] != "/morning/"+f.day.Date {
		t.Fatalf("the push = %q / %q / %q / %q", p.Title, p.Body, p.Tag, p.Data["url"])
	}
	if strings.Contains(p.Body, "exhibit-hall") {
		t.Fatalf("the push quotes the question: %q", p.Body)
	}
}

// TestARestartBetweenRecordAndPushStillPushesOnce: a row recorded but never
// pushed (the process died between the two) is pushed by the next run, once.
func TestARestartBetweenRecordAndPushStillPushesOnce(t *testing.T) {
	f := newMorningFixture(t)
	if _, err := f.srv.ext.MorningReport.Store.Insert(f.asOwner, morning.Row{Date: f.day.Date, Since: f.day.Since, Until: f.day.Slot,
		Questions: 3, Body: morning.Body{Done: []morning.Done{{Kind: morning.DoneMerged, Label: "Restock the booth", Ref: "acme/booth#2"}}}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		f.report(t, f.asOwner, f.owner)
	}
	got := f.push.to(f.owner)
	if len(got) != 1 || got[0].Body != "3 questions for you · 1 merged" {
		t.Fatalf("pushes = %d (%v); want one, \"3 questions for you · 1 merged\"", len(got), got)
	}
}

// TestAQuietDaySendsNothing: an owner with nothing open, nothing done and
// nothing newly stuck gets the day recorded as quiet and no push; its page
// says so. Positive control: the same owner with one question the next day
// is pushed.
func TestAQuietDaySendsNothing(t *testing.T) {
	f := newMorningFixture(t)
	quiet := f.newMember(t)
	ctx := f.as(quiet)
	quietChief := f.newAgent(t, quiet, "qchief-"+strconv.FormatInt(time.Now().UnixNano(), 36), "chief", "q-chief-token-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	// A session idle since BEFORE the night: listed as stuck, but it did not
	// happen overnight, so it does not make the day loud.
	f.window(quiet, "idle")
	f.srv.ext.Mirror = morningObservedMirror{digestMirror: f.mirror, at: f.day.Since.Add(-time.Hour)}
	pushed, row := f.report(t, ctx, quiet)
	if pushed || !row.Quiet || len(f.push.to(quiet)) != 0 {
		t.Fatalf("a quiet day: pushed %v, quiet %v, %d pushes; want no push and quiet", pushed, row.Quiet, len(f.push.to(quiet)))
	}
	if len(row.Body.Stuck) != 1 || row.Body.Stuck[0].Kind != morning.StuckIdle {
		t.Fatalf("CONTROL: the session stuck since before the night is not listed: %+v", row.Body.Stuck)
	}
	if page := f.page(t, ctx, f.day.Date); !strings.Contains(page, "Nothing happened, so no notification was sent.") {
		t.Fatalf("the quiet day's page does not say so:\n%.3000s", page)
	}
	// The control: a question makes the next day loud.
	next := f.day.Slot.Add(24 * time.Hour).In(f.loc)
	f.day = f.srv.ext.MorningReport.Schedule.On(next.Year(), next.Month(), next.Day())
	f.ask(t, quiet, quietChief.ID, "Pick the poster size?")
	if pushed, row := f.report(t, ctx, quiet); !pushed || row.Quiet || len(f.push.to(quiet)) != 1 {
		t.Fatalf("control: a loud day: pushed %v, quiet %v, %d pushes", pushed, row.Quiet, len(f.push.to(quiet)))
	}
}

// TestEverySectionIsFilledFromItsSource: questions, merged pull requests,
// finished tasks, failed agents, idle sessions and the chief's usage, each
// with a row INSIDE the window and one OUTSIDE it (or of the wrong kind) whose
// name no other row shares — so a filter that keeps too much or too little
// changes a literal.
func TestEverySectionIsFilledFromItsSource(t *testing.T) {
	f := newMorningFixture(t)
	in, done := f.seedEverySection(t)

	pushed, row := f.report(t, f.asOwner, f.owner)
	if !pushed || row.Quiet {
		t.Fatalf("a loud day: pushed %v, quiet %v", pushed, row.Quiet)
	}
	if row.Questions != 1 {
		t.Errorf("questions = %d; want 1", row.Questions)
	}
	var doneLabels, stuckNames []string
	for _, d := range row.Body.Done {
		doneLabels = append(doneLabels, d.Kind+":"+d.Label)
	}
	for _, s := range row.Body.Stuck {
		stuckNames = append(stuckNames, s.Kind+":"+s.Name)
	}
	if got, want := strings.Join(doneLabels, ","), "merged:Hang the lanterns over the gate,task:Hang the paper lanterns"; got != want {
		t.Errorf("done = %q; want %q", got, want)
	}
	if len(row.Body.Done) == 2 && row.Body.Done[1].Href != "/tasks/"+strconv.FormatInt(done.ID, 10) {
		t.Errorf("the task's link = %q", row.Body.Done[1].Href)
	}
	if len(stuckNames) != 2 || stuckNames[0] != "failed:"+f.worker.Name || !strings.HasPrefix(stuckNames[1], "idle:") {
		t.Errorf("stuck = %q; want the worker failed, then one idle session", stuckNames)
	}
	if len(row.Body.Stuck) > 0 && row.Body.Stuck[0].Why != "failed: the build broke" {
		t.Errorf("the failed agent's reason = %q", row.Body.Stuck[0].Why)
	}
	c := row.Body.Usage.Chief
	if !c.Tracked || c.Turns != 2 || c.Busy != 2*time.Minute {
		t.Errorf("chief usage = %+v; want 2 turns, 2m", c)
	}
	p := row.Body.Usage.Plan
	if !p.Tracked || len(p.Accounts) != 1 || *p.Accounts[0].FiveHour != 37 || *p.Accounts[0].Week != 62 {
		t.Errorf("plan usage = %+v", p)
	}
	if len(row.Body.Stuck) > 1 && !row.Body.Stuck[1].Since.Equal(in) {
		t.Errorf("the idle session's since = %v; want %v", row.Body.Stuck[1].Since, in)
	}
	if got := f.push.to(f.owner); len(got) != 1 || got[0].Body != "1 question for you · 1 merged · 2 stuck" {
		t.Errorf("the push = %v", got)
	}

	// How long the question has left, on the page's clock: the ask expires 3h
	// 30m (and a few seconds) from the moment the page is drawn.
	pageNow := time.Now()
	f.srv.now = func() time.Time { return pageNow }
	if _, err := f.pool.Exec(authz.System(f.ctx), `UPDATE attention_asks SET expires_at = $2 WHERE owner_id = $1 AND question = 'Approve the lantern budget?'`,
		f.owner, pageNow.Add(3*time.Hour+30*time.Minute+30*time.Second)); err != nil {
		t.Fatal(err)
	}
	page := f.page(t, f.asOwner, f.day.Date)
	for _, want := range []string{
		"Approve the lantern budget?", `action="/attention/asks/`, `name="return" value="/morning/` + f.day.Date + `"`,
		"Hang the lanterns over the gate", "Pull request merged at", "Hang the paper lanterns", "Complete task, changed overnight",
		"failed: the build broke", "idle, nothing running", "Answer within 3h 30m",
		"Your chief worked 2m over 2 turns.", "Claude plan 1: this 5-hour period 37% used, this week 62% used",
		"Cost isn&#39;t tracked for Claude plans.",
	} {
		if !strings.Contains(page, strings.ReplaceAll(want, `"`, `"`)) {
			t.Errorf("the page lacks %q", want)
		}
	}
	for _, never := range []string{"acme/old-lanterns", "acme/abandoned", "Order last year", "Sweep the courtyard", "Old failure",
		"acme/lanterns#41", "lantern-pool", "Older lanterns", "Abandoned lanterns"} {
		if strings.Contains(page, never) {
			t.Errorf("the page lists %q, which is outside the window or not done", never)
		}
	}
}

// seedEverySection gives the owner a row for every section INSIDE the window
// and, per section, one outside it or of the wrong kind; it returns the
// in-window instant and the finished task.
func (f *morningFixture) seedEverySection(t *testing.T) (time.Time, notes.Note) {
	t.Helper()
	in := f.day.Slot.Add(-2 * time.Hour)
	before := f.day.Since.Add(-3 * time.Hour)
	f.ask(t, f.owner, f.chief.ID, "Approve the lantern budget?")
	// Pull requests: merged in the window; merged before it; closed unmerged.
	key, _ := f.window(f.owner, "busy")
	f.pr(t, f.owner, key, "acme/lanterns", 41, &githubapp.PRState{State: "closed", Merged: true, ClosedAt: in, Title: "Hang the lanterns over the gate"})
	f.pr(t, f.owner, key, "acme/old-lanterns", 7, &githubapp.PRState{State: "closed", Merged: true, ClosedAt: before, Title: "Older lanterns"})
	f.pr(t, f.owner, key, "acme/abandoned", 9, &githubapp.PRState{State: "closed", Merged: false, ClosedAt: in, Title: "Abandoned lanterns"})
	// Tasks: complete in the window; complete before it; open in it.
	done := f.task(t, f.owner, "Hang the paper lanterns", notes.StatusComplete, in)
	f.task(t, f.owner, "Order last year's lanterns", notes.StatusComplete, before)
	f.task(t, f.owner, "Sweep the courtyard", notes.StatusOpen, in)
	// Agents: failed in the window; failed before it.
	if _, err := f.pool.Exec(authz.System(f.ctx), `UPDATE agents SET outcome = 'failed', outcome_reason = 'the build broke', outcome_at = $2 WHERE id = $1`,
		f.worker.ID, in); err != nil {
		t.Fatal(err)
	}
	old := f.newAgent(t, f.owner, "oldfail-"+strconv.FormatInt(time.Now().UnixNano(), 36), "Old failure", "old-fail-token-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	if _, err := f.pool.Exec(authz.System(f.ctx), `UPDATE agents SET outcome = 'failed', outcome_at = $2 WHERE id = $1`, old.ID, before); err != nil {
		t.Fatal(err)
	}
	// A session idle with nothing running since inside the window (the stall
	// facts).
	f.window(f.owner, "idle")
	// The window carries its host's label, "<codename>:<index>": a tmux place,
	// which the page must never draw as the session's name.
	f.mirror.mu.Lock()
	ws := f.mirror.own[f.owner]
	ws[len(ws)-1].Label = "Gold:2"
	f.mirror.mu.Unlock()
	f.srv.ext.Mirror = morningObservedMirror{digestMirror: f.mirror, at: in}
	// The chief's turns: two in the window (90 s and 30 s), one before it.
	for _, r := range []struct {
		at time.Time
		ms int64
	}{{in, 90000}, {in.Add(time.Minute), 30000}, {before, 3600000}} {
		if _, err := f.pool.Exec(f.as(f.owner), `INSERT INTO chief_turn_usage (chief_agent, trigger, outcome, input_tokens, duration_ms, created_at)
			VALUES ($1, 'owner_chat', 'other', 100, $2, $3)`, f.chief.ID, r.ms, r.at); err != nil {
			t.Fatal(err)
		}
	}
	// Plan usage: the poll on, one account's latest reading.
	cc := ccusage.NewPG(f.pool)
	f.srv.ext.CCUsage, f.srv.ext.CCUsageInterval = cc, 15*time.Minute
	five, week := 37.0, 62.0
	if err := cc.Save(authz.System(f.ctx), f.owner, ccusage.Row{Account: "lantern-pool", ReadAt: in, CheckedAt: in,
		Windows: map[string]ccusage.Window{ccusage.FiveHour: {Utilization: &five}, ccusage.SevenDay: {Utilization: &week}}}); err != nil {
		t.Fatal(err)
	}

	return in, done
}

// TestAMembersReportHoldsOnlyTheirOwnItems: A's report and page carry A's
// question and task and none of B's; B's page for the same day is B's own
// (no report), never A's.
func TestAMembersReportHoldsOnlyTheirOwnItems(t *testing.T) {
	f := newMorningFixture(t)
	in := f.day.Slot.Add(-time.Hour)
	b := f.newMember(t)
	bChief := f.newAgent(t, b, "bchief-"+strconv.FormatInt(time.Now().UnixNano(), 36), "chief", "b-chief-token-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	f.ask(t, f.owner, f.chief.ID, "A: frame the koi print?")
	f.task(t, f.owner, "A: hang the koi print", notes.StatusComplete, in)
	f.ask(t, b, bChief.ID, "B: repaint the gate?")
	f.task(t, b, "B: oil the hinges", notes.StatusComplete, in)

	_, row := f.report(t, f.asOwner, f.owner)
	if row.Questions != 1 || len(row.Body.Done) != 1 || row.Body.Done[0].Label != "A: hang the koi print" {
		t.Fatalf("A's report = %d questions, done %+v; want A's one each", row.Questions, row.Body.Done)
	}
	page := f.page(t, f.asOwner, f.day.Date)
	if !strings.Contains(page, "A: frame the koi print?") || strings.Contains(page, "B: ") {
		t.Fatalf("A's page: A's question %v, any of B's %v", strings.Contains(page, "A: frame"), strings.Contains(page, "B: "))
	}
	bPage := f.page(t, f.as(b), f.day.Date)
	if strings.Contains(bPage, "A: ") || !strings.Contains(bPage, `data-testid="morning-missing"`) {
		t.Fatalf("B's page for A's day shows A's items, or is not B's own empty one:\n%.3000s", bPage)
	}
	// A TEAM's question, which A's own browser can read (A manages the team,
	// and a browser session carries its teams): the Asks queue lists it — the
	// control that the filter below is reached — and the report does not.
	team := newTeam(t, f.pool, f.owner)
	if _, err := f.srv.ext.Attention.Create(f.srv.attentionPrincipal(context.Background(), team, true),
		attention.New{Asker: attention.Asker{System: true, Label: attention.SystemAskerLabel}, Question: "Team: reprint the banners?"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM attention_asks WHERE owner_id = $1`, team)
	})
	withTeams := authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.owner, Role: authz.RoleAdmin, Via: authz.ViaSession,
		Teams: authz.Memberships(strconv.FormatInt(team, 10) + ":manager")})
	queue, _ := f.srv.attentionItems(withTeams)
	listed := false
	for _, it := range queue {
		listed = listed || it.Text == "Team: reprint the banners?"
	}
	if !listed {
		t.Fatal("CONTROL: A's own queue does not list the team's question, so the report's owner filter is never reached")
	}
	if page := f.page(t, withTeams, f.day.Date); strings.Contains(page, "Team: reprint") || !strings.Contains(page, "A: frame the koi print?") {
		t.Fatalf("A's report page lists the team's question (or lost A's own)")
	}
}

// TestTheUsagePartSaysPlainlyWhenThePollIsOff: with the usage poll off the
// page says "Usage isn't tracked on this server." — never a zero — and only an
// admin's ⓘ holds the setting; a member's says to ask their admin.
func TestTheUsagePartSaysPlainlyWhenThePollIsOff(t *testing.T) {
	f := newMorningFixture(t)
	f.srv.ext.CCUsage, f.srv.ext.CCUsageInterval = ccusage.NewPG(f.pool), 0
	f.ask(t, f.owner, f.chief.ID, "Lock the doors at ten?")
	_, row := f.report(t, f.asOwner, f.owner)
	if row.Body.Usage.Plan.Tracked || row.Body.Usage.Plan.Off != "Usage isn't tracked on this server." {
		t.Fatalf("plan usage with the poll off = %+v", row.Body.Usage.Plan)
	}
	member := f.page(t, f.as(f.owner), f.day.Date)
	admin := f.page(t, f.asOwner, f.day.Date)
	for name, page := range map[string]string{"member": member, "admin": admin} {
		if !strings.Contains(page, "Usage isn&#39;t tracked on this server.") {
			t.Errorf("%s: the page does not say usage is not tracked", name)
		}
		if strings.Contains(page, "0% used") {
			t.Errorf("%s: the page shows a zero for an untracked source", name)
		}
	}
	if !strings.Contains(admin, `data-info-code="">MUSTER_CC_USAGE_POLL=1<`) {
		t.Errorf("admin: no ⓘ holding the setting")
	}
	if strings.Contains(member, "MUSTER_CC_USAGE_POLL") || !strings.Contains(member, ui.InfoSheetWithheldText) {
		t.Errorf("member: the setting is shown, or the ⓘ does not say to ask their admin")
	}
}

// morningJargon is what the report page's top layer must not hold beyond the
// shared scans (jargonInTopLayer: a tmux address; hostJargon: a flag, a
// mirror error, "tool calls"): an environment variable, a table or column
// name, or muster's own words for its internals.
var morningJargon = regexp.MustCompile(`MUSTER_[A-Z_]+|\b[a-z]+_[a-z_]+\b|\bepisodes?\b|\bstall(ed)?\b|\bRLS\b|\bprincipal\b|\bowner card\b`)

// TestTheMorningReportPageHasNoJargonInItsTopLayer: with every section filled
// and every untracked source said, what a person reads without opening an ⓘ —
// text, tooltips, labels, placeholders — names no variable, table, address or
// internal word, for an admin and a member alike. The setting is in the
// admin's ⓘ (the control that the scan skips sheets, and only sheets).
//
// The scan's own negative control: a top-layer line that spells the setting
// fails it.
func TestTheMorningReportPageHasNoJargonInItsTopLayer(t *testing.T) {
	f := newMorningFixture(t)
	f.seedEverySection(t)
	f.srv.ext.CCUsageInterval = 0 // the plan part says it is not tracked
	f.report(t, f.asOwner, f.owner)
	scan := func(what, page string) []string {
		var bad []string
		texts := topLayerTexts(t, sessionBody(t, page))
		if len(texts) < 10 {
			t.Fatalf("%s: CONTROL: only %d top-layer texts — the scan sees nothing", what, len(texts))
		}
		for _, s := range texts {
			if morningJargon.MatchString(s) || jargonInTopLayer.MatchString(s) || hostJargon.MatchString(s) {
				bad = append(bad, s)
			}
		}
		return bad
	}
	admin := f.page(t, f.asOwner, f.day.Date)
	for what, page := range map[string]string{"admin": admin, "member": f.page(t, f.as(f.owner), f.day.Date)} {
		if bad := scan(what, page); len(bad) > 0 {
			t.Errorf("%s: the top layer says %q", what, bad)
		}
	}
	if !strings.Contains(admin, `data-info-code="">MUSTER_CC_USAGE_POLL=1<`) {
		t.Errorf("CONTROL: the admin's ⓘ does not hold the setting, so the scan's skip of sheets is not exercised")
	}
	// The scan can go red: the same setting in the top layer is caught.
	leaked := strings.Replace(admin, "Usage isn&#39;t tracked on this server.", "Set MUSTER_CC_USAGE_POLL=1 to track usage.", 1)
	if leaked == admin || len(scan("negative control", leaked)) == 0 {
		t.Errorf("NEGATIVE CONTROL: a top-layer line naming the setting passed the scan")
	}
}

// TestAPullRequestNotCheckedKeepsTheDayLoud: a pull request whose state
// GitHub did not answer, on a day with nothing else, is a gap on the page AND
// keeps the day from being quiet (a merge may hide there) — so it pushes.
func TestAPullRequestNotCheckedKeepsTheDayLoud(t *testing.T) {
	f := newMorningFixture(t)
	key, _ := f.window(f.owner, "busy")
	f.pr(t, f.owner, key, "acme/unanswered", 5, nil) // GitHub answers nothing for it
	pushed, row := f.report(t, f.asOwner, f.owner)
	if row.Quiet || !pushed {
		t.Fatalf("an unchecked pull request: quiet %v, pushed %v; want loud and pushed", row.Quiet, pushed)
	}
	if len(row.Body.Gaps) != 1 || row.Body.Gaps[0].Text != "Some pull requests could not be checked just now." {
		t.Fatalf("gaps = %+v", row.Body.Gaps)
	}
}

// TestTheReadBoundOnPullRequestsIsSaidAndKeepsTheDayLoud: past the bound
// (morningPRsRead+1 pull requests, every one answered, none merged) the page
// says only that many were checked and the day is not quiet; exactly the bound
// is every pull request read, quiet, with no such line (the control).
func TestTheReadBoundOnPullRequestsIsSaidAndKeepsTheDayLoud(t *testing.T) {
	for _, tc := range []struct {
		n     int
		quiet bool
	}{{morningPRsRead, true}, {morningPRsRead + 1, false}} {
		f := newMorningFixture(t)
		owner := f.newMember(t)
		// An admin, so the GitHub App reads its pull requests.
		ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: owner, Role: authz.RoleAdmin, Via: authz.ViaSession})
		key, _ := f.window(owner, "busy")
		closed := f.day.Since.Add(-48 * time.Hour)
		for i := 1; i <= tc.n; i++ {
			f.pr(t, owner, key, "acme/bound-"+strconv.Itoa(tc.n), int64(i), &githubapp.PRState{State: "closed", Merged: false, ClosedAt: closed, Title: "unmerged"})
		}
		_, row := f.report(t, ctx, owner)
		capped := false
		for _, g := range row.Body.Gaps {
			capped = capped || g.Text == "Only your 30 most recent pull requests were checked."
		}
		if row.Quiet != tc.quiet || capped == tc.quiet {
			t.Errorf("%d pull requests: quiet %v (want %v), the bound said %v; gaps %+v", tc.n, row.Quiet, tc.quiet, capped, row.Body.Gaps)
		}
	}
}

// TestAQuestionAskedBeforeTheNightIsListedButKeepsTheDayQuiet: owner asks
// wait up to 24h, so one asked before the window would make every morning
// loud. It is listed on the page, counted for the push, and the day is quiet;
// the same question asked inside the window makes it loud (the control).
func TestAQuestionAskedBeforeTheNightIsListedButKeepsTheDayQuiet(t *testing.T) {
	f := newMorningFixture(t)
	owner := f.newMember(t)
	ctx := f.as(owner)
	chief := f.newAgent(t, owner, "ochief-"+strconv.FormatInt(time.Now().UnixNano(), 36), "chief", "o-chief-token-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	a := f.ask(t, owner, chief.ID, "Reprint last week's flyers?")
	if _, err := f.pool.Exec(authz.System(f.ctx), `UPDATE attention_asks SET created_at = $2 WHERE id = $1`, a.ID, f.day.Since.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	pushed, row := f.report(t, ctx, owner)
	if pushed || !row.Quiet || row.Questions != 1 {
		t.Fatalf("an old question: pushed %v, quiet %v, %d questions; want quiet, no push, 1 listed", pushed, row.Quiet, row.Questions)
	}
	if page := f.page(t, ctx, f.day.Date); !strings.Contains(page, "Reprint last week&#39;s flyers?") {
		t.Fatalf("the old question is not on the page")
	}
	// The control: the next day, a question asked inside its window.
	next := f.day.Slot.Add(24 * time.Hour).In(f.loc)
	f.day = f.srv.ext.MorningReport.Schedule.On(next.Year(), next.Month(), next.Day())
	f.ask(t, owner, chief.ID, "Order new flyers?")
	if pushed, row := f.report(t, ctx, owner); !pushed || row.Quiet {
		t.Fatalf("control: a question asked overnight: pushed %v, quiet %v", pushed, row.Quiet)
	}
}

// TestAStalledAgentIsNamedByItsNameNeverItsWindow: a dispatched agent whose
// last turn failed, with no human name, is listed by its own name (the agent
// fallback of stuckName); a labelled mirrored window with no human name is
// "A session", never its label (TestTheMorningReportPageHasNoJargonInItsTopLayer).
func TestAStalledAgentIsNamedByItsNameNeverItsWindow(t *testing.T) {
	f := newMorningFixture(t)
	f.chat(t, f.worker, agents.ChatMessage{Role: "user", Kind: "text", Content: "Paint the gate."},
		agents.ChatMessage{Role: "assistant", Kind: "text", Content: AgentTurnFailedPrefix + "the usage limit was reached"})
	_, row := f.report(t, f.asOwner, f.owner)
	var names []string
	for _, s := range row.Body.Stuck {
		names = append(names, s.Kind+":"+s.Name)
	}
	if len(names) != 1 || names[0] != "error:"+f.worker.Name {
		t.Fatalf("stuck = %q; want the worker, stopped on an error, by its name", names)
	}
}

// TestTheNightEndsAtTheReportTime: the window is [Since, Slot). A question
// asked a minute AFTER the report time, and an agent that failed then, belong
// to tomorrow's night: today's report lists the question (it is open) but it
// is not new, the agent is not listed, and the day is quiet. The same
// question a minute BEFORE the report time makes it loud (the control).
func TestTheNightEndsAtTheReportTime(t *testing.T) {
	f := newMorningFixture(t)
	owner := f.newMember(t)
	ctx := f.as(owner)
	chief := f.newAgent(t, owner, "nchief-"+strconv.FormatInt(time.Now().UnixNano(), 36), "chief", "n-chief-token-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	late := f.newAgent(t, owner, "nlate-"+strconv.FormatInt(time.Now().UnixNano(), 36), "Late painter", "n-late-token-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	if _, err := f.pool.Exec(authz.System(f.ctx), `UPDATE agents SET outcome = 'failed', outcome_at = $2 WHERE id = $1`, late.ID, f.day.Slot.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	a := f.ask(t, owner, chief.ID, "Open the gates early?")
	f.askedAt(t, a.ID, f.day.Slot.Add(time.Minute))
	pushed, row := f.report(t, ctx, owner)
	if pushed || !row.Quiet || row.Questions != 1 || len(row.Body.Stuck) != 0 {
		t.Fatalf("after the report time: pushed %v, quiet %v, %d questions, stuck %+v; want quiet, no push, 1 listed, none stuck",
			pushed, row.Quiet, row.Questions, row.Body.Stuck)
	}
	// The control: the next day, the same kind of question a minute before
	// ITS report time.
	next := f.day.Slot.Add(24 * time.Hour).In(f.loc)
	f.day = f.srv.ext.MorningReport.Schedule.On(next.Year(), next.Month(), next.Day())
	b := f.ask(t, owner, chief.ID, "Close the gates late?")
	f.askedAt(t, b.ID, f.day.Slot.Add(-time.Minute))
	if pushed, row := f.report(t, ctx, owner); !pushed || row.Quiet {
		t.Fatalf("control: a question a minute before the report time: pushed %v, quiet %v", pushed, row.Quiet)
	}
}

// fakeDaySpend is the chief OpenRouter sessions' store with a fixed spend per
// UTC day ("2006-01-02").
type fakeDaySpend struct {
	chiefusage.SessionLLMStore
	usd map[string]float64
}

func (f fakeDaySpend) DaySpent(_ context.Context, day time.Time) (float64, error) {
	return f.usd[day.UTC().Format("2006-01-02")], nil
}

// TestTheChiefsCostIsTheLastWholeUTCDay: with the chief's OpenRouter
// sessions on, the cost line is the spend of the last COMPLETE UTC day before
// the report time — the day the report time falls in is still running, and
// the next report counts it — pinned whole, with distinct amounts on the two
// days the night touches so a sum or the wrong day changes the text.
func TestTheChiefsCostIsTheLastWholeUTCDay(t *testing.T) {
	f := newMorningFixture(t)
	slotDay := f.day.Slot.UTC()
	prev := slotDay.AddDate(0, 0, -1).Format("2006-01-02")
	f.srv.chiefSessions = &chiefSessionsRuntime{store: fakeDaySpend{usd: map[string]float64{
		prev: 0.11, slotDay.Format("2006-01-02"): 2.30}}}
	f.ask(t, f.owner, f.chief.ID, "Light the lanterns?")
	_, row := f.report(t, f.asOwner, f.owner)
	if c := row.Body.Usage.Cost; !c.Tracked || c.USD != 0.11 {
		t.Fatalf("cost = %+v; want $0.11 on %s", c, prev)
	}
	want := "Your chief&#39;s OpenRouter cost on " + prev + " (UTC, the last whole day): $0.11."
	if page := f.page(t, f.asOwner, f.day.Date); !strings.Contains(page, want) {
		t.Fatalf("the page lacks %q", want)
	}
}
