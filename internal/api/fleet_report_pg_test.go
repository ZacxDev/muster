package api

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/prroute"
)

// THE FLEET REPORT (fleet_report.go), through the real server against a real
// Postgres: a verified, listed sender's fleet_report_request is answered by
// muster itself with an already-approved reply listing the owner's sessions in
// the window — ended ones included, another owner's never — and every other
// mail is handed on exactly as before.
//
// Fixture values are invented and pairwise distinct: the sender, the member's
// login, the hosts, the session ids and every focus title differ from each
// other and from every other test's.

const (
	frSender     = "standup-desk/report-ask"
	frMember     = "Example-Member"
	frEndedSID   = "5e55a1d0-0000-4000-8000-0000000e0ded"
	frForeignSID = "5e55a1d0-0000-4000-8000-0000000f0e19"
	frOldTitle   = "Bisecting the flaky widget cache test"
	frLiveTitle  = "Wiring the gizmo export button"
	frEndTitle   = "Reviewing the sprocket migration"
	frStopTitle  = "Indexing the doohickey catalogue"
	frChiefTitle = "Triage of the morning inbox"
	frForeign    = "Another owner's thingamajig rewrite"
)

// fleetMirror is the route fixture's mirror plus one OFFLINE host.
type fleetMirror struct {
	*routeMirror
	offlineSince time.Time
}

func (m fleetMirror) Hosts(ctx context.Context) []mirror.HostView {
	return append(m.routeMirror.Hosts(ctx), mirror.HostView{Host: "lab-2", OfflineSince: m.offlineSince})
}

type frFixture struct {
	*prFixture
	focus   *focus.PGStore
	stopped agents.Agent
	other   int64
}

func newFleetReportFixture(t *testing.T) *frFixture {
	t.Helper()
	f := &frFixture{prFixture: newPRFixture(t)}
	f.focus = focus.NewPG(f.pool)
	f.srv.ext.Focus = f.focus
	f.srv.ext.Mirror = fleetMirror{routeMirror: f.mirror, offlineSince: time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)}
	senders, err := plugins.ParseAgentMailSenders(frSender)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.ext.FleetReport = &FleetReport{Senders: senders, Members: plugins.FleetReportMembers{f.owner: frMember}, Replies: f.plugins}
	sfx := strconv.FormatInt(time.Now().UnixNano(), 36)
	f.stopped = f.newAgent(t, f.owner, "frstop-"+sfx, "catalogue indexer", "frstop-tok-"+sfx)
	if err := f.srv.ext.Agents.UpdateStatus(f.asOwner, f.stopped.ID, agents.StatusStopped, "", ""); err != nil {
		t.Fatal(err)
	}
	f.other = f.newMember(t)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM session_prs WHERE owner_id = $1`, f.other)
	})

	set := func(ctx context.Context, sub focus.Subject, title string) {
		t.Helper()
		if _, err := f.focus.Upsert(ctx, sub, focus.Write{Title: title, Source: focus.SourceSummarizer}); err != nil {
			t.Fatal(err)
		}
	}
	// The live window's session: an old title (three days ago, outside the
	// window) and a current one.
	set(f.asOwner, focus.MirrorSubject(routeSID), frOldTitle)
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE agent_focus_history SET created_at = now() - interval '3 days'
		WHERE subject_key = '`+routeSID+`' AND title = '`+frOldTitle+`'`)
	set(f.asOwner, focus.MirrorSubject(routeSID), frLiveTitle)
	// An ENDED mirrored session: no window shows it; it opened a pull request.
	set(f.asOwner, focus.MirrorSubject(frEndedSID), frEndTitle)
	f.authored(t, f.asOwner, frEndedSID, prroute.Signal{Kind: prroute.KindPRCreate, Repo: prRepo, PR: prNumber})
	// A STOPPED agent, and the chief itself (left out of its own report).
	set(f.asOwner, focus.AgentSubject(f.stopped.ID), frStopTitle)
	set(f.asOwner, focus.AgentSubject(f.chief.ID), frChiefTitle)
	// Another owner's session, in the same window, with a pull request too.
	set(f.as(f.other), focus.MirrorSubject(frForeignSID), frForeign)
	f.authored(t, f.as(f.other), frForeignSID, prroute.Signal{Kind: prroute.KindPRCreate, Repo: prRepo, PR: prNumber + 1})
	return f
}

// requestBody is a fleet_report_request for member over the last day.
func requestBody(member, id string) string {
	now := time.Now().UTC()
	return `{"type":"fleet_report_request","v":1,"request_id":"` + id + `","member":"` + member +
		`","window":{"since":"` + now.Add(-24*time.Hour).Format(time.RFC3339) + `","until":"` + now.Add(time.Minute).Format(time.RFC3339) +
		`","zone":"Europe/Berlin"}}`
}

// ask stores one agent-mail message and hands it on, waited for.
func (f *frFixture) ask(t *testing.T, external, sender string, verified bool, body string) plugins.Message {
	t.Helper()
	m, created, err := f.plugins.Submit(f.asOwner, f.agentMail.ID, plugins.MessageIn{ExternalID: external, Sender: sender,
		Subject: "fleet-report-request", Body: body, SenderVerified: verified})
	if err != nil || !created {
		t.Fatalf("submit: %v %v", created, err)
	}
	f.dispatch(t, m)
	return m
}

// drafts is every reply draft for m.
func (f *frFixture) drafts(t *testing.T, m plugins.Message) []plugins.Draft {
	t.Helper()
	rows, err := f.pool.Query(authz.System(context.Background()), `SELECT id FROM plugin_reply_drafts WHERE message_id = $1 ORDER BY id`, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	var out []plugins.Draft
	for _, id := range ids {
		d, err := f.plugins.Draft(f.asOwner, id)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

type frReply struct {
	Type        string            `json:"type"`
	V           int               `json:"v"`
	RequestID   string            `json:"request_id"`
	GeneratedAt string            `json:"generated_at"`
	Window      map[string]string `json:"window"`
	Error       string            `json:"error"`
	Coverage    *struct {
		Sessions int      `json:"sessions"`
		Omitted  int      `json:"omitted"`
		Hosts    []string `json:"hosts"`
		Gaps     []string `json:"gaps"`
	} `json:"coverage"`
	Sessions []struct {
		ID, Kind, Name, Host, State string
		Repo                        *string
		FirstSeen                   string `json:"first_seen"`
		LastSeen                    string `json:"last_seen"`
		Focus                       []struct{ Title, At string }
		PRs                         []string `json:"prs"`
	} `json:"sessions"`
}

func decodeReply(t *testing.T, body string) frReply {
	t.Helper()
	var r frReply
	dec := json.NewDecoder(strings.NewReader(body))
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("the reply is not JSON: %v\n%s", err, body)
	}
	return r
}

// 🔴 TestAVerifiedListedRequestIsAnsweredWithTheWindowsSessions: the whole
// door, and the report's content — the ended session and the stopped agent
// are there with their state; the out-of-window title, the chief and the other
// owner's session are not.
func TestAVerifiedListedRequestIsAnsweredWithTheWindowsSessions(t *testing.T) {
	f := newFleetReportFixture(t)
	m := f.ask(t, "fr-ok-1", frSender, true, requestBody("example-member", "req-0001"))
	ds := f.drafts(t, m)
	if len(ds) != 1 || ds[0].Status != plugins.DraftApproved || ds[0].AgentID == nil || *ds[0].AgentID != f.chief.ID {
		t.Fatalf("drafts %+v; want ONE approved draft under the chief", ds)
	}
	if n := f.turns.count(); n != 0 {
		t.Fatalf("the chief took %d turn(s); muster answers a fleet-report request itself", n)
	}
	got, err := f.plugins.Message(f.asOwner, m.ID)
	if err != nil || got.Status != plugins.StatusDelivered || got.AgentID == nil || *got.AgentID != f.chief.ID || got.SettleReason == nil {
		t.Fatalf("the message %+v (%v); want delivered to the chief and settled", got, err)
	}
	// The plugin's next claim collects it: the at-most-once send path, unchanged.
	c, ok, err := f.plugins.ClaimDraft(f.asOwner, f.agentMail.ID)
	if err != nil || !ok || c.DraftID != ds[0].ID || c.MessageExternalID != "fr-ok-1" || c.Body != ds[0].Body {
		t.Fatalf("claim %+v %v %v; want this draft", c, ok, err)
	}

	r := decodeReply(t, ds[0].Body)
	if r.Type != "fleet_report" || r.V != 1 || r.RequestID != "req-0001" || r.Error != "" || r.Window["zone"] != "Europe/Berlin" ||
		r.Coverage == nil || r.GeneratedAt == "" {
		t.Fatalf("reply header %+v", r)
	}
	by := map[string]int{}
	for i, s := range r.Sessions {
		by[s.ID] = i
	}
	live, ended, stopped := "mirror:"+routeSID, "mirror:"+frEndedSID, "agent:"+strconv.FormatInt(f.stopped.ID, 10)
	for _, id := range []string{live, ended, stopped} {
		if _, ok := by[id]; !ok {
			t.Fatalf("the report lacks %s: %s", id, ds[0].Body)
		}
	}
	if s := r.Sessions[by[live]]; s.State != "active" || s.Host != "tower" || s.Kind != "mirror" || len(s.Focus) != 1 || s.Focus[0].Title != frLiveTitle {
		t.Errorf("the live session: %+v; want active on tower with only its in-window title", s)
	}
	if s := r.Sessions[by[ended]]; s.State != "ended" || len(s.Focus) != 1 || s.Focus[0].Title != frEndTitle ||
		len(s.PRs) != 1 || s.PRs[0] != prRepo+"#"+strconv.Itoa(prNumber) {
		t.Errorf("the ended session: %+v; want ended, its title and its pull request", s)
	}
	if s := r.Sessions[by[stopped]]; s.State != "ended" || s.Kind != "agent" || s.Name != "catalogue indexer" {
		t.Errorf("the stopped agent: %+v; want ended, named", s)
	}
	for _, never := range []string{frOldTitle, frChiefTitle, frForeign, frForeignSID, "agent:" + strconv.FormatInt(f.chief.ID, 10) + `"`,
		prRepo + "#" + strconv.Itoa(prNumber+1)} {
		if strings.Contains(ds[0].Body, never) {
			t.Errorf("the report carries %q: %s", never, ds[0].Body)
		}
	}
	if r.Coverage.Sessions != len(r.Sessions) || r.Coverage.Omitted != 0 || strings.Join(r.Coverage.Hosts, ",") != "tower" {
		t.Errorf("coverage %+v", *r.Coverage)
	}
	gapped := false
	for _, g := range r.Coverage.Gaps {
		gapped = gapped || strings.HasPrefix(g, "host lab-2 has been offline since 2000-01-02T03:04:05Z")
	}
	if !gapped {
		t.Errorf("the offline host is not a gap: %v", r.Coverage.Gaps)
	}
}

// Every mail that is not a verified, listed sender's request is handed on as
// before: to the chief, with no draft.
func TestAnythingButAVerifiedListedRequestIsHandedOnAsBefore(t *testing.T) {
	f := newFleetReportFixture(t)
	cases := []struct {
		name, sender, body string
		verified           bool
	}{
		{"unverified", frSender, requestBody("example-member", "req-u"), false},
		{"not listed", "standup-desk/other-bot", requestBody("example-member", "req-n"), true},
		{"not JSON", frSender, "Could you send me the fleet report for yesterday?", true},
		{"another type", frSender, `{"type":"fleet_report","v":1}`, true},
		{"JSON then prose", frSender, requestBody("example-member", "req-p") + "\nthanks!", true},
	}
	for i, c := range cases {
		before := f.turns.count()
		m := f.ask(t, "fr-on-"+strconv.Itoa(i), c.sender, c.verified, c.body)
		if ds := f.drafts(t, m); len(ds) != 0 {
			t.Errorf("%s: drafts %+v; want none", c.name, ds)
		}
		if f.turns.count() != before+1 {
			t.Errorf("%s: the chief took %d turn(s); want 1 (the ordinary delivery)", c.name, f.turns.count()-before)
		}
	}
}

// 🔴 A member that is not the owner gets an error and no data — and so does
// an owner whose GitHub login is not configured.
func TestAMemberWhoIsNotTheOwnerGetsAnErrorAndNoData(t *testing.T) {
	f := newFleetReportFixture(t)
	m := f.ask(t, "fr-mm-1", frSender, true, requestBody("someone-else", "req-mm"))
	ds := f.drafts(t, m)
	if len(ds) != 1 || ds[0].Status != plugins.DraftApproved {
		t.Fatalf("drafts %+v; want one approved error reply", ds)
	}
	r := decodeReply(t, ds[0].Body)
	if r.Type != "fleet_report" || r.RequestID != "req-mm" || !strings.Contains(r.Error, "not its owner") || r.Sessions != nil || r.Coverage != nil {
		t.Fatalf("reply %s; want an error and no data", ds[0].Body)
	}
	for _, never := range []string{routeSID, frEndedSID, frLiveTitle} {
		if strings.Contains(ds[0].Body, never) {
			t.Fatalf("the error reply carries %q", never)
		}
	}
	f.srv.ext.FleetReport.Members = plugins.FleetReportMembers{f.owner + 1000: frMember}
	m = f.ask(t, "fr-mm-2", frSender, true, requestBody("example-member", "req-mm2"))
	if ds := f.drafts(t, m); len(ds) != 1 || !strings.Contains(decodeReply(t, ds[0].Body).Error, "not its owner") {
		t.Fatalf("an owner with no configured login: %+v", ds)
	}
}

// A request whose window breaks the contract is answered with an error.
func TestABadWindowIsAnsweredWithAnError(t *testing.T) {
	f := newFleetReportFixture(t)
	body := strings.Replace(requestBody("example-member", "req-w"), `"since":"`, `"since":"1999-01-01T00:00:00Z","x":"`, 1)
	m := f.ask(t, "fr-bw-1", frSender, true, body)
	ds := f.drafts(t, m)
	if len(ds) != 1 || !strings.Contains(decodeReply(t, ds[0].Body).Error, "longer than 4 days") {
		t.Fatalf("drafts %+v; want an error reply about the window", ds)
	}
}

// 🔴 THE SIZE CAP: sessions are left out, counted in omitted, and the
// encoding fits.
func TestTheSizeCapCountsWhatItLeavesOut(t *testing.T) {
	f := newFleetReportFixture(t)
	now := time.Now()
	full, err := f.srv.buildFleetReport(f.asOwner, f.chief.ID, now.Add(-time.Hour), now.Add(time.Minute), fleetWindow{}, nil, noFleetScreen, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	all := decodeReply(t, full)
	if len(all.Sessions) < 3 || all.Coverage.Omitted != 0 {
		t.Fatalf("control: the uncapped report has %d sessions, omitted %d", len(all.Sessions), all.Coverage.Omitted)
	}
	limit := len(full) - 1
	capped, err := f.srv.buildFleetReport(f.asOwner, f.chief.ID, now.Add(-time.Hour), now.Add(time.Minute), fleetWindow{}, nil, noFleetScreen, limit)
	if err != nil {
		t.Fatal(err)
	}
	r := decodeReply(t, capped)
	if len(capped) > limit || r.Coverage.Omitted < 1 || len(r.Sessions)+r.Coverage.Omitted != len(all.Sessions) ||
		r.Coverage.Sessions != len(r.Sessions) {
		t.Fatalf("capped at %d: %d bytes, %d sessions, omitted %d (of %d)", limit, len(capped), len(r.Sessions), r.Coverage.Omitted, len(all.Sessions))
	}
}

// The mail door screens each free-text field; the chief's tool does not.
func TestTheMailWithholdsAScreenedTitleTheToolShows(t *testing.T) {
	f := newFleetReportFixture(t)
	const leaky = "Rotating the kubectl credentials"
	if _, err := f.focus.Upsert(f.asOwner, focus.MirrorSubject(frEndedSID), focus.Write{Title: leaky, Source: focus.SourceSummarizer}); err != nil {
		t.Fatal(err)
	}
	m := f.ask(t, "fr-scr-1", frSender, true, requestBody("example-member", "req-scr"))
	ds := f.drafts(t, m)
	if len(ds) != 1 || strings.Contains(ds[0].Body, leaky) || !strings.Contains(ds[0].Body, fleetWithheld) ||
		!strings.Contains(ds[0].Body, "field(s) withheld by muster's reply screen") {
		t.Fatalf("drafts %+v; want the title withheld and counted", ds)
	}
	text := f.srv.AgentToolDispatch(f.asOwner, f.chief)("fleet_report", `{"since":"`+time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)+`"}`)
	if !strings.Contains(text, leaky) || !strings.Contains(text, frEndedSID) || !strings.Contains(text, `"untrusted"`) {
		t.Fatalf("the tool: %s", text)
	}
}

// The tool refuses a window it would not read.
func TestTheToolRefusesABadWindow(t *testing.T) {
	f := newFleetReportFixture(t)
	call := f.srv.AgentToolDispatch(f.asOwner, f.chief)
	for args, want := range map[string]string{
		`{}`:                    "arguments are {since, until?} (RFC 3339)",
		`{"since":"yesterday"}`: "since is not an RFC 3339 time",
		`{"since":"2000-01-01T00:00:00Z","until":"2000-01-06T00:00:00Z"}`: "the window is longer than 4 days",
		`{"since":"2000-01-02T00:00:00Z","until":"2000-01-01T00:00:00Z"}`: "the window is empty: since must be before until",
	} {
		if got := call("fleet_report", args); got != toolErr("%s", want) {
			t.Errorf("%s: %s; want %q", args, got, want)
		}
	}
}
