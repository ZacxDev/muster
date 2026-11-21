package uikit

import (
	"sort"
	"strconv"
	"strings"
)

// Icons is the sprite's drawings: name → the inner markup of a 24-unit
// <symbol>, stroked in currentColor by the .mu-icon class (round caps and
// joins, width 2). A filled shape says fill="currentColor" itself.
//
// 🔴 v1 IS ADDITIVE ONLY. A name here is a promise to every plugin that
// draws it (docs/ui-kit.md#icons): never remove or rename one, and never draw
// it as something else. TestIconsSVGCarriesTheContract fails when one listed
// in the contract is missing.
var Icons = map[string]string{
	"mic":               `<rect x="9" y="3" width="6" height="11" rx="3"/><path d="M5 11a7 7 0 0 0 14 0M12 18v3M8 21h8"/>`,
	"mic-off":           `<path d="M15 9.3V6a3 3 0 0 0-5.7-1.3M9 9v2a3 3 0 0 0 5.1 2.1M19 11a7 7 0 0 1-1.2 3.9M5 11a7 7 0 0 0 11.1 5.7M12 18v3M8 21h8M3 3l18 18"/>`,
	"camera":            `<rect x="2" y="6" width="14" height="12" rx="2"/><path d="m16 10 6-3.5v11L16 14z"/>`,
	"camera-off":        `<path d="M10.7 6H14a2 2 0 0 1 2 2v3.3l1 1 5-3.3v10M16 16v0a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h2M3 3l18 18"/>`,
	"screen-share":      `<rect x="2" y="3" width="20" height="14" rx="2"/><path d="M8 21h8M12 17v4M9 10l3-3 3 3M12 7v6"/>`,
	"screen-share-stop": `<rect x="2" y="3" width="20" height="14" rx="2"/><path d="M8 21h8M12 17v4M9.5 7.5l5 5M14.5 7.5l-5 5"/>`,
	"leave":             `<path d="M3.6 14.6c-.8-.8-.8-2.1.1-2.8C5.9 9.9 8.8 9 12 9s6.1.9 8.3 2.8c.9.7.9 2 .1 2.8l-1.3 1.3a1.7 1.7 0 0 1-2.2.2l-1.7-1.2a1.7 1.7 0 0 1-.7-1.4v-1.4a10 10 0 0 0-5 0v1.4c0 .6-.3 1.1-.7 1.4l-1.7 1.2a1.7 1.7 0 0 1-2.2-.2z"/>`,
	"phone":             `<path d="M5 3h3l1.5 4.5-2 1.2a11 11 0 0 0 5 5l1.2-2L18 13v3a2 2 0 0 1-2 2A13 13 0 0 1 3 5a2 2 0 0 1 2-2z"/>`,
	"people":            `<circle cx="9" cy="8" r="3.5"/><path d="M2 20a7 7 0 0 1 14 0M16 4.5a3.5 3.5 0 0 1 0 7M18 13.5a7 7 0 0 1 4 6.5"/>`,
	"chat":              `<path d="M4 5h16a1 1 0 0 1 1 1v10a1 1 0 0 1-1 1h-9l-5 4v-4H4a1 1 0 0 1-1-1V6a1 1 0 0 1 1-1z"/>`,
	"transcript":        `<path d="M6 2h9l5 5v13a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2z"/><path d="M14 2v6h6M8 13h8M8 17h6"/>`,
	"captions":          `<rect x="2" y="5" width="20" height="14" rx="2"/><path d="M10 10.5a2 2 0 1 0 0 3M17 10.5a2 2 0 1 0 0 3"/>`,
	"settings":          `<circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.7 1.7 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-1.8-.3 1.7 1.7 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-1.1-1.5 1.7 1.7 0 0 0-1.8.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.7 1.7 0 0 0 .3-1.8 1.7 1.7 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1a1.7 1.7 0 0 0 1.5-1.1 1.7 1.7 0 0 0-.3-1.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.7 1.7 0 0 0 1.8.3H9a1.7 1.7 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.7 1.7 0 0 0 1 1.5 1.7 1.7 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.7 1.7 0 0 0-.3 1.8V9a1.7 1.7 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.7 1.7 0 0 0-1.5 1z"/>`,
	"more":              `<circle cx="5" cy="12" r="1.5" fill="currentColor"/><circle cx="12" cy="12" r="1.5" fill="currentColor"/><circle cx="19" cy="12" r="1.5" fill="currentColor"/>`,
	"more-vertical":     `<circle cx="12" cy="5" r="1.5" fill="currentColor"/><circle cx="12" cy="12" r="1.5" fill="currentColor"/><circle cx="12" cy="19" r="1.5" fill="currentColor"/>`,
	"copy":              `<rect x="9" y="9" width="12" height="12" rx="2"/><path d="M5 15H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h10a1 1 0 0 1 1 1v1"/>`,
	"link":              `<path d="M10 14a5 5 0 0 0 7.1 0l3-3a5 5 0 0 0-7.1-7.1l-1.7 1.7M14 10a5 5 0 0 0-7.1 0l-3 3a5 5 0 0 0 7.1 7.1l1.7-1.7"/>`,
	"share":             `<path d="M4 12v7a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-7M16 6l-4-4-4 4M12 2v13"/>`,
	"hand":              `<path d="M18 11V6a2 2 0 0 0-4 0M14 10V4a2 2 0 0 0-4 0v6M10 10.5V6a2 2 0 0 0-4 0v8M18 8a2 2 0 0 1 4 0v6a8 8 0 0 1-8 8h-2c-2.8 0-4.5-.9-6-2.4l-3.6-3.6a2 2 0 0 1 2.8-2.8L6 14"/>`,
	"check":             `<path d="m4 12.5 5 5L20 6.5"/>`,
	"close":             `<path d="M6 6l12 12M18 6 6 18"/>`,
	"warning":           `<path d="M10.3 3.9 1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0zM12 9v4M12 17h.01"/>`,
	"info":              `<circle cx="12" cy="12" r="10"/><path d="M12 16v-4M12 8h.01"/>`,
	"error":             `<path d="M7.9 2h8.2L22 7.9v8.2L16.1 22H7.9L2 16.1V7.9z"/><path d="m15 9-6 6M9 9l6 6"/>`,
	"signal-0":          signalBars(0),
	"signal-1":          signalBars(1),
	"signal-2":          signalBars(2),
	"signal-3":          signalBars(3),
	"signal-4":          signalBars(4),
	"record":            `<circle cx="12" cy="12" r="7" fill="currentColor" stroke="none"/>`,
	"stop":              `<rect x="6" y="6" width="12" height="12" rx="1.5" fill="currentColor" stroke="none"/>`,
	"pin":               `<path d="M12 17v5M9 3h6l-1 6 3 3v2H7v-2l3-3z"/>`,
	"fullscreen":        `<path d="M3 8V3h5M16 3h5v5M21 16v5h-5M8 21H3v-5"/>`,
	"fullscreen-exit":   `<path d="M8 3v5H3M21 8h-5V3M16 21v-5h5M3 16h5v5"/>`,
	"volume":            `<path d="M11 5 6 9H2v6h4l5 4zM15.5 8.5a5 5 0 0 1 0 7M19 5a10 10 0 0 1 0 14"/>`,
	"volume-off":        `<path d="M11 5 6 9H2v6h4l5 4zM22 9l-6 6M16 9l6 6"/>`,
	"send":              `<path d="M22 2 11 13M22 2l-7 20-4-9-9-4z"/>`,
	"plus":              `<path d="M12 5v14M5 12h14"/>`,
	"minus":             `<path d="M5 12h14"/>`,
	"search":            `<circle cx="11" cy="11" r="7"/><path d="m21 21-4.3-4.3"/>`,
	"chevron-up":        `<path d="m6 15 6-6 6 6"/>`,
	"chevron-down":      `<path d="m6 9 6 6 6-6"/>`,
	"chevron-left":      `<path d="m15 6-6 6 6 6"/>`,
	"chevron-right":     `<path d="m9 6 6 6-6 6"/>`,
	"external":          `<path d="M14 4h6v6M20 4l-9 9M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5"/>`,
}

// signalBars is four rising bars, the first n filled (the rest outlined), for
// signal-0 … signal-4: a different drawing per level, so the level is a shape
// and not a colour.
func signalBars(n int) string {
	var b strings.Builder
	for i := 0; i < 4; i++ {
		h := 4 + i*5
		x := 3 + i*5
		fill := "none"
		if i < n {
			fill = "currentColor"
		}
		b.WriteString(`<rect x="` + strconv.Itoa(x) + `" y="` + strconv.Itoa(21-h) + `" width="3" height="` + strconv.Itoa(h) + `" rx=".5" fill="` + fill + `"/>`)
	}
	return b.String()
}

// IconNames is every icon's name, sorted.
func IconNames() []string {
	out := make([]string, 0, len(Icons))
	for n := range Icons {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// IconsSVG is the sprite: one <symbol id="mu-<name>" viewBox="0 0 24 24"> per
// icon, in name order. A sprite is only a set of definitions: the outer <svg>
// draws nothing on its own.
func IconsSVG() []byte {
	var b strings.Builder
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" aria-hidden="true" style="display:none">` + "\n")
	for _, n := range IconNames() {
		b.WriteString(`<symbol id="mu-` + n + `" viewBox="0 0 24 24">` + Icons[n] + "</symbol>\n")
	}
	b.WriteString("</svg>\n")
	return []byte(b.String())
}
