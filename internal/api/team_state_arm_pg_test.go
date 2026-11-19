package api

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sessionstate"
	"github.com/ZacxDev/muster/internal/ui"
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

// pushedTeamWait is the waiting test's first half: a team window waits, its
// episode is pushed to ben and ana, and the episode's id is returned.
func pushedTeamWait(t *testing.T, f *stateFixture, st stateTeam, rec *fanPush) int64 {
	t.Helper()
	f.waitWithSecrets()
	f.srv.AttentionMirrorChanged(st.team)
	f.pastGrace(t)
	f.srv.WaitForPushes()
	eps := f.teamEpisodes(t, st.team)
	if len(eps) != 1 || eps[0].Ended {
		t.Fatalf("the team's episodes: %+v; want one open", eps)
	}
	if got := episodePushesTo(rec, eps[0].ID, st.ben, st.ana); got[0] != 1 || got[1] != 1 {
		t.Fatalf("precondition: episode pushes to ben, ana: %v; want [1 1]", got)
	}
	return eps[0].ID
}

// TestLeavingATeamWithdrawsItsEpisodePushes (#690 round 1, 🟡1): ben holds a
// team session's "Needs you" push; he leaves the team, and the sweep sends
// him its withdrawal — the episode is still open, so its own clear would only
// ever reach the CURRENT operators and managers. CONTROL: ana, still a
// manager, is sent none.
//
// 🔴 RED at 1181b8aa: withdrawableTeamKinds had no episode kind.
func TestLeavingATeamWithdrawsItsEpisodePushes(t *testing.T) {
	f := newStateFixture(t, false)
	st, rec := newStateTeam(t, f)
	id := pushedTeamWait(t, f, st, rec)
	sys := authz.System(context.Background())
	if err := users.NewPG(f.pool).RemoveTeamMember(sys, users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}, st.team, st.ben); err != nil {
		t.Fatal(err)
	}
	f.srv.teamMembershipEnded(st.team, st.ben)
	f.srv.WaitForPushes()
	tag := "attention-episode-" + strconv.FormatInt(id, 10)
	if got := rec.withdrawnFrom(tag, st.ben, st.ana); len(got) != 1 || got[0] != st.ben {
		t.Fatalf("the open episode's push was withdrawn from %v; want ben alone", got)
	}
}

// TestRevokingATeamsLastTokenEndsItsEpisodes (#690 round 1, 🟡2): a team
// window waits and is pushed; the team's only host token is revoked. The next
// pass still visits the team, ends the episode 'gone' and withdraws its push
// from ben and ana — no host will settle it again.
//
// 🔴 RED at 1181b8aa: the team left the owner list with its last token, and
// its episode stayed open and shown.
func TestRevokingATeamsLastTokenEndsItsEpisodes(t *testing.T) {
	f := newStateFixture(t, false)
	st, rec := newStateTeam(t, f)
	id := pushedTeamWait(t, f, st, rec)
	f.advance(pushBroadcastTimeout + time.Second) // its push has been sent (clearEpisode holds a withdrawal inside that)
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE mirror_tokens SET revoked_at = now() WHERE owner_id = $1`, st.team); err != nil {
		t.Fatal(err)
	}
	f.statePass(t)
	f.srv.WaitForPushes()
	eps := f.teamEpisodes(t, st.team)
	if len(eps) != 1 || !eps[0].Ended {
		t.Fatalf("after the last token's revoke the team's episodes are %+v; want the one ended", eps)
	}
	tag := "attention-episode-" + strconv.FormatInt(id, 10)
	if got := rec.withdrawnFrom(tag, st.ana, st.ben); len(got) != 2 {
		t.Fatalf("withdrawn from %v; want ana and ben", got)
	}
}

// failingAccount is the users store with one id's account read failing.
type failingAccount struct {
	*users.PGStore
	id int64
}

func (u failingAccount) Account(ctx context.Context, id int64) (users.Account, error) {
	if id == u.id {
		return users.Account{}, errors.New("the account read failed")
	}
	return u.PGStore.Account(ctx, id)
}

// TestAFailedAccountReadNeverRunsATeamsPassAsAPerson (#690 round 1, 🟡4): the
// team's account read FAILS (a member principal, role uncertain, would be
// built for it). Its pass still runs as the team — no classifier — because
// the owner list, not a principal, says it is a team. CONTROL: the person's
// stop is classified (one call).
//
// 🔴 RED at 1181b8aa: the pass took team-ness from the principal, so the
// failed read ran the team's pass as a person's and classified its stop.
func TestAFailedAccountReadNeverRunsATeamsPassAsAPerson(t *testing.T) {
	f := newStateFixture(t, true)
	j := withJev(t, f.nudgeFixture)
	j.set(200, 0.81, 0.10, 0.09)
	st, _ := newStateTeam(t, f)
	tctx := authz.WithPrincipal(context.Background(), teamActingPrincipal(st.team, authz.ViaAttention))
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := agents.NewPG(f.pool).Create(tctx, agents.Agent{Name: "fachief-" + suffix, Namespace: "devpod-fachief-" + suffix,
		DisplayName: "chief", Status: agents.StatusRunning, HooksToken: "fa-chief-tok-" + suffix}); err != nil {
		t.Fatal(err)
	}
	f.srv.ext.Users = failingAccount{PGStore: users.NewPG(f.pool), id: st.team}
	f.statePass(t)
	if eps := f.teamEpisodes(t, st.team); len(eps) != 1 || eps[0].JevErr != jevNotConfigured {
		t.Fatalf("the team's episodes: %+v; want one, unclassified (%q)", eps, jevNotConfigured)
	}
	if n := f.jevCalls(j); n != 1 {
		t.Fatalf("classifier calls: %d; want 1 (the person's stop alone)", n)
	}
}

// TestATeamEpisodeOpensTheTeamSessionForItsOperators (#690 round 1, 🟡3): the
// fixture's operator (user 1) owns a "ws" of their own AND is an operator of
// the team, whose "ws" lists the same window, labelled "Team:1" there. The
// team's episode's tap opens the TEAM's window (owner=), and its Attention row
// is named over the team's hosts ("Team:1"), never the operator's own
// ("Gold:1"); a viewer (cal) is drawn no row of it.
//
// 🔴 RED at 1181b8aa: the tap fell back to the queue, the row was named over
// the member's own hosts, and every member saw it.
func TestATeamEpisodeOpensTheTeamSessionForItsOperators(t *testing.T) {
	f := newStateFixture(t, false)
	st, rec := newStateTeam(t, f)
	sys := authz.System(context.Background())
	if err := users.NewPG(f.pool).SetTeamMember(sys, users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}, st.team, authz.OperatorUserID, authz.TeamOperator); err != nil {
		t.Fatal(err)
	}
	id := pushedTeamWait(t, f, st, rec)
	f.mirror.mu.Lock()
	tw := append([]mirror.Window(nil), f.mirror.windows...)
	tw[0].Label = "Team:1"
	f.mirror.teamHosts = []mirror.HostView{{Owner: st.team, Host: "ws", Windows: tw, Online: true, Interactive: true, Rendered: true, StatusReport: true}}
	f.mirror.mu.Unlock()
	as := func(uid int64) context.Context {
		return authz.WithPrincipal(context.Background(), f.srv.principalFor(sys, uid, authz.ViaSession))
	}
	key := episodeKey(id)
	want := ui.MirrorRef{Owner: st.team, Host: "ws", Session: "main", Window: "@1"}.ViewPath()
	if got := f.srv.openTarget(as(authz.OperatorUserID), key); got != want {
		t.Errorf("the operator's tap on the team episode opens %q; want the team's window %q", got, want)
	}
	if got := f.srv.openTarget(as(st.ben), key); got != want {
		t.Errorf("ben's tap opens %q; want %q", got, want)
	}
	now := time.Now()
	octx := as(authz.OperatorUserID)
	rows, _ := f.srv.episodeRowsHTML(octx, f.srv.readAsks(octx), now)
	if !strings.Contains(rows, `id="`+key+`"`) || !strings.Contains(rows, "Team:1") {
		t.Errorf("the operator's rows do not draw the team episode named over the team's host:\n%s", rows)
	}
	if i := strings.Index(rows, `id="`+key+`"`); i >= 0 {
		row := rows[i:]
		if j := strings.Index(row, "</li>"); j >= 0 {
			row = row[:j]
		}
		if strings.Contains(row, "Gold:1") {
			t.Errorf("the team episode's row carries the operator's OWN window's label:\n%s", row)
		}
	}
	cctx := as(st.cal)
	if rows, _ := f.srv.episodeRowsHTML(cctx, f.srv.readAsks(cctx), now); strings.Contains(rows, `id="`+key+`"`) {
		t.Errorf("cal (viewer) is drawn the team episode's row:\n%s", rows)
	}
}
