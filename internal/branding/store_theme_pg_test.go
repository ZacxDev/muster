package branding

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/theme"
)

// themeStore is pgStore with no theme audit rows left by another test (the
// package's tests share one database).
func themeStore(t *testing.T) *PG {
	t.Helper()
	p, dsn := pgStore(t)
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action = $1`, ThemeAuditAction)
	return p
}

// themeAudits is every theme audit row, oldest first: who, via, and detail.
func themeAudits(t *testing.T, p *PG) []struct {
	UserID int64
	Via    string
	Detail map[string]map[string]string
} {
	t.Helper()
	rows, err := p.pool.Query(authz.System(context.Background()),
		`SELECT coalesce(user_id, 0), via, detail FROM audit_log WHERE action = $1 ORDER BY id`, ThemeAuditAction)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []struct {
		UserID int64
		Via    string
		Detail map[string]map[string]string
	}
	for rows.Next() {
		var r struct {
			UserID int64
			Via    string
			Detail map[string]map[string]string
		}
		var raw []byte
		if err := rows.Scan(&r.UserID, &r.Via, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &r.Detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// TestAThemeIsStoredAuditedAndKeptBesideTheBranding: an admin's theme is read
// back (by an anonymous context too), keeps the name and logo it sits beside,
// and is audited — who, from what, to what — once per change; a name-only
// change keeps the theme and writes no theme audit.
func TestAThemeIsStoredAuditedAndKeptBesideTheBranding(t *testing.T) {
	p := themeStore(t)
	admin := as(3, authz.RoleAdmin)
	if _, err := p.Apply(admin, Change{Name: strp("Team Example")}); err != nil {
		t.Fatal(err)
	}
	b, err := p.Apply(admin, Change{Theme: &theme.Choice{Preset: "harbor", Accent: "#5AA9F0"}})
	if err != nil {
		t.Fatal(err)
	}
	want := theme.Choice{Preset: "harbor", Accent: "#5aa9f0"}
	if b.Theme != want || b.Name != "Team Example" {
		t.Fatalf("after the theme save: %+v; want %+v and the name kept", b, want)
	}
	if got, err := p.Load(context.Background()); err != nil || got.Theme != want {
		t.Fatalf("anonymous read: %+v err=%v", got.Theme, err)
	}
	if b, err = p.Apply(admin, Change{Name: strp("Renamed")}); err != nil || b.Theme != want {
		t.Fatalf("a name-only change: %+v err=%v; the theme must stay", b.Theme, err)
	}
	a := themeAudits(t, p)
	if len(a) != 1 {
		t.Fatalf("%d theme audit rows, want exactly 1 (the name changes are not theme changes)", len(a))
	}
	if a[0].UserID != 3 || a[0].Via != string(authz.ViaSession) ||
		a[0].Detail["from"]["preset"] != "" || a[0].Detail["to"]["preset"] != "harbor" || a[0].Detail["to"]["accent"] != "#5aa9f0" {
		t.Errorf("audit row = %+v; want user 3 via session, from nothing to harbor #5aa9f0", a[0])
	}
	// Back to the default: stored as the default preset with no accent,
	// audited from harbor.
	if b, err = p.Apply(admin, Change{Theme: &theme.Choice{}}); err != nil || !b.Theme.IsDefault() {
		t.Fatalf("back to the default: %+v err=%v", b.Theme, err)
	}
	if a = themeAudits(t, p); len(a) != 2 || a[1].Detail["from"]["preset"] != "harbor" || a[1].Detail["to"]["accent"] != "" {
		t.Errorf("second audit = %+v", a)
	}
}

// TestANonCompliantThemeIsRefusedAndNothingIsWritten: an accent that fails
// the contrast table (#3A2F6B, no preset's) is refused with the pairs named;
// the stored theme and the audit log are as they were. So is an unknown
// preset, and a member's theme (before anything is checked).
func TestANonCompliantThemeIsRefusedAndNothingIsWritten(t *testing.T) {
	p := themeStore(t)
	admin := as(3, authz.RoleAdmin)
	if _, err := p.Apply(admin, Change{Theme: &theme.Choice{Preset: "ember"}}); err != nil {
		t.Fatal(err)
	}
	_, err := p.Apply(admin, Change{Theme: &theme.Choice{Preset: "ember", Accent: "#3a2f6b"}})
	var ce *ThemeContrastError
	if !errors.As(err, &ce) || !errors.Is(err, ErrThemeContrast) || len(ce.Failures) == 0 {
		t.Fatalf("a dark accent: %v; want a *ThemeContrastError naming pairs", err)
	}
	if _, err := p.Apply(admin, Change{Theme: &theme.Choice{Preset: "neon"}}); !errors.Is(err, ErrThemePreset) {
		t.Errorf("an unknown preset: %v, want ErrThemePreset", err)
	}
	if _, err := p.Apply(admin, Change{Theme: &theme.Choice{Accent: "#zzzzzz"}}); !errors.Is(err, ErrThemeAccent) {
		t.Errorf("a malformed accent: %v, want ErrThemeAccent", err)
	}
	if _, err := p.Apply(as(2, authz.RoleMember), Change{Theme: &theme.Choice{Preset: "harbor"}}); !errors.Is(err, ErrNotAdmin) {
		t.Errorf("a member: %v, want ErrNotAdmin", err)
	}
	if got, _ := p.Load(context.Background()); got.Theme != (theme.Choice{Preset: "ember"}) {
		t.Errorf("after the refusals the theme is %+v; want ember, untouched", got.Theme)
	}
	if a := themeAudits(t, p); len(a) != 1 {
		t.Errorf("%d theme audit rows; a refused change must not be audited", len(a))
	}
}
