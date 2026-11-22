package ui

import (
	"html"
	"strconv"
	"strings"
)

// THE ASKS PAGE'S FILTER CHIPS (internal/api asks_filter.go): one row under
// the search box. Type chips (Questions, Confirmations, Drafts, Notices), then
// the sources that have items in the view — busiest first, the rest under
// More — each called by its name (an officer's, an agent's, a session's), and
// Clear while a filter is on.
//
// 🔴 EVERY CHIP IS A PLAIN LINK (operator decision "One-tap chips in the
// URL"): the filter is in the address (?type=, ?from=), so it is bookmarkable,
// Back undoes a tap, and the row works with no script. The page's script only
// keeps each link's ?tab= and ?q= in step with the address (asksChipsJS), so a
// tap after a tab switch or a typed search keeps them.

// AsksChip is one chip: its words, where a tap goes (the filter with this
// chip toggled), whether it is the active one, and how many items it has in
// the view (0: no count drawn).
type AsksChip struct {
	Label  string
	Href   string
	Active bool
	Count  int
	// Key is the chip's value (a type, a source key): data-asks-chip-key.
	Key string
}

// AsksChipsView is the row: the type chips, the sources shown inline, those
// under More, and Clear's link ("" while no filter is on).
type AsksChipsView struct {
	Types, Sources, More []AsksChip
	Clear                string
}

// The row's classes. 🔴 ONE LINE, SCROLLED SIDEWAYS (AsksChipsRowClass): a
// wrapped row took four lines of a phone's screen above the first card; its
// right edge fades (AsksChipsClass's ::after) so a cut chip reads as "more
// this way". More
// is a panel UNDER the row (AsksChipsMoreListClass: absolute, its containing
// block the <nav> — outside the scroller, so it is not clipped by it).
const (
	AsksChipsClass         = "relative min-w-0 px-1 after:pointer-events-none after:absolute after:inset-y-0 after:right-0 after:w-6 after:bg-gradient-to-l after:from-bg after:content-['']"
	AsksChipsRowClass      = "-mx-1 flex min-w-0 flex-nowrap items-center gap-1.5 overflow-x-auto px-1 py-0.5 pr-6 [scrollbar-width:none] [&::-webkit-scrollbar]:hidden"
	AsksChipClass          = "press inline-flex min-h-[36px] shrink-0 items-center gap-1.5 whitespace-nowrap rounded-full px-3 text-sm font-medium ring-1 ring-inset transition focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"
	AsksChipIdleClass      = "bg-s1 text-fg ring-line hover:bg-s2"
	AsksChipActiveClass    = "bg-accent text-on-accent ring-accent"
	AsksChipCountClass     = "text-xs font-normal opacity-70"
	AsksChipLabelClass     = "max-w-[12rem] truncate"
	AsksChipsRuleClass     = "mx-0.5 h-5 w-px shrink-0 bg-line"
	AsksChipsMoreClass     = "shrink-0 [&[open]_.asks-chevron]:rotate-180"
	AsksChipsMoreSumClass  = "press inline-flex min-h-[36px] cursor-pointer list-none items-center gap-1 whitespace-nowrap rounded-full px-3 text-sm font-medium text-fg2 ring-1 ring-inset ring-line transition hover:bg-s2 [&::-webkit-details-marker]:hidden"
	AsksChipsMoreListClass = "absolute inset-x-1 top-full z-20 mt-1 flex flex-wrap items-center gap-1.5 rounded-2xl bg-s1 p-2 shadow-lg ring-1 ring-inset ring-line"
	AsksChipsClearClass    = "inline-flex min-h-[36px] shrink-0 items-center whitespace-nowrap px-2 text-sm font-medium text-accent underline-offset-2 hover:underline"
)

// AsksChipsMoreLabel is the disclosure's words.
const AsksChipsMoreLabel = "More"

// asksChipHTML is one chip as its link.
func asksChipHTML(c AsksChip, kind string) string {
	esc := html.EscapeString
	cls, cur := AsksChipClass+" "+AsksChipIdleClass, ""
	if c.Active {
		cls, cur = AsksChipClass+" "+AsksChipActiveClass, ` aria-current="true"`
	}
	count := ""
	if c.Count > 0 {
		count = `<span class="` + AsksChipCountClass + `" data-asks-chip-count>` + strconv.Itoa(c.Count) + `</span>`
	}
	return `<a href="` + esc(c.Href) + `" class="` + cls + `"` + cur + ` data-asks-chip="` + kind + `" data-asks-chip-key="` + esc(c.Key) + `">` +
		`<span class="` + AsksChipLabelClass + `">` + esc(c.Label) + `</span>` + count + `</a>`
}

// AsksChipsHTML is the chip row, "" when it has no chip at all.
func AsksChipsHTML(v AsksChipsView) string {
	if len(v.Types)+len(v.Sources)+len(v.More) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<nav class="` + AsksChipsClass + `" aria-label="Filter asks" data-asks-chips><div class="` + AsksChipsRowClass + `" data-asks-chips-row>`)
	// 🔴 THE ACTIVE CHIPS LEAD THE ROW, Clear beside them: the row scrolls
	// sideways, and an active source chip after the four types sat off a
	// phone's screen — a filtered page that did not say it was filtered.
	active := false
	for _, c := range v.Types {
		if c.Active {
			b.WriteString(asksChipHTML(c, "type"))
			active = true
		}
	}
	for _, c := range v.Sources {
		if c.Active {
			b.WriteString(asksChipHTML(c, "from"))
			active = true
		}
	}
	if v.Clear != "" {
		b.WriteString(`<a href="` + html.EscapeString(v.Clear) + `" class="` + AsksChipsClearClass + `" data-asks-chips-clear>Clear</a>`)
	}
	if active {
		b.WriteString(`<span class="` + AsksChipsRuleClass + `" aria-hidden="true"></span>`)
	}
	for _, c := range v.Types {
		if !c.Active {
			b.WriteString(asksChipHTML(c, "type"))
		}
	}
	rest := 0
	for _, c := range v.Sources {
		if !c.Active {
			rest++
		}
	}
	if rest+len(v.More) > 0 {
		b.WriteString(`<span class="` + AsksChipsRuleClass + `" aria-hidden="true"></span>`)
	}
	for _, c := range v.Sources {
		if !c.Active {
			b.WriteString(asksChipHTML(c, "from"))
		}
	}
	if len(v.More) > 0 {
		b.WriteString(`<details class="` + AsksChipsMoreClass + `" data-asks-chips-more><summary class="` + AsksChipsMoreSumClass + `">` + AsksChipsMoreLabel +
			`<svg class="asks-chevron h-4 w-4 transition-transform" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="m6 9 6 6 6-6"/></svg></summary>` +
			`<div class="` + AsksChipsMoreListClass + `" data-asks-chips-more-list>`)
		for _, c := range v.More {
			b.WriteString(asksChipHTML(c, "from"))
		}
		b.WriteString(`</div></details>`)
	}
	b.WriteString(`</div></nav>`)
	return b.String()
}

// asksChipsJS keeps every chip's ?tab= and ?q= what the address says now
// (window.muAsksChips, which the page's tab script calls on a tab change):
// the row is drawn once, and a tab switch or a typed search moves the address
// without a reload. Each chip's own ?type= and ?from= are its target and are
// never touched.
var asksChipsJS = `(function () {
  function sync() {
    var cur;
    try { cur = new URL(window.location.href); } catch (e) { return; }
    var links = document.querySelectorAll('[data-asks-chips] a[href]');
    for (var i = 0; i < links.length; i++) {
      try {
        var u = new URL(links[i].getAttribute('href'), window.location.href);
        ['tab', 'q'].forEach(function (k) {
          var v = cur.searchParams.get(k);
          if (v) u.searchParams.set(k, v); else u.searchParams.delete(k);
        });
        links[i].setAttribute('href', u.pathname + (u.search || ''));
      } catch (e) {}
    }
  }
  window.muAsksChips = sync;
  document.addEventListener('htmx:afterSettle', sync);
})();`

// AsksChipsScriptHTML is the chip row's script, inline (a few lines; the page
// writes it once, after the row).
func AsksChipsScriptHTML() string { return `<script>` + asksChipsJS + `</script>` }
