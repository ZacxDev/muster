package agents

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestDisplayNameSourceIsStoredAndARenameIsManual: Create stores the source it
// is given ("" as default); a rename (SetDisplayName) marks the name manual —
// what no automatic name may replace — and a blank rename returns the slug as
// a default name.
func TestDisplayNameSourceIsStoredAndARenameIsManual(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := NewPG(pool)
	tag := time.Now().Format("150405.000000")
	var created []int64
	defer func() {
		for _, id := range created {
			cleanupAgent(ctx, pool, id)
		}
	}()
	mk := func(suffix, display, src string) Agent {
		a, err := store.Create(ctx, Agent{Name: "dns-" + suffix + "-" + tag, Namespace: "devpod-dns-" + suffix + "-" + tag,
			DisplayName: display, DisplayNameSource: src, Status: StatusRunning})
		if err != nil {
			t.Fatalf("create %s: %v", suffix, err)
		}
		created = append(created, a.ID)
		return a
	}
	if a := mk("task", "#9 · Fix it", NameDispatch); a.DisplayNameSource != NameDispatch {
		t.Errorf("dispatch: stored %q", a.DisplayNameSource)
	}
	if a := mk("role", "chief", NameSystem); a.DisplayNameSource != NameSystem {
		t.Errorf("system: stored %q", a.DisplayNameSource)
	}
	plain := mk("plain", "", "")
	if plain.DisplayNameSource != NameDefault {
		t.Errorf("unset: stored %q, want %q", plain.DisplayNameSource, NameDefault)
	}
	if plain.DisplayNameAt.IsZero() {
		t.Fatal("a created agent has no display_name_at")
	}
	time.Sleep(10 * time.Millisecond)
	a, err := store.SetDisplayName(ctx, plain.ID, "Billing")
	if err != nil || a.DisplayName != "Billing" || a.DisplayNameSource != NameManual {
		t.Fatalf("rename: %q %q (%v), want Billing / manual", a.DisplayName, a.DisplayNameSource, err)
	}
	// The drift offer's clock for an agent's name (migration 0091): a rename
	// moves it.
	if !a.DisplayNameAt.After(plain.DisplayNameAt) {
		t.Fatalf("rename: display_name_at %v not after creation's %v", a.DisplayNameAt, plain.DisplayNameAt)
	}
	if got, _ := store.Get(ctx, plain.ID); got.DisplayNameSource != NameManual {
		t.Fatalf("rename read back: %q", got.DisplayNameSource)
	}
	a, err = store.SetDisplayName(ctx, plain.ID, "")
	if err != nil || a.DisplayName != plain.Name || a.DisplayNameSource != NameDefault {
		t.Fatalf("blank rename: %q %q (%v), want the slug / default", a.DisplayName, a.DisplayNameSource, err)
	}
}
