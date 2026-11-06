package ui

import (
	"strconv"
	"time"
)

// FocusView is a session's FOCUS (internal/focus): what the session is working
// on, and when that was last written. The palette finds an entry by its title
// (switcher.go focusTitle).
type FocusView struct {
	Title       string
	Description string
	// UpdatedAt is when the title was last written: its age (FocusAgeOf).
	UpdatedAt time.Time
	// Problem is why the focus is missing (Title "") or stale (the latest
	// refresh failed after it was written): a short, non-secret reason
	// (focus.Reason), at ProblemAt. "" when the focus is current.
	Problem   string
	ProblemAt time.Time
}

// (The retired Agents list's card was headed by its focus title, with a
// popover of the focus's description, age and refresh failure; Description,
// Problem and ProblemAt were for that card. Its focus row sat beside the card's
// control cluster, keeping the same pr-28 clear of it.)

// 🔴 THE FOCUS AGE RULE — ONE PLACE. Every surface that shows a focus title
// (the switcher dock's second line, the palette's results, the Asks list's
// rows, a chief notice's push body, and the chief's fleet_list and
// sessions_overview rows — internal/api's focusIndex.ageFields) says how old
// it is through FocusAgeOf, and nothing else decides it:
// the dock and the palette draw in the browser, so the server computes the
// age at RENDER time and hands them the result as attributes
// (data-switcher-focus-age, data-switcher-focus-stale); their scripts only
// draw it and hold no threshold of their own.
//
// A title summarises what the session was doing WHEN IT WAS WRITTEN; an idle
// session keeps its last one for days, and without an age it reads as current.
// So:
//
//   - older than FocusAgeShownAfter (a day): its age is shown after it, in
//     whole days ("· 2d");
//   - older than FocusStaleAfter (four days): it is also marked stale and
//     drawn muted (FocusStaleClass).
//
// The age is UpdatedAt: when the title was last WRITTEN. A refresh that fails
// keeps the old title and leaves UpdatedAt where it was, so a title that
// stopped refreshing ages like any other. Problem plays no part: a refresh
// that failed minutes after a fresh title does not make that title old.
const (
	FocusAgeShownAfter = 24 * time.Hour
	FocusStaleAfter    = 4 * 24 * time.Hour
)

// FocusStaleClass is the muted style of a stale focus title wherever one is
// drawn. internal/api's Asks list uses it too; it is spelled here, in
// Tailwind's content globs, so its classes are always in the stylesheet.
const FocusStaleClass = "italic opacity-60"

// FocusAge is how old a focus title is, as the surfaces show it: Text is ""
// (no age shown) or whole days ("2d"), and Stale marks it stale.
type FocusAge struct {
	Text  string
	Stale bool
}

// FocusAgeLabel is age in words, for its element's title and aria-label: the
// focus age sits beside a waiting time drawn in the same "· 3m" shape, and
// must not be read as one. "" when there is no age.
func FocusAgeLabel(age FocusAge) string {
	if age.Text == "" {
		return ""
	}
	l := "focus title set " + age.Text + " ago"
	if age.Stale {
		l += " (stale)"
	}
	return l
}

// FocusAgeSuffix is age as plain text after its title — " · 2d" — or "" when
// there is no age: the Asks list's age element and a notice's push body both
// spell it through here.
func FocusAgeSuffix(age FocusAge) string {
	if age.Text == "" {
		return ""
	}
	return " · " + age.Text
}

// FocusAgeOf is the age at now of a focus title written at updatedAt. A zero
// updatedAt (no focus, or no known time) has none.
func FocusAgeOf(updatedAt, now time.Time) FocusAge {
	if updatedAt.IsZero() {
		return FocusAge{}
	}
	d := now.Sub(updatedAt)
	if d <= FocusAgeShownAfter {
		return FocusAge{}
	}
	return FocusAge{Text: strconv.Itoa(int(d/(24*time.Hour))) + "d", Stale: d > FocusStaleAfter}
}

// focusAgeOf is f's age at now; none for no focus (a FocusView with no title
// has no UpdatedAt: internal/api's focusIndex.view).
func focusAgeOf(f *FocusView, now time.Time) FocusAge {
	if f == nil {
		return FocusAge{}
	}
	return FocusAgeOf(f.UpdatedAt, now)
}

// focusAgeLineJS is the ONE script that draws a focus title with its age, for
// both the dock (switcher_dock.go) and the palette (palette.go), each of which
// splices it into its own script beside its own el(tag, cls, text).
// focusAgeLine(age, stale, title) is title (the drawn title element) with
// " · <age>" after it when age is set (data-switcher-focus-age, which
// FocusAgeOf decided at render time — the script holds no threshold): a
// flex line whose title truncates and whose age never does, muted when stale
// (data-switcher-focus-stale). No age: title itself.
const focusAgeLineJS = `  function focusAgeLine(age, stale, title) {
    if (!age) return title;
    var line = el('span', 'flex min-w-0 items-baseline gap-1 text-xs text-muted' + (stale ? ' ` + FocusStaleClass + `' : ''));
    line.setAttribute('data-focus-line', '');
    if (stale) line.setAttribute('data-focus-stale', '');
    title.className += ' min-w-0';
    line.appendChild(title);
    var a = el('span', 'shrink-0', '\u00b7 ' + age);
    a.setAttribute('data-focus-age', '');
    var label = 'focus title set ' + age + ' ago' + (stale ? ' (stale)' : '');
    a.setAttribute('title', label);
    a.setAttribute('aria-label', label);
    line.appendChild(a);
    return line;
  }`
