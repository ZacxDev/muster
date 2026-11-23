package ui

import (
	"html"
	"math"
	"strconv"
)

// ---------------------------------------------------------------------------
// THE PROGRESS BAR: one horizontal bar of how much of something is used,
// with its words beside it. The officer page's spend bar is its first user
// (internal/api officerRunHTML: "$X of $Y today").
//
// It is a role="progressbar" with its value in words (aria-valuetext), so a
// screen reader reads "$8.00 of $10 today", not "80". The fill's width is an
// inline style (the app's CSP allows style-src 'unsafe-inline'); everything
// else is a class written HERE, where the stylesheet scans.
//
// The TONE is the caller's decision, not the bar's arithmetic: the bar draws
// what it is told (ProgressOK, ProgressWarn, ProgressFull), so a rule such as
// "amber from 80%" lives in exactly one place, the caller's.
// ---------------------------------------------------------------------------

// ProgressTone is how a progress bar is coloured.
type ProgressTone string

const (
	// ProgressOK is the plain bar: plenty left.
	ProgressOK ProgressTone = "ok"
	// ProgressWarn is the amber bar: getting close.
	ProgressWarn ProgressTone = "warn"
	// ProgressFull is the red bar: used up (for the spend bar, held).
	ProgressFull ProgressTone = "full"
)

// ProgressBar is one bar.
type ProgressBar struct {
	// ID is the bar's id ("" for none).
	ID string
	// Label is its accessible name ("Spent today").
	Label string
	// Value and Max are the amounts; the fill is Value/Max, clamped to
	// 0..100%. Max <= 0 draws an empty bar.
	Value, Max float64
	// ValueText is the amount in words, read for the bar ("$8.00 of $10 today").
	ValueText string
	// Tone colours it ("" is ProgressOK).
	Tone ProgressTone
}

// The bar's classes. The track is full width and 10px tall, rounded; the fill
// takes the tone's colour.
const (
	ProgressTrackClass = "relative h-2.5 w-full overflow-hidden rounded-full bg-s2 ring-1 ring-inset ring-line"
	progressFillBase   = "absolute inset-y-0 left-0 rounded-full transition-[width]"
	ProgressFillOK     = progressFillBase + " bg-accent"
	ProgressFillWarn   = progressFillBase + " bg-st-warning-fg"
	ProgressFillFull   = progressFillBase + " bg-st-error-fg"
)

// ProgressPercent is the bar's fill in whole percent, clamped to 0..100.
func ProgressPercent(value, max float64) int {
	if max <= 0 || value <= 0 || math.IsNaN(value) {
		return 0
	}
	p := int(math.Floor(value / max * 100))
	if p > 100 {
		return 100
	}
	return p
}

// ProgressBarHTML draws p.
func ProgressBarHTML(p ProgressBar) string {
	esc := html.EscapeString
	tone := p.Tone
	fill := ProgressFillOK
	switch tone {
	case ProgressWarn:
		fill = ProgressFillWarn
	case ProgressFull:
		fill = ProgressFillFull
	default:
		tone = ProgressOK
	}
	pct := strconv.Itoa(ProgressPercent(p.Value, p.Max))
	id := ""
	if p.ID != "" {
		id = ` id="` + esc(p.ID) + `"`
	}
	return `<div` + id + ` role="progressbar" aria-label="` + esc(p.Label) + `" aria-valuemin="0" aria-valuemax="100" aria-valuenow="` + pct +
		`" aria-valuetext="` + esc(p.ValueText) + `" data-progress-tone="` + string(tone) + `" class="` + ProgressTrackClass + `">` +
		`<div class="` + fill + `" style="width:` + pct + `%" data-progress-fill></div></div>`
}
