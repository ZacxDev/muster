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
// its owner alone, and a team's REPLY draft (not an officer's) does not fan
// out. The positive control is that the members' pushes are counted at all
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
	viewer, gone := mk("fanout-viewer-"), mk("fanout-gone-")
	w.setRole(t, team, op, authz.TeamOperator)
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
	people := []int64{mgr, op, viewer, gone}
	want := ids(mgr, op)

	// 1. A team officer's ask.
	o := officers.Officer{ID: "mk:a", Owner: team}
	if _, err := (officerAsker{w.srv}).Raise(context.Background(), o, builtin.AskAbout{}, "Which launch date?", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	w.srv.WaitForPushes()
	if got := rec.to("attention-", people...); !sameIDs(got, want) {
		t.Errorf("a team officer's ask pushed to %v; want its operator and manager %v (never viewer %d or ex-member %d)", got, want, viewer, gone)
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
	// …and a team's REPLY draft (no officer) does not fan out.
	rec.reset()
	w.srv.notifyDraftAwaitingNoting(context.Background(), team, plugins.Draft{ID: 990002, OwnerID: team}, plugins.Message{}, "")
	w.srv.WaitForPushes()
	if got := rec.to("plugin-draft-", people...); len(got) != 0 {
		t.Errorf("a team's reply draft pushed to members %v; want none (only an officer's draft fans out)", got)
	}

	// 4. A PERSONAL officer's ask and draft: its owner alone, as before.
	rec.reset()
	if _, err := (officerAsker{w.srv}).Raise(context.Background(), officers.Officer{ID: "mk:own", Owner: mgr}, builtin.AskAbout{}, "Mine?", nil, time.Hour); err != nil {
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
	chief := w.personAt(t, w.a.uid, authz.ViaAgent)
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
