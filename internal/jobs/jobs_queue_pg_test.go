package jobs

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestHeadsIsEachFreeAgentsOldestQueuedJob pins the dispatch read the runner
// takes every job from (Queue.Heads), across every owner, with no principal:
//   - a free agent's head is its OLDEST queued job, whichever was submitted
//     after it — the order a worker runs its jobs in;
//   - an agent with a job running has no head (one job per agent at a time,
//     whichever process runs it), including another owner's;
//   - a job re-queued by an account failover holds its agent until its start's
//     deadline (started_at + timeout_s + ReapSlack) and is then its head;
//   - a queued job whose agent was deleted is its own head, with agent 0;
//   - agentID narrows the read to one agent.
func TestHeadsIsEachFreeAgentsOldestQueuedJob(t *testing.T) {
	s, pool := pgStore(t)
	idle := mustAgent(t, pool, 1, "job-head-idle")
	busy := mustAgent(t, pool, 2, "job-head-busy")
	moved := mustAgent(t, pool, 1, "job-head-moved")
	gone := mustAgent(t, pool, 1, "job-head-gone")
	mk := func(uid, agentID int64, name string, timeout int) Job {
		t.Helper()
		j, _, err := s.Create(as(uid), NewJob{AgentID: agentID, Agent: name, Prompt: "p", TimeoutSeconds: timeout})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	start := func(uid int64, j Job) {
		t.Helper()
		if ok, err := s.Start(as(uid), j.ID, "alpha", 0); !ok || err != nil {
			t.Fatalf("start %d: %v %v", j.ID, ok, err)
		}
	}
	// idle: two queued (the newer one is made last of all, below), the older first in line.
	idleOld := mk(1, idle.ID, idle.Name, 60)
	// busy (user 2's): one running, one queued behind it.
	busyRun := mk(2, busy.ID, busy.Name, 60)
	busyNext := mk(2, busy.ID, busy.Name, 60)
	// moved: a job re-queued by a failover, mid-move, and one behind it.
	movedJob := mk(1, moved.ID, moved.Name, 120)
	mk(1, moved.ID, moved.Name, 60)
	// gone: queued, then its agent deleted.
	goneJob := mk(1, gone.ID, gone.Name, 60)
	mk(1, idle.ID, idle.Name, 60)
	start(2, busyRun)
	start(1, movedJob)
	if ok, err := s.Requeue(as(1), movedJob.ID, Failover{From: "alpha", To: "beta", Reason: "rate_limited"}); !ok || err != nil {
		t.Fatalf("requeue: %v %v", ok, err)
	}
	dbtest.OwnerExec(t, dbtest.DSN(t), fmt.Sprintf(`DELETE FROM agents WHERE id = %d`, gone.ID))
	moveStart, err := s.Get(as(1), movedJob.ID)
	if err != nil || moveStart.StartedAt.IsZero() {
		t.Fatalf("the re-queued job = %+v, %v; want its started_at kept", moveStart, err)
	}

	heads := func(agentID int64, at time.Time) string {
		t.Helper()
		// No principal: Heads elevates itself, like the reaper.
		hs, err := s.Heads(context.Background(), agentID, at)
		if err != nil {
			t.Fatal(err)
		}
		out := ""
		for _, h := range hs {
			out += fmt.Sprintf("[%d agent %d owner %d]", h.ID, h.AgentID, h.OwnerID)
		}
		return out
	}
	now := time.Now()
	want := fmt.Sprintf("[%d agent %d owner 1][%d agent 0 owner 1]", idleOld.ID, idle.ID, goneJob.ID)
	if got := heads(0, now); got != want {
		t.Fatalf("Heads(every agent) = %s, want %s (idle's OLDEST job; the deleted agent's job; nothing of the busy "+
			"agent or the one mid-failover)", got, want)
	}
	if got, want := heads(idle.ID, now), fmt.Sprintf("[%d agent %d owner 1]", idleOld.ID, idle.ID); got != want {
		t.Fatalf("Heads(idle) = %s, want %s", got, want)
	}
	if got := heads(busy.ID, now); got != "" {
		t.Fatalf("Heads(busy) = %s, want none: its job %d is running", got, busyRun.ID)
	}
	// Past the re-queued job's hold (its start + 120s + ReapSlack): its runner
	// is gone, so it is the moved agent's head again — before the job behind it.
	past := moveStart.StartedAt.Add(120*time.Second + ReapSlack + time.Second)
	if got, want := heads(moved.ID, past), fmt.Sprintf("[%d agent %d owner 1]", movedJob.ID, moved.ID); got != want {
		t.Fatalf("Heads(moved) past the hold = %s, want %s", got, want)
	}
	if got := heads(moved.ID, past.Add(-2*time.Second)); got != "" {
		t.Fatalf("Heads(moved) inside the hold = %s, want none", got)
	}
	// Once the running job ends, the busy agent's next job is its head, as
	// owner 2.
	if ok, err := s.Finish(as(2), busyRun.ID, Outcome{Status: StatusSucceeded, Result: "r"}); !ok || err != nil {
		t.Fatalf("finish: %v %v", ok, err)
	}
	if got, want := heads(busy.ID, now), fmt.Sprintf("[%d agent %d owner 2]", busyNext.ID, busy.ID); got != want {
		t.Fatalf("Heads(busy) after its job ended = %s, want %s", got, want)
	}
}

// TestFinishQueuedLeavesAStartedJobAlone: a graceful shutdown fails the jobs it
// accepted but never started with FinishQueued, so a job another process has
// since started keeps running there.
func TestFinishQueuedLeavesAStartedJobAlone(t *testing.T) {
	s, pool := pgStore(t)
	a := mustAgent(t, pool, 1, "job-finish-queued")
	q, _, err := s.Create(as(1), NewJob{AgentID: a.ID, Agent: a.Name, Prompt: "p", TimeoutSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := s.Create(as(1), NewJob{AgentID: a.ID, Agent: a.Name, Prompt: "p", TimeoutSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Start(as(1), r.ID, "alpha", 0); !ok || err != nil {
		t.Fatalf("start: %v %v", ok, err)
	}
	fail := Failed(ReasonInterrupted, ShutdownQueuedError)
	if ok, err := s.FinishQueued(as(1), r.ID, fail); ok || err != nil {
		t.Fatalf("FinishQueued(running) = %v, %v; want false", ok, err)
	}
	if got, _ := s.Get(as(1), r.ID); got.Status != StatusRunning {
		t.Fatalf("the running job = %s, want running", got.Status)
	}
	if ok, err := s.FinishQueued(as(1), q.ID, fail); !ok || err != nil {
		t.Fatalf("FinishQueued(queued) = %v, %v; want true", ok, err)
	}
	if got, _ := s.Get(as(1), q.ID); got.Status+"/"+got.Reason != "failed/interrupted" || got.Error != ShutdownQueuedError {
		t.Fatalf("the queued job = %s/%s %q", got.Status, got.Reason, got.Error)
	}
}

// TestFinishQueuedLeavesARequeuedJobAlone: a job re-queued by an account
// failover reads queued again, but its runner still holds it (it waits for the
// worker's new pod, then starts it again). A shutdown of ANOTHER server that
// once accepted it must not fail it: FinishQueued requires a job that never
// started.
func TestFinishQueuedLeavesARequeuedJobAlone(t *testing.T) {
	s, pool := pgStore(t)
	a := mustAgent(t, pool, 1, "job-finish-requeued")
	j, _, err := s.Create(as(1), NewJob{AgentID: a.ID, Agent: a.Name, Prompt: "p", TimeoutSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Start(as(1), j.ID, "alpha", 0); !ok || err != nil {
		t.Fatalf("start: %v %v", ok, err)
	}
	if ok, err := s.Requeue(as(1), j.ID, Failover{From: "alpha", To: "beta", Reason: "rate_limited"}); !ok || err != nil {
		t.Fatalf("requeue: %v %v", ok, err)
	}
	if ok, err := s.FinishQueued(as(1), j.ID, Failed(ReasonInterrupted, ShutdownQueuedError)); ok || err != nil {
		t.Fatalf("FinishQueued(re-queued) = %v, %v; want false", ok, err)
	}
	if got, _ := s.Get(as(1), j.ID); got.Status != StatusQueued || got.Reason != "" {
		t.Fatalf("the re-queued job = %s/%s, want queued", got.Status, got.Reason)
	}
}
