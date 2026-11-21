package db_test

import (
	"strings"
	"testing"
)

// TestMigration0174SigninRequests: a database at 0173 has no signin_requests
// (the control); at 0174 it does, empty, under forced row-level security whose
// only policy is scope_all (a user's principal reads and writes nothing; the
// store runs as the system). The checks refuse what the store never writes: a
// hash of the wrong size, an unknown state, an approval without its approver
// or sign-in time, an approver on a denial, a redeemed row with no redeemed_at.
// Then the header's ROLLBACK block runs as written and leaves no table, and the
// file applies again after it.
func TestMigration0174SigninRequests(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0174_signin", 173)
	if _, err := pool.Exec(ctx, `SELECT 1 FROM signin_requests`); err == nil || !strings.Contains(err.Error(), "signin_requests") {
		t.Fatalf("control: before 0174 signin_requests exists: %v", err)
	}
	applyThroughFrom(ctx, t, pool, 174)
	if n := countRows(ctx, t, pool, "signin_requests"); n != 0 {
		t.Fatalf("0174 wrote %d row(s); want none", n)
	}

	var rls, forced bool
	if err := pool.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'signin_requests'`).Scan(&rls, &forced); err != nil || !rls || !forced {
		t.Fatalf("row-level security enabled=%v forced=%v err=%v; want both", rls, forced, err)
	}
	rows, err := pool.Query(ctx, `SELECT policyname || ' ' || cmd || ' ' || permissive || ' USING=' || coalesce(qual, '') || ' CHECK=' || coalesce(with_check, '')
		FROM pg_policies WHERE schemaname = 'public' AND tablename = 'signin_requests'`)
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

	const hashA, hashB = `'\x` + "0000000000000000000000000000000000000000000000000000000000000001" + `'`,
		`'\x` + "0000000000000000000000000000000000000000000000000000000000000002" + `'`
	mustExec(ctx, t, pool, `INSERT INTO signin_requests (code_sha256, binding_sha256, expires_at) VALUES (`+hashA+`, `+hashB+`, now() + interval '2 minutes')`)
	for name, q := range map[string]string{
		"a short code hash":           `INSERT INTO signin_requests (code_sha256, binding_sha256, expires_at) VALUES ('\x01', '\x02', now())`,
		"the same code again":         `INSERT INTO signin_requests (code_sha256, binding_sha256, expires_at) VALUES (` + hashA + `, '\x` + strings.Repeat("03", 32) + `', now())`,
		"an unknown state":            `UPDATE signin_requests SET state = 'expired'`,
		"an approval with no one":     `UPDATE signin_requests SET state = 'approved'`,
		"an approval with no sign-in": `UPDATE signin_requests SET state = 'approved', approved_by = 1`,
		"a denial naming an approver": `UPDATE signin_requests SET state = 'denied', approved_by = 1, approver_signin_at = now()`,
		"a redeem with no time":       `UPDATE signin_requests SET state = 'redeemed', approved_by = 1, approver_signin_at = now()`,
		"redeemed_at on a pending":    `UPDATE signin_requests SET redeemed_at = now()`,
		"an unknown approver":         `UPDATE signin_requests SET state = 'approved', approved_by = 999999, approver_signin_at = now()`,
		"a 121-character agent":       `UPDATE signin_requests SET requester_agent = repeat('a', 121)`,
	} {
		if _, err := pool.Exec(ctx, q); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// The shapes the store does write are accepted (the positive control).
	mustExec(ctx, t, pool, `UPDATE signin_requests SET state = 'approved', approved_by = 1, approver_signin_at = now(), requester_agent = repeat('😀', 120)`)
	mustExec(ctx, t, pool, `UPDATE signin_requests SET state = 'redeemed', redeemed_at = now()`)

	for _, q := range asksRollbackSQL(t, "0174_signin_requests.sql") {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	if _, err := pool.Exec(ctx, `SELECT 1 FROM signin_requests`); err == nil {
		t.Error("after the rollback signin_requests still exists")
	}
	applyThroughFrom(ctx, t, pool, 174)
	if n := countRows(ctx, t, pool, "signin_requests"); n != 0 {
		t.Errorf("re-applied after the rollback: %d row(s)", n)
	}
}
