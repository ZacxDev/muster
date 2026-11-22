package plugins

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/fence"
)

// A REPLY'S CONTEXT (migration 0182, plugin_reply_drafts.reply_context): the
// post or message an officer's OUTBOUND reply answers — who wrote it, its
// whole text, when, and where it lives — as the officer read it upstream (a
// relay's read of an X status or a Reddit comment) and passed it with
// officer_post_draft. The owner's cards draw it above the draft, so they can
// see what the reply is to (the operator's ask, 2026-11-22).
//
// 🔴 IT IS AN OUTSIDE PARTY'S WORDS, AS AN OFFICER RELAYED THEM: UNTRUSTED. It
// is screened as an inbound plugin message is (MessageIn.Validate: UTF-8
// without NUL, the same caps, a URL only an absolute http(s) one), never by
// the outbound deny-word screen (it is not what muster sends), and every face
// escapes it — an agent-facing one fences it (internal/api). muster NEVER
// fetches anything for it: a reply filed without one says so on its card.
//
// Only an outbound REPLY carries one (validate; 0182's CHECK). It is stored
// once, at filing, and a retry chain's successor carries its root's.

// Bounds of a reply's context: an inbound message's (plugins.go).
const (
	MaxReplyContextTextBytes   = MaxBodyBytes
	MaxReplyContextAuthorRunes = MaxSenderRunes
)

// ReplyContext is what an outbound reply answers, as its officer filed it.
// Text is required; the rest may be absent.
type ReplyContext struct {
	// Author is who wrote it, as the venue names them ("@ada", "u/ada").
	Author string `json:"author,omitempty"`
	// Text is its whole text, line breaks kept.
	Text string `json:"text"`
	// WrittenAt is when it was written upstream (UTC), nil when not given.
	WrittenAt *time.Time `json:"written_at,omitempty"`
	// URL is where it lives upstream: an absolute http(s) URL (fence.WebURL).
	URL string `json:"url,omitempty"`
}

// DecodeReplyContext decodes officer_post_draft's `context` argument: nil for
// absent or null; written_at is RFC 3339. Its rules are validate's.
func DecodeReplyContext(raw json.RawMessage) (*ReplyContext, error) {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 || string(b) == "null" {
		return nil, nil
	}
	var w struct {
		Author    string `json:"author"`
		Text      string `json:"text"`
		WrittenAt string `json:"written_at"`
		URL       string `json:"url"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, invalid("context is {author?, text, written_at?, url?}, each a string")
	}
	c := &ReplyContext{Author: strings.TrimSpace(w.Author), Text: w.Text, URL: strings.TrimSpace(w.URL)}
	if at := strings.TrimSpace(w.WrittenAt); at != "" {
		t, err := time.Parse(time.RFC3339, at)
		if err != nil {
			return nil, invalid("context.written_at is an RFC 3339 time, e.g. 2026-11-22T14:05:00Z")
		}
		t = t.UTC()
		c.WrittenAt = &t
	}
	return c, nil
}

// validate checks c against an inbound message's rules (the file header).
func (c ReplyContext) validate() error {
	switch {
	case strings.TrimSpace(c.Text) == "":
		return invalid("context.text is the whole text of the post or message the reply answers")
	case len(c.Text) > MaxReplyContextTextBytes:
		return invalid("context.text is over %d bytes", MaxReplyContextTextBytes)
	case utf8.RuneCountInString(c.Author) > MaxReplyContextAuthorRunes:
		return invalid("context.author is over %d characters", MaxReplyContextAuthorRunes)
	case c.URL != "" && fence.WebURL(c.URL) == "":
		return invalid("context.url is an absolute http or https URL with a host, at most %d bytes, with no spaces, control or invisible characters", fence.MaxURLBytes)
	}
	for _, f := range []struct{ name, v string }{{"context.author", c.Author}, {"context.text", c.Text}} {
		// Postgres refuses invalid UTF-8 and NUL in text (jsonb as well).
		if !utf8.ValidString(f.v) || strings.ContainsRune(f.v, 0) {
			return invalid("%s must be UTF-8 text without NUL", f.name)
		}
	}
	return nil
}

// replyContextArg is c as FileOutbound's insert writes it: nil (SQL NULL) for
// none, else its JSON.
func replyContextArg(c *ReplyContext) (any, error) {
	if c == nil {
		return nil, nil
	}
	b, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}
