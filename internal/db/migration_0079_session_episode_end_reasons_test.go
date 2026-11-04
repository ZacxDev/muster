package db_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// refusedBy reports whether err is a refusal by the named constraint, so a
// control cannot pass on some other error (row-level security, a missing
// column).
func refusedBy(err error, constraint string) bool {
	return err != nil && strings.Contains(err.Error(), constraint)
}

// TestMigration0079WidensTheEndReasonResetsTheBacklogAndRollsBack: at version
// 78 an episode cannot end 'owner', 'viewed' or 'reset' (the control: the CHECK
// refuses each); after 0079 it can, while an unknown reason is still refused
// and the ended-iff-reason rule still holds. 0079 ends EVERY episode open when
// it applies — of every owner — 'reset', and leaves an ended one as it was.
// The header's rollback SQL narrows the check again; a second apply widens it
// again (idempotent).
func TestMigration0079WidensTheEndReasonResetsTheBacklogAndRollsBack(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0079_end_reasons", 78)
	owner := scanID(ctx, t, pool, `INSERT INTO users (role) VALUES ('member') RETURNING id`)
	other := scanID(ctx, t, pool, `INSERT INTO users (role) VALUES ('member') RETURNING id`)
	seq := 0
	insert := func(uid int64, key, reason string, ended bool) (int64, error) {
		seq++
		endedAt := "NULL"
		if ended {
			endedAt = "now() - interval '1 hour'"
		}
		var id int64
		err := pool.QueryRow(ctx, `INSERT INTO session_episodes (owner_id, session_kind, session_key, seq, state, ended_at, end_reason, pushed_at)
			VALUES ($1, 'mirror', $2, $3, 'stalled', `+endedAt+`, $4, now() - interval '2 hours') RETURNING id`, uid, key, seq, reason).Scan(&id)
		return id, err
	}
	end := func(reason string, ended bool) error {
		_, err := insert(owner, "k-0079", reason, ended)
		return err
	}
	if err := end("worked", true); err != nil {
		t.Fatalf("control: at version 78 an episode cannot even end 'worked': %v", err)
	}
	for _, r := range []string{"owner", "viewed", "reset"} {
		if err := end(r, true); !refusedBy(err, "session_episodes_end_reason_check") {
			t.Fatalf("control: at version 78 an episode ending %q: %v, want the CHECK's refusal", r, err)
		}
	}
	// The backlog: an open episode of each of two owners, and an ended one.
	openA, err := insert(owner, "k-open-a", "", false)
	if err != nil {
		t.Fatal(err)
	}
	openB, err := insert(other, "k-open-b", "", false)
	if err != nil {
		t.Fatal(err)
	}
	worked, err := insert(owner, "k-worked", "worked", true)
	if err != nil {
		t.Fatal(err)
	}

	applyThroughFrom(ctx, t, pool, 79)

	for _, id := range []int64{openA, openB} {
		var reason string
		var ended, recent bool
		if err := pool.QueryRow(ctx, `SELECT end_reason, ended_at IS NOT NULL, coalesce(ended_at > now() - interval '1 minute', false)
			FROM session_episodes WHERE id = $1`, id).Scan(&reason, &ended, &recent); err != nil {
			t.Fatal(err)
		}
		if reason != "reset" || !ended || !recent {
			t.Fatalf("episode %d open before 0079: reason %q ended %v (now %v), want ended 'reset' by 0079", id, reason, ended, recent)
		}
	}
	var wReason string
	var wOld bool
	if err := pool.QueryRow(ctx, `SELECT end_reason, ended_at < now() - interval '30 minutes' FROM session_episodes WHERE id = $1`,
		worked).Scan(&wReason, &wOld); err != nil {
		t.Fatal(err)
	}
	if wReason != "worked" || !wOld {
		t.Fatalf("an episode ended before 0079 was touched: reason %q, ended_at kept %v", wReason, wOld)
	}

	for _, r := range []string{"worked", "gone", "owner", "viewed", "reset"} {
		if err := end(r, true); err != nil {
			t.Fatalf("after 0079, end_reason %q: %v", r, err)
		}
	}
	if err := end("bored", true); !refusedBy(err, "session_episodes_end_reason_check") {
		t.Fatalf("after 0079 an unknown end_reason: %v, want the CHECK's refusal", err)
	}
	if err := end("viewed", false); !refusedBy(err, `"session_episodes_end_reason"`) {
		t.Fatalf("after 0079 an open episode carrying an end reason: %v, want session_episodes_end_reason's refusal", err)
	}

	// The rollback SQL AS THE MIGRATION'S HEADER WRITES IT (rollbackSQL), not a
	// copy that could drift from it.
	for _, q := range rollbackSQL(t) {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	// 🔴 It deletes this migration's row and no other: 0078 is another
	// migration's (0078_chief_nudge_notify), and dropping its row would have it
	// re-applied.
	var has78, has79 bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = 78),
			EXISTS (SELECT 1 FROM schema_migrations WHERE version = 79)`).Scan(&has78, &has79); err != nil {
		t.Fatal(err)
	}
	if !has78 || has79 {
		t.Fatalf("after the rollback SQL: version 78 recorded %v (want true), 79 recorded %v (want false)", has78, has79)
	}
	if err := end("viewed", true); !refusedBy(err, "session_episodes_end_reason_check") {
		t.Fatalf("after the rollback SQL an episode ending 'viewed': %v, want the CHECK's refusal", err)
	}

	applyThroughFrom(ctx, t, pool, 79)
	if err := end("owner", true); err != nil {
		t.Fatalf("after re-applying 0079: %v", err)
	}
}

// rollbackSQL is the statements of migration 0079's header ROLLBACK block: the
// comment lines indented under it ("--   "), joined and split on ';'.
func rollbackSQL(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("migrations", "0079_*.sql"))
	if err != nil || len(files) != 1 {
		t.Fatalf("migration 0079: %v %v", files, err)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var sql []string
	in := false
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "-- ROLLBACK.") {
			in = true
			continue
		}
		if !in {
			continue
		}
		if !strings.HasPrefix(line, "--") {
			break
		}
		if strings.HasPrefix(line, "--   ") {
			sql = append(sql, strings.TrimSpace(strings.TrimPrefix(line, "--")))
		}
	}
	var out []string
	for _, q := range strings.Split(strings.Join(sql, " "), ";") {
		if q = strings.TrimSpace(q); q != "" {
			out = append(out, q)
		}
	}
	if len(out) != 4 {
		t.Fatalf("the header's rollback SQL: %d statements, want 4: %q", len(out), out)
	}
	return out
}
