package ui

// A REPLY'S CONTEXT ON ITS CARDS (internal/api draft_reply_context.go): the
// post or message an outbound reply answers, quoted above the draft in the
// card's quote box (AsksQuoteClass) — who wrote it, when, an ⓘ link to the
// original, and its text, a long one folded under "Show more" (a native
// <details>, no script: closed it shows the text's start, open the whole).
const (
	// ReplyContextHeadClass is the quote's head line: who, the untrusted
	// pill, when, and the ⓘ link.
	ReplyContextHeadClass = "flex min-w-0 flex-wrap items-center gap-1.5 text-xs text-fg2"
	// ReplyContextLinkClass is the ⓘ link to the original, with its host.
	ReplyContextLinkClass = "inline-flex min-h-[24px] items-center gap-1 break-all text-xs text-fg2 underline decoration-dotted underline-offset-2"
	// ReplyContextTextClass is the quoted text.
	ReplyContextTextClass = "whitespace-pre-wrap break-words text-sm text-fg2"
	// ReplyContextFoldClass / ReplyContextSummaryClass are a long text's fold;
	// ReplyContextStartClass is its start, shown only while it is closed.
	ReplyContextFoldClass    = "group min-w-0"
	ReplyContextSummaryClass = "flex min-w-0 cursor-pointer list-none flex-col gap-1 [&::-webkit-details-marker]:hidden"
	ReplyContextStartClass   = "whitespace-pre-wrap break-words text-sm text-fg2 group-open:hidden"
	// ReplyContextMoreClass is "Show more" (closed); ReplyContextLessClass
	// "Show less" (open).
	ReplyContextMoreClass = "text-xs font-semibold text-fg underline group-open:hidden"
	ReplyContextLessClass = "hidden text-xs font-semibold text-fg underline group-open:inline"
	// ReplyContextMissingClass is the line a reply without its context draws.
	ReplyContextMissingClass = "text-xs text-fg2"
)
