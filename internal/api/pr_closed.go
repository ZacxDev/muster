package api

import (
	"context"
	"time"

	"github.com/ZacxDev/muster/internal/githubapp"
	"github.com/ZacxDev/muster/internal/plugins"
)

// ---------------------------------------------------------------------------
// A REVIEW OF A PULL REQUEST ALREADY MERGED OR CLOSED:
// docs/plugins.md#a-review-of-a-pull-request-already-merged-or-closed.
//
// A review mail (prMailRef) whose pull request GitHub reports CLOSED — merged
// or not — needs nothing more, so muster settles its message itself
// (plugins.SettledByMuster) at each point it would otherwise reach a person:
//   - on arrival (settleClosedPRReview): no paste into a session, no chief turn;
//   - when its route would be handed back to the chief (deliverHandBack): a
//     review timed out or superseded before its paste was sent;
//   - when the "not acted on" ask is due (sweepUnroutedMessages).
//
// 🔴 ONLY A CLOSED ANSWER SETTLES. An open pull request, a repository the App
// has no installation for, a GitHub error or a slow answer are all "unknown",
// and unknown keeps today's behaviour exactly — a review of an open pull
// request is never dropped on a guess.
// ---------------------------------------------------------------------------

// PRStateReader reads a pull request's state from GitHub (the GitHub App:
// githubapp.App.PullRequestState).
type PRStateReader interface {
	PullRequestState(ctx context.Context, repo string, n int64) (githubapp.PRState, error)
}

// ClosedPRs is the closed-pull-request settle's dependencies.
type ClosedPRs struct {
	States PRStateReader
	Store  plugins.PRReviewSettler
}

// prStateTimeout bounds one GitHub read: past it the state is unknown.
const prStateTimeout = 5 * time.Second

// Where a closed review was settled (the audit's "via").
const (
	closedPRViaArrival  = "pr-closed-arrival"
	closedPRViaHandBack = "pr-closed-handback"
	closedPRViaAsk      = "pr-closed-ask"
)

// closedPRReason is muster's settle reason for message m of plugin p when it
// is a review of a pull request GitHub reports closed; false otherwise
// (unknown included). ctx acts as m's owner.
func (s *Server) closedPRReason(ctx context.Context, p plugins.Plugin, m plugins.Message) (string, bool) {
	if s.ext.ClosedPRs == nil || s.ext.ClosedPRs.States == nil || s.ext.ClosedPRs.Store == nil {
		return "", false
	}
	ref, ok := s.prMailRef(p, m)
	if !ok {
		return "", false
	}
	rctx, cancel := context.WithTimeout(ctx, prStateTimeout)
	defer cancel()
	st, err := s.ext.ClosedPRs.States.PullRequestState(rctx, ref.Repo, ref.PR)
	if err != nil {
		s.logger.Printf("prclosed: message %d: read %s: %v; handled as open", m.ID, prName(ref), err)
		return "", false
	}
	if st.State != "closed" {
		return "", false
	}
	how := "closed without being merged"
	if st.Merged {
		how = "merged"
	}
	return plugins.SettledByMuster + "a review of pull request " + prName(ref) + ", which GitHub reports already " + how +
		": nothing left to act on", true
}

// settleClosedPRReview settles a NEW message m on arrival when it reviews a
// closed pull request: handed to the chief with no turn, and settled. It
// reports whether m is taken care of; false hands it on as before.
func (s *Server) settleClosedPRReview(ctx context.Context, p plugins.Plugin, m plugins.Message) bool {
	reason, ok := s.closedPRReason(ctx, p, m)
	if !ok {
		return false
	}
	chief, err := s.chiefAgent(ctx)
	if err != nil {
		s.logger.Printf("prclosed: message %d: no chief to settle it under (%v); handed on", m.ID, err)
		return false
	}
	if err := s.ext.ClosedPRs.Store.SettleQueuedAsMuster(ctx, chief.ID, agentOwnerID(chief), m.ID, reason, closedPRViaArrival); err != nil {
		s.logger.Printf("prclosed: message %d: settle: %v; handed on", m.ID, err)
		return false
	}
	s.logger.Printf("prclosed: message %d: %s", m.ID, reason)
	return true
}

// settleClosedPRMessage settles delivered message messageID when it reviews a
// closed pull request, and reports whether it did (then nothing more is owed
// for it: no hand-back, no ask). ctx acts as its owner; via names the path.
func (s *Server) settleClosedPRMessage(ctx context.Context, messageID int64, via string) bool {
	if s.ext.ClosedPRs == nil || messageID == 0 {
		return false
	}
	m, err := s.ext.Plugins.Message(ctx, messageID)
	if err != nil {
		return false
	}
	p, err := s.ext.Plugins.Plugin(ctx, m.PluginID)
	if err != nil {
		return false
	}
	reason, ok := s.closedPRReason(ctx, p, m)
	if !ok {
		return false
	}
	if err := s.ext.ClosedPRs.Store.SettleAsMuster(ctx, messageID, reason, via); err != nil {
		s.logger.Printf("prclosed: message %d: settle: %v", messageID, err)
		return false
	}
	s.logger.Printf("prclosed: message %d: %s", messageID, reason)
	return true
}
