package plugins

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestAReplysContextIsScreenedAsAnInboundMessage: officer_post_draft's
// context decodes ({author?, text, written_at?, url?}, written_at RFC 3339),
// rides only on a reply, and is held to an inbound message's rules — text
// required and at most 16 KiB, an author of at most 200 characters, UTF-8
// without NUL, a URL only an absolute http(s) one.
func TestAReplysContextIsScreenedAsAnInboundMessage(t *testing.T) {
	reply := NewOutbound{Officer: "example:main", Plugin: "example-relay", Venue: "x",
		Target: OutboundTarget{Kind: TargetReply, Ref: "2122495152350453825"}, Body: "It does."}
	ctxOf := func(raw string) *ReplyContext {
		t.Helper()
		c, err := DecodeReplyContext(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		return c
	}
	good := ctxOf(`{"author": " @ada ", "text": "Does it run on a Pi?", "written_at": "2026-11-22T09:05:00-05:00", "url": "https://x.com/ada/status/1"}`)
	if good.Author != "@ada" || good.WrittenAt == nil || !good.WrittenAt.Equal(time.Date(2026, 11, 22, 14, 5, 0, 0, time.UTC)) ||
		good.WrittenAt.Location() != time.UTC || good.URL != "https://x.com/ada/status/1" {
		t.Fatalf("decoded: %+v", good)
	}
	in := reply
	in.Context = good
	if err := in.validate(); err != nil {
		t.Fatalf("control: a reply with its context: %v", err)
	}
	if c := ctxOf(`null`); c != nil {
		t.Errorf("null decoded to %+v", c)
	}
	if c := ctxOf(``); c != nil {
		t.Errorf("absent decoded to %+v", c)
	}
	for _, raw := range []string{`{"text": "x", "written_at": "yesterday"}`, `"just a string"`, `{"text": 7}`} {
		if _, err := DecodeReplyContext(json.RawMessage(raw)); !errors.Is(err, ErrInvalid) {
			t.Errorf("decode %s: %v, want ErrInvalid", raw, err)
		}
	}
	for _, bad := range []struct {
		why string
		in  NewOutbound
		// want is a word of THIS refusal's error, so a case refused for
		// another reason (an earlier check) does not pass for this one.
		want string
	}{
		{"context on a post", func() NewOutbound {
			n := reply
			n.Target, n.Context = OutboundTarget{Kind: TargetPost}, good
			return n
		}(), "only a reply carries context"},
		{"context on a like", func() NewOutbound {
			n := reply
			n.Target, n.Body, n.Reason, n.BecauseRef, n.Context = OutboundTarget{Kind: TargetLike, Ref: "1"}, "", "liked it", "1", good
			return n
		}(), "only a reply carries context"},
		{"a blank text", withContext(reply, ReplyContext{Text: "  \n "}), "context.text is the whole text"},
		{"a text over 16 KiB", withContext(reply, ReplyContext{Text: strings.Repeat("a", MaxReplyContextTextBytes+1)}), "context.text is over"},
		{"an author over 200 characters", withContext(reply, ReplyContext{Text: "x", Author: strings.Repeat("é", MaxReplyContextAuthorRunes+1)}), "context.author is over"},
		{"a NUL in the text", withContext(reply, ReplyContext{Text: "a\x00b"}), "context.text must be UTF-8"},
		{"invalid UTF-8 in the author", withContext(reply, ReplyContext{Text: "x", Author: "\xff"}), "context.author must be UTF-8"},
		{"a javascript: URL", withContext(reply, ReplyContext{Text: "x", URL: "javascript:alert(1)"}), "context.url"},
		{"a relative URL", withContext(reply, ReplyContext{Text: "x", URL: "/ada/status/1"}), "context.url"},
		{"a URL with a space", withContext(reply, ReplyContext{Text: "x", URL: "https://x.com/a b"}), "context.url"},
	} {
		if err := bad.in.validate(); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%s: %v, want ErrInvalid saying %q", bad.why, err, bad.want)
		}
	}
	// At the bounds: exactly 16 KiB and 200 characters pass.
	if err := withContext(reply, ReplyContext{Text: strings.Repeat("a", MaxReplyContextTextBytes),
		Author: strings.Repeat("é", MaxReplyContextAuthorRunes)}).validate(); err != nil {
		t.Errorf("a context at its bounds: %v", err)
	}
	// Outside text keeps what an inbound message keeps: HTML and hidden
	// characters are stored as they came (every face escapes and flags them).
	if err := withContext(reply, ReplyContext{Text: "<script>alert(1)</script> ‮"}).validate(); err != nil {
		t.Errorf("outside text refused for its characters: %v", err)
	}
}

func withContext(n NewOutbound, c ReplyContext) NewOutbound {
	n.Context = &c
	return n
}
