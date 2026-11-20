package db_test

import (
	"fmt"
	"path/filepath"
	"testing"
)

// pagesAgentShareDefaultOnVersion is 0163_pages_agent_share_default_on's
// number: renumber the file and this together.
const pagesAgentShareDefaultOnVersion = 163

// predecessorOf is the newest on-disk migration below v: v-1 once the tree is
// contiguous, and the right "just before" while an earlier number is still
// held by an unmerged branch.
func predecessorOf(t *testing.T, v int) int {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("list migrations: %v (%d files)", err, len(files))
	}
	prev := 0
	for _, f := range files {
		var n int
		if _, err := fmt.Sscanf(filepath.Base(f), "%04d_", &n); err != nil {
			t.Fatalf("migration %s: %v", f, err)
		}
		if n < v && n > prev {
			prev = n
		}
	}
	return prev
}

// TestMigration0163PagesAgentShareDefaultsOnAndKeepsStoredChoices: a database
// just before 0163 holds two rows — user 2 wrote "Let my agents share pages
// without asking" OFF, user 3 wrote it ON. After 0163 both read exactly as
// stored (nothing is backfilled), while a row written with no value for it
// (user 4, as another switch writes one) reads ON. Then the header's ROLLBACK
// block, run as written, puts the default back: a row written with no value
// after it (user 5) reads off, and every earlier row is untouched.
func TestMigration0163PagesAgentShareDefaultsOnAndKeepsStoredChoices(t *testing.T) {
	v := pagesAgentShareDefaultOnVersion
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0163_pages_share_on", predecessorOf(t, v))
	mustExec(ctx, t, pool, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member'), (3, 'member'), (4, 'member'), (5, 'member') ON CONFLICT (id) DO NOTHING`)
	mustExec(ctx, t, pool, `INSERT INTO notify_preferences (owner_id, pages_agent_share) VALUES (2, false), (3, true)`)
	read := func(owner int) bool {
		t.Helper()
		var on bool
		if err := pool.QueryRow(ctx, `SELECT pages_agent_share FROM notify_preferences WHERE owner_id = $1`, owner).Scan(&on); err != nil {
			t.Fatalf("read user %d: %v", owner, err)
		}
		return on
	}
	// Control: before 0163 a row written with no value reads off.
	mustExec(ctx, t, pool, `INSERT INTO notify_preferences (owner_id, tmux_address) VALUES (5, true)`)
	if read(5) {
		t.Fatal("control: before 0163 a row written with no value reads on")
	}
	mustExec(ctx, t, pool, `DELETE FROM notify_preferences WHERE owner_id = 5`)

	applyThroughFrom(ctx, t, pool, v)
	if read(2) || !read(3) {
		t.Fatalf("after 0163 the stored choices read user 2 %v, user 3 %v; want false and true (nothing backfilled)", read(2), read(3))
	}
	mustExec(ctx, t, pool, `INSERT INTO notify_preferences (owner_id, tmux_address) VALUES (4, true)`)
	if !read(4) {
		t.Fatal("after 0163 a row written by another switch reads the pages switch off, want on")
	}

	rb := asksRollbackSQL(t, fmt.Sprintf("%04d_pages_agent_share_default_on.sql", v))
	if len(rb) != 2 {
		t.Fatalf("the ROLLBACK block has %d statement(s), want 2: %q", len(rb), rb)
	}
	for _, q := range rb {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	mustExec(ctx, t, pool, `INSERT INTO notify_preferences (owner_id, tmux_address) VALUES (5, true)`)
	if read(5) {
		t.Fatal("after the rollback a row written with no value reads on, want off (the old default)")
	}
	if read(2) || !read(3) || !read(4) {
		t.Fatalf("the rollback changed a stored row: 2 %v, 3 %v, 4 %v; want false, true, true", read(2), read(3), read(4))
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version = $1`, v).Scan(&n); err != nil || n != 0 {
		t.Errorf("after the rollback schema_migrations still records %d (%d row(s), err %v)", v, n, err)
	}
}
