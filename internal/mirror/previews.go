package mirror

// LIVE PREVIEWS PER WINDOW (docs/previews.md, "What you do"): how many of a
// mirrored window's web servers the host's preview side (internal/previewhost)
// OFFERS right now, and how many previews of it are OPEN. muster draws the
// session views' Preview button and the sidebar row's preview mark from it,
// and only while a window has at least one of either.
//
// 🔴 COUNTS, NOTHING ELSE: no port, no process name, no preview id. What a
// window serves is the gateway's session page's to show; muster only needs to
// know whether there is anything to show there.
//
// 🔴 IT TRAVELS ON A MESSAGE OF ITS OWN (TypePreviews), NOT ON Window, exactly
// as the work counts do (work.go): the window list's worst case already fills
// 98% of MaxFrame (TestTheLargestWindowListFitsOneFrame). It is the WHOLE map
// of window id → counts, sent after the window list whenever it changed; an
// empty map clears it. Its worst case is measured by
// TestTheLargestPreviewsMessageFitsOneFrame.
//
// 🔴 NO VERSION BUMP, BACKWARD-COMPATIBLE BOTH WAYS: an older muster's read
// loop has no case for the type and drops the message; an older host (or one
// without -preview) never sends one, and its windows have no previews — no
// button, no mark.
//
// 🔴 ABSENT MEANS "NOTHING TO SHOW", and it is never kept for an offline host:
// a count is a claim about NOW (HostView.Previews).

// TypePreviews is the host → muster message carrying Msg.Previews.
const TypePreviews = "previews"

// MaxPreviewCount bounds each count. The host's own limits are far below it
// (maxOpen ≤ 32; the allowlist's listeners in one window); it only bounds what
// a host can make muster draw.
const MaxPreviewCount = 99

// WindowPreviews is what one window has to preview now: Offered is its
// listeners the host offers that have no open preview (the gateway's session
// page lists the same set: an open port is shown as its preview, not again as
// an offer), Open its open previews.
type WindowPreviews struct {
	Offered int `json:"offered,omitempty"`
	Open    int `json:"open,omitempty"`
}

// IsZero: nothing to show.
func (p WindowPreviews) IsZero() bool { return p.Offered == 0 && p.Open == 0 }

// Total is how many things the window's session page lists: its offers and
// its open previews.
func (p WindowPreviews) Total() int { return p.Offered + p.Open }

// NormalizePreviews is in restricted to windows (a host cannot report previews
// for a window it does not list), each count clamped to [0, MaxPreviewCount],
// entries with nothing to show dropped; nil when nothing is left. Both halves
// apply it: the host to what it sends, muster to what it keeps. windows must
// be NormalizeWindows' output, so the result has at most MaxWindows entries.
func NormalizePreviews(in map[string]WindowPreviews, windows []Window) map[string]WindowPreviews {
	if len(in) == 0 {
		return nil
	}
	clamp := func(n int) int { return min(max(n, 0), MaxPreviewCount) }
	var out map[string]WindowPreviews
	for _, w := range windows {
		v, ok := in[w.ID]
		if !ok {
			continue
		}
		v = WindowPreviews{Offered: clamp(v.Offered), Open: clamp(v.Open)}
		if v.IsZero() {
			continue
		}
		if out == nil {
			out = map[string]WindowPreviews{}
		}
		out[w.ID] = v
	}
	return out
}
