package api

import (
	"context"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/chiefusage"
	"github.com/ZacxDev/muster/internal/plugins"
)

// ---------------------------------------------------------------------------
// THE CHIEF'S TOKEN TELEMETRY (internal/chiefusage; docs/usage.md): one
// chief_turn_usage row per chief turn — what started it, what it did and the
// tokens its runtime reported — and one plugin_message_handling row per
// inbound plugin message naming who handled it. The usage page (usage.go)
// reads both.
//
// 🔴 ONE RECORDER PER TURN, IN chatTurn: every door's chief turn passes it (the
// plugin deliverer, the owner's chat and view doors, an answer's delivery, a
// hand-back), so no door can run a chief turn the page does not count. The
// door only says what started the turn (chiefusage.WithTrigger).
// ---------------------------------------------------------------------------

// chiefTurnUsage is one chief turn's recorder: the runtime's usage sink, the
// tools the turn called and when it began.
type chiefTurnUsage struct {
	s       *Server
	chief   agents.Agent
	trigger chiefusage.Trigger
	usage   *agents.TurnUsage
	start   time.Time

	mu    sync.Mutex
	tools []string
}

// beginChiefTurnUsage starts the recorder for a's turn under ctx: nil (and ctx
// unchanged) when nothing records — no telemetry store, or a is not its
// owner's chief.
func (s *Server) beginChiefTurnUsage(ctx context.Context, a agents.Agent) (context.Context, *chiefTurnUsage) {
	if s.ext.ChiefUsage == nil || !s.isChiefAgent(ctx, a) {
		return ctx, nil
	}
	ctx, sink := agents.WithTurnUsage(ctx)
	return ctx, &chiefTurnUsage{s: s, chief: a, trigger: chiefusage.TriggerFrom(ctx), usage: sink, start: time.Now()}
}

// observe is emit, noting every tool the turn calls.
func (u *chiefTurnUsage) observe(emit agents.StreamEmit) agents.StreamEmit {
	return func(ev agents.StreamEvent) {
		if ev.Kind == "tool_call" && ev.ToolName != "" {
			u.mu.Lock()
			u.tools = append(u.tools, ev.ToolName)
			u.mu.Unlock()
		}
		if emit != nil {
			emit(ev)
		}
	}
}

// finish records the turn, which ended with err (nil for an answer). A turn
// the runtime refused as busy (another holds the session) ran nothing and is
// not recorded: the wait sends it again, and that send is the turn.
func (u *chiefTurnUsage) finish(ctx context.Context, err error) {
	if _, busy := agentgateway.TurnInFlight(err); busy {
		return
	}
	in, out, read, create, known := u.usage.Snapshot()
	u.mu.Lock()
	tools := append([]string(nil), u.tools...)
	u.mu.Unlock()
	outcome, proposed := chiefusage.OutcomeOf(tools, err != nil)
	t := chiefusage.Turn{Chief: u.chief.ID, Trigger: u.trigger, Outcome: outcome, RuleProposed: proposed, Known: known,
		Tokens:   chiefusage.Tokens{Input: in, Output: out, CacheRead: read, CacheCreation: create},
		Duration: time.Since(u.start)}
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if rerr := u.s.ext.ChiefUsage.RecordTurn(bctx, t); rerr != nil {
		u.s.logger.Printf("chief usage: record %s's turn (%s): %v", u.chief.Name, u.trigger.Kind, rerr)
	}
}

// recordHandling records that handler handled plugin message m of plugin p
// (ruleID for a rule's hit, fallbackRuleID for a hand-off to the chief after a
// rule could not route it). Nothing without the telemetry store.
func (s *Server) recordHandling(ctx context.Context, p plugins.Plugin, m plugins.Message, handler string, ruleID, fallbackRuleID int64) {
	if s.ext.ChiefUsage == nil || m.ID <= 0 {
		return
	}
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	h := chiefusage.Handling{MessageID: m.ID, Plugin: p.Name, Sender: m.Sender, Handler: handler, RuleID: ruleID, FallbackRuleID: fallbackRuleID}
	if err := s.ext.ChiefUsage.RecordHandling(bctx, h); err != nil {
		s.logger.Printf("chief usage: record message %d's handler %s: %v", m.ID, handler, err)
	}
}

// turnTriggerFor is what started a turn entered by runEnteredTurn: a line
// the owner typed on a session's view (about set), one they typed in the
// chat, or — typed by nobody — a route's hand-back (runUserTurn's one caller).
func turnTriggerFor(typedBy int64, about *chatSubject) chiefusage.Trigger {
	switch {
	case typedBy > 0 && about != nil:
		return chiefusage.Trigger{Kind: chiefusage.TriggerView}
	case typedBy > 0:
		return chiefusage.Trigger{Kind: chiefusage.TriggerOwnerChat}
	}
	return chiefusage.Trigger{Kind: chiefusage.TriggerHandBack}
}
