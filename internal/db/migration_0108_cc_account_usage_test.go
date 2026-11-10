package db_test

import (
	"strings"
	"testing"
)

// TestMigration0108CCAccountUsage: a database at 0107 has no
// cc_account_usage; at 0108 it does, empty (nothing backfilled), one row per
// (owner, account), and its CHECKs refuse what the poller never writes — an
// unknown error kind, a fingerprint that is not 12 hex (a whole token, say)
// and a windows value that is not an object. Re-applying the body (db.RLSOn)
// is harmless, and the header's ROLLBACK block runs as written.
func TestMigration0108CCAccountUsage(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0108_cc_account_usage", 107)
	if _, err := pool.Exec(ctx, `SELECT 1 FROM cc_account_usage`); err == nil || !strings.Contains(err.Error(), "cc_account_usage") {
		t.Fatalf("control: before 0108 cc_account_usage exists: %v", err)
	}
	applyThroughFrom(ctx, t, pool, 108)
	if n := countRows(ctx, t, pool, "cc_account_usage"); n != 0 {
		t.Fatalf("0108 backfilled %d row(s)", n)
	}
	mustExec(ctx, t, pool, `INSERT INTO cc_account_usage (owner_id, account, windows, token_fp)
		VALUES (1, 'alpha', '{"five_hour": {"utilization": 12, "resets_at": "2026-11-10T20:00:00Z"}}', '0123456789ab')`)
	if _, err := pool.Exec(ctx, `INSERT INTO cc_account_usage (owner_id, account) VALUES (1, 'alpha')`); err == nil {
		t.Error("a second row for (1, alpha) was accepted; the key is (owner_id, account)")
	}
	for _, bad := range []string{
		`INSERT INTO cc_account_usage (owner_id, account, error_kind) VALUES (1, 'beta', 'whatever')`,
		`INSERT INTO cc_account_usage (owner_id, account, token_fp) VALUES (1, 'beta', 'sk-ant-oat01-not-a-fingerprint')`,
		`INSERT INTO cc_account_usage (owner_id, account, windows) VALUES (1, 'beta', '[1]')`,
		`INSERT INTO cc_account_usage (owner_id, account) VALUES (1, 'Bad Name')`,
		`INSERT INTO cc_account_usage (owner_id, account, rate_limit_streak) VALUES (1, 'beta', -1)`,
		`INSERT INTO cc_account_usage (owner_id, account, source) VALUES (1, 'beta', 'guess')`,
		`INSERT INTO cc_account_usage (owner_id, account, usage_api) VALUES (1, 'beta', 'maybe')`,
		`INSERT INTO cc_account_usage (owner_id, account, status) VALUES (1, 'beta', 'Allowed; --')`,
	} {
		if _, err := pool.Exec(ctx, bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, err := pool.Exec(ctx, readMigration(t, "0108_cc_account_usage.sql")); err != nil {
		t.Fatalf("re-applying 0108: %v", err)
	}
	if n := countRows(ctx, t, pool, "cc_account_usage"); n != 1 {
		t.Fatalf("re-applying 0108 left %d rows, want the 1 written", n)
	}
	for _, q := range asksRollbackSQL(t, "0108_cc_account_usage.sql") {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	if _, err := pool.Exec(ctx, `SELECT 1 FROM cc_account_usage`); err == nil {
		t.Error("after the rollback cc_account_usage still exists")
	}
}
