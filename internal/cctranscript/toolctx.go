package cctranscript

// TOOL-CALL CONTEXT: how many tokens of conversation context each tool call
// added — its call plus its result — for the rendered view's "+3.2k ctx" badge.
//
// MEASURED WHERE THE TRANSCRIPT ALLOWS, ESTIMATED ONLY WHERE IT DOES NOT, AND
// NEVER ONE PASSED OFF AS THE OTHER (ToolCost.Kind):
//
//   - CostMeasured: the turn that issued the call reported its context (Usage)
//     and so did the NEXT turn, with nothing between them but the turn's own
//     tool results. The growth, next − issuing, is what the call and its result
//     added — together with whatever else the issuing turn wrote (its text, its
//     thinking: one output_tokens figure covers them all, and the transcript does
//     not split it). That is the number shown.
//   - CostSplit: the issuing turn made SEVERAL calls (parallel). The growth is
//     measured once for all of them; each call is shown a share of it,
//     apportioned by the bytes of its call and result. The SHARE is not
//     measured, so it is marked approximate; the group total it came from is.
//   - CostEstimate: no usage to measure by — a runtime that records none
//     (Hermes), a host that predates Usage, or a sequence the growth cannot be
//     attributed to the calls alone (a prompt or a notice arrived before the
//     next turn; the context SHRANK, a compaction). Estimated from the call's
//     and result's size at EstimateBytesPerToken.
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

// EstimateBytesPerToken is the size-to-tokens rate an estimate uses: the usual
// rough figure for English text and code. It is a rule of thumb, which is why
// an estimate is always marked as one.
const EstimateBytesPerToken = 4

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
}

// ToolContext attributes context growth to tool calls, fed every event of a
// transcript in order (Observe). In memory and bounded: it holds only the
// calls of the latest turn.
type ToolContext struct {
	// base is the latest assistant turn's context (0: none seen yet).
	base int64
	// pend is the calls issued since base, in order.
	pend []pendingCall
	// results says a result for one of pend arrived since base; clean that
	// nothing else (a prompt, a notice, an error) did.
	results bool
	clean   bool
	// measured is the sum of every measured cost settled so far (shares
	// included: they add up to their measured total).
	measured int64
}

type pendingCall struct {
	id        string
	in, out   int64
	hasResult bool
}

// Measured is the total context the tool calls settled so far were measured
// to add (CostMeasured and CostSplit; never an estimate).
func (c *ToolContext) Measured() int64 { return c.measured }

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
			c.base, c.pend, c.results, c.clean = u, nil, false, true
		case u != c.base:
			// A new turn with no tool result before it: the calls still pending
			// (none answered) are dropped — no result, no cost.
			c.base, c.pend, c.clean = u, nil, true
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
			if e, ok := estimate(p); ok {
				out = append(out, e)
			}
			break
		}
		c.results = true
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
		for _, p := range done {
			if e, ok := estimate(p); ok {
				out = append(out, e)
			}
		}
		return out
	}
	c.measured += delta
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
func estimate(p pendingCall) (ToolCost, bool) {
	if p.in+p.out == 0 {
		return ToolCost{}, false
	}
	n := (p.in + p.out + EstimateBytesPerToken - 1) / EstimateBytesPerToken
	return ToolCost{ToolID: p.id, Tokens: n, Kind: CostEstimate}, true
}

// eventBytes is an event's uncapped size (Event.Bytes), or the capped text's
// for a host that predates it.
func eventBytes(ev Event, capped string) int64 {
	if ev.Bytes > 0 {
		return int64(ev.Bytes)
	}
	return int64(len(capped))
}
