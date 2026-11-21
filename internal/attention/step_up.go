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
// itself. The host, the session, the call in words and the security reason
// are on the record, but folded away (Details); no hash is shown on any face.
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
//
// 🔴 A LONG PART IS CUT, NEVER REFUSED. Every text part is built from inputs
// other doors bound more loosely (a reject reason, a plugin's label, a media
// ref, an officer item's subject), so a part over its bound is cut with "…"
// (stepUpLine), never a reason to file no card: a card that cannot be filed
// leaves a valid call unconfirmable. Only the identifiers (the tool's name, a
// medium's SHA-256), which a cut would make a wrong value, are refused.
// ---------------------------------------------------------------------------

// The bounds of a StepUpCard. A card's Text must fit MaxQuestionRunes: the
// sum of these (with its separators) stays under it.
const (
	MaxStepUpTitleRunes   = 200
	MaxStepUpWhyRunes     = 300
	MaxStepUpAboutRunes   = 200
	MaxStepUpAskerRunes   = 200
	MaxStepUpVenueRunes   = 64
	MaxStepUpToolRunes    = 64
	MaxStepUpPreviewRunes = 2600
	MaxStepUpDetailsRunes = 3000
	MaxStepUpExcerptRunes = 300
	MaxStepUpParts        = 1000
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
	// About is the card's where line: who or what the call is about, when
	// the title and preview do not: the route an answer goes on, the batch,
	// the session a message is routed to, the pairing code, or — on a draft
	// card — who asked. "" for none.
	About string `json:"about,omitempty"`
	// Preview is what is about to happen: a post's exact text, or a plain
	// summary of the call. Line breaks kept.
	Preview string `json:"preview,omitempty"`
	// Venue is where a post goes, "" for any other card.
	Venue string `json:"venue,omitempty"`
	// Platform is an outbound draft's venue as its plugin names it ("x",
	// "reddit"): what picks the preview's logo. "" for a reply draft (its
	// venue is its plugin's label) and for any other card.
	Platform string `json:"platform,omitempty"`
	// Target is an outbound draft's target kind (post, reply, dm, follow,
	// like): how its preview is labelled. "" for any other card.
	Target string `json:"target,omitempty"`
	// Handle is the person a dm or a follow reaches, as the draft names it;
	// "" for every other card.
	Handle string `json:"handle,omitempty"`
	// Parts is how many posts an X thread's WHOLE body becomes (counted
	// before Preview was cut to an excerpt of it), 0 for anything that is
	// not a thread. A preview drawn as a thread takes its part count from
	// here, never from the parts the excerpt happens to hold.
	Parts int `json:"parts,omitempty"`
	// Excerpt is the one line saying Preview is only part of what is about
	// to happen ("⚠ AN EXCERPT: …"), "" when Preview is all of it. Drawn
	// OUTSIDE the preview on every face, and a paragraph of Text.
	Excerpt string `json:"excerpt,omitempty"`
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
	// call and its arguments in words (never JSON, never a hash), and why the
	// phone is needed.
	Details string `json:"details"`
}

// StepUpMedium is one medium of a draft a card shows: its index among the
// draft's refs (what the draft's media route serves), its ref, type and hash.
// The type picks how it is drawn (an image or a video) and the hash is on the
// record; neither is ever drawn on any face of the card.
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

// Text is the card's question: its title, its why and its where line, then
// its preview and its media line. No host, no hash, no JSON — those are
// Details. THE ONE TEXT the page's summary and the phone's prompt read.
func (c *StepUpCard) Text() string {
	if c == nil {
		return ""
	}
	head := c.Title + "\n" + c.Why
	if c.About != "" {
		head += "\n" + c.About
	}
	parts := []string{head}
	if c.Preview != "" {
		parts = append(parts, c.Preview)
	}
	if c.Excerpt != "" {
		parts = append(parts, c.Excerpt)
	}
	if l := c.MediaLine(); l != "" {
		parts = append(parts, l)
	}
	return strings.Join(parts, "\n\n")
}

// stepUpLine is s as a one-line field of at most max runes: sanitised, line
// breaks folded to spaces, runs of space folded; when longer, its first max-1
// runes and "…" (the file header: a long part is cut, never refused).
func stepUpLine(s string, max int) string {
	out := whyLine(s)
	if utf8.RuneCountInString(out) > max {
		return fence.Cut(out, max-1)
	}
	return out
}

// stepUpID is s as a one-line identifier of at most max runes, refused when
// longer: a cut identifier would be a wrong one.
func stepUpID(field, s string, max int) (string, error) {
	out := whyLine(s)
	if utf8.RuneCountInString(out) > max {
		return "", fmt.Errorf("%w: a step-up card's %s is longer than %d characters", ErrInvalid, field, max)
	}
	return out, nil
}

// stepUpBlock is s as a multi-line field bounded to max runes: CRLF and a
// lone CR folded to a line break (as a venue reads them: internal/plugins
// normPart), each line sanitised (fence.Sanitize), blank edges trimmed;
// refused when longer.
func stepUpBlock(field, s string, max int) (string, error) {
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n"), "\n")
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
	if out.Tool, err = stepUpID("tool", c.Tool, MaxStepUpToolRunes); err != nil {
		return nil, err
	}
	out.Title = stepUpLine(c.Title, MaxStepUpTitleRunes)
	out.Why = stepUpLine(c.Why, MaxStepUpWhyRunes)
	out.Asker = stepUpLine(c.Asker, MaxStepUpAskerRunes)
	out.About = stepUpLine(c.About, MaxStepUpAboutRunes)
	out.Venue = stepUpLine(c.Venue, MaxStepUpVenueRunes)
	out.Platform = stepUpLine(c.Platform, MaxStepUpVenueRunes)
	out.Target = stepUpLine(c.Target, 16)
	out.Handle = stepUpLine(c.Handle, MaxStepUpAskerRunes)
	if out.Preview, err = stepUpBlock("preview", c.Preview, MaxStepUpPreviewRunes); err != nil {
		return nil, err
	}
	out.Excerpt = stepUpLine(c.Excerpt, MaxStepUpExcerptRunes)
	if c.Parts < 0 || c.Parts > MaxStepUpParts {
		return nil, fmt.Errorf("%w: a step-up card's part count is not 0 to %d", ErrInvalid, MaxStepUpParts)
	}
	out.Parts = c.Parts
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
		cm.Ref = stepUpLine(m.Ref, MaxStepUpMediumRunes)
		cm.Type = stepUpLine(m.Type, 100)
		if cm.SHA256, err = stepUpID("medium sha256", m.SHA256, 64); err != nil {
			return nil, err
		}
		out.Media = append(out.Media, cm)
	}
	if out.Changed {
		// A changed draft's card shows nothing of it.
		out.Preview, out.Venue, out.Media = "", "", nil
		out.Platform, out.Target, out.Handle = "", "", ""
		out.Parts, out.Excerpt = 0, ""
	}
	return &out, nil
}
