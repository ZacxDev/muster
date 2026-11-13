package db_test

import (
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

// TestMigration0125Teams: a database at 0124 has no users.kind and no
// team_members (the control); at 0125 every existing row is a person, a team
// row can be made, a membership must join a team to a person with a known
// role, and the team_members table is under forced row-level security. Then
// the header's ROLLBACK block runs as written and leaves neither, and the file
// applies again after it.
func TestMigration0125Teams(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0125_teams", 124)
	if _, err := pool.Exec(ctx, `SELECT kind FROM users`); err == nil {
		t.Fatal("control: before 0125 users.kind exists")
	}
	person := scanID(ctx, t, pool, `INSERT INTO users (role, email) VALUES ('member', 'ana@example.test') RETURNING id`)
	applyThroughFrom(ctx, t, pool, 125)

	var kinds string
	if err := pool.QueryRow(ctx, `SELECT string_agg(DISTINCT kind, ',') FROM users`).Scan(&kinds); err != nil || kinds != "person" {
		t.Fatalf("existing rows' kind = %q (%v); want every one a person", kinds, err)
	}
	team := scanID(ctx, t, pool, `INSERT INTO users (kind, role, display_name) VALUES ('team', 'member', 'Platform') RETURNING id`)
	other := scanID(ctx, t, pool, `INSERT INTO users (kind, role, display_name) VALUES ('team', 'member', 'Other') RETURNING id`)
	mustExec(ctx, t, pool, `INSERT INTO team_members (team_id, user_id, role, added_by) VALUES ($1, $2, 'viewer', 1)`, team, person)

	for name, q := range map[string]string{
		"a duplicate team name":   `INSERT INTO users (kind, role, display_name) VALUES ('team', 'member', ' platform ')`,
		"a team with no name":     `INSERT INTO users (kind, role, display_name) VALUES ('team', 'member', '')`,
		"an unknown kind":         `INSERT INTO users (kind, role) VALUES ('robot', 'member')`,
		"a team as admin":         `INSERT INTO users (kind, role, display_name) VALUES ('team', 'admin', 'A')`,
		"a team with an email":    `INSERT INTO users (kind, role, display_name, email) VALUES ('team', 'member', 'B', 'b@example.test')`,
		"a team as a member":      `INSERT INTO team_members (team_id, user_id, role, added_by) VALUES (` + itoa(team) + `, ` + itoa(other) + `, 'viewer', 1)`,
		"a person as the team":    `INSERT INTO team_members (team_id, user_id, role, added_by) VALUES (` + itoa(person) + `, 1, 'viewer', 1)`,
		"an unknown role":         `INSERT INTO team_members (team_id, user_id, role, added_by) VALUES (` + itoa(team) + `, 1, 'owner', 1)`,
		"the same member twice":   `INSERT INTO team_members (team_id, user_id, role, added_by) VALUES (` + itoa(team) + `, ` + itoa(person) + `, 'manager', 1)`,
		"a membership of nobody":  `INSERT INTO team_members (team_id, user_id, role, added_by) VALUES (` + itoa(team) + `, 999999, 'viewer', 1)`,
		"re-pointing to a team":   `UPDATE team_members SET user_id = ` + itoa(other) + ` WHERE team_id = ` + itoa(team),
		"a person becoming team":  `UPDATE users SET kind = 'team', display_name = 'X' WHERE id = ` + itoa(person),
		"a team becoming person":  `UPDATE users SET kind = 'person' WHERE id = ` + itoa(team),
		"a team signing in later": `UPDATE users SET gotrue_sub = gen_random_uuid() WHERE id = ` + itoa(team),
	} {
		if _, err := pool.Exec(ctx, q); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// Control: a second person joins, and the first is re-roled.
	mustExec(ctx, t, pool, `INSERT INTO team_members (team_id, user_id, role, added_by) VALUES ($1, 1, 'manager', 1)`, team)
	mustExec(ctx, t, pool, `UPDATE team_members SET role = 'operator' WHERE team_id = $1 AND user_id = $2`, team, person)

	var rls, forced bool
	if err := pool.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'team_members'`).Scan(&rls, &forced); err != nil || !rls || !forced {
		t.Fatalf("team_members row-level security enabled=%v forced=%v err=%v; want both", rls, forced, err)
	}

	for _, q := range asksRollbackSQL(t, "0125_teams.sql") {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	if _, err := pool.Exec(ctx, `SELECT 1 FROM team_members`); err == nil {
		t.Error("after the rollback team_members still exists")
	}
	if _, err := pool.Exec(ctx, `SELECT kind FROM users`); err == nil {
		t.Error("after the rollback users.kind still exists")
	}
	applyThroughFrom(ctx, t, pool, 125)
	if n := countRows(ctx, t, pool, "team_members"); n != 0 {
		t.Errorf("re-applied after the rollback: %d membership row(s)", n)
	}
}

// TestATeamMemberSeesNoTeamRowInAnyOwnedTable is phase 1's "nothing is
// shared" claim, over EVERY owned table (rlsTables, which
// TestRLSCoversExactlyTheOwnedTables pins to the set of tables with an
// owner_id column, so a new owned table is covered without anyone adding it
// here). A team writes one row into each; a person who is the team's MANAGER
// — membership read through users.Account, as every request reads it — sees
// none of them, and can neither change nor delete them. Controls: a principal
// that IS the team sees each row (so "none" is about the member, not an empty
// table), and System sees each row.
func TestATeamMemberSeesNoTeamRowInAnyOwnedTable(t *testing.T) {
	f := newRLSFixture(t, "ms_0125_nothing_shared")
	sys := authz.System(f.base)
	store := users.NewPG(f.pool)
	team, err := store.CreateTeam(f.base, users.Actor{UserID: 1, Via: authz.ViaSession}, "Platform", f.two)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := store.Account(sys, f.two)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := acct.Teams.RoleIn(team.ID); !ok || r != authz.TeamManager {
		t.Fatalf("fixture: the member's account says %q; want manager of team %d", acct.Teams, team.ID)
	}
	member := authz.WithPrincipal(f.base, authz.Principal{UserID: f.two, Role: acct.Role, Via: authz.ViaSession, Teams: acct.Teams})
	// The team's own principal is built by hand: no request may act as a team
	// (WithPrincipal refuses Team: true), but the database scope is what is
	// under test, and it keys on the id alone.
	asTeam := authz.WithPrincipal(f.base, authz.Principal{UserID: team.ID, Role: authz.RoleMember, Via: authz.ViaSession})
	if err := writeEveryOwnedTable(asTeam, f.pool, "team"); err != nil {
		t.Fatalf("the team's writes: %v", err)
	}
	tables := rlsTables()
	if len(tables) < 60 {
		t.Fatalf("rlsTables has %d tables; the ledger is reading nothing", len(tables))
	}
	for _, tbl := range tables {
		owned := count(t, f.mig, f.pool, tbl, "WHERE owner_id = $1", team.ID)
		if owned == 0 {
			t.Errorf("fixture: the team owns no row in %s", tbl)
			continue
		}
		if got := count(t, asTeam, f.pool, tbl, "WHERE owner_id = $1", team.ID); got != owned {
			t.Errorf("control: the team itself sees %d of its %d rows in %s", got, owned, tbl)
		}
		if got := count(t, sys, f.pool, tbl, "WHERE owner_id = $1", team.ID); got != owned {
			t.Errorf("control: System sees %d of the team's %d rows in %s", got, owned, tbl)
		}
		if got := count(t, member, f.pool, tbl, "WHERE owner_id = $1", team.ID); got != 0 {
			t.Errorf("SHARED: the team's manager sees %d of the team's rows in %s; phase 1 shares nothing", got, tbl)
		}
		if tag, err := f.pool.Exec(member, `UPDATE `+tbl+` SET owner_id = owner_id WHERE owner_id = $1`, team.ID); err != nil || tag.RowsAffected() != 0 {
			t.Errorf("the team's manager UPDATE of the team's %s rows: %d rows, %v; want 0", tbl, tag.RowsAffected(), err)
		}
		if tag, err := f.pool.Exec(member, `DELETE FROM `+tbl+` WHERE owner_id = $1`, team.ID); err != nil || tag.RowsAffected() != 0 {
			t.Errorf("the team's manager DELETE of the team's %s rows: %d rows, %v; want 0", tbl, tag.RowsAffected(), err)
		}
		if got := count(t, f.mig, f.pool, tbl, "WHERE owner_id = $1", team.ID); got != owned {
			t.Errorf("%s: the team owns %d rows after its manager's attempts, %d before", tbl, got, owned)
		}
	}
	// Nor can the manager write a row AS the team (WITH CHECK).
	_, err = f.pool.Exec(member, `INSERT INTO notes (title, owner_id) VALUES ('forged', $1)`, team.ID)
	assertSQLState(t, "the team's manager inserting a team-owned task", err, "42501")
}
