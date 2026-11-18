package morning

import (
	"strconv"
	"strings"
	"time"
)

// Report is one owner's morning report as the job builds it: the window
// [Since, Until), the open questions counted, and the Body it stores. Every
// field is muster's own rows; nothing here is a model's words.
type Report struct {
	Since, Until time.Time
	// Questions is how many questions wait on the owner now (the Asks
	// queue's open questions); NewQuestions how many of them were asked in
	// the window. The page lists them live, so only the counts are kept.
	Questions, NewQuestions int
	// Unchecked: whether a pull request merged could not be settled — a
	// state GitHub did not answer, or more recent pull requests than one
	// report reads. Such a day is never quiet: a merge may be hiding there.
	Unchecked bool
	Body
}

// The kinds of a Done line.
const (
	DoneMerged = "merged" // a pull request merged
	// DoneTaskChanged: a task that is complete and was last changed in the
	// window. ⚠ muster keeps no time a task was COMPLETED: any change to a
	// complete task (a tag, a synced snapshot) moves updated_at, so the page
	// says "changed overnight", never "finished", and such a line does not by
	// itself make a day loud.
	DoneTaskChanged = "task"
)

// Done is one thing that got done (or, for a task, changed).
type Done struct {
	Kind string
	// Label is the line's text: a pull request's title, a task's title.
	Label string
	// Ref is a pull request's owner/repo#n ("" for a task): kept for the
	// link, never drawn as the line's text.
	Ref  string `json:",omitempty"`
	Href string
	At   time.Time
}

// The kinds of a Stuck line.
const (
	StuckIdle   = "idle"   // a session stopped with nothing running
	StuckError  = "error"  // a session stopped on an error (a usage limit, an API error)
	StuckFailed = "failed" // a dispatched agent failed
)

// Stuck is one thing that is stuck.
type Stuck struct {
	Kind string
	Name string
	// Why is the plain reason ("idle, nothing running", "stopped on an error").
	Why   string
	Href  string
	Since time.Time
}

// Usage is the window's usage and cost. A part whose source is off says so
// in Off, a plain sentence; the setting behind it is the page's to name (an
// admin's ⓘ), never stored.
type Usage struct {
	Chief ChiefUsage
	Plan  PlanUsage
	Cost  CostUsage
}

// ChiefUsage is the chief's own work in the window: turns and how long they
// ran (chief_turn_usage).
type ChiefUsage struct {
	Tracked bool
	Off     string
	Turns   int64
	Busy    time.Duration
}

// PlanUsage is the AI plan's usage: each Claude account's latest reading.
// 🔴 A LATEST READING, NEVER A NIGHT'S SUM: muster keeps one reading per
// account (cc_account_usage), overwritten on each poll, so the page says when
// it was read and never claims an overnight figure it does not have.
type PlanUsage struct {
	Tracked  bool
	Off      string
	Accounts []PlanAccount
}

// PlanAccount is one account's reading. Its configured name is not kept: the
// page calls it "Claude plan 1", "Claude plan 2", in the accounts' order.
type PlanAccount struct {
	// FiveHour and Week are the percent used (nil: not read).
	FiveHour, Week *float64
	ReadAt         time.Time
}

// CostUsage is what the chief's work cost, where muster knows: its
// OpenRouter sessions' spend (chief_session_llm_usage), which is booked per
// UTC DAY — so it covers whole UTC days FirstDay..LastDay, not the window.
// Claude plans carry no cost.
type CostUsage struct {
	Tracked           bool
	Off               string
	USD               float64
	FirstDay, LastDay string
}

// Gap is one thing the report could not read: its plain sentence. The page
// names the setting behind it, for an admin.
type Gap struct {
	Text string `json:"text"`
}

// Quiet reports a day on which nothing happened: no question asked in the
// window still waits, no pull request merged, nothing got stuck in the window,
// and every pull request was checked. A quiet day sends no push (operator
// decision: "send it only on days when something happened").
//
// 🔴 WHAT NEVER MAKES A DAY LOUD BY ITSELF: usage; a question asked before the
// window (owner asks wait up to 24 h, so yesterday's would push every
// morning); a session stuck since before the window (or a session left stopped
// would push forever); and a complete task that only CHANGED in the window
// (DoneTaskChanged: a tag or a sync moves it). All of them are still listed
// on the page.
func (r Report) Quiet() bool {
	if r.NewQuestions > 0 || r.Unchecked {
		return false
	}
	for _, d := range r.Done {
		if d.Kind == DoneMerged {
			return false
		}
	}
	for _, s := range r.Stuck {
		if !s.Since.Before(r.Since) {
			return false
		}
	}
	return true
}

// Merged is how many pull requests in b merged.
func (b Body) Merged() int {
	n := 0
	for _, d := range b.Done {
		if d.Kind == DoneMerged {
			n++
		}
	}
	return n
}

// PushTitle is the push's title.
const PushTitle = "While you were away"

// PushTag is the push's tag: a newer report replaces the older one on the
// phone.
const PushTag = "morning-report"

// PushBody is the push's one line for a report with questions open
// questions, merged pull requests merged and stuck things stuck: counts only,
// never a session's, a task's or a question's text ("1 question for you · 3
// merged · 2 stuck"). Zero terms are left out.
func PushBody(questions, merged, stuck int) string {
	var parts []string
	switch {
	case questions == 1:
		parts = append(parts, "1 question for you")
	case questions > 1:
		parts = append(parts, strconv.Itoa(questions)+" questions for you")
	}
	if merged > 0 {
		parts = append(parts, strconv.Itoa(merged)+" merged")
	}
	if stuck > 0 {
		parts = append(parts, strconv.Itoa(stuck)+" stuck")
	}
	if len(parts) == 0 {
		return "See what happened overnight"
	}
	return strings.Join(parts, " · ")
}
