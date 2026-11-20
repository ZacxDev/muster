package db_test

import "testing"

// TestMigration0127TeamAccess: at 0126 every owned table has owner_isolation
// and nothing reads muster.teams (the control); at 0127 the share-class
// tables carry the four team policies instead; the header's ROLLBACK block
// runs as written and puts owner_isolation back on every owned table with no
// team policy, function or attribution column left; and the file applies
// again after it.
// ownedAfter0127 is every owned table a migration AFTER 0127 creates. This
// test stops at 0127, so those tables are in rlsTables() but not in its
// schema; a new owned table added after 0127 is listed here, or the control
// below counts one table too many.
var ownedAfter0127 = []string{
	"agent_secrets",                              // 0128
	"officer_contacts", "officer_contact_events", // 0132
	"officer_public_branches",        // 0138
	"plugin_chief_instructions",      // 0141
	"chief_session_llm_usage",        // 0144
	"chief_session_llm_reservations", // 0144
	"fleet_owner_marks",              // 0147
	"officer_connector_blocks",       // 0148
	"officer_seen",                   // 0156
	"officer_cap_raises",             // 0157
	"fleet_actions",                  // 0158
	"officer_chat_cards",             // 0160
	"morning_reports",                // 0161
}

// sharedAfter0127 is every share-class table a migration AFTER 0127 shares
// (it was personal at 0127), so the counts at 0127 leave it out.
var sharedAfter0127 = []string{
	"agents",                                     // 0131
	"officer_contacts", "officer_contact_events", // 0132
	"officer_public_branches",  // 0138
	"officer_connector_blocks", // 0148
}

func TestMigration0127TeamAccess(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0127_access", 126)
	countPolicies := func(name string) int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(DISTINCT tablename) FROM pg_policies WHERE schemaname = 'public' AND policyname = $1`, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	owned := len(rlsTables()) - len(ownedAfter0127)
	shared := len(shareClasses) - len(sharedAfter0127)
	if n := countPolicies("owner_isolation"); n != owned || countPolicies("team_read") != 0 {
		t.Fatalf("control: at 0126 %d tables have owner_isolation (want %d) and %d team_read", n, owned, countPolicies("team_read"))
	}
	applyThroughFrom(ctx, t, pool, 127)
	if n, s := countPolicies("owner_isolation"), countPolicies("team_read"); n != owned-shared || s != shared {
		t.Fatalf("at 0127: %d owner_isolation, %d team_read; want %d and %d", n, s, owned-shared, shared)
	}
	for _, q := range asksRollbackSQL(t, "0127_team_access.sql") {
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
				'muster_actor_column', 'muster_team_access_rollback'))
			+ (SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND
				(table_name, column_name) IN (('audit_log', 'team_id'), ('send_queue', 'queued_by'), ('note_comments', 'author_id'),
					('attention_asks', 'answered_by_user'), ('plugin_reply_drafts', 'decided_by_user')))
			+ (SELECT count(*) FROM pg_trigger WHERE tgname IN ('owner_fixed', 'actor_queued_by', 'actor_author_id', 'actor_answered_by_user', 'actor_decided_by_user'))`).Scan(&leftovers); err != nil {
		t.Fatal(err)
	}
	if leftovers != 0 {
		t.Errorf("after the rollback %d functions, columns or triggers of 0127 remain", leftovers)
	}
	applyThroughFrom(ctx, t, pool, 127)
	if n, s := countPolicies("owner_isolation"), countPolicies("team_read"); n != owned-shared || s != shared {
		t.Errorf("re-applied after the rollback: %d owner_isolation, %d team_read", n, s)
	}
}
