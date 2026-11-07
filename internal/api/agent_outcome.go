package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agentoutcome"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// EVERY STARTED AGENT ENDS IN AN OUTCOME YOU CAN SEE (internal/agentoutcome's
// package doc has the incident and the states; docs/provisioner.md, "How an agent's work ends").
//
// This file is the tracker: what each event an agent produces does to its
// outcome, and the three things a changed outcome does —
//
//   - a PUSH to the owner, once per change ("<agent> failed: <reason>",
//     "<agent> is stuck: <reason>", "<agent> finished: <first line>"), tagged
//     agent-<id>-outcome so a newer outcome replaces an older one on the device,
//     under its own /account toggle (push.EventAgentOutcome) and the per-owner
//     cap like every push (goNotify);
//   - a NOTE in the chat of the chief that started it (agents.dispatched_by), so
//     the chief can tell its owner and offer to start it again; it costs the
//     chief no turn (noteToChief's shape);
//   - an agent.changed broadcast, so its page and the sidebar redraw.
//
// THE EVENTS, AND WHO FEEDS THEM:
//
//	a dispatch with a first turn  createAndDispatchAgent          → running
//	the first turn starts         internal/agentkickoff           (activity; running if untracked)
//	a turn's stream shows life    agentkickoff, chatTurn          (activity; stuck → running)
//	a turn answers                agentkickoff, chatTurn          → done when idle (first reply recorded)
//	a turn FAILS                  agentkickoff, chatTurn, the
//	                              machine message route            → failed, status `error`
//	its pod reports work          POST /agent/work                 (activity; done once work clears)
//	it moves its task to review   setNoteStatus                   → done
//	or complete
//	nothing, for too long         the watchdog (agent_watchdog.go) → stuck
//	its owner Starts it again     handleAgentStart                 (untracked again)
//
// 🔴 THE CHIEF IS NOT SET TO `error` BY A FAILED TURN. Its status gates every
// delivery muster makes to it (agentCanBeSent: answers, task events, nudges), so
// one failed turn would cut its owner's whole interface off until a restart. Its
// failure is still pushed once and shown as its outcome; its next good turn
// clears it.
// ---------------------------------------------------------------------------

// outcomeActivityEvery bounds how often one agent's activity is WRITTEN: a stream
// calls the tracker per read, and the watchdog's thresholds are minutes.
const outcomeActivityEvery = 10 * time.Second

// outcomePushTitleMax bounds the push title's reason.
const outcomePushTitleMax = 120

// outcomeTracker is the tracker's in-process state: when each agent's activity
// was last written, and how many of its turns are open in this process.
type outcomeTracker struct {
	mu        sync.Mutex
	written   map[int64]time.Time
	openTurns map[int64]int
}

func (t *outcomeTracker) due(id int64, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.written == nil {
		t.written = map[int64]time.Time{}
	}
	if last, ok := t.written[id]; ok && now.Sub(last) < outcomeActivityEvery {
		return false
	}
	t.written[id] = now
	return true
}

func (t *outcomeTracker) open(id int64, delta int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.openTurns == nil {
		t.openTurns = map[int64]int{}
	}
	t.openTurns[id] += delta
	if t.openTurns[id] <= 0 {
		delete(t.openTurns, id)
	}
}

func (t *outcomeTracker) busy(id int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.openTurns[id] > 0
}

// outcomeCtx is the context every outcome write runs on: detached from the
// caller's cancellation (a turn cut off by its socket closing must still be
// recorded) and acting as authz.System (a background loop and a request alike
// write the agent's own row; nothing read here is returned to a caller).
// Listed in the System ledger (internal/authz/ledger_test.go).
func outcomeCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(authz.System(context.WithoutCancel(ctx)), 15*time.Second)
}

func (s *Server) outcomesOn() bool { return s.ext.Outcomes != nil && s.ext.Agents != nil }

// outcomeDispatched marks a freshly created agent with a first turn as started
// (running), naming the chief that started it.
func (s *Server) outcomeDispatched(ctx context.Context, a agents.Agent, chief int64) {
	if !s.outcomesOn() {
		return
	}
	var by *int64
	if chief > 0 && chief != a.ID {
		by = &chief
	}
	octx, cancel := outcomeCtx(ctx)
	defer cancel()
	if err := s.ext.Outcomes.Start(octx, a.ID, by, s.now()); err != nil {
		s.logger.Printf("outcomes: agent %d: mark started: %v", a.ID, err)
	}
}

// outcomeTurnStarted: a turn of a is starting and is known to be taken (the
// kickoff deliverer's first turn): open, and taken now.
func (s *Server) outcomeTurnStarted(ctx context.Context, a agents.Agent) {
	if !s.outcomesOn() {
		return
	}
	s.agentOutcomes.open(a.ID, 1)
	s.outcomeTurnTaken(ctx, a)
}

// outcomeTurn brackets ONE ASK of a turn of a — the one rule for every chat
// door (chatTurn, the machine message route). It returns the context to send
// the ask under and the function to call with its result.
//
// 🔴 AN ASK IS NOT A TURN UNTIL THE RUNTIME TAKES IT. A message sent while
// another turn holds the session is refused before anything is typed (a typed
// 409 busy, agentgateway.Unsent) and #494's hold asks again every poll for up to
// ten minutes. Each refused ask used to record activity and re-arm a waiting
// agent to running: the watchdog read the asks as life, and the agent stopped
// being "waiting" though nothing reached it. So the activity and the re-arm
// (outcomeTurnTaken) run once, at the first sign the runtime took the text —
// its 103 (agents.WithTurnAccepted), the first read of its stream — or when the
// ask ends without an Unsent refusal. The turn is open (the watchdog's
// "working") for the ask's duration either way.
//
// 🔴 THE TURN CLOSES EXACTLY ONCE: end (the ask's result, judged) and abandon (a
// panic: closed, judged as nothing) share one sync.Once, and a caller defers
// them (outcomeTurnBracket.close). A turn left open reads as "working" to the
// watchdog for the life of the process.
func (s *Server) outcomeTurn(ctx context.Context, a agents.Agent) (context.Context, *outcomeTurnBracket) {
	if !s.outcomesOn() {
		return ctx, &outcomeTurnBracket{}
	}
	s.agentOutcomes.open(a.ID, 1)
	b := &outcomeTurnBracket{s: s, ctx: ctx, a: a}
	tctx := agents.WithTurnActivity(ctx, func() { b.take(); s.outcomeActivity(ctx, a) })
	tctx = agents.WithTurnAccepted(tctx, func(acc agents.TurnAcceptance) {
		agents.MarkTurnAccepted(ctx, acc) // the callback ctx already carried
		b.take()
	})
	return tctx, b
}

// outcomeTurnBracket is one ask's bracket (outcomeTurn). The zero value (no
// outcome store) does nothing.
type outcomeTurnBracket struct {
	s           *Server
	ctx         context.Context
	a           agents.Agent
	taken, done sync.Once
}

// take: the runtime took the turn (once).
func (b *outcomeTurnBracket) take() {
	b.taken.Do(func() { b.s.outcomeTurnTaken(b.ctx, b.a) })
}

// end closes the turn with the ask's result, judged (outcomeTurnEnded).
func (b *outcomeTurnBracket) end(reply string, err error) {
	if b.s == nil {
		return
	}
	b.done.Do(func() {
		if agentgateway.Unsent(err) == "" {
			b.take()
		}
		b.s.outcomeTurnEnded(b.ctx, b.a, reply, err)
	})
}

// abandon closes the turn and judges nothing (the ask panicked).
func (b *outcomeTurnBracket) abandon() {
	if b.s == nil {
		return
	}
	b.done.Do(func() { b.s.agentOutcomes.open(b.a.ID, -1) })
}

// close is what a caller defers, with its results: on a panic the turn is
// abandoned and the panic continues; otherwise it ends with them.
func (b *outcomeTurnBracket) close(reply *string, err *error) {
	if r := recover(); r != nil {
		b.abandon()
		panic(r)
	}
	b.end(*reply, *err)
}

// outcomeTurnTaken: the runtime took a turn of a. Activity now (unthrottled),
// and a done or waiting agent re-armed to running.
func (s *Server) outcomeTurnTaken(ctx context.Context, a agents.Agent) {
	octx, cancel := outcomeCtx(ctx)
	defer cancel()
	now := s.now()
	s.recordActivity(octx, a, now)
	// 🔴 A NEW TURN RE-ARMS A DISPATCHED AGENT THAT HAD STOPPED: done or waiting
	// back to running, so its new work is watched (and ends in an outcome)
	// again. An untracked agent (saved, an officer's, the chief) stays untracked.
	if applied, err := s.ext.Outcomes.Transition(octx, a.ID, []string{agentoutcome.Done, agentoutcome.Waiting},
		agentoutcome.Running, "", now); err != nil {
		s.logger.Printf("outcomes: agent %d: re-arm: %v", a.ID, err)
	} else if applied {
		s.outcomeChanged(a)
	}
}

// outcomeActivity: a's turn showed life (a stream event, a work report). Written
// at most every outcomeActivityEvery per agent.
func (s *Server) outcomeActivity(ctx context.Context, a agents.Agent) {
	if !s.outcomesOn() {
		return
	}
	now := s.now()
	if !s.agentOutcomes.due(a.ID, now) {
		return
	}
	octx, cancel := outcomeCtx(ctx)
	defer cancel()
	s.recordActivity(octx, a, now)
}

func (s *Server) recordActivity(octx context.Context, a agents.Agent, now time.Time) {
	recovered, err := s.ext.Outcomes.Activity(octx, a.ID, now)
	if err != nil {
		s.logger.Printf("outcomes: agent %d: record activity: %v", a.ID, err)
		return
	}
	if recovered {
		s.logger.Printf("outcomes: agent %d (%s) is working again", a.ID, a.Name)
		s.outcomeChanged(a)
	}
}

// outcomeTurnEnded: a's turn ended with reply, or with err. A failure that
// counts (turnFailureCounts) is outcomeFailed; an answer records the first
// reply, and — when the agent is idle — its completion.
func (s *Server) outcomeTurnEnded(ctx context.Context, a agents.Agent, reply string, err error) {
	if !s.outcomesOn() {
		return
	}
	s.agentOutcomes.open(a.ID, -1)
	if err != nil {
		if why, counts := turnFailureCounts(ctx, err); counts {
			s.outcomeFailed(ctx, a, why)
		}
		return
	}
	s.outcomeAnswered(ctx, a, reply)
}

// outcomeAnswered: a's turn ended with reply (possibly empty: a tool loop can
// end on a tool call, attention_ask among them).
//
// 🔴 AN IDLE REPLY IS NEVER "DONE". A turn that ends is the agent's question as
// often as its result — "Which repository should I work in?" — and reading it as
// finished stops watching an agent that is waiting on its owner: the lost-agent
// failure this tracker exists to end. Done is an EXPLICIT signal only (its task
// moved to review/complete, or agent_report_done). Every other idle end is
// WAITING: one push quoting the reply's LAST line (where a question is) and a
// chief note quoting its last lines.
func (s *Server) outcomeAnswered(ctx context.Context, a agents.Agent, reply string) {
	if !s.outcomesOn() {
		return
	}
	octx, cancel := outcomeCtx(ctx)
	defer cancel()
	now := s.now()
	if strings.TrimSpace(reply) != "" {
		if err := s.ext.Outcomes.Replied(octx, a.ID, now); err != nil {
			s.logger.Printf("outcomes: agent %d: record reply: %v", a.ID, err)
		}
	}
	// A failure is cleared by the next good turn (only the chief, whose status a
	// failure leaves alone, can have one).
	if cleared, err := s.ext.Outcomes.Clear(octx, a.ID, []string{agentoutcome.Failed}); err == nil && cleared {
		s.outcomeChanged(a)
		return
	}
	// A reply while its pod still reports subagents or background shells is a
	// progress report mid-work: it is not waiting yet (outcomeWorkReported).
	if s.agentOutcomes.busy(a.ID) || !s.AgentWork(a.ID).IsZero() {
		return
	}
	s.outcomeWaiting(octx, a, agentoutcome.LastLines(agents.ScrubNote(reply, a.PendingNote), 3, outcomePushTitleMax))
}

// outcomeWaiting moves a running (or stuck) agent to waiting on its owner, with
// the reply's last line as the reason. It TELLS nobody yet: the watchdog sweep
// does, after the waiting hold (outcomeTellWaiting).
//
// 🔴 A WAIT IS NOT TOLD AT ONCE. In a conversation every turn ends waiting on
// the owner, and pushing (and noting to the chief) at each one buzzed the owner
// for every reply they were sitting there reading. The tell waits the hold
// session state uses for a waiting window (insideGrace, Server.waitingGrace:
// 60 s) and is dropped if, meanwhile, the owner replies (the new turn re-arms
// the agent: no longer waiting) or opens its page (outcomeViewed).
func (s *Server) outcomeWaiting(octx context.Context, a agents.Agent, last []string) {
	reason := "its turn ended with no reply text"
	if len(last) > 0 {
		reason = last[len(last)-1]
	}
	applied, err := s.ext.Outcomes.Transition(octx, a.ID, []string{agentoutcome.Running, agentoutcome.Stuck},
		agentoutcome.Waiting, reason, s.now())
	if err != nil {
		s.logger.Printf("outcomes: agent %d: mark waiting: %v", a.ID, err)
		return
	}
	if !applied {
		return
	}
	s.logger.Printf("outcomes: agent %d (%s) is waiting for its owner", a.ID, a.Name)
	s.outcomeChanged(a)
}

// outcomeTellWaiting tells a's wait — once, past the waiting hold, if nobody
// has seen it: the push (unless session state is on: its own episode for the
// conversation already pushes "needs you", with the same hold) and ONE note in
// the chief's chat for the wait, quoting the last lines of the agent's newest
// reply in its latest conversation. Claimed by agentoutcome.Store.Told,
// conditional on the wait that began at a.OutcomeAt, so of two sweeps (two
// replicas) one tells, and a newer wait is not told by an older read.
func (s *Server) outcomeTellWaiting(octx context.Context, a agents.Agent, now time.Time) {
	if a.Outcome != agentoutcome.Waiting || a.OutcomeToldAt != nil || a.OutcomeAt == nil ||
		insideGrace(now.Sub(*a.OutcomeAt), s.waitingGrace) {
		return
	}
	applied, err := s.ext.Outcomes.Told(octx, a.ID, a.OutcomeAt, now)
	if err != nil {
		s.logger.Printf("outcomes: agent %d: tell waiting: %v", a.ID, err)
		return
	}
	if !applied {
		return
	}
	lines := []string{a.OutcomeReason}
	if sid := s.latestSessionIDs(octx, []int64{a.ID})[a.ID]; sid > 0 {
		if msgs, err := s.ext.Agents.ListRecentChatMessages(octx, sid, 1); err == nil && len(msgs) == 1 && msgs[0].Role == "assistant" {
			if l := agentoutcome.LastLines(agents.ScrubNote(msgs[0].Content, a.PendingNote), 3, outcomePushTitleMax); len(l) > 0 {
				lines = l
			}
		}
	}
	if !s.sessionStateOn() {
		s.pushOutcome(a, agentoutcome.Waiting, "is waiting for you", a.OutcomeReason)
	}
	s.noteOutcomeToChief(octx, a, agentoutcome.Waiting, "is waiting for your owner", strings.Join(lines, "\n"))
}

// outcomeViewed: the agent's owner opened its page. A wait not yet told is
// seen, and is never told (the push would be for something they looked at).
func (s *Server) outcomeViewed(ctx context.Context, a agents.Agent) {
	if !s.outcomesOn() || a.Outcome != agentoutcome.Waiting || requestOwner(ctx) != agentOwnerID(a) {
		return
	}
	octx, cancel := outcomeCtx(ctx)
	defer cancel()
	if _, err := s.ext.Outcomes.Told(octx, a.ID, nil, s.now()); err != nil {
		s.logger.Printf("outcomes: agent %d: seen: %v", a.ID, err)
	}
}

// outcomeDone moves a running (or stuck) agent to done, and — once — says so.
//
// push is false when another push already says it for the same transition (the
// task's ready-for-review push).
func (s *Server) outcomeDone(octx context.Context, a agents.Agent, verb, detail string, push bool) {
	applied, err := s.ext.Outcomes.Transition(octx, a.ID, []string{agentoutcome.Running, agentoutcome.Waiting, agentoutcome.Stuck},
		agentoutcome.Done, "", s.now())
	if err != nil {
		s.logger.Printf("outcomes: agent %d: mark done: %v", a.ID, err)
		return
	}
	if !applied {
		return
	}
	s.logger.Printf("outcomes: agent %d (%s) %s", a.ID, a.Name, verb)
	s.outcomeChanged(a)
	if push {
		s.pushOutcome(a, agentoutcome.Done, verb, detail)
	}
	s.noteOutcomeToChief(octx, a, agentoutcome.Done, verb, detail)
}

// outcomeFailed: a turn of a failed, for reason. Status `error` with the reason
// (not for the chief, see the header), outcome failed, and — once per failure —
// the push and the chief's note.
func (s *Server) outcomeFailed(ctx context.Context, a agents.Agent, reason string) {
	if !s.outcomesOn() {
		return
	}
	octx, cancel := outcomeCtx(ctx)
	defer cancel()
	reason = agents.ScrubNote(strings.TrimSpace(reason), a.PendingNote)
	chief := s.outcomeIsChief(octx, a)
	// 🔴 ONLY A DISPATCHED AGENT (one whose work is tracked) IS SET TO `error`.
	// An officer's instance, a saved agent Started by hand and other untracked
	// rows keep their status (their own subsystems judge them); the chief is
	// pushed but never set to error (the header).
	if a.Outcome == "" && !chief {
		return
	}
	applied, err := s.ext.Outcomes.Fail(octx, a.ID, reason, !chief, s.now())
	if err != nil {
		s.logger.Printf("outcomes: agent %d: record failure: %v", a.ID, err)
		return
	}
	s.logger.Printf("outcomes: agent %d (%s) failed: %s", a.ID, a.Name, reason)
	s.outcomeChanged(a)
	if applied {
		s.announceOutcome(octx, a, agentoutcome.Failed, "failed", reason)
	}
}

// outcomeStuck is the watchdog's verdict applied: a is stuck for reason. The
// caller's conditional write applied it; this says so, once.
func (s *Server) outcomeStuck(octx context.Context, a agents.Agent, reason string) {
	s.logger.Printf("outcomes: agent %d (%s) is stuck: %s", a.ID, a.Name, reason)
	s.outcomeChanged(a)
	s.announceOutcome(octx, a, agentoutcome.Stuck, "is stuck", reason)
}

// outcomeChanged is THE broadcast an outcome change sends: agent.changed for a,
// to its owner, so its page's strip and notices and the sidebar redraw.
func (s *Server) outcomeChanged(a agents.Agent) { s.agentChangedFor(a.OwnerID, a.Name) }

// outcomeIsChief is the chief predicate (chiefPredicate) over the whole table,
// read as System: whether a is its owner's chief.
func (s *Server) outcomeIsChief(octx context.Context, a agents.Agent) bool {
	if agents.IsChiefName(a.Name) {
		return true
	}
	list, err := s.ext.Agents.List(octx)
	if err != nil {
		// FAIL SAFE: an unreadable list answers "maybe the chief", so a failed
		// turn sets no `error` that could cut the chief off.
		s.logger.Printf("outcomes: agent %d: could not decide whether it is the chief (treated as maybe): %v", a.ID, err)
		return true
	}
	return chiefPredicate(list)(a)
}

// agentLabel is how a push and a note name an agent: its display name, else its slug.
func agentLabel(a agents.Agent) string {
	if l := strings.TrimSpace(a.DisplayName); l != "" {
		return l
	}
	return a.Name
}

// announceOutcome pushes a's new outcome to its owner and notes it in the chat
// of the chief that started it.
func (s *Server) announceOutcome(octx context.Context, a agents.Agent, outcome, verb, detail string) {
	s.pushOutcome(a, outcome, verb, detail)
	s.noteOutcomeToChief(octx, a, outcome, verb, detail)
}

// pushOutcome is the one push an outcome change sends: "<agent> <verb>: <detail>".
func (s *Server) pushOutcome(a agents.Agent, outcome, verb, detail string) {
	label := agentLabel(a)
	title := label + " " + verb
	if detail != "" {
		title += ": " + truncate(detail, outcomePushTitleMax)
	}
	body := detail
	if body == "" {
		body = "Open the agent to see what it did."
	}
	if outcome != agentoutcome.Done && outcome != agentoutcome.Waiting && a.DispatchedBy != nil {
		body += " Your chief has been told."
	}
	s.pushTask(a.OwnerID, push.EventAgentOutcome, RouterNotification{
		Type:  taskNotificationType,
		Title: title,
		Body:  body,
		Tag:   "agent-" + strconv.FormatInt(a.ID, 10) + "-outcome",
		// The bare page: it resolves the agent's latest session at tap time.
		Data: map[string]string{"url": ui.AgentPath(a.Name)},
	})
}

// outcomeChiefNote is the note a chief reads about an agent it started.
func outcomeChiefNote(a agents.Agent, outcome, verb, detail string) string {
	who := fmt.Sprintf("Agent %s (%q, id %d) that you started with agent_dispatch", a.Name, agentLabel(a), a.ID)
	switch outcome {
	case agentoutcome.Failed:
		return "[muster] " + who + " FAILED: " + detail + ". Its status is now error. Tell your owner what went wrong, " +
			"and offer to start it again with that fixed (agent_dispatch, with a new request_key)."
	case agentoutcome.Stuck:
		return "[muster] " + who + " looks STUCK: " + detail + ". Tell your owner, and offer to look at it or start it again."
	case agentoutcome.Waiting:
		return "[muster] " + who + " stopped and is WAITING FOR YOUR OWNER — it did not say it finished. Its last words:\n" +
			detail + "\nTell your owner it is waiting on them (its question, if it asked one, is above)."
	}
	line := "[muster] " + who + " " + verb
	if detail != "" {
		line += ": " + detail
	}
	return line + ". Tell your owner it is done."
}

// noteOutcomeToChief writes the outcome note into the default session of the
// chief that started a, when one did and it is still a's owner's agent.
func (s *Server) noteOutcomeToChief(octx context.Context, a agents.Agent, outcome, verb, detail string) {
	if a.DispatchedBy == nil || *a.DispatchedBy == a.ID {
		return
	}
	chief, err := s.ext.Agents.Get(octx, *a.DispatchedBy)
	if err != nil || chief.OwnerID != a.OwnerID {
		return // destroyed, or not the owner's: nobody to tell
	}
	sess, err := s.defaultSession(octx, chief)
	if err != nil {
		s.logger.Printf("outcomes: agent %d: the chief's session: %v", a.ID, err)
		return
	}
	if _, err := s.ext.Agents.AddChatMessage(octx, agents.ChatMessage{AgentID: chief.ID, SessionID: sess.ID, Role: "user",
		Content: outcomeChiefNote(a, outcome, verb, detail)}); err != nil {
		s.logger.Printf("outcomes: agent %d: note to the chief: %v", a.ID, err)
		return
	}
	s.broadcastTo(chief.OwnerID, EventChatReply, chief.Name)
}

// turnFailureCounts is whether a turn's error is the AGENT failing — and the
// reason, in the owner's words — rather than muster or the person moving on.
//
// It does NOT count:
//   - the caller going away (its context cancelled: a closed chat socket, a
//     shutdown) or the turn outliving its budget — the runtime may still be
//     running it, so it is the watchdog's to judge, as a shutdown is;
//   - a typed refusal ccd gives BEFORE it types anything (`503 not_ready`,
//     `409 busy`, `409 rate_limited`): nothing was sent, the agent is busy or
//     starting, and the sender is told;
//   - a connection that could not be OPENED (a pod restarting): nothing was
//     sent, and the instance's own state says why;
//   - a turn the gate refused (ErrAgentBeingReplaced).
//
// Everything else — the runtime's own failure (agents.StreamError, a non-200)
// — counts.
func turnFailureCounts(ctx context.Context, err error) (string, bool) {
	if err == nil || ctx.Err() != nil {
		return "", false
	}
	if errors.Is(err, ErrAgentBeingReplaced) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "", false
	}
	var rt *agents.RuntimeError
	if errors.As(err, &rt) && (rt.Status == http.StatusServiceUnavailable && rt.Type == "not_ready" ||
		rt.Status == http.StatusConflict && (rt.Type == "busy" || rt.Type == "rate_limited")) {
		return "", false
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return "", false
	}
	return "a turn failed: " + err.Error(), true
}

// ---------------------------------------------------------------------------
// THE OUTCOME API — what a live card reads (the chief panel's dispatched-agent
// cards through outcomeCardState; agentJSON's outcome fields on GET /api/agents).
// ---------------------------------------------------------------------------

// AgentOutcome is an agent's status and the outcome of what it was started on.
type AgentOutcome struct {
	AgentID     int64  `json:"agentId"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	// Status is the agent's stored status (pending|provisioning|running|stopped|error).
	Status string `json:"status"`
	// Outcome is agentoutcome's running|done|failed|stuck, or "starting" for an
	// agent whose first turn is still owed and that no outcome tracks yet, or ""
	// for an agent nobody started with a first turn.
	Outcome string `json:"outcome"`
	// Reason is why (a failure's error, the watchdog's verdict); "" otherwise.
	Reason string `json:"reason"`
	// Since is when Outcome last changed; null when it never has.
	Since *time.Time `json:"since"`
	// LastActivityAt is the newest evidence of life (agentoutcome.LastActivity).
	LastActivityAt time.Time `json:"lastActivityAt"`
	// DispatchedBy is the chief that started it; null for any other start.
	DispatchedBy *int64 `json:"dispatchedBy"`
}

// OutcomeStarting is AgentOutcome.Outcome for an untracked agent whose first
// turn is still owed.
const OutcomeStarting = "starting"

// AgentOutcomeOf projects a row the caller already holds (a card's): no read.
func (s *Server) AgentOutcomeOf(a agents.Agent, lastMessage time.Time) AgentOutcome {
	out := AgentOutcome{
		AgentID: a.ID, Name: a.Name, DisplayName: a.DisplayName, Status: a.Status,
		Outcome: a.Outcome, Reason: agents.ScrubNote(a.OutcomeReason, a.PendingNote),
		LastActivityAt: agentoutcome.LastActivity(agentoutcome.Input{Agent: a, LastMessage: lastMessage,
			Working: !s.AgentWork(a.ID).IsZero()}, s.now()),
	}
	if out.Outcome == "" && agents.KickoffOwed(a) {
		out.Outcome = OutcomeStarting
	}
	if a.OutcomeAt != nil {
		t := *a.OutcomeAt
		out.Since = &t
	}
	if a.DispatchedBy != nil {
		id := *a.DispatchedBy
		out.DispatchedBy = &id
	}
	return out
}

// outcomeWorkReported: a's pod posted a work report — a heartbeat, so activity.
// cleared is a report that took its work to none: an agent that already
// answered and has no turn open is then waiting on its owner.
func (s *Server) outcomeWorkReported(ctx context.Context, a agents.Agent, cleared bool) {
	if !s.outcomesOn() {
		return
	}
	s.outcomeActivity(ctx, a)
	if !cleared || s.agentOutcomes.busy(a.ID) {
		return
	}
	octx, cancel := outcomeCtx(ctx)
	defer cancel()
	fresh, err := s.ext.Agents.Get(octx, a.ID)
	if err != nil || fresh.FirstReplyAt == nil {
		return
	}
	// Its background work ended and no turn is open: idle, and it did not say it
	// finished — waiting on its owner (never done: outcomeAnswered's rule).
	s.outcomeWaiting(octx, fresh, []string{"its background work ended"})
}

// outcomeTaskDone: actor (an agent's name, from the writer's refOrigin) moved
// taskID to review or complete. When actor IS the task's own agent its work is
// done, whatever it is doing next, and it is moved to done.
//
// 🔴 ONLY THE TASK'S OWN AGENT. task_set_status is also the chief's (every
// task) and a mirrored session's; one of them moving an agent's task used to
// be read as that agent finishing — done, credited to it, no push, and its next
// question swallowed because a done agent is not waiting. Anyone else's move
// leaves the agent's outcome alone: it is still judged by its own turns.
func (s *Server) outcomeTaskDone(ctx context.Context, taskID int64, status, actor string) {
	if !s.outcomesOn() {
		return
	}
	octx, cancel := outcomeCtx(ctx)
	defer cancel()
	byNote, err := s.ext.Agents.AgentsByNoteIDs(octx, []int64{taskID})
	if err != nil {
		s.logger.Printf("outcomes: task %d's agents: %v", taskID, err)
		return
	}
	if a, ok := byNote[taskID]; ok && actor != "" && a.Name == actor {
		// A move to ready_for_review already pushes the owner (notifyTaskDone,
		// the same funnel): no second push for one transition. complete pushes.
		s.outcomeDone(octx, a, "moved its task to "+strings.ReplaceAll(status, "_", " "), "", status != notes.StatusReadyForReview)
	}
}

// outcomeRestarted: the owner Started agent id again. Whatever its last work
// ended in (failed, stuck) is over; it is untracked until it is given new work.
func (s *Server) outcomeRestarted(ctx context.Context, id int64) {
	if !s.outcomesOn() {
		return
	}
	octx, cancel := outcomeCtx(ctx)
	defer cancel()
	if _, err := s.ext.Outcomes.Clear(octx, id, []string{agentoutcome.Failed, agentoutcome.Stuck}); err != nil {
		s.logger.Printf("outcomes: agent %d: clear on start: %v", id, err)
	}
}

// KickoffOutcomes is the tracker as internal/agentkickoff's OutcomeReporter
// (cmd/muster-server wires it), and false when nothing is tracked.
func (s *Server) KickoffOutcomes() (*KickoffOutcomeReporter, bool) {
	if s == nil || !s.outcomesOn() {
		return nil, false
	}
	return &KickoffOutcomeReporter{s: s}, true
}

// KickoffOutcomeReporter feeds a first turn's course to the tracker.
type KickoffOutcomeReporter struct{ s *Server }

// TurnStarted: the first turn is about to be sent.
func (k *KickoffOutcomeReporter) TurnStarted(ctx context.Context, a agents.Agent) {
	k.s.outcomeTurnStarted(ctx, a)
}

// TurnActivity: the first turn's stream showed life.
func (k *KickoffOutcomeReporter) TurnActivity(ctx context.Context, a agents.Agent) {
	k.s.outcomeActivity(ctx, a)
}

// TurnEnded: the first turn is no longer open (agentkickoff runs it once per
// TurnStarted, on every path). Closing is all it does.
func (k *KickoffOutcomeReporter) TurnEnded(_ context.Context, a agents.Agent) {
	if k.s.outcomesOn() {
		k.s.agentOutcomes.open(a.ID, -1)
	}
}

// TurnAnswered: the first turn answered (it is already closed: TurnEnded).
func (k *KickoffOutcomeReporter) TurnAnswered(ctx context.Context, a agents.Agent, reply string) {
	k.s.outcomeAnswered(ctx, a, reply)
}

// TurnFailed: the first turn failed (closed already: TurnEnded), or was never
// delivered (it opened none). It never touches the open-turn count — guessing
// "this agent's turn is open" from busy() would close another door's turn.
func (k *KickoffOutcomeReporter) TurnFailed(ctx context.Context, a agents.Agent, reason string) {
	k.s.outcomeFailed(ctx, a, reason)
}

// OutcomeHold holds a turn of a open for the outcome watchdog — the agent is
// working, never "silent" — and judges NOTHING: a scheduled job's turn (its
// reply is the job's result, not the agent waiting on its owner) and a memo
// relay turn (internal/memorelay Config.TurnHold). It neither records activity
// nor re-arms a waiting agent. release closes it, exactly once; a no-op when no
// outcome is tracked.
func (s *Server) OutcomeHold(a agents.Agent) (release func()) {
	if s == nil || !s.outcomesOn() {
		return func() {}
	}
	s.agentOutcomes.open(a.ID, 1)
	var once sync.Once
	return func() { once.Do(func() { s.agentOutcomes.open(a.ID, -1) }) }
}
