package api

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/redact"
	"github.com/ZacxDev/muster/internal/sessionstate"
)

// ---------------------------------------------------------------------------
// SESSION STATE AND EPISODES (internal/sessionstate, migration 0073;
// docs/design/session-state.md). OFF unless MUSTER_SESSION_STATE=1. Its
// readers: the episode push and the Asks page's Stalled / waiting rows
// (session_push.go).
//
// Every SessionStateInterval the pass computes, for each owner, every
// session's state from the stall facts (stallFacts — the same read the stall
// nudges and any report use, never a copy) and keeps session_episodes in step:
//
//   - a session that enters needs_you or stalled OPENS an episode, claimed by
//     the partial unique index (one open episode per session, across
//     replicas) — unless its last episode ended HANDLED on the same streak
//     (sessionstate.Handled: the owner opened it, or the 0079 reset), so a
//     stop the owner dealt with is not pushed again while it sits still — and
//     one another process opened anyway is ended again (PG.EndRepeat);
//   - an open episode follows its session (state, where it is, its streak) and
//     ENDS when the OWNER ACTS in the session after it opened — a message of
//     their own, or a waiting prompt answered (sessionstate.OwnerActed;
//     episodeOwnerActed reads it) — when the owner OPENS the session's own page
//     in muster (episodeViewed, session_viewed.go), after sustained work (sessionstate.Advance;
//     for an agent, a reply turn that long), or when its window is gone from a
//     host that is online, reporting status, and has sent its window list. A
//     host that dropped or has not been read, a failed agent list, a failed
//     transcript read, a failed classifier: none of them ends an episode or
//     quiets one.
//
// 🔴 THE CLASSIFIER RUNS ONLY WHERE THE CHIEF'S NUDGES WOULD RUN IT: the
// feature on and the owner's chief on (docs/chief-nudges.md, "The stall
// classifier"). Everywhere else no model is called, and no transcript is read
// to classify: an idle stop is "stalled" and labelled as asking
// (sessionstate.Asked), so the classifier's absence can never quiet it. The
// one read made for every owner is the owner-acted check's
// (episodeOwnerActed): it calls no model, and reads an open episode once after
// it opens, then again only when its session moves. Where it does run, its input is
// stallClassifierText — redacted and cut — the SAME text the nudge sweep sends,
// and its answer is billed once per streak whichever pass runs first.
//
// 🔴 THIS PASS POLLS (every 30 s). A waiting window is pushed AT ONCE, from the
// mirror hub's change hook (attentionMirrorChanged → episodeWaitingPushes);
// this pass pushes a waiting stop only when the hook missed it, and pushes an
// idle stop (session_push.go) — a stall is 10 minutes old by then.
// ---------------------------------------------------------------------------

// SessionStateInterval is how often the state pass runs.
const SessionStateInterval = 30 * time.Second

// sessionStateClassifyPerPass bounds, across every owner, how many idle
// streaks one pass reads a transcript for and classifies. A session past it
// opens unclassified (counted as asked) and is classified on a later pass.
const sessionStateClassifyPerPass = 5

// The error classes an episode stores when no classifier was run for it, so
// it is counted as asked and its streak is not read again.
const (
	// jevNotConfigured: the owner's classifier does not run (the chief's
	// nudges are off, there is no classifier, or the owner's chief is off).
	jevNotConfigured = "not_configured"
	// jevNoTranscript: a view-only runtime muster reads no transcript of
	// (Hermes).
	jevNoTranscript = "no_transcript"
)

// sessionStateOn reports whether the pass runs here (MUSTER_SESSION_STATE=1,
// with a database).
func (s *Server) sessionStateOn() bool { return s.ext.Episodes != nil }

// RunSessionStateSweep runs SessionStatePass every interval (<= 0:
// SessionStateInterval) until ctx ends. It does nothing when off.
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

// statePass is one owner's part of a pass: what every session's step reads.
type statePass struct {
	owner  int64
	cl     *chiefnudge.Classifier // nil: the owner's classifier does not run
	budget *int
	now    time.Time
	// judges: the owner's chief judges idle stops live (episodeChiefJudges),
	// so an idle stop's push waits on its decision.
	judges bool
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
	// ONE read of the owner's hosts for everything below: the stall facts,
	// the answerable asks and which hosts can say a window is gone.
	var hs []mirror.HostView
	if s.ext.Mirror != nil {
		hs = s.ext.Mirror.Hosts(octx)
	}
	// The hosts that can say a window is GONE: online, reporting status, and
	// with a window list read. 🔴 A host that reports status but has sent no
	// window list has not been read (mirror.Hub.AgentState) — an empty
	// listing is unknown, never "every window gone".
	reporting := map[string]bool{}
	for _, h := range hs {
		if h.Online && h.StatusReport && len(h.Windows) > 0 {
			reporting[h.Host] = true
		}
	}
	answerable := answerableWaits(s.waitingSessionsIn(octx, hs))
	sp := statePass{owner: owner, cl: s.stateClassifier(octx), budget: budget, now: s.now(), judges: s.episodeChiefJudges(octx, owner)}
	// The restart rule's clear first: an ended stop's notification is closed
	// whether it ended in a pass, or while muster was down.
	defer s.clearEndedEpisodes(octx, owner)
	read := s.stallFacts(octx, nil, hs, sessionStateListsHermes)
	seen := map[string]bool{}
	agentsSeen := map[int64]bool{}
	for _, c := range read.sessions {
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
		s.sessionStateStep(octx, sp, c, ep, has, answerable[c.Key])
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
			// 🔴 A FAILED AGENT LIST PROVES NOTHING: it lists no agent at all.
			gone = read.agentsRead && !agentsSeen[ep.AgentID]
		}
		if gone {
			s.endEpisode(octx, ep, sessionstate.EndGone, sp.now)
		}
	}
}

// stateClassifier is octx's owner's stall classifier for the state pass, nil
// where the chief's nudges would not run one: the feature off, no classifier,
// or the owner with no chief (or theirs turned off).
func (s *Server) stateClassifier(octx context.Context) *chiefnudge.Classifier {
	if !s.chiefNudgesOn() || s.ext.ChiefNudge.Classifier == nil {
		return nil
	}
	if _, err := s.chiefAgent(octx); err != nil {
		return nil
	}
	return s.ext.ChiefNudge.Classifier
}

// sessionStateStep is one session's observation: ep is its open episode when
// has.
func (s *Server) sessionStateStep(octx context.Context, sp statePass, c nudgeCandidate, ep sessionstate.Episode, has, answerable bool) {
	now := sp.now
	if has {
		// 🔴 A HANDLED STOP OPENED AGAIN ENDS AGAIN (PG.EndRepeat): an open
		// episode on the streak its previous episode ended 'reset' or 'viewed'
		// on. PG.Open's guard cannot refuse every such write, so this is a
		// PERMANENT repair, not a rollout step. Three writers: an image from
		// before 0079 (no guard), still running during a rolling update; the
		// waiting hook — the older image's, and this one's whenever
		// windowStreakKey returns "" — which opens with no streak, and "" never
		// matches the guard; and a pass whose facts were read before a
		// concurrent view, which opens on the streak it read, not the one the
		// view ended on. Compared on c's CURRENT streak, it is ended here for
		// that same reason, its push is withdrawn by this pass's
		// clearEndedEpisodes, and the Open below then refuses it (the same rule).
		reason, ended, err := s.ext.Episodes.EndRepeat(octx, ep.ID, c.StreakKey, now)
		if err != nil {
			s.logger.Printf("session state: episode %d: repeat: %v", ep.ID, err)
		} else if ended {
			s.logger.Printf("session state: episode %d: ended %s: it repeats a handled stop", ep.ID, reason)
			s.ownerChecked.settle(ep.ID)
			has = false
		}
	}
	// anchor is the streak the episode keeps: normally the session's current
	// one. 🔴 FOR AN AGENT MID-TURN IT STAYS PUT: while the newest message is
	// the owner's (a turn no turn gate saw — the API message door, a job
	// turn), the stop's anchor is NOT moved onto that prompt, so the reply,
	// when it lands, is timed from the first message after the stop rather
	// than from the prompt (which would read a single long reply as 0 s and
	// never end the stop).
	anchor := c.StreakKey
	if has && c.Kind == chiefnudge.KindAgent && c.StreakKey != ep.StreakKey {
		switch {
		case c.ownerLast:
			anchor = ep.StreakKey
		case s.agentTurnWork(octx, c, ep) >= sessionstate.EndAfter:
			// 🔴 AN AGENT HAS NO BUSY STATUS TO TIME, SO ITS TURN IS TIMED BY
			// ITS MESSAGES: a new reply ends the stop only when the turn ran at
			// least EndAfter (agentTurnWork). A quick exchange — it asks, the
			// owner answers, it asks a follow-up — stays one stop, as a
			// mirrored window's would.
			s.endEpisode(octx, ep, sessionstate.EndWorked, now)
			has = false
		}
	}
	f := sessionstate.Facts{Waiting: c.Kind == chiefnudge.KindMirror && c.Status == mirror.StatusWaiting, Answerable: answerable}
	var jev chiefnudge.Result
	classified := false
	if chiefnudge.Exclude(c.Session, s.chiefNudgeIdle()) == "" {
		f.Stalled = true
		switch {
		case has && ep.ClassifiedStreak == c.StreakKey && !(ep.Jev.Err == jevNotConfigured && sp.cl != nil):
			// Classified for this streak already — by an earlier pass, or
			// before a restart.
			jev, classified = episodeJev(ep), true
		case c.noTranscript:
			jev, classified = chiefnudge.Result{Err: jevNoTranscript}, true
		case sp.cl == nil:
			// With no classifier no transcript is read, so a streak that ends
			// on the OWNER's word (an Esc-interrupted window, a prompt not yet
			// taken up) opens a "stalled, asked" stop too. DECIDED (coordinator
			// default N5, session_push.go): an agent conversation whose newest
			// message is the owner's is a quiet row and never pushes; a
			// mirrored window's last word is unknown here, so it pushes.
			jev, classified = chiefnudge.Result{Err: jevNotConfigured}, true
		default:
			var lastOwn bool
			jev, classified, lastOwn = s.sessionStateClassify(octx, sp, c)
			// The owner spoke last: the session is not stalled on its own word
			// (the nudge sweep's rule). An unreadable transcript is NOT that:
			// it stays a stop, unclassified.
			f.Stalled = lastOwn
		}
		f.AskStall = f.Stalled && s.needsYouStall(jev)
	}
	state, wants := sessionstate.StateOf(f)
	busy := c.Kind == chiefnudge.KindMirror && c.Status == mirror.StatusBusy
	if has && s.episodeOwnerActed(octx, c, ep) {
		// The owner went to the session and acted in it: the stop is over
		// (its push is withdrawn by this pass's clearEndedEpisodes), whether
		// or not the session then works for EndAfter.
		s.endEpisode(octx, ep, sessionstate.EndOwner, now)
		has = false
	}
	if has {
		step := sessionstate.Advance(busy && !wants, ep.BusySince, ep.BusySeen, now)
		if step.End {
			s.endEpisode(octx, ep, step.Reason, now)
			has = false
		} else {
			o := sessionstate.Observation{State: ep.State, Waiting: ep.Waiting, Host: c.Host, TmuxSession: c.TmuxSession, Window: c.Window,
				Index: c.Index, StreakKey: anchor, BusySince: step.BusySince, BusySeen: step.BusySeen, At: now}
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
			// Another pass or replica holds this stop, or the owner already
			// handled it on this streak (sessionstate.Handled).
			return
		}
		ep = opened
		// Nothing the owner wrote can be after an episode opened this instant:
		// its owner-acted check starts from this streak.
		s.ownerChecked.checked(ep.ID, c.StreakKey)
	}
	if f.Stalled && classified && (ep.ClassifiedStreak != c.StreakKey || ep.Jev.Err != jev.Err || ep.Jev.Label != jev.Label) {
		asked := sessionstate.Asked(true, jev.Err, f.AskStall)
		if err := s.ext.Episodes.SetClassification(octx, ep.ID, c.StreakKey, classificationOf(jev), asked); err != nil {
			s.logger.Printf("session state: episode %d: classify: %v", ep.ID, err)
		}
	}
	// The stop's one push (session_push.go): what this pass saw of it now.
	ep.State, ep.StreakKey = state, anchor
	s.settleEpisode(octx, ep, episodeFacts{
		Waiting: f.Waiting,
		Asked:   !f.Stalled || sessionstate.Asked(classified, jev.Err, f.AskStall),
		// N5 (coordinator default): with no classifier, an agent conversation
		// whose newest message is the owner's is not stopped on its own word —
		// a quiet row, never a push. A mirrored window's last word is unknown
		// without a transcript, so it pushes.
		OwnerLast:   c.Kind == chiefnudge.KindAgent && c.ownerLast && sp.cl == nil,
		ChiefJudges: sp.judges,
		OpenFor:     now.Sub(ep.OpenedAt),
		Now:         now,
	}, c.IdleFor)
}

// agentTurnWork is how long the reply turn that moved c's conversation past
// ep's streak ran: from the first message after the stop's own (normally the
// owner's answer, which starts the turn) to the conversation's latest. A
// failed read is 0: the stop is not ended on it.
func (s *Server) agentTurnWork(octx context.Context, c nudgeCandidate, ep sessionstate.Episode) time.Duration {
	since, err := strconv.ParseInt(strings.TrimPrefix(ep.StreakKey, "msg:"), 10, 64)
	if err != nil || s.ext.Agents == nil {
		return 0
	}
	msgs, err := s.ext.Agents.ListRecentChatMessages(octx, c.ChatSessionID, chiefNudgeAgentMessages)
	if err != nil {
		return 0
	}
	var start, end time.Time
	for _, m := range msgs {
		if m.ID <= since {
			continue
		}
		if start.IsZero() || m.CreatedAt.Before(start) {
			start = m.CreatedAt
		}
		if m.CreatedAt.After(end) {
			end = m.CreatedAt
		}
	}
	if start.IsZero() {
		return 0
	}
	return end.Sub(start)
}

// sessionStateClassify classifies c's current streak for the state pass, on
// sp.cl (never nil here). classified: jev holds an answer for it (an error
// class included); lastOwn: the session's own message is the last one, or that
// could not be read — a failure never makes a session look like it has not
// stopped.
//
// 🔴 ONE CLASSIFIER BILL AND ONE TRANSCRIPT READ PER STREAK, AT MOST: a streak
// the nudge sweep classified reuses that answer (in memory, or its decision
// row after the sweep forgot it), a streak that ends on the owner's word is
// remembered as such, a read that fails is retried with an exponential
// backoff (chief_nudge_retry.go), and the pass's budget bounds the reads as
// well as the calls.
func (s *Server) sessionStateClassify(octx context.Context, sp statePass, c nudgeCandidate) (jev chiefnudge.Result, classified, lastOwn bool) {
	now := sp.now
	rkey := nudgeRetryKey(sp.owner, c.Session)
	if r, ok := s.nudgeRetry.cachedJev(rkey, now); ok {
		return r, true, true
	}
	if r, ok := s.nudgeJevFor(octx, c.Session); ok {
		s.nudgeRetry.keepJev(rkey, r, now)
		return r, true, true
	}
	if s.nudgeRetry.isOwnerLast(rkey, now) {
		return chiefnudge.Result{}, false, false
	}
	if !s.nudgeRetry.readDue(rkey, now) || *sp.budget <= 0 {
		return chiefnudge.Result{}, false, true
	}
	*sp.budget--
	read, err := s.nudgeTranscript(octx, c)
	if err != nil {
		s.nudgeRetry.readFailed(rkey, now)
		s.logger.Printf("session state: %s %s: transcript: %v", c.Kind, c.Key, err)
		return chiefnudge.Result{}, false, true
	}
	last, ok := chiefnudge.LastIsAssistant(read.Msgs)
	if !ok {
		s.nudgeRetry.markOwnerLast(rkey, now)
		return chiefnudge.Result{}, false, false
	}
	jev = s.classifyStall(octx, sp.cl, sp.owner, last.Text)
	// The nudge sweep reuses it for this streak rather than bill the owner's
	// key twice.
	s.nudgeRetry.keepJev(rkey, jev, now)
	return jev, true, true
}

// 🔴 THE STALL CLASSIFIER'S ONE DOOR. Every call to the classifier goes through
// classifyStall, which sends stallClassifierText of the session's last
// message: redacted, and cut to the backtested length, exactly what the chief
// is shown. The stall ledger (fleet_collector_test.go) pins classifyStall as
// the only caller of Classify and its two callers, so a third, unredacted call
// site fails a test.

// stallClassifierText is the session's last message as the classifier and the
// chief see it: cut to chiefnudge.MaxLastRunes, then redacted.
func stallClassifierText(text string) string {
	return redact.Secrets(chiefnudge.Cut(text, chiefnudge.MaxLastRunes))
}

// classifyStall classifies text (the session's raw last message) for owner.
func (s *Server) classifyStall(ctx context.Context, cl *chiefnudge.Classifier, owner int64, text string) chiefnudge.Result {
	return cl.Classify(ctx, owner, stallClassifierText(text))
}

// streakJevReader is the nudge store's read of a streak's recorded
// classification (chiefnudge.PG.JevForStreak); a store without it adds
// nothing.
type streakJevReader interface {
	JevForStreak(ctx context.Context, kind, key, streak string) (chiefnudge.Result, bool, error)
}

// nudgeJevFor is the classification the nudge sweep recorded for c's streak.
func (s *Server) nudgeJevFor(octx context.Context, c chiefnudge.Session) (chiefnudge.Result, bool) {
	r, ok := s.ext.ChiefNudges.(streakJevReader)
	if !ok || s.ext.ChiefNudges == nil {
		return chiefnudge.Result{}, false
	}
	jev, found, err := r.JevForStreak(octx, c.Kind, c.Key, c.StreakKey)
	if err != nil || !found {
		return chiefnudge.Result{}, false
	}
	return jev, true
}

// episodeJevFor is the classification session state stored on c's open
// episode for c's streak (it outlives a restart): only a real answer, never a
// not-run or failed one.
func (s *Server) episodeJevFor(octx context.Context, c chiefnudge.Session) (chiefnudge.Result, bool) {
	if s.ext.Episodes == nil {
		return chiefnudge.Result{}, false
	}
	ep, ok, err := s.ext.Episodes.OpenFor(octx, c.Kind, c.Key)
	if err != nil || !ok || ep.ClassifiedStreak != c.StreakKey || ep.Jev.Err != "" || ep.Jev.Label == "" {
		return chiefnudge.Result{}, false
	}
	return episodeJev(ep), true
}

// endEpisode ends ep for reason.
func (s *Server) endEpisode(octx context.Context, ep sessionstate.Episode, reason string, now time.Time) {
	// Its notification is closed by the same pass's clearEndedEpisodes (the
	// owner's pass defers it), which also closes one pushed after this pass
	// read the episode.
	if _, err := s.ext.Episodes.End(octx, ep.ID, reason, now); err != nil {
		s.logger.Printf("session state: episode %d: end: %v", ep.ID, err)
		return
	}
	s.ownerChecked.settle(ep.ID)
}

// episodeOwnerActed reports whether the owner acted in open episode ep's
// session c after it opened (sessionstate.OwnerActed, the rule): a waiting
// episode's prompt answered is known from the status alone; an owner message
// needs the conversation, which is READ WHEN THIS PROCESS HAS NOT YET READ IT
// ON THE SESSION'S CURRENT STREAK (ownerChecked) — so every open episode is
// read at least once after it opened (or after a restart), and again only when
// its session moves (a new status for a window, a new message for an agent). A
// failed read leaves it unread, so it is read again on the next pass. The read
// is the nudge sweep's own (nudgeTranscript) and muster's own pastes are told
// apart by the sweep's own record of them (ownerInjected), so a chief nudge is
// never the owner acting.
//
// 🔴 NOT "THE STREAK MOVED SINCE THE EPISODE'S STORED ONE": every pass's
// Observe writes the current streak onto the episode, so a session that moved
// and was observed before this check existed (or while a read failed under
// an older rule) would never look moved again, and its episode would never be
// read. What was READ is kept apart from what was OBSERVED.
func (s *Server) episodeOwnerActed(octx context.Context, c nudgeCandidate, ep sessionstate.Episode) bool {
	f := sessionstate.OwnerFacts{OpenedAt: ep.OpenedAt, WasWaiting: ep.Waiting, Known: c.Status != "",
		WaitingNow: c.Kind == chiefnudge.KindMirror && c.Status == mirror.StatusWaiting}
	if !f.Known || f.WaitingNow || f.WasWaiting {
		return sessionstate.OwnerActed(f)
	}
	if c.noTranscript || s.ownerChecked.done(ep.ID, c.StreakKey) {
		return false
	}
	read, err := s.nudgeTranscript(octx, c)
	if err != nil {
		s.logger.Printf("session state: episode %d: the owner-acted read: %v", ep.ID, err)
		return false
	}
	injected, err := s.ownerInjected(octx, c.Session)
	if err != nil {
		s.logger.Printf("session state: episode %d: the owner-acted read: injected: %v", ep.ID, err)
		return false
	}
	s.ownerChecked.checked(ep.ID, c.StreakKey)
	f.Msgs, f.Injected, f.Prefixes = read.Msgs, injected, nudgeInjectedPrefixes
	return sessionstate.OwnerActed(f)
}

// ownerInjected is nudgeInjected for the owner-acted check, which runs with
// the chief's nudges off too: with no nudge store there are no nudges or
// relays of its own to leave out, and the send queue's pastes still are.
func (s *Server) ownerInjected(octx context.Context, c chiefnudge.Session) ([]string, error) {
	if s.ext.ChiefNudges != nil {
		return s.nudgeInjected(octx, c)
	}
	if q, ok := s.ext.SendQueue.(injectedBodyReader); ok && c.Kind == chiefnudge.KindMirror {
		return q.InjectedBodies(octx, s.now().Add(-7*24*time.Hour), 1000)
	}
	return nil, nil
}

// episodeChecks is, per open episode, the streak its owner-acted read last
// succeeded on (episodeOwnerActed). In memory: after a restart every open
// episode is read once more, then again only when its session moves.
type episodeChecks struct {
	mu sync.Mutex
	m  map[int64]string
}

// checked records that episode id's conversation was read on streak.
func (o *episodeChecks) checked(id int64, streak string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.m == nil {
		o.m = map[int64]string{}
	}
	o.m[id] = streak
}

// done reports whether episode id was read on streak already.
func (o *episodeChecks) done(id int64, streak string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	got, ok := o.m[id]
	return ok && got == streak
}

// settle forgets episode id (it ended).
func (o *episodeChecks) settle(id int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.m, id)
}

// answerableWaits is the session keys (an item's claudeSession: the Claude
// session id, or a runtime-qualified key) whose wait is an ask muster can
// answer: a held AskUserQuestion or an opencode question (a question item),
// a Hermes approval with a prompt id its host answers, an opencode
// permission request its host answers.
func answerableWaits(items []attentionItem) map[string]bool {
	out := map[string]bool{}
	for _, it := range items {
		if it.claudeSession != "" && answerableItem(it) {
			out[it.claudeSession] = true
		}
	}
	return out
}

// answerableItem reports whether waiting item it is an ask muster can answer
// (answerableWaits' rule, one place): it keeps its own full-text push and is
// never an episode.
func answerableItem(it attentionItem) bool {
	return it.Kind == attentionKindQuestion ||
		it.hermes != nil && it.hermes.promptID != "" ||
		it.openCode != nil && it.openCode.kind != "" && it.openCode.why == ""
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
