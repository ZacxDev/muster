package attention

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/fence"
)

// ---------------------------------------------------------------------------
// A STEP-UP CONFIRMATION CARD'S STRUCTURED PARTS (migration 0171,
// attention_asks.step_up; internal/api host_stepup_card.go).
//
// A step-up card (internal/api hostStepUpGate) asks the owner to confirm, on
// their paired phone, one owner-authority call a session made. What the owner
// needs on its face is WHAT is about to happen and WHY — for a post, the post
// itself. The host, the session, the security reason and the call's hash are
// on the record, but folded away (Details).
//
// 🔴 ONE SOURCE. The card's question — what its push body, its page and the
// phone's step-up prompt (`confirms`) all read — is StepUpCard.Text(), and
// Clean SETS the question from it whenever a card carries one: no caller can
// file a step-up card whose question says something its parts do not.
//
// 🔴 THE PARTS CARRY UNTRUSTED TEXT. A post's body is an officer's words, a
// session's label is a window's. Clean is where they are made safe to show
// (fence.Sanitize: no control, format, bidi or invisible code point; one-line
// fields have no line break; everything bounded), so a renderer only escapes
// them for its medium.
// ---------------------------------------------------------------------------

// The bounds of a StepUpCard. A card's Text must fit MaxQuestionRunes: the
// sum of these (with its separators) stays under it.
const (
	MaxStepUpTitleRunes   = 200
	MaxStepUpWhyRunes     = 300
	MaxStepUpAskerRunes   = 200
	MaxStepUpVenueRunes   = 64
	MaxStepUpToolRunes    = 64
	MaxStepUpPreviewRunes = 2600
	MaxStepUpDetailsRunes = 3000
	MaxStepUpMedia        = 4
	MaxStepUpMediumRunes  = 150
)

// StepUpCard is a step-up card's parts, as muster built them when it filed
// the card. Only muster files one (a System asker).
type StepUpCard struct {
	// Tool is the gated tool's name (e.g. plugin_draft_decide).
	Tool string `json:"tool"`
	// Title is the card's short action title: "Post to X?", "Start this
	// agent?". The push title.
	Title string `json:"title"`
	// Why is the one line that says why it is being asked: a draft's officer
	// item, else the asking session.
	Why string `json:"why"`
	// Asker names who asked (the asking session), drawn as a small label.
	Asker string `json:"asker,omitempty"`
	// Preview is what is about to happen: a post's exact text, or a plain
	// summary of the call. Line breaks kept.
	Preview string `json:"preview,omitempty"`
	// Venue is where a post goes, "" for any other card.
	Venue string `json:"venue,omitempty"`
	// DraftID is the draft a plugin_draft_decide card is bound to (its
	// Media are served from it), 0 for any other card.
	DraftID int64 `json:"draft_id,omitempty"`
	// Media are the draft's media the card shows, in the draft's order.
	Media []StepUpMedium `json:"media,omitempty"`
	// Changed: the draft the call names is not what it names (its body or
	// media SHA-256 differ, or it could not be read), so the card shows no
	// preview and says to refuse.
	Changed bool `json:"changed,omitempty"`
	// Details is the boilerplate, on the record: the host, the session, the
	// call and its arguments, why the phone is needed, and the call's hash.
	Details string `json:"details"`
}

// StepUpMedium is one medium of a draft a card shows: its index among the
// draft's refs (what the draft's media route serves), its ref, type and hash.
type StepUpMedium struct {
	N      int    `json:"n"`
	Ref    string `json:"ref"`
	Type   string `json:"type"`
	SHA256 string `json:"sha256"`
}

// Video reports whether m is a video (else it is drawn as an image).
func (m StepUpMedium) Video() bool { return strings.HasPrefix(m.Type, "video/") }

// MediaLine is the card's one line about its media, "" for none:
// "With 1 image" / "With 2 images and 1 video".
func (c *StepUpCard) MediaLine() string {
	if c == nil || len(c.Media) == 0 {
		return ""
	}
	img, vid := 0, 0
	for _, m := range c.Media {
		if m.Video() {
			vid++
		} else {
			img++
		}
	}
	count := func(n int, one string) string {
		if n == 1 {
			return "1 " + one
		}
		return fmt.Sprintf("%d %ss", n, one)
	}
	var parts []string
	if img > 0 {
		parts = append(parts, count(img, "image"))
	}
	if vid > 0 {
		parts = append(parts, count(vid, "video"))
	}
	return "With " + strings.Join(parts, " and ")
}

// Text is the card's question: its title, its why, then its preview and its
// media line. No host, no session, no hash, no JSON — those are Details.
// THE ONE TEXT the push, the page's summary and the phone's prompt read.
func (c *StepUpCard) Text() string {
	if c == nil {
		return ""
	}
	parts := []string{c.Title + "\n" + c.Why}
	if c.Preview != "" {
		parts = append(parts, c.Preview)
	}
	if l := c.MediaLine(); l != "" {
		parts = append(parts, l)
	}
	return strings.Join(parts, "\n\n")
}

// stepUpLine is s as a one-line field bounded to max runes: sanitised, line
// breaks folded to spaces, runs of space folded; refused when longer.
func stepUpLine(field, s string, max int) (string, error) {
	out := whyLine(s)
	if utf8.RuneCountInString(out) > max {
		return "", fmt.Errorf("%w: a step-up card's %s is longer than %d characters", ErrInvalid, field, max)
	}
	return out, nil
}

// stepUpBlock is s as a multi-line field bounded to max runes: CRLF folded,
// each line sanitised (fence.Sanitize), blank edges trimmed; refused when
// longer.
func stepUpBlock(field, s string, max int) (string, error) {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(fence.Sanitize(l), " ")
	}
	out := strings.Trim(strings.Join(lines, "\n"), "\n ")
	if utf8.RuneCountInString(out) > max {
		return "", fmt.Errorf("%w: a step-up card's %s is longer than %d characters", ErrInvalid, field, max)
	}
	return out, nil
}

// CleanStepUp is c as an ask stores it (the file header's rules): every field
// sanitised and bounded, a title, a why and details required. nil for nil.
func CleanStepUp(c *StepUpCard) (*StepUpCard, error) {
	if c == nil {
		return nil, nil
	}
	var out StepUpCard
	var err error
	if out.Tool, err = stepUpLine("tool", c.Tool, MaxStepUpToolRunes); err != nil {
		return nil, err
	}
	if out.Title, err = stepUpLine("title", c.Title, MaxStepUpTitleRunes); err != nil {
		return nil, err
	}
	if out.Why, err = stepUpLine("why", c.Why, MaxStepUpWhyRunes); err != nil {
		return nil, err
	}
	if out.Asker, err = stepUpLine("asker", c.Asker, MaxStepUpAskerRunes); err != nil {
		return nil, err
	}
	if out.Venue, err = stepUpLine("venue", c.Venue, MaxStepUpVenueRunes); err != nil {
		return nil, err
	}
	if out.Preview, err = stepUpBlock("preview", c.Preview, MaxStepUpPreviewRunes); err != nil {
		return nil, err
	}
	if out.Details, err = stepUpBlock("details", c.Details, MaxStepUpDetailsRunes); err != nil {
		return nil, err
	}
	if out.Tool == "" || out.Title == "" || out.Why == "" || out.Details == "" {
		return nil, fmt.Errorf("%w: a step-up card names its tool, title, why and details", ErrInvalid)
	}
	if c.DraftID < 0 {
		return nil, fmt.Errorf("%w: a step-up card's draft id is not an id", ErrInvalid)
	}
	out.DraftID, out.Changed = c.DraftID, c.Changed
	if len(c.Media) > MaxStepUpMedia {
		return nil, fmt.Errorf("%w: a step-up card shows at most %d media", ErrInvalid, MaxStepUpMedia)
	}
	if len(c.Media) > 0 && out.DraftID == 0 {
		return nil, fmt.Errorf("%w: a step-up card's media are a draft's", ErrInvalid)
	}
	for _, m := range c.Media {
		if m.N < 0 {
			return nil, fmt.Errorf("%w: a step-up card's medium index is negative", ErrInvalid)
		}
		cm := StepUpMedium{N: m.N}
		if cm.Ref, err = stepUpLine("medium ref", m.Ref, MaxStepUpMediumRunes); err != nil {
			return nil, err
		}
		if cm.Type, err = stepUpLine("medium type", m.Type, 100); err != nil {
			return nil, err
		}
		if cm.SHA256, err = stepUpLine("medium sha256", m.SHA256, 64); err != nil {
			return nil, err
		}
		out.Media = append(out.Media, cm)
	}
	if out.Changed {
		// A changed draft's card shows nothing of it.
		out.Preview, out.Venue, out.Media = "", "", nil
	}
	return &out, nil
}
