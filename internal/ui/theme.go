package ui

import (
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// ---------------------------------------------------------------------------
// THE PALETTE'S GO HALF.
//
// The palette itself ("Brass Roll") is defined ONCE, as CSS custom properties in
// web/css/input.css, and every view names a ROLE (`bg-s1`, `text-accent`) rather
// than a hue. Two values cannot be CSS, because the browser reads them before
// any stylesheet exists: the `theme-color` meta that tints the phone's status
// bar, and the web app manifest's `theme_color` / `background_color`. Those two
// come from the constants below, and nowhere else.
//
// 🔴 THESE ARE THE ONLY COLOUR LITERALS ALLOWED IN internal/ui, AND THEY ARE NOT
// FREE-STANDING. TestNoHardCodedColourInUI exempts this file and nothing else,
// and TestThemeColorsAreTheBackgroundTokens parses input.css and fails unless
// each constant equals that theme's `--mu-bg`. So the status bar cannot drift
// from the page it sits on: change the token and this file must follow.
// ---------------------------------------------------------------------------

// ThemeColorDark is the dark theme's background (`--mu-bg` on `.dark`). It is
// the default theme, so it is also the manifest's theme_color and
// background_color (internal/api serves the manifest from these values).
const ThemeColorDark = "#110F1C"

// ThemeColorLight is the light theme's background (`--mu-bg` on
// `:root:not(.dark)`).
//
// ⚠ NOTHING RENDERS IT ANY MORE: the light theme is unreachable (the sidebar's
// toggle is gone and ThemeHead forces dark). The light tokens in input.css and
// their contrast tests stay until a later cleanup removes the light palette as
// a whole; this constant stays with them so TestThemeColorsAreTheBackgroundTokens
// still pins it to its token.
const ThemeColorLight = "#F6F4FB"

// themeStorageKey is the localStorage key the removed light-theme toggle wrote
// ("light"). ThemeHead's script deletes it, so a device that opted into light
// before the toggle went loads dark from then on.
const themeStorageKey = "muster-theme"

// ThemeHead is the document head's theme block: the theme-color meta that tints
// the phone's status bar, and the script that keeps the document dark.
//
// 🔴 DARK IS THE ONLY THEME. The `dark` class is server-rendered on every
// <html>; the script re-asserts it and deletes the opt-in the old toggle stored
// (themeStorageKey), so a device that had chosen light loads dark instead of
// keeping a preference nothing can change back. Storage access is in a
// try/catch: a private window or blocked storage must still render, not throw
// before the page has a body.
//
// ⚠ THE SCRIPT IS SYNCHRONOUS ON PURPOSE. It is a parser-blocking <script src>
// in the head (static_script.go), so it runs before the body is parsed; the
// sign-in page carries it inline (ThemeHeadHTML). There is no `storage`
// listener any more: it existed so one document followed a theme another (the
// view-swap frame, another tab) had set, and nothing sets one now.
func ThemeHead() g.Node {
	return g.Group{
		Meta(Name("theme-color"), Content(ThemeColorDark)),
		staticScript(themeHeadSrc),
	}
}

// themeHeadSrc is the path themeHeadScript is served at, /static/js/theme-head.js.
var themeHeadSrc = staticJS("theme-head", themeHeadScript)

// themeHeadScript forces the dark theme and clears the stored light opt-in. It
// is exported to internal/api's sign-in page through ThemeHeadHTML so the one
// document emitted outside this package behaves the same.
const themeHeadScript = `(function () {
  document.documentElement.classList.add('dark');
  try { localStorage.removeItem('` + themeStorageKey + `'); } catch (e) {}
})();`

// ThemeHeadHTML is ThemeHead as a raw string, for the sign-in page, which is a
// hand-written document in internal/api rather than a gomponents tree.
func ThemeHeadHTML() string {
	return `<meta name="theme-color" content="` + ThemeColorDark + `">` + "\n" +
		`<script>` + themeHeadScript + `</script>`
}

// ---------------------------------------------------------------------------
// STATUS GLYPHS — SHAPE AND COLOUR, NEVER COLOUR ALONE.
// ---------------------------------------------------------------------------

// Glyph kinds. These are the palette's status ROLES, not any one enum: a task
// status, an agent status and a kickoff badge each map onto them (see
// taskGlyphKind, agentGlyphKind).
const (
	glyphOpen     = "open"
	glyphProgress = "progress"
	glyphReview   = "review"
	glyphComplete = "complete"
	glyphError    = "error"
	glyphWarning  = "warning"
	glyphRunning  = "running"
)

// glyphKinds is the closed set, in display order. TestEveryGlyphKindHasAShape
// renders each one and fails on an empty or duplicated shape.
var glyphKinds = []string{glyphOpen, glyphProgress, glyphReview, glyphComplete, glyphError, glyphWarning, glyphRunning}

// glyphText is each kind's foreground utility. Spelled out literally rather
// than concatenated: Tailwind emits only classes it can SEE in the source, so a
// "text-st-"+kind+"-fg" built at runtime would be a class with no CSS.
var glyphText = map[string]string{
	glyphOpen:     "text-st-open-fg",
	glyphProgress: "text-st-progress-fg",
	glyphReview:   "text-st-review-fg",
	glyphComplete: "text-st-complete-fg",
	glyphError:    "text-st-error-fg",
	glyphWarning:  "text-st-warning-fg",
	glyphRunning:  "text-st-running-fg",
}

// glyphChip is each kind's chip: its own fill, its label colour, and a hairline
// in the label colour. A chip's glyph knocks out in the chip fill (glyphKnock).
var glyphChip = map[string]string{
	glyphOpen:     "bg-st-open-bg text-st-open-fg ring-st-open-fg/40",
	glyphProgress: "bg-st-progress-bg text-st-progress-fg ring-st-progress-fg/40",
	glyphReview:   "bg-st-review-bg text-st-review-fg ring-st-review-fg/40",
	glyphComplete: "bg-st-complete-bg text-st-complete-fg ring-st-complete-fg/40",
	glyphError:    "bg-st-error-bg text-st-error-fg ring-st-error-fg/40",
	glyphWarning:  "bg-st-warning-bg text-st-warning-fg ring-st-warning-fg/40",
	glyphRunning:  "bg-st-running-bg text-st-running-fg ring-st-running-fg/40",
}

// glyphSelect is the task status <select>'s fill + label per kind: the chip
// colours without the chip's hairline (the select draws its own control edge).
var glyphSelect = map[string]string{
	glyphOpen:     "bg-st-open-bg text-st-open-fg",
	glyphProgress: "bg-st-progress-bg text-st-progress-fg",
	glyphReview:   "bg-st-review-bg text-st-review-fg",
	glyphComplete: "bg-st-complete-bg text-st-complete-fg",
	glyphError:    "bg-st-error-bg text-st-error-fg",
	glyphWarning:  "bg-st-warning-bg text-st-warning-fg",
	glyphRunning:  "bg-st-running-bg text-st-running-fg",
}

// glyphKnockVar is the custom property a chip sets so its glyph's knock-out
// strokes match the chip fill rather than the card behind it.
var glyphKnockVar = map[string]string{
	glyphOpen:     "--glyph-knock: rgb(var(--mu-st-open-bg))",
	glyphProgress: "--glyph-knock: rgb(var(--mu-st-progress-bg))",
	glyphReview:   "--glyph-knock: rgb(var(--mu-st-review-bg))",
	glyphComplete: "--glyph-knock: rgb(var(--mu-st-complete-bg))",
	glyphError:    "--glyph-knock: rgb(var(--mu-st-error-bg))",
	glyphWarning:  "--glyph-knock: rgb(var(--mu-st-warning-bg))",
	glyphRunning:  "--glyph-knock: rgb(var(--mu-st-running-bg))",
}

// glyphKnock is the style attribute for an element whose glyphs sit on kind's
// chip fill.
func glyphKnock(kind string) g.Node { return g.Attr("style", glyphKnockVar[normGlyph(kind)]) }

func normGlyph(kind string) string {
	if _, ok := glyphText[kind]; ok {
		return kind
	}
	return glyphOpen
}

// glyphSVG is each kind's shape, on a 16-unit grid. The shapes differ by
// OUTLINE as well as fill (ring, half-ring, diamond, disc+tick, octagon+cross,
// triangle+bar, solid dot), so a status is legible to someone who cannot tell
// the colours apart — WCAG 1.4.1, "use of colour".
var glyphSVG = map[string]string{
	glyphOpen:     `<circle cx="8" cy="8" r="5.5" fill="none" stroke="currentColor" stroke-width="2"/>`,
	glyphProgress: `<circle cx="8" cy="8" r="5.5" fill="none" stroke="currentColor" stroke-width="2"/><path d="M8 2.5a5.5 5.5 0 0 1 0 11z" fill="currentColor"/>`,
	glyphReview:   `<path d="M8 1.5 14.5 8 8 14.5 1.5 8z" fill="currentColor"/>`,
	glyphComplete: `<circle cx="8" cy="8" r="7" fill="currentColor"/><path d="m4.8 8.2 2.2 2.2 4.2-4.6" fill="none" style="stroke:var(--glyph-knock)" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"/>`,
	glyphError:    `<path d="M5.2 1.5h5.6l3.7 3.7v5.6l-3.7 3.7H5.2l-3.7-3.7V5.2z" fill="currentColor"/><path d="m5.8 5.8 4.4 4.4m0-4.4-4.4 4.4" style="stroke:var(--glyph-knock)" stroke-width="1.8" stroke-linecap="round"/>`,
	glyphWarning:  `<path d="M8 1.5 15 14H1z" fill="currentColor"/><path d="M8 6v3.6" style="stroke:var(--glyph-knock)" stroke-width="1.8" stroke-linecap="round"/><circle cx="8" cy="11.8" r="1" style="fill:var(--glyph-knock)"/>`,
	glyphRunning:  `<circle cx="8" cy="8" r="4.5" fill="currentColor"/>`,
}

// statusGlyph renders kind's glyph at the given Tailwind size (e.g. "h-3 w-3").
// It is decorative (aria-hidden): the caller always renders the status WORD, or
// an aria-label carrying it, beside the glyph.
//
// pulse adds the expanding halo used for in-flight states; it is a separate
// element so prefers-reduced-motion (motion-safe:) can drop it alone.
func statusGlyph(kind, size string, pulse bool) g.Node {
	kind = normGlyph(kind)
	return Span(
		g.Attr("data-glyph", kind),
		g.Attr("aria-hidden", "true"),
		Class("relative inline-flex shrink-0 "+size+" "+glyphText[kind]),
		g.If(pulse, Span(Class("absolute inset-0 rounded-full bg-current opacity-50 motion-safe:animate-ping"))),
		g.Raw(`<svg class="relative h-full w-full" viewBox="0 0 16 16" focusable="false">`+glyphSVG[kind]+`</svg>`),
	)
}

// statusChip is a pill carrying kind's glyph and a visible label, on kind's own
// fill. Shape + word + colour: the colour is the least load-bearing of the three.
func statusChip(kind, label string, extra ...g.Node) g.Node {
	kind = normGlyph(kind)
	return Span(
		g.Attr("data-status-chip", kind),
		glyphKnock(kind),
		Class("inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 text-xs font-semibold ring-1 ring-inset "+glyphChip[kind]),
		statusGlyph(kind, "h-3 w-3", false),
		g.Group(extra),
		g.Text(label),
	)
}

// taskGlyphKind maps a task status (the stored enum) onto a glyph kind.
func taskGlyphKind(status string) string {
	switch status {
	case "in_progress":
		return glyphProgress
	case "ready_for_review":
		return glyphReview
	case "complete":
		return glyphComplete
	default:
		return glyphOpen
	}
}

// agentGlyphKind maps an agent's stored lifecycle status onto a glyph kind.
// provisioning is work in flight (half ring), running is the live dot, a
// stopped agent has finished (tick), pending is queued (empty ring).
func agentGlyphKind(status string) string {
	switch status {
	case "running":
		return glyphRunning
	case "provisioning":
		return glyphProgress
	case "error":
		return glyphError
	case "stopped":
		return glyphComplete
	default:
		return glyphOpen
	}
}

// toastGlyphJS is the error glyph's markup as a JavaScript string literal, for
// appScript's toast. It is rendered from statusGlyph rather than re-typed so the
// toast's shape is the palette's error shape by construction.
func toastGlyphJS() string {
	var b strings.Builder
	_ = Span(Class("mt-0.5 inline-flex"), statusGlyph(glyphError, "h-3.5 w-3.5", false)).Render(&b)
	// On the danger fill the glyph is drawn in on-danger (the toast's own text
	// colour), knocked out in danger; the chip colour would vanish into the fill.
	return jsonString(strings.Replace(b.String(), glyphText[glyphError], "text-on-danger", 1))
}
