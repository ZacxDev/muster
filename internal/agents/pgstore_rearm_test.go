package agents

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestRearmKickoffMakesADeliveredFirstTurnOwedAgain: an agent whose first turn
// was delivered (kicked_off, a recipient pod, attempts spent, an error left
// over) is, after RearmKickoff, exactly what the kickoff deliverer owes a turn
// to — kicked_off false, the NEW note pending, the delivery markers cleared —
// and nothing else about the row moves.
func TestRearmKickoffMakesADeliveredFirstTurnOwedAgain(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	store := requirePGStore(t, ctx)
	name := "officer-rearm-" + time.Now().Format("150405.000000")
	a, err := store.Create(ctx, Agent{Name: name, Namespace: "muster-agent-" + name, Status: StatusRunning, PendingNote: "first wake", NoteText: "first wake", DisplayName: "officer x"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupAgent(ctx, store.pool, a.ID) })
	if err := store.SetKickedOff(ctx, a.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordKickoffDelivery(ctx, a.ID, "pod-abc", 2); err != nil {
		t.Fatal(err)
	}
	if err := store.SetKickoffError(ctx, a.ID, "a stale send error"); err != nil {
		t.Fatal(err)
	}
	before, _ := store.Get(ctx, a.ID)
	if !before.KickedOff || before.KickoffPod != "pod-abc" || before.KickoffAttempts != 1 || before.KickoffError == "" {
		t.Fatalf("fixture did not reach the delivered state: %+v", before)
	}

	if err := store.RearmKickoff(ctx, a.ID, "second wake"); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.KickedOff || got.PendingNote != "second wake" || got.KickoffPod != "" || got.KickoffRestarts != 0 || got.KickoffAttempts != 0 || got.KickoffError != "" {
		t.Fatalf("after RearmKickoff: kicked_off %v note %q pod %q restarts %d attempts %d error %q",
			got.KickedOff, got.PendingNote, got.KickoffPod, got.KickoffRestarts, got.KickoffAttempts, got.KickoffError)
	}
	if got.Name != before.Name || got.Status != before.Status || got.NoteText != before.NoteText || got.DisplayName != before.DisplayName {
		t.Fatalf("RearmKickoff moved an unrelated column: before %+v after %+v", before, got)
	}
}
