package morning

import (
	"testing"
	"time"
)

func chicago(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// 08:00 in Chicago is 13:00Z under daylight time (CDT, UTC-5) and 14:00Z under
// standard time (CST, UTC-6). The two dates measured on each side of a change
// differ by exactly that hour, and the night containing the change is 25h
// (autumn) or 23h (spring) long — literal instants, never derived from the
// code under test.
func TestTheReportTimeIsEightInChicagoAcrossDST(t *testing.T) {
	s := Schedule{Hour: 8, Loc: chicago(t)}
	for _, tc := range []struct {
		name         string
		y            int
		m            time.Month
		d            int
		slot, since  string
		nightLengthH float64
	}{
		// Autumn: the clocks go back at 02:00 on Sunday 2026-11-01.
		{"the day before the autumn change (CDT)", 2026, time.October, 31, "2026-10-31T13:00:00Z", "2026-10-30T13:00:00Z", 24},
		{"the autumn change day: a 25h night", 2026, time.November, 1, "2026-11-01T14:00:00Z", "2026-10-31T13:00:00Z", 25},
		{"the day after the autumn change (CST)", 2026, time.November, 2, "2026-11-02T14:00:00Z", "2026-11-01T14:00:00Z", 24},
		// Spring: the clocks go forward at 02:00 on Sunday 2026-03-08.
		{"the day before the spring change (CST)", 2026, time.March, 7, "2026-03-07T14:00:00Z", "2026-03-06T14:00:00Z", 24},
		{"the spring change day: a 23h night", 2026, time.March, 8, "2026-03-08T13:00:00Z", "2026-03-07T14:00:00Z", 23},
	} {
		d := s.On(tc.y, tc.m, tc.d)
		if !d.Slot.Equal(utc(tc.slot)) {
			t.Errorf("%s: slot %s; want %s", tc.name, d.Slot.Format(time.RFC3339), tc.slot)
		}
		if !d.Since.Equal(utc(tc.since)) {
			t.Errorf("%s: since %s; want %s", tc.name, d.Since.Format(time.RFC3339), tc.since)
		}
		if got := d.Slot.Sub(d.Since).Hours(); got != tc.nightLengthH {
			t.Errorf("%s: the night is %vh; want %vh", tc.name, got, tc.nightLengthH)
		}
	}
}

// Due answers the local day's report from its slot to CatchUp after it, and
// nothing before or after. Measured on both sides of the autumn change: the
// same wall-clock 08:00 is a different UTC instant on each.
func TestDueOpensAtTheSlotAndClosesAfterTheCatchUp(t *testing.T) {
	s := Schedule{Hour: 8, Loc: chicago(t)}
	for _, tc := range []struct {
		now      string
		wantDate string // "" = nothing due
	}{
		{"2026-10-31T12:59:59Z", ""},           // 07:59:59 CDT
		{"2026-10-31T13:00:00Z", "2026-10-31"}, // 08:00 CDT
		{"2026-10-31T15:00:00Z", "2026-10-31"}, // 10:00 CDT: exactly the catch-up
		{"2026-10-31T15:00:01Z", ""},           // past it
		{"2026-11-02T13:30:00Z", ""},           // 07:30 CST: 13:30Z would be past 08:00 CDT
		{"2026-11-02T14:00:00Z", "2026-11-02"}, // 08:00 CST
		{"2026-11-03T05:00:00Z", ""},           // 23:00 CST on the 2nd: long past
	} {
		d, ok := s.Due(utc(tc.now))
		if tc.wantDate == "" {
			if ok {
				t.Errorf("at %s: due %s; want nothing", tc.now, d.Date)
			}
			continue
		}
		if !ok || d.Date != tc.wantDate {
			t.Errorf("at %s: due %q (%v); want %s", tc.now, d.Date, ok, tc.wantDate)
		}
	}
}

func TestParseAt(t *testing.T) {
	for _, tc := range []struct {
		in   string
		h, m int
		ok   bool
	}{
		{"08:00", 8, 0, true}, {"23:59", 23, 59, true}, {"00:05", 0, 5, true},
		{"8:00", 0, 0, false}, {"24:00", 0, 0, false}, {"08:60", 0, 0, false}, {"0800", 0, 0, false}, {"", 0, 0, false},
	} {
		h, m, err := ParseAt(tc.in)
		if (err == nil) != tc.ok || (tc.ok && (h != tc.h || m != tc.m)) {
			t.Errorf("ParseAt(%q) = %d, %d, %v; want %d, %d, ok=%v", tc.in, h, m, err, tc.h, tc.m, tc.ok)
		}
	}
}
