package api

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/officers"
)

// Team sharing 2b.5 (team officers on team hosts), against a real Postgres.

// TestTheSpawnsPageGroupsATeamsOwnSpawnsUnderTheTeam: a team host officer's
// spawn row is owned by the TEAM. /officers/spawns shows it to every member
// (a viewer included; row-level security's team_owner_read, 0133) in a
// section of its own, under the team's name, with no share or unshare form —
// it is nobody's to lend. A non-member does not see it.
// CONTROLS: the same viewer's page still lists the person-owned spawn the
// owner LENT to the team in the personal list, "by a teammate"; and the
// manager's own spawn keeps its share form.
//
// 🔴 RED at the base (c96acc08): the row was drawn in the personal list as
// "Shared with  by a teammate (read-only)" — a team-owned row read as a lend
// to no team.
func TestTheSpawnsPageGroupsATeamsOwnSpawnsUnderTheTeam(t *testing.T) {
	w := newIsoWorld(t)
	team := w.aTeam
	w.setRole(t, team, w.b.uid, authz.TeamViewer)
	instance := "tmo-" + w.suffix
	var teamSpawn int64
	if err := w.pool.QueryRow(w.sys, `INSERT INTO officer_host_spawns (owner_id, officer, instance, host)
		VALUES ($1, 'tmo:x', $2, 'build-01') RETURNING id`, team, instance).Scan(&teamSpawn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = w.pool.Exec(authz.System(context.Background()), `DELETE FROM officer_host_spawns WHERE id = $1`, teamSpawn)
	})
	// The manager lends their own spawn to the team (the CONTROL row).
	lent := w.objA.spawn
	if rec := w.do(w.h, "POST", officerSpawnsPath+"/"+strconv.FormatInt(lent, 10)+"/share", "team="+strconv.FormatInt(team, 10), cred{sessionUser: w.a.uid}); rec.Code != http.StatusSeeOther {
		t.Fatalf("fixture: the manager lending their spawn: %d %.300s", rec.Code, rec.Body.String())
	}
	page := func(uid int64) string {
		t.Helper()
		rec := w.do(w.h, "GET", officerSpawnsPath, "", cred{sessionUser: uid})
		if rec.Code != http.StatusOK {
			t.Fatalf("user %d's spawns page: %d", uid, rec.Code)
		}
		return rec.Body.String()
	}
	tid, sid := strconv.FormatInt(team, 10), strconv.FormatInt(teamSpawn, 10)
	section := regexp.MustCompile(`(?s)<section[^>]*id="officer-spawns-team-` + tid + `"[^>]*>.*?</section>`)
	personal := regexp.MustCompile(`(?s)<ul[^>]*id="officer-spawns">.*?</ul>`)
	for _, who := range []struct {
		name string
		uid  int64
	}{{"the team's viewer", w.b.uid}, {"the team's manager", w.a.uid}} {
		body := page(who.uid)
		sec := section.FindString(body)
		if sec == "" || !strings.Contains(sec, instance) || !strings.Contains(sec, `data-spawn-id="`+sid+`" data-spawn-owner-team="`+tid+`"`) {
			t.Fatalf("%s: no section for the team holding its spawn %s\n%.2000s", who.name, instance, body)
		}
		if !strings.Contains(sec, "'s officers") {
			t.Errorf("%s: the team's section is not headed as the team's officers: %.500s", who.name, sec)
		}
		if strings.Contains(personal.FindString(body), instance) {
			t.Errorf("%s: the team's own spawn is in the personal list", who.name)
		}
		if strings.Contains(body, officerSpawnsPath+"/"+sid+"/share") || strings.Contains(body, officerSpawnsPath+"/"+sid+"/unshare") {
			t.Errorf("%s: the team's own spawn is drawn with a lend form", who.name)
		}
	}
	// CONTROLS: the lent row stays a lend, in the personal list.
	if got := personal.FindString(page(w.b.uid)); !strings.Contains(got, w.canary) || !strings.Contains(got, "by a teammate") {
		t.Errorf("CONTROL: the viewer's personal list does not show the manager's lent spawn as a lend: %.800s", got)
	}
	if got := page(w.a.uid); !strings.Contains(got, officerSpawnsPath+"/"+strconv.FormatInt(lent, 10)+"/unshare") {
		t.Errorf("CONTROL: the manager's own lent spawn has no Stop sharing form")
	}
	// A non-member sees none of the team's.
	if body := page(w.op.uid); strings.Contains(body, instance) || strings.Contains(body, `id="officer-spawns-team-`+tid+`"`) {
		t.Errorf("a non-member sees the team's spawn")
	}
}

// enableHostOfficers turns the officer runtime on in srv with the given host
// officers (HQ-backed fixtures: the gate and the binding read the roster only).
func enableHostOfficers(t *testing.T, srv *Server, list ...officers.Officer) {
	t.Helper()
	if srv.ext.Agents == nil {
		t.Fatal("fixture: no agents store")
	}
	srv.officerRT = nil
	if _, err := srv.EnableOfficers(OfficersConfig{Officers: list, HostSpawns: officers.NewMemHostSpawnStore()}); err != nil {
		t.Fatal(err)
	}
}

// fixtureHostOfficer is officer id of owner on host.
func fixtureHostOfficer(id string, owner int64, host string) officers.Officer {
	return officers.Officer{ID: id, HQURL: "https://hq.example.invalid/api/v1", MaxInstances: 1, IdleAfter: time.Hour,
		PollInterval: time.Minute, Owner: owner, Runtime: officers.RuntimeHost, Host: host, Repo: hostOffRepo}.WithToken("fixture-" + strings.ReplaceAll(id, ":", "-"))
}

// TestATeamHostFilesNoStepUpCard (team sharing 2b.5, the gate ordering): on a
// team host running a TEAM host officer — so the step-up gate WOULD hold the
// machine (hostOfficersOnMachine names the officer: checked) —
// plugin_draft_decide and attention_answer from a session the team host lists
// answer the teamHostTools refusal, and no Attention row is filed: a team has
// no phone, so teamHostTools runs before hostStepUpGate.
// POSITIVE CONTROL: the same calls on the same host name from the PERSON's
// host, which runs the person's host officer, each file a step-up card.
//
// 🔴 RED at the base (c96acc08): EnableOfficers refused the team's host
// officer (ErrTeamHostOfficer), so no team machine ran a host officer.
func TestATeamHostFilesNoStepUpCard(t *testing.T) {
	f, team, teamTok, personTok := newTeamHostFixture(t)
	enableHostOfficers(t, f.srv, fixtureHostOfficer("ops:team", team, "ws"), fixtureHostOfficer("ops:mine", 1, "ws"))
	asTeam := authz.WithPrincipal(context.Background(), teamActingPrincipal(team, authz.ViaMirrorHost))
	if ids := f.srv.hostOfficersOnMachine(asTeam, team, "ws"); len(ids) != 1 || ids[0] != "ops:team" {
		t.Fatalf("reachability: the team's machine runs host officers %v; want [ops:team] (else the gate would never hold it)", ids)
	}
	asks := func() int {
		var n int
		if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM attention_asks`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	calls := []struct {
		tool string
		args map[string]any
	}{
		{pluginDraftDecideTool, map[string]any{"id": 1, "decision": "approve"}},
		{attentionAnswerTool, map[string]any{"id": 1, "answer": "yes"}},
	}
	before := asks()
	for _, c := range calls {
		_, r, body := f.mirrorRPC(t, mirrorAs(teamTok, focusSID), mcpToolCall(c.tool, c.args))
		if r == nil || !strings.Contains(string(r.Result), `"isError":true`) || !strings.Contains(body, teamHostToolRefusal(c.tool)) {
			t.Errorf("%s from the team host running a team officer: %s; want the teamHostTools refusal", c.tool, body)
		}
	}
	if after := asks(); after != before {
		t.Fatalf("the team host's refused calls filed %d Attention row(s); want none", after-before)
	}
	// POSITIVE CONTROL: the person's host of the same name, running the
	// person's host officer, gets a card for each.
	for _, c := range calls {
		_, r, body := f.mirrorRPC(t, mirrorAs(personTok, focusSID), mcpToolCall(c.tool, c.args))
		text, isErr := toolText(t, r)
		if r == nil || isErr {
			t.Fatalf("CONTROL: %s from the person's host: %s; want a step-up card", c.tool, body)
		}
		card := stepUpCardOf(t, text)
		var owner int64
		if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT owner_id FROM attention_asks WHERE id = $1`, card).Scan(&owner); err != nil || owner != 1 {
			t.Fatalf("CONTROL: card %d is owned by %d (%v); want the person", card, owner, err)
		}
	}
	if got := asks(); got != before+len(calls) {
		t.Fatalf("CONTROL: %d card(s) filed for the person; want %d", got-before, len(calls))
	}
}

// TestATeamHostOfficerInstanceBindsOnlyOnTheTeamHost (team sharing 2b.5): a
// team host officer's instance — its spawn row and agent row the TEAM's,
// written as the team acting principal, the row's window the one the team
// host lists running the session — is bound on the team host's MCP door: an
// officer tool from that session is served as the instance, not refused as
// "not an instance". CONTROL: the same session id, from the member's PERSONAL
// host of the same name, is not bound (the mirror door carries no teams, so
// team_owner_read never reaches it), and the officer tool is refused there.
//
// 🔴 RED at the base (c96acc08): EnableOfficers refused the team's host
// officer, so no team instance ever bound.
func TestATeamHostOfficerInstanceBindsOnlyOnTheTeamHost(t *testing.T) {
	f, team, teamTok, personTok := newTeamHostFixture(t)
	o := fixtureHostOfficer("ops:team", team, "ws")
	st := officers.NewPGHostSpawnStore(f.pool)
	f.srv.officerRT = nil
	if _, err := f.srv.EnableOfficers(OfficersConfig{Officers: []officers.Officer{o}, HostSpawns: st}); err != nil {
		t.Fatalf("a team's host officer at boot: %v", err)
	}
	asTeam := authz.WithPrincipal(context.Background(), teamActingPrincipal(team, authz.ViaOfficer))
	name := o.InstanceName(1)
	a, err := agents.NewPG(f.pool).Create(asTeam, agents.Agent{Name: name, DisplayName: "team instance", DisplayNameSource: agents.NameSystem,
		Status: agents.StatusStopped, Kind: agents.KindGateway, RunsOnHost: "ws", OwnerID: team})
	if err != nil {
		t.Fatalf("the team instance's agent row: %v", err)
	}
	h, err := st.InsertHostSpawn(asTeam, o.ID, name, "ws")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.RecordHostSpawnWindow(asTeam, h.ID, "ws", "@1", focusSID); err != nil || !ok {
		t.Fatalf("record the team spawn's window: %v %v", ok, err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM officer_host_spawns WHERE id = $1`, h.ID)
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, a.ID)
	})
	notInstance := strings.SplitN(officerHostToolRefusal, ":", 2)[0]
	notInstance = strings.Replace(notInstance, "%s", officers.ToolBrief, 1)
	_, r, body := f.mirrorRPC(t, mirrorAs(teamTok, focusSID), mcpToolCall(officers.ToolBrief, map[string]any{}))
	if r == nil || strings.Contains(body, notInstance) || strings.Contains(body, teamHostToolRefusal(officers.ToolBrief)) {
		t.Fatalf("the team instance's own officer tool from the team host: %s; want it served as the instance", body)
	}
	_, r, body = f.mirrorRPC(t, mirrorAs(personTok, focusSID), mcpToolCall(officers.ToolBrief, map[string]any{}))
	if r == nil || !strings.Contains(body, notInstance) {
		t.Fatalf("CONTROL: the same session id from the member's personal host: %s; want the not-an-instance refusal", body)
	}
}
