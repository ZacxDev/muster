package api

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/jobs"
)

// sweepingWorkspace is the job fixture's workspace with the sweep half
// (JobWorktreeSweeper): each sweep records the job's status in the store AT
// THAT MOMENT, which is what the runner's ordering is about, and whether the
// job's own worktree was already removed.
type sweepingWorkspace struct {
	*jobWorkspace
	store *memJobs
	jobID func() int64

	mu    sync.Mutex
	calls []string
}

func (h *sweepingWorkspace) SweepJobWorktrees(_ context.Context, a agents.Agent) ([]string, error) {
	st := h.store.row(h.jobID()).Status
	_, removed := h.jobWorkspace.snapshot()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, "sweep "+a.Name+" while "+st+" after "+strconv.Itoa(len(removed))+" removal(s)")
	return nil, nil
}

func (h *sweepingWorkspace) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

// TestAShaJobSweepsLeftWorktreesWhileItIsRunning: for a sha job the runner
// removes its own worktree, then sweeps every other job worktree WHILE THE JOB
// STILL READS running (so the store starts no other job of the agent on any
// server, and every worktree found is an orphan). A job without a sha (no
// worktree) does not sweep.
func TestAShaJobSweepsLeftWorktreesWhileItIsRunning(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	var cur int64
	var curMu sync.Mutex
	setCur := func(id int64) { curMu.Lock(); cur = id; curMu.Unlock() }
	h := &sweepingWorkspace{jobWorkspace: f.ws, store: f.store, jobID: func() int64 {
		curMu.Lock()
		defer curMu.Unlock()
		return cur
	}}
	f.s.ext.JobWorkspace = h
	f.gw.reply = "LGTM"

	setCur(1) // memJobs numbers jobs from 1
	id := f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "repo": "acme/widgets", "sha": testFullSHA, "pr": 7})
	if id != 1 {
		t.Fatalf("job id = %d, want 1", id)
	}
	if out := f.await(t, id); out["status"] != jobs.StatusSucceeded {
		t.Fatalf("job = %v", out)
	}
	want := "sweep keen-wren while " + jobs.StatusRunning + " after 1 removal(s)"
	if got := h.seen(); len(got) != 1 || got[0] != want {
		t.Fatalf("sweeps = %q, want [%q]", got, want)
	}

	setCur(2)
	if out := f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "hello"})); out["status"] != jobs.StatusSucceeded {
		t.Fatalf("job without a sha = %v", out)
	}
	time.Sleep(50 * time.Millisecond)
	if got := h.seen(); len(got) != 1 {
		t.Fatalf("a job without a sha swept: %q", got)
	}
}

// TestNoSweepUnlessTheJobStillRunsAndTheServerIsNotStopping: the sweep is
// what makes another job's worktree disposable, so it runs only while the
// sweeping job still reads running AND its server is not stopping. A stopping
// server (StopJobs, then FailOwnJobs fails its jobs while their turns unwind)
// and a job the store already ended (the reaper, or that shutdown) let another
// server start the agent's next job, whose worktree a sweep would delete.
// The control: the same running job on a server that is not stopping sweeps.
func TestNoSweepUnlessTheJobStillRunsAndTheServerIsNotStopping(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	j := f.store.putQueued(jobs.Job{AgentID: 1, Agent: "keen-wren", Prompt: "review", SHA: testFullSHA})
	h := &sweepingWorkspace{jobWorkspace: f.ws, store: f.store, jobID: func() int64 { return j.ID }}
	f.s.ext.JobWorkspace = h
	a := agents.Agent{ID: 1, Name: "keen-wren"}
	ctx := context.Background()

	// Queued, not running: no sweep.
	f.s.sweepJobWorktrees(ctx, a, j)
	if got := h.seen(); len(got) != 0 {
		t.Fatalf("a job that is not running swept: %q", got)
	}
	if ok, err := f.store.Start(ctx, j.ID, "", 0); !ok || err != nil {
		t.Fatalf("start: %v %v", ok, err)
	}
	// Running, server stopping: no sweep.
	f.s.StopJobs()
	f.s.sweepJobWorktrees(ctx, a, j)
	if got := h.seen(); len(got) != 0 {
		t.Fatalf("a stopping server swept: %q", got)
	}
	// Control: running, not stopping: one sweep.
	f.s.jobsStopping.Store(false)
	f.s.sweepJobWorktrees(ctx, a, j)
	if got := h.seen(); len(got) != 1 {
		t.Fatalf("control: a running job on a live server swept %d time(s), want 1: %q", len(got), got)
	}
	// Ended by the store (the reaper) while its runner lives: no further sweep.
	if _, err := f.store.Finish(ctx, j.ID, jobs.Failed(jobs.ReasonInterrupted, "reaped")); err != nil {
		t.Fatal(err)
	}
	f.s.sweepJobWorktrees(ctx, a, j)
	if got := h.seen(); len(got) != 1 {
		t.Fatalf("a job the store had ended swept: %q", got)
	}
}
