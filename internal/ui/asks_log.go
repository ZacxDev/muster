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
	// wraps between facts, never inside one ("median 31s" never splits). 🔴
	// Every fact after the first carries its "· " separator INSIDE its own
	// unit ("· median 31s"), so a wrap starts the next line with the
	// separator and never leaves one dangling at the end of a line (it did at
	// a phone's width: "… always “continue” ·" / "median 11s").
	AsksLogFactClass = "whitespace-nowrap"
	// AsksLogSubClass is a log row's muted line under its title: where it was
	// asked, then how long ago — "ratelimit @ work-laptop · acme/api · 15m
	// ago" (internal/api askLogSubHTML), its parts AsksLogWhoClass then
	// AsksLogTailClass. 🔴 THE AGE IS ALWAYS SEEN, AND NO PART OF THE PLACE IS
	// CUT TO A STUB: it was one truncated line, age last, so a long place cut
	// the age off ("… acme/api · …"); then a place that truncated as one
	// string cut its repo to "· a…" on a phone.
	AsksLogSubClass = "flex min-w-0 items-baseline gap-1 text-xs text-fg2"
	// AsksLogWhoClass is the sub line's session @ host part. It gives way to
	// the tail (which never narrows past the age) and truncates against the
	// rest of the line: on a phone that leaves well over a dozen characters
	// ("ratelimit-backoff-and-retry-…"), never a stub.
	AsksLogWhoClass = "min-w-0 truncate"
	// AsksLogTailClass is the sub line's repo and age: ONE line (h-4 is
	// text-xs's 1rem line) that wraps its items and clips the rest. 🔴 A REPO
	// WITH NO ROOM TO BE READ WRAPS AWAY WHOLE: a flex item moves to the next
	// (clipped) line when its flex basis does not fit beside the items before
	// it, and the age is laid out first (AsksLogAgeClass's order-first), so it
	// is the repo, never the age, that goes — with no script measuring
	// anything. The line runs right-to-left (flex-row-reverse: the age, first,
	// is drawn rightmost) and is packed to its LEFT edge (justify-end, in a
	// reversed row), so it reads "· repo · age" straight after the session @
	// host part and any room left over is at the line's end, not a gap where
	// a dropped repo was. It takes the rest of the sub line (grow) and never
	// narrows past its min-content (min-w-min), which is the age's width: the
	// repo contributes none (AsksLogRepoClass's w-0).
	AsksLogTailClass = "flex h-4 min-w-min grow flex-row-reverse flex-wrap content-start justify-end gap-x-1 overflow-hidden"
	// AsksLogRepoClass is the sub line's repo ("· acme/api"). Its flex basis,
	// 12ch, is the room it needs to be read when it is cut: its "· ", SIX
	// characters, and the ellipsis — six because fewer reads as debris ("· a…",
	// "· ac…") while six names a repo ("· acme-c…"). Measured at the line's
	// 12px in Chromium's sans-serif (DejaVu Sans on the e2e host), "· " + six
	// characters + "…" is 7.5ch for "api-ga", 8.3ch for "acme-c", and at most
	// 11.9ch for the widest run measured ("WWWWWW"; "mmmmmm" 11.8ch) — so 12ch
	// leaves six characters of every run measured. With less room than that beside
	// the age it wraps onto the tail's clipped line and is not drawn; with
	// more it grows up to its own width (max-w-max), so a repo that fits shows
	// whole (a repo narrower than 12ch needs only its own width: max-w-max
	// clamps the basis). w-0 makes its contribution to the tail's min-content
	// zero, so a long repo never holds the tail wide (the flex basis, not the
	// width, sizes it).
	AsksLogRepoClass = "w-0 min-w-0 max-w-max grow basis-[12ch] truncate"
	// AsksLogAgeClass is the sub line's age ("· 15m ago"): it never shrinks,
	// never wraps, and is first on the tail's line (order-first), so it is
	// never the item that wraps away.
	AsksLogAgeClass = "order-first shrink-0 whitespace-nowrap"
	// AsksLogClampClass clamps a question to three lines in the list.
	AsksLogClampClass = "line-clamp-3 whitespace-pre-wrap break-words text-sm text-fg"
	// AsksLogFactsClass is a card's secondary line that WRAPS (a repeated
	// question's facts, an answer): AsksMutedTextClass truncates, and at a
	// phone's width that cut the facts mid-word.
	AsksLogFactsClass = "break-words text-xs text-fg2"
	// AsksLogExpiredClass is the line a row whose text expired shows instead.
	AsksLogExpiredClass = "text-sm italic text-muted"
)
