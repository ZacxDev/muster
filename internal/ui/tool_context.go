package ui

import (
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/cctranscript"
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE TOOL ROW'S CONTEXT BADGE: how much conversation context the call and its
// result added, on the mirrored view's tool rows ("+3.3k ctx").
//
// The number is cctranscript.ToolContext's, and so is the honesty rule: a
// MEASURED figure is shown bare; a share of a measured total (parallel calls)
// and a size estimate (no usage recorded) are both marked "~", and the title
// says which one it is and how it was got. A call whose cost is not known yet —
// still running, or its next turn not written — draws an empty, hidden slot
// that a later batch fills in place (a "patch"), so the row is never redrawn
// for it and an expanded row stays open.
//
// 🔴 NO LAYOUT SHIFT: the badge is inline in the row's one-line summary, which
// already holds its 44px height; it takes the right-hand end (ml-auto) beside
// the status glyph (web/css/input.css drops the glyph's own ml-auto when a
// badge is shown), and the target, which truncates, gives up the width.

// ctxBadgeID is a row's badge slot id.
func ctxBadgeID(rowID string) string { return rowID + "-x" }

// observeCost feeds ev to the ledger and returns a patch for each row whose
// cost it settled — except the row ev itself is about to redraw, which draws
// its badge from t.costs.
//
// A RESUMED socket (Resumed) badges the rows an EARLIER socket drew too: its
// ledger was restored from that socket's checkpoint (RestoreLedger), so a call
// still running across the reconnect settles here, and the row — which this
// socket never drew — is patched where the page has it (a patch of a slot the
// page lacks does nothing). Such a call is tracked like any other, so its cost
// is bounded with the rest and a later redraw of its card keeps the badge.
func (t *MirrorTranscript) observeCost(ev cctranscript.Event) []MirrorOp {
	var ops []MirrorOp
	for _, c := range t.ledger.Observe(ev) {
		if t.costs == nil {
			t.costs = map[string]cctranscript.ToolCost{}
		}
		_, drawn := t.calls[c.ToolID]
		if !drawn && !t.Resumed {
			continue
		}
		if !drawn {
			t.track(c.ToolID)
		}
		t.costs[c.ToolID] = c
		if drawn && ev.Kind == cctranscript.KindToolResult && ev.ToolID == c.ToolID {
			continue
		}
		ops = append(ops, MirrorOp{Op: "patch", ID: ctxBadgeID(toolRowID(c.ToolID)), HTML: render(t.ctxBadge(c.ToolID))})
	}
	return ops
}

// Ledger is a copy of the tool-context ledger as Ops has left it: a
// checkpoint a later socket resuming from the same place restores
// (RestoreLedger), so a call still running across a reconnect is still
// attributed when its next turn arrives.
func (t *MirrorTranscript) Ledger() cctranscript.ToolContext { return t.ledger.Clone() }

// RestoreLedger resumes the ledger from a checkpoint (Ledger) taken at the
// very place this socket resumes from.
func (t *MirrorTranscript) RestoreLedger(l cctranscript.ToolContext) { t.ledger = l.Clone() }

// ctxBadge is a tool row's badge: its settled cost, or the hidden empty slot.
func (t *MirrorTranscript) ctxBadge(toolID string) g.Node {
	id := ctxBadgeID(toolRowID(toolID))
	c, ok := t.costs[toolID]
	if !ok {
		return Span(ID(id), g.Attr("data-tool-ctx", ""), g.Attr("hidden", ""))
	}
	text, title := "+"+ctxCount(c.Tokens)+" ctx", ""
	switch c.Kind {
	case cctranscript.CostMeasured:
		title = "Measured: the context grew " + groupDigits(c.Tokens) +
			" tokens from the turn that made this call to the next one (the call, its result, anything else that turn wrote, and any small reminder the CLI adds)"
	case cctranscript.CostSplit:
		text = "~" + text
		title = "About " + groupDigits(c.Tokens) + " tokens: a share of " + groupDigits(c.GroupTokens) +
			" measured for " + strconv.Itoa(c.GroupSize) + " parallel calls, split by the size of each call and result (an image by its pixels)"
	default:
		text = "~" + text
		why := "no usage was recorded to measure it by"
		switch c.Why {
		case cctranscript.WhyInterrupted:
			why = "something else entered the context before the next turn (a message, a notice, or context the CLI injected), so the growth is not this call's alone"
		case cctranscript.WhyShrank:
			why = "the context shrank before the next turn (a compaction), so the growth cannot be measured"
		}
		rate := "about " + strconv.FormatFloat(cctranscript.ResultBytesPerToken, 'f', -1, 64) +
			" bytes per token (an image by its pixels), a rate calibrated against measured calls"
		if c.Output > 0 {
			title = "Estimated: this call's share of its turn's output (" + groupDigits(c.Output) + " tokens) plus its result at " + rate
		} else {
			title = "Estimated from the call and result size at " + rate + "; the turn's own output is not known, so it reads low"
		}
		title += ". " + strings.ToUpper(why[:1]) + why[1:]
	}
	return Span(ID(id),
		g.Attr("data-tool-ctx", c.Kind),
		g.Attr("data-tool-ctx-tokens", strconv.FormatInt(c.Tokens, 10)),
		g.Attr("title", title),
		Class("ml-auto shrink-0 whitespace-nowrap font-mono text-[10px] tabular-nums text-muted"),
		g.Text(text))
}

// ctxCount is a badge's token count, short and precise enough to compare
// calls by: 950, 3.3k, 12k, 1.2M.
func ctxCount(n int64) string {
	switch {
	case n < 1000:
		return strconv.FormatInt(n, 10)
	case n < 9950: // past it, one decimal would print "10.0k"
		return strings.TrimSuffix(strconv.FormatFloat(float64(n)/1000, 'f', 1, 64), ".0") + "k"
	default:
		return tokenCount(n)
	}
}

// groupDigits is n with thousands separators: 6,000.
func groupDigits(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
