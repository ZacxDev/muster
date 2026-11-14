package db_test

import "testing"

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

// (Phase 1's TestATeamMemberSeesNoTeamRowInAnyOwnedTable became phase 2a's
// share ledger: TestEveryOwnedTableHasAShareClass and
// TestTheShareLedgerHoldsForEveryRole, in share_ledger_test.go.)
