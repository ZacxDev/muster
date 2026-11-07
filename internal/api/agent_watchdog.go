package api

import (
	"context"
	"time"

	"github.com/ZacxDev/muster/internal/agentoutcome"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
)

// ---------------------------------------------------------------------------
// THE SILENCE WATCHDOG (operator decision 3 of the dispatch-outcomes change;
// docs/provisioner.md, "How an agent's work ends").
//
// A sweep over every agent whose outcome is still open (running or stuck),
// applying THE stuck predicate, agentoutcome.Decide:
//
//   - no first reply and no activity for MUSTER_AGENT_STUCK_NO_REPLY (5 min)
//     since the kickoff → stuck, "no reply since kickoff";
//   - replied, still working, and no activity for MUSTER_AGENT_STUCK_SILENT
//     (30 min) → stuck, "silent for 30 min";
//   - stuck, and activity since → running again (no push: the next outcome says it).
//
// "Activity" is everything muster can observe of the agent (agentoutcome.LastActivity):
// its row and transcript (agents.LastActivity, the one definition), every turn's
// start, end and stream bytes, and its pod's work reports — so a long tool run,
// which streams and heartbeats while writing no transcript row, is never flipped.
//
// 🔴 EACH VERDICT IS A CONDITIONAL WRITE (agentoutcome.Store.MarkStuck: still
// running, activity unchanged since the read), and only the write that applied
// pushes — one push per transition, also across replicas. Before the kickoff is
// delivered the verdict is internal/agentkickoff's, not this sweep's.
// ---------------------------------------------------------------------------

// watchdogThresholds is the configured thresholds, defaults filled.
func (s *Server) watchdogThresholds() agentoutcome.Thresholds {
	th := s.ext.AgentWatchdog
	def := agentoutcome.DefaultThresholds()
	if th.NoReply <= 0 {
		th.NoReply = def.NoReply
	}
	if th.Silent <= 0 {
		th.Silent = def.Silent
	}
	return th
}

// RunAgentWatchdog sweeps every interval (agentoutcome.DefaultInterval when
// interval <= 0) until ctx ends. It returns at once with no outcome store.
func (s *Server) RunAgentWatchdog(ctx context.Context, interval time.Duration) {
	if !s.outcomesOn() {
		return
	}
	if interval <= 0 {
		interval = agentoutcome.DefaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := s.AgentWatchdogPass(ctx); err != nil && ctx.Err() == nil {
			s.logger.Printf("outcomes: watchdog: %v", err)
		}
	}
}

// AgentWatchdogPass is one sweep: it returns how many agents it called stuck.
func (s *Server) AgentWatchdogPass(ctx context.Context) (int, error) {
	if !s.outcomesOn() {
		return 0, nil
	}
	// A background loop acts for every owner (accounts PR 2/6); see internal/authz.
	ctx = authz.System(ctx)
	list, err := s.ext.Agents.List(ctx)
	if err != nil {
		return 0, err
	}
	var open []agents.Agent
	for _, a := range list {
		if a.Outcome == agentoutcome.Running || a.Outcome == agentoutcome.Stuck {
			open = append(open, a)
		}
	}
	if len(open) == 0 {
		return 0, nil
	}
	last := s.lastMessageByAgents(ctx, open)
	th := s.watchdogThresholds()
	now := s.now()
	stuck := 0
	for _, a := range open {
		// 🔴 A TURN IN FLIGHT IS ACTIVITY. ccd answers /v1/responses only when its
		// CLI's turn is over, so a claude-code first turn streams NOTHING for as
		// long as it runs; without this a ten-minute first turn would read "no
		// reply since kickoff" at five. The open-turn counter (this process's
		// kickoff and chat turns) and the pod's work reports are what muster sees.
		v := agentoutcome.Decide(agentoutcome.Input{Agent: a, LastMessage: last[a.ID],
			Working: !s.AgentWork(a.ID).IsZero() || s.agentOutcomes.busy(a.ID)}, now, th)
		switch v.Action {
		case agentoutcome.MarkStuck:
			applied, err := s.ext.Outcomes.MarkStuck(ctx, a.ID, v.Reason, now, a.ActivityAt)
			if err != nil {
				s.logger.Printf("outcomes: agent %d: mark stuck: %v", a.ID, err)
				continue
			}
			if applied {
				stuck++
				s.outcomeStuck(ctx, a, v.Reason)
			}
		case agentoutcome.Recover:
			applied, err := s.ext.Outcomes.Transition(ctx, a.ID, []string{agentoutcome.Stuck}, agentoutcome.Running, "", now)
			if err != nil {
				s.logger.Printf("outcomes: agent %d: recover: %v", a.ID, err)
				continue
			}
			if applied {
				s.logger.Printf("outcomes: agent %d (%s) is working again", a.ID, a.Name)
				s.outcomeChanged(a)
			}
		}
	}
	return stuck, nil
}
