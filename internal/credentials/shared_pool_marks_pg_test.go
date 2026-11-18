package credentials

import (
	"context"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ccpool"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestARateLimitIsTheAccountsNotTheOwners is the team-sharing phase 4
// acceptance bug: a team's first agent on the server pool was placed on an
// account the admin's agents had already found exhausted, then failed over onto
// a second exhausted one — the marks were per owner, and row-level security
// hid the admin's from the team acting as itself. A rate limit is a property of
// the ACCOUNT: every owner on the shared pool sees every mark on it, whoever
// recorded it, through the REAL store under the REAL principals.
//
//   - the admin's mark on acct-a: the team's selection skips acct-a, and its
//     failover from acct-b skips it too;
//   - the team's own mark on acct-c: the admin's failover from acct-b finds
//     nothing usable (both others cooling), and no mark row is written under
//     the team's id; the team's cooldown read and clear act on that one mark
//     (the admin then finds acct-c again);
//   - a member's OWN account named acct-a is a different subscription: the
//     pool's mark does not push it down, and the member's own mark stays the
//     member's (an invariant guard: green before the fix too).
//
// The control: read as the team acting principal, the store itself returns
// none of the admin's marks — row-level security is what the fix crosses.
//
// 🔴 RED at origin/main 797e8bb5: the team selects acct-a, fails over onto it,
// and its mark lands under its own id, unseen by the admin.
func TestARateLimitIsTheAccountsNotTheOwners(t *testing.T) {
	const team, member = int64(9301), int64(9302)
	const tokA, tokB, tokC = "sk-ant-oat01-FAKE-shared-a-1234567", "sk-ant-oat01-FAKE-shared-b-7654321", "sk-ant-oat01-FAKE-shared-c-1357913"
	const memTokA, memTokZ = "sk-ant-oat01-FAKE-own-a-246802468", "sk-ant-oat01-FAKE-own-z-864208642"
	_, pg := pgStore(t)
	dsn := dbtest.DSN(t)
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role, kind, display_name) OVERRIDING SYSTEM VALUE VALUES ($1, 'member', 'team', 'shared pool team') ON CONFLICT (id) DO NOTHING`, team)
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES ($1, 'member') ON CONFLICT (id) DO NOTHING`, member)
	dbtest.OwnerExec(t, dsn, `DELETE FROM cc_account_marks`)
	dbtest.OwnerExec(t, dsn, `DELETE FROM agents WHERE owner_id IN (1, $1, $2)`, team, member)

	env, err := ccpool.NewStatic(map[string]string{"acct-a": tokA, "acct-b": tokB, "acct-c": tokC})
	if err != nil {
		t.Fatal(err)
	}
	plane := &Plane{EnvClaude: env, Roles: teamRoles{}, TeamsOnServerPool: map[int64]bool{team: true},
		Personal: fakePersonal{claude: map[int64]map[string]string{member: {"acct-a": memTokA, "acct-z": memTokZ}}}}
	store := ccpool.NewPG(pg)
	pool, err := ccpool.New(plane, store)
	if err != nil {
		t.Fatal(err)
	}
	adminCtx := as(authz.OperatorUserID)
	teamCtx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: team, Team: true, Role: authz.RoleMember, Via: authz.ViaAgent})
	memberCtx := as(member)

	if err := pool.MarkFailure(adminCtx, authz.OperatorUserID, "acct-a", ccpool.FailureRateLimited, "weekly limit reached"); err != nil {
		t.Fatal(err)
	}
	// The control: what the team's principal reads of the admin's row itself.
	if got, err := store.Marks(teamCtx, authz.OperatorUserID); err != nil || len(got) != 0 {
		t.Fatalf("control: the team principal read %d of the admin's marks directly (%v); row-level security should hide them", len(got), err)
	}

	if got, err := pool.Select(teamCtx, team, ""); err != nil || got != "acct-b" {
		t.Errorf("the team's selection = %q (%v); want acct-b — acct-a is rate-limited, whoever recorded it", got, err)
	}
	if got, ok, err := pool.FailoverTarget(teamCtx, team, "acct-b"); err != nil || !ok || got != "acct-c" {
		t.Errorf("the team's failover from acct-b = %q, %v (%v); want acct-c, never the exhausted acct-a", got, ok, err)
	}

	// The other direction: the team's mark is the admin's to see.
	if err := pool.MarkFailure(teamCtx, team, "acct-c", ccpool.FailureRateLimited, "session limit reached"); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := pool.FailoverTarget(adminCtx, authz.OperatorUserID, "acct-b"); err != nil || ok {
		t.Errorf("the admin's failover from acct-b = %q, %v (%v); want none — acct-a and acct-c are both rate-limited", got, ok, err)
	}
	// The live reading's clear, asked for the team, clears the pool's mark:
	// the admin's failover from acct-b then finds acct-c again.
	cd, err := pool.Cooldown(teamCtx, team, "acct-c")
	if err != nil || cd.MarkedAt.IsZero() {
		t.Fatalf("the team reads acct-c's cooldown as %+v (%v); want the mark it just recorded", cd, err)
	}
	if ok, err := pool.ClearRateLimit(teamCtx, team, "acct-c", cd.MarkedAt); err != nil || !ok {
		t.Errorf("the team's clear of acct-c's mark: %v (%v); want cleared", ok, err)
	}
	if got, ok, err := pool.FailoverTarget(adminCtx, authz.OperatorUserID, "acct-b"); err != nil || !ok || got != "acct-c" {
		t.Errorf("after the clear, the admin's failover from acct-b = %q, %v (%v); want acct-c", got, ok, err)
	}
	var underTeam int
	if err := pg.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM cc_account_marks WHERE owner_id = $1`, team).Scan(&underTeam); err != nil {
		t.Fatal(err)
	}
	if underTeam != 0 {
		t.Errorf("%d mark rows were written under the team's id; a shared pool account's mark is kept once, under the pool", underTeam)
	}

	// A member's own acct-a is another subscription.
	if got, err := pool.Select(memberCtx, member, ""); err != nil || got != "acct-a" {
		t.Errorf("the member's selection = %q (%v); want their own acct-a — the pool's mark is not theirs", got, err)
	}
	if err := pool.MarkFailure(memberCtx, member, "acct-z", ccpool.FailureRateLimited, "member limit"); err != nil {
		t.Fatal(err)
	}
	var memberRows, poolZ int
	if err := pg.QueryRow(authz.System(context.Background()), `SELECT count(*) FILTER (WHERE owner_id = $1),
		count(*) FILTER (WHERE owner_id = 1 AND account = 'acct-z') FROM cc_account_marks`, member).Scan(&memberRows, &poolZ); err != nil {
		t.Fatal(err)
	}
	if memberRows != 1 || poolZ != 0 {
		t.Errorf("the member's mark: %d rows under the member, %d under the pool; want 1 and 0", memberRows, poolZ)
	}
}
