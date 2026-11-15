package api

import (
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// THE TEAM-SHARING PHASE 4 ACCEPTANCE RUN's team-chief bugs (a live test team
// and its chief-<id>), each pinned against the team-agent walk's REAL store.

// page is GET path as uid, following the redirects a bare agent URL answers
// (to its session's URL), as a browser does.
func (w *isoWorld) page(path string, uid int64) *httptest.ResponseRecorder {
	rec := w.do(w.h, "GET", path, "", cred{sessionUser: uid})
	for i := 0; i < 3 && rec.Code == http.StatusSeeOther; i++ {
		rec = w.do(w.h, "GET", rec.Header().Get("Location"), "", cred{sessionUser: uid})
	}
	return rec
}

// TestATeamsChiefIsKickedOffAsTheTeamsNotAsTheFleets: a manager's provision of
// a team's chief hands the new chief a first turn that says it is the TEAM's
// chief and speaks only of the team's work — never "muster's fleet agent …
// report the current state of the fleet", a scope it does not have. The text
// is pinned WHOLE (it is prose the chief acts on): a reword is a deliberate
// change to this test.
//
// 🔴 RED at origin/main 797e8bb5: the team chief's kickoff was the personal
// one (the live acceptance run's first turn).
func TestATeamsChiefIsKickedOffAsTheTeamsNotAsTheFleets(t *testing.T) {
	tw := newTeamAgentWalk(t)
	w := tw.w
	team := tw.newChieflessTeam(t)
	plane := poolPlane(t, w)
	plane.TeamsOnServerPool = map[int64]bool{team: true}
	t.Cleanup(func() { plane.TeamsOnServerPool = nil })
	if rec := w.do(w.h, "POST", "/teams/"+strconv.FormatInt(team, 10)+"/chief/provision", "", cred{sessionUser: w.a.uid}); rec.Code != http.StatusSeeOther {
		t.Fatalf("A (manager) provisioning: %d %q", rec.Code, truncate(rec.Body.String(), 300))
	}
	chief, has := tw.teamChiefRow(t, team)
	if !has {
		t.Fatal("A's provision created no chief-<team>")
	}
	const want = "You are your team's chief in muster. You have no task and no repo, and that is expected. " +
		"Your standing instructions are already loaded (the managed CLAUDE.md): they say which team you belong to and " +
		"that several of its members talk to you here. Introduce yourself to the team in a few lines, and say what you " +
		"can do for it with your muster tools (mcp__muster__…). Speak only of the team's own tasks and agents: you act " +
		"as the team and see nothing that is not the team's."
	if chief.NoteText != want {
		t.Errorf("the team chief's kickoff:\n  %q\nwant\n  %q", chief.NoteText, want)
	}
	for _, personal := range []string{"fleet", "AGENTS.md"} {
		if strings.Contains(chief.NoteText, personal) {
			t.Errorf("the team chief's kickoff says %q: a person's chief's scope, not a team's", personal)
		}
	}
}

// TestAPersonsChiefKeepsItsKickoff is the other arm, pinned whole: a person's
// chief — provisioned through the real POST /chief/provision — is still told
// it is muster's fleet agent. An invariant guard (green
// before the fix too): it fails a fix that hands everyone the team's text.
func TestAPersonsChiefKeepsItsKickoff(t *testing.T) {
	const want = "You are chief, muster's fleet agent. You have no task and no repo, and that is expected. " +
		"Read AGENTS.md, then report the current state of the fleet."
	// A's chief, provisioned by A's own session (POST /chief/provision, in
	// the iso world's seed).
	w := newIsoWorld(t)
	if w.aChief.NoteText != want {
		t.Errorf("A's own chief's kickoff (POST /chief/provision) = %q; want %q", w.aChief.NoteText, want)
	}
}

// TestAMembersVisitToATeamAgentRecordsNothingAndLogsNothing: A (a manager)
// opening the team chief's page — its chat page and its Terminal view — writes
// no recents row (a recents row is its viewer's own, and its owner must own the
// agent it names: 0018's trigger) and logs no error. The control: A opening
// A's OWN agent records a visit, so the recents store is wired and writing.
//
// 🔴 RED at origin/main 797e8bb5: every load logged "recents: visit agent N:
// … owner A is not the owner of agent N (SQLSTATE 23514)".
func TestAMembersVisitToATeamAgentRecordsNothingAndLogsNothing(t *testing.T) {
	tw := newTeamAgentWalk(t)
	w := tw.w
	logs := &lockedLog{}
	w.srv.logger = log.New(logs, "", 0)
	recentsOf := func(agentID int64) int {
		t.Helper()
		var n int
		if err := w.pool.QueryRow(w.sys, `SELECT count(*) FROM recents WHERE agent_id = $1`, agentID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, path := range []string{"/agents/" + tw.chief.Name, "/agents/" + tw.chief.Name + "/terminal"} {
		rec := w.page(path, w.a.uid)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tw.canary) && !strings.HasSuffix(path, "/terminal") {
			t.Fatalf("fixture: A's GET %s: %d (canary on the page: %v)", path, rec.Code, strings.Contains(rec.Body.String(), tw.canary))
		}
	}
	if n := recentsOf(tw.chief.ID); n != 0 {
		t.Errorf("%d recents row(s) name the team chief after a member's visit; a member's strip holds only their own agents", n)
	}
	if got := logs.String(); strings.Contains(got, "recents") || strings.Contains(got, "SQLSTATE") {
		t.Errorf("a member's visit to a team agent logged an error:\n%s", got)
	}
	// The control: A's own agent's page records A's visit.
	if rec := w.page("/agents/"+w.a.agent.Name, w.a.uid); rec.Code != http.StatusOK {
		t.Fatalf("control: A's own agent page: %d", rec.Code)
	}
	if n := recentsOf(w.a.agent.ID); n != 1 {
		t.Errorf("control: A's own agent has %d recents rows after A's visit; want 1 (the recents store is not recording)", n)
	}
}

// TestATeamChiefsPageOffersAMemberNoChatTheServerRefuses: the team chief has
// ONE shared conversation, and the server refuses every member New side
// chat, Fresh primary, Make primary, Archive and Unarchive on it
// (teamAgentRoutes). The page a manager, an operator and a viewer are drawn
// carries none of those controls — and still draws the chief's Chats list
// (the control: the panel the buttons lived in is on the page). The owner's
// own chief's page still offers them (the second control).
//
// 🔴 RED at origin/main 797e8bb5: the team chief's page showed "New side chat"
// and "Fresh primary", and the server answered both 403.
func TestATeamChiefsPageOffersAMemberNoChatTheServerRefuses(t *testing.T) {
	tw := newTeamAgentWalk(t)
	w := tw.w
	refused := []string{chiefSideChatAttrHTML, chiefFreshPrimaryAttrHTML, `data-chief-make-primary=""`, `data-chief-archive=""`,
		`data-chief-unarchive=""`, `data-chief-chat-menu=""`}
	// A side chat on the team chief, so a row ⋯ menu WOULD be drawn.
	if _, err := agents.NewPG(w.pool).CreateSession(tw.teamCtx, tw.chief.ID, tw.chief.Name); err != nil {
		t.Fatal(err)
	}
	for _, who := range []isoUser{w.a, w.b, tw.v} {
		rec := w.page("/agents/"+tw.chief.Name, who.uid)
		body := rec.Body.String()
		if rec.Code != http.StatusOK || !strings.Contains(body, `data-chief-chats`) {
			t.Fatalf("control: %s's team chief page: %d, Chats list drawn %v", who.name, rec.Code, strings.Contains(body, `data-chief-chats`))
		}
		for _, attr := range refused {
			if strings.Contains(body, attr) {
				t.Errorf("%s's team chief page offers %s, which the server refuses every member", who.name, attr)
			}
		}
	}
	// The second control: A's OWN chief (provisioned by the iso world) offers them.
	body := w.page("/agents/"+w.aChief.Name, w.a.uid).Body.String()
	for _, attr := range []string{chiefSideChatAttrHTML, chiefFreshPrimaryAttrHTML} {
		if !strings.Contains(body, attr) {
			t.Errorf("control: A's own chief's page does not offer %s", attr)
		}
	}
}

// The attributes the page's side-chat and fresh-primary buttons carry,
// spelled here so the assertion does not read them from the renderer.
const (
	chiefSideChatAttrHTML     = `data-chief-side-chat=""`
	chiefFreshPrimaryAttrHTML = `data-chief-fresh-primary=""`
)

// TestAdminTeamsSaysWhatATeamShares: /admin/teams no longer says "Nothing is
// shared yet … its members see nothing of it" — phases 2 to 4 share tasks, a
// chief, agents, officers and hosts' sessions — and each team's line says
// whether its agents run on the server pool and use the GitHub App. The
// sentence is pinned WHOLE.
//
// 🔴 RED at origin/main 797e8bb5: the page carried phase 1's copy and no
// per-team line.
func TestAdminTeamsSaysWhatATeamShares(t *testing.T) {
	tw := newTeamAgentWalk(t)
	w := tw.w
	other := tw.newChieflessTeam(t)
	plane := poolPlane(t, w)
	plane.TeamsOnServerPool = map[int64]bool{tw.team: true}
	t.Cleanup(func() { plane.TeamsOnServerPool = nil })
	w.srv.ext.GitHubApp = &listingApp{repos: []string{"example/app"}}
	w.srv.ext.GitHubAppTeams = map[int64]bool{tw.team: true}
	t.Cleanup(func() { w.srv.ext.GitHubApp, w.srv.ext.GitHubAppTeams = nil, nil })

	rec := w.do(w.h, "GET", "/admin/teams", "", cred{sessionUser: w.op.uid})
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("the admin's /admin/teams: %d", rec.Code)
	}
	for _, stale := range []string{"Nothing is shared yet", "see nothing of it", "in this version"} {
		if strings.Contains(body, stale) {
			t.Errorf("/admin/teams still says %q", stale)
		}
	}
	const shares = "A team owns shared work, and its members see it by role: viewers read, operators also file, " +
		"comment and chat, managers also run the team&#39;s chief and agents. A team has its own tasks, a team chief " +
		"(set up by a manager on the team&#39;s Chief page), the agents its chief starts, its officers, and its hosts&#39; " +
		"sessions. Its agents run on the server&#39;s Claude pool only when the team is listed in MUSTER_TEAMS_ON_SERVER_POOL, " +
		"and work in your repositories only when it is listed in MUSTER_TEAMS_ON_GITHUB_APP; each team&#39;s line below says which."
	if !strings.Contains(body, shares) && !strings.Contains(body, strings.ReplaceAll(shares, "&#39;", "'")) {
		t.Errorf("/admin/teams does not say what a team shares, verbatim")
	}
	line := func(team int64) string {
		_, after, ok := strings.Cut(body, `data-team-id="`+strconv.FormatInt(team, 10)+`"`)
		if !ok {
			t.Fatalf("/admin/teams lists no team %d", team)
		}
		_, after, _ = strings.Cut(after, `data-team-credentials>`)
		got, _, _ := strings.Cut(after, `</p>`)
		return got
	}
	if got, want := line(tw.team), "Claude: on the server pool. GitHub App: listed; a manager approves each repository."; got != want {
		t.Errorf("the listed team's line = %q; want %q", got, want)
	}
	if got, want := line(other), "Claude: not on the server pool, so its agents cannot run. GitHub App: not listed, so its agents work in no repository."; got != want {
		t.Errorf("the unlisted team's line = %q; want %q", got, want)
	}
}

// TestAdminTeamsSaysAListedTeamHasNoPoolToRunOn (round 0, item 5): on a server
// with no Claude pool configured, a team LISTED on the server pool is not "not
// on the server pool" — its line says it is listed and there is no pool. An
// unlisted team's line is unchanged (the control: the two lines differ, so the
// listing is what the line reads).
//
// 🔴 RED at 0740e0cb: the listed team's line read "Claude: not on the server
// pool, so its agents cannot run."
func TestAdminTeamsSaysAListedTeamHasNoPoolToRunOn(t *testing.T) {
	tw := newTeamAgentWalk(t)
	w := tw.w
	other := tw.newChieflessTeam(t)
	plane := poolPlane(t, w)
	env := plane.EnvClaude
	plane.EnvClaude = nil
	plane.TeamsOnServerPool = map[int64]bool{tw.team: true}
	t.Cleanup(func() { plane.EnvClaude, plane.TeamsOnServerPool = env, nil })

	rec := w.do(w.h, "GET", "/admin/teams", "", cred{sessionUser: w.op.uid})
	if rec.Code != http.StatusOK {
		t.Fatalf("the admin's /admin/teams: %d", rec.Code)
	}
	body := rec.Body.String()
	line := func(team int64) string {
		_, after, ok := strings.Cut(body, `data-team-id="`+strconv.FormatInt(team, 10)+`"`)
		if !ok {
			t.Fatalf("/admin/teams lists no team %d", team)
		}
		_, after, _ = strings.Cut(after, `data-team-credentials>`)
		got, _, _ := strings.Cut(after, `</p>`)
		return got
	}
	if got, want := line(tw.team), "Claude: listed on the server pool, but this server has no Claude pool configured, so its agents cannot run. GitHub App: none is configured."; got != want {
		t.Errorf("the listed team's line = %q; want %q", got, want)
	}
	if got, want := line(other), "Claude: not on the server pool, so its agents cannot run. GitHub App: none is configured."; got != want {
		t.Errorf("the unlisted team's line = %q; want %q", got, want)
	}
}
