// Package morning is the morning report: one push a day, at a fixed local
// time, saying what happened while the owner was away, and the page it opens
// (docs/features.md#morning-report). This file is the schedule; report.go is the
// report's shape, its quiet-day rule and its push text; store.go is the one
// row per owner per day.
//
// 🔴 READ-ONLY. Nothing here, and nothing that calls it, acts on a session,
// an ask or a task: the report reads muster's own rows and says what they
// hold. Its open questions are answered on its page through the Asks queue's
// own answer route, by the owner's tap, exactly as on /attention.
//
// 🔴 THE TIME IS A SERVER SETTING (an operator decision): muster
// stores no time zone per owner yet (docs/design/check-ins.md, "Time zones"),
// so every owner's report is due at the same wall-clock time in one zone.
package morning

import (
	"time"
	_ "time/tzdata" // the report's zone is named; the image may carry no zoneinfo
)

// CatchUp is how late a report may still go out: a server that was down at
// the report time and comes back within this sends that day's report late; past
// it the day is skipped, never backfilled (the check-ins design's catch-up
// rule, docs/design/check-ins.md "The scheduler").
const CatchUp = 2 * time.Hour

// Schedule is when the report is due: Hour:Minute every day in Loc.
type Schedule struct {
	Hour, Minute int
	Loc          *time.Location
}

// ReportHour is the report's time of day, 08:00 (an operator decision).
// Only its zone is a setting (MUSTER_MORNING_REPORT_TZ).
const ReportHour = 8

// In is the report's schedule in loc: ReportHour every day.
func In(loc *time.Location) Schedule { return Schedule{Hour: ReportHour, Loc: loc} }

// Day is one day's report: its local date, the instant it is due, and the
// window it covers — from the previous local day's report time to this one.
//
// 🔴 THE WINDOW IS TWO WALL-CLOCK TIMES, NOT "SLOT MINUS 24 HOURS": across a
// daylight-saving change the night is 23 or 25 hours long, and a fixed 24h
// would drop, or count twice, the hour the clocks moved. Each window starts
// exactly where the previous day's ended, so no event is in two reports and
// none falls between them.
type Day struct {
	// Date is the local date, "2006-01-02": the report's key and its URL.
	Date string
	// Slot is the instant the report is due (UTC).
	Slot time.Time
	// Since is the previous local day's slot: the window's start.
	Since time.Time
}

// On is the report of the local date y-m-d.
func (s Schedule) On(y int, m time.Month, d int) Day {
	slot := time.Date(y, m, d, s.Hour, s.Minute, 0, 0, s.Loc)
	prev := time.Date(y, m, d-1, s.Hour, s.Minute, 0, 0, s.Loc)
	return Day{Date: slot.Format("2006-01-02"), Slot: slot.UTC(), Since: prev.UTC()}
}

// Due is the report due at now: today's (in Loc) once its slot has passed and
// for CatchUp after it; false before the slot and after the catch-up.
func (s Schedule) Due(now time.Time) (Day, bool) {
	local := now.In(s.Loc)
	d := s.On(local.Year(), local.Month(), local.Day())
	if now.Before(d.Slot) || now.Sub(d.Slot) > CatchUp {
		return Day{}, false
	}
	return d, true
}

// ParseDate reads a report's date ("2006-01-02") and returns that day's
// report under s; false for anything else.
func (s Schedule) ParseDate(date string) (Day, bool) {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return Day{}, false
	}
	return s.On(t.Year(), t.Month(), t.Day()), true
}
