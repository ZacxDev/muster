package api

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/githubapp"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/prroute"
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
// The reason says which: merged, or closed WITHOUT being merged. A closed,
// unmerged pull request's work may have been re-filed as another one, so its
// settle also leaves the chief a one-line note (closedPRNote); a merged one's
// does not.
//
// 🔴 ONLY A CLOSED ANSWER SETTLES. An open pull request, a repository the App
// has no installation for, a GitHub error or a slow answer are all "unknown",
// and unknown keeps today's behaviour exactly — a review of an open pull
// request is never dropped on a guess.
//
// GitHub is not asked every time: each answer is cached per pull request
// (closedPRTTL for closed, openPRTTL for open or unknown), and a sweep pass
// makes at most prReadsPerPass reads — past that the state is unknown for the
// rest of the pass, so a slow GitHub cannot stall a sweep or exhaust the App's
// rate limit.
// ---------------------------------------------------------------------------

// PRStateReader reads a pull request's state from GitHub (the GitHub App:
// githubapp.App.PullRequestState).
type PRStateReader interface {
	PullRequestState(ctx context.Context, repo string, n int64) (githubapp.PRState, error)
}

// ClosedPRs is the closed-pull-request settle's dependencies, and its cache of
// GitHub's answers. The zero cache is ready to use.
type ClosedPRs struct {
	States PRStateReader
	Store  plugins.PRReviewSettler
	// Timeout bounds one GitHub read (prStateTimeout when 0).
	Timeout time.Duration
	// Now is the cache's clock (time.Now when nil).
	Now func() time.Time

	mu    sync.Mutex
	cache map[string]prStateEntry
}

// prStateEntry is one cached answer: the state when known, and until when it
// is used.
type prStateEntry struct {
	st    githubapp.PRState
	known bool
	until time.Time
}

const (
	// prStateTimeout bounds one GitHub read: past it the state is unknown.
	prStateTimeout = 5 * time.Second
	// closedPRTTL is how long a CLOSED answer is reused: a reopen is rare.
	closedPRTTL = 24 * time.Hour
	// openPRTTL is how long an OPEN or UNKNOWN answer is reused.
	openPRTTL = 10 * time.Minute
	// prReadsPerPass caps the GitHub reads one sweep pass makes.
	prReadsPerPass = 5
	// prCacheMax bounds the cache: past it, it is emptied.
	prCacheMax = 4096
)

// prReadBudgetKey carries a sweep pass's remaining GitHub reads.
type prReadBudgetKey struct{}

// withPRReadBudget is ctx for one sweep pass: at most n GitHub reads.
func withPRReadBudget(ctx context.Context, n int32) context.Context {
	b := &atomic.Int32{}
	b.Store(n)
	return context.WithValue(ctx, prReadBudgetKey{}, b)
}

func (c *ClosedPRs) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// state is pull request ref's state and whether it is known: from the cache,
// else from GitHub within ctx's read budget (none: an arrival, one read).
func (c *ClosedPRs) state(ctx context.Context, ref prroute.Ref) (githubapp.PRState, bool, error) {
	key := prName(ref)
	now := c.now()
	c.mu.Lock()
	e, hit := c.cache[key]
	c.mu.Unlock()
	if hit && now.Before(e.until) {
		return e.st, e.known, nil
	}
	if b, ok := ctx.Value(prReadBudgetKey{}).(*atomic.Int32); ok && b.Add(-1) < 0 {
		return githubapp.PRState{}, false, errPRReadBudget
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = prStateTimeout
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	st, err := c.States.PullRequestState(rctx, ref.Repo, ref.PR)
	e = prStateEntry{st: st, known: err == nil, until: now.Add(openPRTTL)}
	if e.known && st.State == "closed" {
		e.until = now.Add(closedPRTTL)
	}
	c.mu.Lock()
	if c.cache == nil || len(c.cache) >= prCacheMax {
		c.cache = map[string]prStateEntry{}
	}
	c.cache[key] = e
	c.mu.Unlock()
	return st, e.known, err
}

// errPRReadBudget: the sweep pass has made its prReadsPerPass reads.
var errPRReadBudget = errors.New("this sweep pass has made its " + strconv.Itoa(prReadsPerPass) + " GitHub reads")

// closedPR is a review's closed pull request: muster's settle reason, and the
// chief's note when it was closed WITHOUT being merged ("" when merged).
type closedPR struct {
	reason, note string
}

// closedPRReason is the settle for message m of plugin p when it is a review
// of a pull request GitHub reports closed; false otherwise (unknown included).
// ctx acts as m's owner.
func (s *Server) closedPRReason(ctx context.Context, p plugins.Plugin, m plugins.Message) (closedPR, bool) {
	c := s.ext.ClosedPRs
	if c == nil || c.States == nil || c.Store == nil {
		return closedPR{}, false
	}
	ref, ok := s.prMailRef(p, m)
	if !ok {
		return closedPR{}, false
	}
	st, known, err := c.state(ctx, ref)
	if !known {
		if err != nil {
			s.logger.Printf("prclosed: message %d: read %s: %v; handled as open", m.ID, prName(ref), err)
		}
		return closedPR{}, false
	}
	if st.State != "closed" {
		return closedPR{}, false
	}
	if st.Merged {
		return closedPR{reason: plugins.SettledByMuster + "a review of pull request " + prName(ref) +
			", which GitHub reports already merged: nothing left to act on"}, true
	}
	return closedPR{
		reason: plugins.SettledByMuster + "a review of pull request " + prName(ref) +
			", which GitHub reports already closed WITHOUT being merged: nothing left to act on there",
		note: "[muster] plugin message_id " + strconv.FormatInt(m.ID, 10) + " is a review of pull request " + prName(ref) +
			", which GitHub reports closed WITHOUT being merged, so muster settled it. If that work was re-filed as another " +
			"pull request, its findings may still apply there.",
	}, true
}

// settleClosedPRReview settles a NEW message m on arrival when it reviews a
// closed pull request: handed to the chief with no turn, and settled (a closed,
// unmerged one recorded in the chief's conversation with its note). It reports
// whether m is taken care of; false hands it on as before.
func (s *Server) settleClosedPRReview(ctx context.Context, p plugins.Plugin, m plugins.Message) bool {
	c, ok := s.closedPRReason(ctx, p, m)
	if !ok {
		return false
	}
	chief, err := s.chiefAgent(ctx)
	if err != nil {
		s.logger.Printf("prclosed: message %d: no chief to settle it under (%v); handed on", m.ID, err)
		return false
	}
	if err := s.ext.ClosedPRs.Store.SettleQueuedAsMuster(ctx, chief.ID, agentOwnerID(chief), m.ID, c.reason, closedPRViaArrival); err != nil {
		s.logger.Printf("prclosed: message %d: settle: %v; handed on", m.ID, err)
		return false
	}
	s.logger.Printf("prclosed: message %d: %s", m.ID, c.reason)
	if c.note != "" && s.pluginDeliverer != nil {
		cur, err := s.ext.Plugins.Message(ctx, m.ID)
		if err != nil {
			cur = m
		}
		if err := s.pluginDeliverer.Handed(ctx, p, chief, cur, c.note); err != nil {
			s.logger.Printf("prclosed: message %d: record it in the chief's conversation: %v", m.ID, err)
		}
	}
	return true
}

// Where a closed review was settled (the audit's "via").
const (
	closedPRViaArrival  = "pr-closed-arrival"
	closedPRViaHandBack = "pr-closed-handback"
	closedPRViaAsk      = "pr-closed-ask"
)

// settleClosedPRMessage settles delivered message m when it reviews a closed
// pull request, and reports whether it did (then nothing more is owed for it:
// no hand-back, no ask). A closed, unmerged one leaves the chief that already
// holds it a note. ctx acts as its owner; via names the path.
func (s *Server) settleClosedPRMessage(ctx context.Context, m plugins.Message, via string) bool {
	if s.ext.ClosedPRs == nil || !s.prRouteOn() {
		return false
	}
	p, err := s.ext.Plugins.Plugin(ctx, m.PluginID)
	if err != nil {
		return false
	}
	c, ok := s.closedPRReason(ctx, p, m)
	if !ok {
		return false
	}
	if err := s.ext.ClosedPRs.Store.SettleAsMuster(ctx, m.ID, c.reason, via); err != nil {
		s.logger.Printf("prclosed: message %d: settle: %v", m.ID, err)
		return false
	}
	s.logger.Printf("prclosed: message %d: %s", m.ID, c.reason)
	if c.note != "" && p.ChiefAgentID != nil && p.ChiefSessionID != nil && s.ext.Agents != nil {
		// The chief's PRIMARY chat (primarySession): the plugin's recorded
		// session may be an archived former primary.
		if primary, ok := s.chiefPrimaryID(ctx, *p.ChiefAgentID); ok {
			if _, err := s.ext.Agents.AddChatMessage(context.WithoutCancel(ctx), agents.ChatMessage{AgentID: *p.ChiefAgentID,
				SessionID: primary, Role: "user", Content: c.note}); err != nil {
				s.logger.Printf("prclosed: message %d: note to the chief: %v", m.ID, err)
			}
		}
	}
	return true
}

// settleClosedPRRoute is settleClosedPRMessage for the message route r
// carries: the hand-back's check.
func (s *Server) settleClosedPRRoute(ctx context.Context, r plugins.Route) bool {
	if s.ext.ClosedPRs == nil || r.MessageID == 0 || !r.IsPRReview() {
		return false
	}
	m, err := s.ext.Plugins.Message(ctx, r.MessageID)
	if err != nil {
		return false
	}
	return s.settleClosedPRMessage(ctx, m, closedPRViaHandBack)
}
