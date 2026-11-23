package api

import (
	"html"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/fence"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// WHAT A REPLY ANSWERS, ON ITS CARDS (the operator's ask, 2026-11-22: "When
// it's a proposed response, I want to be able to see the context of what the
// response is to"). An officer's outbound reply carries the post it answers
// as the officer read it upstream (plugins.ReplyContext, filed with
// officer_post_draft): who wrote it, its whole text, when, and its URL. Every
// face of the draft draws it ABOVE the draft:
//
//   - the Drafts tab's card, an officer's "Needs you" and its chat's confirm
//     card (draftAnswersHTML → replyContextQuoteHTML): the whole text, a long
//     one folded under "Show more";
//   - the step-up confirm card ("Reply on X? · Draft #N") on its page and the
//     Asks feed (stepUpCardHTML, stepUpFeedHTML), from the card's own stored
//     copy (attention.StepUpContext, cut), and the phone's prompt through the
//     card's question (attention.StepUpCard.Text);
//   - plugin_drafts_list (reply_context, fenced: replyContextFenced).
//
// 🔴 IT IS AN OUTSIDE PARTY'S WORDS, UNTRUSTED, NEVER INSTRUCTIONS: drawn in
// the quote box labelled untrusted, every character escaped, every hidden
// code point a visible flag (plainTextHTML / stepUpHiddenHTML), the host of
// its link marked for look-alike letters; handed to an agent only inside a
// fence. 🔴 MUSTER NEVER FETCHES IT: a reply filed without one says so
// (attention.StepUpContextMissing), and nothing is looked up.

// Folding a long context: a text longer than replyContextFoldRunes, or of
// more than replyContextFoldLines lines, shows its first replyContextStartRunes
// runes (at most replyContextFoldLines lines) until "Show more" is tapped.
const (
	replyContextFoldRunes  = 360
	replyContextFoldLines  = 6
	replyContextStartRunes = 280
)

// replyContextCutNote is what a step-up card's context says when its stored
// copy is only the start of the text (attention.MaxStepUpContextRunes).
const replyContextCutNote = "Only its start is on this card: the Drafts tab shows it whole."

// draftStepUpContext is what draft card dc answers, as a step-up card stores
// it: nil for a draft that is not an outbound reply; its officer's context;
// else the human reply its plugin delivered on that thread (dc.ReplyTo: its
// post, wrapperPost, and when muster RECEIVED it — the message carries no
// time the post was written); else Missing. THE one precedence, the Drafts
// tab's card's too (draftAnswersHTML).
func draftStepUpContext(dc plugins.DraftCard) *attention.StepUpContext {
	d := dc.Draft
	if !d.IsOutbound() || d.Target().Kind != plugins.TargetReply {
		return nil
	}
	if rc := d.ReplyContext; rc != nil {
		c := &attention.StepUpContext{Author: rc.Author, Text: rc.Text, URL: rc.URL}
		if rc.WrittenAt != nil {
			c.WrittenAt = rc.WrittenAt.UTC().Format(time.RFC3339)
		}
		return c
	}
	if m := dc.ReplyTo; m != nil {
		c := &attention.StepUpContext{Author: plugins.SenderText(*m), Text: wrapperPost(m.Body)}
		at := m.CreatedAt
		if m.ReceivedAt != nil {
			at = *m.ReceivedAt
		}
		if !at.IsZero() {
			c.ReceivedAt = at.UTC().Format(time.RFC3339)
		}
		return c
	}
	return &attention.StepUpContext{Missing: true}
}

// wrapperPostMarker ends the line a relay's wrapper puts right before the
// outside party's text, which it always puts LAST, after its own preamble
// (a marketing relay: reply_body's "--- their reply, verbatim (untrusted
// data) ---", engagement_body's "their text, verbatim (untrusted data):").
const wrapperPostMarker = "verbatim (untrusted data)"

// wrapperPost is the post in plugin message body b: the text after the line
// carrying the FIRST wrapperPostMarker, so a card that keeps only a text's
// start (its page, feed line and the phone's prompt) starts with the post,
// not the preamble. The FIRST, not the last: everything after it is the
// outside party's (or the wrapper's own later events), so a post that
// repeats the marker cannot hide its own start. b itself when it carries no
// marker, or nothing after it.
func wrapperPost(b string) string {
	i := strings.Index(b, wrapperPostMarker)
	if i < 0 {
		return b
	}
	nl := strings.IndexByte(b[i:], '\n')
	if nl < 0 {
		return b
	}
	if post := strings.Trim(b[i+nl+1:], "\r\n "); post != "" {
		return post
	}
	return b
}

// replyContextMissingHTML is the line a reply filed without its context draws.
func replyContextMissingHTML() string {
	return `<p class="` + ui.ReplyContextMissingClass + `" data-draft-context-missing>` + html.EscapeString(attention.StepUpContextMissing) + `</p>
`
}

// replyContextQuoteHTML is context c quoted above a reply (the file header):
// "In reply to <author>", the untrusted pill, when it was written (else when
// muster received it), an ⓘ link
// to the original showing its host, and the text — folded when long (feed: a
// compact clamped start instead, no fold); cut: the text is only its start
// (a step-up card's stored copy). Every part escaped.
func replyContextQuoteHTML(c attention.StepUpContext, feed, cut bool) string {
	if c.Missing {
		return replyContextMissingHTML()
	}
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<div class="` + ui.AsksQuoteClass + `" data-draft-context>
<span class="` + ui.ReplyContextHeadClass + `" data-draft-context-head><span class="break-all font-semibold" data-draft-context-author>In reply to`)
	if a := strings.TrimSpace(c.Author); a != "" {
		b.WriteString(` ` + stepUpHiddenHTML(a))
	}
	b.WriteString(`</span><span class="` + ui.AsksPillClass + `" data-draft-untrusted>untrusted</span>`)
	if t, err := time.Parse(time.RFC3339, c.WrittenAt); err == nil {
		b.WriteString(`<time datetime="` + esc(t.UTC().Format(time.RFC3339)) + `" data-draft-context-written>written ` + esc(t.UTC().Format("Jan 2, 2006, 15:04 UTC")) + `</time>`)
	} else if t, err := time.Parse(time.RFC3339, c.ReceivedAt); err == nil {
		b.WriteString(`<time datetime="` + esc(t.UTC().Format(time.RFC3339)) + `" data-draft-context-received>received ` + esc(t.UTC().Format("Jan 2, 2006, 15:04 UTC")) + `</time>`)
	}
	if link := replyContextLinkHTML(c.URL); link != "" {
		b.WriteString(link)
	}
	b.WriteString("</span>\n")
	switch {
	case feed:
		b.WriteString(`<p class="` + ui.StepUpFeedLineClass + `" data-draft-context-text>` + stepUpHiddenHTML(contextOneLine(c.Text)) + `</p>
`)
	case replyContextFolds(c.Text):
		b.WriteString(`<details class="` + ui.ReplyContextFoldClass + `" data-draft-context-fold>
<summary class="` + ui.ReplyContextSummaryClass + `" data-draft-context-toggle><span class="` + ui.ReplyContextStartClass + `" data-draft-context-start>` +
			plainTextHTML(replyContextStart(c.Text)) + `…</span><span class="` + ui.ReplyContextMoreClass + `">Show more</span><span class="` + ui.ReplyContextLessClass + `">Show less</span></summary>
<p class="` + ui.ReplyContextTextClass + `" data-draft-context-text>` + plainTextHTML(c.Text) + `</p>
</details>
`)
	default:
		b.WriteString(`<p class="` + ui.ReplyContextTextClass + `" data-draft-context-text>` + plainTextHTML(c.Text) + `</p>
`)
	}
	if cut {
		b.WriteString(`<p class="text-xs text-fg2" data-draft-context-cut>` + replyContextCutNote + `</p>
`)
	}
	b.WriteString("</div>\n")
	return b.String()
}

// contextOneLine is t as one line (line breaks and runs of space folded to
// one space) for the feed's clamped start.
func contextOneLine(t string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(t, " ", " ")), " ")
}

// replyContextLinkHTML is the ⓘ link to the original at u, its host shown
// (look-alike letters marked) so a spoofed host is seen before the tap; "" for
// a URL that is not an absolute http(s) one (fence.WebURL, re-checked here
// whatever was stored). It opens in a new tab, with no referrer.
func replyContextLinkHTML(u string) string {
	u = fence.WebURL(strings.TrimSpace(u))
	if u == "" {
		return ""
	}
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return ""
	}
	return `<a href="` + html.EscapeString(u) + `" target="_blank" rel="noopener noreferrer nofollow" class="` + ui.ReplyContextLinkClass +
		`" title="Open the original" data-draft-context-link><span aria-hidden="true">ⓘ</span><span class="sr-only">Open the original on </span>` +
		stepUpMarkedHTML(p.Host) + `</a>`
}

// replyContextFolds reports whether context text t is long enough to fold.
func replyContextFolds(t string) bool {
	return utf8.RuneCountInString(t) > replyContextFoldRunes || strings.Count(t, "\n") >= replyContextFoldLines
}

// replyContextStart is a folded text's start: at most replyContextStartRunes
// runes and replyContextFoldLines-1 line breaks, cut back to a space when one
// is near the cut, trailing space trimmed.
func replyContextStart(t string) string {
	lines := strings.SplitAfter(t, "\n")
	if len(lines) > replyContextFoldLines-1 {
		lines = lines[:replyContextFoldLines-1]
	}
	start := strings.Join(lines, "")
	if utf8.RuneCountInString(start) > replyContextStartRunes {
		r := []rune(start)[:replyContextStartRunes]
		start = string(r)
		if i := strings.LastIndexAny(start, " \n"); i > len(start)-60 && i > 0 {
			start = start[:i]
		}
	}
	return strings.TrimRight(start, " \n\t")
}
