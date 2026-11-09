package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/jobs"
)

// ---------------------------------------------------------------------------
// THE JOB QUEUE (jobs_queue.go): a job a crashed process left queued is run by
// the next reaper pass of a fresh process, in its place in line.
//
// The crash state is built as a crash leaves it: a queued row in the store and
// nothing else — no goroutine, no ownJobs entry, no lane held — in a fixture
// whose Server has never seen the job (a fresh process).
// ---------------------------------------------------------------------------

// leftQueued records, as a dead process would have, a queued job on keen-wren.
func leftQueued(f *jobFixture, prompt string) jobs.Job {
	return f.store.putQueued(jobs.Job{AgentID: 7, Agent: "keen-wren", Prompt: prompt, TimeoutSeconds: 60,
		RequestedBy: "review-orchestrator"})
}

// turnPrompts is the prompt of each turn the gateway ran, in order.
func turnPrompts(f *jobFixture) []string {
	var out []string
	for _, t := range f.gw.seen() {
		out = append(out, t[strings.LastIndex(t, " ")+1:])
	}
	return out
}

// TestAJobACrashLeftQueuedRunsAtTheNextReaperPass: the incident. A job queued
// when the process died stayed queued for good: its goroutine was gone, and
// the reaper only fails a queued job after jobs.MaxQueueWait. The boot pass of
// the next process (reapJobsOnce) now runs it, as its owner, to its real
// outcome.
func TestAJobACrashLeftQueuedRunsAtTheNextReaperPass(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.gw.reply = "reviewed"
	b := leftQueued(f, "orphaned")
	f.s.reapJobsOnce(context.Background())
	out := f.await(t, b.ID)
	if out["status"] != jobs.StatusSucceeded || out["result"] != "reviewed" || out["cc_account"] != "alpha" {
		t.Fatalf("the job a crash left queued = %v/%v on %v, want succeeded/reviewed on alpha", out["status"], out["result"], out["cc_account"])
	}
	if got := turnPrompts(f); len(got) != 1 || got[0] != "orphaned" {
		t.Fatalf("turns = %q, want the orphaned job's one turn", got)
	}
	// It was this process's while it ran, and is not once it finished.
	f.s.ownJobs.mu.Lock()
	_, still := f.s.ownJobs.m[b.ID]
	f.s.ownJobs.mu.Unlock()
	if still {
		t.Fatal("the job is still registered as this process's after it finished")
	}
}

// TestANewerJobDoesNotJumpAnOlderQueuedOne: after the restart the same worker
// ran every NEWER job while the orphan sat queued. A submission now runs its
// agent's queue oldest first, so the job a crash left queued runs before the
// one just submitted — without waiting for a reaper pass.
func TestANewerJobDoesNotJumpAnOlderQueuedOne(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.gw.reply = "ok"
	b := leftQueued(f, "older")
	c := f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "newer"})
	for _, id := range []int64{b.ID, c} {
		if out := f.await(t, id); out["status"] != jobs.StatusSucceeded {
			t.Fatalf("job %d = %v", id, out)
		}
	}
	if got := turnPrompts(f); len(got) != 2 || got[0] != "older" || got[1] != "newer" {
		t.Fatalf("turns ran in the order %q, want [older newer]: the older queued job first", got)
	}
}

// TestADeadRunnersJobHoldsTheAgentUntilItIsReaped: the incident's other job.
// The crashed process was RUNNING job A on the worker (its turn may still be
// running in the agent's runtime). A holds the worker until the reaper fails
// it at its own deadline; neither the orphan queued behind it nor a job
// submitted after the restart runs beside it or ahead of the orphan. The pass
// that reaps A then runs the orphan, then the newer job.
func TestADeadRunnersJobHoldsTheAgentUntilItIsReaped(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.gw.reply = "ok"
	a := leftQueued(f, "dead-runner")
	started := time.Now()
	f.store.mu.Lock()
	row := f.store.rows[a.ID]
	row.Status, row.StartedAt, row.CCAccount = jobs.StatusRunning, started, "alpha"
	f.store.rows[a.ID] = row
	f.store.mu.Unlock()
	b := leftQueued(f, "orphaned")

	// Inside A's deadline: the boot pass runs nothing, and a new submission
	// waits behind the orphan.
	f.s.jobNow = func() time.Time { return started.Add(time.Minute) }
	f.s.reapJobsOnce(context.Background())
	c := f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "newer"})
	time.Sleep(100 * time.Millisecond)
	if got := turnPrompts(f); len(got) != 0 {
		t.Fatalf("turns %q ran while the dead runner's job still held the worker", got)
	}
	if st := f.store.row(b.ID).Status + "/" + f.store.row(c).Status; st != "queued/queued" {
		t.Fatalf("orphan/newer = %s, want queued/queued", st)
	}

	// Past A's deadline (60s + ReapSlack): the pass fails A, then runs B, then C.
	f.s.jobNow = func() time.Time { return started.Add(60*time.Second + jobs.ReapSlack + time.Minute) }
	f.s.reapJobsOnce(context.Background())
	if got := f.store.row(a.ID); got.Status != jobs.StatusFailed || got.Reason != jobs.ReasonInterrupted {
		t.Fatalf("the dead runner's job = %s/%s, want failed/interrupted", got.Status, got.Reason)
	}
	for _, id := range []int64{b.ID, c} {
		if out := f.await(t, id); out["status"] != jobs.StatusSucceeded {
			t.Fatalf("job %d = %v", id, out)
		}
	}
	if got := turnPrompts(f); len(got) != 2 || got[0] != "orphaned" || got[1] != "newer" {
		t.Fatalf("turns = %q, want [orphaned newer]", got)
	}
}

// TestAQueuedJobWhoseAgentWasDeletedIsFailedAtThePass: with no agent left to
// run on (agent_id SET NULL), the pass fails it agent_unavailable instead of
// leaving it queued for jobs.MaxQueueWait.
func TestAQueuedJobWhoseAgentWasDeletedIsFailedAtThePass(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	j := f.store.putQueued(jobs.Job{Agent: "deleted-wren", Prompt: "p", TimeoutSeconds: 60})
	f.s.reapJobsOnce(context.Background())
	got := f.store.row(j.ID)
	if got.Status != jobs.StatusFailed || got.Reason != jobs.ReasonAgentUnavailable ||
		got.Error != `agent "deleted-wren" was deleted before the job started; nothing was sent to it` {
		t.Fatalf("job = %s/%s %q, want failed/%s", got.Status, got.Reason, got.Error, jobs.ReasonAgentUnavailable)
	}
	if turns := f.gw.seen(); len(turns) != 0 {
		t.Fatalf("turns %q ran for a job with no agent", turns)
	}
}

// TestAShutdownLeavesAJobAnotherProcessStarted: this process accepted a job it
// never started; another process's queue runner has since started it. The
// graceful shutdown fails this process's jobs, but a job it never started only
// while it is still queued, so the other process's run is not cut.
func TestAShutdownLeavesAJobAnotherProcessStarted(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	j := leftQueued(f, "taken-elsewhere")
	f.s.ownJobs.add(j.ID, authz.WithPrincipal(context.Background(),
		authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaService}))
	if ok, err := f.store.Start(context.Background(), j.ID, "alpha", 0); !ok || err != nil {
		t.Fatalf("start = %v, %v", ok, err)
	}
	if n := f.s.FailOwnJobs(time.Second); n != 0 {
		t.Fatalf("FailOwnJobs failed %d job(s), want 0", n)
	}
	if got := f.store.row(j.ID).Status; got != jobs.StatusRunning {
		t.Fatalf("the job another process runs = %s, want running", got)
	}
}
