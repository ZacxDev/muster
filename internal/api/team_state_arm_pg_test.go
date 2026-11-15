package api

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sessionstate"
	"github.com/ZacxDev/muster/internal/users"
)

// THE SESSION-STATE PASS'S TEAM ARM (team sharing 2b.3;
// docs/design/team-sharing.md A, "Waiting and done pushes for team
// sessions"): a team host's waiting window is an episode, opened, settled and
// pushed AS THE TEAM, and the push fan-out reaches the team's operators and
// managers once each — never a viewer. No classifier runs for a team.
//
// Synthetic fixtures: team "statearm-<n>" with ana (manager), ben (operator)
// and cal (viewer), whose host is the fixture's "ws" (fakeMirror.teamHost).

// stateTeam is the fixture team and its people.
type stateTeam struct {
	team, ana, ben, cal int64
}

// newStateTeam makes the team in f, gives it an unrevoked mirror token for
// "ws" (so the state pass visits it) and makes the fixture's host its host
// too. Pushes are recorded per person (fanPush).
func newStateTeam(t *testing.T, f *stateFixture) (stateTeam, *fanPush) {
	t.Helper()
	sys := authz.System(context.Background())
	us := users.NewPG(f.pool)
	f.srv.ext.Users = us
	rec := &fanPush{}
	f.srv.ext.Push = rec
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	mk := func(name string) int64 {
		var id int64
		if err := f.pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, name+"-"+suffix).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	st := stateTeam{ana: mk("sa-ana"), ben: mk("sa-ben"), cal: mk("sa-cal")}
	admin := users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}
	tm, err := us.CreateTeam(sys, admin, "statearm-"+suffix, st.ana)
	if err != nil {
		t.Fatal(err)
	}
	st.team = tm.ID
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM session_episodes WHERE owner_id = $1`, `DELETE FROM mirror_tokens WHERE owner_id = $1`,
			`DELETE FROM chief_nudges WHERE owner_id = $1`, `DELETE FROM attention_asks WHERE owner_id = $1`,
			`DELETE FROM agents WHERE owner_id = $1`, `DELETE FROM team_members WHERE team_id = $1`,
			`DELETE FROM audit_log WHERE team_id = $1 OR user_id = $1`, `DELETE FROM users WHERE id = $1`} {
			_, _ = f.pool.Exec(bg, q, st.team)
		}
		for _, id := range []int64{st.ana, st.ben, st.cal} {
			_, _ = f.pool.Exec(bg, `DELETE FROM team_members WHERE user_id = $1`, id)
			_, _ = f.pool.Exec(bg, `DELETE FROM audit_log WHERE user_id = $1`, id)
			_, _ = f.pool.Exec(bg, `DELETE FROM users WHERE id = $1`, id)
		}
	})
	for id, role := range map[int64]authz.TeamRole{st.ben: authz.TeamOperator, st.cal: authz.TeamViewer} {
		if err := us.SetTeamMember(sys, admin, st.team, id, role); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := mirror.NewPG(f.pool).CreateTeamToken(sys, admin, st.team, "ws"); err != nil {
		t.Fatal(err)
	}
	f.mirror.mu.Lock()
	f.mirror.teamHost = st.team
	f.mirror.mu.Unlock()
	return st, rec
}

// teamEpisode is one of a team's episodes as the table holds it.
type teamEpisode struct {
	ID     int64
	State  string
	Ended  bool
	JevErr string
}

// teamEpisodes is the team's episodes, newest first, read straight from the
// table.
func (f *stateFixture) teamEpisodes(t *testing.T, team int64) []teamEpisode {
	t.Helper()
	rows, err := f.pool.Query(authz.System(context.Background()), `SELECT id, state, ended_at IS NOT NULL, coalesce(jev_error, '')
		FROM session_episodes WHERE owner_id = $1 ORDER BY id DESC`, team)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []teamEpisode
	for rows.Next() {
		var e teamEpisode
		if err := rows.Scan(&e.ID, &e.State, &e.Ended, &e.JevErr); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// episodePushesTo is how many non-withdrawal pushes of episode id each of
// people received, in people's order.
func episodePushesTo(rec *fanPush, id int64, people ...int64) []int {
	tag := "attention-episode-" + strconv.FormatInt(id, 10)
	out := make([]int, len(people))
	for i, p := range people {
		for _, pl := range rec.sent(p, tag, false) {
			if pl.Tag == tag {
				out[i]++
			}
		}
	}
	return out
}

// TestATeamSessionWaitingPushesItsOperatorsOnce: a team host's window waits.
// The waiting hook opens ONE episode, owned by the TEAM, held; once its grace has
// passed the state pass pushes it, and ben (operator) and ana (manager) get
// ONE push each, cal (viewer) none — and another pass and another hook nudge
// send nothing more. When the window moves on (sustained work) the episode
// settles, and its notification is withdrawn from ben and ana.
//
// 🔴 RED at the base (fcd4bbfd): the pass iterated persons only and the hook
// opened no episode for a team (2b.2's branch), so no episode push reached
// anyone: the team's wait took the diff path.
func TestATeamSessionWaitingPushesItsOperatorsOnce(t *testing.T) {
	f := newStateFixture(t, false)
	st, rec := newStateTeam(t, f)
	f.waitWithSecrets()
	f.srv.AttentionMirrorChanged(st.team)
	// The waiting hook opens the team's episode itself, as a person's (2b.2's
	// branch, removed, left a team's waits on the diff path instead).
	if eps := f.teamEpisodes(t, st.team); len(eps) != 1 || eps[0].Ended {
		t.Fatalf("after the waiting hook the team's episodes are %+v; want one open, held for its grace", eps)
	}
	f.pastGrace(t)
	f.srv.AttentionMirrorChanged(st.team)
	f.statePass(t)
	f.srv.WaitForPushes()
	eps := f.teamEpisodes(t, st.team)
	if len(eps) != 1 || eps[0].State != sessionstate.StateNeedsYou || eps[0].Ended {
		t.Fatalf("the team's episodes: %+v; want one open needs-you episode", eps)
	}
	id := eps[0].ID
	if got := episodePushesTo(rec, id, st.ben, st.ana, st.cal); got[0] != 1 || got[1] != 1 || got[2] != 0 {
		t.Fatalf("episode pushes to ben, ana, cal: %v; want [1 1 0]", got)
	}
	// ONCE: the episode's push is the only notification of this wait — the
	// waiting hook's diff path (2b.2's branch for a team) would send a second.
	for _, who := range []int64{st.ben, st.ana} {
		if all := rec.sent(who, "", false); len(all) != 1 {
			var tags []string
			for _, p := range all {
				tags = append(tags, p.Tag)
			}
			t.Errorf("person %d received %d notifications of the wait (%v); want the episode's one", who, len(all), tags)
		}
	}
	for _, p := range rec.sent(st.ben, "attention-episode-", false) {
		if strings.Contains(p.Title+p.Body, "PUSHSECRET") {
			t.Fatalf("session text in the team push: %q / %q", p.Title, p.Body)
		}
	}

	// The window moves on: sustained work settles the stop.
	f.advance(10 * time.Second)
	f.statusSince(mirror.StatusBusy, nudgeSince+300_000, 0)
	f.statePass(t)
	f.advance(sessionstate.EndAfter + 9*time.Second)
	f.statePass(t)
	f.srv.WaitForPushes()
	eps = f.teamEpisodes(t, st.team)
	if len(eps) != 1 || !eps[0].Ended {
		t.Fatalf("after the window moved on: %+v; want the episode ended", eps)
	}
	tag := "attention-episode-" + strconv.FormatInt(id, 10)
	if got := rec.withdrawnFrom(tag, st.ana, st.ben, st.cal); len(got) != 2 {
		t.Fatalf("withdrawn from %v; want ana and ben", got)
	}
}

// TestTheStatePassRunsNoClassifierForATeam: with the classifier configured
// and the chief's nudges on, a team whose own chief is on — the one case in
// which a person's pass would classify — has its idle stop opened unclassified
// (counted as asked, not_configured), and the classifier is called for the
// PERSON's stop only. CONTROL: the person's session, the same window, IS
// classified (one call). The team's chief makes the guard reachable: without
// one, stateClassifier would answer nil for the team anyway.
func TestTheStatePassRunsNoClassifierForATeam(t *testing.T) {
	f := newStateFixture(t, true)
	j := withJev(t, f.nudgeFixture)
	j.set(200, 0.81, 0.10, 0.09)
	st, _ := newStateTeam(t, f)
	tctx := authz.WithPrincipal(context.Background(), teamActingPrincipal(st.team, authz.ViaAttention))
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := agents.NewPG(f.pool).Create(tctx, agents.Agent{Name: "sachief-" + suffix, Namespace: "devpod-sachief-" + suffix,
		DisplayName: "chief", Status: agents.StatusRunning, HooksToken: "sa-chief-tok-" + suffix}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.chiefAgent(tctx); err != nil {
		t.Fatalf("the fixture's team chief is not resolved (the guard would be unreachable): %v", err)
	}
	f.statePass(t)
	person := f.oneEpisode(t)
	if person.ClassifiedStreak == "" || person.Jev.Err != "" || person.State != sessionstate.StateNeedsYou {
		t.Fatalf("CONTROL: the person's stop was not classified: %+v", person)
	}
	eps := f.teamEpisodes(t, st.team)
	if len(eps) != 1 {
		t.Fatalf("the team's episodes: %+v; want one (its idle stop)", eps)
	}
	if eps[0].JevErr != jevNotConfigured {
		t.Fatalf("the team's stop classification error %q; want %q (no classifier ran)", eps[0].JevErr, jevNotConfigured)
	}
	if n := f.jevCalls(j); n != 1 {
		t.Fatalf("classifier calls: %d; want 1 (the person's stop alone)", n)
	}
}
