package agents

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestClearKickoffFailureErasesOnlyTheRecord pins ClearKickoffFailure's SQL on a
// kicked-off row whose first turn failed: the record goes (KickoffFailed reads
// false), and NOTHING ELSE MOVES — kicked_off stays stamped and kickoff_attempts
// unchanged (at-most-once: no re-send is armed), and updated_at stays put (the
// dwell clock; see the PGStore method's doc).
func TestClearKickoffFailureErasesOnlyTheRecord(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	store := requirePGStore(t, ctx)

	name := "kickoff-clear-" + time.Now().Format("150405.000000")
	created, err := store.Create(ctx, Agent{
		Name: name, Namespace: "muster-agent-" + name, Status: StatusRunning,
		PendingNote: "fixture first turn",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer cleanupAgent(ctx, store.pool, created.ID)
	if err := store.SetKickedOff(ctx, created.ID, true); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if err := store.RecordKickoffDelivery(ctx, created.ID, "pod-a", 0); err != nil {
		t.Fatalf("record delivery: %v", err)
	}
	if err := store.SetKickoffError(ctx, created.ID, "kickoff turn failed: context deadline exceeded"); err != nil {
		t.Fatalf("set kickoff error: %v", err)
	}
	if _, err := store.pool.Exec(ctx,
		`UPDATE agents SET updated_at = now() - interval '14 minutes' WHERE id=$1`, created.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	before, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !KickoffFailed(before) || before.KickoffAttempts != 1 {
		t.Fatalf("instrument check FAILED: fixture is not a failed kickoff (failed=%v attempts=%d)",
			KickoffFailed(before), before.KickoffAttempts)
	}

	if err := store.ClearKickoffFailure(ctx, created.ID); err != nil {
		t.Fatalf("ClearKickoffFailure: %v", err)
	}
	after, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if KickoffFailed(after) {
		t.Fatalf("KickoffFailed still true after ClearKickoffFailure (kickoff_error %q)", after.KickoffError)
	}
	if !after.KickedOff || after.KickoffAttempts != before.KickoffAttempts {
		t.Errorf("ClearKickoffFailure changed kickoff state: kicked_off %v→%v, attempts %d→%d — "+
			"a later success must not re-arm (or burn) a first turn",
			before.KickedOff, after.KickedOff, before.KickoffAttempts, after.KickoffAttempts)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("ClearKickoffFailure moved updated_at from %s to %s", before.UpdatedAt, after.UpdatedAt)
	}
}

// TestClearKickoffFailureLeavesAnOwedRowAlone: on a row that is NOT kicked off,
// kickoff_error is the checkout-wait marker (or a not-delivered record), and an
// operator's chat turn to that agent completing must not erase it — the
// `kicked_off` conjunct is what refuses it.
func TestClearKickoffFailureLeavesAnOwedRowAlone(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	store := requirePGStore(t, ctx)

	name := "kickoff-owed-" + time.Now().Format("150405.000000")
	created, err := store.Create(ctx, Agent{
		Name: name, Namespace: "muster-agent-" + name, Status: StatusRunning,
		PendingNote: "fixture first turn",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer cleanupAgent(ctx, store.pool, created.ID)
	marker := KickoffAwaitingGrantReason + ": repo access not granted yet"
	if err := store.SetKickoffError(ctx, created.ID, marker); err != nil {
		t.Fatalf("set marker: %v", err)
	}
	if err := store.ClearKickoffFailure(ctx, created.ID); err != nil {
		t.Fatalf("ClearKickoffFailure: %v", err)
	}
	after, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.KickedOff || after.KickoffError != marker {
		t.Fatalf("ClearKickoffFailure touched a not-kicked-off row: kicked_off=%v kickoff_error=%q, want false / %q",
			after.KickedOff, after.KickoffError, marker)
	}
}

// TestClearKickoffFailureKeepsAResendSafeCause pins the operator decision on
// which causes a later completed turn clears. A cause that PROVES the task was
// never delivered (KickoffResendSafe: never connected, not accepted, busy) keeps
// its badge and re-send remedy — the later turn did not carry the task. A cause
// under which the task may have arrived (a timeout, an empty reply) clears.
//
// Each resend-safe row is its own subtest so dropping ONE prefix conjunct from
// the SQL fails exactly that subtest.
func TestClearKickoffFailureKeepsAResendSafeCause(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	store := requirePGStore(t, ctx)

	for _, c := range []struct {
		name  string
		cause string
		kept  bool
	}{
		{"never connected", KickoffNeverConnectedReason + ": dial tcp: connection refused", true},
		{"not accepted", KickoffNotAcceptedReason + ": runtime 503 not_ready", true},
		{"busy", KickoffBusyReason + ": runtime 409 busy", true},
		{"timeout", "kickoff turn failed after it was handed to the gateway: context deadline exceeded", false},
		{"empty reply", KickoffEmptyReplyReason, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			name := "kickoff-safe-" + time.Now().Format("150405.000000")
			created, err := store.Create(ctx, Agent{
				Name: name, Namespace: "muster-agent-" + name, Status: StatusRunning,
				PendingNote: "fixture first turn",
			})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			defer cleanupAgent(ctx, store.pool, created.ID)
			if err := store.SetKickedOff(ctx, created.ID, true); err != nil {
				t.Fatalf("stamp: %v", err)
			}
			if err := store.SetKickoffError(ctx, created.ID, c.cause); err != nil {
				t.Fatalf("set kickoff error: %v", err)
			}
			before, err := store.Get(ctx, created.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if !KickoffFailed(before) || KickoffResendSafe(before) != c.kept {
				t.Fatalf("instrument check FAILED: fixture failed=%v resendSafe=%v, want true/%v",
					KickoffFailed(before), KickoffResendSafe(before), c.kept)
			}
			if err := store.ClearKickoffFailure(ctx, created.ID); err != nil {
				t.Fatalf("ClearKickoffFailure: %v", err)
			}
			after, err := store.Get(ctx, created.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if c.kept && (!KickoffFailed(after) || !KickoffResendSafe(after) || after.KickoffError != c.cause) {
				t.Fatalf("a later completed turn erased a RESEND-SAFE cause (%s): kickoff_error now %q — "+
					"the task was never delivered, so the badge and its re-send remedy must stay", c.name, after.KickoffError)
			}
			if !c.kept && KickoffFailed(after) {
				t.Fatalf("a later completed turn did not clear a cause under which the task may have "+
					"arrived (%s): kickoff_error still %q", c.name, after.KickoffError)
			}
		})
	}
}
