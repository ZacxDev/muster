package db_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/db"
)

// freshAt is a scratch database migrated to exactly version v.
func freshAt(ctx context.Context, t *testing.T, name string, v int) *pgxpool.Pool {
	t.Helper()
	pool, err := db.Connect(ctx, scratchDatabase(ctx, t, name, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	applyThrough(ctx, t, pool, v)
	return pool
}

func readMigration(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("migrations", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestMigration0010MakesTheMarksPerOwnerAndKeepsTheEnvironmentPoolsMarks: a
// database at version 9 holding marks on the environment pool's accounts
// arrives at 0010 with those marks owned by user 1 (the admin, whose pool it
// is) — none lost — and from then on an account is (owner, name): a member may
// mark their own "work" beside the admin's, while a second mark on the same
// (owner, name) is still refused. Re-running the body changes nothing.
func TestMigration0010MakesTheMarksPerOwnerAndKeepsTheEnvironmentPoolsMarks(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0010_marks", 9)
	mustExec(ctx, t, pool, `INSERT INTO cc_account_marks (account, rate_limited_detail) VALUES ('work', 'env pool'), ('personal-2', 'env pool')`)
	member := scanID(ctx, t, pool, `INSERT INTO users (role) VALUES ('member') RETURNING id`)

	applyThroughFrom(ctx, t, pool, 10)

	var owner1 int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM cc_account_marks WHERE owner_id = 1 AND rate_limited_detail = 'env pool'`).Scan(&owner1); err != nil {
		t.Fatal(err)
	}
	if owner1 != 2 || countRows(ctx, t, pool, "cc_account_marks") != 2 {
		t.Fatalf("after 0010: %d of the environment pool's marks are user 1's (of %d rows); want 2 of 2",
			owner1, countRows(ctx, t, pool, "cc_account_marks"))
	}
	mustExec(ctx, t, pool, `INSERT INTO cc_account_marks (owner_id, account, rate_limited_detail) VALUES ($1, 'work', 'member')`, member)
	if _, err := pool.Exec(ctx, `INSERT INTO cc_account_marks (owner_id, account) VALUES (1, 'work')`); sqlState(err) != "23505" {
		t.Fatalf("a second mark row for (1, work): %v; want a unique violation (23505)", err)
	}

	// The body is idempotent (--rls-on re-applies it).
	body := readMigration(t, "0010_user_credentials.sql")
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, body); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("re-running 0010: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countRows(ctx, t, pool, "cc_account_marks"); n != 3 {
		t.Fatalf("after a re-run: %d mark rows, want 3", n)
	}
	var pk string
	if err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = 'public.cc_account_marks'::regclass AND contype = 'p'`).Scan(&pk); err != nil {
		t.Fatal(err)
	}
	if pk != "PRIMARY KEY (owner_id, account)" {
		t.Fatalf("cc_account_marks key = %q, want PRIMARY KEY (owner_id, account)", pk)
	}
}

// TestUserCredentialsRowsAreShapedByKind: the table's own checks — an
// OpenRouter row always carries the key minted from it and is named 'default';
// a Claude row carries none and its name follows the account-name rule.
func TestUserCredentialsRowsAreShapedByKind(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshMigrated(ctx, t, "ms_0010_shape")
	ok := []string{
		`INSERT INTO user_credentials (owner_id, kind, name, secret_ciphertext, fingerprint) VALUES (1, 'claude', 'work', '\x00', 'f')`,
		`INSERT INTO user_credentials (owner_id, kind, name, secret_ciphertext, fingerprint, derived_ciphertext, derived_ref) VALUES (1, 'openrouter', 'default', '\x00', 'f', '\x01', 'h')`,
	}
	for _, q := range ok {
		mustExec(ctx, t, pool, q)
	}
	for name, q := range map[string]string{
		"claude with a derived key":      `INSERT INTO user_credentials (owner_id, kind, name, secret_ciphertext, fingerprint, derived_ciphertext, derived_ref) VALUES (1, 'claude', 'w2', '\x00', 'f', '\x01', 'h')`,
		"openrouter without one":         `INSERT INTO user_credentials (owner_id, kind, name, secret_ciphertext, fingerprint) VALUES (1, 'openrouter', 'default', '\x00', 'f')`,
		"openrouter not named default":   `INSERT INTO user_credentials (owner_id, kind, name, secret_ciphertext, fingerprint, derived_ciphertext, derived_ref) VALUES (1, 'openrouter', 'two', '\x00', 'f', '\x01', 'h')`,
		"a bad account name":             `INSERT INTO user_credentials (owner_id, kind, name, secret_ciphertext, fingerprint) VALUES (1, 'claude', 'Work', '\x00', 'f')`,
		"an unknown kind":                `INSERT INTO user_credentials (owner_id, kind, name, secret_ciphertext, fingerprint) VALUES (1, 'gemini', 'x', '\x00', 'f')`,
		"a second claude 'work' for one": `INSERT INTO user_credentials (owner_id, kind, name, secret_ciphertext, fingerprint) VALUES (1, 'claude', 'work', '\x00', 'f')`,
	} {
		if _, err := pool.Exec(ctx, q); err == nil {
			t.Errorf("%s: accepted", name)
		} else if s := sqlState(err); s != "23514" && s != "23505" {
			t.Errorf("%s: %v; want a check (23514) or unique (23505) violation", name, err)
		}
	}
}
