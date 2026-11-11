// Package chiefusage is muster's record of what the owner's CHIEF costs and
// what it is spared (migration 0107; docs/usage.md):
//
//   - one row per chief turn (chief_turn_usage): what started it (Trigger),
//     what it did (Outcome, from the tools it called) and the tokens its
//     runtime reported for it — input, output, cache read, cache creation;
//   - one row per inbound plugin message (plugin_message_handling): who
//     handled it — one of muster's own deterministic paths, an approved
//     routing rule, or the chief.
//
// The usage page (internal/api usage.go) reads both through Report.
//
// 🔴 THE TOKENS ARE THE RUNTIME'S OWN NUMBERS, NEVER AN ESTIMATE. Every chief
// runtime answers muster's agent wire (internal/agents responses.go), whose
// response.completed carries the turn's `usage`: ccd sums the Claude Code
// transcript's per-message usage for the turn (cmd/ccd transcript.go) — the
// same numbers whether the CLI reaches Anthropic directly or OpenRouter
// through muster's LLM proxy — and a gateway runtime sends the Responses
// API's own. A turn whose runtime sent none is recorded with usage_known
// false and zero tokens, and the page counts it apart, never as free.
//
// 🔴 OWNED AND UNDER ROW-LEVEL SECURITY: ctx acts as the owner whose chief it
// is, and every read is that owner's alone.
package chiefusage

import (
	"context"
	"strings"
	"time"
)

// Trigger kinds: what started a chief turn (chief_turn_usage.trigger).
const (
	TriggerPlugin    = "plugin"     // an inbound plugin message delivered to the chief
	TriggerTaskEvent = "task_event" // a task muster handed the chief (a plugin's task notice)
	TriggerOwnerChat = "owner_chat" // a line the owner typed in the chief's chat
	TriggerView      = "view"       // a line the owner typed on a session's view
	TriggerAnswer    = "answer"     // the owner's answer to the chief's question, delivered
	TriggerHandBack  = "hand_back"  // a route handed back to the chief
	// TriggerSessionQuestion: one of the owner's mirrored sessions asked the
	// chief a question (chief_ask), delivered as a turn.
	TriggerSessionQuestion = "session_question"
	// TriggerMachine: the machine message door (POST /api/agents/{name}/messages).
	TriggerMachine = "machine"
	// TriggerKickoff: the chief's first turn, handed over by the kickoff deliverer.
	TriggerKickoff = "kickoff"
	TriggerOther   = "other" // anything else (a nudge, a relay, a door this package does not know)
)

// Trigger is what started a chief turn: its kind and, for a plugin message or
// a task event, the plugin, the sender and the message.
type Trigger struct {
	Kind      string
	Plugin    string
	Sender    string
	MessageID int64
}

type triggerKey struct{}

// WithTrigger marks ctx as a turn started by t. The door that starts the turn
// sets it (the plugin deliverer, the owner's chat doors, the answer delivery);
// the turn's recorder reads it (TriggerFrom).
func WithTrigger(ctx context.Context, t Trigger) context.Context {
	return context.WithValue(ctx, triggerKey{}, t)
}

// TriggerFrom is ctx's trigger, TriggerOther when no door set one.
func TriggerFrom(ctx context.Context) Trigger {
	t, ok := ctx.Value(triggerKey{}).(Trigger)
	if !ok || t.Kind == "" {
		return Trigger{Kind: TriggerOther}
	}
	return t
}

// Outcomes: what a chief turn did (chief_turn_usage.outcome).
const (
	OutcomeRouted       = "routed"
	OutcomeSettled      = "settled"
	OutcomeAsk          = "ask"
	OutcomeTask         = "task"
	OutcomeRuleProposed = "rule_proposed"
	OutcomeOther        = "other"
	OutcomeFailed       = "failed"
)

// RuleProposeTool is the chief's tool that proposes a routing rule
// (internal/api routing_rules.go): named here because the outcome reads it.
const RuleProposeTool = "routing_rule_propose"

// toolOutcomes maps the chief's tools (bare names) to the outcome they mean.
var toolOutcomes = map[string]string{
	"route_to_session":      OutcomeRouted,
	"plugin_message_settle": OutcomeSettled,
	"ask_owner":             OutcomeAsk,
	"attention_ask":         OutcomeAsk,
	"task_route_attention":  OutcomeAsk,
	"task_create_gated":     OutcomeTask,
	"task_forward":          OutcomeTask,
	"agent_dispatch":        OutcomeTask,
	RuleProposeTool:         OutcomeRuleProposed,
}

// outcomeRank orders outcomes when a turn did several things: the one with the
// lower rank is the turn's outcome (a turn that routed a message and proposed
// a rule for it was a route).
var outcomeRank = map[string]int{OutcomeRouted: 0, OutcomeTask: 1, OutcomeAsk: 2, OutcomeSettled: 3, OutcomeRuleProposed: 4, OutcomeOther: 5}

// mcpPrefix is how a Claude Code chief names muster's tools (its MCP server
// is called muster).
const mcpPrefix = "mcp__muster__"

// OutcomeOf is the outcome of a turn that called tools (by name, in either
// spelling: bare, or mcp__muster__-prefixed) and failed or not, and whether
// one of its calls proposed a rule.
func OutcomeOf(tools []string, failed bool) (string, bool) {
	if failed {
		return OutcomeFailed, hasRuleProposal(tools)
	}
	out := OutcomeOther
	proposed := false
	for _, t := range tools {
		o, ok := toolOutcomes[strings.TrimPrefix(t, mcpPrefix)]
		if !ok {
			continue
		}
		if o == OutcomeRuleProposed {
			proposed = true
		}
		if outcomeRank[o] < outcomeRank[out] {
			out = o
		}
	}
	return out, proposed
}

func hasRuleProposal(tools []string) bool {
	for _, t := range tools {
		if strings.TrimPrefix(t, mcpPrefix) == RuleProposeTool {
			return true
		}
	}
	return false
}

// Tokens is a turn's token counts, as its runtime reported them.
type Tokens struct {
	Input, Output, CacheRead, CacheCreation int64
}

// Total is every token the turn moved: input, output, cache read and cache
// creation. ⚠ A COUNT, NOT A COST: a cache read is billed far below an input
// token, so two turns with equal totals can cost very different amounts. The
// page shows the four apart for that reason.
func (t Tokens) Total() int64 { return t.Input + t.Output + t.CacheRead + t.CacheCreation }

// Add is t plus u.
func (t Tokens) Add(u Tokens) Tokens {
	return Tokens{Input: t.Input + u.Input, Output: t.Output + u.Output, CacheRead: t.CacheRead + u.CacheRead,
		CacheCreation: t.CacheCreation + u.CacheCreation}
}

// Turn is one chief turn as recorded.
type Turn struct {
	Chief        int64
	Trigger      Trigger
	Outcome      string
	RuleProposed bool
	Tokens       Tokens
	// Known: the runtime reported usage for the turn (else Tokens is zero and
	// means "unknown", not "free").
	Known    bool
	Duration time.Duration
}

// Handlers: who handled an inbound plugin message
// (plugin_message_handling.handler).
const (
	HandlerFleetReport      = "fleet_report"
	HandlerThread           = "thread"
	HandlerClosedPR         = "closed_pr"
	HandlerPRRoute          = "pr_route"
	HandlerMailResume       = "mail_resume"
	HandlerTaskEventMention = "task_event_mention"
	// The task triage (internal/api task_triage.go): an assignment muster
	// placed itself — forwarded to the agent on the task, routed to the one
	// session on it, or put on the owner's board.
	HandlerTaskForward = "task_forward"
	HandlerTaskRoute   = "task_route"
	HandlerTaskBoard   = "task_board"
	// HandlerTaskClaimed: the triage left an assignment of a CLAIMED task
	// alone (notes.Claims): no card, no turn.
	HandlerTaskClaimed = "task_claimed"
	HandlerRule        = "rule"
	HandlerChief       = "chief"
)

// MusterHandlers are the handlers that are muster's own deterministic paths
// (neither a rule nor the chief).
var MusterHandlers = []string{HandlerFleetReport, HandlerThread, HandlerClosedPR, HandlerPRRoute, HandlerMailResume, HandlerTaskEventMention,
	HandlerTaskForward, HandlerTaskRoute, HandlerTaskBoard, HandlerTaskClaimed}

// Handling is who handled one inbound plugin message. RuleID is set exactly
// for HandlerRule; FallbackRuleID for HandlerChief when a rule matched and its
// session could not take the message.
type Handling struct {
	MessageID      int64
	Plugin, Sender string
	Handler        string
	RuleID         int64
	FallbackRuleID int64
}

// Store records turns and handlings and reads them back as a Report.
type Store interface {
	RecordTurn(ctx context.Context, t Turn) error
	RecordHandling(ctx context.Context, h Handling) error
	Report(ctx context.Context, since time.Time) (Report, error)
}
