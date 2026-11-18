package db_test

import (
	"strings"
	"testing"
)

// TestMigration0065AddsAnAbsoluteOrUnknownCwd: at version 64 agent_focus has
// no cwd (the control); after 0065 a row from before reads an empty cwd (unknown), an
// absolute directory is stored, and a relative or over-long one is refused by
// agent_focus_cwd_check. Fixture paths are invented placeholders.
func TestMigration0065AddsAnAbsoluteOrUnknownCwd(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0065_focus_cwd", 64)
	insert := func(key string) error {
		_, err := pool.Exec(ctx, `INSERT INTO agent_focus (owner_id, subject_kind, subject_key, title) VALUES (1, 'mirror', $1, 'A focus')`, key)
		return err
	}
	if err := insert("cwd-old"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_focus SET cwd = '/srv/example-work' WHERE subject_key = 'cwd-old'`); err == nil ||
		!strings.Contains(err.Error(), "cwd") {
		t.Fatalf("control: at version 64 agent_focus has a cwd column (%v)", err)
	}

	applyThroughFrom(ctx, t, pool, 65)

	var cwd string
	if err := pool.QueryRow(ctx, `SELECT cwd FROM agent_focus WHERE subject_key = 'cwd-old'`).Scan(&cwd); err != nil || cwd != "" {
		t.Fatalf("a row from before 0065: cwd %q (%v); want '' (unknown)", cwd, err)
	}
	set := func(v string) error {
		_, err := pool.Exec(ctx, `UPDATE agent_focus SET cwd = $1 WHERE subject_key = 'cwd-old'`, v)
		return err
	}
	if err := set("/srv/example-work/app"); err != nil {
		t.Fatalf("an absolute directory: %v; want stored", err)
	}
	for _, bad := range []string{"srv/example-work", "/" + strings.Repeat("d", 512)} {
		if err := set(bad); err == nil || !strings.Contains(err.Error(), "agent_focus_cwd_check") {
			t.Errorf("cwd %.20q…: %v; want refused by agent_focus_cwd_check", bad, err)
		}
	}
}
