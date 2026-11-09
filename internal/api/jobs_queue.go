package api

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/jobs"
)

// THE JOB QUEUE'S RUNNER: each agent's jobs are taken from the table, oldest
// first (jobs.Store.Heads), by one drain per agent in this process.
//
// 🔴 WHY: A QUEUED JOB USED TO LIVE IN A GOROUTINE. The submission started one
// goroutine per job, which waited on its agent's in-process lane (jobLanes).
// A crash took every waiting goroutine with it, and nothing ever looked at the
// rows again: a job queued behind a running one stayed `queued` for the whole
// of jobs.MaxQueueWait, never started and never reaped inside that window,
// while the same worker ran every job submitted after the restart (each got a
// goroutine of its own and found the lane free). Now the submission only
// kicks its agent's drain, and the reaper's pass (boot, then every
// JobReapInterval) kicks every agent with a head, so a job a dead process left
// queued is run by the next pass of any process, in its place in line.
//
// ⚠ WHAT THIS DOES NOT CHANGE:
//   - one job per agent at a time. A drain runs its agent's jobs one after
//     another, each through runJob (which still takes the agent's lane, so a
//     pool failover's holdIdleAgent still holds jobs off a restarting pod). And
//     Heads answers nothing for an agent with a job running in ANY process, so
//     a crashed process's running job holds its agent until the reaper fails
//     it, at its own deadline — its turn may still be running in the agent's
//     runtime (a dropped request does not stop it).
//   - where a job's account failover waits. The re-queued job is held for its
//     runner (Heads skips it until its start's deadline), so another process's
//     pass does not take it while its runner waits for the worker's new pod.
//   - a graceful shutdown still fails the jobs this process accepted
//     (FailOwnJobs) — a queued one only while it is still queued
//     (jobs.Store.FinishQueued), so one another process has started since is
//     left to it.

// jobDrainHeadTimeout bounds one read of the queue's heads.
const jobDrainHeadTimeout = 10 * time.Second

// jobDrains is the set of agents whose drain is running in this process, each
// with whether it was kicked again while it ran.
type jobDrains struct {
	mu sync.Mutex
	// again[agent] exists while the agent's drain runs; true when a kick came in
	// since its last read of the queue.
	again map[int64]bool
}

// start reports whether the caller must start agentID's drain: false when one
// is already running, which is then told to read the queue once more.
func (d *jobDrains) start(agentID int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.again == nil {
		d.again = map[int64]bool{}
	}
	if _, running := d.again[agentID]; running {
		d.again[agentID] = true
		return false
	}
	d.again[agentID] = false
	return true
}

// next reports whether agentID's drain must read the queue again (a kick came
// in while it ran); when it need not, the drain is over and the next kick
// starts a new one. The check and the end are one step, so no kick can fall
// between them.
func (d *jobDrains) next(agentID int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.again[agentID] {
		d.again[agentID] = false
		return true
	}
	delete(d.again, agentID)
	return false
}

// forget ends agentID's drain whatever was asked (its goroutine is unwinding).
func (d *jobDrains) forget(agentID int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.again, agentID)
}

// kickJobs makes sure agentID's drain runs: it starts one, or tells the
// running one to read the queue again.
func (s *Server) kickJobs(agentID int64) {
	if s.ext.Jobs == nil || agentID == 0 || !s.jobDrains.start(agentID) {
		return
	}
	safeGo(s.logger, "job queue of agent "+strconv.FormatInt(agentID, 10), func() { s.drainJobs(agentID) })
}

// drainJobs runs agentID's queue until it is empty (or its head cannot be
// run now) and no kick came in meanwhile.
func (s *Server) drainJobs(agentID int64) {
	defer func() {
		// A panic outside a job's own run (each is recovered below) must not
		// leave the agent marked as drained for ever: the next kick starts again.
		if rec := recover(); rec != nil {
			s.jobDrains.forget(agentID)
			panic(rec)
		}
	}()
	for {
		s.drainJobsOnce(agentID)
		if !s.jobDrains.next(agentID) {
			return
		}
	}
}

// drainJobsOnce runs agentID's heads one after another until it has none.
func (s *Server) drainJobsOnce(agentID int64) {
	var last int64
	for {
		hctx, cancel := context.WithTimeout(context.Background(), jobDrainHeadTimeout)
		heads, err := s.ext.Jobs.Heads(hctx, agentID, s.jobClock())
		cancel()
		if err != nil {
			s.logger.Printf("jobs: agent %d: read the queue: %v (the next reaper pass reads it again)", agentID, err)
			return
		}
		if len(heads) == 0 {
			return
		}
		h := heads[0]
		if h.ID == last {
			// The job just run is still queued: its outcome could not be
			// recorded. Reading it again now would loop on a failing store;
			// the next reaper pass tries it again.
			s.logger.Printf("jobs: %d: still queued after its run; left to the next reaper pass", h.ID)
			return
		}
		last = h.ID
		ctx, adopted := s.jobContext(h)
		j, err := s.ext.Jobs.Get(ctx, h.ID)
		if err != nil {
			s.logger.Printf("jobs: %d: read the queued job: %v", h.ID, err)
			if adopted {
				s.ownJobs.drop(h.ID)
			}
			return
		}
		if j.Status != jobs.StatusQueued {
			if adopted {
				s.ownJobs.drop(h.ID)
			}
			continue
		}
		if adopted {
			s.logger.Printf("jobs: %d: queued on agent %q since %s with no runner in this process; running it now",
				j.ID, j.Agent, j.CreatedAt.UTC().Format(time.RFC3339))
		}
		func() {
			// Deferred directly, as safeGo's is: a job's panic is recorded on
			// the job by runJob, logged here, and the drain goes on.
			defer recoverGoroutine(s.logger, "job "+strconv.FormatInt(j.ID, 10))
			s.runJob(ctx, j)
		}()
	}
}

// jobContext is the context head h's job runs under: its submission's, when
// this process accepted it (ownJobs), or else its owner's (jobPrincipal),
// registered in ownJobs so a graceful shutdown fails it like any other.
func (s *Server) jobContext(h jobs.Head) (ctx context.Context, adopted bool) {
	if ctx, ok := s.ownJobs.ctxOf(h.ID); ok {
		return ctx, false
	}
	ctx = s.jobPrincipal(context.Background(), h.OwnerID)
	s.ownJobs.add(h.ID, ctx)
	return ctx, true
}

// jobPrincipal is ctx acting as owner, through authz.ViaJobQueue: how the
// queue's runner reads and runs a job no request of this process holds, after
// jobs.PGStore.Heads read its head under System. Every store call on the job
// then runs as its owner, as its submission's did. internal/authz's
// withPrincipalLedger lists it.
func (s *Server) jobPrincipal(ctx context.Context, owner int64) context.Context {
	return authz.WithPrincipal(ctx, s.principalFor(ctx, owner, authz.ViaJobQueue))
}

// jobClock is the job runner's and the reaper's clock.
func (s *Server) jobClock() time.Time {
	if s.jobNow != nil {
		return s.jobNow()
	}
	return time.Now()
}

// dispatchQueuedJobs is the reaper pass's dispatch half: it kicks the drain of
// every agent with a head — above all one whose jobs a dead process left
// queued — and fails a queued job whose agent was deleted, which has nothing
// to run on.
func (s *Server) dispatchQueuedJobs(ctx context.Context) {
	if s.ext.Jobs == nil {
		return
	}
	hctx, cancel := context.WithTimeout(ctx, jobDrainHeadTimeout)
	heads, err := s.ext.Jobs.Heads(hctx, 0, s.jobClock())
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Printf("jobs: read the queue: %v", err)
		}
		return
	}
	for _, h := range heads {
		if h.AgentID != 0 {
			s.kickJobs(h.AgentID)
			continue
		}
		jctx, fcancel := context.WithTimeout(s.jobPrincipal(context.WithoutCancel(ctx), h.OwnerID), jobFinishTimeout)
		msg := "the job's agent was deleted before the job started; nothing was sent to it"
		if j, err := s.ext.Jobs.Get(jctx, h.ID); err == nil {
			msg = fmt.Sprintf("agent %q was deleted before the job started; nothing was sent to it", j.Agent)
		}
		if _, err := s.ext.Jobs.FinishQueued(jctx, h.ID, jobs.Failed(jobs.ReasonAgentUnavailable, msg)); err != nil {
			s.logger.Printf("jobs: %d: fail a job whose agent was deleted: %v", h.ID, err)
		}
		fcancel()
	}
}
