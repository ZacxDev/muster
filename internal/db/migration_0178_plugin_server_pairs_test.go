package db_test

import (
	"strings"
	"testing"
)

// TestMigration0178PluginServerPairs: a database at 0177 has no
// plugin_server_pairs (the control); at 0178 it does, empty, under forced
// row-level security whose only policy is scope_all. 🔴 ONE PAIR PER APP BY
// CONSTRAINTS ALONE, each refusal by its own name: a row with no mirror
// (plugin_server_pairs_mutual), a second partner for an app already paired
// (the primary key, both as the row and as its mirror), an app its own
// partner (plugin_server_pairs_not_self), an app that does not exist (the
// foreign key). Deleting either row of a pair deletes its mirror. Then the
// header's ROLLBACK block runs as written and leaves no table, and the file
// applies again after it.
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

	// Five server apps (a, b, c, d, e), straight into the table as the
	// migrator: the constraints are what is under test, not the store.
	var ids []int64
	for _, n := range []string{"pa", "pb", "pc", "pd", "pe"} {
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO plugins (owner_id, scope, name, label, token_hash, data_scope, capabilities, origin, entry_url,
			app_label, app_icon, installed_via) VALUES (NULL, 'server', $1, 'x', 'h-pair-' || $1, 'none', '{app}',
			'https://*.apps.example.net', 'https://m.apps.example.net/' || $1, 'Slate', 'video', 'cli') RETURNING id`, "srv-"+n).Scan(&id); err != nil {
			t.Fatalf("insert app %s: %v", n, err)
		}
		ids = append(ids, id)
	}
	a, b, c, d, e := ids[0], ids[1], ids[2], ids[3], ids[4]
	pair := func(x, y int64) error {
		_, err := pool.Exec(ctx, `INSERT INTO plugin_server_pairs (plugin_id, partner_id) VALUES ($1, $2), ($2, $1)`, x, y)
		return err
	}
	if err := pair(a, b); err != nil {
		t.Fatalf("the pair (a, b): %v", err)
	}
	for name, q := range map[string]struct {
		sql        string
		args       []any
		constraint string
	}{
		"a row with no mirror":         {`INSERT INTO plugin_server_pairs (plugin_id, partner_id) VALUES ($1, $2)`, []any{c, d}, "plugin_server_pairs_mutual"},
		"a second partner for a (row)": {`INSERT INTO plugin_server_pairs (plugin_id, partner_id) VALUES ($1, $2), ($2, $1)`, []any{a, c}, "plugin_server_pairs_pkey"},
		"b as a second app's partner":  {`INSERT INTO plugin_server_pairs (plugin_id, partner_id) VALUES ($1, $2), ($2, $1)`, []any{c, b}, "plugin_server_pairs_pkey"},
		"an app its own partner":       {`INSERT INTO plugin_server_pairs (plugin_id, partner_id) VALUES ($1, $1)`, []any{c}, "plugin_server_pairs_not_self"},
		"an app that is not a plugin":  {`INSERT INTO plugin_server_pairs (plugin_id, partner_id) VALUES ($1, $2), ($2, $1)`, []any{c, e + 1000}, "fkey"},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err == nil || !strings.Contains(err.Error(), q.constraint) {
			t.Errorf("%s: %v; want a refusal naming %s", name, err, q.constraint)
		}
	}
	// The shape the store does write is accepted (the positive control): a
	// second, disjoint pair, both rows in one INSERT.
	if err := pair(c, d); err != nil {
		t.Fatalf("a second, disjoint pair (c, d): %v", err)
	}
	if n := countRows(ctx, t, pool, "plugin_server_pairs"); n != 4 {
		t.Fatalf("%d pair rows; want the two pairs' four", n)
	}
	// One DELETE of either row ends the pair: the mutual key cascades.
	if _, err := pool.Exec(ctx, `DELETE FROM plugin_server_pairs WHERE plugin_id = $1`, b); err != nil {
		t.Fatal(err)
	}
	if n := countRows(ctx, t, pool, "plugin_server_pairs"); n != 2 {
		t.Fatalf("after deleting b's row: %d pair rows; want only (c, d)'s two", n)
	}
	// A DELETE of a plugins row (which nothing does) takes its pair with it.
	if _, err := pool.Exec(ctx, `DELETE FROM plugins WHERE id = $1`, d); err != nil {
		t.Fatal(err)
	}
	if n := countRows(ctx, t, pool, "plugin_server_pairs"); n != 0 {
		t.Fatalf("after deleting app d: %d pair rows; want none", n)
	}

	for _, q := range asksRollbackSQL(t, "0178_plugin_server_pairs.sql") {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	if _, err := pool.Exec(ctx, `SELECT 1 FROM plugin_server_pairs`); err == nil {
		t.Error("after the rollback plugin_server_pairs still exists")
	}
	applyThroughFrom(ctx, t, pool, 178)
	if n := countRows(ctx, t, pool, "plugin_server_pairs"); n != 0 {
		t.Errorf("re-applied after the rollback: %d row(s)", n)
	}
}
