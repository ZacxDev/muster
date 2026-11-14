package db_test

import "testing"

// TestMigration0126TeamAccess: at 0125 every owned table has owner_isolation
// and nothing reads muster.teams (the control); at 0126 the share-class
// tables carry the four team policies instead; the header's ROLLBACK block
// runs as written and puts owner_isolation back on every owned table with no
// team policy, function or attribution column left; and the file applies
// again after it.
func TestMigration0126TeamAccess(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0126_access", 125)
	countPolicies := func(name string) int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(DISTINCT tablename) FROM pg_policies WHERE schemaname = 'public' AND policyname = $1`, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	owned := len(rlsTables())
	if n := countPolicies("owner_isolation"); n != owned || countPolicies("team_read") != 0 {
		t.Fatalf("control: at 0125 %d tables have owner_isolation (want %d) and %d team_read", n, owned, countPolicies("team_read"))
	}
	applyThroughFrom(ctx, t, pool, 126)
	if n, s := countPolicies("owner_isolation"), countPolicies("team_read"); n != owned-len(shareClasses) || s != len(shareClasses) {
		t.Fatalf("at 0126: %d owner_isolation, %d team_read; want %d and %d", n, s, owned-len(shareClasses), len(shareClasses))
	}
	for _, q := range asksRollbackSQL(t, "0126_team_access.sql") {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	if n, s := countPolicies("owner_isolation"), countPolicies("team_read"); n != owned || s != 0 {
		t.Errorf("after the rollback: %d owner_isolation (want %d), %d team_read (want 0)", n, owned, s)
	}
	var leftovers int
	if err := pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM pg_proc WHERE proname IN ('muster_team_ids', 'muster_owner_fixed',
				'muster_stamp_answerer', 'muster_stamp_decider', 'muster_team_access_rollback'))
			+ (SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND
				(table_name, column_name) IN (('audit_log', 'team_id'), ('send_queue', 'queued_by'), ('note_comments', 'author_id'),
					('attention_asks', 'answered_by_user'), ('plugin_reply_drafts', 'decided_by_user')))
			+ (SELECT count(*) FROM pg_trigger WHERE tgname IN ('owner_fixed', 'stamp_answerer', 'stamp_decider'))`).Scan(&leftovers); err != nil {
		t.Fatal(err)
	}
	if leftovers != 0 {
		t.Errorf("after the rollback %d functions, columns or triggers of 0126 remain", leftovers)
	}
	applyThroughFrom(ctx, t, pool, 126)
	if n, s := countPolicies("owner_isolation"), countPolicies("team_read"); n != owned-len(shareClasses) || s != len(shareClasses) {
		t.Errorf("re-applied after the rollback: %d owner_isolation, %d team_read", n, s)
	}
}
