package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/fleet"
	"github.com/ZacxDev/muster/internal/githubapp"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/prroute"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/sessionstate"
	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/internal/users"
)

// THE FLEET READER, THE DIGEST CARD AND THE FLEET CHAT (fleet_read.go,
// fleet_digest.go) on a real Postgres: the owner's sessions in their states,
// grouped with a count per group no other group has; another owner's sessions
// and a team's window absent; fleet_list's new fields as literals; the Fleet
// chat opened with a draft and nothing sent. Every id, title, repo and number
// is invented and pairwise distinct.

// digestMirror is a mirror whose Hosts are per owner (by the caller's
// principal) and whose VisibleHosts adds a TEAM host the owner can see — the
// window the fleet must never list.
type digestMirror struct {
	*routeMirror
	mu        sync.Mutex
	own       map[int64][]mirror.Window
	team      []mirror.Window
	teamOwner int64
	visible   int // VisibleHosts calls, the positive control's
}

func fleetHostView(host string, owner int64, raw []mirror.Window) mirror.HostView {
	ws := mirror.NormalizeWindows(raw, 0)
	h := mirror.HostView{Host: host, Owner: owner, Windows: ws, Online: true, Rendered: true, Interactive: true, PanePaste: true, StatusReport: true}
	h.Sessions, h.Claude, h.Labels, h.Activity = mirror.SessionsOf(ws)
	return h
}

func (m *digestMirror) Hosts(ctx context.Context) []mirror.HostView {
	p, _ := authz.From(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	if ws := m.own[p.UserID]; len(ws) > 0 {
		return []mirror.HostView{fleetHostView("bench-"+strconv.FormatInt(p.UserID, 10), 0, ws)}
	}
	return nil
}

func (m *digestMirror) VisibleHosts(ctx context.Context) []mirror.HostView {
	m.mu.Lock()
	m.visible++
	team := m.team
	m.mu.Unlock()
	out := m.Hosts(ctx)
	if len(team) > 0 {
		out = append(out, fleetHostView("shared-rig", m.teamOwner, team))
	}
	return out
}

// AgentState is the window's own status, as the host reports it.
func (m *digestMirror) AgentState(ctx context.Context, host, session, window string) mirror.AgentState {
	for _, h := range m.VisibleHosts(ctx) {
		if h.Host != host {
			continue
		}
		for _, w := range h.Windows {
			if w.ID == window {
				return mirror.AgentState{Status: w.Status, Since: w.StatusSince}
			}
		}
	}
	return mirror.AgentState{}
}

// fakeFleetPRs is GitHub's pull-request read: the answers by "repo#n", an
// error for any other.
type fakeFleetPRs struct {
	mu     sync.Mutex
	states map[string]githubapp.PRState
	reads  int
}

func (f *fakeFleetPRs) PullRequestState(_ context.Context, repo string, n int64) (githubapp.PRState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	st, ok := f.states[repo+"#"+strconv.FormatInt(n, 10)]
	if !ok {
		return githubapp.PRState{}, errors.New("not found")
	}
	return st, nil
}

type fleetFixture struct {
	*gatedFixture
	mirror *digestMirror
	prs    *fakeFleetPRs
	eps    *sessionstate.PG
	queue  *sendqueue.PGStore
	n      int
}

func newFleetFixture(t *testing.T) *fleetFixture {
	t.Helper()
	f := &fleetFixture{mirror: &digestMirror{routeMirror: &routeMirror{interactive: true, statusReport: true}, own: map[int64][]mirror.Window{}},
		prs: &fakeFleetPRs{states: map[string]githubapp.PRState{}}}
	var pool *pgxpool.Pool
	f.gatedFixture = newGatedFixtureWith(t, func(g *gatedFixture, ext *Extensions) {
		ext.Mirror = f.mirror
		ext.PRSessions = g.plugins
		ext.FleetPRStates = f.prs
		ext.FleetDigest = true
	})
	pool = f.pool
	f.eps = sessionstate.NewPG(pool)
	f.queue = sendqueue.NewPG(pool)
	f.srv.ext.Episodes, f.srv.ext.Fleet, f.srv.ext.SendQueue = f.eps, fleet.NewPG(pool), f.queue
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM session_prs WHERE owner_id = $1`, `DELETE FROM send_queue WHERE owner_id = $1`,
			`DELETE FROM fleet_owner_marks WHERE owner_id = $1`, `DELETE FROM session_episodes WHERE owner_id = $1`,
			`DELETE FROM attention_asks WHERE owner_id = $1`} {
			_, _ = f.pool.Exec(sys, q, f.owner)
		}
	})
	return f
}

// sid is the n-th invented Claude session id of this fixture.
func (f *fleetFixture) sid() string {
	f.n++
	return "5e55a1d0-0000-4000-8000-" + strings.Repeat("0", 8) + "f" + strconv.FormatInt(int64(1000+f.n), 10)[1:]
}

// window lists a Claude Code window for owner in status, returning its
// session key and window id.
func (f *fleetFixture) window(owner int64, status string) (string, string) {
	sid := f.sid()
	f.mirror.mu.Lock()
	defer f.mirror.mu.Unlock()
	id := "@" + strconv.Itoa(10+len(f.mirror.own[owner]))
	f.mirror.own[owner] = append(f.mirror.own[owner], mirror.Window{Session: "work", ID: id, Index: len(f.mirror.own[owner]) + 1,
		Claude: true, ClaudeSession: sid, Status: status, StatusSince: 1700000000000})
	return sid, id
}

// stop opens an episode for owner's session key in state (waiting: opened
// waiting), opened at the fixed instant at.
func (f *fleetFixture) stop(t *testing.T, owner int64, key, window, state string, waiting bool, at time.Time) sessionstate.Episode {
	t.Helper()
	ctx := f.as(owner)
	ep, ok, err := f.eps.Open(ctx, sessionstate.Episode{Kind: sessionstate.KindMirror, Key: key, State: state, Waiting: waiting, Asked: true,
		Host: "bench-" + strconv.FormatInt(owner, 10), TmuxSession: "work", Window: window, StreakKey: "since:1700000000000"})
	if err != nil || !ok {
		t.Fatalf("open %s: %v %v", key, ok, err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE session_episodes SET opened_at = $2 WHERE id = $1`, ep.ID, at); err != nil {
		t.Fatal(err)
	}
	ep.OpenedAt = at
	return ep
}

// pr records owner's session key creating repo#n, GitHub answering st.
func (f *fleetFixture) pr(t *testing.T, owner int64, key, repo string, n int64, st *githubapp.PRState) {
	t.Helper()
	f.record(t, f.as(owner), key, prroute.Signal{Kind: prroute.KindPRCreate, Repo: repo, PR: n})
	if st != nil {
		f.prs.mu.Lock()
		f.prs.states[repo+"#"+strconv.FormatInt(n, 10)] = *st
		f.prs.mu.Unlock()
	}
}

// digest renders the card as uid (look: the panel opened on it).
func (f *fleetFixture) digest(t *testing.T, uid int64, look bool) string {
	t.Helper()
	path := ui.FleetDigestPath
	if look {
		path += "?look=1"
	}
	rec := httptest.NewRecorder()
	f.srv.handleFleetDigest(rec, httptest.NewRequest(http.MethodGet, path, nil).WithContext(f.as(uid)))
	if rec.Code != http.StatusOK {
		t.Fatalf("digest: %d %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

var (
	fleetChipCount   = regexp.MustCompile(`data-fleet-chip="([a-z_]+)"[^>]*><span>[^<]*</span><span data-fleet-count=""[^>]*>([0-9]+)</span>`)
	fleetDetailCount = regexp.MustCompile(`<details data-fleet-group="([a-z_]+)"[^>]*><summary[^>]*><span[^>]*>[^<]*</span><span data-fleet-count=""[^>]*>([0-9]+)</span>`)
)

// counts is the card's per-group counts, as the chips and as the desktop
// groups draw them; the two must agree.
func counts(t *testing.T, html string) map[string]int {
	t.Helper()
	read := func(re *regexp.Regexp) map[string]int {
		out := map[string]int{}
		for _, m := range re.FindAllStringSubmatch(html, -1) {
			n, _ := strconv.Atoi(m[2])
			out[m[1]] = n
		}
		return out
	}
	chips, details := read(fleetChipCount), read(fleetDetailCount)
	if len(chips) != 5 || len(details) != 5 {
		t.Fatalf("instrument: %d chips and %d groups parsed, want 5 of each:\n%s", len(chips), len(details), html)
	}
	for k, n := range chips {
		if details[k] != n {
			t.Fatalf("group %s: chip %d, desktop %d", k, n, details[k])
		}
	}
	return chips
}

var fleetT0 = time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)

// TestTheDigestCountsEachGroupFromAFleetWithDistinctCounts: a fleet whose five
// groups hold 6, 1, 5, 4 and 3 — Needs you (2 prompts on screen, 3 needs-you
// stops, the chief's own open card), Waiting on CI (a stall with its pull
// request open), Working (4 windows and the worker agent; the chief itself is
// not listed), Done (stalls whose latest pull request merged or closed), and
// Stalled (no pull request; one merged with a message queued; one GitHub
// cannot answer for). A session sent to the wrong group moves two counts.
func TestTheDigestCountsEachGroupFromAFleetWithDistinctCounts(t *testing.T) {
	f := newFleetFixture(t)
	o := f.owner
	ctx := f.as(o)
	merged := &githubapp.PRState{State: "closed", Merged: true, HeadSHA: "0123456789abcdef0123456789abcdef01234567", ClosedAt: fleetT0}
	for i := 0; i < 2; i++ {
		f.window(o, mirror.StatusWaiting)
	}
	for i := 0; i < 3; i++ {
		k, w := f.window(o, mirror.StatusIdle)
		f.stop(t, o, k, w, sessionstate.StateNeedsYou, false, fleetT0)
	}
	if _, err := f.srv.ext.Attention.Create(ctx, attention.New{Asker: attention.Asker{AgentID: f.chief.ID}, Question: "Merge the parser fix now?",
		Options: []string{"Yes", "Not yet"}, OwnerCard: true}); err != nil {
		t.Fatal(err)
	}
	// Not the chief's cards, so not in Needs you: the worker's own question,
	// and a plain (non-card) ask of the chief's.
	if _, err := f.srv.ext.Attention.Create(ctx, attention.New{Asker: attention.Asker{AgentID: f.worker.ID}, Question: "Which branch?",
		Options: []string{"main", "next"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.ext.Attention.Create(ctx, attention.New{Asker: attention.Asker{AgentID: f.worker.ID}, Question: "Card from the worker?",
		Options: []string{"Yes", "No"}, OwnerCard: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.ext.Attention.Create(ctx, attention.New{Asker: attention.Asker{AgentID: f.chief.ID}, Question: "A plain chief ask",
		Options: []string{"Yes", "No"}}); err != nil {
		t.Fatal(err)
	}
	k, w := f.window(o, mirror.StatusIdle)
	f.stop(t, o, k, w, sessionstate.StateStalled, false, fleetT0)
	f.pr(t, o, k, "example-org/widget", 101, &githubapp.PRState{State: "open"})
	for i := 0; i < 4; i++ {
		f.window(o, mirror.StatusBusy)
	}
	for i := 0; i < 4; i++ {
		k, w := f.window(o, mirror.StatusIdle)
		f.stop(t, o, k, w, sessionstate.StateStalled, false, fleetT0)
		st := merged
		if i == 3 {
			st = &githubapp.PRState{State: "closed"}
		}
		f.pr(t, o, k, "example-org/widget", int64(110+i), st)
	}
	k, w = f.window(o, mirror.StatusIdle) // stalled, no pull request
	f.stop(t, o, k, w, sessionstate.StateStalled, false, fleetT0)
	k, w = f.window(o, mirror.StatusIdle) // merged, but a message queued to it
	f.stop(t, o, k, w, sessionstate.StateStalled, false, fleetT0)
	f.pr(t, o, k, "example-org/widget", 120, merged)
	if _, err := f.queue.Enqueue(ctx, sendqueue.Item{Target: sendqueue.Target{Host: "bench-" + strconv.FormatInt(o, 10), Session: "work", Window: w},
		Body: "and the changelog", StepUpAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	k, w = f.window(o, mirror.StatusIdle) // GitHub does not answer for its pull request
	f.stop(t, o, k, w, sessionstate.StateStalled, false, fleetT0)
	f.pr(t, o, k, "example-org/widget", 130, nil)

	html := f.digest(t, o, false)
	want := map[string]int{"needs_you": 6, "waiting_ci": 1, "working": 5, "done": 4, "stalled": 3}
	got := counts(t, html)
	for g, n := range want {
		if got[g] != n {
			t.Errorf("group %s: %d; want %d", g, got[g], n)
		}
	}
	if !strings.Contains(html, ">18 sessions<") {
		t.Errorf("total: want 18 sessions (17 windows and the worker; not the chief):\n%s", html)
	}
	if !strings.Contains(html, `data-fleet-ask=`) || !strings.Contains(html, "Chief asks: Merge the parser fix now?") {
		t.Errorf("the chief's own open card is not in Needs you")
	}
	if strings.Contains(html, f.chief.Name) {
		t.Errorf("the chief itself is listed as a session of its fleet")
	}
}

// TestTheDigestIsTheOwnersOwnFleet: another owner's windows and agents are
// absent from the owner's card, fleet_list and drill-in; the other owner's own
// card lists them (the positive control: their absence is the scope, not an
// empty fixture); and a TEAM's window the owner can see (VisibleHosts) is in
// none of the owner's fleet.
func TestTheDigestIsTheOwnersOwnFleet(t *testing.T) {
	f := newFleetFixture(t)
	stranger := f.newMember(t)
	strangerAgent := f.newAgent(t, stranger, "fleetstranger-"+strconv.FormatInt(time.Now().UnixNano(), 36), "", "fleet-stranger-token-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	mine, _ := f.window(f.owner, mirror.StatusBusy)
	theirs, theirWin := f.window(stranger, mirror.StatusIdle)
	f.stop(t, stranger, theirs, theirWin, sessionstate.StateStalled, false, fleetT0)
	teamKey := f.sid()
	f.mirror.mu.Lock()
	f.mirror.teamOwner = stranger + 1_000
	f.mirror.team = []mirror.Window{{Session: "shared", ID: "@90", Index: 1, Claude: true, ClaudeSession: teamKey, Status: mirror.StatusWaiting, StatusSince: 1}}
	f.mirror.mu.Unlock()

	// The positive control for the team window: it IS visible to the owner.
	var sawTeam bool
	for _, h := range f.mirror.VisibleHosts(f.asOwner) {
		for _, w := range h.Windows {
			sawTeam = sawTeam || w.ClaudeSession == teamKey
		}
	}
	if !sawTeam {
		t.Fatal("fixture: the team window is not in the owner's VisibleHosts")
	}

	ownHTML := f.digest(t, f.owner, false)
	if !strings.Contains(ownHTML, `data-fleet-row="mirror:`+mine+`"`) {
		t.Fatalf("control: the owner's own window is not on its card:\n%s", ownHTML)
	}
	for what, needle := range map[string]string{"another owner's window": theirs, "another owner's agent": "agent:" + strconv.FormatInt(strangerAgent.ID, 10),
		"a team's window": teamKey} {
		if strings.Contains(ownHTML, needle) {
			t.Errorf("the owner's card holds %s (%s)", what, needle)
		}
	}
	list := f.srv.toolFleetList(f.asOwner, `{}`)
	for _, needle := range []string{theirs, teamKey, "agent:" + strconv.FormatInt(strangerAgent.ID, 10)} {
		if strings.Contains(list, needle) {
			t.Errorf("the owner's fleet_list holds %s", needle)
		}
	}
	// The drill-in answers the stranger's session the same as a gone one.
	for _, id := range []string{"mirror:" + theirs, "mirror:" + teamKey, "mirror:not-a-session"} {
		rec := httptest.NewRecorder()
		f.srv.handleFleetDrill(rec, httptest.NewRequest(http.MethodGet, ui.FleetDrillPath+"?id="+url.QueryEscape(id), nil).WithContext(f.asOwner))
		if !strings.Contains(rec.Body.String(), "data-fleet-drill-failed") || strings.Contains(rec.Body.String(), "data-fleet-drill-in") {
			t.Errorf("the owner's drill-in of %s answered: %s", id, rec.Body.String())
		}
	}
	// A TEAM's agent the owner can see (team sharing: the owner operates the
	// team) is the team chief's, never in the owner's fleet.
	store := users.NewPG(f.pool)
	team, err := store.CreateTeam(authz.System(context.Background()), users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession},
		"fleet team "+strconv.FormatInt(time.Now().UnixNano(), 36), f.owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		_, _ = f.pool.Exec(sys, `DELETE FROM agents WHERE owner_id = $1`, team.ID)
		_, _ = f.pool.Exec(sys, `DELETE FROM team_members WHERE team_id = $1`, team.ID)
		_, _ = f.pool.Exec(sys, `DELETE FROM audit_log WHERE team_id = $1::bigint OR object = $2`, team.ID, users.TeamObject(team.ID))
		_, _ = f.pool.Exec(sys, `DELETE FROM users WHERE id = $1`, team.ID)
	})
	if err := store.SetTeamMember(authz.System(context.Background()), users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession},
		team.ID, f.owner, authz.TeamOperator); err != nil {
		t.Fatal(err)
	}
	teamAgent, err := agents.NewPG(f.pool).Create(authz.WithPrincipal(context.Background(), teamActingPrincipal(team.ID, authz.ViaAgent)),
		agents.Agent{Name: "fleetteam-" + strconv.FormatInt(time.Now().UnixNano(), 36), Namespace: "ns-fleet-team", Status: agents.StatusRunning,
			HooksToken: "fleet-team-hooks-" + strconv.FormatInt(time.Now().UnixNano(), 36)})
	if err != nil {
		t.Fatal(err)
	}
	ms, err := authz.NewMemberships([]authz.Membership{{Team: team.ID, Role: authz.TeamOperator}})
	if err != nil {
		t.Fatal(err)
	}
	asMember := authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.owner, Role: authz.RoleMember, Via: authz.ViaSession, Teams: ms})
	// The positive control: the owner's list (as a member) holds the team's agent.
	visibleAgents, err := f.srv.ext.Agents.List(asMember)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(visibleAgents, func(a agents.Agent) bool { return a.ID == teamAgent.ID }) {
		t.Fatalf("fixture: the team's agent is not in the member owner's agent list")
	}
	teamRow := "agent:" + strconv.FormatInt(teamAgent.ID, 10)
	if s := f.srv.toolFleetList(asMember, `{}`); strings.Contains(s, `"`+teamRow+`"`) {
		t.Errorf("the owner's fleet_list (as a member of the team) holds the team's agent %s", teamRow)
	}
	rec := httptest.NewRecorder()
	f.srv.handleFleetDigest(rec, httptest.NewRequest(http.MethodGet, ui.FleetDigestPath, nil).WithContext(asMember))
	if strings.Contains(rec.Body.String(), `data-fleet-row="`+teamRow+`"`) {
		t.Errorf("the owner's card (as a member of the team) holds the team's agent")
	}

	// The stranger's own card does list them.
	theirHTML := f.digest(t, stranger, false)
	if !strings.Contains(theirHTML, `data-fleet-row="mirror:`+theirs+`"`) || !strings.Contains(theirHTML, `data-fleet-row="agent:`+strconv.FormatInt(strangerAgent.ID, 10)+`"`) {
		t.Errorf("control: the stranger's own card does not list its window and agent:\n%s", theirHTML)
	}
}

// TestFleetListCarriesTheFleetFields: fleet_list's new fields, as literals,
// on a stall with an open pull request and an owner
// message followed by two of muster's texts; on a working window; on a
// prompt on screen; and on the worker agent.
func TestFleetListCarriesTheFleetFields(t *testing.T) {
	f := newFleetFixture(t)
	o := f.owner
	stalled, sw := f.window(o, mirror.StatusIdle)
	ep := f.stop(t, o, stalled, sw, sessionstate.StateStalled, false, fleetT0)
	if _, err := f.pool.Exec(f.asOwner, `UPDATE session_episodes SET jev_label = 'asks_permission' WHERE id = $1`, ep.ID); err != nil {
		t.Fatal(err)
	}
	f.pr(t, o, stalled, "example-org/gadget", 42, &githubapp.PRState{State: "open", HeadSHA: "fedcba9876543210fedcba9876543210fedcba98"})
	working, _ := f.window(o, mirror.StatusBusy)
	waiting, _ := f.window(o, mirror.StatusWaiting)
	ownerAt := time.Date(2000, 1, 2, 2, 30, 0, 0, time.UTC)
	f.srv.nudgeReadHook = func(_ context.Context, c chiefnudge.Session) (nudgeRead, error) {
		if c.Key != stalled {
			return nudgeRead{}, errors.New("not this fixture's stall")
		}
		return nudgeRead{Msgs: []chiefnudge.Msg{
			{Role: "user", Text: "fix the gadget", ID: "line:1", At: ownerAt},
			{Role: "assistant", Text: "done; shall I continue?"},
			{Role: "user", Text: chiefnudge.NudgeLabel + " continue", ID: "line:2", At: ownerAt.Add(time.Minute)},
			{Role: "user", Text: RoutedPrefacePrefix + " a review arrived", ID: "line:3", At: ownerAt.Add(2 * time.Minute)},
		}}, nil
	}

	var got struct {
		Sessions []map[string]json.RawMessage `json:"sessions"`
	}
	text := f.srv.toolFleetList(f.asOwner, `{}`)
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("%v:\n%s", err, text)
	}
	byID := map[string]map[string]json.RawMessage{}
	for _, r := range got.Sessions {
		var id string
		_ = json.Unmarshal(r["id"], &id)
		byID[id] = r
	}
	field := func(id, k string) string {
		r, ok := byID[id]
		if !ok {
			t.Fatalf("no row %s in:\n%s", id, text)
		}
		return string(r[k])
	}
	epID := strconv.FormatInt(ep.ID, 10)
	for _, tc := range []struct{ id, key, want string }{
		{"mirror:" + stalled, "state", `"stalled"`},
		{"mirror:" + stalled, "episode", `{"id":` + epID + `,"since":"2000-01-02T03:04:05Z","jev_label":"asks_permission"}`},
		{"mirror:" + stalled, "prs", `[{"repo":"example-org/gadget","number":42,"state":"open","head_sha12":"fedcba987654"}]`},
		{"mirror:" + stalled, "last_owner_message_at", `"2000-01-02T02:30:00Z"`},
		{"mirror:" + stalled, "chief_texts_since_owner", `2`},
		{"mirror:" + working, "state", `"working"`},
		{"mirror:" + working, "episode", ``},
		{"mirror:" + working, "prs", `[]`},
		{"mirror:" + working, "last_owner_message_at", ``},
		{"mirror:" + working, "chief_texts_since_owner", ``},
		{"mirror:" + waiting, "state", `"waiting"`},
		{"agent:" + strconv.FormatInt(f.worker.ID, 10), "state", `"working"`},
		{"agent:" + strconv.FormatInt(f.worker.ID, 10), "prs", ``},
	} {
		if g := field(tc.id, tc.key); g != tc.want {
			t.Errorf("%s %s = %s; want %s", tc.id, tc.key, g, tc.want)
		}
	}
	if strings.Contains(text, "state_unavailable") {
		t.Errorf("state is on: no state_unavailable note:\n%s", text)
	}
	// Session state off: no state, said so.
	f.srv.ext.Episodes = nil
	off := f.srv.toolFleetList(f.asOwner, `{}`)
	var offRows struct {
		Sessions []map[string]json.RawMessage `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(off), &offRows); err != nil || len(offRows.Sessions) != len(got.Sessions) {
		t.Fatalf("session state off: %v:\n%s", err, off)
	}
	for _, r := range offRows.Sessions {
		if _, has := r["state"]; has {
			t.Errorf("session state off: a row carries a state: %s", off)
		}
		if _, has := r["episode"]; has {
			t.Errorf("session state off: a row carries an episode: %s", off)
		}
	}
	if !strings.Contains(off, `"state_unavailable"`) {
		t.Errorf("session state off: no state_unavailable note: %s", off)
	}
}

// TestTheFleetChatOpensWithADraftAndSendsNothing: the card's "Ask the chief
// about this" opens a side chat named Fleet — not the primary — and answers
// with the panel body on it carrying the draft about that session; a second
// tap opens the same chat; no message is stored by either; a session not in
// the owner's fleet drafts about the fleet as a whole.
func TestTheFleetChatOpensWithADraftAndSendsNothing(t *testing.T) {
	f := newFleetFixture(t)
	k, w := f.window(f.owner, mirror.StatusIdle)
	f.stop(t, f.owner, k, w, sessionstate.StateStalled, false, fleetT0)
	post := func(about string) string {
		t.Helper()
		form := url.Values{}
		if about != "" {
			form.Set("about", about)
		}
		req := httptest.NewRequest(http.MethodPost, ui.FleetChatPath, strings.NewReader(form.Encode())).WithContext(f.asOwner)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		f.srv.handleFleetChat(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("fleet chat: %d %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	body := post("mirror:" + k)
	wantDraft := `data-chief-prefill="About mirror:` + k
	if !strings.Contains(body, wantDraft) || !strings.Contains(body, `, stalled: "`) {
		t.Fatalf("the draft: want %s…, stalled: in:\n%s", wantDraft, body)
	}
	chat, err := f.srv.ext.Fleet.FleetChat(f.asOwner)
	if err != nil || chat == 0 {
		t.Fatalf("no Fleet chat recorded: %d %v", chat, err)
	}
	if !strings.Contains(body, `data-chief-active-session="`+strconv.FormatInt(chat, 10)+`"`) {
		t.Errorf("the panel body is not on the Fleet chat %d", chat)
	}
	cs, err := f.srv.ext.Agents.GetSession(f.asOwner, chat)
	if err != nil || cs.Title != "Fleet" || cs.IsDefault || cs.AgentID != f.chief.ID {
		t.Errorf("the Fleet chat = %+v, %v; want the chief's non-primary chat titled Fleet", cs, err)
	}
	if again := post(""); !strings.Contains(again, `data-chief-active-session="`+strconv.FormatInt(chat, 10)+`"`) ||
		!strings.Contains(again, `data-chief-prefill="About my fleet: "`) {
		t.Errorf("the second tap: not the same chat, or not the fleet's draft:\n%s", again)
	}
	if other := post("mirror:5e55a1d0-0000-4000-8000-0000ffffffff"); !strings.Contains(other, `data-chief-prefill="About my fleet: "`) {
		t.Errorf("a session not in the fleet: the draft names it")
	}
	msgs, err := f.srv.ext.Agents.ListRecentChatMessages(f.asOwner, chat, 10)
	if err != nil || len(msgs) != 0 {
		t.Errorf("SENT: the Fleet chat holds %d messages (%v); want none", len(msgs), err)
	}
}

// TestTheDigestIsOffUnlessTheFlagIsSet: with MUSTER_FLEET_DIGEST unset the
// build is the one before the digest — the shell draws no card slot, the three
// routes answer 404 (and ?look=1 records nothing), fleet_list's rows carry
// none of the fleet fields and its description names none. The same fleet,
// flag on, carries them (the positive control: their absence is the flag).
func TestTheDigestIsOffUnlessTheFlagIsSet(t *testing.T) {
	f := newFleetFixture(t)
	k, w := f.window(f.owner, mirror.StatusIdle)
	f.stop(t, f.owner, k, w, sessionstate.StateStalled, false, fleetT0)
	f.pr(t, f.owner, k, "example-org/widget", 301, &githubapp.PRState{State: "open"})
	fleetKeys := []string{`"state"`, `"episode"`, `"prs"`, `"last_owner_message_at"`, `"chief_texts_since_owner"`, `"state_unavailable"`, `"owner_facts_unread"`}

	on := f.srv.toolFleetList(f.asOwner, `{}`)
	if !strings.Contains(on, `"state":"stalled"`) || !strings.Contains(on, `"prs":[`) {
		t.Fatalf("control: flag on, fleet_list lacks the fleet fields:\n%s", on)
	}

	f.srv.ext.FleetDigest = false
	off := f.srv.toolFleetList(f.asOwner, `{}`)
	for _, k := range fleetKeys {
		if strings.Contains(off, k+":") {
			t.Errorf("flag off: fleet_list carries %s:\n%s", k, off)
		}
	}
	for _, d := range focusToolDefs(false) {
		if d.Name == fleetListToolName && strings.Contains(d.Description, "fleet's view") {
			t.Errorf("flag off: fleet_list's description names the fleet fields")
		}
	}
	if f.srv.shellFeatures(f.asOwner).FleetDigest {
		t.Errorf("flag off: the shell would draw the digest slot")
	}
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, ui.FleetDigestPath+"?look=1", nil),
		httptest.NewRequest(http.MethodGet, ui.FleetDrillPath+"?id=mirror%3A"+k, nil),
		httptest.NewRequest(http.MethodPost, ui.FleetChatPath, strings.NewReader("about=mirror%3A"+k)),
	} {
		rec := httptest.NewRecorder()
		h := map[string]http.HandlerFunc{ui.FleetDigestPath: f.srv.handleFleetDigest, ui.FleetDrillPath: f.srv.handleFleetDrill, ui.FleetChatPath: f.srv.handleFleetChat}[req.URL.Path]
		h(rec, req.WithContext(f.asOwner))
		if rec.Code != http.StatusNotFound {
			t.Errorf("flag off: %s %s answered %d; want 404", req.Method, req.URL.Path, rec.Code)
		}
	}
	if b, err := f.srv.ext.Fleet.Baseline(f.asOwner); err != nil || !b.IsZero() {
		t.Errorf("flag off: ?look=1 moved the marks: %v %v", b, err)
	}
	var looked int
	if err := f.pool.QueryRow(f.asOwner, `SELECT count(*) FROM fleet_owner_marks`).Scan(&looked); err != nil || looked != 0 {
		t.Errorf("flag off: ?look=1 wrote %d marks rows (%v); want 0", looked, err)
	}
}
