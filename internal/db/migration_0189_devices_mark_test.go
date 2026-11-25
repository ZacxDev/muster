package db_test

import (
	"slices"
	"testing"
	"time"
)

// TestMigration0189MarksInTheSameStatement: 0189 backfills an account whose
// step-up phone was paired unmarked after 0188, and from then on inserting,
// deleting, keying or moving a step-up phone marks its account(s) in that statement; a
// read-only phone marks nothing. Its function is SECURITY DEFINER of 0007's
// shape and not executable by PUBLIC.
func TestMigration0189MarksInTheSameStatement(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0189_mark", 188)
	user := func(name string) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	phone := func(owner int64, name string, keyed bool) int64 {
		t.Helper()
		q := `INSERT INTO devices (owner_id, name, token_sha256, paired_signin_at) VALUES ($1, $2, sha256(convert_to($2, 'UTF8')), now()) RETURNING id`
		if keyed {
			q = `INSERT INTO devices (owner_id, name, token_sha256, paired_signin_at, stepup_pubkey, stepup_key_type, stepup_key_level)
				VALUES ($1, $2, sha256(convert_to($2, 'UTF8')), now(), '\x01'::bytea, 'biometric', 'strongbox') RETURNING id`
		}
		var id int64
		if err := pool.QueryRow(ctx, q, owner, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	marked := func(id int64) bool {
		t.Helper()
		var at *time.Time
		if err := pool.QueryRow(ctx, `SELECT had_strong_factor_at FROM users WHERE id = $1`, id).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at != nil
	}
	unmark := func(id int64) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE users SET had_strong_factor_at = NULL WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	// At 0188 an insert marks nothing (the control).
	gap := user("paired-in-the-gap")
	phone(gap, "gap-phone", true)
	if marked(gap) {
		t.Fatal("control: at 0188 a keyed insert already marks")
	}
	applyThroughFrom(ctx, t, pool, 189)
	if !marked(gap) {
		t.Fatal("0189 did not backfill an unmarked account with a step-up phone")
	}
	// The trigger: insert, delete, key.
	a, ro := user("keyed"), user("read-only")
	pa := phone(a, "a-phone", true)
	pr := phone(ro, "ro-phone", false)
	if !marked(a) || marked(ro) {
		t.Fatalf("after the inserts: keyed marked %v (want true), read-only marked %v (want false)", marked(a), marked(ro))
	}
	unmark(a)
	if _, err := pool.Exec(ctx, `DELETE FROM devices WHERE id = $1`, pa); err != nil {
		t.Fatal(err)
	}
	if !marked(a) {
		t.Fatal("deleting a step-up phone left its account unmarked")
	}
	if _, err := pool.Exec(ctx, `UPDATE devices SET stepup_pubkey = '\x02'::bytea, stepup_key_type = 'biometric', stepup_key_level = 'strongbox' WHERE id = $1`, pr); err != nil {
		t.Fatal(err)
	}
	if !marked(ro) {
		t.Fatal("keying a phone left its account unmarked")
	}
	// A keyed phone moved between accounts marks BOTH (UPDATE OF owner_id).
	from, to := user("moved-from"), user("moved-to")
	pm := phone(from, "moved-phone", true)
	unmark(from)
	if _, err := pool.Exec(ctx, `UPDATE devices SET owner_id = $2 WHERE id = $1`, pm, to); err != nil {
		t.Fatal(err)
	}
	if !marked(from) || !marked(to) {
		t.Fatalf("a keyed phone moved between owners: old marked %v, new marked %v (want both)", marked(from), marked(to))
	}
	var definer, public bool
	var config []string
	if err := pool.QueryRow(ctx, `SELECT prosecdef, proconfig, has_function_privilege('public', oid, 'EXECUTE')
		FROM pg_proc WHERE proname = 'muster_devices_mark_strong_factor'`).Scan(&definer, &config, &public); err != nil {
		t.Fatal(err)
	}
	slices.Sort(config)
	if !definer || public || !slices.Equal(config, []string{"muster.scope=all", "search_path=pg_catalog"}) {
		t.Errorf("muster_devices_mark_strong_factor: prosecdef=%v public=%v proconfig=%v", definer, public, config)
	}
}
