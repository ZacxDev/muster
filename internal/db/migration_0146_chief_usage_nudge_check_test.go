package db_test

import (
	"strings"
	"testing"
)

// TestMigration0146CountsStallChecks: below 0146 (at 0144, the version this
// branch was cut from; 0145 is another change's) chief_turn_usage refuses the
// trigger 'nudge_check' (the control: the CHECK this migration widens is the
// one that refuses it). After 0146 it stores one, still refuses a trigger no
// door writes, keeps every row it held, and re-applying the file changes
// nothing. The header's rollback restores the narrower check.
func TestMigration0146CountsStallChecks(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0146_nudge_check", 144)
	insert := func(trigger string) error {
		_, err := pool.Exec(ctx, `INSERT INTO chief_turn_usage (owner_id, chief_agent, trigger, outcome) VALUES (1, 7, $1, 'other')`, trigger)
		return err
	}
	const constraint = "chief_turn_usage_trigger_check"
	if err := insert("nudge_check"); err == nil || !strings.Contains(err.Error(), constraint) {
		t.Fatalf("control: before 0146 a nudge_check row: %v; want refused by %s", err, constraint)
	}
	if err := insert("kickoff"); err != nil {
		t.Fatalf("a kickoff row before 0146: %v", err)
	}

	applyThroughFrom(ctx, t, pool, 146)

	if err := insert("nudge_check"); err != nil {
		t.Fatalf("after 0146 a nudge_check row: %v; want stored", err)
	}
	if err := insert("nudge"); err == nil || !strings.Contains(err.Error(), constraint) {
		t.Fatalf("after 0146 an unknown trigger: %v; want refused by %s", err, constraint)
	}
	count := func() (kick, check int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE trigger = 'kickoff'), count(*) FILTER (WHERE trigger = 'nudge_check')
			FROM chief_turn_usage`).Scan(&kick, &check); err != nil {
			t.Fatal(err)
		}
		return kick, check
	}
	if k, c := count(); k != 1 || c != 1 {
		t.Fatalf("rows after 0146: kickoff %d, nudge_check %d; want 1 and 1", k, c)
	}
	if _, err := pool.Exec(ctx, readMigration(t, "0146_chief_usage_nudge_check.sql")); err != nil {
		t.Fatalf("re-applying 0146: %v", err)
	}
	if k, c := count(); k != 1 || c != 1 {
		t.Fatalf("rows after re-applying 0146: kickoff %d, nudge_check %d; want 1 and 1", k, c)
	}

	// The header's rollback, statement for statement.
	body := readMigration(t, "0146_chief_usage_nudge_check.sql")
	var rollback []string
	in := false
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "-- ROLLBACK."):
			in = true
		case in && strings.HasPrefix(line, "--   "):
			rollback = append(rollback, strings.TrimPrefix(line, "--   "))
		case in && !strings.HasPrefix(line, "--"):
			in = false
		}
	}
	sql := strings.Join(rollback, "\n")
	if !strings.Contains(sql, "DROP CONSTRAINT") || !strings.Contains(sql, "DELETE FROM schema_migrations WHERE version = 146") {
		t.Fatalf("instrument check: the header's rollback did not parse: %q", sql)
	}
	if _, err := pool.Exec(ctx, sql); err != nil {
		t.Fatalf("the header's rollback: %v\n%s", err, sql)
	}
	if err := insert("nudge_check"); err == nil || !strings.Contains(err.Error(), constraint) {
		t.Fatalf("after the rollback a nudge_check row: %v; want refused by %s", err, constraint)
	}
	if k, c := count(); k != 1 || c != 0 {
		t.Fatalf("rows after the rollback: kickoff %d, nudge_check %d; want 1 and 0", k, c)
	}
}
