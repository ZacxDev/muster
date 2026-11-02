package agents

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestTheOwnerTurnMarkerIsPerAgentAndClearedOnlyByItsOwnTurn: a marker reads
// back on its agent only; a clear naming ANOTHER turn's row leaves it; a second
// turn overwrites it and the first turn's late clear does not remove the
// second's; its own clear removes it; a lapsed one reads as none. Ids are
// pairwise distinct.
func TestTheOwnerTurnMarkerIsPerAgentAndClearedOnlyByItsOwnTurn(t *testing.T) {
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
	mk := func(suffix string) Agent {
		a, err := store.Create(ctx, Agent{Name: "otm-" + suffix + "-" + tag, Namespace: "devpod-otm-" + suffix + "-" + tag, Status: StatusRunning})
		if err != nil {
			t.Fatalf("create agent %s: %v", suffix, err)
		}
		created = append(created, a.ID)
		return a
	}
	chief, other := mk("chief"), mk("other")
	read := func(a Agent) OwnerTurn {
		t.Helper()
		got, err := store.OwnerTurnInFlight(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := read(chief); got.MessageID != 0 {
		t.Fatalf("a fresh agent has a marker: %+v", got)
	}
	if err := store.SetOwnerTurn(ctx, chief.ID, 9001, 7001, "lane-one", time.Minute); err != nil {
		t.Fatal(err)
	}
	if got := read(chief); got.By != 9001 || got.MessageID != 7001 || got.Lane != "lane-one" {
		t.Fatalf("marker %+v; want by 9001, message 7001", got)
	}
	if got := read(other); got.MessageID != 0 {
		t.Fatalf("another agent reads the marker: %+v", got)
	}
	if err := store.ClearOwnerTurn(ctx, chief.ID, 7999); err != nil {
		t.Fatal(err)
	}
	if got := read(chief); got.MessageID != 7001 {
		t.Fatalf("another turn's clear removed the marker: %+v", got)
	}
	if err := store.SetOwnerTurn(ctx, chief.ID, 9001, 7002, "lane-two", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.ClearOwnerTurn(ctx, chief.ID, 7001); err != nil { // the first turn ends late
		t.Fatal(err)
	}
	if got := read(chief); got.MessageID != 7002 || got.Lane != "lane-two" {
		t.Fatalf("the first turn's late clear removed the second's marker: %+v", got)
	}
	if err := store.ClearOwnerTurn(ctx, chief.ID, 7002); err != nil {
		t.Fatal(err)
	}
	if got := read(chief); got.MessageID != 0 {
		t.Fatalf("its own clear left the marker: %+v", got)
	}
	if err := store.SetOwnerTurn(ctx, chief.ID, 9001, 7003, "lane-three", -time.Second); err != nil {
		t.Fatal(err)
	}
	if got := read(chief); got.MessageID != 0 {
		t.Fatalf("a lapsed marker reads as in flight: %+v", got)
	}
}
