package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

// Phase 3 of docs/design/team-sharing.md (migration 0129): team officers.

// hintOf is err's Postgres SQLSTATE and HINT.
func hintOf(err error) (code, hint string) {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code, pg.Hint
	}
	return "", ""
}

// teamActing is the team acting principal for team through via (a team
// officer's runtime, a team-owned agent).
func teamActing(f *rlsFixture, team int64, via authz.Via) context.Context {
	return authz.WithPrincipal(f.base, authz.Principal{UserID: team, Team: true, Role: authz.RoleMember, Via: via})
}

// lendFixture is a team T with operator op (who owns a spawn), viewer v and
// manager m; a second team T2 where op is only a viewer; and outsider o.
type lendFixture struct {
	f                    *rlsFixture
	store                *users.PGStore
	team, team2          int64
	op, viewer, mgr, out int64
	spawn                int64
}

func newLendFixture(t *testing.T, name string) *lendFixture {
	t.Helper()
	f := newRLSFixture(t, name)
	l := &lendFixture{f: f, store: users.NewPG(f.pool)}
	by := users.Actor{UserID: 1, Via: authz.ViaSession}
	for _, p := range []*int64{&l.op, &l.viewer, &l.mgr, &l.out} {
		*p = scanID(f.mig, t, f.pool, `INSERT INTO users (role, display_name) VALUES ('member', 'p') RETURNING id`)
	}
	tm, err := l.store.CreateTeam(f.base, by, "Platform", l.mgr)
	if err != nil {
		t.Fatal(err)
	}
	tm2, err := l.store.CreateTeam(f.base, by, "Growth", l.mgr)
	if err != nil {
		t.Fatal(err)
	}
	l.team, l.team2 = tm.ID, tm2.ID
	for _, c := range []struct {
		team, user int64
		role       authz.TeamRole
	}{{l.team, l.op, authz.TeamOperator}, {l.team, l.viewer, authz.TeamViewer}, {l.team2, l.op, authz.TeamViewer}} {
		if err := l.store.SetTeamMember(f.base, by, c.team, c.user, c.role); err != nil {
			t.Fatal(err)
		}
	}
	l.spawn = scanID(l.as(t, l.op), t, f.pool, `INSERT INTO officer_host_spawns (officer, instance, host)
		VALUES ('marketing:example', 'officer-marketing-example-1', 'build-01') RETURNING id`)
	return l
}

// as is uid's web session principal, with the memberships users.Account reads.
func (l *lendFixture) as(t *testing.T, uid int64) context.Context {
	return member(t, l.f, uid, authz.ViaSession)
}

func (l *lendFixture) sees(t *testing.T, ctx context.Context) int {
	return count(t, ctx, l.f.pool, "officer_host_spawns", "WHERE id = $1", l.spawn)
}

func (l *lendFixture) lend(ctx context.Context, team any) (int64, error) {
	tag, err := l.f.pool.Exec(ctx, `UPDATE officer_host_spawns SET shared_team_id = $2 WHERE id = $1`, l.spawn, team)
	return tag.RowsAffected(), err
}

// TestALentSpawnIsReadByItsTeamOnly: a spawn is its owner's alone until they
// lend it; lent, the team's members read it through a person's own doors
// (session, phone) and NO machine door; an outsider never does; no member but
// the owner writes it; unlent, it is the owner's alone again. CONTROLS: the
// owner reads it throughout, and System sees it.
func TestALentSpawnIsReadByItsTeamOnly(t *testing.T) {
	l := newLendFixture(t, "ms_0129_lent_read")
	opCtx, vCtx, mCtx, oCtx := l.as(t, l.op), l.as(t, l.viewer), l.as(t, l.mgr), l.as(t, l.out)
	check := func(stage string, want map[string]int) {
		t.Helper()
		for who, c := range map[string]context.Context{"owner": opCtx, "viewer": vCtx, "manager": mCtx, "outsider": oCtx} {
			if got := l.sees(t, c); got != want[who] {
				t.Errorf("%s: the %s sees %d of the spawn; want %d", stage, who, got, want[who])
			}
		}
	}
	check("before the lend", map[string]int{"owner": 1})
	if n, err := l.lend(opCtx, l.team); err != nil || n != 1 {
		t.Fatalf("the owner (an operator) lending it to the team: %d rows, %v", n, err)
	}
	check("lent", map[string]int{"owner": 1, "viewer": 1, "manager": 1})
	if got := l.sees(t, member(t, l.f, l.viewer, authz.ViaDevice)); got != 1 {
		t.Errorf("the team's viewer on their paired phone sees %d; want 1 (a person's own door)", got)
	}
	for _, via := range []authz.Via{authz.ViaToken, authz.ViaAgent, authz.ViaMirrorHost, authz.ViaOfficer, authz.ViaConnect,
		authz.ViaRelay, authz.ViaHook, authz.ViaQueue, authz.ViaAttention} {
		if got := l.sees(t, member(t, l.f, l.mgr, via)); got != 0 {
			t.Errorf("the team's manager through the %s door sees %d of the lent spawn; want 0", via, got)
		}
	}
	// No member but the owner writes it (owner_isolation): an update or a
	// delete finds no row, and re-lending it is not theirs to do.
	for who, c := range map[string]context.Context{"viewer": vCtx, "manager": mCtx} {
		if n, err := rolledBack(c, l.f, `UPDATE officer_host_spawns SET refusal = 'x' WHERE id = $1`, l.spawn); err != nil || n != 0 {
			t.Errorf("the team's %s updating the lent spawn: %d rows, %v; want 0", who, n, err)
		}
		if n, err := rolledBack(c, l.f, `UPDATE officer_host_spawns SET shared_team_id = NULL WHERE id = $1`, l.spawn); err != nil || n != 0 {
			t.Errorf("the team's %s unlending the owner's spawn: %d rows, %v; want 0", who, n, err)
		}
	}
	if got := count(t, authz.System(l.f.base), l.f.pool, "officer_host_spawns", "WHERE id = $1 AND shared_team_id = $2", l.spawn, l.team); got != 1 {
		t.Fatalf("control: System reads the lend on %d rows; want 1", got)
	}
	if n, err := l.lend(opCtx, nil); err != nil || n != 1 {
		t.Fatalf("the owner unlending it: %d rows, %v", n, err)
	}
	check("unlent", map[string]int{"owner": 1})
}

// TestOnlyTheOwnerLendsAndOnlyToATeamTheyOperate: lend_target refuses
// (42501, its hint) a lend to a team the owner is only a viewer of, to a team
// they are not in, and a row filed already lent to such a team; the team's
// MANAGER cannot lend the operator's row at all (it is not theirs). CONTROLS:
// the owner's lend to the team they operate succeeds, and System may set any.
func TestOnlyTheOwnerLendsAndOnlyToATeamTheyOperate(t *testing.T) {
	l := newLendFixture(t, "ms_0129_lend_target")
	opCtx := l.as(t, l.op)
	other, err := l.store.CreateTeam(l.f.base, users.Actor{UserID: 1, Via: authz.ViaSession}, "Elsewhere", l.mgr)
	if err != nil {
		t.Fatal(err)
	}
	for what, team := range map[string]int64{"a team they only view": l.team2, "a team they are not in": other.ID} {
		_, err := l.lend(opCtx, team)
		if code, hint := hintOf(err); code != "42501" || hint != "muster:lend-target" {
			t.Errorf("the owner lending to %s: %v (code %q, hint %q); want lend_target's 42501", what, err, code, hint)
		}
	}
	_, err = l.f.pool.Exec(opCtx, `INSERT INTO officer_host_spawns (officer, instance, host, shared_team_id)
		VALUES ('marketing:example', 'officer-marketing-example-2', 'build-01', $1)`, l.team2)
	if code, hint := hintOf(err); code != "42501" || hint != "muster:lend-target" {
		t.Errorf("filing a spawn already lent to a team the owner only views: %v; want lend_target's 42501", err)
	}
	if n, err := l.lend(l.as(t, l.mgr), l.team); err != nil || n != 0 {
		t.Errorf("the team's manager lending the operator's spawn: %d rows, %v; want 0 (not theirs)", n, err)
	}
	// The team acting principal (a team officer's runtime) lends nothing: it
	// is nobody's member.
	teamSpawn := scanID(teamActing(l.f, l.team, authz.ViaOfficer), t, l.f.pool, `INSERT INTO officer_host_spawns (officer, instance, host)
		VALUES ('marketing:example', 'officer-marketing-example-3', 'build-01') RETURNING id`)
	_, err = l.f.pool.Exec(teamActing(l.f, l.team, authz.ViaOfficer), `UPDATE officer_host_spawns SET shared_team_id = $2 WHERE id = $1`, teamSpawn, l.team)
	if code, hint := hintOf(err); code != "42501" || hint != "muster:lend-target" {
		t.Errorf("a team lending its own spawn: %v; want lend_target's 42501", err)
	}
	if n, err := l.lend(opCtx, l.team); err != nil || n != 1 {
		t.Errorf("control: the owner lending to the team they operate: %d rows, %v", n, err)
	}
	if n, err := l.lend(authz.System(l.f.base), other.ID); err != nil || n != 1 {
		t.Errorf("control: System setting a lend: %d rows, %v", n, err)
	}
}

// TestLeavingOrDroppingToViewerEndsALend: internal/users takes a person's
// lends to a team back in the same transaction that removes them from it or
// re-roles them to viewer; a re-role that keeps them an operator or manager
// keeps the lend (the control).
func TestLeavingOrDroppingToViewerEndsALend(t *testing.T) {
	by := users.Actor{UserID: 1, Via: authz.ViaSession}
	lentTo := func(t *testing.T, l *lendFixture) int64 {
		var team *int64
		if err := l.f.pool.QueryRow(l.f.mig, `SELECT shared_team_id FROM officer_host_spawns WHERE id = $1`, l.spawn).Scan(&team); err != nil {
			t.Fatal(err)
		}
		if team == nil {
			return 0
		}
		return *team
	}
	for what, c := range map[string]struct {
		change func(l *lendFixture) error
		want   func(l *lendFixture) int64
	}{
		"removed from the team": {func(l *lendFixture) error { return l.store.RemoveTeamMember(l.f.base, by, l.team, l.op) },
			func(*lendFixture) int64 { return 0 }},
		"re-roled to viewer": {func(l *lendFixture) error { return l.store.SetTeamMember(l.f.base, by, l.team, l.op, authz.TeamViewer) },
			func(*lendFixture) int64 { return 0 }},
		"re-roled to manager (control)": {func(l *lendFixture) error {
			return l.store.SetTeamMember(l.f.base, by, l.team, l.op, authz.TeamManager)
		},
			func(l *lendFixture) int64 { return l.team }},
	} {
		t.Run(what, func(t *testing.T) {
			l := newLendFixture(t, "ms_0129_end_lend")
			if n, err := l.lend(l.as(t, l.op), l.team); err != nil || n != 1 {
				t.Fatalf("fixture: lend: %d, %v", n, err)
			}
			if err := c.change(l); err != nil {
				t.Fatal(err)
			}
			if got, want := lentTo(t, l), c.want(l); got != want {
				t.Errorf("after being %s the spawn is lent to %d; want %d", what, got, want)
			}
		})
	}
}

// TestNobodyApprovesTheirOwnTeamDraft: draft_approver refuses (42501, its
// hint) the TEAM approving a draft it owns (an update to approved) and a
// manager approving a team draft they caused (requested_by_user). A draft
// FILED approved is NOT this guard's (a standing approval, #668 round 0 D2):
// the team filing one is admitted. CONTROLS: another manager approves the
// team draft the team's officer filed, and a person approves their own
// personal draft as today.
func TestNobodyApprovesTheirOwnTeamDraft(t *testing.T) {
	f := newRLSFixture(t, "ms_0129_approver")
	store := users.NewPG(f.pool)
	by := users.Actor{UserID: 1, Via: authz.ViaSession}
	m2 := scanID(f.mig, t, f.pool, `INSERT INTO users (role, display_name) VALUES ('member', 'm2') RETURNING id`)
	team, err := store.CreateTeam(f.base, by, "Platform", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetTeamMember(f.base, by, team.ID, m2, authz.TeamManager); err != nil {
		t.Fatal(err)
	}
	asTeam := teamActing(f, team.ID, authz.ViaOfficer)
	if err := writeEveryOwnedTable(asTeam, f.pool, "approver"); err != nil {
		t.Fatal(err)
	}
	sys := authz.System(f.base)
	draft := scanID(sys, t, f.pool, `SELECT id FROM plugin_reply_drafts WHERE owner_id = $1 AND status = 'pending' LIMIT 1`, team.ID)
	var requester *int64
	if err := f.pool.QueryRow(sys, `SELECT requested_by_user FROM plugin_reply_drafts WHERE id = $1`, draft).Scan(&requester); err != nil {
		t.Fatal(err)
	}
	if requester == nil || *requester != team.ID {
		t.Fatalf("the team officer's draft records requested_by_user %v; want the team %d", requester, team.ID)
	}
	approve := `UPDATE plugin_reply_drafts SET status = 'approved', decided_at = now(), approved_at = now() WHERE id = $1`
	isApprover := func(what string, err error) {
		t.Helper()
		if code, hint := hintOf(err); code != "42501" || hint != "muster:draft-approver" {
			t.Errorf("%s: %v (code %q, hint %q); want draft_approver's 42501", what, err, code, hint)
		}
	}
	_, err = rolledBack(asTeam, f, approve, draft)
	isApprover("the team approving its own officer's draft", err)
	_, err = rolledBack(asTeam, f, `INSERT INTO plugin_reply_drafts (message_id, agent_id, agent_name, body, expires_at, status, decided_at, approved_at)
		SELECT message_id, agent_id, agent_name, 'born approved', expires_at, 'approved', now(), now() FROM plugin_reply_drafts WHERE id = $1`, draft)
	if err != nil {
		t.Errorf("the team filing a draft already approved (a standing approval, not a decision): %v; want it admitted", err)
	}
	// A manager who CAUSED the draft (set by the login role, as a person's
	// routed session would have stamped it).
	mustExec(f.mig, t, f.pool, `UPDATE plugin_reply_drafts SET requested_by_user = $2 WHERE id = $1`, draft, m2)
	_, err = rolledBack(member(t, f, m2, authz.ViaSession), f, approve, draft)
	isApprover("a manager approving the team draft they caused", err)
	if n, err := rolledBack(member(t, f, 1, authz.ViaSession), f, approve, draft); err != nil || n != 1 {
		t.Errorf("control: another manager approving it: %d rows, %v", n, err)
	}
	// A person's own personal draft, approved by them (requested by them).
	own := member(t, f, f.two, authz.ViaSession)
	if err := writeEveryOwnedTable(own, f.pool, "approver-own"); err != nil {
		t.Fatal(err)
	}
	mine := scanID(sys, t, f.pool, `SELECT id FROM plugin_reply_drafts WHERE owner_id = $1 AND status = 'pending' LIMIT 1`, f.two)
	if n, err := rolledBack(own, f, approve, mine); err != nil || n != 1 {
		t.Errorf("control: a person approving their own personal draft: %d rows, %v", n, err)
	}
	// Rejecting stays open to the team's managers and is not this guard's.
	if n, err := rolledBack(member(t, f, m2, authz.ViaSession), f, `UPDATE plugin_reply_drafts SET status = 'rejected', decided_at = now() WHERE id = $1`, draft); err != nil || n != 1 {
		t.Errorf("control: the requesting manager rejecting it: %d rows, %v", n, err)
	}
}

// TestRequestedByUserCannotBeForged: requested_by_user is the acting
// principal's id on insert, whatever the statement wrote, and an update that
// rewrites it is refused (0127's muster_actor_column). CONTROL: the login role
// may write it (a restore).
func TestRequestedByUserCannotBeForged(t *testing.T) {
	f := newRLSFixture(t, "ms_0129_forge")
	own := member(t, f, f.two, authz.ViaSession)
	if err := writeEveryOwnedTable(own, f.pool, "forge-req"); err != nil {
		t.Fatal(err)
	}
	sys := authz.System(f.base)
	d := scanID(sys, t, f.pool, `SELECT id FROM plugin_reply_drafts WHERE owner_id = $1 LIMIT 1`, f.two)
	// Filed rejected: a message holds one pending draft at a time.
	forged := scanID(own, t, f.pool, `INSERT INTO plugin_reply_drafts (message_id, agent_id, agent_name, body, expires_at, status, decided_at, requested_by_user)
		SELECT message_id, agent_id, agent_name, 'forged', expires_at, 'rejected', now(), 1 FROM plugin_reply_drafts WHERE id = $1 RETURNING id`, d)
	var got *int64
	if err := f.pool.QueryRow(sys, `SELECT requested_by_user FROM plugin_reply_drafts WHERE id = $1`, forged).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != f.two {
		t.Errorf("a forged requested_by_user stored %v; want the acting person %d", got, f.two)
	}
	_, err := f.pool.Exec(own, `UPDATE plugin_reply_drafts SET requested_by_user = 1 WHERE id = $1`, forged)
	assertSQLState(t, "rewriting requested_by_user", err, "42501")
	if _, err := f.pool.Exec(f.mig, `UPDATE plugin_reply_drafts SET requested_by_user = 1 WHERE id = $1`, forged); err != nil {
		t.Errorf("control: the login role rewriting requested_by_user: %v", err)
	}
}
