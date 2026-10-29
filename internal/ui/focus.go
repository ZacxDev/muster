package ui

import (
	"time"
)

// FocusView is a session's FOCUS (internal/focus) as a card shows it: what the
// session is working on, and when that was last written. A card with none
// shows nothing.
type FocusView struct {
	Title       string
	Description string
	UpdatedAt   time.Time
	// Problem is why the focus is missing (Title "") or stale (the latest
	// refresh failed after it was written): a short, non-secret reason
	// (focus.Reason), at ProblemAt. "" when the focus is current.
	Problem   string
	ProblemAt time.Time
}

// THE FOCUS IS THE CARD'S TITLE (operator ask): a session with a focus is
// headed by what it is working on (cardFocusTitle), and what the card was
// called before — the agent's name, the mirrored window's title with its muted
// session — moves under it as a subtitle (cardSubtitleClass). With no focus,
// or only a recorded failure, the card keeps its old title and has no
// subtitle, and the failure is the footer chip (cardFocus) it always was.
//
// 🔴 THE TITLE IS A BUTTON OUTSIDE THE NAV ANCHOR, IN ITS OWN SLOT ("focus").
// A button inside an <a> is invalid markup and its tap would bubble into the
// navigation, so the focus row sits above the anchor, beside the control
// cluster (it reserves the same pr-28): a tap on the title opens the popover
// and never the card; the rest of the card is the anchor, as before.
