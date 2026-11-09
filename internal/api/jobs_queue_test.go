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

// serviceCtx is the service tier's principal, as a submission's job carries it.
func serviceCtx() context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaService})
}

// waitStatus waits until job id reads want.
func waitStatus(t *testing.T, f *jobFixture, id int64, want string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); f.store.row(id).Status != want; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("job %d = %s, want %s", id, f.store.row(id).Status, want)
		}
	}
}

// TestAStoppingServerTakesNoJobFromTheTable (Y1): once StopJobs has run — the
// first step of a graceful shutdown — this process takes no job it does not
// already hold. Job X is running here; job Y, left queued by another server,
// waits behind it. The server begins to stop, X ends and the worker is free:
// neither the drain that ran X nor a reaper pass takes Y, which stays queued
// for another server, and is not registered here (so FailOwnJobs leaves it).
func TestAStoppingServerTakesNoJobFromTheTable(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.gw.reply = "ok"
	f.gw.release = make(chan struct{})
	x := f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "running"})
	waitStatus(t, f, x, jobs.StatusRunning)
	y := leftQueued(f, "behind")
	f.s.StopJobs()
	close(f.gw.release)
	if out := f.await(t, x); out["status"] != jobs.StatusSucceeded {
		t.Fatalf("the running job = %v, want it to finish", out)
	}
	f.s.reapJobsOnce(context.Background())
	time.Sleep(100 * time.Millisecond)
	if got := f.store.row(y.ID).Status; got != jobs.StatusQueued {
		t.Fatalf("a stopping server took job Y: %s, want queued", got)
	}
	if got := turnPrompts(f); len(got) != 1 || got[0] != "running" {
		t.Fatalf("turns = %q, want only the job that was already running", got)
	}
	// Not even read up to its Start: no chat session was opened for Y (the
	// queue read itself is refused, not only the Start).
	f.ags.mu.Lock()
	sessions := len(f.ags.sessions)
	f.ags.mu.Unlock()
	if sessions != 1 {
		t.Fatalf("%d chat sessions opened, want 1 (the running job's)", sessions)
	}
	if n := f.s.FailOwnJobs(time.Second); n != 0 || f.store.row(y.ID).Status != jobs.StatusQueued {
		t.Fatalf("FailOwnJobs failed %d job(s), Y = %s; want 0 and queued", n, f.store.row(y.ID).Status)
	}
}

// TestAStopBeforeStartLeavesATakenJobQueued (Y1, the check before Start): the
// server begins to stop while its runner is inside a job it took from the
// queue but has not started (here: reading the job's agent). The job is not
// started, no turn runs, and it stays queued and unregistered.
func TestAStopBeforeStartLeavesATakenJobQueued(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.gw.reply = "ok"
	f.ags.onGet = f.s.StopJobs
	y := leftQueued(f, "taken")
	f.s.reapJobsOnce(context.Background())
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		f.ags.mu.Lock()
		n := len(f.ags.getAs)
		f.ags.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("instrument check FAILED: the runner never reached the job's agent read")
		}
	}
	time.Sleep(100 * time.Millisecond)
	if got := f.store.row(y.ID); got.Status != jobs.StatusQueued || !got.StartedAt.IsZero() {
		t.Fatalf("job = %s (started %v), want queued and never started", got.Status, got.StartedAt)
	}
	if turns := f.gw.seen(); len(turns) != 0 {
		t.Fatalf("turns %q ran on a stopping server", turns)
	}
	f.s.ownJobs.mu.Lock()
	_, held := f.s.ownJobs.m[y.ID]
	f.s.ownJobs.mu.Unlock()
	if held {
		t.Fatal("the untaken job is still registered as this process's")
	}
}

// TestAPreStartFailureLeavesTheWinnersRunningJob (Y2): two servers read the
// same queued job; the winner started it. The loser's run then fails before its
// own Start (its agent read fails). That failure must not end the winner's
// running job, and the winner's real result must land.
func TestAPreStartFailureLeavesTheWinnersRunningJob(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	// The loser's snapshot: queued, on an agent its read cannot load.
	stale := f.store.putQueued(jobs.Job{AgentID: 99, Agent: "vanishing-wren", Prompt: "p", TimeoutSeconds: 60})
	if ok, err := f.store.Start(context.Background(), stale.ID, "alpha", 0); !ok || err != nil {
		t.Fatalf("the winner's start = %v, %v", ok, err)
	}
	f.s.runJob(serviceCtx(), stale)
	if got := f.store.row(stale.ID); got.Status != jobs.StatusRunning {
		t.Fatalf("after the loser's failure the winner's job = %s/%s %q, want running", got.Status, got.Reason, got.Error)
	}
	if ok, err := f.store.Finish(context.Background(), stale.ID, jobs.Outcome{Status: jobs.StatusSucceeded, Result: "the winner's review"}); !ok || err != nil {
		t.Fatalf("the winner's finish = %v, %v; want it recorded", ok, err)
	}
	if got := f.store.row(stale.ID); got.Status != jobs.StatusSucceeded || got.Result != "the winner's review" {
		t.Fatalf("job = %s %q, want the winner's result", got.Status, got.Result)
	}
}

// TestAJobAnotherServerRanLeavesThisProcessesHands (G1): this process accepted
// a job and never ran it; another server ran it to its end. The next reaper
// pass drops it from this process's jobs, so nothing here acts on it again.
func TestAJobAnotherServerRanLeavesThisProcessesHands(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	j := leftQueued(f, "ran-elsewhere")
	f.s.ownJobs.add(j.ID, serviceCtx())
	if ok, _ := f.store.Start(context.Background(), j.ID, "alpha", 0); !ok {
		t.Fatal("start")
	}
	if ok, _ := f.store.Finish(context.Background(), j.ID, jobs.Outcome{Status: jobs.StatusSucceeded, Result: "r"}); !ok {
		t.Fatal("finish")
	}
	f.s.reapJobsOnce(context.Background())
	f.s.ownJobs.mu.Lock()
	_, held := f.s.ownJobs.m[j.ID]
	f.s.ownJobs.mu.Unlock()
	if held {
		t.Fatal("a job another server finished is still registered as this process's")
	}
}

// TestAShutdownLeavesAJobAnotherServerRequeued (G1): this process accepted a
// job it never started; another server started it, and its account failover
// re-queued it (queued again, with a started_at), and still holds it while the
// worker's pod restarts. This process's shutdown must not fail it.
func TestAShutdownLeavesAJobAnotherServerRequeued(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	j := leftQueued(f, "requeued-elsewhere")
	f.s.ownJobs.add(j.ID, serviceCtx())
	if ok, _ := f.store.Start(context.Background(), j.ID, "alpha", 0); !ok {
		t.Fatal("start")
	}
	f.store.mu.Lock()
	row := f.store.rows[j.ID]
	row.Status = jobs.StatusQueued // the other server's Requeue: started_at kept
	f.store.rows[j.ID] = row
	f.store.mu.Unlock()
	if n := f.s.FailOwnJobs(time.Second); n != 0 {
		t.Fatalf("FailOwnJobs failed %d job(s), want 0", n)
	}
	if got := f.store.row(j.ID); got.Status != jobs.StatusQueued || got.Reason != "" {
		t.Fatalf("the re-queued job = %s/%s, want queued", got.Status, got.Reason)
	}
}

// TestATakenJobRunsAsItsOwner (G2): a job taken from the queue runs as the
// owner Heads names — here user 5, not the operator — through the queue's own
// door, so a runner that ran every such job as user 1 is caught.
func TestATakenJobRunsAsItsOwner(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.gw.reply = "ok"
	j := f.store.putQueuedFor(5, jobs.Job{AgentID: 7, Agent: "keen-wren", Prompt: "theirs", TimeoutSeconds: 60})
	f.s.reapJobsOnce(context.Background())
	if out := f.await(t, j.ID); out["status"] != jobs.StatusSucceeded {
		t.Fatalf("job = %v", out)
	}
	f.ags.mu.Lock()
	defer f.ags.mu.Unlock()
	if len(f.ags.getAs) != 1 || f.ags.getAs[0].UserID != 5 || f.ags.getAs[0].Via != authz.ViaJobQueue {
		t.Fatalf("the job's agent was read as %+v, want once as user 5 via %s", f.ags.getAs, authz.ViaJobQueue)
	}
}
