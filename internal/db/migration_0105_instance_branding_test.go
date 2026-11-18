package db_test

import (
	"strings"
	"testing"
)

// TestMigration0105InstanceBranding: a database at 0104 has no
// instance_branding (the control); at 0105 it does, EMPTY (no row is "nothing
// set", so every server draws the built-in mark until an admin uploads), under
// forced row-level security whose only policy is scope_all. A second row is
// refused (the singleton key), as are a half-set logo, a 65-character or
// empty name and an ETag that is not 16 hex digits. Then the header's
// ROLLBACK block runs as written and leaves no table, and the file applies
// again after it.
func TestMigration0105InstanceBranding(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0105_branding", 104)
	if _, err := pool.Exec(ctx, `SELECT 1 FROM instance_branding`); err == nil || !strings.Contains(err.Error(), "instance_branding") {
		t.Fatalf("control: before 0105 instance_branding exists: %v", err)
	}
	applyThroughFrom(ctx, t, pool, 105)
	if n := countRows(ctx, t, pool, "instance_branding"); n != 0 {
		t.Fatalf("0105 wrote %d row(s); want none (nothing set)", n)
	}

	var rls, forced bool
	if err := pool.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'instance_branding'`).Scan(&rls, &forced); err != nil || !rls || !forced {
		t.Fatalf("row-level security enabled=%v forced=%v err=%v; want both", rls, forced, err)
	}
	rows, err := pool.Query(ctx, `SELECT policyname || ' ' || cmd || ' ' || permissive || ' USING=' || coalesce(qual, '') || ' CHECK=' || coalesce(with_check, '')
		FROM pg_policies WHERE schemaname = 'public' AND tablename = 'instance_branding'`)
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

	mustExec(ctx, t, pool, `INSERT INTO instance_branding (name, logo_png, icon192_png, icon512_png, logo_etag, updated_by)
		VALUES ('Team Example', '\x01', '\x02', '\x03', '0123456789abcdef', 1)`)
	for name, q := range map[string]string{
		"a second row":              `INSERT INTO instance_branding (singleton) VALUES (false)`,
		"the same row again":        `INSERT INTO instance_branding (singleton) VALUES (true)`,
		"a half-set logo":           `UPDATE instance_branding SET icon512_png = NULL`,
		"an etag without logo":      `UPDATE instance_branding SET logo_png = NULL, icon192_png = NULL, icon512_png = NULL`,
		"a 65-character name":       `UPDATE instance_branding SET name = repeat('a', 65)`,
		"an empty name":             `UPDATE instance_branding SET name = ''`,
		"a non-hex etag":            `UPDATE instance_branding SET logo_etag = 'ZZZZZZZZZZZZZZZZ'`,
		"an etag of the wrong size": `UPDATE instance_branding SET logo_etag = '0123'`,
		"an unknown updated_by":     `UPDATE instance_branding SET updated_by = 999999`,
	} {
		if _, err := pool.Exec(ctx, q); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	mustExec(ctx, t, pool, `UPDATE instance_branding SET name = repeat('😀', 64), logo_png = NULL, icon192_png = NULL, icon512_png = NULL, logo_etag = NULL`)

	for _, q := range asksRollbackSQL(t, "0105_instance_branding.sql") {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	if _, err := pool.Exec(ctx, `SELECT 1 FROM instance_branding`); err == nil {
		t.Error("after the rollback instance_branding still exists")
	}
	applyThroughFrom(ctx, t, pool, 105)
	if n := countRows(ctx, t, pool, "instance_branding"); n != 0 {
		t.Errorf("re-applied after the rollback: %d row(s)", n)
	}
}
