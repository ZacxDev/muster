package attention

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/askattach"
	"github.com/ZacxDev/muster/internal/fence"
)

// ---------------------------------------------------------------------------
// WHAT AN AGENT'S ASK CARRIES BESIDE ITS QUESTION (migration 0145)
//
//   - Attachments: files the agent attached (attention_ask's attachments /
//     attachment_ids), each an upload in muster's upload store (migration
//     0024) of the ask's own owner. internal/api checks that before it files
//     the ask; Clean checks the shape (a public id, a bounded name, a type and
//     a size) and the count.
//   - Why: why the ask is here. The agent's own `context` (Text), or — when it
//     gave none — the fallback muster read when the ask was filed (Focus, the
//     asking session's focus title; Last, its last assistant message, secrets
//     already redacted by the caller). Never both: Clean drops the fallback
//     when Text is set.
//
// 🔴 BOTH ARE THE ASKER'S, SO UNTRUSTED. Clean is where they are made safe to
// show (fence.Sanitize: no line break, no control, format, bidi or invisible
// code point; bounded), so the row, attention_list's JSON and every rendering
// carry the same text, and a renderer only escapes it for its medium.
// ---------------------------------------------------------------------------

// The bounds of an ask's attachments and why.
const (
	// MaxAttachments is how many files one ask may carry (0145's CHECK holds 4).
	MaxAttachments = askattach.MaxCount
	// MaxAttachmentNameRunes bounds an attachment's shown name.
	MaxAttachmentNameRunes = 120
	// MaxWhyRunes bounds the agent's own context: a longer one is REFUSED, as
	// an over-long question is.
	MaxWhyRunes = 500
	// MaxWhyFocusRunes and MaxWhyLastRunes bound the fallback's two fields; a
	// longer one is cut (muster built it, so there is nobody to refuse).
	MaxWhyFocusRunes = 200
	MaxWhyLastRunes  = 600
)

// Attachment is one file an ask carries: an upload of the ask's owner.
type Attachment struct {
	// ID is the upload's public id (uploads.Upload.PublicID); its object is
	// "uploads/" + ID.
	ID string `json:"id"`
	// Name is the upload's cleaned filename, as the owner is shown it.
	Name string `json:"name"`
	// Type is the type muster SNIFFED from the bytes when it stored them.
	Type string `json:"type"`
	Size int64  `json:"size"`
}

// AskWhy is why an ask is here: Text (the agent's own context), or the
// fallback Focus and Last. Exactly one of the two forms is ever stored.
type AskWhy struct {
	Text  string `json:"text,omitempty"`
	Focus string `json:"focus,omitempty"`
	Last  string `json:"last,omitempty"`
}

// FromAgent reports whether w is the agent's own words (Text), not muster's
// fallback.
func (w *AskWhy) FromAgent() bool { return w != nil && w.Text != "" }

// attachmentIDPattern is an upload public id (uploads.ValidPublicID's: 128
// random bits, unpadded base64url). Spelled here so this package does not
// import the upload store.
var attachmentIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)

// CleanAttachments is in as an ask stores it: each id an upload public id,
// none twice, at most MaxAttachments; each name sanitised and bounded (a blank
// one is "file"); each type one line; each size positive. nil for none.
func CleanAttachments(in []Attachment) ([]Attachment, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > MaxAttachments {
		return nil, fmt.Errorf("%w: at most %d attachments", ErrInvalid, MaxAttachments)
	}
	seen := map[string]bool{}
	out := make([]Attachment, 0, len(in))
	for _, a := range in {
		if !attachmentIDPattern.MatchString(a.ID) {
			return nil, fmt.Errorf("%w: an attachment's id is not an upload id", ErrInvalid)
		}
		if seen[a.ID] {
			return nil, fmt.Errorf("%w: attachment %s is listed twice", ErrInvalid, a.ID)
		}
		seen[a.ID] = true
		if a.Size <= 0 {
			return nil, fmt.Errorf("%w: attachment %s is empty", ErrInvalid, a.ID)
		}
		name := fence.Cut(strings.TrimSpace(fence.Sanitize(a.Name)), MaxAttachmentNameRunes)
		if name == "" {
			name = "file"
		}
		typ := fence.Cut(strings.TrimSpace(fence.Sanitize(a.Type)), 100)
		if typ == "" {
			typ = "application/octet-stream"
		}
		out = append(out, Attachment{ID: a.ID, Name: name, Type: typ, Size: a.Size})
	}
	return out, nil
}

// whyLine is s as one line of an ask's why: every line break a space, then
// sanitised (fence.Sanitize: a control, format, bidi or invisible code point
// becomes U+FFFD), runs of spaces folded, trimmed.
func whyLine(s string) string {
	return strings.Join(strings.Fields(fence.Sanitize(whyBreaks.Replace(s))), " ")
}

var whyBreaks = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")

// CleanWhy is w as an ask stores it. The agent's Text is one line, refused
// past MaxWhyRunes; when it is set, the fallback is dropped. Otherwise Focus
// and Last are one line each, cut to their bounds. nil when nothing is left.
func CleanWhy(w *AskWhy) (*AskWhy, error) {
	if w == nil {
		return nil, nil
	}
	if text := whyLine(w.Text); text != "" {
		if utf8.RuneCountInString(text) > MaxWhyRunes {
			return nil, fmt.Errorf("%w: the context is longer than %d characters", ErrInvalid, MaxWhyRunes)
		}
		return &AskWhy{Text: text}, nil
	}
	out := AskWhy{Focus: fence.Cut(whyLine(w.Focus), MaxWhyFocusRunes), Last: fence.Cut(whyLine(w.Last), MaxWhyLastRunes)}
	if out.Focus == "" && out.Last == "" {
		return nil, nil
	}
	return &out, nil
}
