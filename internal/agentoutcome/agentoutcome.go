// Package agentoutcome decides how a started agent's work ENDED: done, failed
// or stuck — never silently "running" (docs/provisioner.md, "How an agent's work ends").
//
// 🔴 THE DEFECT IT CLOSES, MEASURED. The chief dispatched a gateway agent with a
// model DISPLAY NAME where an id belonged. The runtime's first turn failed
// ("Unknown model"), and so did every later turn. muster kept the agent
// `running` with an empty error_message and last_output and one chat row (the
// kickoff it sent); nothing pushed and nothing told the chief. The owner found it
// hours later by looking. Four things were missing, and each has one home:
//
//   - a model nobody can run is refused before a pod exists
//     (internal/api model_validate.go);
//   - a failed turn sets status `error` with its reason, pushes once and tells
//     the chief that started it (internal/api agent_outcome.go, fed by
//     internal/agentkickoff for the first turn and by chatTurn for the rest);
//   - an agent that goes quiet is called stuck by a sweep ([Decide], run by
//     internal/api agent_watchdog.go);
//   - an agent that finishes says so ([Done]) — only on an explicit signal —
//     and one that stops without saying so is [Waiting] on its owner.
//
// THE STATES (agents.outcome, migration 0088). "" is UNTRACKED — an agent
// nobody started with a first turn, and every row from before 0088.
//
//	running  started with a first turn; working (or about to)
//	waiting  its turn ended, idle, with no completion signal (a question, a
//	         progress report): the owner's move — pushed once, with its last line
//	done     it SAID it finished: moved its task to review/complete, or called
//	         agent_report_done. 🔴 NEVER inferred from an idle reply: a question
//	         read as "done" is the lost-agent failure this package exists to end
//	failed   a turn failed; the reason is in outcome_reason and error_message
//	stuck    no reply since its kickoff, or silent past the threshold
//
// 🔴 ONE PUSH PER CHANGE, BY CONSTRUCTION RATHER THAN BY A LEDGER. Every
// transition is a write conditional on the outcome it leaves (Store.Transition's
// `from`), so of two writers racing one change — two replicas, a turn ending
// while the sweep runs — exactly one applies it, and only the one whose write
// applied pushes. A repeated failure of a `failed` agent applies nothing and
// pushes nothing.
//
// 🔴 IT IS NOT A SIXTH agents.status. The five statuses key the reconcile table
// and every consumer; `stuck` is an outcome BESIDE a status, the shape
// agents.KickoffOwed already has, because a stuck agent's pod is still running
// and its recovery is new activity, not a restart.
package agentoutcome

import (
	"fmt"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// The outcomes (the agents_outcome_check constraint's closed set, minus "").
const (
	Running = "running"
	// Waiting: the agent's turn ended and it is idle WITHOUT an explicit
	// completion signal — it asked something, or reported progress. The owner's
	// move; the watchdog does not judge it, and its next turn re-arms running.
	Waiting = "waiting"
	Done    = "done"
	Failed  = "failed"
	Stuck   = "stuck"
)

// The thresholds' defaults (operator decision): no first reply within five
// minutes of the kickoff, or no activity for thirty once it has replied.
const (
	DefaultNoReplyAfter = 5 * time.Minute
	DefaultSilentAfter  = 30 * time.Minute
	// DefaultInterval is how often the watchdog sweeps. A verdict lands at most
	// this long after its threshold passed.
	DefaultInterval = time.Minute
)

// MinThreshold bounds a configured threshold or interval from below: anything
// shorter is a typo, not a policy (a browser test sets seconds).
const MinThreshold = time.Second

// ReasonNoReply is the stuck reason before the agent's first reply.
const ReasonNoReply = "no reply since kickoff"

// SilentReason is the stuck reason after it: "silent for 30 min".
func SilentReason(d time.Duration) string { return "silent for " + HumanDuration(d) }

// HumanDuration says d the way the reasons do: whole minutes as "30 min", whole
// hours as "2 h", anything else as Go spells it ("45s").
func HumanDuration(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%d h", d/time.Hour)
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%d min", d/time.Minute)
	}
	return d.String()
}

// Thresholds is the watchdog's configuration (MUSTER_AGENT_STUCK_NO_REPLY,
// MUSTER_AGENT_STUCK_SILENT, MUSTER_AGENT_WATCHDOG_INTERVAL).
type Thresholds struct {
	// NoReply is how long a started agent may go without its first reply AND
	// without any activity before it is stuck.
	NoReply time.Duration
	// Silent is how long an agent that has replied, and is still working, may go
	// without activity before it is stuck.
	Silent time.Duration
}

// DefaultThresholds is the operator's defaults.
func DefaultThresholds() Thresholds {
	return Thresholds{NoReply: DefaultNoReplyAfter, Silent: DefaultSilentAfter}
}

// ParseThreshold reads one threshold variable: "" is def; otherwise a Go
// duration of at least MinThreshold.
func ParseThreshold(v string, def time.Duration) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration (e.g. 5m, 30m)", v)
	}
	if d < MinThreshold {
		return 0, fmt.Errorf("%q is under %s", v, MinThreshold)
	}
	return d, nil
}

// Action is what the watchdog does with one agent.
type Action int

const (
	// None: nothing changes.
	None Action = iota
	// MarkStuck: running → stuck, with Verdict.Reason.
	MarkStuck
	// Recover: stuck → running (new activity since it was called stuck).
	Recover
)

// Verdict is [Decide]'s answer.
type Verdict struct {
	Action Action
	Reason string
	// Last is the activity the verdict was taken on (the newest of the evidence).
	Last time.Time
}

// Input is everything [Decide] reads about one agent.
type Input struct {
	Agent agents.Agent
	// LastMessage is the agent's newest transcript row (zero when none):
	// agents.LastActivity's second half.
	LastMessage time.Time
	// Working is whether the agent is visibly working NOW: a turn of it is in
	// flight (a claude-code turn streams nothing until it ends), or its pod
	// reports subagents or background shells (refreshed every minute).
	Working bool
}

// LastActivity is the newest evidence that the agent is alive: agents.LastActivity
// (its row and its transcript — THE definition the sidebar and GET /api/agents
// already use) and the activity muster observed itself (agents.activity_at: turn
// starts, ends and stream events, pod work reports).
//
// 🔴 ONE DEFINITION, REUSED, NOT A SECOND ONE. The watchdog asks "when was it last
// active" and agents.LastActivity is where muster answers that; this only adds the
// half that the transcript cannot see — a long tool run streams events and
// heartbeats for an hour while writing no transcript row until it ends.
func LastActivity(in Input, now time.Time) time.Time {
	last := agents.LastActivity(in.Agent, in.LastMessage)
	if t := in.Agent.ActivityAt; t != nil && t.After(last) {
		last = *t
	}
	if in.Working && now.After(last) {
		last = now
	}
	return last
}

// Decide is THE stuck predicate: whether one tracked agent is stuck now, or has
// recovered. Pure; the watchdog applies what it answers.
//
//   - Only an agent whose outcome is running or stuck is judged; done, failed and
//     untracked are left alone.
//   - Only a live agent whose first turn was delivered: before the kickoff is
//     stamped, internal/agentkickoff owns the verdict (its dwell bound ends an
//     undelivered first turn in `error`), and a stopped or errored agent is not
//     quiet, it is stopped or failed.
//   - Before its first reply, NoReply of no activity is stuck ("no reply since
//     kickoff"); after it, Silent of no activity is ("silent for 30 min").
//     Activity is [LastActivity]: a working long tool run streams events and
//     heartbeats, so it is never flipped.
//   - A stuck agent with activity after the verdict recovers.
func Decide(in Input, now time.Time, th Thresholds) Verdict {
	a := in.Agent
	if a.Outcome != Running && a.Outcome != Stuck {
		return Verdict{}
	}
	if a.Status == agents.StatusError || a.Status == agents.StatusStopped || !a.KickedOff {
		return Verdict{}
	}
	last := LastActivity(in, now)
	if a.Outcome == Stuck {
		if a.OutcomeAt != nil && last.After(*a.OutcomeAt) {
			return Verdict{Action: Recover, Last: last}
		}
		return Verdict{Last: last}
	}
	quiet := now.Sub(last)
	if a.FirstReplyAt == nil {
		if quiet >= th.NoReply {
			return Verdict{Action: MarkStuck, Reason: ReasonNoReply, Last: last}
		}
		return Verdict{Last: last}
	}
	if quiet >= th.Silent {
		return Verdict{Action: MarkStuck, Reason: SilentReason(th.Silent), Last: last}
	}
	return Verdict{Last: last}
}

// FirstLine is a reply's first non-empty line, cut to max runes with an
// ellipsis: what the "finished" push says.
func FirstLine(s string, max int) string {
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			r := []rune(t)
			if max > 0 && len(r) > max {
				return string(r[:max-1]) + "…"
			}
			return t
		}
	}
	return ""
}

// LastLines is a reply's last n non-empty lines, each cut to max runes: what a
// "waiting for you" push and note quote — the end of a turn is where its
// question is, not its first line.
func LastLines(s string, n, max int) []string {
	var out []string
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			if r := []rune(t); max > 0 && len(r) > max {
				t = string(r[:max-1]) + "…"
			}
			out = append([]string{t}, out...)
		}
	}
	return out
}
