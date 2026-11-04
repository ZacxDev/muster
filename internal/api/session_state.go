package api

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sessionstate"
)

// ---------------------------------------------------------------------------
// SESSION STATE AND EPISODES (internal/sessionstate, migration 0072;
// docs/design/session-state.md).
//
// Every SessionStateInterval the pass computes, for each owner, every
// session's state from the stall facts (stallInputs — the same read the stall
// nudges and any report use, never a copy) and keeps session_episodes in step:
//
//   - a session that enters needs_you or stalled OPENS an episode, claimed by
//     the partial unique index (one open episode per session, across
//     replicas);
//   - an open episode follows its session (state, where it is, its streak) and
//     ENDS only after sustained work (sessionstate.Advance) or when its window
//     is gone from a host that is online and reporting status. A host that
//     dropped, a failed read, a failed classifier: none of them ends an
//     episode or quiets one.
//
// 🔴 INDEPENDENT OF THE CHIEF. The pass runs with MUSTER_CHIEF_NUDGES unset and
// for an owner with no chief; only the stall classifier, configured with the
// nudges, is borrowed when it is there. Without it every idle episode is
// "stalled" and counts as asked (sessionstate.Asked), so nothing it would have
// told can be quieted by its absence.
//
// This release RECORDS episodes and nothing more: the episode push, its tap
// route and the Attention section read them in the design's next PR.
// ---------------------------------------------------------------------------

// SessionStateInterval is how often the state pass runs.
const SessionStateInterval = 30 * time.Second

// sessionStateClassifyPerPass bounds the stall classifier calls one pass
// makes across every owner. A session left unclassified past it opens
// unclassified (counted as asked) and is classified on a later pass.
const sessionStateClassifyPerPass = 5

// sessionStateOn reports whether the pass runs here.
func (s *Server) sessionStateOn() bool { return s.ext.Episodes != nil }

// RunSessionStateSweep runs SessionStatePass every interval (<= 0:
// SessionStateInterval) until ctx ends. It does nothing without the store.
func (s *Server) RunSessionStateSweep(ctx context.Context, interval time.Duration) {
	if !s.sessionStateOn() {
		return
	}
	if interval <= 0 {
		interval = SessionStateInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := s.SessionStatePass(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Printf("session state: %v", err)
		}
	}
}

// SessionStatePass is one pass over every owner.
func (s *Server) SessionStatePass(ctx context.Context) error {
	if !s.sessionStateOn() {
		return nil
	}
	owners, err := s.ext.Episodes.Owners(ctx)
	if err != nil {
		return err
	}
	budget := sessionStateClassifyPerPass
	for _, o := range owners {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.sessionStateOwner(s.attentionPrincipal(ctx, o), o, &budget)
	}
	return nil
}

// sessionStateOwner is one owner's part of the pass, as that owner.
func (s *Server) sessionStateOwner(octx context.Context, owner int64, budget *int) {
	st := s.ext.Episodes
	open, err := st.OpenEpisodes(octx)
	if err != nil {
		s.logger.Printf("session state: owner %d: open episodes: %v", owner, err)
		return
	}
	byKey := map[string]sessionstate.Episode{}
	for _, e := range open {
		byKey[e.Kind+":"+e.Key] = e
	}
	// The hosts that can say a window is GONE: online and reporting status. A
	// window missing from any other host is unknown, never gone.
	reporting := map[string]bool{}
	var answerable map[string]bool
	if s.ext.Mirror != nil {
		hs := s.ext.Mirror.Hosts(octx)
		for _, h := range hs {
			if h.Online && h.StatusReport {
				reporting[h.Host] = true
			}
		}
		answerable = answerableWaits(s.waitingSessionsIn(octx, hs))
	}
	now := s.now()
	seen := map[string]bool{}
	agentsSeen := map[int64]bool{}
	for _, c := range s.stallInputs(octx, nil) {
		if c.OtherUser || c.Chief || c.Officer {
			continue
		}
		if c.Kind == chiefnudge.KindAgent {
			agentsSeen[c.AgentID] = true
			if c.ChatSessionID == 0 {
				continue // the agent as a whole (busy, or not sendable): no conversation to judge
			}
		}
		k := c.Kind + ":" + c.Key
		seen[k] = true
		ep, has := byKey[k]
		s.sessionStateStep(octx, owner, c, ep, has, answerable[c.Key], now, budget)
	}
	for k, ep := range byKey {
		if seen[k] {
			continue
		}
		gone := false
		switch ep.Kind {
		case sessionstate.KindMirror:
			gone = reporting[ep.Host]
		case sessionstate.KindAgent:
			gone = !agentsSeen[ep.AgentID]
		}
		if gone {
			s.endEpisode(octx, ep, sessionstate.EndGone, now)
		}
	}
}

// sessionStateStep is one session's observation: ep is its open episode when
// has.
func (s *Server) sessionStateStep(octx context.Context, owner int64, c nudgeCandidate, ep sessionstate.Episode, has, answerable bool,
	now time.Time, budget *int) {
	if has && c.Kind == chiefnudge.KindAgent && c.StreakKey != ep.StreakKey {
		// 🔴 AN AGENT CONVERSATION'S NEW MESSAGE ENDS ITS STOP: an agent has no
		// busy status to time, and a new message is a turn that ran. (Its next
		// stop, if it stops again, is a new episode.)
		s.endEpisode(octx, ep, sessionstate.EndWorked, now)
		has = false
	}
	f := sessionstate.Facts{Waiting: c.Kind == chiefnudge.KindMirror && c.Status == mirror.StatusWaiting, Answerable: answerable}
	var jev chiefnudge.Result
	classified := false
	if chiefnudge.Exclude(c.Session, s.chiefNudgeIdle()) == "" {
		switch {
		case has && ep.ClassifiedStreak == c.StreakKey:
			jev, classified = episodeJev(ep), true
			f.Stalled = true
		default:
			var lastOwn bool
			jev, classified, lastOwn = s.sessionStateClassify(octx, owner, c, budget, now)
			// The owner spoke last: the session is not stalled on its own word
			// (the nudge sweep's rule). An unreadable transcript is NOT that:
			// it stays a stop, unclassified.
			f.Stalled = lastOwn
		}
		f.AskStall = f.Stalled && chiefnudge.AskStall(jev, s.chiefNudgeAskThreshold())
	}
	state, wants := sessionstate.Classify(f)
	busy := c.Kind == chiefnudge.KindMirror && c.Status == mirror.StatusBusy
	if has {
		step := sessionstate.Advance(busy && !wants, ep.BusySince, ep.BusySeen, now)
		if step.End {
			s.endEpisode(octx, ep, step.Reason, now)
			has = false
		} else {
			o := sessionstate.Observation{State: ep.State, Waiting: ep.Waiting, Host: c.Host, TmuxSession: c.TmuxSession, Window: c.Window,
				Index: c.Index, StreakKey: c.StreakKey, BusySince: step.BusySince, BusySeen: step.BusySeen, At: now}
			if wants {
				o.State, o.Waiting = state, f.Waiting
			}
			if err := s.ext.Episodes.Observe(octx, ep.ID, o); err != nil {
				s.logger.Printf("session state: episode %d: %v", ep.ID, err)
			}
		}
	}
	if !wants {
		return
	}
	if !has {
		e := sessionstate.Episode{Kind: c.Kind, Key: c.Key, State: state, Waiting: f.Waiting, Asked: true, Runtime: c.Runtime,
			Host: c.Host, TmuxSession: c.TmuxSession, Window: c.Window, Index: c.Index, AgentID: c.AgentID,
			ChatSessionID: c.ChatSessionID, StreakKey: c.StreakKey}
		opened, claimed, err := s.ext.Episodes.Open(octx, e)
		if err != nil {
			s.logger.Printf("session state: %s %s: open: %v", c.Kind, c.Key, err)
			return
		}
		if !claimed {
			return // another pass or replica holds this stop
		}
		ep = opened
	}
	if f.Stalled && classified && ep.ClassifiedStreak != c.StreakKey {
		asked := sessionstate.Asked(true, jev.Err, f.AskStall)
		if err := s.ext.Episodes.Classify(octx, ep.ID, c.StreakKey, classificationOf(jev), asked); err != nil {
			s.logger.Printf("session state: episode %d: classify: %v", ep.ID, err)
		}
	}
}

// sessionStateClassify classifies c's current streak for the state pass.
// classified: jev holds an answer for it (an error class included); lastOwn:
// the session's own message is the last one, or that could not be read — a
// failure never makes a session look like it has not stopped.
//
// 🔴 ONE TRANSCRIPT READ PER STREAK, AT MOST: a streak the nudge sweep (or an
// earlier pass) classified reuses that answer (chief_nudge_retry.go), a streak
// that ends on the owner's word is remembered as such, and with no classifier
// the read's answer is stored on the episode as `not_configured`. Only a
// failed read, or a pass out of classifier budget, leaves it to a later pass.
func (s *Server) sessionStateClassify(octx context.Context, owner int64, c nudgeCandidate, budget *int, now time.Time) (jev chiefnudge.Result, classified, lastOwn bool) {
	var cl *chiefnudge.Classifier
	if s.ext.ChiefNudge != nil {
		cl = s.ext.ChiefNudge.Classifier
	}
	rkey := nudgeRetryKey(owner, c.Session)
	if r, ok := s.nudgeRetry.cachedJev(rkey, now); ok {
		return r, true, true
	}
	if s.nudgeRetry.isOwnerLast(rkey, now) {
		return chiefnudge.Result{}, false, false
	}
	if cl != nil && *budget <= 0 {
		return chiefnudge.Result{}, false, true
	}
	read, err := s.nudgeTranscript(octx, c)
	if err != nil {
		s.logger.Printf("session state: %s %s: transcript: %v", c.Kind, c.Key, err)
		return chiefnudge.Result{}, false, true
	}
	last, ok := chiefnudge.LastIsAssistant(read.Msgs)
	if !ok {
		s.nudgeRetry.markOwnerLast(rkey, now)
		return chiefnudge.Result{}, false, false
	}
	if cl == nil {
		return chiefnudge.Result{Err: jevNotConfigured}, true, true
	}
	*budget--
	jev = cl.Classify(octx, owner, last.Text)
	// The nudge sweep reuses it for this streak rather than bill the owner's
	// key twice.
	s.nudgeRetry.keepJev(rkey, jev, now)
	return jev, true, true
}

// jevNotConfigured is the error class an episode stores when no stall
// classifier is configured (the nudges are off): counted as asked.
const jevNotConfigured = "not_configured"

// endEpisode ends ep for reason.
func (s *Server) endEpisode(octx context.Context, ep sessionstate.Episode, reason string, now time.Time) {
	if _, err := s.ext.Episodes.End(octx, ep.ID, reason, now); err != nil {
		s.logger.Printf("session state: episode %d: end: %v", ep.ID, err)
	}
}

// answerableWaits is the session keys (an item's claudeSession: the Claude
// session id, or a runtime-qualified key) whose wait is an ask muster can
// answer: a held AskUserQuestion or an opencode question (a question item),
// a Hermes approval with a prompt id its host answers, an opencode
// permission request its host answers.
func answerableWaits(items []attentionItem) map[string]bool {
	out := map[string]bool{}
	for _, it := range items {
		if it.claudeSession == "" {
			continue
		}
		switch {
		case it.Kind == attentionKindQuestion,
			it.hermes != nil && it.hermes.promptID != "",
			it.openCode != nil && it.openCode.kind != "" && it.openCode.why == "":
			out[it.claudeSession] = true
		}
	}
	return out
}

// episodeJev is ep's stored classification as a classifier Result.
func episodeJev(ep sessionstate.Episode) chiefnudge.Result {
	return chiefnudge.Result{Label: ep.Jev.Label, AskScore: ep.Jev.AskScore, Model: ep.Jev.Model, Err: ep.Jev.Err,
		Scores: map[string]float64{chiefnudge.LabelNeedsInput: ep.Jev.NeedsInput, chiefnudge.LabelAsksPermission: ep.Jev.AsksPermission,
			chiefnudge.LabelNeither: ep.Jev.Neither}}
}

// classificationOf is r as an episode keeps it.
func classificationOf(r chiefnudge.Result) sessionstate.Classification {
	return sessionstate.Classification{Label: r.Label, NeedsInput: r.Scores[chiefnudge.LabelNeedsInput],
		AsksPermission: r.Scores[chiefnudge.LabelAsksPermission], Neither: r.Scores[chiefnudge.LabelNeither],
		AskScore: r.AskScore, Model: r.Model, Err: strings.TrimSpace(r.Err)}
}
