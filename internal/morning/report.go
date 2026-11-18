package morning

import (
	"strconv"
	"strings"
	"time"
)

// Report is one owner's morning report: what internal/api read for the
// window [Since, Until) and, for the open questions and the stuck sessions, as
// they stand when it is read. Every field is muster's own rows; nothing here
// is a model's words.
type Report struct {
	Since, Until time.Time
	// Questions are the questions still waiting on the owner (the Asks
	// queue's open questions — the chief's, the owner's sessions' and
	// agents', and muster's own), newest first.
	Questions []Question
	// Done is what got done in the window: pull requests merged and tasks
	// finished, one line each.
	Done []Done
	// Stuck is what is stuck: sessions stopped idle or on an error, and
	// agents that failed.
	Stuck []Stuck
	// Usage is the window's usage, each part saying when it is not tracked.
	Usage Usage
}

// Question is one open question.
type Question struct {
	// AskID is the attention ask's id; 0 for a question held in a session
	// (answered in that session).
	AskID   int64
	Text    string
	AskedAt time.Time
}

// The kinds of a Done line.
const (
	DoneMerged = "merged" // a pull request merged
	DoneTask   = "task"   // a task finished
)

// Done is one thing that got done.
type Done struct {
	Kind string
	// Label is the line's text ("ZacxDev/muster#12", a task's title).
	Label string
	// Detail is the line's second part: the session that opened the pull
	// request, or who finished the task ("" for none).
	Detail string
	Href   string
	At     time.Time
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
	// Why is the plain reason ("stopped 3h ago", "hit an error").
	Why   string
	Href  string
	Since time.Time
}

// Usage is the window's usage. A part whose source is off says so: Off is
// the plain sentence and Setting the variable that turns it on, which the
// page shows only to an admin (an ⓘ).
type Usage struct {
	Chief ChiefUsage
	Plan  PlanUsage
}

// ChiefUsage is the chief's own work in the window: turns and how long they
// ran (chief_turn_usage).
type ChiefUsage struct {
	Tracked bool
	Off     string
	Setting string
	Turns   int64
	Busy    time.Duration
	Tokens  int64
}

// PlanUsage is the AI plan's usage: each Claude account's latest reading.
// 🔴 A LATEST READING, NEVER A NIGHT'S SUM: muster keeps one reading per
// account (cc_account_usage), overwritten on each poll, so the page says when
// it was read and never claims an overnight figure it does not have.
type PlanUsage struct {
	Tracked  bool
	Off      string
	Setting  string
	Accounts []PlanAccount
}

// PlanAccount is one account's reading.
type PlanAccount struct {
	Name string
	// FiveHour and Week are the percent used (nil: not read).
	FiveHour, Week *float64
	ReadAt         time.Time
}

// newlyStuck is how many stuck lines began in the window: a session that was
// already stuck yesterday is listed again, but does not by itself make a day
// one on which something happened.
func (r Report) newlyStuck() int {
	n := 0
	for _, s := range r.Stuck {
		if !s.Since.Before(r.Since) {
			n++
		}
	}
	return n
}

// Quiet reports a day on which nothing happened: no question waits, nothing
// got done, and nothing got stuck in the window. A quiet day sends no push
// (operator decision: "send it only on days when something happened").
//
// 🔴 USAGE NEVER MAKES A DAY LOUD: a chief that ran turns but changed nothing
// the owner can see is not news worth a push. A session stuck since BEFORE the
// window is shown on the page but does not make the day loud either, or a
// session left stopped would push every morning forever.
func (r Report) Quiet() bool {
	return len(r.Questions) == 0 && len(r.Done) == 0 && r.newlyStuck() == 0
}

// PushTitle is the push's title.
const PushTitle = "While you were away"

// PushTag is the push's tag: a newer report replaces the older one on the
// phone.
const PushTag = "morning-report"

// PushBody is the push's one line for a report with questions open
// questions, done things done and stuck things stuck: counts only, never a
// session's, a task's or a question's text ("1 question for you · 3 done · 2
// stuck"). Zero terms are left out; a loud day always has at least one.
func PushBody(questions, done, stuck int) string {
	var parts []string
	switch {
	case questions == 1:
		parts = append(parts, "1 question for you")
	case questions > 1:
		parts = append(parts, strconv.Itoa(questions)+" questions for you")
	}
	if done > 0 {
		parts = append(parts, strconv.Itoa(done)+" done")
	}
	if stuck > 0 {
		parts = append(parts, strconv.Itoa(stuck)+" stuck")
	}
	if len(parts) == 0 {
		return "Nothing new"
	}
	return strings.Join(parts, " · ")
}
