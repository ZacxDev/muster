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
