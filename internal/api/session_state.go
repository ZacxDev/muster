package api

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/redact"
	"github.com/ZacxDev/muster/internal/sessionstate"
)

// ---------------------------------------------------------------------------
// SESSION STATE AND EPISODES (internal/sessionstate, migration 0072;
// docs/design/session-state.md). OFF unless MUSTER_SESSION_STATE=1: nothing
// reads the episodes until the design's next PR, which turns it on with its
// reader.
//
// Every SessionStateInterval the pass computes, for each owner, every
// session's state from the stall facts (stallFacts — the same read the stall
// nudges and any report use, never a copy) and keeps session_episodes in step:
//
//   - a session that enters needs_you or stalled OPENS an episode, claimed by
//     the partial unique index (one open episode per session, across
//     replicas);
//   - an open episode follows its session (state, where it is, its streak) and
//     ENDS only after sustained work (sessionstate.Advance; for an agent, a
//     reply turn that long) or when its window is gone from a host that is
//     online, reporting status, and has sent its window list. A host that
//     dropped or has not been read, a failed agent list, a failed transcript
//     read, a failed classifier: none of them ends an episode or quiets one.
//
// 🔴 THE CLASSIFIER RUNS ONLY WHERE THE CHIEF'S NUDGES WOULD RUN IT: the
// feature on and the owner's chief on (docs/chief-nudges.md, "The stall
// classifier"). Everywhere else no transcript is read and no model is called:
// an idle stop is "stalled" and labelled as asking (sessionstate.Asked), so
// the classifier's absence can never quiet it. Where it does run, its input is
// stallClassifierText — redacted and cut — the SAME text the nudge sweep sends,
// and its answer is billed once per streak whichever pass runs first.
//
// 🔴 FOR THE NEXT PR: this pass POLLS (every 30 s). Today a waiting window is
// pushed AT ONCE, from the mirror hub's change hook (attentionMirrorChanged).
// The episode push must keep pushing from that hook and use this pass only to
// keep the rows in step — pushing from the poll would add up to 30 s to every
// waiting push.
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
	sp := statePass{owner: owner, cl: s.stateClassifier(octx), budget: budget, now: s.now()}
	read := s.stallFacts(octx, nil, hs, true)
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
	if has && c.Kind == chiefnudge.KindAgent && c.StreakKey != ep.StreakKey {
		// 🔴 AN AGENT HAS NO BUSY STATUS TO TIME, SO ITS TURN IS TIMED BY ITS
		// MESSAGES: a new message ends the stop only when the reply turn ran at
		// least EndAfter (agentTurnWork). A quick exchange — it asks, the owner
		// answers, it asks a follow-up — stays one stop, as a mirrored window's
		// would.
		if s.agentTurnWork(octx, c, ep) >= sessionstate.EndAfter {
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
		case c.Runtime == mirror.RuntimeHermes:
			jev, classified = chiefnudge.Result{Err: jevNoTranscript}, true
		case sp.cl == nil:
			jev, classified = chiefnudge.Result{Err: jevNotConfigured}, true
		default:
			var lastOwn bool
			jev, classified, lastOwn = s.sessionStateClassify(octx, sp, c)
			// The owner spoke last: the session is not stalled on its own word
			// (the nudge sweep's rule). An unreadable transcript is NOT that:
			// it stays a stop, unclassified.
			f.Stalled = lastOwn
		}
		f.AskStall = f.Stalled && chiefnudge.AskStall(jev, s.chiefNudgeAskThreshold())
	}
	state, wants := sessionstate.StateOf(f)
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
	if f.Stalled && classified && (ep.ClassifiedStreak != c.StreakKey || ep.Jev.Err != jev.Err || ep.Jev.Label != jev.Label) {
		asked := sessionstate.Asked(true, jev.Err, f.AskStall)
		if err := s.ext.Episodes.SetClassification(octx, ep.ID, c.StreakKey, classificationOf(jev), asked); err != nil {
			s.logger.Printf("session state: episode %d: classify: %v", ep.ID, err)
		}
	}
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
