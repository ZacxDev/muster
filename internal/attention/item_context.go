package attention

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/fence"
)

// ---------------------------------------------------------------------------
// WHAT AN OFFICER'S ASK IS ABOUT (migration 0125, attention_asks.item_context)
//
// A builtin officer's ask (officer_ask_person) or escalation notice is the
// officer's own prose. What the owner needs to judge it — which item, and for
// an item a plugin message became, that message as the outside sender wrote
// it — is muster's to show, read from the item when the ask is raised, never
// from the officer's words. It is stored with the ask and drawn APART from the
// question (internal/api officerContextHTML), which stays "the officer's note".
//
// 🔴 THE SOURCE IS UNTRUSTED AND CLEAN IS WHERE IT IS MADE SAFE TO SHOW. Every
// one-line field passes fence.Sanitize (no line break, no control, format,
// bidi or invisible code point) and is bounded; the body keeps its line breaks
// but nothing else fence.Unsafe, and is bounded in runes AND lines with
// Truncated set; a url that is not fence.WebURL's is dropped. So the
// stored row, attention_list's JSON and every rendering carry the same safe
// text, and a renderer only has to escape it for its medium.
// ---------------------------------------------------------------------------

// The bounds of an ItemContext.
const (
	// MaxContextFieldRunes bounds each one-line field.
	MaxContextFieldRunes = 200
	// MaxSourceBodyRunes and MaxSourceBodyLines bound the source message's
	// body as an ask shows it; a longer one is cut, with Truncated set.
	MaxSourceBodyRunes = 1500
	MaxSourceBodyLines = 30
)

// ItemContext is the officer item an ask is about, as muster read it.
type ItemContext struct {
	Officer string `json:"officer"`
	ItemID  int64  `json:"item_id"`
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	// Source is the plugin message the item is, nil for an item no plugin
	// message became.
	Source *SourceMessage `json:"source,omitempty"`
}

// SourceMessage is the plugin message an officer item is: UNTRUSTED, an
// outside sender's words.
type SourceMessage struct {
	Plugin    string `json:"plugin"`
	MessageID int64  `json:"message_id"`
	Venue     string `json:"venue,omitempty"`
	Thread    string `json:"thread,omitempty"`
	Sender    string `json:"sender,omitempty"`
	// SenderVerified is the plugin's assertion that it authenticated Sender;
	// false is shown UNVERIFIED.
	SenderVerified bool   `json:"sender_verified"`
	Body           string `json:"body,omitempty"`
	// Truncated: Body is the first MaxSourceBodyRunes runes or
	// MaxSourceBodyLines lines of a longer body.
	Truncated bool `json:"truncated,omitempty"`
	// URL is the message's url when the plugin sent one: an absolute http(s)
	// URL, shown as text, never as a link.
	URL string `json:"url,omitempty"`
	// Unread: muster could not read the message when it raised the ask, so
	// Sender and Body are empty (the item's venue and thread are still shown).
	Unread bool `json:"unread,omitempty"`
}

// oneLine is s as a one-line context field: sanitised, trimmed, bounded.
func oneLine(s string) string {
	return fence.Cut(strings.TrimSpace(fence.Sanitize(s)), MaxContextFieldRunes)
}

// SourceBody is body as an ask shows it: CRLF folded, every fence.Unsafe code
// point but the line break replaced (fence.Sanitize per line), blank edges
// trimmed, then cut to MaxSourceBodyLines lines and MaxSourceBodyRunes runes;
// cut reports whether anything was dropped.
func SourceBody(body string) (out string, cut bool) {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(fence.Sanitize(l), " ")
	}
	out = strings.Trim(strings.Join(lines, "\n"), "\n ")
	if lines = strings.Split(out, "\n"); len(lines) > MaxSourceBodyLines {
		out, cut = strings.Join(lines[:MaxSourceBodyLines], "\n"), true
	}
	if utf8.RuneCountInString(out) > MaxSourceBodyRunes {
		out, cut = fence.Cut(out, MaxSourceBodyRunes), true
	}
	return out, cut
}

// clean is c as an ask stores it (the package comment's rules); an error only
// for a context that names no officer or item, which no muster path builds.
func (c *ItemContext) clean() (*ItemContext, error) {
	if c == nil {
		return nil, nil
	}
	out := ItemContext{Officer: oneLine(c.Officer), ItemID: c.ItemID, Kind: oneLine(c.Kind), Subject: oneLine(c.Subject)}
	if out.Officer == "" || out.ItemID <= 0 {
		return nil, fmt.Errorf("%w: an item context names its officer and item", ErrInvalid)
	}
	if s := c.Source; s != nil {
		src := SourceMessage{Plugin: oneLine(s.Plugin), MessageID: s.MessageID, Venue: oneLine(s.Venue), Thread: oneLine(s.Thread),
			Sender: oneLine(s.Sender), SenderVerified: s.SenderVerified, URL: fence.WebURL(s.URL), Unread: s.Unread}
		src.Body, src.Truncated = SourceBody(s.Body)
		src.Truncated = src.Truncated || s.Truncated
		out.Source = &src
	}
	return &out, nil
}
