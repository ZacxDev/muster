package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/fence"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/redact"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// ---------------------------------------------------------------------------
// THE CHIEF'S STALL NUDGES (docs/chief-nudges.md; internal/chiefnudge).
//
// Every ChiefNudgeInterval the sweep looks at each owner's sessions — their
// mirrored Claude Code windows and their own dispatched agents, never an
// officer, the chief or another user's agent — and picks the ones that have
// stalled: idle for at least ChiefNudges.Idle, no background work, no turn
// error, no open ask, the last message the session's own, and this idle streak
// not decided yet. Only those reach the chief, so a tick with no candidate
// makes no model call.
//
// For each, the chief is asked once (a bounded call on the chief's own runtime,
// under a gateway key of its own so it never enters the operator's
// conversation) for ONE decision: nudge, ask, or leave. Then the guards — in
// code, never in the prompt — decide what muster does with it:
//
//   - a nudge is accepted only when its quote is, byte for byte, inside one of
//     the owner's OWN messages to that session (chiefnudge.CheckQuote over
//     chiefnudge.OwnerMessages): never the session's own words, never text muster
//     injected;
//   - at most chiefnudge.MaxNudgesPerAnchor nudges per owner message, then the
//     next becomes an ask;
//   - at most ChiefNudges.HourlyCap nudges an hour across every owner;
//   - the session must still be in the same idle streak when muster acts, and
//     the send queue's paste refuses a window that is waiting on a prompt or
//     has text in its box, as it always has.
//
// 🔴 SHADOW BY DEFAULT. An owner not in ChiefNudges.Live gets the whole
// pipeline — the chief's call, the guards, a recorded decision — and NOTHING
// else: no queued message, no turn, no attention ask (chief_nudges' CHECK
// refuses a shadow row that holds either). The Asks page's "Chief nudges" tab
// lists every decision.
//
// LIVE, a nudge is a LABELLED message quoting the owner (chiefnudge.NudgeText,
// never the owner's voice), queued like a routed message (send_queue
// .chief_nudge_id, gated by sendQueueNudgeGate) or sent as a turn to an agent.
// An ask is an attention ask on the owner's queue; the owner's answer, given on
// the Attention view, is relayed into the session VERBATIM (relayNudgeAnswer)
// — the one new path that types an answer into a mirrored session.
// ---------------------------------------------------------------------------

// ChiefNudges is the stall nudges' configuration (cmd/muster-server).
type ChiefNudges struct {
	// Idle is how long a session must have been idle (MUSTER_CHIEF_NUDGE_IDLE).
	Idle time.Duration
	// HourlyCap bounds nudges per hour across every owner, shadow and live
	// together (MUSTER_CHIEF_NUDGE_HOURLY_CAP).
	HourlyCap int
	// Live is the owners whose nudges are SENT (MUSTER_CHIEF_NUDGE_LIVE_OWNERS);
	// everyone else is in shadow mode.
	Live map[int64]bool
	// Classifier is the stall classifier (Jev over OpenRouter's Decisions API,
	// on the owner's OpenRouter key); nil: the chief decides alone.
	Classifier *chiefnudge.Classifier
	// AskThreshold is the ask score at which a stall defaults to an ask
	// (MUSTER_CHIEF_NUDGE_ASK_THRESHOLD; 0 = chiefnudge.DefaultAskThreshold).
	AskThreshold float64
}

// ChiefNudgeInterval is how often the sweep runs.
const ChiefNudgeInterval = 2 * time.Minute

const (
	// chiefNudgeMaxPerPass bounds the chief calls one pass makes, across every
	// owner: a fleet that stalls all at once costs a few calls a tick, not one
	// per session.
	chiefNudgeMaxPerPass = 5
	// chiefNudgeTurnBudget bounds one chief call.
	chiefNudgeTurnBudget = 3 * time.Minute
	// chiefNudgeAgentMessages is how many of an agent's newest messages are read.
	chiefNudgeAgentMessages = 200
	// chiefNudgePendingBatch bounds one reconcile.
	chiefNudgePendingBatch = 200
	// chiefNudgeAskLabel names the chief on a nudge's ask.
	chiefNudgeAskLabel = "your chief"
	// chiefNudgeAgentSessionAge: an agent conversation not active for this
	// long is not looked at (it is not a stall worth a call any more).
	chiefNudgeAgentSessionAge = 24 * time.Hour
)

// chiefNudgesOn reports whether the feature runs here.
func (s *Server) chiefNudgesOn() bool { return s.ext.ChiefNudge != nil && s.ext.ChiefNudges != nil }

// chiefNudgeLive reports whether owner's nudges are sent (live mode).
func (s *Server) chiefNudgeLive(owner int64) bool {
	return s.chiefNudgesOn() && s.ext.ChiefNudge.Live[owner]
}

func (s *Server) chiefNudgeMode(owner int64) string {
	if s.chiefNudgeLive(owner) {
		return chiefnudge.ModeLive
	}
	return chiefnudge.ModeShadow
}

func (s *Server) chiefNudgeIdle() time.Duration {
	if s.ext.ChiefNudge == nil || s.ext.ChiefNudge.Idle <= 0 {
		return chiefnudge.DefaultIdle
	}
	return s.ext.ChiefNudge.Idle
}

func (s *Server) chiefNudgeAskThreshold() float64 {
	if s.ext.ChiefNudge == nil || s.ext.ChiefNudge.AskThreshold <= 0 {
		return chiefnudge.DefaultAskThreshold
	}
	return s.ext.ChiefNudge.AskThreshold
}

func (s *Server) chiefNudgeHourlyCap() int {
	if s.ext.ChiefNudge == nil || s.ext.ChiefNudge.HourlyCap <= 0 {
		return chiefnudge.DefaultHourlyCap
	}
	return s.ext.ChiefNudge.HourlyCap
}

// RunChiefNudgeSweep runs ChiefNudgePass every interval (<= 0:
// ChiefNudgeInterval) until ctx ends. It does nothing with the feature off.
func (s *Server) RunChiefNudgeSweep(ctx context.Context, interval time.Duration) {
	if !s.chiefNudgesOn() {
		return
	}
	if interval <= 0 {
		interval = ChiefNudgeInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.nudgeKickChan():
			// An answer was claimed for a relay (relayNudgeAnswer): settle the
			// decisions in flight now, without waiting for the tick.
			s.reconcileNudges(ctx)
			continue
		case <-t.C:
		}
		if _, err := s.ChiefNudgePass(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Printf("chief nudges: %v", err)
		}
	}
}

// nudgeKickChan is the sweep's wake-up (one pending kick is enough).
func (s *Server) nudgeKickChan() chan struct{} {
	s.nudgeKickOnce.Do(func() { s.nudgeKick = make(chan struct{}, 1) })
	return s.nudgeKick
}

// kickNudges asks the sweep to settle the decisions in flight now.
func (s *Server) kickNudges() {
	select {
	case s.nudgeKickChan() <- struct{}{}:
	default:
	}
}

// reconcileNudges settles every live decision in flight (reconcileNudge),
// returning the owner sessions that are still in flight.
func (s *Server) reconcileNudges(ctx context.Context) (map[int64]map[string]bool, error) {
	pending, err := s.ext.ChiefNudges.Pending(ctx, chiefNudgePendingBatch)
	if err != nil {
		return nil, fmt.Errorf("pending decisions: %w", err)
	}
	busy := map[int64]map[string]bool{} // owner -> session keys with a decision in flight
	for _, d := range pending {
		if !s.reconcileNudge(ctx, d) {
			if busy[d.Owner] == nil {
				busy[d.Owner] = map[string]bool{}
			}
			busy[d.Owner][d.Kind+":"+d.Key] = true
		}
	}
	return busy, nil
}

// ChiefNudgePass is one sweep: settle the live decisions in flight, then
// decide each owner's stalled sessions. It returns how many decisions it
// recorded.
func (s *Server) ChiefNudgePass(ctx context.Context) (int, error) {
	if !s.chiefNudgesOn() {
		return 0, nil
	}
	busy, err := s.reconcileNudges(ctx)
	if err != nil {
		return 0, err
	}
	owners, err := s.ext.ChiefNudges.Owners(ctx)
	if err != nil {
		return 0, fmt.Errorf("owners: %w", err)
	}
	hourly, err := s.ext.ChiefNudges.NudgesSince(ctx, s.now().Add(-time.Hour))
	if err != nil {
		return 0, fmt.Errorf("hourly count: %w", err)
	}
	p := &nudgePass{budget: chiefNudgeMaxPerPass, hourly: hourly}
	for _, o := range owners {
		if p.budget <= 0 || ctx.Err() != nil {
			break
		}
		s.sweepOwnerNudges(s.attentionPrincipal(ctx, o.ID), o, busy[o.ID], p)
	}
	return p.decided, nil
}

// nudgePass is one pass's running totals.
type nudgePass struct {
	budget  int // chief calls left
	hourly  int // nudges in the last hour, this pass's included
	decided int
}

// nudgeCandidate is a session that passed chiefnudge.Exclude, with what acting
// on it needs.
type nudgeCandidate struct {
	chiefnudge.Session
	agent   agents.Agent
	session agents.ChatSession // an agent's conversation
}

// sweepOwnerNudges decides octx's owner's candidates, while the pass has
// budget. An owner with no chief (or its chief off) has nothing decided: the
// nudges are the chief's.
func (s *Server) sweepOwnerNudges(octx context.Context, o chiefnudge.Owner, inFlight map[string]bool, p *nudgePass) {
	chief, err := s.chiefAgent(octx)
	if err != nil {
		return
	}
	for _, c := range s.nudgeCandidates(octx, inFlight) {
		if p.budget <= 0 || octx.Err() != nil {
			return
		}
		decided, stop := s.decideNudge(octx, o, chief, c, p)
		if decided {
			p.decided++
		}
		if stop {
			return
		}
	}
}

// nudgeCandidates is octx's owner's sessions that pass chiefnudge.Exclude:
// mirrored Claude Code windows, then dispatched agents. inFlight names the
// sessions with a live decision still in flight (an open nudge ask, a queued
// nudge or relay).
func (s *Server) nudgeCandidates(octx context.Context, inFlight map[string]bool) []nudgeCandidate {
	owner := requestOwner(octx)
	now := s.now()
	idle := s.chiefNudgeIdle()
	asked := s.nudgeOpenAskers(octx)
	var all []nudgeCandidate
	if s.ext.Mirror != nil {
		seen := map[string]bool{}
		for _, h := range s.ext.Mirror.Hosts(octx) {
			if !h.Online || !h.StatusReport {
				continue
			}
			for _, w := range h.Windows {
				if !w.Claude || w.ClaudeSession == "" || w.Runtime != "" || seen[w.ClaudeSession] {
					continue
				}
				seen[w.ClaudeSession] = true
				st := s.ext.Mirror.AgentState(octx, h.Host, w.Session, w.ID)
				c := chiefnudge.Session{Kind: chiefnudge.KindMirror, Key: w.ClaudeSession, Host: h.Host,
					TmuxSession: w.Session, Window: w.ID, Label: w.Label, Status: st.Status,
					Shell:     !h.Work[w.ID].IsZero(),
					TurnError: st.TurnError != nil || st.HostLimit != nil || st.GuardHeld != "",
					OpenAsk:   asked["mirror:"+w.ClaudeSession] || inFlight[chiefnudge.KindMirror+":"+w.ClaudeSession]}
				if st.Unknown != nil {
					c.Status = ""
				}
				if st.Since > 0 && !st.Observed.IsZero() {
					c.StreakKey = "since:" + strconv.FormatInt(st.Since, 10)
					c.IdleSince, c.IdleFor = st.Observed, now.Sub(st.Observed)
				}
				all = append(all, nudgeCandidate{Session: c})
			}
		}
	}
	if s.ext.Agents != nil {
		list, err := s.ext.Agents.List(octx)
		if err != nil {
			s.logger.Printf("chief nudges: owner %d: agents: %v", owner, err)
		}
		isChief := chiefPredicate(list)
		for _, a := range list {
			_, _, officer := s.officerOf(a)
			agentKey := "agent:" + strconv.FormatInt(a.ID, 10)
			base := chiefnudge.Session{Kind: chiefnudge.KindAgent, Key: chiefnudge.AgentKey(a.ID, 0), AgentID: a.ID,
				Label: firstNonBlank(a.DisplayName, a.Name), OtherUser: a.OwnerID != owner, Chief: isChief(a), Officer: officer,
				OpenAsk: asked[agentKey]}
			if base.OtherUser || base.Chief || base.Officer ||
				!agentCanBeSent(s.liveStatus(octx, a)) || agents.KickoffOwed(a) || s.turns.busy(a.ID) {
				all = append(all, nudgeCandidate{Session: base, agent: a}) // status "": not idle
				continue
			}
			sessions, err := s.ext.Agents.ListSessions(octx, a.ID)
			if err != nil {
				continue
			}
			// 🔴 EACH CONVERSATION ON ITS OWN: an agent can hold several, and
			// the one that stalled is the one decided, nudged and relayed to.
			for _, sess := range sessions {
				if now.Sub(sess.UpdatedAt) > chiefNudgeAgentSessionAge {
					break // most recently active first: the rest are older still
				}
				last, err := s.ext.Agents.ListRecentChatMessages(octx, sess.ID, 1)
				if err != nil || len(last) == 0 {
					continue
				}
				m := last[len(last)-1]
				c := base
				c.ChatSessionID, c.Key = sess.ID, chiefnudge.AgentKey(a.ID, sess.ID)
				c.OpenAsk = c.OpenAsk || inFlight[chiefnudge.KindAgent+":"+c.Key]
				c.Status = "idle"
				c.StreakKey = "msg:" + strconv.FormatInt(m.ID, 10)
				c.IdleSince, c.IdleFor = m.CreatedAt, now.Sub(m.CreatedAt)
				c.TurnError = m.Role == "assistant" && strings.HasPrefix(m.Content, AgentTurnFailedPrefix)
				all = append(all, nudgeCandidate{Session: c, agent: a, session: sess})
			}
		}
	}
	var out []nudgeCandidate
	for _, c := range all {
		if chiefnudge.Exclude(c.Session, idle) != "" {
			continue
		}
		decided, err := s.ext.ChiefNudges.Decided(octx, c.Kind, c.Key, c.StreakKey)
		if err != nil || decided {
			continue
		}
		c.Title = s.nudgeTitle(octx, c.Session)
		out = append(out, c)
	}
	return out
}

// nudgeOpenAskers is the sessions of octx's owner with an open attention ask of
// their own, or an open question to the chief: "mirror:<claude session id>" or
// "agent:<id>". Such a session is waiting on an answer, not stalled.
func (s *Server) nudgeOpenAskers(octx context.Context) map[string]bool {
	out := map[string]bool{}
	if s.ext.Attention == nil {
		return out
	}
	open, err := s.ext.Attention.Open(octx)
	if err != nil {
		s.logger.Printf("chief nudges: open asks: %v", err)
		return out
	}
	for _, a := range open {
		switch {
		case a.Asker.MirrorSession != "":
			out["mirror:"+a.Asker.MirrorSession] = true
		case a.Asker.AgentID > 0:
			out["agent:"+strconv.FormatInt(a.Asker.AgentID, 10)] = true
		}
	}
	return out
}

// nudgeTitle is c's focus title, "" for none.
func (s *Server) nudgeTitle(ctx context.Context, c chiefnudge.Session) string {
	if s.ext.Focus == nil {
		return ""
	}
	sub := focus.MirrorSubject(c.Key)
	if c.Kind == chiefnudge.KindAgent {
		sub = focus.AgentSubject(c.AgentID)
	}
	f, ok, err := s.ext.Focus.Get(ctx, sub)
	if err != nil || !ok {
		return ""
	}
	return f.Title
}

// errNudgeChiefBusy: the chief could not take the call now (busy, not running,
// claimed); the owner's other candidates wait for the next pass.
var errNudgeChiefBusy = errors.New("the chief is busy")

// decideNudge reads c's transcript, asks the chief and acts on its answer
// under the guards. decided: a decision was recorded; stop: the owner's other
// candidates should wait (the chief is busy).
func (s *Server) decideNudge(octx context.Context, o chiefnudge.Owner, chief agents.Agent, c nudgeCandidate, p *nudgePass) (decided, stop bool) {
	read, err := s.nudgeTranscript(octx, c)
	msgs, full := read.Msgs, read.Full
	if err != nil {
		s.logger.Printf("chief nudges: %s %s: transcript: %v", c.Kind, c.Key, err)
		return false, false
	}
	last, ok := chiefnudge.LastIsAssistant(msgs)
	if !ok {
		// The owner spoke last (or the session said nothing): not stalled on
		// its own word.
		return false, false
	}
	injected, err := s.nudgeInjected(octx, c.Session)
	if err != nil {
		s.logger.Printf("chief nudges: %s %s: injected: %v", c.Kind, c.Key, err)
		return false, false
	}
	owner := chiefnudge.OwnerMessages(msgs, injected, nudgeInjectedPrefixes, s.now().Add(-chiefnudge.OwnerWindow),
		c.Kind == chiefnudge.KindAgent && full)
	untimed := chiefnudge.Untimed(msgs)
	// 🔴 THE CHIEF SEES REDACTED TEXT, AND THE QUOTE IS MATCHED AGAINST EXACTLY
	// WHAT IT SAW: each owner message is redacted and cut here, once, and both
	// the prompt and CheckQuote read this copy.
	for i := range owner {
		owner[i].Text = redact.Secrets(chiefnudge.Cut(owner[i].Text, chiefnudge.MaxOwnerMessageRunes))
	}
	last.Text = redact.Secrets(chiefnudge.Cut(last.Text, chiefnudge.MaxLastRunes))
	p.budget--
	d := chiefnudge.Decision{Kind: c.Kind, Key: c.Key, Host: c.Host, TmuxSession: c.TmuxSession, Window: c.Window,
		AgentID: c.AgentID, ChatSessionID: c.ChatSessionID, Title: c.Title, StreakKey: c.StreakKey, AnchorKey: chiefnudge.Anchor(owner),
		IdleSince: c.IdleSince, Mode: s.chiefNudgeMode(o.ID)}
	// The stall classifier first (jev.go): it runs in shadow too — measuring it
	// on real idle stalls is the point — and a failure only means the chief
	// decides alone.
	var jev chiefnudge.Result
	if cl := s.ext.ChiefNudge.Classifier; cl != nil {
		jev = cl.Classify(octx, o.ID, last.Text)
		if jev.Err != "" {
			s.logger.Printf("chief nudges: %s %s: the stall classifier: %s; the chief decides alone", c.Kind, c.Key, jev.Err)
		}
	}
	d = d.WithJev(jev)
	askStall := chiefnudge.AskStall(jev, s.chiefNudgeAskThreshold())
	hint := chiefnudge.Hint{}
	if askStall {
		hint = chiefnudge.Hint{AskStall: true, Label: jev.Label, AskScore: jev.AskScore}
	}
	raw, err := s.askChiefForNudge(octx, chief, nudgeSessionName(chief.Name, o.ID, c.Session),
		chiefnudge.Prompt(c.Session, last, owner, c.IdleFor, hint, fence.NewNonce()))
	if errors.Is(err, errNudgeChiefBusy) {
		return false, true
	}
	if err != nil {
		// 🔴 ONE FAILED CALL DOES NOT WEDGE THE OWNER'S SWEEP: it is recorded
		// on THIS candidate (so this streak is not retried every pass) and the
		// owner's next candidate is judged. A new streak is judged afresh.
		d.Decision, d.Outcome, d.Detail = chiefnudge.DecisionInvalid, chiefnudge.Suppressed(chiefnudge.GuardChiefError), "the chief's call failed"+failureKind(err)
		return s.recordNudge(octx, d, "chief_nudge.suppressed"), false
	}
	prop, perr := chiefnudge.ParseProposal(raw)
	if perr != nil {
		d.Decision, d.Outcome, d.Reason = chiefnudge.DecisionInvalid, chiefnudge.Suppressed(chiefnudge.GuardInvalid), perr.Error()
		return s.recordNudge(octx, d, "chief_nudge.suppressed"), false
	}
	d.Decision, d.Reason, d.Covered = prop.Decision, prop.Reason, prop.CoveredAction
	// 🔴 THE BACKTESTED RULE: an ask-stall (ask score at or over the threshold)
	// defaults to an ASK. A nudge survives it only when the classifier's label
	// is asks_permission AND the quote guard passes (the owner's own words);
	// a "leave", or any other nudge, becomes the ask.
	if askStall && prop.Decision != chiefnudge.DecisionAsk {
		why := ""
		switch {
		case prop.Decision == chiefnudge.DecisionLeave:
			why = "the chief said leave it"
		case jev.Label != chiefnudge.LabelAsksPermission:
			why = "a nudge is allowed only on an asks_permission stall, and this one is " + jev.Label
		default:
			if _, qerr := chiefnudge.CheckQuote(prop, owner); qerr != nil {
				why = "the nudge's quote failed: " + qerr.Error()
			}
		}
		if why != "" {
			q, opts := prop.Question, chiefnudge.CleanOptions(prop.Options)
			if strings.TrimSpace(q) == "" {
				// 🔴 NO SESSION TEXT: the generic ask (jev.go), never an excerpt
				// of the session's last message under the chief's label.
				q, opts = chiefnudge.GenericAskQuestion(c.Title), append([]string(nil), chiefnudge.GenericAskOptions...)
			}
			d.Decision, d.Question, d.Options = chiefnudge.DecisionAsk, q, opts
			d.Reason = fmt.Sprintf("classifier: %s, ask score %.2f — the default is an ask (%s); %s",
				jev.Label, jev.AskScore, why, prop.Reason)
			return s.actAsk(octx, c, d), false
		}
	}
	switch prop.Decision {
	case chiefnudge.DecisionLeave:
		d.Outcome = chiefnudge.OutcomeLeft
		return s.recordNudge(octx, d, "chief_nudge.left"), false
	case chiefnudge.DecisionAsk:
		d.Question, d.Options = prop.Question, chiefnudge.CleanOptions(prop.Options)
		return s.actAsk(octx, c, d), false
	}
	// A nudge.
	if read.Partial {
		// The read cannot vouch for the owner's last six hours (it ended
		// early, has a gap, or a cap cut the window): its owner messages
		// authorise nothing, so the nudge is an ask, and the row says why.
		d.Decision = chiefnudge.DecisionAsk
		d.Question, d.Options = chiefnudge.EscalationPartial(c.Title), chiefnudge.CleanOptions(nil)
		d.Reason = chiefnudge.GuardPartial + ": the transcript read was incomplete, so no owner message can authorise a nudge; " + d.Reason
		return s.actAsk(octx, c, d), false
	}
	d.QuoteID, d.QuoteText = prop.QuoteID, chiefnudge.Cut(prop.Quote, chiefnudge.MaxQuoteRunes)
	quoted, qerr := chiefnudge.CheckQuote(prop, owner)
	if qerr != nil && untimed > 0 && len(owner) == 0 {
		// The host sent the owner's messages with no times (an older
		// muster-mirror): none can authorise a nudge, so it is an ask — and
		// the decision says why.
		d.Decision = chiefnudge.DecisionAsk
		d.Question, d.Options = chiefnudge.EscalationNoTimes(c.Title), chiefnudge.CleanOptions(nil)
		d.Detail = strconv.Itoa(untimed) + " message(s) of the owner's arrived with no time (an older muster-mirror): none can authorise a nudge"
		return s.actAsk(octx, c, d), false
	}
	if qerr != nil {
		d.Outcome, d.Detail = chiefnudge.Suppressed(chiefnudge.GuardQuote), qerr.Error()
		return s.recordNudge(octx, d, "chief_nudge.suppressed"), false
	}
	d.QuoteAt = quoted.At
	n, err := s.ext.ChiefNudges.NudgesForAnchor(octx, c.Kind, c.Key, d.AnchorKey)
	if err != nil {
		s.logger.Printf("chief nudges: %s %s: nudge count: %v", c.Kind, c.Key, err)
		return false, false
	}
	if n >= chiefnudge.MaxNudgesPerAnchor {
		// 🔴 THE THIRD NUDGE IS AN ASK: the session has been told twice on the
		// strength of the same owner message and stopped again.
		d.Decision = chiefnudge.DecisionAsk
		d.Question, d.Options = chiefnudge.EscalationQuestion(c.Title), chiefnudge.CleanOptions(nil)
		d.Reason = "escalated: " + strconv.Itoa(n) + " nudges since the owner's last message; " + d.Reason
		return s.actAsk(octx, c, d), false
	}
	// The hourly cap bounds what is SENT. Shadow mode records what it would do,
	// unsuppressed (operator decision), and is not counted.
	if d.Mode == chiefnudge.ModeLive && p.hourly >= s.chiefNudgeHourlyCap() {
		d.Outcome, d.Detail = chiefnudge.Suppressed(chiefnudge.GuardHourlyCap),
			strconv.Itoa(p.hourly)+" nudges in the last hour (cap "+strconv.Itoa(s.chiefNudgeHourlyCap())+")"
		return s.recordNudge(octx, d, "chief_nudge.suppressed"), false
	}
	d.Message = chiefnudge.NudgeText(o.Name, quoted, prop.Quote, time.Local)
	if d.Mode != chiefnudge.ModeLive {
		d.Outcome = chiefnudge.OutcomeWouldSend
		return s.recordNudge(octx, d, "chief_nudge.would_send"), false
	}
	if s.actNudge(octx, c, d) {
		p.hourly++
		return true, false
	}
	return false, false
}

// nudgeInjectedPrefixes are the openings of muster's own pastes, from the
// constants their senders write them with (one rule, one place).
var nudgeInjectedPrefixes = []string{RoutedPrefacePrefix, chiefnudge.NudgeLabel}

// injectedBodyReader is the send queue's read of what muster itself queued
// (sendqueue.PGStore.InjectedBodies); a queue without it adds nothing.
type injectedBodyReader interface {
	InjectedBodies(ctx context.Context, since time.Time, limit int) ([]string, error)
}

// nudgeInjected is every text muster itself put into c's conversation that it
// still has a record of: the routed messages, nudges and relays it queued for
// the owner's windows (send_queue, the last 7 days — a row's lifetime), and
// this feature's own nudges and relays for c (chief_nudges). 🔴 STRUCTURAL,
// NOT SPELLED: a paste is matched by the row that carried it, so a reworded
// template cannot slip one into the owner's messages.
func (s *Server) nudgeInjected(ctx context.Context, c chiefnudge.Session) ([]string, error) {
	out, err := s.ext.ChiefNudges.Injected(ctx, c.Kind, c.Key)
	if err != nil {
		return nil, err
	}
	if q, ok := s.ext.SendQueue.(injectedBodyReader); ok && c.Kind == chiefnudge.KindMirror {
		bodies, err := q.InjectedBodies(ctx, s.now().Add(-7*24*time.Hour), 1000)
		if err != nil {
			return nil, err
		}
		out = append(out, bodies...)
	}
	return out, nil
}

// nudgeRead is one candidate's transcript as the sweep read it.
type nudgeRead struct {
	Msgs []chiefnudge.Msg
	// Full: the read holds the WHOLE conversation (its first message is the
	// session's first), which is what lets OwnerMessages leave out a
	// dispatched agent's kickoff.
	Full bool
	// Partial: the read cannot vouch for the owner's last six hours — it ended
	// before the host finished, it has a gap the prompt index shows, or the
	// window was cut by a cap before reaching six hours back. Its owner
	// messages then authorise NO nudge (a nudge becomes an ask, partial_read).
	Partial bool
}

// nudgeTranscript is c's conversation, oldest first (nudgeRead). The test
// hook nudgeReadHook replaces it.
func (s *Server) nudgeTranscript(ctx context.Context, c nudgeCandidate) (nudgeRead, error) {
	if s.nudgeReadHook != nil {
		return s.nudgeReadHook(ctx, c.Session)
	}
	now := s.now()
	if c.Kind == chiefnudge.KindAgent {
		msgs, err := s.ext.Agents.ListRecentChatMessages(ctx, c.ChatSessionID, chiefNudgeAgentMessages)
		if err != nil {
			return nudgeRead{}, err
		}
		var out []chiefnudge.Msg
		for _, m := range msgs {
			if m.Kind != "" && m.Kind != "text" {
				continue
			}
			out = append(out, chiefnudge.Msg{Role: m.Role, Text: m.Content, ID: strconv.FormatInt(m.ID, 10), At: m.CreatedAt})
		}
		full := len(msgs) < chiefNudgeAgentMessages
		// Capped at chiefNudgeAgentMessages before reaching six hours back:
		// older owner messages of the window were not read.
		partial := !full && len(msgs) > 0 && msgs[0].CreatedAt.After(now.Add(-chiefnudge.OwnerWindow))
		return nudgeRead{Msgs: out, Full: full, Partial: partial}, nil
	}
	e := mirror.SessionView{Host: c.Host, Session: c.TmuxSession, Window: c.Window, ClaudeSession: c.Key}
	evs, idx, err := s.mirrorRead(ctx, e, c.Key)
	if err != nil && (len(evs) == 0 || errors.Is(err, errNoSession)) {
		return nudgeRead{}, err
	}
	return nudgeRead{Msgs: nudgeMirrorMessages(evs, now), Partial: !nudgeMirrorComplete(evs, idx, now)}, nil
}

// nudgeFutureSkew is how far ahead of muster's clock a prompt's time may be
// and still be believed (host clocks drift); further ahead, it is untimed.
const nudgeFutureSkew = 5 * time.Minute

// nudgeMirrorMessages is a mirrored backfill's events as transcript messages,
// oldest first (the host's backfill is chronological: cctranscript.Backfill
// reads the window backwards and reverses what it keeps). A prompt is named by
// its place ("line:<offset>") and timed by its own record (PromptAt, the host's
// clock) — a time more than nudgeFutureSkew ahead of now is not believed, and
// the message is untimed (it authorises nothing).
func nudgeMirrorMessages(evs []cctranscript.Event, now time.Time) []chiefnudge.Msg {
	var out []chiefnudge.Msg
	for _, ev := range evs {
		switch ev.Kind {
		case cctranscript.KindPrompt:
			id := ""
			if ev.Line > 0 {
				id = "line:" + strconv.FormatInt(ev.Line, 10)
			}
			var at time.Time
			if ev.PromptAt > 0 {
				if t := time.UnixMilli(ev.PromptAt); !t.After(now.Add(nudgeFutureSkew)) {
					at = t
				}
			}
			out = append(out, chiefnudge.Msg{Role: "user", Text: ev.Text, ID: id, At: at})
		case cctranscript.KindText:
			out = append(out, chiefnudge.Msg{Role: "assistant", Text: ev.Text})
		}
	}
	return out
}

// nudgeMirrorComplete reports whether a mirrored read can vouch for the
// owner's last six hours:
//
//   - the host's prompt index arrived (the backfill was sent in full; a read
//     that ended on the quiet timer or the deadline is not complete);
//   - the backfill's prompts are in transcript order (their places ascend);
//   - every prompt the index lists from the backfill's oldest prompt on is in
//     the backfill (no gap), and so is the newest one the index lists;
//   - when the index lists prompts OLDER than the backfill (a cap cut the
//     window), the backfill's oldest prompt is itself older than six hours —
//     so nothing inside the window was cut.
func nudgeMirrorComplete(evs []cctranscript.Event, idx mirrorIndex, now time.Time) bool {
	if !idx.Got {
		return false
	}
	have := map[int64]bool{}
	var lines []int64
	var oldestAt int64
	for _, ev := range evs {
		if ev.Kind != cctranscript.KindPrompt || ev.Line <= 0 {
			continue
		}
		if n := len(lines); n > 0 && ev.Line <= lines[n-1] {
			return false // out of order: ordering cannot be confirmed
		}
		if len(lines) == 0 {
			oldestAt = ev.PromptAt
		}
		lines = append(lines, ev.Line)
		have[ev.Line] = true
	}
	if len(idx.Lines) == 0 {
		return len(lines) == 0 // the index lists none: nothing can be missing
	}
	if len(lines) == 0 {
		return false
	}
	cut := idx.More > 0
	newest := idx.Lines[0]
	for _, l := range idx.Lines {
		newest = max(newest, l)
		switch {
		case l < lines[0]:
			cut = true
		case !have[l]:
			return false
		}
	}
	if !have[newest] {
		return false
	}
	if cut && (oldestAt <= 0 || time.UnixMilli(oldestAt).After(now.Add(-chiefnudge.OwnerWindow))) {
		return false
	}
	return true
}

// nudgeSessionName is the chief's gateway key for ONE stall check: derived
// from the decision it is for (owner, session, idle streak), so every check
// starts a fresh context — no check sees another's, none grows with the ones
// before it — and never an operator thread's key, so a check never enters the
// operator's conversation with the chief. No chat_sessions row exists for it
// (chief_agent.go says why).
func nudgeSessionName(agentName string, owner int64, c chiefnudge.Session) string {
	sum := sha256.Sum256([]byte(strconv.FormatInt(owner, 10) + "\x00" + c.Kind + "\x00" + c.Key + "\x00" + c.StreakKey))
	return "agent:" + agentName + ":webchat:nudge:" + hex.EncodeToString(sum[:8])
}

// askChiefForNudge puts prompt to the chief and returns its answer, holding
// the chief's session claim and turn gate (as a question turn does) so it never
// overlaps another of the chief's turns. errNudgeChiefBusy when the chief
// cannot take it now. The test hook nudgeChief replaces the call itself.
func (s *Server) askChiefForNudge(ctx context.Context, chief agents.Agent, key, prompt string) (string, error) {
	if s.ext.Gateway == nil || s.ext.Agents == nil {
		return "", errNudgeChiefBusy
	}
	if !agentCanBeSent(s.liveStatus(ctx, chief)) || agents.KickoffOwed(chief) {
		return "", errNudgeChiefBusy
	}
	by := "chief-nudge/" + strconv.FormatInt(chief.ID, 10)
	won, err := s.ext.Agents.ClaimKickoff(ctx, chief.ID, by, chiefNudgeTurnBudget+time.Minute)
	if err != nil || !won {
		return "", errNudgeChiefBusy
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if err := s.ext.Agents.ReleaseKickoffClaim(rctx, chief.ID, by); err != nil {
			s.logger.Printf("chief nudges: release the chief's claim: %v", err)
		}
	}()
	if ok, _ := s.turns.enterIdle(chief.ID); !ok {
		return "", errNudgeChiefBusy
	}
	defer s.turns.leave(chief.ID)
	tctx, cancel := context.WithTimeout(ctx, chiefNudgeTurnBudget)
	defer cancel()
	reply, err := s.ext.Gateway.Chat(tctx, chief, key, prompt, nil)
	if agentgateway.Unsent(err) != "" {
		return "", errNudgeChiefBusy
	}
	return reply, err
}

// recordNudge inserts d with its audit row; false when it was not recorded
// (another pass decided this streak, or the write failed — logged).
func (s *Server) recordNudge(ctx context.Context, d chiefnudge.Decision, action string) bool {
	_, ok, err := s.ext.ChiefNudges.Insert(ctx, d, action)
	if err != nil {
		s.logger.Printf("chief nudges: %s %s: record: %v", d.Kind, d.Key, err)
		return false
	}
	return ok
}

// stillStalled reports whether c is still in the idle streak the decision was
// taken on: the last thing muster checks before it sends or asks anything.
func (s *Server) stillStalled(ctx context.Context, d chiefnudge.Decision) bool {
	return s.nudgeStreakNow(ctx, d) == d.StreakKey
}

// nudgeStreakNow is d's session's idle streak now, "" when it is not idle (or
// cannot be read).
func (s *Server) nudgeStreakNow(ctx context.Context, d chiefnudge.Decision) string {
	if d.Kind == chiefnudge.KindMirror {
		if s.ext.Mirror == nil {
			return ""
		}
		st := s.ext.Mirror.AgentState(ctx, d.Host, d.TmuxSession, d.Window)
		if st.Unknown != nil || st.Status != mirror.StatusIdle || st.Session != d.Key || st.Since <= 0 {
			return ""
		}
		return "since:" + strconv.FormatInt(st.Since, 10)
	}
	if s.ext.Agents == nil || s.turns.busy(d.AgentID) || d.ChatSessionID == 0 {
		return ""
	}
	last, err := s.ext.Agents.ListRecentChatMessages(ctx, d.ChatSessionID, 1)
	if err != nil || len(last) == 0 {
		return ""
	}
	return "msg:" + strconv.FormatInt(last[len(last)-1].ID, 10)
}

// actNudge sends a live nudge: queued for a mirrored window (sendQueueNudgeGate
// re-checks it at the paste), a turn for an agent. The decision is recorded
// FIRST — the streak's claim — so two passes never send it twice.
func (s *Server) actNudge(ctx context.Context, c nudgeCandidate, d chiefnudge.Decision) bool {
	if !s.stillStalled(ctx, d) {
		d.Outcome, d.Detail = chiefnudge.Suppressed(chiefnudge.GuardMovedOn), "the session left its idle streak before muster acted"
		return s.recordNudge(ctx, d, "chief_nudge.suppressed")
	}
	d.Outcome = chiefnudge.OutcomeQueued
	rec, ok, err := s.ext.ChiefNudges.Insert(ctx, d, "")
	if err != nil || !ok {
		if err != nil {
			s.logger.Printf("chief nudges: %s %s: record: %v", d.Kind, d.Key, err)
		}
		return false
	}
	if c.Kind == chiefnudge.KindAgent {
		s.sendNudgeTurn(ctx, rec, rec.Message, chiefnudge.OutcomeQueued, chiefnudge.OutcomeSent, "chief_nudge.sent")
		return true
	}
	s.queueNudgeText(ctx, rec, rec.Message, chiefnudge.OutcomeQueued)
	return true
}

// queueNudgeText queues text for d's mirrored window as a chief-nudge row and
// records the row on d (still in state); a failure moves d to failed.
func (s *Server) queueNudgeText(ctx context.Context, d chiefnudge.Decision, text, state string) {
	if s.ext.SendQueue == nil || s.ext.Mirror == nil {
		s.failNudge(ctx, d, state, "the send queue is off on this server")
		return
	}
	target := sendqueue.Target{Host: d.Host, Session: d.TmuxSession, Window: d.Window}
	st := s.ext.Mirror.AgentState(ctx, d.Host, d.TmuxSession, d.Window)
	it, err := s.ext.SendQueue.Enqueue(ctx, sendqueue.Item{Target: target, Body: text, StepUpAt: s.sendQueueNow(),
		ChiefNudgeID: d.ID, AfterSince: st.Pasted})
	if err != nil {
		s.failNudge(ctx, d, state, "it could not be queued: "+err.Error())
		return
	}
	if _, err := s.ext.ChiefNudges.Move(ctx, d.ID, []string{state}, chiefnudge.Move{Outcome: state, SendQueueID: it.ID, RelayText: d.RelayText}, ""); err != nil {
		s.logger.Printf("chief nudges: decision %d: record queued message %d: %v", d.ID, it.ID, err)
	}
	s.kickSendQueue()
}

// sendNudgeTurn starts text as a turn to d's agent — never overlapping one of
// its turns — and moves d from state to done (with action's audit row), or to
// suppressed:moved_on when the agent is busy or gone.
func (s *Server) sendNudgeTurn(ctx context.Context, d chiefnudge.Decision, text, state, done, action string) {
	// 🔴 THE KILL SWITCH REACHES AN AGENT'S NUDGE AND RELAY TOO, read at the
	// moment of sending (the send queue's gate does it for a window).
	if !s.chiefNudgeLive(d.Owner) {
		s.failNudge(ctx, d, state, SendQueueNudgeOff)
		return
	}
	if s.ext.Gateway == nil {
		s.failNudge(ctx, d, state, "the agent gateway is off on this server")
		return
	}
	a, err := s.ext.Agents.Get(ctx, d.AgentID)
	if err != nil {
		s.failNudge(ctx, d, state, "the agent is gone")
		return
	}
	// The conversation the decision was about — not whichever is newest.
	sess, err := s.ext.Agents.GetSession(ctx, d.ChatSessionID)
	if err != nil || sess.AgentID != a.ID {
		s.failNudge(ctx, d, state, "the agent's conversation is gone")
		return
	}
	if !agentCanBeSent(s.liveStatus(ctx, a)) {
		s.failNudge(ctx, d, state, "the agent is not running")
		return
	}
	if !s.stillStalled(ctx, d) {
		if done == chiefnudge.OutcomeRelayed && s.turns.busy(a.ID) {
			return // busy for now: the next pass retries it, up to RelayMaxAge
		}
		why := "the conversation moved on before muster sent it"
		if _, err := s.ext.ChiefNudges.Move(ctx, d.ID, []string{state}, chiefnudge.Move{Outcome: chiefnudge.Suppressed(chiefnudge.GuardMovedOn),
			Detail: why}, "chief_nudge.suppressed"); err != nil {
			s.logger.Printf("chief nudges: decision %d: %v", d.ID, err)
		}
		return
	}
	if ok, _ := s.turns.enterIdle(a.ID); !ok {
		return // a turn started this instant: the next pass retries (a nudge is then moved on)
	}
	if _, err := s.ext.ChiefNudges.Move(ctx, d.ID, []string{state}, chiefnudge.Move{Outcome: done}, action); err != nil {
		s.turns.leave(a.ID)
		s.logger.Printf("chief nudges: decision %d: %v", d.ID, err)
		return
	}
	s.startAgentTurn(ctx, a, sess, text)
}

// failNudge moves d from state to failed, saying why.
func (s *Server) failNudge(ctx context.Context, d chiefnudge.Decision, state, why string) {
	if _, err := s.ext.ChiefNudges.Move(ctx, d.ID, []string{state}, chiefnudge.Move{Outcome: chiefnudge.OutcomeFailed, Detail: why}, "chief_nudge.failed"); err != nil {
		s.logger.Printf("chief nudges: decision %d: fail: %v", d.ID, err)
	}
}

// actAsk is an ask decision: shadow records it; live raises an attention ask on
// the owner's queue (muster's own asker, labelled as the chief), whose answer
// relayNudgeAnswer relays.
func (s *Server) actAsk(ctx context.Context, c nudgeCandidate, d chiefnudge.Decision) bool {
	if strings.TrimSpace(d.Question) == "" {
		d.Outcome = chiefnudge.Suppressed(chiefnudge.GuardNoQuestion)
		return s.recordNudge(ctx, d, "chief_nudge.suppressed")
	}
	if d.Mode != chiefnudge.ModeLive {
		d.Outcome = chiefnudge.OutcomeWouldAsk
		return s.recordNudge(ctx, d, "chief_nudge.would_ask")
	}
	if s.ext.Attention == nil {
		d.Outcome, d.Detail = chiefnudge.Suppressed(chiefnudge.GuardAskFailed), "the attention queue is off"
		return s.recordNudge(ctx, d, "chief_nudge.suppressed")
	}
	if !s.stillStalled(ctx, d) {
		d.Outcome, d.Detail = chiefnudge.Suppressed(chiefnudge.GuardMovedOn), "the session left its idle streak before muster acted"
		return s.recordNudge(ctx, d, "chief_nudge.suppressed")
	}
	ask, err := s.ext.Attention.Create(ctx, attention.New{
		Asker:    attention.Asker{System: true, Label: chiefNudgeAskLabel},
		Question: nudgeAskQuestion(c.Session, d.Question, chiefnudge.IsGenericAsk(d.Options)),
		Options:  d.Options,
	})
	switch {
	case errors.Is(err, attention.ErrTooMany):
		d.Outcome, d.Detail = chiefnudge.Suppressed(chiefnudge.GuardAskCap), err.Error()
		return s.recordNudge(ctx, d, "chief_nudge.suppressed")
	case err != nil:
		d.Outcome, d.Detail = chiefnudge.Suppressed(chiefnudge.GuardAskFailed), err.Error()
		return s.recordNudge(ctx, d, "chief_nudge.suppressed")
	}
	d.Outcome, d.AskID = chiefnudge.OutcomeAsked, ask.ID
	if !s.recordNudge(ctx, d, "chief_nudge.asked") {
		// Another pass decided this streak first: withdraw the duplicate.
		if _, err := s.cancelAsk(ctx, ask.ID); err != nil {
			s.logger.Printf("chief nudges: withdraw duplicate ask %d: %v", ask.ID, err)
		}
		return false
	}
	s.askFiled(ask)
	return true
}

// nudgeAskQuestion is the ask's text: which session (its focus title and
// where), the question the chief extracted, and what happens to the answer.
// The session's words reach the owner only through the chief's own question.
//
// generic: the generic ask (chiefnudge.GenericAskQuestion) — muster's own
// sentence, whose options send nothing.
func nudgeAskQuestion(c chiefnudge.Session, q string, generic bool) string {
	where := c.Title
	if where == "" {
		where = "A session"
	}
	if c.Kind == chiefnudge.KindMirror {
		label := c.Label
		if label == "" {
			label = c.TmuxSession
		}
		where += " (" + label + " @ " + c.Host + ")"
	} else {
		where += " (agent " + c.Label + ")"
	}
	text := where + " stopped and is waiting on you:\n\n" + strings.TrimSpace(q) +
		"\n\nYour answer is sent into that session word for word."
	if generic {
		text = where + " is waiting on you. Its last turn looks like it asked you something.\n\n" +
			"Choosing an option sends nothing into the session; an answer you type is sent into it word for word."
	}
	return fence.Cut(text, attention.MaxQuestionRunes)
}

// relayNudgeAnswer relays the owner's answer to a nudge's ask into its session,
// VERBATIM — the owner's own words and nothing else. Called on every answer
// (answerAskFrom) and by the sweep's reconcile (an answer this process did not
// see). Only an answer given on the Attention view is relayed: one given
// through a mirrored session's attention_answer (which a session fed outside
// text could write) is recorded and not relayed.
func (s *Server) relayNudgeAnswer(ctx context.Context, a attention.Ask) {
	if s.ext.ChiefNudges == nil || a.Status != attention.Answered {
		return
	}
	d, err := s.ext.ChiefNudges.ByAsk(ctx, a.ID)
	if err != nil || d.Outcome != chiefnudge.OutcomeAsked {
		return
	}
	closeAsk := func(why string) {
		if _, err := s.ext.ChiefNudges.Move(ctx, d.ID, []string{chiefnudge.OutcomeAsked},
			chiefnudge.Move{Outcome: chiefnudge.OutcomeAskClosed, Detail: why}, "chief_nudge.ask_closed"); err != nil {
			s.logger.Printf("chief nudges: decision %d: %v", d.ID, err)
		}
	}
	switch {
	case a.AnsweredVia.ViaSession():
		closeAsk("answered through a mirrored session (" + a.AnsweredVia.String() + "), not on the Attention view: not relayed")
		return
	case !s.chiefNudgeLive(d.Owner):
		closeAsk("chief nudges are off or in shadow mode for this owner now: not relayed")
		return
	case chiefnudge.IsGenericChoice(d.Options, a.Answer):
		// The generic ask's options are not an answer to forward.
		closeAsk("the owner chose \u201c" + strings.TrimSpace(a.Answer) + "\u201d: nothing to relay")
		return
	case d.Kind == chiefnudge.KindAgent && s.ext.Gateway == nil:
		// An agent's relay is a turn, and with no gateway there is none to run
		// (cmd/muster-server leaves it nil on purpose: doc_seams.go).
		closeAsk("the agent gateway is off on this server: not relayed")
		return
	}
	// 🔴 CLAIM FIRST: asked → relay_queued, with the text, in one conditional
	// update. A second caller (the sweep racing the answer) finds it moved and
	// relays nothing, so the answer reaches the session at most once.
	moved, err := s.ext.ChiefNudges.Move(ctx, d.ID, []string{chiefnudge.OutcomeAsked},
		chiefnudge.Move{Outcome: chiefnudge.OutcomeRelayQueued, RelayText: a.Answer}, "")
	if err != nil || !moved {
		return
	}
	// 🔴 THE SEND IS THE SWEEP'S, NOT THIS REQUEST'S: the claim above is all an
	// answer does here; the sweep's reconcile (woken now) queues the text or
	// starts the agent's turn, on its own goroutine — so no answer route ever
	// reaches the agent gateway (provisioner_seam_test.go holds that).
	s.kickNudges()
}

// deliverRelay sends d's relay text: queued for a mirrored window, a turn for
// an agent.
//
// The kill switch is read where each send happens — sendNudgeTurn for an
// agent, sendQueueNudgeGate at the paste for a window — never only here.
func (s *Server) deliverRelay(ctx context.Context, d chiefnudge.Decision) {
	if d.Kind == chiefnudge.KindAgent {
		s.sendNudgeTurn(ctx, d, d.RelayText, chiefnudge.OutcomeRelayQueued, chiefnudge.OutcomeRelayed, "chief_nudge.relayed")
		return
	}
	s.queueNudgeText(ctx, d, d.RelayText, chiefnudge.OutcomeRelayQueued)
}

// reconcileNudge settles one live decision in flight, as its owner: a queued
// row that was sent (or failed), an ask that was answered (relay), closed or
// outlived its session's stall. done reports that it is no longer in flight.
func (s *Server) reconcileNudge(ctx context.Context, d chiefnudge.Decision) (done bool) {
	octx := s.attentionPrincipal(ctx, d.Owner)
	switch d.Outcome {
	case chiefnudge.OutcomeQueued, chiefnudge.OutcomeRelayQueued:
		if d.SendQueueID == 0 {
			age := s.now().Sub(d.UpdatedAt)
			switch {
			case d.Outcome == chiefnudge.OutcomeRelayQueued && age > chiefnudge.RelayMaxAge:
				s.failNudge(octx, d, d.Outcome, "the relay was not delivered within "+chiefnudge.RelayMaxAge.String()+"; dropped")
				return true
			case d.Outcome == chiefnudge.OutcomeRelayQueued:
				// Claimed by the answer, not yet sent (or the agent was busy):
				// send it now.
				s.deliverRelay(octx, d)
			case d.Kind == chiefnudge.KindAgent:
				// An agent's nudge whose turn could not start (a turn began
				// that instant): tried again, which records it moved on if
				// the conversation has.
				s.sendNudgeTurn(octx, d, d.Message, d.Outcome, chiefnudge.OutcomeSent, "chief_nudge.sent")
			case d.Kind == chiefnudge.KindMirror && age > chiefnudge.QueueRepairAfter:
				// Recorded queued, but its send_queue row was never written (a
				// stop between the two): nothing will ever send it.
				s.failNudge(octx, d, d.Outcome, "its message was never queued (interrupted)")
				return true
			}
			return false
		}
		if s.ext.SendQueue == nil {
			return false
		}
		it, err := s.ext.SendQueue.Get(octx, d.SendQueueID)
		if err != nil {
			return false
		}
		sent, action := chiefnudge.OutcomeSent, "chief_nudge.sent"
		if d.Outcome == chiefnudge.OutcomeRelayQueued {
			sent, action = chiefnudge.OutcomeRelayed, "chief_nudge.relayed"
		}
		switch it.State {
		case sendqueue.Sent:
			_, _ = s.ext.ChiefNudges.Move(octx, d.ID, []string{d.Outcome}, chiefnudge.Move{Outcome: sent, Detail: it.Detail}, action)
			return true
		case sendqueue.Failed, sendqueue.Cancelled:
			if it.Detail == SendQueueRelayStale || it.Detail == SendQueueNudgeStale {
				// Superseded by a newer turn: not a failure to retry.
				_, _ = s.ext.ChiefNudges.Move(octx, d.ID, []string{d.Outcome}, chiefnudge.Move{
					Outcome: chiefnudge.Suppressed(chiefnudge.GuardSuperseded), Detail: it.Detail}, "chief_nudge.suppressed")
				return true
			}
			s.failNudge(octx, d, d.Outcome, it.Detail)
			return true
		}
		return false
	case chiefnudge.OutcomeAsked:
		if s.ext.Attention == nil {
			return false
		}
		a, err := s.ext.Attention.Get(octx, d.AskID)
		if err != nil {
			return false
		}
		switch a.Status {
		case attention.Open:
			if !s.stillStalled(octx, d) {
				// The owner (or anything) moved the session on: the question is
				// stale. Withdraw it rather than relay an answer later.
				if _, err := s.cancelAsk(octx, a.ID); err == nil {
					_, _ = s.ext.ChiefNudges.Move(octx, d.ID, []string{chiefnudge.OutcomeAsked},
						chiefnudge.Move{Outcome: chiefnudge.OutcomeAskClosed, Detail: "the session moved on; the ask was withdrawn"}, "chief_nudge.ask_closed")
					return true
				}
			}
			return false
		case attention.Answered:
			s.relayNudgeAnswer(octx, a)
			return true
		}
		_, _ = s.ext.ChiefNudges.Move(octx, d.ID, []string{chiefnudge.OutcomeAsked},
			chiefnudge.Move{Outcome: chiefnudge.OutcomeAskClosed, Detail: "the ask ended " + string(a.Status) + "; nothing to relay"}, "chief_nudge.ask_closed")
		return true
	}
	return true
}

// Why a chief-nudge queued row is not pasted (sendQueueNudgeGate).
const (
	SendQueueNudgeOff     = "Not sent: chief nudges are off, or in shadow mode for you, on this server."
	SendQueueNudgeUnknown = "Not sent: its chief nudge could not be read."
	SendQueueNudgeClosed  = "Not sent: its chief nudge is no longer waiting to be sent"
	SendQueueNudgeMoved   = "Not sent: the window no longer holds the conversation the chief nudged."
	SendQueueNudgeStale   = "Not sent: the session moved on after the chief decided to nudge it."
	SendQueueRelayStale   = "Not sent: the session moved on after the ask was raised, so your answer is not pasted into its newer turn."
)

// sendQueueNudgeGate is why chief-nudge row it may not be pasted now, "" when
// it may: the feature on and the owner live (the kill switch reaches a row
// already queued), its decision still waiting on THIS row, the window still
// holding the conversation the decision was about, and — for a nudge, not a
// relay of the owner's own answer — the session still in the idle streak the
// chief judged. ctx is the row's owner.
func (s *Server) sendQueueNudgeGate(ctx context.Context, it sendqueue.Item) string {
	if !s.chiefNudgeLive(requestOwner(ctx)) {
		return SendQueueNudgeOff
	}
	d, err := s.ext.ChiefNudges.Get(ctx, it.ChiefNudgeID)
	if err != nil {
		return SendQueueNudgeUnknown
	}
	if (d.Outcome != chiefnudge.OutcomeQueued && d.Outcome != chiefnudge.OutcomeRelayQueued) ||
		(d.SendQueueID != 0 && d.SendQueueID != it.ID) {
		return SendQueueNudgeClosed + " (" + d.Outcome + ")."
	}
	holds := ""
	for _, e := range s.mirrorClaudeEntries(ctx) {
		if e.Host == it.Target.Host && e.Session == it.Target.Session && e.Window == it.Target.Window {
			holds = e.ClaudeSession
		}
	}
	if holds != d.Key {
		return SendQueueNudgeMoved
	}
	if !s.stillStalled(ctx, d) {
		// 🔴 A RELAY IS TIED TO THE STALL IT ANSWERS, like an agent's
		// (sendNudgeTurn): an owner who typed into the window after answering
		// started a newer turn, and the answer is not pasted into it.
		if d.Outcome == chiefnudge.OutcomeRelayQueued {
			return SendQueueRelayStale
		}
		return SendQueueNudgeStale
	}
	return ""
}
