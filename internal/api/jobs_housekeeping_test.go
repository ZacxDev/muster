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

// waitIdle blocks until no job holds any agent's lane.
func (l *jobLanes) waitIdle() {
	l.mu.Lock()
	lanes := make([]*sync.Mutex, 0, len(l.m))
	for _, m := range l.m {
		lanes = append(lanes, m)
	}
	l.mu.Unlock()
	for _, m := range lanes {
		m.Lock()
		m.Unlock() //nolint:staticcheck // waiting for the holder, nothing to guard
	}
}

// housekeepingWorkspace is the job fixture's workspace with the housekeeping
// half (JobWorkspaceHousekeeper): each call records the job's status in the
// store AT THAT MOMENT, which is what the runner's ordering is about.
type housekeepingWorkspace struct {
	*jobWorkspace
	store *memJobs
	jobID func() int64

	mu    sync.Mutex
	calls []string
}

func (h *housekeepingWorkspace) record(what string) {
	st := h.store.row(h.jobID()).Status
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, what+" while "+st)
}

func (h *housekeepingWorkspace) SweepJobWorktrees(_ context.Context, a agents.Agent) ([]string, error) {
	h.record("sweep " + a.Name)
	return nil, nil
}

func (h *housekeepingWorkspace) MaintainJobRepository(_ context.Context, a agents.Agent) (string, error) {
	h.record("maintain " + a.Name)
	return "packs 9 -> 1", nil
}

func (h *housekeepingWorkspace) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

// TestAShaJobSweepsWhileRunningAndMaintainsAfterItsOutcome: for a sha job the
// runner removes its worktree, then sweeps every other job worktree WHILE THE
// JOB STILL READS running (so the store starts no other job of the agent and
// every worktree found is an orphan), then — after the outcome is recorded, so
// the result is not delayed — runs one maintenance pass. A job without a sha
// (no worktree, no fetch) does neither.
func TestAShaJobSweepsWhileRunningAndMaintainsAfterItsOutcome(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	var cur int64
	var curMu sync.Mutex
	h := &housekeepingWorkspace{jobWorkspace: f.ws, store: f.store, jobID: func() int64 {
		curMu.Lock()
		defer curMu.Unlock()
		return cur
	}}
	f.s.ext.JobWorkspace = h
	f.gw.reply = "LGTM"

	// The first job's id is 1 (memJobs counts from 1).
	curMu.Lock()
	cur = 1
	curMu.Unlock()
	id := f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "repo": "acme/widgets", "sha": testFullSHA, "pr": 7})
	if id != 1 {
		t.Fatalf("job id = %d, want 1", id)
	}
	if out := f.await(t, id); out["status"] != jobs.StatusSucceeded {
		t.Fatalf("job = %v", out)
	}
	want := []string{"sweep keen-wren while " + jobs.StatusRunning, "maintain keen-wren while " + jobs.StatusSucceeded}
	deadline := time.Now().Add(5 * time.Second)
	for len(h.seen()) < len(want) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := h.seen(); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("housekeeping = %q, want %q", got, want)
	}
	if _, removed := f.ws.snapshot(); len(removed) != 1 || removed[0] != "keen-wren#"+strconv.FormatInt(id, 10) {
		t.Fatalf("worktrees removed = %q, want the job's own", removed)
	}

	// A job without a sha: no sweep, no maintenance.
	curMu.Lock()
	cur = 2
	curMu.Unlock()
	if out := f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "hello"})); out["status"] != jobs.StatusSucceeded {
		t.Fatalf("job without a sha = %v", out)
	}
	time.Sleep(50 * time.Millisecond)
	if got := h.seen(); len(got) != len(want) {
		t.Fatalf("a job without a sha ran housekeeping: %q", got)
	}
}
