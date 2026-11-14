package db_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/db"
)

// teamHostsVersion is 0133_team_hosts.sql's number.
// 🔴 RENUMBER IT WITH THE FILE (and db.teamHostsMigrationVersion) at merge.
const teamHostsVersion = 133

// TestMigration0133TeamHosts: just below 0133 team_members has no notify
// column, mirror_tokens no rotate_at, and officer_host_spawns only
// owner_isolation and team_lent_read (the control). 0133 adds notify (true
// for an EXISTING member and for a new one), rotate_at (NULL), and
// team_owner_read. The header's ROLLBACK runs as written and removes all
// three; the body applies again after it; --rls-off then --rls-on keeps
// team_owner_read (db.RLSOn re-applies 0133 after 0125 and 0130).
func TestMigration0133TeamHosts(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0133_team_hosts", teamHostsVersion-1)
	policies := func() string {
		rows, err := pool.Query(ctx, `SELECT policyname::text FROM pg_policies WHERE schemaname='public' AND tablename='officer_host_spawns'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			out = append(out, n)
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	columns := func() string {
		var out []string
		rows, err := pool.Query(ctx, `SELECT table_name || '.' || column_name FROM information_schema.columns
			WHERE table_schema='public' AND ((table_name='team_members' AND column_name='notify')
			   OR (table_name='mirror_tokens' AND column_name IN ('rotate_at', 'rotate_reason'))) ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			out = append(out, n)
		}
		return strings.Join(out, ",")
	}
	if got := policies(); got != "owner_isolation,team_lent_read" {
		t.Fatalf("control: below 0133 officer_host_spawns has policies %s", got)
	}
	if got := columns(); got != "" {
		t.Fatalf("control: below 0133 the columns %s already exist", got)
	}
	team := scanID(ctx, t, pool, `INSERT INTO users (kind, role, display_name) VALUES ('team', 'member', 'mig hosts team') RETURNING id`)
	before := scanID(ctx, t, pool, `INSERT INTO users (role, display_name) VALUES ('member', 'mig hosts before') RETURNING id`)
	after := scanID(ctx, t, pool, `INSERT INTO users (role, display_name) VALUES ('member', 'mig hosts after') RETURNING id`)
	mustExec(ctx, t, pool, `INSERT INTO team_members (team_id, user_id, role, added_by) VALUES ($1, $2, 'operator', $2)`, team, before)

	applyThroughFrom(ctx, t, pool, teamHostsVersion)

	if got, want := policies(), "owner_isolation,team_lent_read,team_owner_read"; got != want {
		t.Errorf("at 0133 officer_host_spawns has policies %s; want %s", got, want)
	}
	// rotate_reason is NOT added: the design derives the reason from the audit.
	if got, want := columns(), "mirror_tokens.rotate_at,team_members.notify"; got != want {
		t.Errorf("at 0133 the columns are %q; want %q", got, want)
	}
	mustExec(ctx, t, pool, `INSERT INTO team_members (team_id, user_id, role, added_by) VALUES ($1, $2, 'viewer', $2)`, team, after)
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM team_members WHERE team_id=$1 AND notify`, team).Scan(&n); err != nil || n != 2 {
		t.Errorf("after 0133, %d of the team's 2 members have notify on (%v); want both: an existing member's and a new one's default is on", n, err)
	}
	var nullRotate bool
	if err := pool.QueryRow(ctx, `SELECT is_nullable = 'YES' AND column_default IS NULL FROM information_schema.columns
		WHERE table_schema='public' AND table_name='mirror_tokens' AND column_name='rotate_at'`).Scan(&nullRotate); err != nil || !nullRotate {
		t.Errorf("mirror_tokens.rotate_at is not a nullable column with no default (%v)", err)
	}

	for _, q := range asksRollbackSQL(t, "0133_team_hosts.sql") {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	if got := policies(); got != "owner_isolation,team_lent_read" {
		t.Errorf("after the rollback officer_host_spawns has policies %s; want owner_isolation,team_lent_read", got)
	}
	if got := columns(); got != "" {
		t.Errorf("after the rollback the columns %s remain", got)
	}
	var fn int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_proc WHERE proname = 'muster_team_hosts_rollback'`).Scan(&fn); err != nil || fn != 0 {
		t.Errorf("after the rollback muster_team_hosts_rollback remains (%d, %v)", fn, err)
	}
	applyThroughFrom(ctx, t, pool, teamHostsVersion)
	if got := policies(); got != "owner_isolation,team_lent_read,team_owner_read" {
		t.Errorf("re-applied after the rollback: officer_host_spawns has policies %s", got)
	}
	if err := db.RLSOff(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := db.RLSOn(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if got := policies(); got != "owner_isolation,team_lent_read,team_owner_read" {
		t.Errorf("after --rls-off then --rls-on officer_host_spawns has policies %s; want team_owner_read kept", got)
	}
}
