package db_test

import (
	"strings"
	"testing"
)

// refusedBy reports whether err is a refusal by the named constraint, so a
// control cannot pass on some other error (row-level security, a missing
// column).
func refusedBy(err error, constraint string) bool {
	return err != nil && strings.Contains(err.Error(), constraint)
}

// TestMigration0078WidensTheEndReasonAndRollsBack: at version 77 an episode
// cannot end 'owner' (the control: the CHECK refuses it); after 0078 it can,
// while an unknown reason is still refused and the ended-iff-reason rule
// still holds; the header's rollback SQL narrows it again; a second apply
// widens it again (idempotent).
func TestMigration0078WidensTheEndReasonAndRollsBack(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0078_end_owner", 77)
	owner := scanID(ctx, t, pool, `INSERT INTO users (role) VALUES ('member') RETURNING id`)
	seq := 0
	end := func(reason string, ended bool) error {
		seq++
		endedAt := "NULL"
		if ended {
			endedAt = "now()"
		}
		_, err := pool.Exec(ctx, `INSERT INTO session_episodes (owner_id, session_kind, session_key, seq, state, ended_at, end_reason)
			VALUES ($1, 'mirror', 'k-0078', $2, 'stalled', `+endedAt+`, $3)`, owner, seq, reason)
		return err
	}
	if err := end("worked", true); err != nil {
		t.Fatalf("control: at version 77 an episode cannot even end 'worked': %v", err)
	}
	if err := end("owner", true); !refusedBy(err, "session_episodes_end_reason_check") {
		t.Fatalf("control: at version 77 an episode ending 'owner': %v, want the CHECK's refusal", err)
	}

	applyThroughFrom(ctx, t, pool, 78)

	for _, r := range []string{"worked", "gone", "owner"} {
		if err := end(r, true); err != nil {
			t.Fatalf("after 0078, end_reason %q: %v", r, err)
		}
	}
	if err := end("bored", true); !refusedBy(err, "session_episodes_end_reason_check") {
		t.Fatalf("after 0078 an unknown end_reason: %v, want the CHECK's refusal", err)
	}
	if err := end("owner", false); !refusedBy(err, `"session_episodes_end_reason"`) {
		t.Fatalf("after 0078 an open episode carrying an end reason: %v, want session_episodes_end_reason's refusal", err)
	}

	// [rollback-sql], as the migration's header writes it.
	for _, q := range []string{
		`UPDATE session_episodes SET end_reason = 'worked' WHERE end_reason = 'owner'`,
		`ALTER TABLE session_episodes DROP CONSTRAINT IF EXISTS session_episodes_end_reason_check`,
		`ALTER TABLE session_episodes ADD CONSTRAINT session_episodes_end_reason_check CHECK (end_reason IN ('', 'worked', 'gone'))`,
		`DELETE FROM schema_migrations WHERE version = 78`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	if err := end("owner", true); !refusedBy(err, "session_episodes_end_reason_check") {
		t.Fatalf("after the rollback SQL an episode ending 'owner': %v, want the CHECK's refusal", err)
	}

	applyThroughFrom(ctx, t, pool, 78)
	if err := end("owner", true); err != nil {
		t.Fatalf("after re-applying 0078: %v", err)
	}
}
