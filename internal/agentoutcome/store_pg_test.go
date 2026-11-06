package agentoutcome

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// pgFixture is the real store over a migrated database, and an agent row.
func pgFixture(t *testing.T) (context.Context, *PGStore, *agents.PGStore, agents.Agent) {
	t.Helper()
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ag := agents.NewPG(pool)
	name := "outcome-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	a, err := ag.Create(ctx, agents.Agent{Name: name, Namespace: "devpod-" + name, Status: agents.StatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	return ctx, NewPG(pool), ag, a
}

func reread(t *testing.T, ctx context.Context, ag *agents.PGStore, id int64) agents.Agent {
	t.Helper()
	a, err := ag.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestTheOutcomeStoreAppliesEachChangeOnce pins the store's conditional writes
// against the REAL SQL — the property "one push per change" rests on: of two
// writers of one change, exactly one sees applied.
//
//   - Start: running, clocks set, the chief recorded; and NO updated_at move.
//   - Fail: applied the first time, NOT the second (a repeated failure pushes
//     nothing); status error with the reason; updated_at moves (a real transition).
//   - MarkStuck: refused when activity moved since the read, applied when not,
//     refused again once stuck.
//   - Activity: recovers stuck → running exactly once.
//   - Transition / Clear: only from the named outcomes.
func TestTheOutcomeStoreAppliesEachChangeOnce(t *testing.T) {
	ctx, s, ag, a := pgFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	chief := int64(424242)

	if err := s.Start(ctx, a.ID, &chief, now); err != nil {
		t.Fatal(err)
	}
	got := reread(t, ctx, ag, a.ID)
	if got.Outcome != Running || got.DispatchedBy == nil || *got.DispatchedBy != chief ||
		got.ActivityAt == nil || !got.ActivityAt.Equal(now) || got.FirstReplyAt != nil {
		t.Fatalf("after Start: %+v", got)
	}
	if !got.UpdatedAt.Equal(a.UpdatedAt) {
		t.Errorf("Start moved updated_at (%s → %s): the kickoff/promoter verdicts compare it", a.UpdatedAt, got.UpdatedAt)
	}

	// MarkStuck on a stale read is refused: activity moved since.
	seen := got.ActivityAt
	if _, err := s.Activity(ctx, a.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if applied, err := s.MarkStuck(ctx, a.ID, ReasonNoReply, now.Add(10*time.Minute), seen); err != nil || applied {
		t.Fatalf("MarkStuck over a moved activity: applied=%v err=%v; want refused", applied, err)
	}
	got = reread(t, ctx, ag, a.ID)
	if applied, err := s.MarkStuck(ctx, a.ID, ReasonNoReply, now.Add(10*time.Minute), got.ActivityAt); err != nil || !applied {
		t.Fatalf("MarkStuck on the current read: applied=%v err=%v; want applied", applied, err)
	}
	if applied, _ := s.MarkStuck(ctx, a.ID, ReasonNoReply, now.Add(11*time.Minute), got.ActivityAt); applied {
		t.Fatal("MarkStuck applied twice")
	}
	if got = reread(t, ctx, ag, a.ID); got.Outcome != Stuck || got.OutcomeReason != ReasonNoReply {
		t.Fatalf("after MarkStuck: %q %q", got.Outcome, got.OutcomeReason)
	}

	// Activity recovers it, once.
	rec, err := s.Activity(ctx, a.ID, now.Add(12*time.Minute))
	if err != nil || !rec {
		t.Fatalf("Activity on a stuck agent: recovered=%v err=%v; want recovered", rec, err)
	}
	if rec, _ := s.Activity(ctx, a.ID, now.Add(13*time.Minute)); rec {
		t.Fatal("Activity recovered a running agent")
	}
	if got = reread(t, ctx, ag, a.ID); got.Outcome != Running || got.OutcomeReason != "" {
		t.Fatalf("after recovery: %q %q", got.Outcome, got.OutcomeReason)
	}

	// Fail: once.
	before := got.UpdatedAt
	applied, err := s.Fail(ctx, a.ID, "a turn failed: boom", true, now.Add(14*time.Minute))
	if err != nil || !applied {
		t.Fatalf("first Fail: applied=%v err=%v", applied, err)
	}
	got = reread(t, ctx, ag, a.ID)
	if got.Outcome != Failed || got.Status != agents.StatusError || got.ErrorMessage != "a turn failed: boom" {
		t.Fatalf("after Fail: outcome %q status %q error %q", got.Outcome, got.Status, got.ErrorMessage)
	}
	if !got.UpdatedAt.After(before) {
		t.Error("Fail with a status did not move updated_at")
	}
	if applied, _ := s.Fail(ctx, a.ID, "a turn failed: again", true, now.Add(15*time.Minute)); applied {
		t.Fatal("a second failure applied again (it would push twice)")
	}

	// Transition only from what it names; Clear likewise.
	if applied, _ := s.Transition(ctx, a.ID, []string{Running, Stuck}, Done, "", now); applied {
		t.Fatal("Transition moved a failed agent it did not name")
	}
	if cleared, _ := s.Clear(ctx, a.ID, []string{Stuck}); cleared {
		t.Fatal("Clear cleared an outcome it did not name")
	}
	if cleared, err := s.Clear(ctx, a.ID, []string{Failed}); err != nil || !cleared {
		t.Fatalf("Clear(failed): %v %v", cleared, err)
	}
	if got = reread(t, ctx, ag, a.ID); got.Outcome != "" {
		t.Fatalf("after Clear: %q", got.Outcome)
	}

	// Fail without a status (the chief): the outcome moves, the status does not.
	b, err := ag.Create(ctx, agents.Agent{Name: a.Name + "-b", Namespace: "devpod-" + a.Name + "-b", Status: agents.StatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := s.Fail(ctx, b.ID, "a turn failed: x", false, now); err != nil || !applied {
		t.Fatalf("Fail(no status): %v %v", applied, err)
	}
	if got := reread(t, ctx, ag, b.ID); got.Status != agents.StatusRunning || got.Outcome != Failed {
		t.Fatalf("Fail(no status): status %q outcome %q", got.Status, got.Outcome)
	}
}
