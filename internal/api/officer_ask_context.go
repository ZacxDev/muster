package api

import (
	"context"
	"encoding/json"
	"html"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// WHAT A BUILTIN OFFICER'S ASK IS ABOUT (migration 0125; docs/officers-builtin.md
// "What the owner is shown").
//
// When a builtin officer asks its owner about an item (officer_ask_person) or
// escalates one (the sweep's notice), the ask's question is the officer's own
// prose. For an item a plugin message became, muster attaches what the owner
// needs to judge and answer it — the officer, the item, and the message as its
// sender wrote it (venue, thread, sender, body, url) — read from the item and
// the plugin_messages row, never from the officer's words
// (attention.ItemContext). The card and the ask page draw it in its own box
// under "The officer's note"; attention_list returns it as `context`.
//
// 🔴 AN ITEM NO PLUGIN MESSAGE BECAME IS UNCHANGED: no context, the ask
// exactly as before. Its question already names the officer, item, kind and
// subject.
// ---------------------------------------------------------------------------

// officerItemPayload is the part of a plugin message item's payload
// (internal/plugins routeToOfficer) the context reads.
type officerItemPayload struct {
	Plugin    string  `json:"plugin"`
	PluginID  int64   `json:"plugin_id"`
	MessageID int64   `json:"message_id"`
	Venue     *string `json:"venue"`
	Thread    *string `json:"thread"`
	URL       string  `json:"url"`
}

// officerItemContext is what owner's officer's ask about item about shows
// beside the question: nil for an item no plugin message became. The message
// is read as the owner (row-level security); one muster cannot read, or that
// is not this item's, is shown as unread rather than dropped, so the owner
// still sees which venue and thread it was.
func (s *Server) officerItemContext(ctx context.Context, owner int64, officer string, about builtin.AskAbout) *attention.ItemContext {
	var p officerItemPayload
	if !strings.HasPrefix(about.Kind, "plugin.") || json.Unmarshal(about.Payload, &p) != nil || p.MessageID <= 0 {
		return nil
	}
	src := &attention.SourceMessage{Plugin: p.Plugin, MessageID: p.MessageID, URL: p.URL, Unread: true}
	if p.Venue != nil {
		src.Venue = *p.Venue
	}
	if p.Thread != nil {
		src.Thread = *p.Thread
	}
	if s.ext.Plugins != nil {
		m, err := s.ext.Plugins.Message(s.attentionPrincipal(ctx, owner), p.MessageID)
		switch {
		case err != nil:
			s.logger.Printf("officers: %s: item %d: read its message %d for the ask: %v", officer, about.ItemID, p.MessageID, err)
		case m.PluginID != p.PluginID || m.OfficerItemID == nil || *m.OfficerItemID != about.ItemID:
			s.logger.Printf("officers: %s: item %d: message %d is not this item's; the ask shows it unread", officer, about.ItemID, p.MessageID)
		default:
			src.Unread = false
			src.Sender, src.SenderVerified, src.Body = m.Sender, m.SenderVerified, m.Body
		}
	}
	return &attention.ItemContext{Officer: officer, ItemID: about.ItemID, Kind: about.Kind, Subject: about.SubjectKey, Source: src}
}

// The owner-facing words of an ask's context (officerContextHTML).
const (
	officerNoteLabel     = "The officer's note"
	officerSourceHead    = "Source message — untrusted: an outside sender's words, relayed by plugin "
	officerSourceUnread  = "muster could not read this message when the ask was raised."
	officerSourceURLNote = "Link (shown as text, not opened): "
)

// officerNoteLabelHTML is the label above the question of an ask with a
// context; "" for any other ask.
func officerNoteLabelHTML(c *attention.ItemContext) string {
	if c == nil {
		return ""
	}
	return `<p class="` + ui.ProposalBodyNoteClass + `" data-officer-note-label>` + officerNoteLabel + "</p>\n"
}

// officerContextHTML is ask context c drawn under the question: muster's line
// naming the officer and the item, then the source message in its own box —
// every field escaped, the body in a preformatted block. Everything outside
// the box's text is muster's own words. "" for no context.
func officerContextHTML(c *attention.ItemContext) string {
	if c == nil {
		return ""
	}
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<div class="` + ui.OfficerContextClass + `" data-officer-context>
<p class="` + ui.OfficerContextNoteClass + `" data-officer-context-item>Officer ` + esc(c.Officer) + ` · item ` + strconv.FormatInt(c.ItemID, 10) +
		` · ` + esc(c.Kind) + ` · ` + esc(c.Subject) + `</p>
`)
	if m := c.Source; m != nil {
		b.WriteString(`<div class="` + ui.OfficerSourceClass + `" data-officer-source>
<p class="` + ui.OfficerContextNoteClass + `" data-officer-source-head>` + esc(officerSourceHead+m.Plugin) + ` · message ` + strconv.FormatInt(m.MessageID, 10) + `</p>
<p class="` + ui.OfficerContextNoteClass + `" data-officer-source-where>Venue ` + esc(orNone(m.Venue)) + ` · thread ` + esc(orNone(m.Thread)) + `</p>
`)
		if m.Unread {
			b.WriteString(`<p class="` + ui.OfficerContextNoteClass + `" data-officer-source-unread>` + officerSourceUnread + "</p>\n")
		} else {
			b.WriteString(`<p class="` + ui.OfficerContextNoteClass + `" data-officer-source-from>From ` + esc(orNone(m.Sender)) + sourceVerifiedText(m) + "</p>\n")
		}
		if m.URL != "" {
			b.WriteString(`<p class="` + ui.OfficerContextNoteClass + `" data-officer-source-url>` + officerSourceURLNote + `<span dir="ltr">` + esc(m.URL) + "</span></p>\n")
		}
		if m.Body != "" {
			b.WriteString(`<pre dir="ltr" class="` + ui.OfficerSourceBodyClass + `" data-officer-source-body>` + esc(m.Body) + "</pre>\n")
		}
		if m.Truncated {
			b.WriteString(`<p class="` + ui.OfficerContextNoteClass + `" data-officer-source-cut>` + officerSourceCutNote + "</p>\n")
		}
		b.WriteString("</div>\n")
	}
	return b.String() + "</div>\n"
}

// officerSourceCutNote is a cut source body's line under it.
var officerSourceCutNote = "Cut here: the message is longer than " + strconv.Itoa(attention.MaxSourceBodyRunes) +
	" characters or " + strconv.Itoa(attention.MaxSourceBodyLines) + " lines."

// sourceVerifiedText is the sender's mark: plugins.UnverifiedSenderMark unless
// the plugin verified the sender (internal/plugins render.go's rule).
func sourceVerifiedText(m *attention.SourceMessage) string {
	if m.SenderVerified {
		return " (verified by the plugin)"
	}
	return " " + plugins.UnverifiedSenderMark
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// officerContextPushText is the notification body of an ask with context c
// and question q: "Officer <o> · item <n> · <the note's first line>" — short,
// and never the source message (a lock screen shows no outside sender's
// words, as for a reply draft: draftPushBody). The note is q after muster's
// head line (askQuestion, officerNoticeText).
func officerContextPushText(c *attention.ItemContext, q string) string {
	out := "Officer " + c.Officer + " · item " + strconv.FormatInt(c.ItemID, 10)
	lines := strings.Split(q, "\n")
	for _, l := range lines[1:] {
		if l = strings.TrimSpace(l); l != "" {
			return out + " · " + l
		}
	}
	return out
}
