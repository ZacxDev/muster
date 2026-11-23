package ui

// THE OFFICER PAGE's classes that only its own sections draw (internal/api
// officers_pages.go, officer_needs_you.go, officer_page_charter.go). They live
// here, not there, because the stylesheet scans internal/ui and not
// internal/api: a class written only in internal/api renders unstyled.
//
// 🔴 MOBILE FIRST: every row is a column on a phone and a row from sm: up,
// and every control is at least 44px tall.
const (
	// OfficerCharterTextClass is the charter read where it cannot be edited:
	// its own line breaks kept, long words wrapped, never wider than the card.
	OfficerCharterTextClass = "max-h-80 overflow-y-auto whitespace-pre-wrap break-words rounded-lg bg-bg px-3 py-2 text-sm text-fg ring-1 ring-inset ring-edge"
	// OfficerCharterInputClass is the charter's editor: the card's full width.
	OfficerCharterInputClass = "block min-h-[11rem] w-full min-w-0 resize-y rounded-md bg-bg px-3 py-2 text-base leading-relaxed text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus"
	// OfficerButtonRowClass is a row of a section's buttons: stacked and full
	// width on a phone, side by side from sm: up.
	OfficerButtonRowClass = "flex flex-col gap-2 sm:flex-row sm:flex-wrap"
	// OfficerRowButtonClass widens a button in OfficerButtonRowClass on a phone.
	OfficerRowButtonClass = "w-full justify-center sm:w-auto"
	// OfficerVersionChipClass is the charter's "Version 3" chip.
	OfficerVersionChipClass = "inline-flex shrink-0 items-center rounded-full bg-s2 px-2 py-0.5 text-xs font-medium text-fg2"
	// OfficerStaleClass is the warning that it works from an older version.
	OfficerStaleClass = "rounded-lg bg-st-warning-bg px-3 py-2 text-sm text-st-warning-fg"
	// OfficerDisclosureClass is a section's <summary> that opens more of it.
	OfficerDisclosureClass = "flex min-h-[44px] cursor-pointer items-center gap-2 text-sm font-medium text-accent"
	// OfficerSpendWordsClass is the spend bar's line: "$8.00 of $10 today"
	// and its state, wrapping under each other on a phone.
	OfficerSpendWordsClass = "flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1"
	// OfficerSpendHeldClass is the held line under a full bar.
	OfficerSpendHeldClass = "text-sm text-st-error-fg"
	// OfficerSpendNearClass is the "getting close" line under an amber bar.
	OfficerSpendNearClass = "text-sm text-st-warning-fg"
)
