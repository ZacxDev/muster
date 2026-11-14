package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/users"
)

// Team sharing phase 3 (team officers), through the real server against a
// real Postgres.

// personAt is uid's principal through via, with the memberships
// users.Account reads — as every request door builds it.
func (w *isoWorld) personAt(t *testing.T, uid int64, via authz.Via) context.Context {
	t.Helper()
	a, err := users.NewPG(w.pool).Account(w.sys, uid)
	if err != nil {
		t.Fatal(err)
	}
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: a.Role, Via: via, Teams: a.Teams})
}

func (w *isoWorld) setRole(t *testing.T, team, user int64, role authz.TeamRole) {
	t.Helper()
	if err := users.NewPG(w.pool).SetTeamMember(w.sys, users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}, team, user, role); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = w.pool.Exec(authz.System(context.Background()), `DELETE FROM team_members WHERE team_id = $1 AND user_id = $2`, team, user)
	})
}

func (w *isoWorld) spawnTeam(t *testing.T, id int64) int64 {
	t.Helper()
	var team *int64
	if err := w.pool.QueryRow(w.sys, `SELECT shared_team_id FROM officer_host_spawns WHERE id = $1`, id).Scan(&team); err != nil {
		t.Fatal(err)
	}
	if team == nil {
		return 0
	}
	return *team
}

// TestAnOperatorCannotEditATeamCharter (phase 3 acceptance): a team's
// officers page shows every member the team's charter, and only a MANAGER
// changes it — an operator's write is refused by the handler (403, the
// manager-only text) AND by Postgres (officer_types is managed: the same store
// call under the operator's own principal is refused). A non-member gets the
// 404 a missing team gets. CONTROLS: the manager's write lands, owned by the
// team, and the operator then reads the new charter.
func TestAnOperatorCannotEditATeamCharter(t *testing.T) {
	w := newIsoWorld(t)
	team := w.aTeam
	w.setRole(t, team, w.b.uid, authz.TeamOperator)
	ost := builtin.NewStore(w.pool)
	t.Cleanup(func() {
		_, _ = w.pool.Exec(authz.System(context.Background()), `DELETE FROM officer_types WHERE owner_id = $1`, team)
	})
	page := "/teams/" + strconv.FormatInt(team, 10) + "/officers"
	form := url.Values{"name": {"Marketing"}, "charter": {"team charter " + w.suffix}}.Encode()

	// The manager defines it (CONTROL).
	if rec := w.do(w.h, "POST", page+"/types/mk", form, cred{sessionUser: w.a.uid}); rec.Code != http.StatusSeeOther {
		t.Fatalf("control: the manager defining the team's type: %d %.300s", rec.Code, rec.Body.String())
	}
	got, err := ost.GetType(w.sys, team, "mk")
	if err != nil || got.Charter != "team charter "+w.suffix {
		t.Fatalf("control: the team's type after the manager's write: %+v, %v", got, err)
	}
	// The operator reads it…
	if rec := w.do(w.h, "GET", page, "", cred{sessionUser: w.b.uid}); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "team charter "+w.suffix) {
		t.Fatalf("the team's operator reading its officers: %d, charter shown %v", rec.Code, strings.Contains(rec.Body.String(), "team charter "+w.suffix))
	}
	// …and cannot change it: the handler refuses,
	forged := url.Values{"name": {"Marketing"}, "charter": {"operator charter"}}.Encode()
	rec := w.do(w.h, "POST", page+"/types/mk", forged, cred{sessionUser: w.b.uid})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), TeamOfficerManagerOnly) {
		t.Errorf("the operator's charter write: %d; want 403 %q\n%.300s", rec.Code, TeamOfficerManagerOnly, rec.Body.String())
	}
	if rec := w.do(w.h, "POST", page+"/defs/mk:a", url.Values{"mode": {"live"}}.Encode(), cred{sessionUser: w.b.uid}); rec.Code != http.StatusForbidden {
		t.Errorf("the operator's officer write: %d; want 403", rec.Code)
	}
	// …and so does Postgres, for the same store call under their principal.
	if _, err := ost.PutTypeReturning(w.personAt(t, w.b.uid, authz.ViaSession), team, builtin.Type{Key: "mk", Name: "Marketing", Charter: "operator charter"}); err == nil {
		t.Error("the operator's own store write of the team's charter succeeded; want Postgres to refuse it (managed)")
	}
	if got, _ := ost.GetType(w.sys, team, "mk"); got.Charter != "team charter "+w.suffix {
		t.Errorf("after the operator's writes the charter is %q", got.Charter)
	}
	// A non-member: the 404 a missing team gets.
	outsider := w.do(w.h, "GET", page, "", cred{sessionUser: w.op.uid})
	ghost := w.do(w.h, "GET", "/teams/9000000000/officers", "", cred{sessionUser: w.op.uid})
	if outsider.Code != http.StatusNotFound || ghost.Code != outsider.Code || strings.Contains(outsider.Body.String(), "team charter") {
		t.Errorf("a non-member's page: %d (a missing team %d), charter shown %v", outsider.Code, ghost.Code, strings.Contains(outsider.Body.String(), "team charter"))
	}
}

// TestTheSpawnsPageLendsToATeamAndTakesItBack: the owner shares their host
// spawn with a team they manage from /officers/spawns; a viewer of the team
// then sees it there (its host), and stops seeing it once the owner stops
// sharing. A member of the team cannot unshare or re-lend the owner's spawn.
// CONTROL: before the share the viewer sees nothing of it, and the owner sees
// it throughout.
func TestTheSpawnsPageLendsToATeamAndTakesItBack(t *testing.T) {
	w := newIsoWorld(t)
	spawn := w.objA.spawn
	w.setRole(t, w.aTeam, w.b.uid, authz.TeamViewer)
	host := w.canary
	sees := func(uid int64) bool {
		rec := w.do(w.h, "GET", officerSpawnsPath, "", cred{sessionUser: uid})
		if rec.Code != http.StatusOK {
			t.Fatalf("user %d's spawns page: %d", uid, rec.Code)
		}
		return strings.Contains(rec.Body.String(), host)
	}
	if !sees(w.a.uid) || sees(w.b.uid) {
		t.Fatalf("before the share: owner sees %v, viewer sees %v; want true, false", sees(w.a.uid), sees(w.b.uid))
	}
	path := officerSpawnsPath + "/" + strconv.FormatInt(spawn, 10)
	share := url.Values{"team": {strconv.FormatInt(w.aTeam, 10)}}.Encode()
	if rec := w.do(w.h, "POST", path+"/share", share, cred{sessionUser: w.a.uid}); rec.Code != http.StatusSeeOther {
		t.Fatalf("the owner sharing: %d %.300s", rec.Code, rec.Body.String())
	}
	if w.spawnTeam(t, spawn) != w.aTeam || !sees(w.b.uid) {
		t.Fatalf("after the share: lent to %d, viewer sees %v; want %d, true", w.spawnTeam(t, spawn), sees(w.b.uid), w.aTeam)
	}
	// The viewer cannot take it back (not theirs), nor re-lend it.
	if rec := w.do(w.h, "POST", path+"/unshare", "", cred{sessionUser: w.b.uid}); rec.Code != http.StatusNotFound {
		t.Errorf("the viewer unsharing the owner's spawn: %d; want 404", rec.Code)
	}
	if w.spawnTeam(t, spawn) != w.aTeam {
		t.Fatalf("the viewer's unshare changed the lend")
	}
	if rec := w.do(w.h, "POST", path+"/unshare", "", cred{sessionUser: w.a.uid}); rec.Code != http.StatusSeeOther {
		t.Fatalf("the owner unsharing: %d", rec.Code)
	}
	if w.spawnTeam(t, spawn) != 0 || sees(w.b.uid) || !sees(w.a.uid) {
		t.Errorf("after the unshare: lent to %d, viewer sees %v, owner sees %v", w.spawnTeam(t, spawn), sees(w.b.uid), sees(w.a.uid))
	}
}

// TestTheChiefLendsASpawnAndAPersonalTokenCannot: the lend gate against the
// real store. The owner's CHIEF door (ViaAgent) lends and takes back through
// officer_spawn_share; the SAME person's PERSONAL API TOKEN (ViaToken) is
// refused with ErrLendDoor and the row is unchanged; a viewer's lend gets
// ErrLendRole; another person's lend of the owner's row is "no such spawn".
func TestTheChiefLendsASpawnAndAPersonalTokenCannot(t *testing.T) {
	w := newIsoWorld(t)
	spawn := w.objA.spawn
	viewerTeam, err := users.NewPG(w.pool).CreateTeam(w.sys, users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}, "viewer-team-"+w.suffix, w.b.uid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		_, _ = w.pool.Exec(bg, `DELETE FROM team_members WHERE team_id = $1`, viewerTeam.ID)
		_, _ = w.pool.Exec(bg, `DELETE FROM audit_log WHERE team_id = $1 OR object = $2`, viewerTeam.ID, users.TeamObject(viewerTeam.ID))
		_, _ = w.pool.Exec(bg, `DELETE FROM users WHERE id = $1`, viewerTeam.ID)
	})
	w.setRole(t, viewerTeam.ID, w.a.uid, authz.TeamViewer)

	token := w.personAt(t, w.a.uid, authz.ViaToken)
	if _, err := w.srv.lendHostSpawn(token, spawn, w.aTeam); !errors.Is(err, ErrLendDoor) {
		t.Errorf("the owner's personal API token lending: %v; want ErrLendDoor", err)
	}
	if out := w.srv.toolOfficerSpawnShare(token, `{"action":"share","id":`+strconv.FormatInt(spawn, 10)+`,"team_id":`+strconv.FormatInt(w.aTeam, 10)+`}`); !strings.Contains(out, ErrLendDoor.Error()) {
		t.Errorf("the tool through a personal token: %s; want ErrLendDoor", out)
	}
	if got := w.spawnTeam(t, spawn); got != 0 {
		t.Fatalf("the personal token's refused lend left the spawn lent to %d", got)
	}
	chief := w.personAt(t, w.a.uid, authz.ViaAgent)
	if _, err := w.srv.lendHostSpawn(chief, spawn, viewerTeam.ID); !errors.Is(err, ErrLendRole) {
		t.Errorf("lending to a team the owner only views: %v; want ErrLendRole", err)
	}
	if _, err := w.srv.lendHostSpawn(w.personAt(t, w.b.uid, authz.ViaSession), spawn, viewerTeam.ID); !errors.Is(err, officers.ErrHostSpawnNotFound) {
		t.Errorf("another person lending the owner's spawn: %v; want ErrHostSpawnNotFound", err)
	}
	out := w.srv.toolOfficerSpawnShare(chief, `{"action":"share","id":`+strconv.FormatInt(spawn, 10)+`,"team_id":`+strconv.FormatInt(w.aTeam, 10)+`}`)
	var got struct {
		ID         int64 `json:"id"`
		SharedTeam int64 `json:"shared_team_id"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.ID != spawn || got.SharedTeam != w.aTeam || w.spawnTeam(t, spawn) != w.aTeam {
		t.Fatalf("the chief sharing: %s (row lent to %d)", out, w.spawnTeam(t, spawn))
	}
	list := w.srv.toolOfficerSpawnShare(chief, `{"action":"list"}`)
	if !strings.Contains(list, `"id":`+strconv.FormatInt(spawn, 10)) {
		t.Errorf("the chief's list: %s; want its owner's spawn", list)
	}
	if out := w.srv.toolOfficerSpawnShare(chief, `{"action":"unshare","id":`+strconv.FormatInt(spawn, 10)+`}`); w.spawnTeam(t, spawn) != 0 {
		t.Errorf("the chief unsharing: %s (row lent to %d)", out, w.spawnTeam(t, spawn))
	}
}

// TestATeamOfficersRuntimeAndAskAreTheTeams (phase 3 acceptance): a team
// officer — its MUSTER_OFFICERS owner the team — runs as the team acting
// principal (officerPrincipal: Postgres holds muster.uid = the team and no
// teams), its builtin store reads the TEAM's type and not a member's, and
// its question to its owner (officer_ask_person "owner") is ONE ask owned by
// the team: a viewer of the team sees it, a non-member does not, an operator
// answers it and the answer records them, and a second answer is refused.
// officerOwnerKind reads each owner as its kind for the boot check.
func TestATeamOfficersRuntimeAndAskAreTheTeams(t *testing.T) {
	w := newIsoWorld(t)
	w.srv.ext.Users = users.NewPG(w.pool)
	team := w.aTeam
	w.setRole(t, team, w.b.uid, authz.TeamOperator)
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		_, _ = w.pool.Exec(bg, `DELETE FROM attention_asks WHERE owner_id = $1`, team)
		_, _ = w.pool.Exec(bg, `DELETE FROM officer_types WHERE owner_id = $1`, team)
	})
	for id, want := range map[int64]officers.OwnerKind{team: officers.OwnerTeam, w.a.uid: officers.OwnerPerson, 9_000_000_000: officers.OwnerMissing} {
		if got, err := w.srv.officerOwnerKind(id); err != nil || got != want {
			t.Errorf("officerOwnerKind(%d) = %v, %v; want %v", id, got, err, want)
		}
	}
	var octx context.Context
	if msg := panicOf(func() { octx = w.srv.officerPrincipal(context.Background(), team) }); msg != "" {
		t.Fatalf("a team officer's principal PANICKED: %s", msg)
	}
	if p, _ := authz.From(octx); p != (authz.Principal{UserID: team, Team: true, Role: authz.RoleMember, Via: authz.ViaOfficer}) {
		t.Fatalf("a team officer acts as %+v; want the team acting principal through the officer door", p)
	}
	ost := builtin.NewStore(w.pool)
	if err := ost.PutType(authz.System(context.Background()), team, builtin.Type{Key: "mk", Name: "Marketing", Charter: "team charter"}); err != nil {
		t.Fatal(err)
	}
	if list, err := ost.ListTypes(octx, team); err != nil || len(list) != 1 || list[0].Key != "mk" {
		t.Errorf("the team officer's store reads %+v, %v; want the team's one type", list, err)
	}
	if list, err := ost.ListTypes(octx, w.a.uid); err != nil || len(list) != 0 {
		t.Errorf("the team officer's store reads the manager's personal types: %+v, %v", list, err)
	}

	o := officers.Officer{ID: "marketing:team", Owner: team}
	id, err := officerAsker{w.srv}.Raise(context.Background(), o, builtin.AskAbout{}, "Which launch date?", []string{"Monday", "Friday"}, time.Hour)
	if err != nil {
		t.Fatalf("the team officer's ask: %v", err)
	}
	var owner int64
	if err := w.pool.QueryRow(w.sys, `SELECT owner_id FROM attention_asks WHERE id = $1`, id).Scan(&owner); err != nil || owner != team {
		t.Fatalf("the ask is owned by %d (%v); want the team %d", owner, err, team)
	}
	if _, err := w.srv.ext.Attention.Get(w.personAt(t, w.b.uid, authz.ViaSession), id); err != nil {
		t.Errorf("the team's operator reading the team officer's ask: %v", err)
	}
	if _, err := w.srv.ext.Attention.Get(w.personAt(t, w.op.uid, authz.ViaSession), id); !errors.Is(err, attention.ErrNotFound) {
		t.Errorf("a non-member reading the team officer's ask: %v; want ErrNotFound", err)
	}
	if _, err := w.srv.ext.Attention.Answer(w.personAt(t, w.b.uid, authz.ViaSession), id, "Monday"); err != nil {
		t.Fatalf("the operator answering: %v", err)
	}
	var by *int64
	if err := w.pool.QueryRow(w.sys, `SELECT answered_by_user FROM attention_asks WHERE id = $1`, id).Scan(&by); err != nil || by == nil || *by != w.b.uid {
		t.Errorf("the answer records %v (%v); want the operator %d", by, err, w.b.uid)
	}
	if _, err := w.srv.ext.Attention.Answer(w.personAt(t, w.a.uid, authz.ViaSession), id, "Friday"); err == nil {
		t.Error("a second answer to the team's ask succeeded; want it refused (first answer wins)")
	}
}

// TestAnEscalationToATeamIsOneTeamAsk (phase 3 acceptance): an officer item
// escalated to a TEAM (its owner, or a team its escalate_to names) is raised
// by the attention sweep as ONE ask the team owns — a member sees it, a
// non-member does not — not a copy per member. CONTROL: the sweep raised it
// at all (the count is about the team, not a sweep that did nothing).
func TestAnEscalationToATeamIsOneTeamAsk(t *testing.T) {
	w := newIsoWorld(t)
	w.srv.ext.Users = users.NewPG(w.pool)
	team := w.aTeam
	w.setRole(t, team, w.b.uid, authz.TeamViewer)
	sys := authz.System(context.Background())
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		_, _ = w.pool.Exec(bg, `DELETE FROM attention_asks WHERE owner_id = $1`, team)
		_, _ = w.pool.Exec(bg, `DELETE FROM officer_items WHERE owner_id = $1`, team)
		_, _ = w.pool.Exec(bg, `DELETE FROM officer_defs WHERE owner_id = $1`, team)
		_, _ = w.pool.Exec(bg, `DELETE FROM officer_types WHERE owner_id = $1`, team)
	})
	ost := builtin.NewStore(w.pool)
	if err := ost.PutType(sys, team, builtin.Type{Key: "mk", Name: "Marketing", Charter: "c"}); err != nil {
		t.Fatal(err)
	}
	if err := ost.PutDef(sys, team, builtin.Def{Officer: "mk:a", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	var item int64
	if err := w.pool.QueryRow(sys, `INSERT INTO officer_items (owner_id, officer, kind, subject_key, status, outcome, summary, notices)
		VALUES ($1, 'mk:a', 'launch', 'launch-'||$2::text, 'escalated', 'escalated', 'needs a person', $3::jsonb) RETURNING id`,
		team, w.suffix, `[{"recipient":`+strconv.FormatInt(team, 10)+`,"suppressed":false,"attention_id":null,"settled":false}]`).Scan(&item); err != nil {
		t.Fatal(err)
	}
	w.srv.SweepOfficerAttention(context.Background())
	var asks int
	if err := w.pool.QueryRow(sys, `SELECT count(*) FROM attention_asks WHERE owner_id = $1`, team).Scan(&asks); err != nil {
		t.Fatal(err)
	}
	if asks != 1 {
		t.Fatalf("the escalation to the team raised %d team-owned asks; want exactly 1", asks)
	}
	var id int64
	if err := w.pool.QueryRow(sys, `SELECT id FROM attention_asks WHERE owner_id = $1`, team).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := w.srv.ext.Attention.Get(w.personAt(t, w.b.uid, authz.ViaSession), id); err != nil {
		t.Errorf("a viewer of the team reading its escalation: %v", err)
	}
	if _, err := w.srv.ext.Attention.Get(w.personAt(t, w.op.uid, authz.ViaSession), id); !errors.Is(err, attention.ErrNotFound) {
		t.Errorf("a non-member reading the team's escalation: %v; want ErrNotFound", err)
	}
	var elsewhere int
	if err := w.pool.QueryRow(sys, `SELECT count(*) FROM attention_asks WHERE owner_id IN ($1, $2) AND question LIKE '%needs a person%'`, w.a.uid, w.b.uid).Scan(&elsewhere); err != nil || elsewhere != 0 {
		t.Errorf("the escalation also copied %d asks into members' own accounts (%v); want none", elsewhere, err)
	}
}

// TestATeamIsAnEscalationTargetOnlyOnItsOfficersPage: a manager names, as a
// team officer's escalate_to, the team itself or a team they operate — and
// not a team they are only a viewer of. The JSON admin API (A's PERSONAL API
// token) refuses a team id as it always has: a personal token carries no team
// access. CONTROL: the same token names a person.
func TestATeamIsAnEscalationTargetOnlyOnItsOfficersPage(t *testing.T) {
	w := newIsoWorld(t)
	team := w.aTeam
	viewed, err := users.NewPG(w.pool).CreateTeam(w.sys, users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}, "viewed-"+w.suffix, w.b.uid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		_, _ = w.pool.Exec(bg, `DELETE FROM officer_defs WHERE owner_id = $1`, team)
		_, _ = w.pool.Exec(bg, `DELETE FROM officer_types WHERE owner_id = $1`, team)
		_, _ = w.pool.Exec(bg, `DELETE FROM team_members WHERE team_id = $1`, viewed.ID)
		_, _ = w.pool.Exec(bg, `DELETE FROM audit_log WHERE object = $1`, users.TeamObject(viewed.ID))
		_, _ = w.pool.Exec(bg, `DELETE FROM users WHERE id = $1`, viewed.ID)
	})
	w.setRole(t, viewed.ID, w.a.uid, authz.TeamViewer)
	ost := builtin.NewStore(w.pool)
	if err := ost.PutType(w.sys, team, builtin.Type{Key: "mk", Name: "Marketing", Charter: "c"}); err != nil {
		t.Fatal(err)
	}
	page := "/teams/" + strconv.FormatInt(team, 10) + "/officers/defs/mk:a"
	esc := func(ids ...int64) string {
		var parts []string
		for _, id := range ids {
			parts = append(parts, strconv.FormatInt(id, 10))
		}
		return url.Values{"mode": {"live"}, "enabled": {"1"}, "escalate_to": {strings.Join(parts, ",")}}.Encode()
	}
	if rec := w.do(w.h, "POST", page, esc(team, w.b.uid), cred{sessionUser: w.a.uid}); rec.Code != http.StatusSeeOther {
		t.Fatalf("the manager naming the team and a person: %d %.300s", rec.Code, rec.Body.String())
	}
	if d, err := ost.GetDef(w.sys, team, "mk:a"); err != nil || len(d.EscalateTo) != 2 || d.EscalateTo[0] != team {
		t.Fatalf("the team officer's escalate_to: %+v, %v; want the team and the person", d, err)
	}
	if rec := w.do(w.h, "POST", page, esc(viewed.ID), cred{sessionUser: w.a.uid}); rec.Code != http.StatusBadRequest {
		t.Errorf("the manager naming a team they only view: %d; want 400", rec.Code)
	}
	tok := cred{apiToken: w.apiTokens[w.a.uid]}
	if rec := w.do(w.h, "PUT", "/api/officers/defs/"+url.PathEscape(w.objA.officer), `{"escalate_to":[`+strconv.FormatInt(team, 10)+`]}`, tok); rec.Code != http.StatusBadRequest {
		t.Errorf("A's personal API token naming a team as escalate_to: %d; want 400 (no team access)", rec.Code)
	}
	if rec := w.do(w.h, "PUT", "/api/officers/defs/"+url.PathEscape(w.objA.officer), `{"escalate_to":[`+strconv.FormatInt(w.b.uid, 10)+`]}`, tok); rec.Code != http.StatusOK {
		t.Errorf("control: A's personal API token naming a person: %d %.300s", rec.Code, rec.Body.String())
	}
}
