package main

import (
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

// TestTheOfficerAutoRestartIsReadFromItsTwoVariables: unset is the defaults;
// either variable off is the zero value (off); a value in range is read;
// anything else refuses to boot. Expectations are literals.
func TestTheOfficerAutoRestartIsReadFromItsTwoVariables(t *testing.T) {
	for _, c := range []struct {
		after, perDay string
		want          officers.AutoRestart
		refused       bool
	}{
		{"", "", officers.AutoRestart{After: 5 * time.Minute, MaxPerDay: 4}, false},
		{"7m", "", officers.AutoRestart{After: 7 * time.Minute, MaxPerDay: 4}, false},
		{"", "9", officers.AutoRestart{After: 5 * time.Minute, MaxPerDay: 9}, false},
		{" 90s ", " 2 ", officers.AutoRestart{After: 90 * time.Second, MaxPerDay: 2}, false},
		{"off", "", officers.AutoRestart{}, false},
		{"0", "6", officers.AutoRestart{}, false},
		{"11m", "0", officers.AutoRestart{}, false},
		{"30s", "", officers.AutoRestart{}, true},
		{"25h", "", officers.AutoRestart{}, true},
		{"soon", "", officers.AutoRestart{}, true},
		{"", "-1", officers.AutoRestart{}, true},
		{"", "101", officers.AutoRestart{}, true},
		{"", "four", officers.AutoRestart{}, true},
	} {
		env := map[string]string{envOfficerAutoRestartAfter: c.after, envOfficerAutoRestartMaxPerDay: c.perDay}
		got, err := parseOfficerAutoRestart(func(k string) string { return env[k] })
		if (err != nil) != c.refused || got != c.want {
			t.Errorf("after %q per-day %q: %+v, %v; want %+v, refused %v", c.after, c.perDay, got, err, c.want, c.refused)
		}
	}
}
