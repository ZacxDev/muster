package ui

// THE LOGO OF A VENUE A DRAFT POSTS TO (the step-up card's post preview,
// internal/api host_stepup_card.go): an inline SVG glyph, monochrome in
// currentColor so it takes the card's own text token in light and dark, and
// never fetched from anywhere. A venue this table does not know gets the
// neutral VenueLogoFallback, so an unknown venue is drawn as unknown — never
// as the look of a venue it is not.
//
// The keys are the venue names plugins file drafts under
// (internal/plugins venue_rules.go: VenueX, VenueReddit).

// THE POST PREVIEW'S CLASSES. The preview is drawn in internal/api (its
// markup is built from a stored card there), but its classes are written HERE
// so Tailwind's content scan (tailwind.config.js: internal/ui, not the api
// package) emits them; muster's tokens, so light and dark both hold.
const (
	// StepUpPostClass is the page's post card; StepUpPostFeedClass the
	// feed's compact one.
	StepUpPostClass     = "flex min-w-0 flex-col gap-3 rounded-2xl bg-bg p-4 ring-1 ring-inset ring-edge"
	StepUpPostFeedClass = "flex min-w-0 flex-col gap-2 rounded-xl bg-bg p-3 ring-1 ring-inset ring-edge"
	// StepUpPostHeadClass is the page's head row (logo, venue, kind);
	// StepUpPostFeedHeadClass the feed's.
	StepUpPostHeadClass     = "flex min-w-0 items-center gap-3"
	StepUpPostFeedHeadClass = "flex min-w-0 items-center gap-2"
	// StepUpLogoClass is the logo's disc, StepUpLogoGlyphClass its glyph
	// (the feed's: StepUpLogoFeedClass, StepUpLogoFeedGlyphClass). The disc
	// is the text colour with the glyph in the page's: a dark mark on light,
	// a light one on dark.
	StepUpLogoClass          = "flex h-10 w-10 shrink-0 items-center justify-center rounded-full bg-fg text-bg"
	StepUpLogoGlyphClass     = "h-5 w-5"
	StepUpLogoFeedClass      = "flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-fg text-bg"
	StepUpLogoFeedGlyphClass = "h-4 w-4"
	// StepUpPostNamesClass holds the venue (StepUpPostVenueClass) over what
	// the draft does there (StepUpPostKindClass); the feed's is one line
	// (StepUpPostFeedNamesClass).
	StepUpPostNamesClass     = "flex min-w-0 flex-col leading-tight"
	StepUpPostVenueClass     = "truncate text-base font-semibold text-fg"
	StepUpPostKindClass      = "truncate text-sm text-fg2"
	StepUpPostFeedNamesClass = "min-w-0 truncate text-sm text-fg2"
	StepUpPostFeedVenueClass = "font-semibold text-fg"
	// StepUpPartClass is one post of the preview (a thread's later ones add
	// StepUpPartRuleClass); StepUpPartNClass its "1/3".
	StepUpPartClass     = "flex min-w-0 flex-col gap-3"
	StepUpPartRuleClass = "border-t border-line pt-3"
	StepUpPartNClass    = "text-xs font-semibold text-fg2"
	// StepUpTextClass holds a post's paragraphs (StepUpParaClass each);
	// StepUpFeedTextClass is the feed's one clamped block.
	StepUpTextClass     = "flex min-w-0 flex-col gap-3 text-[15px] leading-normal text-fg"
	StepUpParaClass     = "whitespace-pre-wrap break-words"
	StepUpFeedTextClass = "asks-clamp whitespace-pre-wrap break-words text-[15px] leading-snug text-fg"
	// StepUpThreadMoreClass is the feed's "+N more posts in this thread".
	StepUpThreadMoreClass = "text-xs text-fg2"
	// StepUpMediaClass is a post's media grid (StepUpMediaTwoUpClass added
	// for more than one) — a border, not an inset ring, which the image
	// would paint over (a dark image on the dark card had no edge); StepUpMediumOneClass a lone image or video,
	// StepUpMediumTileClass one of several.
	StepUpMediaClass      = "grid min-w-0 gap-0.5 overflow-hidden rounded-xl border border-line"
	StepUpMediaTwoUpClass = "grid-cols-2"
	StepUpMediumOneClass  = "block h-auto max-h-[32rem] w-full bg-s1 object-cover"
	StepUpMediumTileClass = "block aspect-square h-full w-full bg-s1 object-cover"
	// StepUpFeedMediaClass is the feed's row of thumbnails
	// (StepUpFeedThumbClass each, in the image's own proportions).
	StepUpFeedMediaClass = "flex min-w-0 flex-wrap gap-1"
	StepUpFeedThumbClass = "h-20 w-auto max-w-[12rem] rounded-lg border border-line bg-s1 object-cover"
	// The feed card's own lines: its title, its why and where lines
	// (StepUpFeedLineClass), its changed-draft note.
	StepUpFeedClass        = "flex min-w-0 flex-col gap-2"
	StepUpFeedTitleClass   = "break-words text-[15px] font-semibold leading-snug text-fg"
	StepUpFeedLineClass    = "asks-clamp-1 break-words text-sm text-fg2"
	StepUpFeedChangedClass = "rounded-xl bg-s1 px-3 py-2 text-sm font-semibold text-fg ring-1 ring-line"
	// StepUpNoteClass is a line drawn under the post, OUTSIDE its mock, that
	// the post itself never says: that the preview is an excerpt, or that it
	// has look-alike letters (StepUpFeedNoteClass the feed's).
	StepUpNoteClass     = "rounded-2xl bg-s1 px-4 py-3 text-sm font-semibold text-fg ring-1 ring-line"
	StepUpFeedNoteClass = "rounded-xl bg-s1 px-3 py-2 text-sm font-semibold text-fg ring-1 ring-line"
	// StepUpLookalikeClass marks a word of the post holding look-alike
	// letters (internal/api stepUpLookalike): the word drawn as it is, on a
	// danger tint with a wavy underline, so it reads as flagged in light and
	// dark alike.
	StepUpLookalikeClass = "rounded-sm bg-danger/15 text-fg underline decoration-danger decoration-wavy underline-offset-2"
	// StepUpHiddenClass is the visible flag drawn IN PLACE OF a hidden code
	// point (internal/api stepUpHiddenHTML: a bidi control, a zero-width or
	// other invisible character, a control character): a small danger-tinted
	// tag naming it ("U+202E"), never the character itself.
	StepUpHiddenClass = "mx-px inline-block rounded-sm bg-danger/15 px-1 align-baseline font-mono text-[11px] font-semibold leading-tight text-fg ring-1 ring-inset ring-danger/60"
	// StepUpDetailsClass / StepUpDetailsSummaryClass / StepUpDetailsTextClass
	// are a card's folded Details (the step-up card's boilerplate, a draft's
	// media facts and references): one fold, on every card that has one.
	StepUpDetailsClass        = "rounded-lg bg-s1 px-3 py-2 ring-1 ring-inset ring-line"
	StepUpDetailsSummaryClass = "flex min-h-[44px] cursor-pointer items-center justify-between gap-2 text-sm font-semibold text-fg2"
	StepUpDetailsTextClass    = "whitespace-pre-wrap break-words pb-2 text-sm text-fg2"
	// StepUpDetailsFactClass is one line of a fold's list of facts (a
	// medium's type, size and SHA-256), which may break anywhere.
	StepUpDetailsFactClass = "break-all font-mono text-xs text-fg2"
)

// VenueLogoFallback is the key of the neutral glyph (a speech bubble).
const VenueLogoFallback = "other"

// venueLogos maps a venue to its glyph's inner SVG (viewBox 0 0 24 24).
var venueLogos = map[string]string{
	// X: its mark, filled.
	"x": `<path fill="currentColor" d="M18.244 2.25h3.308l-7.227 8.26 8.502 11.24H16.17l-5.214-6.817L4.99 21.75H1.68l7.73-8.835L1.254 2.25H8.08l4.713 6.231zm-1.161 17.52h1.833L7.084 4.126H5.117z"/>`,
	// Reddit: its alien head, drawn in strokes.
	"reddit": `<g fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round">` +
		`<ellipse cx="12" cy="14.5" rx="8.5" ry="5.8"/><path d="M12 8.7l1.3-5.2 4.6 1.1"/><circle cx="19.3" cy="5" r="1.6"/>` +
		`<circle cx="3.9" cy="10.6" r="1.7"/><circle cx="20.1" cy="10.6" r="1.7"/><path d="M8.8 17.3c1.9 1.3 4.5 1.3 6.4 0"/></g>` +
		`<circle cx="9" cy="13.8" r="1.3" fill="currentColor"/><circle cx="15" cy="13.8" r="1.3" fill="currentColor"/>`,
	// Any other venue: a plain speech bubble.
	VenueLogoFallback: `<path fill="none" stroke="currentColor" stroke-width="1.6" stroke-linejoin="round" ` +
		`d="M4 5.5A2.5 2.5 0 0 1 6.5 3h11A2.5 2.5 0 0 1 20 5.5v8a2.5 2.5 0 0 1-2.5 2.5H10l-4.5 4v-4h0A1.5 1.5 0 0 1 4 14.5z"/>`,
}

// VenueLogoKey is the logo venue is drawn with: its own when known, else
// VenueLogoFallback.
func VenueLogoKey(venue string) string {
	if _, ok := venueLogos[venue]; ok && venue != VenueLogoFallback {
		return venue
	}
	return VenueLogoFallback
}

// VenueLogoSVG is venue's glyph (VenueLogoKey) as an inline, decorative SVG
// of class cls: aria-hidden, since the venue's name is always written beside
// it.
func VenueLogoSVG(venue, cls string) string {
	key := VenueLogoKey(venue)
	return `<svg viewBox="0 0 24 24" aria-hidden="true" focusable="false" class="` + cls + `" data-venue-logo="` + key + `">` + venueLogos[key] + `</svg>`
}
