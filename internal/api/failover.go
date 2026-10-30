package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ccfailover"
	"github.com/ZacxDev/muster/internal/ccpool"
	"github.com/ZacxDev/muster/internal/jobs"
	"github.com/ZacxDev/muster/internal/officers"
)

// THE CLAUDE ACCOUNT FAILOVER'S muster HALF (internal/ccfailover does the
// move): the pin policy and the pre-apply hook for officer instances, the job
// runner's re-queue, and the agent's failover history on the API.

// failoverPolicy is ccfailover.Policy: an officer instance is pinned when its
// officer names a `cc_account`, and opted in when it sets `cc_failover`. Read
// from the registry, NOT the row, so it covers instances created before the
// row recorded pins, and a config change applies without touching rows.
func (s *Server) failoverPolicy(a agents.Agent) (pinned, optIn bool) {
	o, _, ok := s.officerOf(a)
	if !ok {
		return false, false
	}
	return o.CCAccount != "", o.CCFailover
}

// failoverPrepare is ccfailover.Prepare, run after an agent is moved to another
// account and before its pod restarts on it.
//
// 🔴 AN OFFICER INSTANCE'S ITEM IS RETRIED: its first turn is re-armed (the
// kickoff deliverer sends KickoffText once the restarted pod is ready, the
// path a Start of an existing instance takes), and every live claim it held is
// released so the item it was working is claimable again at once instead of
// when its lease lapses. Every other agent: nothing (its next turn simply runs
// on the new account).
func (s *Server) failoverPrepare(ctx context.Context, a agents.Agent) error {
	o, n, ok := s.officerOf(a)
	if !ok {
		return nil
	}
	octx := s.officerPrincipal(ctx, o.Owner)
	var errs []error
	if re, ok := s.ext.Agents.(kickoffRearmer); ok {
		if err := re.RearmKickoff(octx, a.ID, officers.KickoffText(o, n)); err != nil {
			errs = append(errs, fmt.Errorf("re-arm %s: %w", a.Name, err))
		}
	} else {
		errs = append(errs, errors.New("the agents store cannot re-arm a first turn"))
	}
	if s.officerRT != nil && s.officerRT.hq != nil {
		if err := s.releaseInstanceClaims(octx, o, a.Name); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// releaseInstanceClaims releases every live claim instance holds.
func (s *Server) releaseInstanceClaims(ctx context.Context, o officers.Officer, instance string) error {
	claimed, err := s.officerRT.hq.AllItems(ctx, o, officers.StatusClaimed)
	if err != nil {
		return fmt.Errorf("read %s's claimed items: %w", o.ID, err)
	}
	var errs []error
	now := time.Now()
	for _, it := range claimed {
		if it.ClaimedBy != instance || !it.LeaseLive(now) {
			continue
		}
		if err := s.officerRT.hq.Release(ctx, o, it.ID, instance); err != nil {
			errs = append(errs, fmt.Errorf("release item %s: %w", it.ID, err))
			continue
		}
		s.logger.Printf("officers: %s: released item %s held by %s: its account failed over, so it restarts", o.ID, it.ID, instance)
	}
	return errors.Join(errs...)
}

// failoverJob decides whether job j, whose turn on agent a ended with o, is
// RE-QUEUED after an account failover rather than finished. It returns the
// agent to run it on again and true when it was re-queued and that agent's
// replacement pod is ready.
//
// 🔴 THE RULE: o failed rate_limited or auth_failed; the failover moved a off
// the account the turn ran on (this call or the gateway's, which ran first);
// and the job was not already re-queued off that account (jobs.Requeuer: at
// most once per FROM account per job, so a job never loops). Then it is
// re-queued on the SAME worker — whose account changed under it — and runs
// again once the worker's new pod is ready. When every other account is
// cooling, nothing moved, and the job fails with the reason as before.
func (s *Server) failoverJob(ctx context.Context, j jobs.Job, a agents.Agent, o jobs.Outcome) (agents.Agent, bool) {
	if s.ext.AccountFailover == nil || o.Status != jobs.StatusFailed ||
		(o.Reason != ccpool.FailureRateLimited && o.Reason != ccpool.FailureAuthFailed) {
		return a, false
	}
	rq, ok := s.ext.Jobs.(jobs.Requeuer)
	if !ok {
		return a, false
	}
	res := s.ext.AccountFailover.AccountFailed(ctx, a.ID, a.CCAccount, o.Reason, o.Error)
	if !res.Moved {
		return a, false
	}
	requeued, err := rq.Requeue(ctx, j.ID, jobs.Failover{From: a.CCAccount, To: res.To, Reason: o.Reason, At: time.Now()})
	if err != nil {
		s.logger.Printf("jobs: %d: re-queue after account failover: %v", j.ID, err)
		return a, false
	}
	if !requeued {
		return a, false
	}
	s.logger.Printf("jobs: %d: re-queued on %s after account_failover from %s to %s, reason %s", j.ID, a.Name, a.CCAccount, res.To, o.Reason)
	if err := s.ext.AccountFailover.AwaitRebound(ctx, res); err != nil {
		s.logger.Printf("jobs: %d: %v", j.ID, err)
		return a, false
	}
	next, err := s.ext.Agents.Get(ctx, a.ID)
	if err != nil {
		s.logger.Printf("jobs: %d: re-read agent %s after its account failover: %v", j.ID, a.Name, err)
		return a, false
	}
	return next, true
}

// failoverView is GET /api/agents/{name}/account-failovers: the agent's account
// binding and its recorded rebinds, newest first. Account NAMES only.
type failoverView struct {
	Agent           string             `json:"agent"`
	CCAccount       string             `json:"ccAccount"`
	CCAccountPinned bool               `json:"ccAccountPinned"`
	CCFailover      bool               `json:"ccFailover"`
	FailoverAllowed bool               `json:"failoverAllowed"`
	Why             string             `json:"why,omitempty"`
	Events          []ccfailover.Event `json:"events"`
}

// handleAPIAgentFailovers handles GET /api/agents/{name}/account-failovers.
func (s *Server) handleAPIAgentFailovers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := strings.TrimSpace(r.PathValue("name"))
	a, err := s.ext.Agents.GetByName(ctx, name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no agent by that name"})
			return
		}
		s.logger.Printf("agents: api failovers get %q: %v", name, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not load that agent"})
		return
	}
	v, err := s.agentFailovers(ctx, a)
	if err != nil {
		s.logger.Printf("agents: api failovers %q: %v", name, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not read that agent's failovers"})
		return
	}
	s.writeJSON(w, http.StatusOK, v)
}

// agentFailovers is a's failoverView (the API's and the agent page's).
func (s *Server) agentFailovers(ctx context.Context, a agents.Agent) (failoverView, error) {
	v := failoverView{Agent: a.Name, CCAccount: a.CCAccount, CCAccountPinned: a.CCAccountPinned, CCFailover: a.CCFailover,
		Events: []ccfailover.Event{}}
	if p, o := s.failoverPolicy(a); p || o {
		v.CCAccountPinned, v.CCFailover = v.CCAccountPinned || p, v.CCFailover || o
	}
	if s.ext.AccountFailover == nil || a.CCAccount == "" {
		v.Why = "account failover is not wired on this deployment, or the agent is not claude-code"
		return v, nil
	}
	v.FailoverAllowed, v.Why = s.ext.AccountFailover.Allowed(a)
	evs, err := s.ext.AccountFailover.Events(ctx, a.ID, 20)
	if err != nil {
		return v, err
	}
	if evs != nil {
		v.Events = evs
	}
	return v, nil
}

// lastFailoverLine is the agent page's one line about a's latest account
// failover ("from north · rate_limited · resets 21:45 UTC"), "" when it has none
// or they cannot be read (the page renders without it).
func (s *Server) lastFailoverLine(ctx context.Context, a agents.Agent) string {
	if s.ext.AccountFailover == nil || a.CCAccount == "" {
		return ""
	}
	evs, err := s.ext.AccountFailover.Events(ctx, a.ID, 1)
	if err != nil || len(evs) == 0 {
		return ""
	}
	e := evs[0]
	line := "from " + e.From + " · " + e.Reason
	if !e.ResetsAt.IsZero() {
		line += " · resets " + e.ResetsAt.UTC().Format("Jan 2 15:04 UTC")
	}
	return line
}
