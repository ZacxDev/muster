package ui

import "html"

// AN ASK'S MEDIA STRIP AND CONTEXT LINE (docs/attention.md "Attachments and
// context"): the two render helpers every surface that draws an agent's ask
// calls — the Asks card and the ask page today; the card feed's card slots
// them in the same way. internal/api builds the values (askMediaView,
// askWhyView); nothing here reads a store.
//
// 🔴 EVERYTHING HERE IS THE ASKER'S, SO UNTRUSTED. Every name, type, size and
// line is escaped (html.EscapeString) into text or a quoted attribute; a URL is
// muster's own authorised route (/attention/asks/{id}/attachments/{aid}),
// never one the asker wrote. An image is an <img> whose src is that route, and
// only for the four raster types the route serves inline (Image); an SVG — a
// document that runs script — and every other file is a CHIP that downloads,
// never an <img>, <object>, <embed> or <iframe>, so nothing it holds runs.

// AskFile is one attachment as a strip draws it.
type AskFile struct {
	// Name is the file's name, Type its sniffed type, Size its size in words
	// ("312 KB").
	Name, Type, Size string
	// URL is muster's authorised route for it.
	URL string
	// Image: a raster image served inline (PNG, JPEG, GIF, WebP), drawn as a
	// thumbnail that opens full-size; false draws a chip.
	Image bool
}

// AskWhy is why an ask is here as a context line draws it: the agent's own
// Text, or muster's fallback Focus and Last.
type AskWhy struct {
	Text, Focus, Last string
}

// AskMediaClass, AskThumbClass and AskChipClass are the strip's classes.
const (
	AskMediaClass = "flex min-w-0 flex-wrap gap-2"
	AskThumbClass = "press block h-14 w-14 shrink-0 overflow-hidden rounded-lg bg-s2 ring-1 ring-inset ring-line"
	AskChipClass  = "press inline-flex min-h-[44px] max-w-full min-w-0 items-center gap-1.5 rounded-lg bg-bg px-3 text-xs text-fg ring-1 ring-inset ring-edge transition hover:bg-s2"
	AskWhyClass   = "break-words text-xs text-fg2"
)

// askFileIcon is a file chip's glyph (a page).
const askFileIcon = `<svg class="h-4 w-4 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="M14 3H6a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9z"/><path d="M14 3v6h6"/></svg>`

// AskMediaStrip is an ask's attachments: each image a thumbnail linking to
// itself full-size (a new tab), each other file a chip with its name and size
// that downloads it. "" for none.
func AskMediaStrip(files []AskFile) string {
	if len(files) == 0 {
		return ""
	}
	esc := html.EscapeString
	out := `<div class="` + AskMediaClass + `" data-ask-media role="list" aria-label="Attachments">`
	for _, f := range files {
		label := f.Name + " (" + f.Size + ")"
		if f.Image {
			out += `<a role="listitem" href="` + esc(f.URL) + `" target="_blank" rel="noopener noreferrer" class="` + AskThumbClass +
				`" title="` + esc(label) + `" data-ask-media-image><img src="` + esc(f.URL) + `" alt="` + esc(f.Name) +
				`" loading="lazy" decoding="async" class="h-full w-full object-cover"></a>`
			continue
		}
		out += `<a role="listitem" href="` + esc(f.URL) + `" download="` + esc(f.Name) + `" class="` + AskChipClass + `" title="` + esc(label+" · "+f.Type) +
			`" data-ask-media-file>` + askFileIcon + `<span class="min-w-0 truncate">` + esc(f.Name) +
			`</span><span class="shrink-0 tabular-nums text-fg2">` + esc(f.Size) + `</span></a>`
	}
	return out + `</div>`
}

// AskContextLine is why an ask is here, one muted line: the agent's own words
// ("Why: …", data-ask-why="agent"), or muster's fallback — the session's focus
// and what it last said (data-ask-why="session"). "" for none.
func AskContextLine(w AskWhy) string { return askContext(w, "p", AskWhyClass) }

// AskContextInline is AskContextLine as a block <span>, for a card that is one
// link whose lines are spans (a held question's row).
func AskContextInline(w AskWhy) string { return askContext(w, "span", "block "+AskWhyClass) }

func askContext(w AskWhy, tag, class string) string {
	esc := html.EscapeString
	switch {
	case w.Text != "":
		return `<` + tag + ` class="` + class + `" data-ask-why="agent"><span class="font-semibold">Why:</span> ` + esc(w.Text) + `</` + tag + `>`
	case w.Focus != "" || w.Last != "":
		out := `<` + tag + ` class="` + class + `" data-ask-why="session">`
		if w.Focus != "" {
			out += `<span class="font-semibold">Working on:</span> ` + esc(w.Focus)
		}
		if w.Last != "" {
			if w.Focus != "" {
				out += ` · `
			}
			out += `<span class="font-semibold">Last said:</span> ` + esc(w.Last)
		}
		return out + `</` + tag + `>`
	}
	return ""
}
