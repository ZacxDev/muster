package api

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/users"
)

// The minimal team fan-out and the team audit rows (team sharing phase 3,
// #668 round 0: Q1 and choice 12), through the real server against a real
// Postgres.

// ownerPush is a PushService that records WHO each push went to (sendPush's
// owner) and its tag; its store mutes nothing.
type ownerPush struct {
	mu  sync.Mutex
	got map[int64][]string // owner -> tags
}

type unmutedStore struct{ push.Store }

func (unmutedStore) Muted(context.Context, int64) ([]string, error) { return nil, nil }

func (o *ownerPush) PublicKey() string         { return "k" }
func (o *ownerPush) Store() push.Store         { return unmutedStore{} }
func (o *ownerPush) Allowlist() push.Allowlist { return push.NewAllowlist(nil) }
func (o *ownerPush) Probe(context.Context, push.Subscription, []byte) push.SendResult {
	return push.SendResult{}
}
func (o *ownerPush) SendTo(_ context.Context, owner int64, p push.Payload) push.Report {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.got == nil {
		o.got = map[int64][]string{}
	}
	o.got[owner] = append(o.got[owner], p.Tag)
	return push.Report{}
}

// to is which of people received a push whose tag has prefix (sorted).
func (o *ownerPush) to(prefix string, people ...int64) []int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []int64
	for _, id := range people {
		for _, tag := range o.got[id] {
			if strings.HasPrefix(tag, prefix) {
				out = append(out, id)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (o *ownerPush) reset() {
	o.mu.Lock()
	o.got = nil
	o.mu.Unlock()
}

func ids(xs ...int64) []int64 {
	sort.Slice(xs, func(i, j int) bool { return xs[i] < xs[j] })
	return xs
}

func sameIDs(a, b []int64) bool { return slices.Equal(a, b) }

// TestATeamOfficersNotificationsReachItsOperatorsAndManagers (Q1): a team
// officer's ask, its escalation to the team and its outbound draft each push
// to every current OPERATOR and MANAGER of the team — and to no viewer and no
// one who has left. A PERSONAL officer's ask and outbound draft still push to
// its owner alone. A team's REPLY draft (not an officer's) fans out too since
// phase 2b.1 (goNotify's one fan-out, team_fanout_pg_test.go). The positive control is that the members' pushes are counted at all
// (the manager's and the operator's).
func TestATeamOfficersNotificationsReachItsOperatorsAndManagers(t *testing.T) {
	w := newIsoWorld(t)
	w.srv.ext.Users = users.NewPG(w.pool)
	rec := &ownerPush{}
	w.srv.ext.Push = rec
	team, mgr, op := w.aTeam, w.a.uid, w.b.uid
	mk := func(name string) int64 {
		var id int64
		if err := w.pool.QueryRow(w.sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, name+w.suffix).Scan(&id); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			bg := authz.System(context.Background())
			_, _ = w.pool.Exec(bg, `DELETE FROM team_members WHERE user_id = $1`, id)
			_, _ = w.pool.Exec(bg, `DELETE FROM audit_log WHERE user_id = $1`, id)
			_, _ = w.pool.Exec(bg, `DELETE FROM users WHERE id = $1`, id)
		})
		return id
	}
	viewer, gone, disabled := mk("fanout-viewer-"), mk("fanout-gone-"), mk("fanout-disabled-")
	w.setRole(t, team, op, authz.TeamOperator)
	// An OPERATOR whose account is disabled (#668 round 1, Y2): a member
	// still, but nobody's phone should ring for them.
	w.setRole(t, team, disabled, authz.TeamOperator)
	if _, err := w.pool.Exec(w.sys, `UPDATE users SET disabled_at = now() WHERE id = $1`, disabled); err != nil {
		t.Fatal(err)
	}
	w.setRole(t, team, viewer, authz.TeamViewer)
	w.setRole(t, team, gone, authz.TeamOperator)
	if err := users.NewPG(w.pool).RemoveTeamMember(w.sys, users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}, team, gone); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		_, _ = w.pool.Exec(bg, `DELETE FROM attention_asks WHERE owner_id IN ($1, $2)`, team, mgr)
		_, _ = w.pool.Exec(bg, `DELETE FROM officer_items WHERE owner_id = $1`, team)
		_, _ = w.pool.Exec(bg, `DELETE FROM officer_defs WHERE owner_id = $1`, team)
		_, _ = w.pool.Exec(bg, `DELETE FROM officer_types WHERE owner_id = $1`, team)
	})
	people := []int64{mgr, op, viewer, gone, disabled}
	want := ids(mgr, op)

	// 1. A team officer's ask.
	o := officers.Officer{ID: "mk:a", Owner: team}
	if _, err := (officerAsker{w.srv}).Raise(context.Background(), o, builtin.AskAbout{}, "Which launch date?", nil, false, time.Hour); err != nil {
		t.Fatal(err)
	}
	w.srv.WaitForPushes()
	if got := rec.to("attention-", people...); !sameIDs(got, want) {
		t.Errorf("a team officer's ask pushed to %v; want its operator and manager %v (never viewer %d, ex-member %d or disabled operator %d)", got, want, viewer, gone, disabled)
	}

	// 2. An escalation to the team.
	rec.reset()
	sys := authz.System(context.Background())
	ost := builtin.NewStore(w.pool)
	if err := ost.PutType(sys, team, builtin.Type{Key: "mk", Name: "Marketing", Charter: "c"}); err != nil {
		t.Fatal(err)
	}
	if err := ost.PutDef(sys, team, builtin.Def{Officer: "mk:a", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.pool.Exec(sys, `INSERT INTO officer_items (owner_id, officer, kind, subject_key, status, outcome, summary, notices)
		VALUES ($1, 'mk:a', 'launch', 'fanout-'||$2::text, 'escalated', 'escalated', 'needs a person', $3::jsonb)`,
		team, w.suffix, `[{"recipient":`+strconv.FormatInt(team, 10)+`,"suppressed":false,"attention_id":null,"settled":false}]`); err != nil {
		t.Fatal(err)
	}
	w.srv.SweepOfficerAttention(context.Background())
	w.srv.WaitForPushes()
	if got := rec.to("attention-", people...); !sameIDs(got, want) {
		t.Errorf("an escalation to the team pushed to %v; want %v", got, want)
	}

	// 3. A team officer's outbound draft awaiting its decision.
	rec.reset()
	officer := "mk:a"
	w.srv.notifyDraftAwaitingNoting(context.Background(), team, plugins.Draft{ID: 990001, OwnerID: team, Officer: &officer}, plugins.Message{}, "")
	w.srv.WaitForPushes()
	if got := rec.to("plugin-draft-", people...); !sameIDs(got, want) {
		t.Errorf("a team officer's outbound draft pushed to %v; want %v", got, want)
	}
	// …and since phase 2b.1 a team's REPLY draft (no officer) fans out too:
	// every team-owned notification goes through goNotify's one fan-out.
	rec.reset()
	w.srv.notifyDraftAwaitingNoting(context.Background(), team, plugins.Draft{ID: 990002, OwnerID: team}, plugins.Message{}, "")
	w.srv.WaitForPushes()
	if got := rec.to("plugin-draft-", people...); !sameIDs(got, want) {
		t.Errorf("a team's reply draft pushed to %v; want %v (phase 2b.1: every team-owned push fans out)", got, want)
	}

	// 4. A PERSONAL officer's ask and draft: its owner alone, as before.
	rec.reset()
	if _, err := (officerAsker{w.srv}).Raise(context.Background(), officers.Officer{ID: "mk:own", Owner: mgr}, builtin.AskAbout{}, "Mine?", nil, false, time.Hour); err != nil {
		t.Fatal(err)
	}
	w.srv.notifyDraftAwaitingNoting(context.Background(), mgr, plugins.Draft{ID: 990003, OwnerID: mgr, Officer: &officer}, plugins.Message{}, "")
	w.srv.WaitForPushes()
	if got := rec.to("attention-", people...); !sameIDs(got, ids(mgr)) {
		t.Errorf("a personal officer's ask pushed to %v; want only its owner %d", got, mgr)
	}
	if got := rec.to("plugin-draft-", people...); !sameIDs(got, ids(mgr)) {
		t.Errorf("a personal officer's draft pushed to %v; want only its owner %d", got, mgr)
	}
}

// auditRows is the audit rows naming team with action, as "action by user".
func (w *isoWorld) auditRows(t *testing.T, team int64, actions ...string) []string {
	t.Helper()
	rows, err := w.pool.Query(w.sys, `SELECT action || ' by ' || user_id FROM audit_log WHERE team_id = $1 AND action = ANY($2) ORDER BY id`, team, actions)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// TestEachTeamActionWritesItsAuditRow (choice 12, design §4): sharing a host
// spawn with a team, taking it back, and a manager's edit of a team officer's
// type and definition each write an audit_log row naming the acting person
// and the team (audit_log.team_id). The share and unshare through the CHIEF's
// door record that person too. CONTROL: a REFUSED edit (an operator's) writes
// none.
func TestEachTeamActionWritesItsAuditRow(t *testing.T) {
	w := newIsoWorld(t)
	w.srv.ext.Users = users.NewPG(w.pool)
	team := w.aTeam
	w.setRole(t, team, w.b.uid, authz.TeamOperator)
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		_, _ = w.pool.Exec(bg, `DELETE FROM officer_defs WHERE owner_id = $1`, team)
		_, _ = w.pool.Exec(bg, `DELETE FROM officer_types WHERE owner_id = $1`, team)
		_, _ = w.pool.Exec(bg, `DELETE FROM audit_log WHERE team_id = $1`, team)
	})
	a := strconv.FormatInt(w.a.uid, 10)
	path := officerSpawnsPath + "/" + strconv.FormatInt(w.objA.spawn, 10)
	if rec := w.do(w.h, "POST", path+"/share", url.Values{"team": {strconv.FormatInt(team, 10)}}.Encode(), cred{sessionUser: w.a.uid}); rec.Code != http.StatusSeeOther {
		t.Fatalf("share: %d", rec.Code)
	}
	if rec := w.do(w.h, "POST", path+"/unshare", "", cred{sessionUser: w.a.uid}); rec.Code != http.StatusSeeOther {
		t.Fatalf("unshare: %d", rec.Code)
	}
	chief := w.personAt(t, w.a.uid, authz.ViaSession) // a browser chat turn's tools
	_ = w.srv.toolOfficerSpawnShare(chief, `{"action":"share","id":`+strconv.FormatInt(w.objA.spawn, 10)+`,"team_id":`+strconv.FormatInt(team, 10)+`}`)
	_ = w.srv.toolOfficerSpawnShare(chief, `{"action":"unshare","id":`+strconv.FormatInt(w.objA.spawn, 10)+`}`)
	got := w.auditRows(t, team, "officer.spawn.share", "officer.spawn.unshare")
	wantSpawn := []string{"officer.spawn.share by " + a, "officer.spawn.unshare by " + a, "officer.spawn.share by " + a, "officer.spawn.unshare by " + a}
	if strings.Join(got, "|") != strings.Join(wantSpawn, "|") {
		t.Errorf("the spawn's audit rows = %v; want %v", got, wantSpawn)
	}

	page := "/teams/" + strconv.FormatInt(team, 10) + "/officers"
	// CONTROL: the operator's refused edit writes nothing.
	if rec := w.do(w.h, "POST", page+"/types/mk", url.Values{"name": {"M"}, "charter": {"c"}}.Encode(), cred{sessionUser: w.b.uid}); rec.Code != http.StatusForbidden {
		t.Fatalf("the operator's edit: %d; want 403", rec.Code)
	}
	if rows := w.auditRows(t, team, "officer.type.put", "officer.def.put"); len(rows) != 0 {
		t.Fatalf("a refused edit wrote audit rows %v", rows)
	}
	if rec := w.do(w.h, "POST", page+"/types/mk", url.Values{"name": {"Marketing"}, "charter": {"c1"}}.Encode(), cred{sessionUser: w.a.uid}); rec.Code != http.StatusSeeOther {
		t.Fatalf("the manager's type edit: %d", rec.Code)
	}
	if rec := w.do(w.h, "POST", page+"/defs/mk:a", url.Values{"mode": {"live"}, "enabled": {"1"}}.Encode(), cred{sessionUser: w.a.uid}); rec.Code != http.StatusSeeOther {
		t.Fatalf("the manager's officer edit: %d", rec.Code)
	}
	got = w.auditRows(t, team, "officer.type.put", "officer.def.put")
	if want := []string{"officer.type.put by " + a, "officer.def.put by " + a}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("the charter edits' audit rows = %v; want %v", got, want)
	}
}

// TestATokenDrivenChiefTurnCanLendTheOwnersOwnSpawn (operator decision on
// #668 after round 1: an approved exception to "a personal API token carries
// no team access"): the chief's officer_spawn_share in a turn a token started
// — the machine-message door (POST /api/agents/{name}/messages) and a
// claude-code chief's own agent door over MCP — LENDS and UNLENDS the
// owner's own spawn. The owner, role and team checks still hold there: a lend
// to a team the owner only views is ErrLendRole, and the row is unchanged.
func TestATokenDrivenChiefTurnCanLendTheOwnersOwnSpawn(t *testing.T) {
	w := newIsoWorld(t)
	w.srv.ext.Users = users.NewPG(w.pool)
	spawn, team := w.objA.spawn, w.aTeam
	viewed, err := users.NewPG(w.pool).CreateTeam(w.sys, users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}, "viewed-tok-"+w.suffix, w.b.uid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		_, _ = w.pool.Exec(bg, `DELETE FROM team_members WHERE team_id = $1`, viewed.ID)
		_, _ = w.pool.Exec(bg, `DELETE FROM audit_log WHERE team_id = $1 OR object = $2`, viewed.ID, users.TeamObject(viewed.ID))
		_, _ = w.pool.Exec(bg, `DELETE FROM users WHERE id = $1`, viewed.ID)
	})
	w.setRole(t, viewed.ID, w.a.uid, authz.TeamViewer)
	shareTo := func(team int64) string {
		return `{"action":"share","id":` + strconv.FormatInt(spawn, 10) + `,"team_id":` + strconv.FormatInt(team, 10) + `}`
	}
	unshare := `{"action":"unshare","id":` + strconv.FormatInt(spawn, 10) + `}`
	for _, via := range []authz.Via{authz.ViaAgentMessage, authz.ViaAgent} {
		ctx := w.personAt(t, w.a.uid, via)
		if out := w.srv.toolOfficerSpawnShare(ctx, shareTo(team)); w.spawnTeam(t, spawn) != team {
			t.Errorf("a chief turn through %s sharing with the owner's team: %s (lent to %d); want lent to %d", via, out, w.spawnTeam(t, spawn), team)
		}
		if out := w.srv.toolOfficerSpawnShare(ctx, unshare); w.spawnTeam(t, spawn) != 0 {
			t.Errorf("a chief turn through %s unsharing: %s (lent to %d); want unlent", via, out, w.spawnTeam(t, spawn))
		}
		if out := w.srv.toolOfficerSpawnShare(ctx, shareTo(viewed.ID)); !strings.Contains(out, ErrLendRole.Error()) || w.spawnTeam(t, spawn) != 0 {
			t.Errorf("a chief turn through %s sharing with a team the owner only views: %s (lent to %d); want ErrLendRole, unlent", via, out, w.spawnTeam(t, spawn))
		}
	}
}

// TestMovingALendWritesTheOldTeamsUnshareRow (#668 round 1, G2): the chief
// moving a lend from one team straight to another writes an unshare row for
// the team it left as well as a share row for the new one, so the first
// team's managers see it go.
func TestMovingALendWritesTheOldTeamsUnshareRow(t *testing.T) {
	w := newIsoWorld(t)
	w.srv.ext.Users = users.NewPG(w.pool)
	other, err := users.NewPG(w.pool).CreateTeam(w.sys, users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}, "second-"+w.suffix, w.a.uid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		_, _ = w.pool.Exec(bg, `DELETE FROM team_members WHERE team_id = $1`, other.ID)
		_, _ = w.pool.Exec(bg, `DELETE FROM audit_log WHERE team_id = $1 OR object = $2`, other.ID, users.TeamObject(other.ID))
		_, _ = w.pool.Exec(bg, `DELETE FROM users WHERE id = $1`, other.ID)
		_, _ = w.pool.Exec(bg, `DELETE FROM audit_log WHERE team_id = $1 AND action LIKE 'officer.spawn.%'`, w.aTeam)
	})
	chief := w.personAt(t, w.a.uid, authz.ViaSession)
	id := strconv.FormatInt(w.objA.spawn, 10)
	_ = w.srv.toolOfficerSpawnShare(chief, `{"action":"share","id":`+id+`,"team_id":`+strconv.FormatInt(w.aTeam, 10)+`}`)
	_ = w.srv.toolOfficerSpawnShare(chief, `{"action":"share","id":`+id+`,"team_id":`+strconv.FormatInt(other.ID, 10)+`}`)
	if w.spawnTeam(t, w.objA.spawn) != other.ID {
		t.Fatalf("the move left the spawn lent to %d; want %d", w.spawnTeam(t, w.objA.spawn), other.ID)
	}
	a := strconv.FormatInt(w.a.uid, 10)
	if got, want := w.auditRows(t, w.aTeam, "officer.spawn.share", "officer.spawn.unshare"), []string{"officer.spawn.share by " + a, "officer.spawn.unshare by " + a}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("the first team's spawn audit rows = %v; want %v (it must see the lend leave)", got, want)
	}
	if got, want := w.auditRows(t, other.ID, "officer.spawn.share", "officer.spawn.unshare"), []string{"officer.spawn.share by " + a}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("the second team's spawn audit rows = %v; want %v", got, want)
	}
}

// TestATeamOfficerInstancesOwnAskFansOut (#668 round 1, G1): a team
// officer's INSTANCE calling attention_ask for itself is filed like the
// officer's other asks — its push reaches the team's operators and managers.
// CONTROL: the same tool from an agent that is no officer's instance pushes to
// its owner only.
func TestATeamOfficerInstancesOwnAskFansOut(t *testing.T) {
	w := newIsoWorld(t)
	w.srv.ext.Users = users.NewPG(w.pool)
	rec := &ownerPush{}
	w.srv.ext.Push = rec
	team := w.aTeam
	w.setRole(t, team, w.b.uid, authz.TeamOperator)
	o := officers.Officer{ID: "mk:a", Owner: team, MaxInstances: 1}
	w.srv.officerRT = &officerRuntime{reg: officers.NewRegistry([]officers.Officer{o})}
	t.Cleanup(func() {
		w.srv.officerRT = nil
		bg := authz.System(context.Background())
		_, _ = w.pool.Exec(bg, `DELETE FROM attention_asks WHERE owner_id = $1`, team)
		_, _ = w.pool.Exec(bg, `DELETE FROM agents WHERE owner_id = $1`, team)
	})
	teamCtx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: team, Team: true, Role: authz.RoleMember, Via: authz.ViaAgent})
	inst, err := agents.NewPG(w.pool).Create(teamCtx, agents.Agent{Name: o.InstanceName(1), Namespace: "devpod-" + o.InstanceName(1),
		Kind: agents.KindGateway, Status: agents.StatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	if bound, _, ok := w.srv.officerOf(inst); !ok || bound.Owner != team {
		t.Fatalf("fixture: %s is not read as the team officer's instance", inst.Name)
	}
	if out := w.srv.toolAttentionAsk(teamCtx, callerOfAgent(inst), `{"question":"Ship it?"}`); !strings.Contains(out, `"status":"pending"`) {
		t.Fatalf("the instance's attention_ask: %s", out)
	}
	w.srv.WaitForPushes()
	if got, want := rec.to("attention-", w.a.uid, w.b.uid), ids(w.a.uid, w.b.uid); !sameIDs(got, want) {
		t.Errorf("a team officer instance's own ask pushed to %v; want the team's manager and operator %v", got, want)
	}
	// CONTROL: a person's own non-officer agent's ask goes to that person.
	rec.reset()
	out := w.srv.toolAttentionAsk(w.personAt(t, w.a.uid, authz.ViaAgent), callerOfAgent(w.a.agent), `{"question":"Mine?"}`)
	w.srv.WaitForPushes()
	if got := rec.to("attention-", w.a.uid, w.b.uid); !sameIDs(got, ids(w.a.uid)) {
		t.Errorf("a non-officer agent's ask pushed to %v; want only its owner (%s)", got, out)
	}
}
