package plugins

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

func appIn(name string, caps ...Capability) NewPlugin {
	if len(caps) == 0 {
		caps = []Capability{CapApp}
	}
	return NewPlugin{Name: name, Label: "Example app", DataScope: "example", Capabilities: caps,
		Origin: hoOrigin, EntryURL: hoEntry, AppLabel: "Team calls", AppIcon: "video"}
}

// TestAnAppPluginStoresItsTabAndOrigin: an `app` token stores its origin,
// entry URL, the trimmed tab label and the icon key, and data scope `none`
// when it brings no messages. It combines with messages and viewer.
func TestAnAppPluginStoresItsTabAndOrigin(t *testing.T) {
	s, _, _ := pgStore(t)
	in := appIn("app-only")
	in.AppLabel = "  Team calls  "
	_, p, err := s.CreatePlugin(session(2), in)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Capabilities, []Capability{CapApp}) || p.DataScope != ViewerOnlyDataScope ||
		p.Origin == nil || *p.Origin != hoOrigin || p.EntryURL == nil || *p.EntryURL != hoEntry ||
		p.AppLabel == nil || *p.AppLabel != "Team calls" || p.AppIcon == nil || *p.AppIcon != "video" {
		t.Fatalf("app plugin = caps %v scope %q origin %v entry %v label %v icon %v", p.Capabilities, p.DataScope, p.Origin, p.EntryURL, p.AppLabel, p.AppIcon)
	}
	// Read back through the one column list, not only RETURNING.
	got, err := s.Plugin(session(2), p.ID)
	if err != nil || got.AppLabel == nil || *got.AppLabel != "Team calls" || got.AppIcon == nil || *got.AppIcon != "video" {
		t.Fatalf("Plugin(%d) = label %v icon %v (%v)", p.ID, got.AppLabel, got.AppIcon, err)
	}
	_, all, err := s.CreatePlugin(session(2), appIn("app-all", CapApp, CapViewer, CapMessages))
	if err != nil || !slices.Equal(all.Capabilities, []Capability{CapMessages, CapViewer, CapApp}) || all.DataScope != "example" {
		t.Fatalf("messages+viewer+app = caps %v scope %q (%v)", all.Capabilities, all.DataScope, err)
	}
	// A viewer-only token has no tab.
	v := mustViewer(t, s, 2, "viewer-only", CapViewer)
	if v.AppLabel != nil || v.AppIcon != nil {
		t.Fatalf("a viewer-only plugin has a tab: label %v icon %v", v.AppLabel, v.AppIcon)
	}
}

// TestAppAtMintRefusesEachMissingOrBadField: `app` without an origin, an
// entry URL, a label or an icon is ErrInvalid (the validator's, not a raw
// constraint error), as is a bad label or icon, and a label or icon on a
// token without `app`. Each case is its own rule.
func TestAppAtMintRefusesEachMissingOrBadField(t *testing.T) {
	s, _, _ := pgStore(t)
	cases := map[string]func(*NewPlugin){
		"no origin":            func(in *NewPlugin) { in.Origin = "" },
		"no entry URL":         func(in *NewPlugin) { in.EntryURL = "" },
		"no origin, no entry":  func(in *NewPlugin) { in.Origin, in.EntryURL = "", "" },
		"no label":             func(in *NewPlugin) { in.AppLabel = "" },
		"a blank label":        func(in *NewPlugin) { in.AppLabel = "   " },
		"no icon":              func(in *NewPlugin) { in.AppIcon = "" },
		"a 25-character label": func(in *NewPlugin) { in.AppLabel = strings.Repeat("m", 25) },
		"markup in the label":  func(in *NewPlugin) { in.AppLabel = "<b>calls" },
		"two spaces":           func(in *NewPlugin) { in.AppLabel = "Team  calls" },
		"a bidi control":       func(in *NewPlugin) { in.AppLabel = "Team‮calls" },
		"an unlisted icon":     func(in *NewPlugin) { in.AppIcon = "skull" },
		"an emoji, not a key":  func(in *NewPlugin) { in.AppIcon = "📹" },
		"a label without app": func(in *NewPlugin) {
			in.Capabilities, in.AppIcon = []Capability{CapViewer}, ""
		},
		"an icon without app": func(in *NewPlugin) {
			in.Capabilities, in.AppLabel = []Capability{CapViewer}, ""
		},
	}
	n := 0
	for why, mutate := range cases {
		n++
		in := appIn(fmt.Sprintf("app-bad-%d", n))
		mutate(&in)
		if _, _, err := s.CreatePlugin(session(2), in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v; want ErrInvalid", why, err)
		}
	}
	if list, _ := s.List(session(2)); len(list) != 0 {
		t.Fatalf("refused mints stored %d plugins", len(list))
	}
	if _, _, err := s.CreatePlugin(session(2), appIn("app-good")); err != nil {
		t.Fatalf("CONTROL: the unmutated app mint: %v", err)
	}
}

// TestASixthAppPluginIsRefused: an owner may hold MaxAppPlugins (5) live
// `app` plugins; the sixth is ErrInvalid. Revoking one makes room (a revoked
// plugin does not count), another owner is unaffected, and a non-app plugin is
// never counted or capped.
func TestASixthAppPluginIsRefused(t *testing.T) {
	s, _, _ := pgStore(t)
	var first Plugin
	for i := 1; i <= MaxAppPlugins; i++ {
		_, p, err := s.CreatePlugin(session(2), appIn(fmt.Sprintf("app-%d", i)))
		if err != nil {
			t.Fatalf("app plugin %d of %d: %v", i, MaxAppPlugins, err)
		}
		if i == 1 {
			first = p
		}
	}
	if _, _, err := s.CreatePlugin(session(2), appIn("app-6")); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "5 app tabs") {
		t.Fatalf("the sixth live app plugin: %v; want ErrInvalid naming the cap", err)
	}
	mustViewer(t, s, 2, "viewer-uncapped", CapViewer)
	if _, _, err := s.CreatePlugin(session(3), appIn("app-other-owner")); err != nil {
		t.Fatalf("another owner's first app plugin: %v", err)
	}
	if ok, err := s.Revoke(session(2), first.ID); !ok || err != nil {
		t.Fatalf("revoke: %v %v", ok, err)
	}
	if _, _, err := s.CreatePlugin(session(2), appIn("app-6")); err != nil {
		t.Fatalf("after revoking one, a fifth live app plugin: %v", err)
	}
}

// TestATeamCannotMintAnAppPlugin: an app tab is a person's in v1 (decision
// 14); the same mint as a team principal is ErrInvalid, while that team's
// viewer plugin (its manager's page mints one) is the control.
func TestATeamCannotMintAnAppPlugin(t *testing.T) {
	s, _, dsn := pgStore(t)
	const team = int64(9166)
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role, kind, display_name) OVERRIDING SYSTEM VALUE VALUES (9166, 'member', 'team', 'app-team')
		ON CONFLICT (id) DO NOTHING`)
	t.Cleanup(func() {
		dbtest.OwnerExec(t, dsn, `DELETE FROM plugins WHERE owner_id = 9166`)
		dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE user_id = 9166 OR team_id = 9166`)
		dbtest.OwnerExec(t, dsn, `DELETE FROM users WHERE id = 9166`)
	})
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: team, Team: true, Role: authz.RoleMember, Via: authz.ViaTeamAdmin})
	if _, _, err := s.CreatePlugin(ctx, appIn("team-app")); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "team") {
		t.Fatalf("a team's app mint: %v; want ErrInvalid naming the team", err)
	}
	if _, _, err := s.CreatePlugin(ctx, NewPlugin{Name: "team-viewer", Label: "x", Capabilities: []Capability{CapViewer},
		Origin: hoOrigin, EntryURL: hoEntry}); err != nil {
		t.Fatalf("CONTROL: the team's viewer mint: %v", err)
	}
}

// TestConcurrentAppMintsStopAtFive: 20 app mints for ONE owner at once, three
// rounds, leave exactly MaxAppPlugins live each round. Red if the advisory
// lock in appMintChecks goes: every mint counts the same committed rows and
// most pass (measured in audit: 20, 18 and 14 of 20 without it).
func TestConcurrentAppMintsStopAtFive(t *testing.T) {
	s, pool, _ := pgStore(t)
	const n = 20
	for round := 1; round <= 3; round++ {
		start := make(chan struct{})
		errs := make(chan error, n)
		for i := 0; i < n; i++ {
			go func(i int) {
				<-start
				_, _, err := s.CreatePlugin(session(2), appIn(fmt.Sprintf("app-r%d-%d", round, i)))
				errs <- err
			}(i)
		}
		close(start)
		ok := 0
		for i := 0; i < n; i++ {
			err := <-errs
			switch {
			case err == nil:
				ok++
			case !errors.Is(err, ErrInvalid):
				t.Fatalf("round %d: a concurrent mint failed otherwise: %v", round, err)
			}
		}
		var live int
		if err := pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM plugins
			WHERE owner_id = 2 AND revoked_at IS NULL AND 'app' = ANY (capabilities)`).Scan(&live); err != nil {
			t.Fatal(err)
		}
		if ok != MaxAppPlugins || live != MaxAppPlugins {
			t.Fatalf("round %d: %d of %d concurrent app mints succeeded, %d live; want exactly %d", round, ok, n, live, MaxAppPlugins)
		}
		list, err := s.List(session(2))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range list {
			if _, err := s.Revoke(session(2), p.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestTheAppCapCountIsReadCommitted: on a server whose default isolation is
// REPEATABLE READ, CreatePlugin still mints (it begins READ COMMITTED
// itself), and appMintChecks refuses a transaction at any other level rather
// than count from a snapshot that may predate another mint's commit.
func TestTheAppCapCountIsReadCommitted(t *testing.T) {
	_, _, dsn := pgStore(t)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	// db.Connect, so the pool carries muster's principal wiring; an unknown
	// DSN parameter is a runtime parameter to pgx.
	rr, err := db.Connect(context.Background(), dsn+sep+"default_transaction_isolation=repeatable%20read")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rr.Close)
	var def string
	if err := rr.QueryRow(context.Background(), `SHOW default_transaction_isolation`).Scan(&def); err != nil || def != "repeatable read" {
		t.Fatalf("control: the pool's default isolation is %q (%v); want repeatable read", def, err)
	}
	s := NewPG(rr)
	if _, _, err := s.CreatePlugin(session(2), appIn("app-rr")); err != nil {
		t.Fatalf("an app mint on a repeatable-read-default server: %v; want minted", err)
	}
	tx, err := rr.BeginTx(session(2), pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := s.appMintChecks(session(2), tx, false); err == nil || !strings.Contains(err.Error(), "read committed") {
		t.Fatalf("appMintChecks in a repeatable read transaction: %v; want refused naming read committed", err)
	}
}
