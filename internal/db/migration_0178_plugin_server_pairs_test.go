package db_test

import (
	"strings"
	"testing"
)

// TestMigration0178PluginServerPairs: a database at 0177 has no
// plugin_server_pairs (the control); at 0178 it does, empty, under forced
// row-level security whose only policy is scope_all. Its constraints refuse
// what the store never writes, EACH BY ITS OWN NAME: a pair out of order
// (plugin_server_pairs_order), a source that is neither app
// (plugin_server_pairs_source), an app twice in the low column
// (plugin_server_pairs_low) or the high column (plugin_server_pairs_high), an
// app in the other column of another row (plugin_server_pairs_one_pair, the
// trigger), and an app that does not exist (the foreign key). Then the
// header's ROLLBACK block runs as written and leaves no table and no
// function, and the file applies again after it.
func TestMigration0178PluginServerPairs(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0178_pairs", 177)
	if _, err := pool.Exec(ctx, `SELECT 1 FROM plugin_server_pairs`); err == nil || !strings.Contains(err.Error(), "plugin_server_pairs") {
		t.Fatalf("control: before 0178 plugin_server_pairs exists: %v", err)
	}
	applyThroughFrom(ctx, t, pool, 178)
	if n := countRows(ctx, t, pool, "plugin_server_pairs"); n != 0 {
		t.Fatalf("0178 wrote %d row(s); want none", n)
	}

	var rls, forced bool
	if err := pool.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'plugin_server_pairs'`).Scan(&rls, &forced); err != nil || !rls || !forced {
		t.Fatalf("row-level security enabled=%v forced=%v err=%v; want both", rls, forced, err)
	}
	rows, err := pool.Query(ctx, `SELECT policyname || ' ' || cmd || ' ' || permissive || ' USING=' || coalesce(qual, '') || ' CHECK=' || coalesce(with_check, '')
		FROM pg_policies WHERE schemaname = 'public' AND tablename = 'plugin_server_pairs'`)
	if err != nil {
		t.Fatal(err)
	}
	var policies []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		policies = append(policies, p)
	}
	rows.Close()
	const want = "scope_all ALL PERMISSIVE USING=(current_setting('muster.scope'::text, true) = 'all'::text) CHECK=(current_setting('muster.scope'::text, true) = 'all'::text)"
	if len(policies) != 1 || policies[0] != want {
		t.Fatalf("policies = %q\nwant exactly [%q]", policies, want)
	}

	// Six server apps (ids a < b < c < d < e < f), straight into the table as
	// the migrator: the constraints are what is under test, not the store.
	var ids []int64
	for _, n := range []string{"pa", "pb", "pc", "pd", "pe", "pf"} {
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO plugins (owner_id, scope, name, label, token_hash, data_scope, capabilities, origin, entry_url,
			app_label, app_icon, installed_via) VALUES (NULL, 'server', $1, 'x', 'h-pair-' || $1, 'none', '{app}',
			'https://*.apps.example.net', 'https://m.apps.example.net/' || $1, 'Slate', 'video', 'cli') RETURNING id`, "srv-"+n).Scan(&id); err != nil {
			t.Fatalf("insert app %s: %v", n, err)
		}
		ids = append(ids, id)
	}
	a, b, c, d, e, f := ids[0], ids[1], ids[2], ids[3], ids[4], ids[5]
	pair := func(low, high, source int64) error {
		_, err := pool.Exec(ctx, `INSERT INTO plugin_server_pairs (low_id, high_id, source_id) VALUES ($1, $2, $3)`, low, high, source)
		return err
	}
	if err := pair(b, d, b); err != nil {
		t.Fatalf("the pair (b, d), source b: %v", err)
	}
	for name, c := range map[string]struct {
		low, high, source int64
		constraint        string
	}{
		"out of order":                 {e, c, e, "plugin_server_pairs_order"},
		"an app linked to itself":      {c, c, c, "plugin_server_pairs_order"},
		"a source that is neither app": {a, c, e, "plugin_server_pairs_source"},
		"b in the low column again":    {b, e, b, "plugin_server_pairs_low"},
		"d in the high column again":   {c, d, c, "plugin_server_pairs_high"},
		"d (a high) in the low column": {d, e, d, "plugin_server_pairs_one_pair"},
		"b (a low) in the high column": {a, b, a, "plugin_server_pairs_one_pair"},
		"an app that is not a plugin":  {e, f + 1000, e, "plugin_server_pairs_high_id_fkey"},
	} {
		if err := pair(c.low, c.high, c.source); err == nil || !strings.Contains(err.Error(), c.constraint) {
			t.Errorf("%s: %v; want a refusal naming %s", name, err, c.constraint)
		}
	}
	// The shapes the store does write are accepted (the positive control): a
	// second, disjoint pair, its source the high id.
	if err := pair(c, e, e); err != nil {
		t.Fatalf("a second, disjoint pair (c, e), source e: %v", err)
	}
	if n := countRows(ctx, t, pool, "plugin_server_pairs"); n != 2 {
		t.Fatalf("%d pairs; want the two accepted", n)
	}
	// An uninstall is an UPDATE, so nothing cascades from it; a DELETE of a
	// plugins row (which nothing does) takes its pair with it.
	if _, err := pool.Exec(ctx, `DELETE FROM plugins WHERE id = $1`, e); err != nil {
		t.Fatal(err)
	}
	if n := countRows(ctx, t, pool, "plugin_server_pairs"); n != 1 {
		t.Fatalf("after deleting app e: %d pairs; want 1", n)
	}

	for _, q := range asksRollbackSQL(t, "0178_plugin_server_pairs.sql") {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	if _, err := pool.Exec(ctx, `SELECT 1 FROM plugin_server_pairs`); err == nil {
		t.Error("after the rollback plugin_server_pairs still exists")
	}
	var fn int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_proc WHERE proname = 'plugin_server_pairs_one_pair'`).Scan(&fn); err != nil || fn != 0 {
		t.Errorf("after the rollback the trigger function: %d (%v); want gone", fn, err)
	}
	applyThroughFrom(ctx, t, pool, 178)
	if n := countRows(ctx, t, pool, "plugin_server_pairs"); n != 0 {
		t.Errorf("re-applied after the rollback: %d row(s)", n)
	}
}
