package ui

import (
	"time"
)

// FocusView is a session's FOCUS (internal/focus): what the session is working
// on, and when that was last written. The palette finds an entry by its title
// (switcher.go focusTitle).
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

// (The retired Agents list's card was headed by its focus title, with a
// popover of the focus's description, age and refresh failure; Description,
// UpdatedAt, Problem and ProblemAt were for that card.)
