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
	"github.com/ZacxDev/muster/internal/provision"
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
// in a context of its own so it never enters the operator's conversation:
// nudgeSessionName, nudgeRoute) for ONE decision: nudge, ask, or leave — with
// session state on, nudge, leave or NOTIFY (actNotify: no question, no notice;
// the session's episode pushes), once per episode. Then the guards — in
// code, never in the prompt — decide what muster does with it:
//
//   - a nudge is accepted only when its quote is, byte for byte, inside one of
//     the owner's OWN messages to that session (chiefnudge.CheckQuote over
//     chiefnudge.OwnerMessages): never the session's own words, never text muster
//     injected;
//   - at most chiefnudge.MaxNudgesPerAnchor nudges per owner message, then the
//     next becomes an ask;
//   - at most ChiefNudges.HourlyCap nudges an hour across every owner;
//   - a NEEDS-YOU nudge (the classifier says the session asked) is sent only
//     for an owner on both live lists (chiefNudgeLiveFor);
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
// An ask decision raises a NOTICE on the owner's queue (attention.KindNotice,
// migration 0071): a pointer at the session — where it is and the chief's
// short message — with no options and no answer. The owner goes to the session
// (or a host's jump key, `muster nudge next`, takes them there); the notice is
// withdrawn when the session moves on. It does not count toward the open-ask
// cap.
//
// 🔴 NOTHING IS RELAYED. A notice cannot be answered (the store refuses it,
// attention.ErrNotice, and 0071's CHECK refuses it again), and the relay of an
// answer into a session is gone: 0071 withdrew every nudge ask raised before
// notices and failed every relay still queued. No path types an owner's answer
// to a nudge into a session.
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
	// NeedsYouLive is the owners whose NEEDS-YOU nudges are sent too
	// (MUSTER_CHIEF_NUDGE_NEEDS_YOU_LIVE_OWNERS): a nudge to a session the
	// classifier says asked something is live only for an owner in BOTH Live
	// and this; for everyone else it is shadow — recorded, never pasted, and
	// the stop pushed. Operator decision 2026-11-04, "Separate shadow gate".
	NeedsYouLive map[int64]bool
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
	// chiefNudgeTurnBudget bounds one chief call: the three minutes a check's
	// turn has always had, plus one for a claude-code chief's ccd to start the
	// check's own CLI first (nudgeRoute) — ccd waits up to its laneStartTimeout
	// (60s, cmd/ccd/lanes.go) for a new lane's CLI to reach its prompt, and
	// answers a typed 503 not_ready past it (unsent: the check is retried next
	// pass), so a check whose CLI did start still has at least three minutes.
	chiefNudgeTurnBudget = 4 * time.Minute
	// chiefNudgeAgentMessages is how many of an agent's newest messages are read.
	chiefNudgeAgentMessages = 200
	// chiefNudgePendingBatch bounds one reconcile.
	chiefNudgePendingBatch = 200
	// chiefNudgeAskLabel names the chief on a nudge's ask raised before
	// notices, and on a notice whose label is somehow blank (noticeItem).
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

// 🔴 THE NEEDS-YOU NUDGE'S OWN GATE, ONE PLACE (operator decision 2026-11-04,
// "Separate shadow gate"). A needs-you nudge — to an idle session the stall
// classifier says asked something (needsYouStall) — answers the session's
// question on the owner's behalf, so it is sent only for an owner listed in
// BOTH MUSTER_CHIEF_NUDGE_LIVE_OWNERS and
// MUSTER_CHIEF_NUDGE_NEEDS_YOU_LIVE_OWNERS. Every other nudge keeps the one
// list it always had. The decision's mode (nudgeModeFor), the send queue's
// paste gate and an agent's turn (nudgeSendLive) all read this. Session-state
// step 3a only: with session state off there is no needs-you nudge and no
// such gate (decideNudge never passes needsYou true there, and nudgeSendLive
// applies it only to a decision taken in an episode).
func (s *Server) chiefNudgeLiveFor(owner int64, needsYou bool) bool {
	if !s.chiefNudgeLive(owner) {
		return false
	}
	return !needsYou || s.ext.ChiefNudge.NeedsYouLive[owner]
}

// nudgeModeFor is the mode of a nudge to owner's session (chiefNudgeLiveFor).
func (s *Server) nudgeModeFor(owner int64, needsYou bool) string {
	if s.chiefNudgeLiveFor(owner, needsYou) {
		return chiefnudge.ModeLive
	}
	return chiefnudge.ModeShadow
}

// needsYouStall is the stall rule's split (chiefnudge.AskStall at the
// configured threshold): an idle session whose last message the classifier
// says asked the owner something. ONE predicate for session state's needs_you
// (f.AskStall), the needs-you nudge's gate, and the backtested rule in
// decideNudge. The leave rule reads sessionstate.Asked(classified, errClass,
// askStall), of which this is the askStall input (a failed or absent
// classifier counts as asked there, not here).
func (s *Server) needsYouStall(jev chiefnudge.Result) bool {
	return chiefnudge.AskStall(jev, s.chiefNudgeAskThreshold())
}

// nudgeSendLive reports whether decision d's nudge may reach its session NOW:
// re-read at the paste (and at an agent's turn), so taking an owner off either
// list stops a nudge already queued. d is a needs-you nudge when the
// classification stored on it asked AND it is under session state: taken in
// an episode, or sent while session state is on. The second covers a nudge
// the image before step 3a queued (no episode) and that is still queued after
// the deploy: with session state on it is held to the needs-you rule like any
// other. With session state off neither holds, so there is no such gate.
func (s *Server) nudgeSendLive(owner int64, d chiefnudge.Decision) bool {
	return s.chiefNudgeLiveFor(owner, (d.EpisodeID != 0 || s.sessionStateOn()) && s.needsYouStall(d.Jev()))
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
		case <-t.C:
		}
		if _, err := s.ChiefNudgePass(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Printf("chief nudges: %v", err)
		}
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
	// noTranscript: a window muster reads no transcript of (a Hermes window,
	// listed for session state only): never read, never classified.
	noTranscript bool
	// ownerLast: an agent conversation's newest message is the owner's — a
	// turn under way that no turn gate saw (session state keeps the stop's
	// anchor where it was).
	ownerLast bool
	// episodeID: with session state on, the session's open episode, which the
	// decision is taken in (nudgeEpisodeFor); 0 with it off.
	episodeID int64
}

// sweepOwnerNudges decides octx's owner's candidates, while the pass has
// budget. An owner with no chief (or its chief off) has nothing decided: the
// nudges are the chief's.
func (s *Server) sweepOwnerNudges(octx context.Context, o chiefnudge.Owner, inFlight map[string]bool, p *nudgePass) {
	chief, err := s.chiefAgent(octx)
	if err != nil {
		return
	}
	cands := s.nudgeCandidates(octx, inFlight)
	for i, c := range cands {
		if p.budget <= 0 || octx.Err() != nil {
			return
		}
		decided, stop, refused := s.decideNudge(octx, o, chief, c, p)
		if decided {
			p.decided++
		}
		if refused != "" {
			// 🔴 THE CANDIDATES WAITING BEHIND A REFUSED CHECK START THEIR CLOCKS
			// TOO (chief_nudge_retry.go): the chief's lanes are full for them as
			// much as for this one. Without it each would start its own bound
			// only once the one before it was recorded — one bound per
			// candidate, in a line. Nothing is called for them.
			now := s.now()
			for _, w := range cands[i+1:] {
				s.nudgeRetry.refused(nudgeRetryKey(o.ID, w.Session), false, now)
			}
		}
		if stop {
			return
		}
	}
}

// nudgeCandidates is octx's owner's sessions that pass chiefnudge.Exclude and
// have no decision recorded for their current streak: stallInputs, filtered.
// inFlight names the sessions with a live decision still in flight (an open
// notice, a queued nudge).
func (s *Server) nudgeCandidates(octx context.Context, inFlight map[string]bool) []nudgeCandidate {
	idle := s.chiefNudgeIdle()
	var out []nudgeCandidate
	for _, c := range s.stallInputs(octx, inFlight) {
		if chiefnudge.Exclude(c.Session, idle) != "" {
			continue
		}
		decided, err := s.ext.ChiefNudges.Decided(octx, c.Kind, c.Key, c.StreakKey)
		if err != nil || decided {
			continue
		}
		if s.sessionStateOn() {
			id, ok := s.nudgeEpisodeFor(octx, c.Session)
			if !ok {
				continue
			}
			c.episodeID = id
		}
		c.Title = s.nudgeTitle(octx, c.Session)
		out = append(out, c)
	}
	return out
}

// nudgeEpisodeFor is, with session state on, the open episode c's decision
// would be taken in: ok false when there is none (the state pass opens it
// within its interval; the next sweep judges it), when it cannot be read, or
// when the chief has already judged it.
//
// 🔴 PER IDLE EPISODE, AT MOST ONCE (docs/design/session-state.md, "The Chief:
// nudge, leave or notify"). An episode is one stop, which can hold several
// idle streaks (a busy blip shorter than sessionstate.EndAfter keeps the
// stop): only its first judged streak reaches the chief. A later streak of the
// same stop is pushed by its episode (no decision for its streak, so
// episodeVerdict pushes past episodeChiefWait), never judged again. The
// partial unique index chief_nudges_one_per_episode (0078) holds the same line
// across passes and replicas.
func (s *Server) nudgeEpisodeFor(octx context.Context, c chiefnudge.Session) (int64, bool) {
	ep, ok, err := s.ext.Episodes.OpenFor(octx, c.Kind, c.Key)
	if err != nil || !ok {
		return 0, false
	}
	if _, decided, err := s.ext.ChiefNudges.ForEpisode(octx, ep.ID); err != nil || decided {
		return 0, false
	}
	return ep.ID, true
}

// 🔴 THE STALL FACTS, ONE PLACE. stallInputs is octx's owner's sessions with
// the facts the stall rule (chiefnudge.Exclude) reads — Status,
// IdleFor/IdleSince and StreakKey, Shell (background work), TurnError,
// OpenAsk, and the Chief/Officer/OtherUser marks — NOT yet filtered by that
// rule, in the sweep's order: mirrored Claude Code and opencode windows
// (nudgeWindowKey), then dispatched agents (one entry per recent conversation
// of an agent that could be sent to; one status-less entry for any other
// agent).
//
// It is NOT one entry per subject. Already left out, with no entry at all:
// every window of a mirror host that is offline or reports no status; a window
// nudgeWindowKey does not judge (neither Claude Code with a Claude session nor
// opencode naming its session), or that repeats a conversation an earlier
// window already listed; for an agent that could be
// sent to, a conversation older than chiefNudgeAgentSessionAge (and every one
// after it), a conversation with no message or whose messages cannot be read,
// and all its conversations when ListSessions fails; and every agent when the
// agent list itself cannot be read (logged, not returned). A caller that needs every
// subject must not expect an entry for each here. Anything that
// classifies a session as stalled reads these, never its own copy, so the
// nudge sweep and any report agree on who is stalled. inFlight (may be nil)
// names sessions with a live nudge decision in flight; they read as OpenAsk.
func (s *Server) stallInputs(octx context.Context, inFlight map[string]bool) []nudgeCandidate {
	var hs []mirror.HostView
	if s.ext.Mirror != nil {
		hs = s.ext.Mirror.Hosts(octx)
	}
	return s.stallFacts(octx, inFlight, hs, false).sessions
}

// stallRead is one read of the stall facts (stallFacts).
type stallRead struct {
	sessions []nudgeCandidate
	// agentsRead: the owner's agent list was read. False, the read failed (or
	// there is no agent store): no agent is listed, and an absence proves
	// nothing about any agent.
	agentsRead bool
}

// stallFacts is stallInputs over hs, ONE read of the owner's hosts that a
// caller judging absences (session state's "gone") also reads, so a host
// cannot be online in one read and offline in the other. hermes also lists
// HERMES windows naming their session (runtime-qualified, as opencode): the
// stall nudges never judge them (nudgeWindowKey), session state gives them
// episodes (docs/design/session-state.md, default D3).
func (s *Server) stallFacts(octx context.Context, inFlight map[string]bool, hs []mirror.HostView, hermes bool) stallRead {
	owner := requestOwner(octx)
	now := s.now()
	asked := s.nudgeOpenAskers(octx)
	var all []nudgeCandidate
	read := stallRead{}
	if s.ext.Mirror != nil {
		seen := map[string]bool{}
		for _, h := range hs {
			if !h.Online || !h.StatusReport {
				continue
			}
			for _, w := range h.Windows {
				key, runtime, noTranscript := stateWindowKey(w, hermes)
				if key == "" || seen[key] {
					continue
				}
				seen[key] = true
				st := s.ext.Mirror.AgentState(octx, h.Host, w.Session, w.ID)
				c := chiefnudge.Session{Kind: chiefnudge.KindMirror, Key: key, Runtime: runtime, Host: h.Host,
					TmuxSession: w.Session, Window: w.ID, Index: w.Index, Label: w.Label, Status: st.Status,
					Shell:     !h.Work[w.ID].IsZero(),
					TurnError: st.TurnError != nil || st.HostLimit != nil || st.GuardHeld != "",
					OpenAsk:   asked["mirror:"+key] || inFlight[chiefnudge.KindMirror+":"+key]}
				if st.Unknown != nil {
					c.Status = ""
				}
				if c.StreakKey = windowStreakKey(st); c.StreakKey != "" {
					c.IdleSince, c.IdleFor = st.Observed, now.Sub(st.Observed)
				}
				all = append(all, nudgeCandidate{Session: c, noTranscript: noTranscript})
			}
		}
	}
	if s.ext.Agents != nil {
		list, err := s.ext.Agents.List(octx)
		if err != nil {
			s.logger.Printf("chief nudges: owner %d: agents: %v", owner, err)
		}
		read.agentsRead = err == nil
		isChief := chiefPredicate(list)
		// 🔴 ONE PROVISIONER LIST PER READ, NOT ONE PER AGENT (live_status.go:
		// liveStatus is for a single-agent path) — and none for an owner with
		// no agents.
		var idx map[string]*provision.Instance
		if len(list) > 0 {
			idx = s.instanceIndex(octx)
		}
		for _, a := range list {
			_, _, officer := s.officerOf(a)
			agentKey := "agent:" + strconv.FormatInt(a.ID, 10)
			base := chiefnudge.Session{Kind: chiefnudge.KindAgent, Key: chiefnudge.AgentKey(a.ID, 0), AgentID: a.ID,
				Label: firstNonBlank(a.DisplayName, a.Name), OtherUser: a.OwnerID != owner, Chief: isChief(a), Officer: officer,
				OpenAsk: asked[agentKey]}
			if base.OtherUser || base.Chief || base.Officer ||
				!agentCanBeSent(liveStatusIndexed(a, idx)) || agents.KickoffOwed(a) || s.turns.busy(a.ID) {
				all = append(all, nudgeCandidate{Session: base, agent: a}) // status "": not idle
				continue
			}
			sessions, err := s.ext.Agents.ListSessions(octx, a.ID)
			if err != nil {
				continue
			}
			// 🔴 EACH CONVERSATION ON ITS OWN: an agent can hold several, and
			// the one that stalled is the one decided and nudged.
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
				c.StreakKey = agentStreakKey(m)
				c.IdleSince, c.IdleFor = m.CreatedAt, now.Sub(m.CreatedAt)
				c.TurnError = m.Role == "assistant" && strings.HasPrefix(m.Content, AgentTurnFailedPrefix)
				all = append(all, nudgeCandidate{Session: c, agent: a, session: sess, ownerLast: m.Role == "user"})
			}
		}
	}
	read.sessions = all
	return read
}

// agentStreakKey is an agent conversation's streak as its newest message m
// keys it. ONE RULE: the stall facts, the nudge's streak re-read
// (nudgeStreakNow) and the owner viewing the conversation (session_viewed.go)
// all key it here, so an episode the view ends names the streak the next pass
// will see (session_state.go reads the id back from the "msg:" prefix).
func agentStreakKey(m agents.ChatMessage) string {
	return "msg:" + strconv.FormatInt(m.ID, 10)
}

// windowStreakKey is a mirrored window's streak as the host reports its state
// st — the status Claude Code wrote, by the time it wrote it — "" when the
// host reports none. ONE RULE: the stall facts and the episode push's waiting
// hook (session_push.go) both key a window's streak by it, so an episode the
// hook opens names the streak the pass will see.
func windowStreakKey(st mirror.AgentState) string {
	if st.Since > 0 && !st.Observed.IsZero() {
		return "since:" + strconv.FormatInt(st.Since, 10)
	}
	return ""
}

// nudgeWindowKey is the session key a mirrored window is judged under, and its
// runtime ("" for Claude Code), or "" when the sweep does not judge it:
//
//   - a Claude Code window with a Claude session: its Claude session id (as
//     the key always was);
//   - an OPENCODE window naming its opencode session: the runtime-qualified
//     key ("opencode:<id>", mirror.Window.SessionKey). Its status and status
//     time are on the window list like Claude Code's (the host's opencode event
//     stream, internal/mirrorhost/opencode.go), and its transcript is the
//     rendered view's backfill (nudgeTranscript) — but it is VIEW ONLY: muster
//     cannot type into it, so a nudge for it becomes a notice (decideNudge).
//
// Every other window — another runtime (Hermes), a plain shell — is not judged.
func nudgeWindowKey(w mirror.Window) (key, runtime string) {
	switch {
	case w.Claude && w.ClaudeSession != "" && w.Runtime == "":
		return w.ClaudeSession, ""
	case !w.Claude && w.AgentRuntime() == mirror.RuntimeOpenCode && w.AgentSession != "":
		// 🔴 BY NAME, ON PURPOSE (runtime_ledger_test.go lists it): the owner
		// chose opencode, not "every view-only runtime" — Hermes is not judged.
		return w.SessionKey(), w.AgentRuntime()
	}
	return "", ""
}

// stateWindowKey is the session key the stall facts list a window under
// (nudgeWindowKey), and with hermes a Hermes window naming its conversation
// too (noTranscript: muster reads none of it). "" when the window is not
// listed. ONE RULE: the episode push's waiting hook keys a window by it too
// (session_push.go), so the hook never opens an episode the pass cannot see.
func stateWindowKey(w mirror.Window, hermes bool) (key, runtime string, noTranscript bool) {
	key, runtime = nudgeWindowKey(w)
	if key == "" && hermes && !w.Claude && w.AgentRuntime() == mirror.RuntimeHermes && w.AgentSession != "" {
		return w.SessionKey(), w.AgentRuntime(), true
	}
	return key, runtime, false
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
// claimed); the owner's other candidates wait for the next pass. A refusal for
// capacity or not-ready is a nudgeRefusal, retried bounded
// (chief_nudge_retry.go).
var errNudgeChiefBusy = errors.New("the chief is busy")

// decideNudge reads c's transcript, asks the chief and acts on its answer
// under the guards. decided: a decision was recorded; stop: the owner's other
// candidates should wait (the chief is busy); refused: the chief's runtime
// refused the check for a reason that does not pass on its own (nudgeRefusal's
// reason, chief_nudge_retry.go), "" otherwise.
func (s *Server) decideNudge(octx context.Context, o chiefnudge.Owner, chief agents.Agent, c nudgeCandidate, p *nudgePass) (decided, stop bool, refused string) {
	read, err := s.nudgeTranscript(octx, c)
	msgs, full := read.Msgs, read.Full
	if err != nil {
		s.logger.Printf("chief nudges: %s %s: transcript: %v", c.Kind, c.Key, err)
		return false, false, ""
	}
	last, ok := chiefnudge.LastIsAssistant(msgs)
	if !ok {
		// The owner spoke last (or the session said nothing): not stalled on
		// its own word.
		return false, false, ""
	}
	injected, err := s.nudgeInjected(octx, c.Session)
	if err != nil {
		s.logger.Printf("chief nudges: %s %s: injected: %v", c.Kind, c.Key, err)
		return false, false, ""
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
	lastRaw := last.Text
	last.Text = stallClassifierText(lastRaw)
	p.budget--
	d := chiefnudge.Decision{Kind: c.Kind, Key: c.Key, Host: c.Host, TmuxSession: c.TmuxSession, Window: c.Window,
		AgentID: c.AgentID, ChatSessionID: c.ChatSessionID, Title: c.Title, StreakKey: c.StreakKey, AnchorKey: chiefnudge.Anchor(owner),
		IdleSince: c.IdleSince, Mode: s.chiefNudgeMode(o.ID), EpisodeID: c.episodeID}
	// The stall classifier first (jev.go): it runs in shadow too — measuring it
	// on real idle stalls is the point — and a failure only means the chief
	// decides alone.
	//
	// 🔴 ONCE PER STREAK, NOT ONCE PER PASS: a streak whose call sent nothing is
	// asked about again next pass, and reuses the Result kept for it
	// (chief_nudge_retry.go) rather than bill the owner's key again.
	rkey := nudgeRetryKey(o.ID, c.Session)
	//
	// …and once per streak whichever pass ran first: session state's pass may
	// have classified it, and stored the answer on the session's episode
	// (session_state.go), which outlives a restart.
	jev, cached := s.nudgeRetry.cachedJev(rkey, s.now())
	if !cached {
		jev, cached = s.episodeJevFor(octx, c.Session)
	}
	if cl := s.ext.ChiefNudge.Classifier; cl != nil && !cached {
		jev = s.classifyStall(octx, cl, o.ID, lastRaw)
		if jev.Err != "" {
			s.logger.Printf("chief nudges: %s %s: the stall classifier: %s; the chief decides alone", c.Kind, c.Key, jev.Err)
		}
	}
	d = d.WithJev(jev)
	// askStall: the session is NEEDS-YOU — idle, and the classifier says its
	// last message asked the owner something.
	askStall := s.needsYouStall(jev)
	// notify: session state is on, so the chief answers nudge, leave or notify
	// (step 3a) and the session's episode carries the push.
	notify := s.sessionStateOn()
	hint := chiefnudge.Hint{Notify: notify}
	if askStall {
		hint.AskStall, hint.Label, hint.AskScore = true, jev.Label, jev.AskScore
	}
	raw, err := s.askChiefForNudge(octx, chief, nudgeSessionName(chief.Name, o.ID, c.Session),
		chiefnudge.Prompt(c.Session, last, owner, c.IdleFor, hint, fence.NewNonce()))
	var refusal nudgeRefusal
	if errors.As(err, &refusal) {
		// Refused for capacity or not-ready: retried, bounded, then recorded
		// (chief_nudge_retry.go).
		now := s.now()
		s.nudgeRetry.keepJev(rkey, jev, now)
		since, logNow := s.nudgeRetry.refused(rkey, true, now)
		if logNow {
			s.logger.Printf("chief nudges: %s %s: the chief refused the check (%s); retrying each pass, and recording it %s after %s of refusals",
				c.Kind, c.Key, refusal.reason, chiefnudge.Suppressed(nudgeRefusedGuard(refusal.reason)), chiefNudgeRefusedBound)
		}
		if now.Sub(since) < chiefNudgeRefusedBound {
			return false, true, refusal.reason
		}
		s.nudgeRetry.forget(rkey)
		d.Decision, d.Outcome, d.Detail = chiefnudge.DecisionInvalid, chiefnudge.Suppressed(nudgeRefusedGuard(refusal.reason)),
			nudgeRefusedDetail(refusal.reason, chiefNudgeRefusedBound)
		// The owner's other candidates are refused alike; their clocks started
		// with this one's, so they are recorded as they are reached.
		return s.recordNudge(octx, d, "chief_nudge.suppressed"), false, refusal.reason
	}
	if errors.Is(err, errNudgeChiefBusy) {
		s.nudgeRetry.keepJev(rkey, jev, s.now())
		return false, true, ""
	}
	// The chief took the call: this streak is decided below, and the owner's
	// chief was not refusing.
	s.nudgeRetry.forget(rkey)
	s.nudgeRetry.answered(o.ID)
	if err != nil {
		// 🔴 ONE FAILED CALL DOES NOT WEDGE THE OWNER'S SWEEP: it is recorded
		// on THIS candidate (so this streak is not retried every pass) and the
		// owner's next candidate is judged. A new streak is judged afresh.
		d.Decision, d.Outcome, d.Detail = chiefnudge.DecisionInvalid, chiefnudge.Suppressed(chiefnudge.GuardChiefError), "the chief's call failed"+failureKind(err)
		return s.recordNudge(octx, d, "chief_nudge.suppressed"), false, ""
	}
	parse := chiefnudge.ParseProposal
	if notify {
		parse = chiefnudge.ParseNotifyProposal // an "ask" is read as a notify
	}
	prop, perr := parse(raw)
	if perr != nil {
		d.Decision, d.Outcome, d.Reason = chiefnudge.DecisionInvalid, chiefnudge.Suppressed(chiefnudge.GuardInvalid), perr.Error()
		return s.recordNudge(octx, d, "chief_nudge.suppressed"), false, ""
	}
	if notify {
		prop.Question = "" // a notify carries no question
	}
	d.Decision, d.Reason, d.Covered = prop.Decision, prop.Reason, prop.CoveredAction
	pointing := prop.Decision == chiefnudge.DecisionAsk || prop.Decision == chiefnudge.DecisionNotify
	if notify && prop.Decision == chiefnudge.DecisionNudge && viewOnly(c) {
		// With session state on the view-only rule comes FIRST, so a
		// needs-you nudge to one says why too (with it off, it runs where it
		// always did, below).
		return s.actViewOnly(octx, c, d), false, ""
	}
	// 🔴 THE BACKTESTED RULE, AND THE NEEDS-YOU NUDGE: an ask-stall (ask score
	// at or over the threshold — the session is needs-you) defaults to pointing
	// the owner at it. A "leave" never quiets it ("Push it anyway", operator
	// decision 12): it becomes the notify. A nudge survives only when the quote
	// guard passes — the owner's own words, which the prompt (needsYouRule)
	// requires to ANSWER the question the session asked (decision 11; any
	// label, needs_input as well as asks_permission). It is then sent only
	// under the needs-you gate (chiefNudgeLiveFor, below).
	if askStall && !pointing {
		why := ""
		switch {
		case prop.Decision == chiefnudge.DecisionLeave:
			why = "the chief said leave it"
		case !notify && jev.Label != chiefnudge.LabelAsksPermission:
			// Session state off: unchanged by step 3a — no needs-you nudge.
			why = "a nudge is allowed only on an asks_permission stall, and this one is " + jev.Label
		default:
			if _, qerr := chiefnudge.CheckQuote(prop, owner); qerr != nil {
				why = "the nudge's quote failed: " + qerr.Error()
			}
		}
		if why != "" {
			q, read, generic := prop.Question, "", false
			if strings.TrimSpace(q) == "" {
				// 🔴 NO SESSION TEXT FROM MUSTER: the generic ask (jev.go), never
				// an excerpt of the session's last message under the chief's
				// label. Its only words about the session are the chief's own:
				// its reason (prop.Reason, never d.Reason's muster-built prefix)
				// as its read of the session.
				read = prop.Reason
				q, generic = chiefnudge.GenericAskQuestion(c.Title, read), true
			}
			d.Decision, d.Question = chiefnudge.DecisionAsk, q
			def := "an ask"
			if notify {
				def = "notify"
			}
			d.Reason = fmt.Sprintf("classifier: %s, ask score %.2f — the default is %s (%s); %s",
				jev.Label, jev.AskScore, def, why, prop.Reason)
			return s.actAsk(octx, c, d, read, generic), false, ""
		}
	}
	switch prop.Decision {
	case chiefnudge.DecisionLeave:
		d.Outcome = chiefnudge.OutcomeLeft
		return s.recordNudge(octx, d, "chief_nudge.left"), false, ""
	case chiefnudge.DecisionAsk, chiefnudge.DecisionNotify:
		d.Question = prop.Question
		return s.actAsk(octx, c, d, "", false), false, ""
	}
	// A nudge.
	if viewOnly(c) {
		return s.actViewOnly(octx, c, d), false, ""
	}
	if read.Partial {
		// The read cannot vouch for the owner's last six hours (it ended
		// early, has a gap, or a cap cut the window): its owner messages
		// authorise nothing, so the nudge is an ask, and the row says why.
		d.Decision = chiefnudge.DecisionAsk
		d.Question = chiefnudge.EscalationPartial(c.Title)
		d.Reason = chiefnudge.GuardPartial + ": the transcript read was incomplete, so no owner message can authorise a nudge; " + d.Reason
		return s.actAsk(octx, c, d, "", false), false, ""
	}
	d.QuoteID, d.QuoteText = prop.QuoteID, chiefnudge.Cut(prop.Quote, chiefnudge.MaxQuoteRunes)
	quoted, qerr := chiefnudge.CheckQuote(prop, owner)
	if qerr != nil && untimed > 0 && len(owner) == 0 {
		// The host sent the owner's messages with no times (an older
		// muster-mirror): none can authorise a nudge, so it is an ask — and
		// the decision says why.
		d.Decision = chiefnudge.DecisionAsk
		d.Question = chiefnudge.EscalationNoTimes(c.Title)
		d.Detail = strconv.Itoa(untimed) + " message(s) of the owner's arrived with no time (an older muster-mirror): none can authorise a nudge"
		return s.actAsk(octx, c, d, "", false), false, ""
	}
	if qerr != nil {
		d.Outcome, d.Detail = chiefnudge.Suppressed(chiefnudge.GuardQuote), qerr.Error()
		return s.recordNudge(octx, d, "chief_nudge.suppressed"), false, ""
	}
	d.QuoteAt = quoted.At
	n, err := s.ext.ChiefNudges.NudgesForAnchor(octx, c.Kind, c.Key, d.AnchorKey)
	if err != nil {
		s.logger.Printf("chief nudges: %s %s: nudge count: %v", c.Kind, c.Key, err)
		return false, false, ""
	}
	if n >= chiefnudge.MaxNudgesPerAnchor {
		// 🔴 THE THIRD NUDGE IS AN ASK: the session has been told twice on the
		// strength of the same owner message and stopped again.
		d.Decision = chiefnudge.DecisionAsk
		d.Question = chiefnudge.EscalationQuestion(c.Title)
		d.Reason = "escalated: " + strconv.Itoa(n) + " nudges since the owner's last message; " + d.Reason
		return s.actAsk(octx, c, d, "", false), false, ""
	}
	// 🔴 A NEEDS-YOU NUDGE IS LIVE ONLY UNDER BOTH LISTS (chiefNudgeLiveFor):
	// otherwise it is shadow — recorded as would_send, never pasted, and its
	// stop pushed (episodeVerdict: shadow never quiets a stop). Session state
	// on only: with it off there is no such gate.
	if notify {
		d.Mode = s.nudgeModeFor(o.ID, askStall)
	}
	// The hourly cap bounds what is SENT. Shadow mode records what it would do,
	// unsuppressed (operator decision), and is not counted.
	if d.Mode == chiefnudge.ModeLive && p.hourly >= s.chiefNudgeHourlyCap() {
		d.Outcome, d.Detail = chiefnudge.Suppressed(chiefnudge.GuardHourlyCap),
			strconv.Itoa(p.hourly)+" nudges in the last hour (cap "+strconv.Itoa(s.chiefNudgeHourlyCap())+")"
		return s.recordNudge(octx, d, "chief_nudge.suppressed"), false, ""
	}
	d.Message = chiefnudge.NudgeText(o.Name, quoted, prop.Quote, time.Local)
	if d.Mode != chiefnudge.ModeLive {
		d.Outcome = chiefnudge.OutcomeWouldSend
		return s.recordNudge(octx, d, "chief_nudge.would_send"), false, ""
	}
	if s.actNudge(octx, c, d) {
		p.hourly++
		return true, false, ""
	}
	return false, false, ""
}

// viewOnly reports whether c is a session muster cannot type into (an
// opencode pane: mirror.RuntimeSpec Caps, no Send). A Hermes window is never
// judged at all (nudgeWindowKey).
func viewOnly(c nudgeCandidate) bool { return c.Kind == chiefnudge.KindMirror && c.Runtime != "" }

// actViewOnly is a nudge to a view-only session: 🔴 NEVER NUDGED — a queued
// nudge could only fail at its paste. It becomes the notice (a notify with
// session state on), with its reason.
func (s *Server) actViewOnly(octx context.Context, c nudgeCandidate, d chiefnudge.Decision) bool {
	d.Decision = chiefnudge.DecisionAsk
	d.Question = chiefnudge.EscalationViewOnly(c.Title)
	d.Reason = "view_only: muster cannot type into a " + c.Runtime + " session, so no nudge is sent; " + d.Reason
	return s.actAsk(octx, c, d, "", false)
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
	if c.Runtime != "" {
		e = mirror.SessionView{Host: c.Host, Session: c.TmuxSession, Window: c.Window, Runtime: c.Runtime, AgentSession: mirror.SessionIDOf(c.Key)}
	}
	// The stream names its conversation by the BARE id (an opencode view's Gen
	// is its opencode session id, not the "opencode:" key).
	evs, idx, err := s.mirrorRead(ctx, e, mirror.SessionIDOf(c.Key))
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

// nudgeSessionName is the session key of ONE stall check: derived from the
// decision it is for (owner, session, idle streak), and never an operator
// thread's key. No chat_sessions row exists for it (chief_agent.go says why).
//
// What makes the check's context fresh is what the chief's runtime does with
// the key, and that differs by kind:
//
//   - a `gateway` chief keys its sessions by the session key it is sent
//     (X-Openclaw-Session-Key), so this key alone is a session of its own;
//   - a `claude-code` chief's ccd does NOT: a turn without
//     agents.HeaderCCSession goes to its primary CLI — the operator's own
//     conversation with the chief — whatever its session key. So the check
//     also carries the key as an explicit session (nudgeRoute), and ccd runs it
//     in a CLI of its own (a lane, cmd/ccd/lanes.go), started with an empty
//     conversation and stopped as soon as the check's turn is over.
//
// Either way no check sees another's and none grows with the ones before it.
// The key is the same only for the same decision, and a decision is asked
// again after a call that sent nothing (errNudgeChiefBusy) — and also after a
// call that DID paste but whose answer muster never recorded: muster restarted
// mid-check, so the next pass asks again under the same key, and the lane
// resumes a transcript that holds that earlier prompt (and any answer to it).
// Harmless: the model sees its own earlier question once, before the same
// question again.
func nudgeSessionName(agentName string, owner int64, c chiefnudge.Session) string {
	sum := sha256.Sum256([]byte(strconv.FormatInt(owner, 10) + "\x00" + c.Kind + "\x00" + c.Key + "\x00" + c.StreakKey))
	return "agent:" + agentName + ":webchat:nudge:" + hex.EncodeToString(sum[:8])
}

// nudgeRoute is ctx carrying the route of a stall check under key: for a
// claude-code chief, key as an explicit claude-code session marked EPHEMERAL
// and TOOL-LESS (withRoute, the one place that route is built), with no model — ccd starts
// the lane's CLI on the pod's default model, as the primary runs. A gateway
// chief gets no route: its runtime has no lanes, and its per-decision session
// key is already a session of its own. Its request carries the same headers as
// before lanes — but what changed around it applies to every kind of chief: the
// call's budget is chiefNudgeTurnBudget (4m, was 3m) and the chief's claim
// lease is that plus a minute (5m, was 4m), and a 503 not_ready is now retried
// bounded, then recorded (chief_nudge_retry.go).
//
// 🔴 EPHEMERAL IS WHAT KEEPS A CHECK OFF THE OPERATOR'S SESSIONS. ccd stops a
// check's CLI as soon as its turn is over, so no idle check CLI holds a slot or
// pod memory. When every slot (CCD_MAX_SESSIONS, default 3) is taken by an
// operator session, ccd refuses the check (503 capacity) rather than stop one of
// them to make room. ccd never stops an idle operator lane on its own, so that
// does not pass by itself: the check is retried for chiefNudgeRefusedBound,
// then recorded suppressed:lanes_full (chief_nudge_retry.go).
//
// 🔴 TOOL-LESS IS WHAT KEEPS A CHECK FROM ACTING. The check's prompt quotes
// other sessions' transcripts (redacted, fenced as data) — but the fence is
// prose. ccd starts the check's CLI with no built-in tool and no MCP server
// (agents.HeaderCCToolless, cmd/ccd toollessArgs), so whatever a transcript
// says, the chief's only possible effect in a check is its text reply, which
// decideNudge parses and validates. Shadow mode gates only the sweep's own
// sends; this gates the chief's tools.
//
// ⚠ VERSION SKEW, TOOLS: a ccd that predates agents.HeaderCCToolless ignores it
// and starts the check's CLI with the chief's full tool set (built-ins and
// muster's MCP tools), and muster does not detect it: tool-lessness is
// ROLLOUT DISCIPLINE (operator decision), not enforced here. The steps are in
// docs/chief-nudges.md, "Rolling out".
//
// ⚠ VERSION SKEW: a ccd that predates agents.HeaderCCEphemeral ignores it and
// keeps the check's CLI like any session's. At its cap it then stops the least
// recently used idle lane, which may be the operator's. The header ships in the
// same release as the claude-code agent image; a chief pod still running an
// older image behaves that way until it is restarted on the new one.
func nudgeRoute(ctx context.Context, chief agents.Agent, key string) context.Context {
	if agents.ResolveKind(chief.Kind) != agents.KindClaudeCode {
		return ctx
	}
	return withRoute(ctx, chief, agents.ChatSession{SessionKey: key, Runtime: agents.SessionRuntimeClaudeCode}, true)
}

// askChiefForNudge puts prompt to the chief and returns its answer, holding
// the chief's session claim and turn gate (as a question turn does) so it never
// overlaps another of the chief's turns. errNudgeChiefBusy when the chief
// cannot take it now: nothing was sent, so the decision is not recorded and the
// streak is asked about again next pass. Two refusals that do not pass on their
// own come back as a nudgeRefusal (which IS errNudgeChiefBusy): ccd has no slot
// for the check's own CLI (agentgateway.LanesFull), or the runtime answered a
// typed 503 not_ready — decideNudge retries those bounded
// (chief_nudge_retry.go).
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
	tctx, cancel := context.WithTimeout(nudgeRoute(ctx, chief, key), chiefNudgeTurnBudget)
	defer cancel()
	reply, err := s.ext.Gateway.Chat(tctx, chief, key, prompt, nil)
	switch {
	case agentgateway.LanesFull(err):
		return "", nudgeRefusal{refusalLanesFull}
	case agentgateway.Unsent(err) == agentgateway.UnsentNotReady:
		return "", nudgeRefusal{refusalNotReady}
	case agentgateway.Unsent(err) != "":
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

// nudgeState is what muster can read, now, of the session a decision is about.
type nudgeState int

const (
	// nudgeUnknown: the session's state could not be read — its host is
	// disconnected or has sent no window list, the window reports no status
	// time, or a read failed. 🔴 UNKNOWN IS NOT MOVED ON: whatever was about to
	// happen (a send, an ask, a withdrawal, a paste) is HELD for a later pass,
	// never suppressed, withdrawn or marked stale on it. Existing bounds still
	// end a hold: a notice's own expiry, the send queue's AuthorizedFor.
	nudgeUnknown nudgeState = iota
	// nudgeStalled: read, and still in the idle streak the decision was taken
	// on.
	nudgeStalled
	// nudgeMovedOn: read, the same conversation, and NOT that streak — not
	// idle, a newer streak, an agent turn running, a newer message.
	nudgeMovedOn
	// nudgeMovedAway: read, and the window no longer holds the conversation —
	// another Claude conversation in it, or a window or session the connected
	// host no longer lists, or one it reports no Claude Code in. Moved on too
	// (movedOn); kept apart so the send queue's gate can say which, from this
	// one read.
	nudgeMovedAway
)

// movedOn: a state that was read and is not the decision's streak.
func (st nudgeState) movedOn() bool { return st == nudgeMovedOn || st == nudgeMovedAway }

// nudgeStateNow is d's session's state now against the streak d was decided
// on: the last thing muster checks before it sends, asks, withdraws an ask or
// pastes anything. The ONE predicate for "moved on" — every live step asks it,
// handles nudgeUnknown as a wait, and only movedOn() as moved on.
//
// 🔴 THE STREAK, THEN THE EPISODE: a decision taken in a session-state episode
// (d.EpisodeID) holds only while that episode is still the session's open one.
// A stop that ended (sustained work, its window gone) and a new one that began
// is a different stop even if the streak reads the same, so a queued nudge for
// the old one is DROPPED at its paste and the new stop pushes. So is one whose
// stop was PUSHED after the decision was taken (its hold, episodeNudgeHold,
// ran out): the owner was told, and must not get the nudge as well.
func (s *Server) nudgeStateNow(ctx context.Context, d chiefnudge.Decision) nudgeState {
	st := s.nudgeStreakNow(ctx, d)
	if st != nudgeStalled || d.EpisodeID == 0 || s.ext.Episodes == nil {
		return st
	}
	ep, ok, err := s.ext.Episodes.OpenFor(ctx, d.Kind, d.Key)
	switch {
	case err != nil:
		return nudgeUnknown
	case !ok || ep.ID != d.EpisodeID:
		return nudgeMovedOn
	case !d.DecidedAt.IsZero() && ep.PushedAt.After(d.DecidedAt):
		return nudgeMovedOn
	}
	return nudgeStalled
}

// nudgeStreakNow is nudgeStateNow's streak half: d's session read now against
// the idle streak d was decided on.
func (s *Server) nudgeStreakNow(ctx context.Context, d chiefnudge.Decision) nudgeState {
	if d.Kind == chiefnudge.KindMirror {
		if s.ext.Mirror == nil {
			return nudgeUnknown
		}
		st := s.ext.Mirror.AgentState(ctx, d.Host, d.TmuxSession, d.Window)
		switch {
		case st.Unknown != nil && nudgeTargetGone(st.Unknown):
			return nudgeMovedAway
		case st.Unknown != nil:
			return nudgeUnknown
		}
		cur, read := st.Session, true
		if cur == "" {
			// AgentState names only a Claude Code conversation: another
			// runtime's (opencode) is its window's session key, from the list.
			cur, read = s.mirrorWindowKey(ctx, d.Host, d.TmuxSession, d.Window)
		}
		switch {
		case !read:
			return nudgeUnknown
		case cur != d.Key:
			return nudgeMovedAway
		case st.Status != mirror.StatusIdle:
			return nudgeMovedOn
		case st.Since <= 0:
			return nudgeUnknown
		}
		return nudgeStreakState("since:"+strconv.FormatInt(st.Since, 10), d)
	}
	if s.ext.Agents == nil || d.ChatSessionID == 0 {
		return nudgeUnknown
	}
	if s.turns.busy(d.AgentID) {
		return nudgeMovedOn
	}
	last, err := s.ext.Agents.ListRecentChatMessages(ctx, d.ChatSessionID, 1)
	switch {
	case err != nil:
		return nudgeUnknown
	case len(last) == 0:
		return nudgeMovedOn
	}
	return nudgeStreakState(agentStreakKey(last[len(last)-1]), d)
}

// mirrorWindowKey is the session key of the conversation the window (host,
// tmux session, window id) shows now (mirror.Window.SessionKey), read from ctx's
// principal's window list; ok false when the list does not carry that window.
func (s *Server) mirrorWindowKey(ctx context.Context, host, session, window string) (key string, ok bool) {
	for _, h := range s.ext.Mirror.Hosts(ctx) {
		if h.Host != host {
			continue
		}
		for _, w := range h.Windows {
			if w.Session == session && w.ID == window {
				return w.SessionKey(), true
			}
		}
	}
	return "", false
}

// nudgeStreakState is a streak that was read, against d's.
func nudgeStreakState(streak string, d chiefnudge.Decision) nudgeState {
	if streak == d.StreakKey {
		return nudgeStalled
	}
	return nudgeMovedOn
}

// nudgeTargetGone: an AgentState refusal that is a READ of the target, not a
// failure to read it — the host is connected and its window list no longer
// carries the tmux session or window, or reports no Claude Code in it. A host
// that has sent no window list yet (its first read failed) is unknown, not
// gone (mirror.Hub.AgentState). Every other refusal (the host offline or not
// reporting status) is unknown.
//
// ⚠ LIMIT: this trusts the host's list. A host whose process-table read
// fails (macOS: `ps` failing) still sends its windows, with no Claude Code
// found in any of them — indistinguishable here from Claude Code having
// exited, so read as gone. Fixing that belongs in muster-mirror (keep the
// last good process table), not here.
func nudgeTargetGone(err error) bool {
	return errors.Is(err, mirror.ErrSessionGone) || errors.Is(err, mirror.ErrWindowGone) || errors.Is(err, mirror.ErrNotClaude)
}

// actNudge sends a live nudge: queued for a mirrored window (sendQueueNudgeGate
// re-checks it at the paste), a turn for an agent. The decision is recorded
// FIRST — the streak's claim — so two passes never send it twice.
func (s *Server) actNudge(ctx context.Context, c nudgeCandidate, d chiefnudge.Decision) bool {
	switch st := s.nudgeStateNow(ctx, d); {
	case st == nudgeUnknown:
		// Held: nothing recorded, so a later pass judges this streak again.
		s.logger.Printf("chief nudges: %s %s: the session's state could not be read; the nudge waits for a later pass", d.Kind, d.Key)
		return false
	case st.movedOn():
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
	if _, err := s.ext.ChiefNudges.Move(ctx, d.ID, []string{state}, chiefnudge.Move{Outcome: state, SendQueueID: it.ID}, ""); err != nil {
		s.logger.Printf("chief nudges: decision %d: record queued message %d: %v", d.ID, it.ID, err)
	}
	s.kickSendQueue()
}

// sendNudgeTurn starts text as a turn to d's agent — never overlapping one of
// its turns — and moves d from state to done (with action's audit row), or to
// suppressed:moved_on when the conversation was read and has moved on. A
// conversation that could not be read leaves d in state for the next pass.
func (s *Server) sendNudgeTurn(ctx context.Context, d chiefnudge.Decision, text, state, done, action string) {
	// 🔴 THE KILL SWITCH REACHES AN AGENT'S NUDGE TOO, read at the
	// moment of sending (the send queue's gate does it for a window).
	if !s.nudgeSendLive(d.Owner, d) {
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
	switch st := s.nudgeStateNow(ctx, d); {
	case st == nudgeUnknown:
		return // held: the next pass's reconcile tries again
	case st.movedOn():
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
	s.startAgentTurn(ctx, a, sess, text, notTyped) // muster's nudge, never the owner's words
}

// failNudge moves d from state to failed, saying why.
func (s *Server) failNudge(ctx context.Context, d chiefnudge.Decision, state, why string) {
	if _, err := s.ext.ChiefNudges.Move(ctx, d.ID, []string{state}, chiefnudge.Move{Outcome: chiefnudge.OutcomeFailed, Detail: why}, "chief_nudge.failed"); err != nil {
		s.logger.Printf("chief nudges: decision %d: fail: %v", d.ID, err)
	}
}

// actAsk is an ask decision: shadow records it; live raises a NOTICE on the
// owner's queue (attention.KindNotice: muster's own, labelled with where the
// session is — noticeLabel), which points at the session and asks nothing:
// no options, no answer, no relay, and not counted toward the open-ask cap.
// The decision keeps the chief's question (the Chief nudges tab shows it); no
// option is asked for or kept — a notice has nothing to choose. generic: the
// chief wrote no question (the generic ask), and read is its own reason the
// notice shows (chiefnudge.GenericAskRead), "" for none.
func (s *Server) actAsk(ctx context.Context, c nudgeCandidate, d chiefnudge.Decision, read string, generic bool) bool {
	if s.sessionStateOn() {
		return s.actNotify(ctx, d)
	}
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
	switch st := s.nudgeStateNow(ctx, d); {
	case st == nudgeUnknown:
		// Held: nothing raised or recorded, so a later pass judges this streak
		// again.
		s.logger.Printf("chief nudges: %s %s: the session's state could not be read; the ask waits for a later pass", d.Kind, d.Key)
		return false
	case st.movedOn():
		d.Outcome, d.Detail = chiefnudge.Suppressed(chiefnudge.GuardMovedOn), "the session left its idle streak before muster acted"
		return s.recordNudge(ctx, d, "chief_nudge.suppressed")
	}
	// 🔴 A NOTICE, NEVER AN ASK: nothing here can be answered, so nothing is
	// relayed, and the open-ask cap (which a notice neither counts toward nor
	// is refused at) can no longer suppress a nudge or hold a real ask back.
	ask, err := s.ext.Attention.Create(ctx, attention.New{
		Asker:    attention.Asker{System: true, Label: noticeLabel(c.Session)},
		Question: nudgeNoticeText(d.Question, read, generic),
		Notice:   true,
	})
	if err != nil {
		d.Outcome, d.Detail = chiefnudge.Suppressed(chiefnudge.GuardAskFailed), err.Error()
		return s.recordNudge(ctx, d, "chief_nudge.suppressed")
	}
	d.Outcome, d.AskID = chiefnudge.OutcomeAsked, ask.ID
	if !s.recordNudge(ctx, d, "chief_nudge.noticed") {
		// Another pass decided this streak first: withdraw the duplicate.
		if _, err := s.cancelAsk(ctx, ask.ID); err != nil {
			s.logger.Printf("chief nudges: withdraw duplicate ask %d: %v", ask.ID, err)
		}
		return false
	}
	s.askFiled(ask)
	return true
}

// actNotify records d as a NOTIFY (session state on; step 3a): the chief, or a
// rule over its answer (view only, a partial read, the nudge budget, a quote
// that failed on an asking session, a leave on one), points the owner at the
// session. 🔴 NOTHING IS RAISED AND NO TEXT IS KEPT: no notice, no question,
// no options — the session's episode carries the one text-free push for the
// stop (session_push.go), in shadow mode as in live (shadow never quiets a
// stop). The row keeps the chief's one-line reason, and why muster made it a
// notify, for the Chief nudges tab.
func (s *Server) actNotify(ctx context.Context, d chiefnudge.Decision) bool {
	d.Decision, d.Question, d.Options, d.Outcome = chiefnudge.DecisionNotify, "", nil, chiefnudge.OutcomeNotified
	return s.recordNudge(ctx, d, "chief_nudge.notified")
}

// nudgeNoticeMaxRunes bounds a notice's message: the chief's short message, one
// line or two — the notice points at the session, which holds the rest.
const nudgeNoticeMaxRunes = 500

// nudgeNoticeText is a notice's message: the chief's own words about what the
// session waits on ("Waiting on you: <the chief's question>"), or — for the
// generic ask, when the chief wrote no question — muster's fixed sentence and
// the chief's read (chiefnudge.GenericAskRead). Where the session is, is the
// notice's label and item (noticeLabel, noticeItem), not this text. The
// session's words reach the owner only through the chief's own.
func nudgeNoticeText(q, read string, generic bool) string {
	text := "Waiting on you: " + strings.TrimSpace(q)
	if generic || strings.TrimSpace(q) == "" {
		text = chiefnudge.GenericAskBody + chiefnudge.GenericAskRead(read)
	}
	return fence.Cut(text, nudgeNoticeMaxRunes)
}

// reconcileNudge settles one live decision in flight, as its owner: a queued
// nudge that was sent (or failed), a notice that was dismissed, expired or
// outlived its session's stall. done reports that it is no longer in flight.
func (s *Server) reconcileNudge(ctx context.Context, d chiefnudge.Decision) (done bool) {
	octx := s.attentionPrincipal(ctx, d.Owner)
	switch d.Outcome {
	case chiefnudge.OutcomeRelayQueued:
		// 🔴 THE ROLLOUT OVERLAP: 0071 failed every relay_queued decision, but an
		// older replica still serving during a rolling deploy can claim an answer
		// to a nudge ask after it ran. Nothing here relays, so it is failed — with
		// its audit row — rather than left in flight for ever.
		s.failNudge(octx, d, d.Outcome, NudgeRelayRetired)
		return true
	case chiefnudge.OutcomeQueued:
		if d.SendQueueID == 0 {
			age := s.now().Sub(d.UpdatedAt)
			switch {
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
		switch it.State {
		case sendqueue.Sent:
			_, _ = s.ext.ChiefNudges.Move(octx, d.ID, []string{d.Outcome}, chiefnudge.Move{Outcome: chiefnudge.OutcomeSent, Detail: it.Detail}, "chief_nudge.sent")
			return true
		case sendqueue.Failed, sendqueue.Cancelled:
			if it.Detail == SendQueueNudgeStale {
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
			// 🔴 ONLY A STATE THAT WAS READ withdraws the ask. One that could not
			// be read (the host's connection dropped, which once withdrew every
			// open ask on a host in one pass while its sessions sat still) leaves it open
			// for the next pass; the ask's own expiry still ends it.
			if s.nudgeStateNow(octx, d).movedOn() {
				// The owner (or anything) moved the session on: the question is
				// stale. Withdraw it.
				if _, err := s.cancelAsk(octx, a.ID); err == nil {
					_, _ = s.ext.ChiefNudges.Move(octx, d.ID, []string{chiefnudge.OutcomeAsked},
						chiefnudge.Move{Outcome: chiefnudge.OutcomeAskClosed, Detail: "the session moved on; the ask was withdrawn"}, "chief_nudge.ask_closed")
					return true
				}
			}
			return false
		}
		moved, _ := s.ext.ChiefNudges.Move(octx, d.ID, []string{chiefnudge.OutcomeAsked},
			chiefnudge.Move{Outcome: chiefnudge.OutcomeAskClosed, Detail: "the ask ended " + string(a.Status) + "; nothing to relay"}, "chief_nudge.ask_closed")
		if moved && a.Status == attention.Cancelled {
			// 🔴 A WITHDRAWAL NOBODY ANNOUNCED: migration 0071 cancels every
			// open nudge ask with a bare UPDATE, so no withdrawal push went out
			// and a phone still shows it. Closing its decision (once: Move is
			// conditional) clears its notification now. A dismissal that already
			// cleared it is cleared again — a withdrawal by tag is idempotent.
			s.askSettled(a)
		}
		return true
	}
	return true
}

// NudgeRelayRetired is the detail of a decision whose answer an older replica
// claimed for a relay (relay_queued) after 0071: relaying is retired.
const NudgeRelayRetired = "not relayed: relaying an answer to a nudge was retired (migration 0071, notices)"

// Why a chief-nudge queued row is not pasted (sendQueueNudgeGate).
const (
	SendQueueNudgeOff     = "Not sent: chief nudges are off, or in shadow mode for you, on this server."
	SendQueueNudgeUnknown = "Not sent: its chief nudge could not be read."
	SendQueueNudgeClosed  = "Not sent: its chief nudge is no longer waiting to be sent"
	SendQueueNudgeMoved   = "Not sent: the window no longer holds the conversation the chief nudged."
	SendQueueNudgeStale   = "Not sent: the session moved on after the chief decided to nudge it."
	// SendQueueNudgeWait is the one gate answer that is NOT final: the
	// session's state could not be read (nudgeUnknown), so the row goes back to
	// queued (sendQueueDeliver hands it on as sendqueue.Outcome.Superseded:
	// nothing reached the host) and is decided on afresh by a later pass.
	// Its words hold on both paths: the dispatcher requeues the row, a
	// person's Send now ends it failed.
	SendQueueNudgeWait = "Not pasted: muster could not read the session's state (is its host connected?)."
)

// sendQueueNudgeGate is why chief-nudge row it may not be pasted now, "" when
// it may: the feature on and the owner live (the kill switch reaches a row
// already queued), its decision still waiting on THIS row, and the session
// still in the idle streak the chief judged (which includes the window still
// holding the conversation the decision was about). A state that cannot be read is
// SendQueueNudgeWait — held, never stale or moved. ctx is the row's owner.
func (s *Server) sendQueueNudgeGate(ctx context.Context, it sendqueue.Item) string {
	if !s.chiefNudgeLive(requestOwner(ctx)) {
		return SendQueueNudgeOff
	}
	d, err := s.ext.ChiefNudges.Get(ctx, it.ChiefNudgeID)
	if err != nil {
		return SendQueueNudgeUnknown
	}
	if !s.nudgeSendLive(requestOwner(ctx), d) {
		// A needs-you nudge for an owner taken off its own list.
		return SendQueueNudgeOff
	}
	if d.Outcome != chiefnudge.OutcomeQueued ||
		(d.SendQueueID != 0 && d.SendQueueID != it.ID) {
		return SendQueueNudgeClosed + " (" + d.Outcome + ")."
	}
	switch s.nudgeStateNow(ctx, d) {
	case nudgeUnknown:
		// 🔴 A DISCONNECTED HOST IS NOT A MOVED WINDOW: its windows are simply
		// absent from the list, which says nothing about what they hold.
		return SendQueueNudgeWait
	case nudgeMovedAway:
		// Read from the same AgentState as the rest of this decision: one read,
		// so a disconnect between two reads cannot mislabel the row.
		return SendQueueNudgeMoved
	case nudgeMovedOn:
		return SendQueueNudgeStale
	}
	return ""
}
