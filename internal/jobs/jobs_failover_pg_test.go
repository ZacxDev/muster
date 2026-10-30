package jobs

import (
	"testing"
	"time"
)

// TestRequeueIsOncePerAccountAndOnlyFromRunning: the account failover's
// re-queue (migration 0066's jobs.failovers). A running job goes back to
// queued with the failover recorded; a second re-queue off the SAME account is
// refused (so a job never loops); one off another account is accepted; a job
// that is not running is never re-queued; and the failovers read back in
// order, as written.
func TestRequeueIsOncePerAccountAndOnlyFromRunning(t *testing.T) {
	s, pool := pgStore(t)
	a := mustAgent(t, pool, 1, "job-failover")
	ctx := as(1)
	j, _, err := s.Create(ctx, NewJob{AgentID: a.ID, Agent: a.Name, Prompt: "review", TimeoutSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Requeue(ctx, j.ID, Failover{From: "north", To: "south", Reason: "rate_limited"}); err != nil || ok {
		t.Fatalf("a QUEUED job re-queued = %v, %v; want refused", ok, err)
	}
	at := time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)
	start := func() {
		t.Helper()
		if ok, err := s.Start(ctx, j.ID, "x", 0); err != nil || !ok {
			t.Fatalf("start = %v, %v", ok, err)
		}
	}
	start()
	if ok, err := s.Requeue(ctx, j.ID, Failover{From: "north", To: "south", Reason: "rate_limited", At: at}); err != nil || !ok {
		t.Fatalf("first re-queue off north = %v, %v; want accepted", ok, err)
	}
	got, err := s.Get(ctx, j.ID)
	if err != nil || got.Status != StatusQueued {
		t.Fatalf("after the re-queue: %+v (%v); want queued", got, err)
	}
	start()
	if ok, err := s.Requeue(ctx, j.ID, Failover{From: "north", To: "east", Reason: "rate_limited"}); err != nil || ok {
		t.Fatalf("a second re-queue off north = %v, %v; want refused", ok, err)
	}
	if ok, err := s.Requeue(ctx, j.ID, Failover{From: "south", To: "north", Reason: "auth_failed", At: at}); err != nil || !ok {
		t.Fatalf("a re-queue off south = %v, %v; want accepted", ok, err)
	}
	got, err = s.Get(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Failovers) != 2 || got.Failovers[0] != (Failover{From: "north", To: "south", Reason: "rate_limited", At: at}) ||
		got.Failovers[1].From != "south" || got.Failovers[1].Reason != "auth_failed" {
		t.Fatalf("failovers = %+v, want north->south then south->north", got.Failovers)
	}
	if !got.Tried("north") || !got.Tried("south") || got.Tried("east") {
		t.Fatal("Tried disagrees with the recorded failovers")
	}
}
