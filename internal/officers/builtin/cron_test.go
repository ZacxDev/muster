package builtin_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers/builtin"
)

func at(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return t
}

// TestCronNext pins the cron grammar's next fires, in UTC: each case's
// expected minute is written out literally, and every fixture's "from" is
// chosen so the answer is not the next minute (a Next that returns t+1m is
// wrong on all but one).
func TestCronNext(t *testing.T) {
	for _, c := range []struct{ expr, from, want string }{
		{"0 6 * * *", "2026-11-12 23:10", "2026-11-13 06:00"},
		{"*/15 * * * *", "2026-11-12 23:10", "2026-11-12 23:15"},
		{"5/20 * * * *", "2026-11-12 23:30", "2026-11-12 23:45"},
		{"30 9 * * 1-5", "2026-11-13 10:00", "2026-11-16 09:30"}, // Fri after 09:30 -> Mon
		{"0 0 1 * *", "2026-11-12 23:10", "2026-12-01 00:00"},
		{"0 12 29 2 *", "2026-03-01 00:00", "2028-02-29 12:00"}, // the next leap day
		{"0 8 13 * 5", "2026-11-12 23:10", "2026-11-13 08:00"},  // dom OR dow: Fri the 13th
		{"0 8 1 * 0", "2026-11-12 23:10", "2026-11-15 08:00"},   // Sunday comes before the 1st
		{"0 8 * * 7", "2026-11-12 23:10", "2026-11-15 08:00"},   // 7 is Sunday
		{"0,30 22-23 * * *", "2026-11-12 23:10", "2026-11-12 23:30"},
		{"0 6 * * *", "2026-11-13 06:00", "2026-11-14 06:00"}, // strictly after
	} {
		cr, err := builtin.ParseCron(c.expr)
		if err != nil {
			t.Fatalf("%q: %v", c.expr, err)
		}
		got, ok := cr.Next(at(c.from))
		if !ok || !got.Equal(at(c.want)) {
			t.Errorf("%q from %s: next %s (%v), want %s", c.expr, c.from, got.Format("2006-01-02 15:04"), ok, c.want)
		}
	}
	never, err := builtin.ParseCron("0 0 31 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := never.Next(at("2026-01-01 00:00")); ok {
		t.Errorf("31 February: next %s, want none", got)
	}
}

// TestCronRefusesWhatItDoesNotParse: every malformed field is refused,
// naming the field.
func TestCronRefusesWhatItDoesNotParse(t *testing.T) {
	for expr, field := range map[string]string{
		"* * * *":        "five fields",
		"60 * * * *":     "minute",
		"* 24 * * *":     "hour",
		"* * 0 * *":      "day of month",
		"* * * 13 *":     "month",
		"* * * * 8":      "day of week",
		"*/0 * * * *":    "step",
		"5-1 * * * *":    "range",
		"MON * * * *":    "minute",
		"@daily * * * *": "minute",
	} {
		if _, err := builtin.ParseCron(expr); err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%q: %v, want a refusal naming %q", expr, err, field)
		}
	}
}
