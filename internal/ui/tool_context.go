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

// ctxBadge is a tool row's context slot, always hidden: its settled cost as
// data (kind and tokens), or the empty slot.
//
// 🔴 NOT DRAWN (the operator rule: no token counters in the top layer —
// "~+140 ctx" means nothing to someone who is not an engineer). The slot
// carries only the measurement's kind and tokens, which the ledger's tests
// read; no text node, no tooltip.
func (t *MirrorTranscript) ctxBadge(toolID string) g.Node {
	id := ctxBadgeID(toolRowID(toolID))
	c, ok := t.costs[toolID]
	if !ok {
		return Span(ID(id), g.Attr("data-tool-ctx", ""), g.Attr("hidden", ""))
	}
	return Span(ID(id),
		g.Attr("data-tool-ctx", c.Kind),
		g.Attr("data-tool-ctx-tokens", strconv.FormatInt(c.Tokens, 10)),
		g.Attr("hidden", ""))
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
