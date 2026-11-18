package ui

// THE ASKS PAGE'S LOG TAB (internal/api/ask_log_read.go renders it;
// docs/asks.md, "Reading the log"). Its classes live here for the same reason
// asks_list.go's do: the stylesheet scans internal/ui, not internal/api, so a
// class written only there would ship unstyled with css-check green.
//
// The tab is the Asks page's column (ui.NavDocumentWideClass): the filters
// behind one shut "Filters" toggle, as a two-column grid of 44px controls; the
// summary as two-column stat tiles; then the list as the page's own cards
// (AsksCardClass).

const (
	// AsksLogFormClass is the filter form: two columns of fields.
	AsksLogFormClass = "grid grid-cols-2 gap-2"
	// AsksLogFieldClass is one labelled field (its label text above it).
	AsksLogFieldClass = "flex min-w-0 flex-col gap-1 text-xs font-medium text-fg2"
	// AsksLogInputClass is a select or text box in the form.
	AsksLogInputClass = "min-h-[44px] w-full min-w-0 rounded-lg bg-bg px-2 text-sm text-fg ring-1 ring-inset ring-edge focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"
	// AsksLogWideClass makes a form row span both columns.
	AsksLogWideClass = "col-span-2"
	// AsksLogStatsClass is the summary's tiles.
	AsksLogStatsClass = "grid grid-cols-2 gap-2"
	// AsksLogStatClass is one tile: its value, then its label.
	AsksLogStatClass = "flex min-w-0 flex-col rounded-xl bg-s1 px-3 py-2 ring-1 ring-inset ring-line"
	// AsksLogStatValueClass is a tile's number.
	AsksLogStatValueClass = "truncate text-lg font-semibold tabular-nums text-fg"
	// AsksLogBreakdownsClass holds the three breakdowns (kind, how it ended,
	// who answered): stacked on a phone, side by side from `sm` up.
	AsksLogBreakdownsClass = "grid grid-cols-1 gap-3 sm:grid-cols-3"
	// AsksLogCountsClass is one breakdown's list of "label … N" rows. 🔴 PLAIN
	// STATISTICS, NOT PILLS: a count is not a control, and drawn as a pill it
	// read as one (the design review of the Log tab: ~15 chips that looked
	// tappable and were not).
	AsksLogCountsClass = "flex flex-col"
	// AsksLogCountRowClass is one row: its label, then its number on the right.
	AsksLogCountRowClass = "flex items-baseline justify-between gap-3 px-1 py-0.5 text-sm"
	// AsksLogCountLabelClass / AsksLogCountValueClass are its two halves.
	AsksLogCountLabelClass = "min-w-0 text-fg2"
	AsksLogCountValueClass = "shrink-0 font-medium tabular-nums text-fg"
	// AsksLogSectionHeadClass is a section heading's row when an ⓘ sits
	// beside it (a heading may hold only text, so the ⓘ is its sibling).
	AsksLogSectionHeadClass = "flex items-center justify-between gap-2"
	// AsksLogFactClass is one fact on a facts line: kept whole, so the line
	// wraps between facts, never inside one ("median 31s" never splits).
	AsksLogFactClass = "whitespace-nowrap"
	// AsksLogClampClass clamps a question to three lines in the list.
	AsksLogClampClass = "line-clamp-3 whitespace-pre-wrap break-words text-sm text-fg"
	// AsksLogFactsClass is a card's secondary line that WRAPS (a repeated
	// question's facts, an answer): AsksMutedTextClass truncates, and at a
	// phone's width that cut the facts mid-word.
	AsksLogFactsClass = "break-words text-xs text-fg2"
	// AsksLogExpiredClass is the line a row whose text expired shows instead.
	AsksLogExpiredClass = "text-sm italic text-muted"
)
