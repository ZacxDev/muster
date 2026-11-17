package db_test

import (
	"strings"
	"testing"
)

// TestMigration0148KeepsAChatLanesMarks: below 0148 (at its predecessor,
// 0148) chat_sessions has no officer_chat_seen column (the control: the
// column this migration adds is the one the read names). After 0148 an
// existing session reads 0 and 0, a lane's marks are stored, the CHECK refuses
// an answered mark past the seen one and a negative one, and re-applying the
// file changes nothing. The header's rollback removes both columns.
func TestMigration0148KeepsAChatLanesMarks(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0148_chat_marks", 147)
	agent := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'officer-release-web-1', 'ns-officer') RETURNING id`)
	sess := scanID(ctx, t, pool, `INSERT INTO chat_sessions (owner_id, agent_id, session_key, title, runtime) VALUES (1, $1, 'officer-chat:c-41', 't', 'claude-code') RETURNING id`, agent)
	read := func() (seen, answered int64, err error) {
		err = pool.QueryRow(ctx, `SELECT officer_chat_seen, officer_chat_answered FROM chat_sessions WHERE id = $1`, sess).Scan(&seen, &answered)
		return seen, answered, err
	}
	if _, _, err := read(); err == nil || !strings.Contains(err.Error(), "officer_chat_seen") {
		t.Fatalf("control: before 0148 the marks read: %v; want an error naming officer_chat_seen", err)
	}

	applyThroughFrom(ctx, t, pool, 148)

	if s, a, err := read(); err != nil || s != 0 || a != 0 {
		t.Fatalf("an existing session after 0148: seen %d, answered %d, %v; want 0, 0", s, a, err)
	}
	set := func(seen, answered int64) error {
		_, err := pool.Exec(ctx, `UPDATE chat_sessions SET officer_chat_seen = $2, officer_chat_answered = $3 WHERE id = $1`, sess, seen, answered)
		return err
	}
	if err := set(7, 5); err != nil {
		t.Fatalf("marks 7/5: %v", err)
	}
	const constraint = "chat_sessions_officer_chat_marks"
	if err := set(4, 5); err == nil || !strings.Contains(err.Error(), constraint) {
		t.Fatalf("answered past seen: %v; want refused by %s", err, constraint)
	}
	if err := set(3, -1); err == nil || !strings.Contains(err.Error(), constraint) {
		t.Fatalf("a negative answered mark: %v; want refused by %s", err, constraint)
	}
	if _, err := pool.Exec(ctx, readMigration(t, "0148_officer_chat_marks.sql")); err != nil {
		t.Fatalf("re-applying 0148: %v", err)
	}
	if s, a, err := read(); err != nil || s != 7 || a != 5 {
		t.Fatalf("after re-applying 0148: seen %d, answered %d, %v; want 7, 5", s, a, err)
	}

	body := readMigration(t, "0148_officer_chat_marks.sql")
	var rollback []string
	in := false
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "-- ROLLBACK"):
			in = true
		case in && strings.HasPrefix(line, "--   "):
			rollback = append(rollback, strings.TrimPrefix(line, "--   "))
		case in && !strings.HasPrefix(line, "--"):
			in = false
		}
	}
	sql := strings.Join(rollback, "\n")
	if !strings.Contains(sql, "DROP COLUMN") || !strings.Contains(sql, "DELETE FROM schema_migrations WHERE version = 148") {
		t.Fatalf("instrument check: the header's rollback did not parse: %q", sql)
	}
	if _, err := pool.Exec(ctx, sql); err != nil {
		t.Fatalf("the header's rollback: %v\n%s", err, sql)
	}
	if _, _, err := read(); err == nil || !strings.Contains(err.Error(), "officer_chat_seen") {
		t.Fatalf("after the rollback the marks read: %v; want an error naming officer_chat_seen", err)
	}
}
