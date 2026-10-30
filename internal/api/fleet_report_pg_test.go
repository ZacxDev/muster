package api

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
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
// the window that work under the configured prefixes — ended ones included,
// another owner's never, hosts never named — and every other mail is handed on
// exactly as before.
//
// Fixture values are invented placeholders and pairwise distinct: the sender,
// the prefix, the session ids and every focus title differ from each other and
// from every other test's. No real path, host or deny word appears.

const (
	frSender     = "standup-desk/report-ask"
	frPrefix     = "/srv/example-work"
	frEndedSID   = "5e55a1d0-0000-4000-8000-0000000e0ded"
	frForeignSID = "5e55a1d0-0000-4000-8000-0000000f0e19"
	frSiblingSID = "5e55a1d0-0000-4000-8000-00000005b11a"
	frOutsideSID = "5e55a1d0-0000-4000-8000-0000000007ae"
	frUnknownSID = "5e55a1d0-0000-4000-8000-0000000c0de0"
	frOldTitle   = "Bisecting the flaky widget cache test"
	frLiveTitle  = "Wiring the gizmo export button"
	frEndTitle   = "Reviewing the sprocket migration"
	frStopTitle  = "Indexing the doohickey catalogue"
	frChiefTitle = "Triage of the morning inbox"
	frForeign    = "Another owner's thingamajig rewrite"
	frSibTitle   = "Sibling-prefix whatsit refactor"
	frOutTitle   = "Personal doodad side project"
	frUnkTitle   = "Unplaced gubbins experiment"
)

// fleetMirror is the route fixture's mirror plus one OFFLINE host — with, when
// clash is set, a retained window whose title ("work:1") is the online one's,
// so the mirror's own list qualifies both titles with their hosts.
type fleetMirror struct {
	*routeMirror
	offlineSince time.Time
	clash        string // the offline window's Claude session, "" for none
}

func (m fleetMirror) Hosts(ctx context.Context) []mirror.HostView {
	off := mirror.HostView{Host: "lab-2", OfflineSince: m.offlineSince}
	if m.clash != "" {
		off.Host = "hostbeta"
		off.Windows = mirror.NormalizeWindows([]mirror.Window{{Session: "work", ID: "@1", Index: 1, Active: true, Claude: true,
			ClaudeSession: m.clash}}, 0)
		off.Sessions, off.Claude, off.Labels, off.Activity = mirror.SessionsOf(off.Windows)
	}
	return append(m.routeMirror.Hosts(ctx), off)
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
	f.srv.ext.Attention = attention.NewPG(f.pool)
	f.srv.ext.Mirror = fleetMirror{routeMirror: f.mirror, offlineSince: time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)}
	// 🔴 NO PERMISSION ROUTER (a standalone deployment): the directories come
	// from the sessions' transcripts, stored with their focus.
	f.srv.UseRouter(nil)
	senders, err := plugins.ParseAgentMailSenders(frSender)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.ext.FleetReport = &FleetReport{Senders: senders, Paths: plugins.FleetReportPaths{frPrefix}, Replies: f.plugins}
	sfx := strconv.FormatInt(time.Now().UnixNano(), 36)
	f.stopped = f.newAgent(t, f.owner, "frstop-"+sfx, "catalogue indexer", "frstop-tok-"+sfx)
	if err := f.srv.ext.Agents.UpdateStatus(f.asOwner, f.stopped.ID, agents.StatusStopped, "", ""); err != nil {
		t.Fatal(err)
	}
	f.other = f.newMember(t)
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		_, _ = f.pool.Exec(sys, `DELETE FROM session_prs WHERE owner_id = $1`, f.other)
		_, _ = f.pool.Exec(sys, `DELETE FROM attention_asks WHERE owner_id = $1`, f.owner)
	})

	// The live window's session: an old title (three days ago, outside the
	// window) and a current one.
	f.cwd(t, f.asOwner, focus.MirrorSubject(routeSID), frOldTitle, frPrefix+"/app")
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE agent_focus_history SET created_at = now() - interval '3 days'
		WHERE subject_key = '`+routeSID+`' AND title = '`+frOldTitle+`'`)
	f.set(t, f.asOwner, focus.MirrorSubject(routeSID), frLiveTitle) // names no cwd: the stored one stays
	// An ENDED mirrored session under the prefix: no window shows it; it opened
	// a pull request.
	f.cwd(t, f.asOwner, focus.MirrorSubject(frEndedSID), frEndTitle, frPrefix)
	f.authored(t, f.asOwner, frEndedSID, prroute.Signal{Kind: prroute.KindPRCreate, Repo: prRepo, PR: prNumber})
	// Sessions the reply must not describe: a sibling-prefix directory, one
	// outside, one with no recorded directory, a STOPPED agent (no directory
	// muster could know), and the chief itself.
	f.cwd(t, f.asOwner, focus.MirrorSubject(frSiblingSID), frSibTitle, frPrefix+"-x/app")
	f.cwd(t, f.asOwner, focus.MirrorSubject(frOutsideSID), frOutTitle, "/opt/example-other")
	f.set(t, f.asOwner, focus.MirrorSubject(frUnknownSID), frUnkTitle)
	f.set(t, f.asOwner, focus.AgentSubject(f.stopped.ID), frStopTitle)
	f.set(t, f.asOwner, focus.AgentSubject(f.chief.ID), frChiefTitle)
	// Another owner's session, in the same window and under the prefix, with a
	// pull request too.
	f.cwd(t, f.as(f.other), focus.MirrorSubject(frForeignSID), frForeign, frPrefix+"/app")
	f.authored(t, f.as(f.other), frForeignSID, prroute.Signal{Kind: prroute.KindPRCreate, Repo: prRepo, PR: prNumber + 1})
	return f
}

func (f *frFixture) set(t *testing.T, ctx context.Context, sub focus.Subject, title string) {
	t.Helper()
	f.cwd(t, ctx, sub, title, "")
}

// cwd writes sub's focus as the refresher does, with the working directory
// its tail named ("" for none).
func (f *frFixture) cwd(t *testing.T, ctx context.Context, sub focus.Subject, title, dir string) {
	t.Helper()
	if _, err := f.focus.Upsert(ctx, sub, focus.Write{Title: title, Source: focus.SourceSummarizer, Cwd: dir, CwdNamed: dir != ""}); err != nil {
		t.Fatal(err)
	}
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

// reply is the one approved draft answering m, decoded.
func (f *frFixture) reply(t *testing.T, m plugins.Message) (frReply, string) {
	t.Helper()
	ds := f.drafts(t, m)
	if len(ds) != 1 || ds[0].Status != plugins.DraftApproved || ds[0].AgentID == nil || *ds[0].AgentID != f.chief.ID {
		t.Fatalf("drafts %+v; want ONE approved draft under the chief", ds)
	}
	return decodeReply(t, ds[0].Body), ds[0].Body
}

type frSession struct {
	ID, Kind, Name, Host, State string
	Repo                        *string
	FirstSeen                   string `json:"first_seen"`
	LastSeen                    string `json:"last_seen"`
	Focus                       []struct{ Title, At string }
	PRs                         []string `json:"prs"`
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
	Sessions []frSession `json:"sessions"`
}

func (r frReply) byID() map[string]frSession {
	out := map[string]frSession{}
	for _, s := range r.Sessions {
		out[s.ID] = s
	}
	return out
}

func (r frReply) hasGap(prefix string) bool {
	for _, g := range r.Coverage.Gaps {
		if strings.HasPrefix(g, prefix) {
			return true
		}
	}
	return false
}

func decodeReply(t *testing.T, body string) frReply {
	t.Helper()
	var r frReply
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&r); err != nil {
		t.Fatalf("the reply is not JSON: %v\n%s", err, body)
	}
	return r
}

// 🔴 TestAVerifiedListedRequestIsAnsweredWithTheWindowsSessions: the whole
// door, and the report's content — only the sessions under the prefix (the
// live one, and the ENDED one with its pull request), no host anywhere; the
// sibling-prefix, outside, unknown-directory and agent sessions, the chief,
// the out-of-window title and the other owner's session are not there.
func TestAVerifiedListedRequestIsAnsweredWithTheWindowsSessions(t *testing.T) {
	f := newFleetReportFixture(t)
	m := f.ask(t, "fr-ok-1", frSender, true, requestBody("example-member", "req-0001"))
	r, body := f.reply(t, m)
	if n := f.turns.count(); n != 0 {
		t.Fatalf("the chief took %d turn(s); muster answers a fleet-report request itself", n)
	}
	got, err := f.plugins.Message(f.asOwner, m.ID)
	if err != nil || got.Status != plugins.StatusDelivered || got.AgentID == nil || *got.AgentID != f.chief.ID || got.SettleReason == nil {
		t.Fatalf("the message %+v (%v); want delivered to the chief and settled", got, err)
	}
	// The plugin's next claim collects it: the at-most-once send path, unchanged.
	if c, ok, err := f.plugins.ClaimDraft(f.asOwner, f.agentMail.ID); err != nil || !ok || c.MessageExternalID != "fr-ok-1" || c.Body != body {
		t.Fatalf("claim %+v %v %v; want this draft", c, ok, err)
	}

	if r.Type != "fleet_report" || r.V != 1 || r.RequestID != "req-0001" || r.Error != "" || r.Window["zone"] != "Europe/Berlin" ||
		r.Coverage == nil || r.GeneratedAt == "" {
		t.Fatalf("reply header %+v", r)
	}
	by := r.byID()
	live, ended := "mirror:"+routeSID, "mirror:"+frEndedSID
	if len(r.Sessions) != 2 {
		t.Fatalf("the reply describes %d sessions; want exactly the two under the prefix: %s", len(r.Sessions), body)
	}
	if s, ok := by[live]; !ok || s.State != "active" || s.Host != "" || s.Kind != "mirror" || len(s.Focus) != 1 || s.Focus[0].Title != frLiveTitle {
		t.Errorf("the live session: %+v %v; want active, NO host, only its in-window title", s, ok)
	}
	if s, ok := by[ended]; !ok || s.State != "ended" || len(s.Focus) != 1 || s.Focus[0].Title != frEndTitle ||
		len(s.PRs) != 1 || s.PRs[0] != prRepo+"#"+strconv.Itoa(prNumber) {
		t.Errorf("the ended session: %+v %v; want ended, its title and its pull request", s, ok)
	}
	for _, never := range []string{frOldTitle, frChiefTitle, frForeign, frForeignSID, frSibTitle, frSiblingSID, frOutTitle, frOutsideSID,
		frUnkTitle, frUnknownSID, frStopTitle, "agent:", prRepo + "#" + strconv.Itoa(prNumber+1), "tower", "lab-2", frPrefix} {
		if strings.Contains(body, never) {
			t.Errorf("the reply carries %q: %s", never, body)
		}
	}
	if r.Coverage.Sessions != 2 || r.Coverage.Omitted != 0 || r.Coverage.Hosts == nil || len(r.Coverage.Hosts) != 0 {
		t.Errorf("coverage %+v; want 2 sessions and an EMPTY host list", *r.Coverage)
	}
	// The unknown-directory mirrored session and the stopped agent are
	// counted; the sibling and outside sessions are not mentioned at all.
	if !r.hasGap("2 session(s) whose working directory muster does not know were left out") ||
		!r.hasGap("1 mirror host(s) offline") {
		t.Errorf("gaps %v", r.Coverage.Gaps)
	}

	// The chief's tool shows the owner everything, hosts included.
	text := f.srv.AgentToolDispatch(f.asOwner, f.chief)("fleet_report", `{"since":"`+time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)+`"}`)
	for _, want := range []string{frSiblingSID, frOutsideSID, frUnknownSID, "agent:" + strconv.Itoa(int(f.stopped.ID)), `"host":"tower"`,
		"host lab-2 has been offline since 2000-01-02T03:04:05Z", `"untrusted"`} {
		if !strings.Contains(text, want) {
			t.Errorf("the tool lacks %q: %s", want, text)
		}
	}
	if strings.Contains(text, frForeignSID) || strings.Contains(text, frChiefTitle) {
		t.Errorf("the tool shows another owner's session or the chief: %s", text)
	}
}

// With no stored directory nothing is described — fail closed — and the
// count says so; a malformed stored directory is unknown too.
func TestNoKnownWorkingDirectoryDescribesNothing(t *testing.T) {
	f := newFleetReportFixture(t)
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE agent_focus SET cwd = '' WHERE owner_id = `+strconv.FormatInt(f.owner, 10))
	r, body := f.reply(t, f.ask(t, "fr-nr-1", frSender, true, requestBody("example-member", "req-nr")))
	if len(r.Sessions) != 0 || !r.hasGap("6 session(s) whose working directory muster does not know were left out") ||
		strings.Contains(body, frLiveTitle) {
		t.Fatalf("no directories: %s", body)
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

// D1 (operator decision): the request's member is not checked — any member
// gets the owner's report, which row-level security already scopes.
func TestTheMemberIsNotChecked(t *testing.T) {
	f := newFleetReportFixture(t)
	r, body := f.reply(t, f.ask(t, "fr-mm-1", frSender, true, requestBody("someone-else", "req-mm")))
	if r.Error != "" || len(r.Sessions) != 2 || strings.Contains(body, frForeign) {
		t.Fatalf("reply %s; want the owner's report, whatever member says", body)
	}
}

// A request whose window breaks the contract is answered with an error.
func TestABadWindowIsAnsweredWithAnError(t *testing.T) {
	f := newFleetReportFixture(t)
	body := strings.Replace(requestBody("example-member", "req-w"), `"since":"`, `"since":"1999-01-01T00:00:00Z","x":"`, 1)
	r, _ := f.reply(t, f.ask(t, "fr-bw-1", frSender, true, body))
	if !strings.Contains(r.Error, "longer than 4 days") || r.Sessions != nil || r.Coverage != nil {
		t.Fatalf("reply %+v; want an error reply about the window, no data", r)
	}
}

// 🔴 THE SIZE CAP: sessions are left out, counted in omitted, and the
// encoding fits.
func TestTheSizeCapCountsWhatItLeavesOut(t *testing.T) {
	f := newFleetReportFixture(t)
	now := time.Now()
	full, err := f.srv.buildFleetReport(f.asOwner, f.chief.ID, now.Add(-time.Hour), now.Add(time.Minute), fleetWindow{}, nil, nil, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	all := decodeReply(t, full)
	if len(all.Sessions) < 3 || all.Coverage.Omitted != 0 {
		t.Fatalf("control: the uncapped report has %d sessions, omitted %d", len(all.Sessions), all.Coverage.Omitted)
	}
	limit := len(full) - 1
	capped, err := f.srv.buildFleetReport(f.asOwner, f.chief.ID, now.Add(-time.Hour), now.Add(time.Minute), fleetWindow{}, nil, nil, limit)
	if err != nil {
		t.Fatal(err)
	}
	r := decodeReply(t, capped)
	if len(capped) > limit || r.Coverage.Omitted < 1 || len(r.Sessions)+r.Coverage.Omitted != len(all.Sessions) ||
		r.Coverage.Sessions != len(r.Sessions) {
		t.Fatalf("capped at %d: %d bytes, %d sessions, omitted %d (of %d)", limit, len(capped), len(r.Sessions), r.Coverage.Omitted, len(all.Sessions))
	}
}

// Q1 (operator decision): titles go UNSCREENED by the agent-text screen — a
// dotted host name and an infrastructure word pass — except that a
// secret-shaped run is still cut out, and counted.
func TestTheMailKeepsInfraWordsAndCutsOnlySecrets(t *testing.T) {
	f := newFleetReportFixture(t)
	const infra = "Debugging kubectl access to build.example.com"
	const secret = "ghp_" + "AbCdEfGhIjKlMnOpQrStUv0123"
	f.set(t, f.asOwner, focus.MirrorSubject(frEndedSID), infra)
	f.set(t, f.asOwner, focus.MirrorSubject(routeSID), "Rotating the token "+secret+" today")
	r, body := f.reply(t, f.ask(t, "fr-scr-1", frSender, true, requestBody("example-member", "req-scr")))
	titles := map[string]bool{}
	for _, s := range r.Sessions {
		for _, t := range s.Focus {
			titles[t.Title] = true
		}
	}
	if !titles[infra] {
		t.Errorf("the infra title was withheld or changed: %s", body)
	}
	if strings.Contains(body, secret) || strings.Contains(body, secret[4:]) || !titles["Rotating the token [removed] today"] ||
		!r.hasGap("1 secret-shaped string(s) removed") {
		t.Errorf("the secret was not cut out and counted: %s", body)
	}
	text := f.srv.AgentToolDispatch(f.asOwner, f.chief)("fleet_report", `{"since":"`+time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)+`"}`)
	if !strings.Contains(text, infra) {
		t.Errorf("the tool: %s", text)
	}
}

// D3 (operator decision): a session with an open attention item is "waiting",
// over "active".
func TestASessionWithAPendingAskIsWaiting(t *testing.T) {
	f := newFleetReportFixture(t)
	if _, err := f.srv.ext.Attention.Create(f.asOwner, attention.New{Asker: attention.Asker{MirrorHost: "tower", MirrorSession: routeSID,
		Label: "work:1"}, Question: "Which gizmo export format?"}); err != nil {
		t.Fatal(err)
	}
	r, body := f.reply(t, f.ask(t, "fr-wait-1", frSender, true, requestBody("example-member", "req-wait")))
	by := r.byID()
	if by["mirror:"+routeSID].State != "waiting" || by["mirror:"+frEndedSID].State != "ended" {
		t.Fatalf("states: %s; want the asking session waiting and the other ended", body)
	}
	if strings.Contains(body, "Which gizmo export format") {
		t.Fatalf("the reply quotes the ask: %s", body)
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

// 🔴 NO HOST LABEL ANYWHERE IN THE REPLY, EVEN WHEN TWO HOSTS' TITLES CLASH.
// The online host's "work:1" and a retained offline host's "work:1" make the
// mirror's list qualify each name with its host ("work:1 · tower"); the reply
// must carry the unqualified title, and no host label in any field.
func TestNoHostLabelLeavesEvenWhenTitlesClash(t *testing.T) {
	f := newFleetReportFixture(t)
	f.srv.ext.Mirror = fleetMirror{routeMirror: f.mirror, offlineSince: time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC), clash: frEndedSID}
	// Control: the chief's tool, which may show hosts, does show the qualified
	// names — so the clash is real and the assertion below can see one.
	text := f.srv.AgentToolDispatch(f.asOwner, f.chief)("fleet_report", `{"since":"`+time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)+`"}`)
	if !strings.Contains(text, "work:1 · tower") || !strings.Contains(text, "work:1 · hostbeta") {
		t.Fatalf("control: the tool does not show the host-qualified names, so the fixture has no clash: %s", text)
	}
	r, body := f.reply(t, f.ask(t, "fr-clash-1", frSender, true, requestBody("example-member", "req-clash")))
	by := r.byID()
	if by["mirror:"+routeSID].Name != "work:1" || by["mirror:"+frEndedSID].Name != "work:1" {
		t.Errorf("names %q / %q; want the unqualified title", by["mirror:"+routeSID].Name, by["mirror:"+frEndedSID].Name)
	}
	for _, host := range []string{"tower", "hostbeta", "lab-2", "·"} {
		if strings.Contains(body, host) {
			t.Errorf("the reply carries %q: %s", host, body)
		}
	}
}

// 🔴 THE CAP COUNTS ONLY WHAT THE REPLY MAY DESCRIBE. More than
// focus.MaxWindowSubjects sessions OUTSIDE the prefixes, all more recent than
// the two inside: the reply still describes the two, and its omitted count
// says nothing about the others.
func TestTheSubjectCapNeverCountsSessionsOutsideThePrefixes(t *testing.T) {
	f := newFleetReportFixture(t)
	for i := 0; i < focus.MaxWindowSubjects+5; i++ {
		f.cwd(t, f.asOwner, focus.MirrorSubject("outside-"+strconv.Itoa(i)), "Elsewhere step "+strconv.Itoa(i), "/opt/example-other")
	}
	r, body := f.reply(t, f.ask(t, "fr-cap-1", frSender, true, requestBody("example-member", "req-cap")))
	by := r.byID()
	if _, ok := by["mirror:"+routeSID]; !ok || len(r.Sessions) != 2 || r.Coverage.Omitted != 0 {
		t.Fatalf("%d sessions, omitted %d; want the two in-prefix ones and omitted 0: %.600s", len(r.Sessions), r.Coverage.Omitted, body)
	}
}

// A session's pull requests are capped at fleetMaxPRs in a reply, and the
// excess is counted.
func TestASessionsPullRequestsAreCapped(t *testing.T) {
	f := newFleetReportFixture(t)
	for i := 1; i <= fleetMaxPRs+3; i++ {
		f.authored(t, f.asOwner, routeSID, prroute.Signal{Kind: prroute.KindPRCreate, Repo: "acme-labs/gizmo-" + strconv.Itoa(i), PR: int64(i)})
	}
	r, body := f.reply(t, f.ask(t, "fr-prs-1", frSender, true, requestBody("example-member", "req-prs")))
	if n := len(r.byID()["mirror:"+routeSID].PRs); n != fleetMaxPRs || !r.hasGap("3 pull request(s) left out (at most 50 per session)") {
		t.Fatalf("%d prs; gaps %v: %.300s", n, r.Coverage.Gaps, body)
	}
	text := f.srv.AgentToolDispatch(f.asOwner, f.chief)("fleet_report", `{"since":"`+time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)+`"}`)
	if !strings.Contains(text, "acme-labs/gizmo-53#53") {
		t.Fatalf("the tool, which never leaves muster, must show them all: %.300s", text)
	}
}
