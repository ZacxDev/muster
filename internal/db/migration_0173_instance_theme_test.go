package db_test

import (
	"strings"
	"testing"
)

// TestMigration0173InstanceTheme: a database at 0172 holding a branding row
// has neither theme column (the control); at 0173 that row reads both NULL —
// the default theme, as before. A preset name of the wrong shape and an
// accent that is not lower-case #rrggbb are refused by the column's check.
// The header's ROLLBACK runs as written, leaves neither column and keeps the
// row; the file then applies again.
func TestMigration0173InstanceTheme(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0173_theme", 172)
	mustExec(ctx, t, pool, `INSERT INTO instance_branding (name) VALUES ('Team Example')`)
	for _, col := range []string{"theme_preset", "theme_accent"} {
		if _, err := pool.Exec(ctx, `SELECT `+col+` FROM instance_branding`); err == nil {
			t.Fatalf("control: before 0173 %s exists", col)
		}
	}
	applyThroughFrom(ctx, t, pool, 173)

	read := func() (preset, accent *string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT theme_preset, theme_accent FROM instance_branding`).Scan(&preset, &accent); err != nil {
			t.Fatal(err)
		}
		return
	}
	if p, a := read(); p != nil || a != nil {
		t.Fatalf("the existing row reads preset=%v accent=%v; want both NULL (the default theme)", p, a)
	}
	mustExec(ctx, t, pool, `UPDATE instance_branding SET theme_preset = 'harbor', theme_accent = '#5aa9f0'`)
	if p, a := read(); p == nil || *p != "harbor" || a == nil || *a != "#5aa9f0" {
		t.Fatalf("a valid theme was not stored: %v %v", p, a)
	}
	for name, q := range map[string]string{
		"an upper-case accent":     `UPDATE instance_branding SET theme_accent = '#5AA9F0'`,
		"a short accent":           `UPDATE instance_branding SET theme_accent = '#5aa'`,
		"an accent with no #":      `UPDATE instance_branding SET theme_accent = '5aa9f0'`,
		"a named colour":           `UPDATE instance_branding SET theme_accent = 'red'`,
		"an empty preset":          `UPDATE instance_branding SET theme_preset = ''`,
		"a preset with a space":    `UPDATE instance_branding SET theme_preset = 'har bor'`,
		"a preset of 33 letters":   `UPDATE instance_branding SET theme_preset = repeat('a', 33)`,
		"a preset with upper case": `UPDATE instance_branding SET theme_preset = 'Harbor'`,
	} {
		if _, err := pool.Exec(ctx, q); err == nil || !strings.Contains(err.Error(), "check") {
			t.Errorf("%s: %v, want a check-constraint refusal", name, err)
		}
	}

	for _, q := range asksRollbackSQL(t, "0173_instance_theme.sql") {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	for _, col := range []string{"theme_preset", "theme_accent"} {
		if _, err := pool.Exec(ctx, `SELECT `+col+` FROM instance_branding`); err == nil {
			t.Errorf("after the rollback %s still exists", col)
		}
	}
	if n := countRows(ctx, t, pool, "instance_branding"); n != 1 {
		t.Errorf("the rollback left %d branding rows, want the 1 it found", n)
	}
	applyThroughFrom(ctx, t, pool, 173)
	if p, a := read(); p != nil || a != nil {
		t.Errorf("re-applied after the rollback: preset=%v accent=%v", p, a)
	}
}
