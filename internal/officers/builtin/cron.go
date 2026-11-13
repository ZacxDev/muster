package builtin

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron is a parsed five-field cron expression — minute hour day-of-month
// month day-of-week — evaluated in UTC. Each field takes `*`, a number, a
// range `a-b`, a list `a,b,c` and a step `*/n` or `a-b/n`; day-of-week is
// 0–6 with 0 (or 7) Sunday. When BOTH day fields are restricted a day
// matches if EITHER does; a field counts as unrestricted only when it is a
// bare `*` — so `*/2` restricts (robfig/cron's rule; Vixie cron treats any
// field starting with `*` as unrestricted). No names (MON, JAN), no `@daily`
// macros, no seconds: 0118's CHECK already holds the column to five fields.
type Cron struct {
	min, hour, dom, month, dow uint64 // bit i set: value i allowed
	domStar, dowStar           bool
}

// cronField is one field's bounds.
type cronField struct {
	name     string
	min, max int
}

var cronFields = [5]cronField{{"minute", 0, 59}, {"hour", 0, 23}, {"day of month", 1, 31}, {"month", 1, 12}, {"day of week", 0, 7}}

// ParseCron parses expr, refusing (naming the field) anything outside the
// grammar above.
func ParseCron(expr string) (Cron, error) {
	f := strings.Fields(expr)
	if len(f) != 5 {
		return Cron{}, fmt.Errorf("cron %q: want five fields (minute hour day-of-month month day-of-week), got %d", expr, len(f))
	}
	var c Cron
	sets := [5]*uint64{&c.min, &c.hour, &c.dom, &c.month, &c.dow}
	for i, s := range f {
		bits, err := parseCronField(s, cronFields[i])
		if err != nil {
			return Cron{}, fmt.Errorf("cron %q: %w", expr, err)
		}
		*sets[i] = bits
	}
	if c.dow&(1<<7) != 0 { // 7 is Sunday too
		c.dow |= 1
		c.dow &^= 1 << 7
	}
	c.domStar, c.dowStar = f[2] == "*", f[4] == "*"
	return c, nil
}

func parseCronField(s string, fd cronField) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(s, ",") {
		rng, stepS, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepS)
			if err != nil || n < 1 || n > fd.max {
				return 0, fmt.Errorf("%s: step %q is not 1..%d", fd.name, stepS, fd.max)
			}
			step = n
		}
		lo, hi := fd.min, fd.max
		switch {
		case rng == "*":
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err1, err2 error
			lo, err1 = strconv.Atoi(a)
			hi, err2 = strconv.Atoi(b)
			if err1 != nil || err2 != nil || lo > hi {
				return 0, fmt.Errorf("%s: range %q is not a-b with a <= b", fd.name, rng)
			}
		default:
			n, err := strconv.Atoi(rng)
			if err != nil {
				return 0, fmt.Errorf("%s: %q is not a number, a range or *", fd.name, rng)
			}
			lo, hi = n, n
			if hasStep { // "5/15" means 5, 20, 35, 50
				hi = fd.max
			}
		}
		if lo < fd.min || hi > fd.max {
			return 0, fmt.Errorf("%s: %q is outside %d..%d", fd.name, part, fd.min, fd.max)
		}
		for v := lo; v <= hi; v += step {
			bits |= 1 << uint(v)
		}
	}
	return bits, nil
}

func (c Cron) dayMatches(t time.Time) bool {
	dom := c.dom&(1<<uint(t.Day())) != 0
	dow := c.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dow
	case c.dowStar:
		return dom
	}
	return dom || dow
}

// cronHorizon bounds Next's search: an expression with no time in it within
// five years (31 February) has no next fire.
const cronHorizon = 5 * 366 * 24 * time.Hour

// Next is the first minute strictly after t (in UTC) that c matches, and
// false when there is none within cronHorizon.
func (c Cron) Next(t time.Time) (time.Time, bool) {
	t = t.UTC().Truncate(time.Minute).Add(time.Minute)
	end := t.Add(cronHorizon)
	for t.Before(end) {
		switch {
		case c.month&(1<<uint(t.Month())) == 0:
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		case !c.dayMatches(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
		case c.hour&(1<<uint(t.Hour())) == 0:
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, time.UTC)
		case c.min&(1<<uint(t.Minute())) == 0:
			t = t.Add(time.Minute)
		default:
			return t, true
		}
	}
	return time.Time{}, false
}
