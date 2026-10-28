package cctranscript

import "math"

// negligibleInjected is the attachment types (KindInjected's Text) that do NOT
// spoil a measurement: small things the CLI adds before most requests. Any
// other type — and a marker with no type, from a host that predates it —
// makes the gap not clean.
//
// 🔴 DECIDED BY cmd/muster-toolctx-calibrate, NOT BY HAND. It measures each
// type's EXCESS — growth − output − result/ResultBytesPerToken over the gaps
// holding it and otherwise only negligible types — and calls a type negligible
// when at least 30 such gaps have a median within 50 tokens of the baseline
// (gaps with no attachment) and a p75 within 100. It prints whether this set
// matches its own; docs/mirroring.md has the table and the numbers. Over the
// local set it was run on, against a baseline of −1: total_tokens_reminder
// (median excess 27, p75 43), hook_permission_decision (29, 46: what a
// PermissionRequest hook writes), prompt_snapshot (30, 80; n=176) and
// credential_org (20, 40; n=120). Not negligible: silent_turn_reminder
// (median 57), deferred_tools_record (median 36 but p75 438),
// read_truncation_notice (median −1164: far below), every context-bearing
// type (diagnostics 401, queued_command 450, …), and hook_success (too few gaps
// of its own to judge). A measured badge's tooltip says it may include these
// small additions.
var negligibleInjected = map[string]bool{
	"total_tokens_reminder":    true,
	"hook_permission_decision": true,
	"prompt_snapshot":          true,
	"credential_org":           true,
}

// NegligibleInjected reports whether attachment type t leaves a gap clean
// (negligibleInjected).
func NegligibleInjected(t string) bool { return negligibleInjected[t] }

// ResultBytesPerToken is the calibrated size-to-tokens rate of a tool result:
// how many bytes of result text make one token of context.
//
// CALIBRATED, NOT ASSUMED (cmd/muster-toolctx-calibrate; docs/mirroring.md has
// the method and the numbers). Over clean single-call gaps of a local
// transcript set — aggregates only — the measured growth minus the issuing
// turn's output_tokens was never negative, so the turn's output re-enters the
// context in full and the REST is the result's. Fitted on half the transcripts
// (split by a hash of the path) as the median of resultBytes / (growth −
// output) over results of 400 B or more: 2.16 (p25 1.94, p75 2.40, n=27366),
// used as 2.2. On the held-out half, |estimate − measured| / measured fell
// from a median of 0.61 (p90 0.78) for the old flat 4 bytes per token over the
// call and result alone to 0.05 (p90 0.12). The fit is Claude Code's alone:
// another runtime or model tokenizes differently, unmeasured.
const ResultBytesPerToken = 2.2

// EstimateTokens is a call's estimated context cost. With the issuing turn's
// output known (outputKnown), it is that output (output: its output_tokens,
// or this call's share of them when the turn made several calls — a share
// that may round to 0) plus the result's tokens at ResultBytesPerToken: the
// split the calibration found. With no output known (a runtime that records
// no usage, an older host), the call's input and result are sized at the same
// rate instead — a rate calibrated on Claude Code transcripts only, so for
// another runtime or model the figure is unvalidated in either direction.
func EstimateTokens(output, in, out int64, outputKnown bool) int64 {
	if outputKnown {
		return max(output, 0) + int64(math.Ceil(float64(out)/ResultBytesPerToken))
	}
	return int64(math.Ceil(float64(in+out) / ResultBytesPerToken))
}

// TOOL-CALL CONTEXT: how many tokens of conversation context each tool call
// added — its call plus its result — for the rendered view's "+3.2k ctx" badge.
//
// MEASURED WHERE THE TRANSCRIPT ALLOWS, ESTIMATED ONLY WHERE IT DOES NOT, AND
// NEVER ONE PASSED OFF AS THE OTHER (ToolCost.Kind):
//
//   - CostMeasured: the turn that issued the call reported its context (Usage)
//     and so did the NEXT turn, with nothing between them but the turn's own
//     tool results (and the small reminders negligibleInjected names). The growth, next − issuing, is what the call and its result
//     added — together with whatever else the issuing turn wrote (its text, its
//     thinking: one output_tokens figure covers them all, and the transcript does
//     not split it). That is the number shown.
//   - CostSplit: the issuing turn made SEVERAL calls (parallel). The growth is
//     measured once for all of them; each call is shown a share of it,
//     apportioned by the bytes of its call and result. The SHARE is not
//     measured, so it is marked approximate; the group total it came from is.
//   - CostEstimate: no usage to measure by — a runtime that records none
//     (Hermes), a host that predates Usage (WhyNoUsage) — or a gap the growth
//     cannot be put down to the calls alone: a prompt, a notice, an error or
//     context the CLI INJECTED (an attachment record: a message typed while
//     the tool ran, diagnostics, a skill listing…) arrived before the next turn
//     (WhyInterrupted), or the context SHRANK, a compaction (WhyShrank).
//     Estimated as EstimateTokens: the issuing turn's output_tokens (its share
//     by input size, for several calls) plus the result's size (Event.Bytes,
//     where an image counts by its pixels: record.go's ResultBytes) at the
//     calibrated ResultBytesPerToken. With no output known, the call and
//     result sizes alone, which reads low.
//
// A call whose result never arrives, whose next turn has not happened yet, or
// whose size is unknown and usage absent (opencode) gets no cost at all: an
// absent badge, not a guess.

// The ToolCost kinds.
const (
	CostMeasured = "measured"
	CostSplit    = "split"
	CostEstimate = "estimate"
)

// ToolCost is one call's context cost, once it is known.
type ToolCost struct {
	ToolID string
	// Tokens is the call's cost: measured, a share of a measured total, or an
	// estimate (Kind).
	Tokens int64
	Kind   string
	// GroupTokens and GroupSize are the measured total a CostSplit share came
	// from and how many calls it covers (0 for the other kinds).
	GroupTokens int64
	GroupSize   int
	// Why is a CostEstimate's reason (WhyNoUsage, WhyInterrupted, WhyShrank).
	// OutputKnown says the issuing turn's output_tokens were known and counted
	// — Output is this call's share of them, which can round to 0 — else the
	// estimate is sizes alone.
	Why         string
	OutputKnown bool
	Output      int64
}

// Why an estimate is one.
const (
	WhyNoUsage     = "no-usage"
	WhyInterrupted = "interrupted"
	WhyShrank      = "shrank"
)

// ToolContext attributes context growth to tool calls, fed every event of a
// transcript in order (Observe). In memory and bounded: it holds only the
// calls of the latest turn.
type ToolContext struct {
	// base is the latest assistant turn's context (0: none seen yet), and
	// baseOut its output_tokens (0: not known).
	base, baseOut int64
	// pend is the calls issued since base, in order.
	pend []pendingCall
	// results says a result for one of pend arrived since base; clean that
	// nothing else (a prompt, a notice, an error, context the CLI injected)
	// did.
	results bool
	clean   bool
	// Negligible decides which attachment types leave a gap clean
	// (KindInjected's Text); nil is NegligibleInjected. The calibration
	// command sets its own, to measure every type.
	Negligible func(attachmentType string) bool
}

type pendingCall struct {
	id        string
	in, out   int64
	hasResult bool
}

// Observe records ev and returns the costs it settled, if any.
func (c *ToolContext) Observe(ev Event) []ToolCost {
	var out []ToolCost
	if ev.Usage != nil && ev.Usage.Tokens > 0 {
		u := ev.Usage.Tokens
		switch {
		case c.results && u != c.base:
			// The next turn after the calls' results: settle them. (The same
			// figure again is still the issuing message: a CLI that runs a call
			// while the message is streaming can write its result between two of
			// the message's records.)
			out = c.settle(u)
			c.base, c.baseOut, c.pend, c.results, c.clean = u, ev.Usage.Output, nil, false, true
		case u != c.base:
			// A new turn with no tool result before it: the calls still pending
			// (none answered) are dropped — no result, no cost.
			c.base, c.baseOut, c.pend, c.clean = u, ev.Usage.Output, nil, true
		}
		// u == base with no result since: another record of the same
		// message (one block per record, each with the message's usage).
	}
	switch ev.Kind {
	case KindToolCall:
		if ev.ToolID == "" {
			break
		}
		for _, p := range c.pend {
			if p.id == ev.ToolID {
				return out // the same call again
			}
		}
		c.pend = append(c.pend, pendingCall{id: ev.ToolID, in: eventBytes(ev, ev.Input)})
	case KindToolResult:
		i := c.find(ev.ToolID)
		if i < 0 {
			// Its call is not this ledger's (before the window, or drawn by an
			// earlier reader): nothing to attribute.
			break
		}
		c.pend[i].out, c.pend[i].hasResult = eventBytes(ev, ev.Output), true
		if c.base == 0 {
			// No usage to wait for: estimate now.
			p := c.pend[i]
			c.pend = append(c.pend[:i:i], c.pend[i+1:]...)
			if e, ok := estimate(p, 0, false, WhyNoUsage); ok {
				out = append(out, e)
			}
			break
		}
		c.results = true
	case KindInjected:
		neg := NegligibleInjected
		if c.Negligible != nil {
			neg = c.Negligible
		}
		if !neg(ev.Text) {
			c.clean = false
		}
	case KindPrompt, KindNotice, KindError:
		c.clean = false
	}
	return out
}

func (c *ToolContext) find(id string) int {
	for i, p := range c.pend {
		if p.id == id {
			return i
		}
	}
	return -1
}

// settle is the costs of the answered pending calls, now that the next turn's
// context is next.
func (c *ToolContext) settle(next int64) []ToolCost {
	var done []pendingCall
	for _, p := range c.pend {
		if p.hasResult {
			done = append(done, p)
		}
	}
	if len(done) == 0 {
		return nil
	}
	out := make([]ToolCost, 0, len(done))
	delta := next - c.base
	if !c.clean || delta <= 0 {
		why := WhyInterrupted
		if c.clean {
			why = WhyShrank
		}
		// The turn's output, shared between its calls by the size of each
		// call's input (equally when none has a size); the last takes the
		// remainder.
		var in int64
		for _, p := range done {
			in += p.in
		}
		var given int64
		for i, p := range done {
			share := c.baseOut / int64(len(done))
			if in > 0 {
				share = c.baseOut * p.in / in
			}
			if i == len(done)-1 {
				share = c.baseOut - given
			}
			given += share
			if e, ok := estimate(p, share, c.baseOut > 0, why); ok {
				out = append(out, e)
			}
		}
		return out
	}
	if len(done) == 1 {
		return append(out, ToolCost{ToolID: done[0].id, Tokens: delta, Kind: CostMeasured})
	}
	// Apportion by size; the last call takes the remainder, so the shares add
	// up to the measured total exactly.
	var w int64
	for _, p := range done {
		w += p.in + p.out
	}
	var given int64
	for i, p := range done {
		share := delta / int64(len(done))
		if w > 0 {
			share = delta * (p.in + p.out) / w
		}
		if i == len(done)-1 {
			share = delta - given
		}
		given += share
		out = append(out, ToolCost{ToolID: p.id, Tokens: share, Kind: CostSplit, GroupTokens: delta, GroupSize: len(done)})
	}
	return out
}

// estimate is p's cost from its size — none when its size is not known at
// all (an opencode call: muster reads neither its input text nor its output),
// since "~+0" would claim the call cost nothing.
func estimate(p pendingCall, output int64, known bool, why string) (ToolCost, bool) {
	if p.in+p.out == 0 {
		return ToolCost{}, false
	}
	if !known {
		output = 0
	}
	return ToolCost{ToolID: p.id, Tokens: EstimateTokens(output, p.in, p.out, known), Kind: CostEstimate, Why: why, OutputKnown: known, Output: max(output, 0)}, true
}

// Clone is an independent copy of the ledger: the state a reconnecting
// socket resumes with (internal/api's ledger checkpoints).
func (c *ToolContext) Clone() ToolContext {
	d := *c
	d.pend = append([]pendingCall(nil), c.pend...)
	return d
}

// eventBytes is an event's uncapped size (Event.Bytes), or the capped text's
// for a host that predates it.
func eventBytes(ev Event, capped string) int64 {
	if ev.Bytes > 0 {
		return int64(ev.Bytes)
	}
	return int64(len(capped))
}
