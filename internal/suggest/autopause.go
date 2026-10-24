package suggest

// THE AUTO-PAUSE (operator decision): suggestions pause for a user who rarely
// uses them — fewer than AutoPauseRate of the sets shown were tapped, once
// AutoPauseMinShown sets have been shown — so the chip row cannot become a
// row of ignored noise.
//
// 🔴 IN MEMORY, AND NUMBERS ONLY. What the composer reports (Record) is an
// event name, the set's fingerprint and, for a send, the edit distance between
// what a tap filled and what was sent: never any text. The tallies live in the
// Engine and are gone at a restart (which un-pauses everyone); nothing is
// written to the database.
//
// 🔴 A SET COUNTS ONCE. "shown" counts a fingerprint once per user however
// often a page re-renders it, and only a fingerprint the engine actually
// served that user (their newest set for that subject); "filled" and "sent"
// count only a fingerprint already counted as shown, once each — so the fill
// rate is at most 1 and cannot be inflated by repeating an event.
//
// Turning suggestions back on (the toggle, on=1) clears the user's tallies:
// that is how a paused user resumes.

// The auto-pause's bounds.
const (
	// AutoPauseMinShown is how many sets must have been shown before the rate
	// is judged.
	AutoPauseMinShown = 200
	// AutoPauseRatePercent: below this percentage of shown sets filled, the
	// user is paused.
	AutoPauseRatePercent = 5
	// maxTallied bounds the fingerprints remembered per user (the oldest are
	// forgotten first; their counts stay).
	maxTallied = 1000
)

// The events the composer reports.
const (
	EventShown  = "shown"
	EventFilled = "filled"
	EventSent   = "sent"
)

// OutcomePaused is Enabled's answer for a user the auto-pause paused.
const OutcomePaused Outcome = "paused"

// Tally is one user's counts.
type Tally struct {
	Shown, Filled, Sent int
	// EditSum is the sum of the edit distances reported with Sent.
	EditSum int
	Paused  bool
}

type tally struct {
	Tally
	// flags is what has been counted per fingerprint (1 shown, 2 filled,
	// 4 sent), and order the fingerprints oldest first, for the bound.
	flags map[string]uint8
	order []string
}

// Record counts one composer event for owner: ev is EventShown, EventFilled
// or EventSent, fp the set's fingerprint, subject the conversation it was
// served for, dist the edit distance (EventSent only). It reports whether the
// event counted (a repeat, an unknown event or fingerprint does not).
func (e *Engine) Record(owner int64, subject, fp, ev string, dist int) bool {
	if e == nil || fp == "" || owner <= 0 {
		return false
	}
	var bit uint8
	switch ev {
	case EventShown:
		bit = 1
	case EventFilled:
		bit = 2
	case EventSent:
		bit = 4
	default:
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.init()
	t := e.tallies[owner]
	if t == nil {
		t = &tally{flags: map[string]uint8{}}
		e.tallies[owner] = t
	}
	have, known := t.flags[fp]
	switch {
	case have&bit != 0:
		return false
	case bit == 1:
		// Only a set this engine served this owner for that subject.
		c, ok := e.cache[cacheKey{owner, subject}]
		if !ok || c.set.Fingerprint != fp || len(c.set.Suggestions) == 0 {
			return false
		}
	case !known || have&1 == 0:
		return false
	}
	if !known {
		t.order = append(t.order, fp)
		if len(t.order) > maxTallied {
			delete(t.flags, t.order[0])
			t.order = t.order[1:]
		}
	}
	t.flags[fp] = have | bit
	switch bit {
	case 1:
		t.Shown++
	case 2:
		t.Filled++
	case 4:
		t.Sent++
		if dist > 0 {
			t.EditSum += dist
		}
	}
	// 🔴 THE RULE: judged at every count once enough sets were shown.
	if t.Shown >= AutoPauseMinShown && t.Filled*100 < t.Shown*AutoPauseRatePercent {
		if !t.Paused {
			e.logf("suggest: owner %d: paused — %d of %d shown sets were tapped (under %d%%)", owner, t.Filled, t.Shown, AutoPauseRatePercent)
		}
		t.Paused = true
	}
	return true
}

// Stats is owner's tally.
func (e *Engine) Stats(owner int64) Tally {
	if e == nil {
		return Tally{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if t := e.tallies[owner]; t != nil {
		return t.Tally
	}
	return Tally{}
}

// Resume clears owner's tally, and with it a pause.
func (e *Engine) Resume(owner int64) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.tallies, owner)
}

// paused reports whether the auto-pause paused owner.
func (e *Engine) paused(owner int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	t := e.tallies[owner]
	return t != nil && t.Paused
}
