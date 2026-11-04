package db_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestMigration0078AddsNotifyAndOneDecisionPerEpisode: at version 77 a
// 'notify' decision, a 'notified' outcome and an episode_id are refused (the
// control); after 0078 they are taken, a second decision for one episode is a
// unique violation while two decisions with no episode are not, and the
// header's rollback SQL (after the new rows are removed) restores the old
// CHECKs.
func TestMigration0078AddsNotifyAndOneDecisionPerEpisode(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0078_notify", 77)
	// insert records one decision; episode "" leaves episode_id out of the
	// statement (so at 77 the column is not named).
	insert := func(streak, decision, outcome, episode string) error {
		q := `INSERT INTO chief_nudges (owner_id, session_kind, session_key, streak_key, decision, mode, outcome)
			VALUES (1, 'mirror', 'synthetic-s', $1, $2, 'shadow', $3)`
		if episode != "" {
			q = `INSERT INTO chief_nudges (owner_id, session_kind, session_key, streak_key, decision, mode, outcome, episode_id)
				VALUES (1, 'mirror', 'synthetic-s', $1, $2, 'shadow', $3, ` + episode + `)`
		}
		_, err := pool.Exec(ctx, q, streak, decision, outcome)
		return err
	}
	code := func(err error) string {
		var pe *pgconn.PgError
		if errors.As(err, &pe) {
			return pe.Code
		}
		return ""
	}
	if err := insert("since:1", "notify", "notified", ""); code(err) != "23514" {
		t.Fatalf("control: at 77 a notify was not refused by a CHECK: %v", err)
	}
	if err := insert("since:1", "leave", "left", "5"); code(err) != "42703" {
		t.Fatalf("control: at 77 episode_id exists: %v", err)
	}

	applyThroughFrom(ctx, t, pool, 78)

	if err := insert("since:1", "notify", "notified", "5"); err != nil {
		t.Fatalf("after 0078 a notify in episode 5: %v", err)
	}
	if err := insert("since:2", "leave", "left", "5"); code(err) != "23505" {
		t.Fatalf("a second decision for episode 5: %v, want a unique violation", err)
	}
	if err := insert("since:3", "leave", "left", ""); err != nil {
		t.Fatalf("a decision with no episode: %v", err)
	}
	if err := insert("since:4", "leave", "left", ""); err != nil {
		t.Fatalf("a second decision with no episode: %v", err)
	}
	if err := insert("since:5", "maybe", "left", ""); code(err) != "23514" {
		t.Fatalf("an unknown decision: %v, want a CHECK violation", err)
	}

	// [rollback-sql], as the migration's header writes it, after the rows that
	// hold the new values are removed.
	for _, q := range []string{
		`DELETE FROM chief_nudges WHERE decision = 'notify' OR outcome = 'notified'`,
		`DROP INDEX IF EXISTS chief_nudges_one_per_episode`,
		`ALTER TABLE chief_nudges DROP COLUMN IF EXISTS episode_id`,
		`ALTER TABLE chief_nudges DROP CONSTRAINT IF EXISTS chief_nudges_decision_check`,
		`ALTER TABLE chief_nudges ADD CONSTRAINT chief_nudges_decision_check CHECK (decision IN ('nudge', 'ask', 'leave', 'invalid'))`,
		`ALTER TABLE chief_nudges DROP CONSTRAINT IF EXISTS chief_nudges_outcome_check`,
		`ALTER TABLE chief_nudges ADD CONSTRAINT chief_nudges_outcome_check CHECK (
			outcome IN ('would_send', 'would_ask', 'left', 'queued', 'sent', 'asked', 'relay_queued', 'relayed', 'failed', 'ask_closed')
			OR outcome ~ '^suppressed:[a-z_]{1,40}$')`,
		`DELETE FROM schema_migrations WHERE version = 78`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	if err := insert("since:6", "notify", "notified", ""); code(err) != "23514" {
		t.Fatalf("after the rollback a notify: %v, want a CHECK violation", err)
	}
	// And it applies again on top.
	applyThroughFrom(ctx, t, pool, 78)
	if err := insert("since:7", "notify", "notified", ""); err != nil {
		t.Fatalf("re-applied: %v", err)
	}
}
