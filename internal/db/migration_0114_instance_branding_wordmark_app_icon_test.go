package db_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// TestMigration0114WordmarkAndAppIcon: a database at 0113 holding a branding
// row (written as an older image writes it) has none of the new columns (the
// control); at 0114 that row reads logo_shows_name false — "draw the name", as
// before — and no app icon, and so does a row inserted without the columns.
// NULL logo_shows_name, half an app icon and an app icon with no logo are
// refused by name. With an app icon set, the header's ROLLBACK runs as
// written, leaves none of the columns, and puts back the ETag of the LOGO's
// icons (what an older image serves); the file then applies again.
func TestMigration0114WordmarkAndAppIcon(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0114_wordmark", 113)
	mustExec(ctx, t, pool, `INSERT INTO instance_branding (name) VALUES ('Team Example')`)
	for _, col := range []string{"logo_shows_name", "appicon192_png", "appicon512_png"} {
		if _, err := pool.Exec(ctx, `SELECT `+col+` FROM instance_branding`); err == nil {
			t.Fatalf("control: before 0114 %s exists", col)
		}
	}
	applyThroughFrom(ctx, t, pool, 114)

	read := func() (shows, icon bool) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT logo_shows_name, appicon192_png IS NOT NULL FROM instance_branding`).Scan(&shows, &icon); err != nil {
			t.Fatal(err)
		}
		return
	}
	if shows, icon := read(); shows || icon {
		t.Fatalf("the existing row reads logo_shows_name=%v app icon=%v; want false, none", shows, icon)
	}
	mustExec(ctx, t, pool, `DELETE FROM instance_branding`)
	mustExec(ctx, t, pool, `INSERT INTO instance_branding (name) VALUES ('Team Example')`)
	if shows, _ := read(); shows {
		t.Fatal("a row inserted without the column reads true; want the default false")
	}
	mustExec(ctx, t, pool, `UPDATE instance_branding SET logo_shows_name = true`)
	if shows, _ := read(); !shows {
		t.Fatal("true was not stored")
	}
	for name, c := range map[string]struct{ q, constraint string }{
		"a NULL logo_shows_name":   {`UPDATE instance_branding SET logo_shows_name = NULL`, "logo_shows_name"},
		"an app icon with no logo": {`UPDATE instance_branding SET appicon192_png = '\x0a', appicon512_png = '\x0b'`, "instance_branding_app_icon_needs_logo"},
	} {
		if _, err := pool.Exec(ctx, c.q); err == nil || !strings.Contains(err.Error(), c.constraint) {
			t.Errorf("%s: %v, want a refusal naming %s", name, err, c.constraint)
		}
	}

	// A logo whose own icons are 0x02/0x03, and an app icon 0x0a/0x0b served
	// in their place under an ETag over logo ‖ app icon.
	mustExec(ctx, t, pool, `UPDATE instance_branding SET logo_png = '\x01', icon192_png = '\x02', icon512_png = '\x03',
		logo_etag = '0123456789abcdef', appicon192_png = '\x0a', appicon512_png = '\x0b'`)
	if _, err := pool.Exec(ctx, `UPDATE instance_branding SET appicon512_png = NULL`); err == nil || !strings.Contains(err.Error(), "instance_branding_app_icon_whole") {
		t.Errorf("half an app icon: %v, want instance_branding_app_icon_whole", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE instance_branding SET logo_png = NULL, icon192_png = NULL, icon512_png = NULL, logo_etag = NULL`); err == nil || !strings.Contains(err.Error(), "instance_branding_app_icon_needs_logo") {
		t.Errorf("clearing the logo under an app icon: %v, want instance_branding_app_icon_needs_logo", err)
	}

	for _, q := range asksRollbackSQL(t, "0114_instance_branding_wordmark_app_icon.sql") {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	for _, col := range []string{"logo_shows_name", "appicon192_png", "appicon512_png"} {
		if _, err := pool.Exec(ctx, `SELECT `+col+` FROM instance_branding`); err == nil {
			t.Errorf("after the rollback %s still exists", col)
		}
	}
	sum := sha256.Sum256([]byte{0x01, 0x02, 0x03})
	var etag string
	if err := pool.QueryRow(ctx, `SELECT logo_etag FROM instance_branding`).Scan(&etag); err != nil {
		t.Fatal(err)
	}
	if want := hex.EncodeToString(sum[:])[:16]; etag != want {
		t.Errorf("after the rollback logo_etag = %s, want %s: the sha256 over the logo and ITS icons", etag, want)
	}
	applyThroughFrom(ctx, t, pool, 114)
	if shows, icon := read(); shows || icon {
		t.Errorf("re-applied after the rollback: logo_shows_name=%v app icon=%v", shows, icon)
	}
}
