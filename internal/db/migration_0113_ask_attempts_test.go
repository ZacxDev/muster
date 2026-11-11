package db_test

import (
	"strings"
	"testing"
)

// TestMigration0113AskAttempts: a database at 0112 has no ask_attempts; at
// 0113 it does, empty, and its CHECKs refuse what the store never writes — an
// unknown door, runtime, method or outcome, a reason that is not a code, a
// shape whose arrays do not match its question count, a snapshot over the
// cap. Re-applying the body (db.RLSOn) is harmless, and the header's ROLLBACK
// block runs as written.
func TestMigration0113AskAttempts(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0113_ask_attempts", 112)
	if _, err := pool.Exec(ctx, `SELECT 1 FROM ask_attempts`); err == nil || !strings.Contains(err.Error(), "ask_attempts") {
		t.Fatalf("control: before 0113 ask_attempts exists: %v", err)
	}
	applyThroughFrom(ctx, t, pool, 113)
	if n := countRows(ctx, t, pool, "ask_attempts"); n != 0 {
		t.Fatalf("0113 backfilled %d row(s)", n)
	}
	const cols = `(owner_id, runtime, door, action, method, outcome, reason, questions, options, multi_select, snapshot)`
	mustExec(ctx, t, pool, `INSERT INTO ask_attempts `+cols+` VALUES (1, 'claude-code', 'card', 'submit', 'typed', 'failed', 'no-picker', 2, '{3,2}', '{f,t}', 'pane')`)
	for _, bad := range []string{
		`INSERT INTO ask_attempts ` + cols + ` VALUES (1, 'claude-code', 'email', 'submit', 'typed', 'ok', 'typed', 0, '{}', '{}', '')`,
		`INSERT INTO ask_attempts ` + cols + ` VALUES (1, 'agent', 'card', 'submit', 'typed', 'ok', 'typed', 0, '{}', '{}', '')`,
		`INSERT INTO ask_attempts ` + cols + ` VALUES (1, 'claude-code', 'card', 'answer', 'typed', 'ok', 'typed', 0, '{}', '{}', '')`,
		`INSERT INTO ask_attempts ` + cols + ` VALUES (1, 'claude-code', 'card', 'submit', 'pasted', 'ok', 'typed', 0, '{}', '{}', '')`,
		`INSERT INTO ask_attempts ` + cols + ` VALUES (1, 'claude-code', 'card', 'submit', 'typed', 'maybe', 'typed', 0, '{}', '{}', '')`,
		`INSERT INTO ask_attempts ` + cols + ` VALUES (1, 'claude-code', 'card', 'submit', 'typed', 'ok', 'Red, please', 0, '{}', '{}', '')`,
		`INSERT INTO ask_attempts ` + cols + ` VALUES (1, 'claude-code', 'card', 'submit', 'typed', 'ok', 'typed', 2, '{3}', '{f,t}', '')`,
		`INSERT INTO ask_attempts ` + cols + ` VALUES (1, 'claude-code', 'card', 'submit', 'typed', 'ok', 'typed', 0, '{}', '{}', repeat('x', 8001))`,
	} {
		if _, err := pool.Exec(ctx, bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, err := pool.Exec(ctx, readMigration(t, "0113_ask_attempts.sql")); err != nil {
		t.Fatalf("re-applying 0113: %v", err)
	}
	if n := countRows(ctx, t, pool, "ask_attempts"); n != 1 {
		t.Fatalf("re-applying 0113 left %d rows, want the 1 written", n)
	}
	for _, q := range asksRollbackSQL(t, "0113_ask_attempts.sql") {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	if _, err := pool.Exec(ctx, `SELECT 1 FROM ask_attempts`); err == nil {
		t.Error("after the rollback ask_attempts still exists")
	}
}
