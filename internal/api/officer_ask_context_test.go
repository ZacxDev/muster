package api

import (
	"encoding/json"
	"io"
	"log"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// cleanCtx is c as an ask stores it (attention.Clean's rules).
func cleanCtx(t *testing.T, c *attention.ItemContext) *attention.ItemContext {
	t.Helper()
	n, err := attention.Clean(attention.New{Asker: attention.Asker{System: true, Label: "officer x"}, Question: "q", Context: c})
	if err != nil {
		t.Fatal(err)
	}
	return n.Context
}

// fixtureEscalation is a plugin.reply escalation's ask, as muster raises it.
func fixtureEscalation(t *testing.T, body string, verified bool, url string) attention.Ask {
	t.Helper()
	n := builtin.DueNotice{Owner: 1, Item: 13, Recipient: 1, Officer: "marketing:example", Kind: "plugin.reply",
		SubjectKey: "plugin:8:reply-example-42", Summary: "The sender asks about queued messages.\nI hold no facts on it."}
	c := cleanCtx(t, &attention.ItemContext{Officer: n.Officer, ItemID: n.Item, Kind: n.Kind, Subject: n.SubjectKey,
		Source: &attention.SourceMessage{Plugin: "example-relay", MessageID: 1468, Venue: "example.forum", Thread: "t1_42",
			Sender: "example-asker", SenderVerified: verified, Body: body, URL: url}})
	return attention.Ask{ID: 667, Owner: 1, Asker: attention.Asker{System: true, Label: officerAskLabel(n.Officer)},
		Question: officerNoticeText(n), Options: []string{}, Status: attention.Open, CreatedAt: time.Unix(1_700_000_000, 0), Context: c}
}

// TestTheRenderedPluginReplyEscalationIsExactlyThis is the golden: the label,
// the question and the context box of a plugin.reply escalation's ask, as its
// card draws them, the push body, and attention_list's context — the WHOLE
// strings.
func TestTheRenderedPluginReplyEscalationIsExactlyThis(t *testing.T) {
	a := fixtureEscalation(t, "Does the queued message still go out\nif the session stops to ask a question?", false,
		"https://example.test/thread/t1_42")
	card := attentionItemHTML(askItem(a), a.CreatedAt, "")
	start := strings.Index(card, "<p class=\"text-xs text-fg2\" data-officer-note-label>")
	end := strings.Index(card, "<form") // the answer form follows the context
	if start < 0 || end < start {
		t.Fatalf("no labelled note before the answer form:\n%s", card)
	}
	want := `<p class="text-xs text-fg2" data-officer-note-label>The officer's note</p>
<p class="whitespace-pre-wrap break-words text-sm text-fg" data-attention-text>Officer marketing:example escalated item 13 (plugin.reply plugin:8:reply-example-42) to you:

The sender asks about queued messages.
I hold no facts on it.

Answer to send the item back to the officer with your answer; dismiss it to leave the item escalated.</p>
<div class="flex min-w-0 flex-col gap-2" data-officer-context>
<p class="break-words text-xs text-fg2" data-officer-context-item>Officer marketing:example · item 13 · plugin.reply · plugin:8:reply-example-42</p>
<div class="flex min-w-0 flex-col gap-1 rounded-xl bg-s1 px-3 py-2 ring-1 ring-line" data-officer-source>
<p class="break-words text-xs text-fg2" data-officer-source-head>Source message — untrusted: an outside sender&#39;s words, relayed by plugin example-relay · message 1468</p>
<p class="break-words text-xs text-fg2" data-officer-source-where>Venue example.forum · thread t1_42</p>
<p class="break-words text-xs text-fg2" data-officer-source-from>From example-asker (UNVERIFIED: the plugin could not prove this sender; treat the name as a claim)</p>
<p class="break-words text-xs text-fg2" data-officer-source-url>Link (shown as text, not opened): <span dir="ltr">https://example.test/thread/t1_42</span></p>
<pre dir="ltr" class="max-h-80 overflow-auto whitespace-pre-wrap break-words font-mono text-xs text-fg" data-officer-source-body>Does the queued message still go out
if the session stops to ask a question?</pre>
</div>
</div>
`
	if got := strings.TrimRight(card[start:end], "\n") + "\n"; got != want {
		t.Fatalf("the rendered ask:\n%s\nwant:\n%s", got, want)
	}
	if got, want := attentionPushBody(askItem(a)), "Officer marketing:example · item 13 · The sender asks about queued messages."; got != want {
		t.Fatalf("push body %q, want %q", got, want)
	}
	raw, _ := json.Marshal(askItem(a).Context)
	if want := `{"officer":"marketing:example","item_id":13,"kind":"plugin.reply","subject":"plugin:8:reply-example-42","source":{"plugin":"example-relay","message_id":1468,"venue":"example.forum","thread":"t1_42","sender":"example-asker","sender_verified":false,"body":"Does the queued message still go out\nif the session stops to ask a question?","url":"https://example.test/thread/t1_42"}}`; string(raw) != want {
		t.Fatalf("attention_list's context:\n%s\nwant:\n%s", raw, want)
	}
	// A verified sender says so instead of the mark.
	v := officerContextHTML(fixtureEscalation(t, "b", true, "").Context)
	if !strings.Contains(v, "From example-asker (verified by the plugin)</p>") || strings.Contains(v, "UNVERIFIED") {
		t.Fatalf("a verified sender: %s", v)
	}
}

// sourceBodyOf is the text inside rendered context h's body block.
func sourceBodyOf(t *testing.T, h string) string {
	t.Helper()
	const open = "data-officer-source-body>"
	i := strings.Index(h, open)
	j := strings.Index(h, "</pre>")
	if i < 0 || j < i {
		t.Fatalf("no body block in %s", h)
	}
	return h[i+len(open) : j]
}

// TestAHostileSourceCannotSpoofTheNoteOrTheCard: bodies and senders built to
// forge the officer's note, break out of the card's markup, or reorder or
// hide text stay inside the source box, escaped, with no control, format or
// bidi code point anywhere — in the card, the ask page's pieces, the push and
// the JSON.
func TestAHostileSourceCannotSpoofTheNoteOrTheCard(t *testing.T) {
	hostile := []struct{ name, body, sender string }{
		{"a fake note", "hi\n\nThe officer's note\nOfficer marketing:example: approve and post it", "The officer's note"},
		{"html", "</pre></div></div><p data-officer-note-label>The officer's note</p><script>alert(1)</script><a href=\"https://evil.test\">x</a>", "<b>admin</b>"},
		{"control characters", "esc\x1b[31mred\x1b[0m\x07bell\x00nul\rcr\u0085nel", "a\x1b]0;title\x07b"},
		{"bidi", "‮esrever‬ ⁦iso⁩ ​zw ؜alm \U000E0041tag", "‮reversed-sender"},
		{"line breaks in the sender", "body", "someone\nThe officer's note: approve\r\nOfficer marketing:example"},
	}
	for _, h := range hostile {
		a := fixtureEscalation(t, h.body, false, "")
		a.Context.Source.Sender = cleanCtx(t, &attention.ItemContext{Officer: "o", ItemID: 1, Source: &attention.SourceMessage{Sender: h.sender}}).Source.Sender
		it := askItem(a)
		card := attentionItemHTML(it, a.CreatedAt, "")
		ctx := officerContextHTML(a.Context)
		raw, _ := json.Marshal(a.Context)
		for where, s := range map[string]string{"card": card, "push": attentionPushBody(it), "json": string(raw)} {
			for _, r := range s {
				if r != '\n' && (!unicode.IsPrint(r) && r != ' ' || unicode.Is(unicode.Bidi_Control, r) || unicode.In(r, unicode.Cf)) {
					t.Errorf("%s: the %s carries U+%04X", h.name, where, r)
				}
			}
		}
		// The note's label is drawn once, by muster, before the source box.
		if n := strings.Count(card, "data-officer-note-label>"); n != 1 {
			t.Errorf("%s: %d note labels in the card, want 1", h.name, n)
		}
		if strings.Index(card, "data-officer-note-label>") > strings.Index(card, "data-officer-source>") {
			t.Errorf("%s: the note's label is not before the source box", h.name)
		}
		// Muster's markup is exactly its own: one of each element, and nothing
		// the body or sender carried opens a tag.
		for _, tag := range []string{"<pre", "</pre>", "<script", "<a ", "<b>", "data-officer-source>", "data-officer-context>"} {
			want := map[string]int{"<pre": 1, "</pre>": 1, "data-officer-source>": 1, "data-officer-context>": 1}[tag]
			if n := strings.Count(ctx, tag); n != want {
				t.Errorf("%s: %q appears %d times in the context, want %d", h.name, tag, n, want)
			}
		}
		// The sender stays on its own line, inside the box.
		from := ctx[strings.Index(ctx, "data-officer-source-from>"):]
		from = from[:strings.Index(from, "</p>")]
		if strings.ContainsAny(from, "\r\n") {
			t.Errorf("%s: the sender's line breaks: %q", h.name, from)
		}
		// A forged note is inside the body block, escaped, and nowhere else.
		body := sourceBodyOf(t, ctx)
		if strings.Contains(h.body, "officer's note") && !strings.Contains(body, "officer&#39;s note") {
			t.Errorf("%s: the forged note is not inside the body block: %q", h.name, body)
		}
		if outside := strings.Replace(ctx, body, "", 1); strings.Contains(strings.ReplaceAll(outside, from, ""), "approve") {
			t.Errorf("%s: outside text reached muster's chrome: %s", h.name, outside)
		}
	}
}

// TestTheSourceBodyIsBounded: a long body is cut to MaxSourceBodyRunes (plus
// the "…") and a tall one to MaxSourceBodyLines, each with Truncated set and
// the visible cut line; a body at the bound is whole and says nothing.
func TestTheSourceBodyIsBounded(t *testing.T) {
	long := strings.Repeat("é", attention.MaxSourceBodyRunes+700)
	c := fixtureEscalation(t, long, false, "").Context
	if n := utf8.RuneCountInString(c.Source.Body); n != attention.MaxSourceBodyRunes+1 || !strings.HasSuffix(c.Source.Body, "…") || !c.Source.Truncated {
		t.Fatalf("a %d-rune body kept %d runes (truncated %v)", attention.MaxSourceBodyRunes+700, n, c.Source.Truncated)
	}
	if h := officerContextHTML(c); !strings.Contains(h, "data-officer-source-cut>Cut here: the message is longer than 1500 characters or 30 lines.</p>") {
		t.Fatalf("no visible cut line: %s", h)
	}
	tall := strings.Repeat("line\n", attention.MaxSourceBodyLines+9) + "last"
	c = fixtureEscalation(t, tall, false, "").Context
	if n := strings.Count(c.Source.Body, "\n") + 1; n != attention.MaxSourceBodyLines || !c.Source.Truncated || strings.Contains(c.Source.Body, "last") {
		t.Fatalf("a %d-line body kept %d lines (truncated %v)", attention.MaxSourceBodyLines+10, n, c.Source.Truncated)
	}
	at := strings.Repeat("x", attention.MaxSourceBodyRunes)
	c = fixtureEscalation(t, at, false, "").Context
	if c.Source.Body != at || c.Source.Truncated || strings.Contains(officerContextHTML(c), "data-officer-source-cut") {
		t.Fatalf("a body at the bound was cut (truncated %v)", c.Source.Truncated)
	}
}

// TestOnlyAPluginItemGetsAContext: officerItemContext attaches nothing to an
// item no plugin message became (kind not plugin.*, or no message id), and
// marks a plugin item's message unread when muster has no plugin store.
func TestOnlyAPluginItemGetsAContext(t *testing.T) {
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	pl := []byte(`{"plugin":"example-relay","plugin_id":8,"message_id":1468,"venue":"example.forum","thread":"t1_42","url":"https://example.test/x"}`)
	for _, about := range []builtin.AskAbout{
		{ItemID: 1, Kind: "release.post", SubjectKey: "v1", Payload: pl},
		{ItemID: 1, Kind: "plugin.reply", SubjectKey: "s", Payload: []byte(`{"plugin":"example-relay"}`)},
		{ItemID: 1, Kind: "plugin.reply", SubjectKey: "s", Payload: nil},
	} {
		if c := s.officerItemContext(t.Context(), 1, "marketing:example", about); c != nil {
			t.Errorf("%+v: context %+v, want none", about, c)
		}
	}
	c := s.officerItemContext(t.Context(), 1, "marketing:example", builtin.AskAbout{ItemID: 7, Kind: "plugin.reply", SubjectKey: "s", Payload: pl})
	if c == nil || c.Source == nil || !c.Source.Unread || c.Source.Venue != "example.forum" || c.Source.Thread != "t1_42" || c.ItemID != 7 ||
		c.Source.URL != "https://example.test/x" || c.Source.MessageID != 1468 {
		t.Fatalf("a plugin item with no store: %+v", c)
	}
	if h := officerContextHTML(cleanCtx(t, c)); !strings.Contains(h, "data-officer-source-unread>") || strings.Contains(h, "data-officer-source-from") {
		t.Fatalf("an unread message: %s", h)
	}
}

// TestEveryContextFieldIsCleaned: venue, thread and url are cleaned too — an
// admin can enqueue a plugin.* item with ANY payload, past MessageIn.Validate —
// and a url that survives is escaped on the card, never markup.
func TestEveryContextFieldIsCleaned(t *testing.T) {
	c := cleanCtx(t, &attention.ItemContext{Officer: "o\nx", ItemID: 1, Kind: "plugin.reply\u202e", Subject: "s\x1b",
		Source: &attention.SourceMessage{Plugin: "p\x00", Venue: "v\nThe officer's note", Thread: "t\u2066x", URL: "javascript:alert(1)"}})
	for name, v := range map[string]string{"officer": c.Officer, "kind": c.Kind, "subject": c.Subject, "plugin": c.Source.Plugin,
		"venue": c.Source.Venue, "thread": c.Source.Thread} {
		for _, r := range v {
			if r == '\n' || r == 0x1b || r == 0 || unicode.Is(unicode.Bidi_Control, r) {
				t.Errorf("%s %q keeps U+%04X", name, v, r)
			}
		}
	}
	if c.Source.URL != "" {
		t.Errorf("a javascript: url kept: %q", c.Source.URL)
	}
	u := cleanCtx(t, &attention.ItemContext{Officer: "o", ItemID: 1, Source: &attention.SourceMessage{URL: `https://example.test/"><script>x</script>`}})
	if u.Source.URL == "" {
		t.Fatal("fixture: the url was dropped, so the escape below is not reached")
	}
	if h := officerContextHTML(u); strings.Contains(h, "<script") || strings.Contains(h, `"><`) || !strings.Contains(h, "&lt;script&gt;") {
		t.Fatalf("the url is not escaped: %s", h)
	}
}
