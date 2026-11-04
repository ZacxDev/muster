package db_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestMigration0079AddsTypedByWithNoBackfill: at version 78 neither
// chat_messages.typed_by nor chief_nudges.quote_source exists (the control);
// after 0079 a chat row written BEFORE it has typed_by NULL — no older turn
// counts as typed by the owner (operator decision 13, "no backfill") — a new
// row can carry one, quote_source takes the empty string, 'session' and
// 'chief_chat' and refuses anything else, and the header's rollback SQL removes both.
func TestMigration0079AddsTypedByWithNoBackfill(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0079_typed_by", 78)
	code := func(err error) string {
		var pe *pgconn.PgError
		if errors.As(err, &pe) {
			return pe.Code
		}
		return ""
	}
	var agent, session, before int64
	if err := pool.QueryRow(ctx, `INSERT INTO agents (name, namespace) VALUES ('synthetic-chief', 'synthetic-ns') RETURNING id`).Scan(&agent); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO chat_sessions (agent_id, session_key) VALUES ($1, 'synthetic-key') RETURNING id`, agent).Scan(&session); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO chat_messages (agent_id, session_id, role, content) VALUES ($1, $2, 'user', 'an older line')
		RETURNING id`, agent, session).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `SELECT typed_by FROM chat_messages`); code(err) != "42703" {
		t.Fatalf("control: at 78 typed_by exists: %v", err)
	}
	if _, err := pool.Exec(ctx, `SELECT quote_source FROM chief_nudges`); code(err) != "42703" {
		t.Fatalf("control: at 78 quote_source exists: %v", err)
	}

	applyThroughFrom(ctx, t, pool, 79)

	var typed *int64
	if err := pool.QueryRow(ctx, `SELECT typed_by FROM chat_messages WHERE id = $1`, before).Scan(&typed); err != nil {
		t.Fatal(err)
	}
	if typed != nil {
		t.Fatalf("a row from before 0079 reads typed by %d; nothing is backfilled", *typed)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO chat_messages (agent_id, session_id, role, content, typed_by) VALUES ($1, $2, 'user', 'a typed line', 1)`,
		agent, session); err != nil {
		t.Fatalf("a typed row after 0079: %v", err)
	}
	insert := func(streak, source string) error {
		_, err := pool.Exec(ctx, `INSERT INTO chief_nudges (owner_id, session_kind, session_key, streak_key, decision, mode, outcome, quote_source)
			VALUES (1, 'mirror', 'synthetic-s', $1, 'nudge', 'shadow', 'would_send', $2)`, streak, source)
		return err
	}
	for i, src := range []string{"", "session", "chief_chat"} {
		if err := insert("since:"+string(rune('a'+i)), src); err != nil {
			t.Fatalf("quote_source %q: %v", src, err)
		}
	}
	if err := insert("since:z", "pane"); code(err) != "23514" {
		t.Fatalf("an unknown quote_source: %v, want a CHECK violation", err)
	}
	var dflt string
	if err := pool.QueryRow(ctx, `INSERT INTO chief_nudges (owner_id, session_kind, session_key, streak_key, decision, mode, outcome)
		VALUES (1, 'mirror', 'synthetic-s', 'since:d', 'leave', 'shadow', 'left') RETURNING quote_source`).Scan(&dflt); err != nil || dflt != "" {
		t.Fatalf("a decision naming no source: %q, %v", dflt, err)
	}

	// [rollback-sql], as the migration's header writes it.
	for _, q := range []string{
		`DROP INDEX IF EXISTS chat_messages_typed_by`,
		`ALTER TABLE chat_messages DROP COLUMN IF EXISTS typed_by`,
		`ALTER TABLE chief_nudges DROP COLUMN IF EXISTS quote_source`,
		`DELETE FROM schema_migrations WHERE version = 79`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	if _, err := pool.Exec(ctx, `SELECT typed_by FROM chat_messages`); code(err) != "42703" {
		t.Fatalf("after the rollback typed_by exists: %v", err)
	}
	applyThroughFrom(ctx, t, pool, 79)
	if _, err := pool.Exec(ctx, `SELECT typed_by, quote_source FROM chat_messages, chief_nudges LIMIT 1`); err != nil {
		t.Fatalf("re-applied: %v", err)
	}
}
