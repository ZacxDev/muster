package db_test

import (
	"testing"
	"time"
)

// TestMigration0188BackfillsAccountsWithAStepUpPhone: at 0187 users has no
// had_strong_factor_at (the control); 0188 adds it and marks exactly the
// accounts that have a paired phone with a step-up key — not one with only a
// read-only phone, not one with none.
func TestMigration0188BackfillsAccountsWithAStepUpPhone(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0188_factor", 187)
	if _, err := pool.Exec(ctx, `SELECT had_strong_factor_at FROM users LIMIT 1`); err == nil {
		t.Fatal("control: before 0188 users.had_strong_factor_at exists")
	}
	user := func(name string) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	phone := func(owner int64, name string, keyed bool) {
		t.Helper()
		q := `INSERT INTO devices (owner_id, name, token_sha256, paired_signin_at) VALUES ($1, $2, sha256(convert_to($2, 'UTF8')), now())`
		if keyed {
			q = `INSERT INTO devices (owner_id, name, token_sha256, paired_signin_at, stepup_pubkey, stepup_key_type, stepup_key_level)
				VALUES ($1, $2, sha256(convert_to($2, 'UTF8')), now(), '\x01'::bytea, 'biometric', 'strongbox')`
		}
		if _, err := pool.Exec(ctx, q, owner, name); err != nil {
			t.Fatal(err)
		}
	}
	keyed, readOnly, none := user("keyed"), user("read-only"), user("none")
	phone(keyed, "keyed-phone", true)
	phone(readOnly, "read-only-phone", false)
	applyThroughFrom(ctx, t, pool, 188)
	for _, tc := range []struct {
		id     int64
		marked bool
	}{{keyed, true}, {readOnly, false}, {none, false}} {
		var at *time.Time
		if err := pool.QueryRow(ctx, `SELECT had_strong_factor_at FROM users WHERE id = $1`, tc.id).Scan(&at); err != nil {
			t.Fatal(err)
		}
		if (at != nil) != tc.marked {
			t.Errorf("user %d: had_strong_factor_at %v, want marked %v", tc.id, at, tc.marked)
		}
	}
}
