package ui

import (
	"strings"
	"testing"
	"time"
)

// TestPlainWhenSaysTheDayAndNoClock: the server's words name the day only
// (the script adds the reader's own time); never a clock or "UTC".
func TestPlainWhenSaysTheDayAndNoClock(t *testing.T) {
	now := time.Date(2030, 3, 9, 1, 30, 0, 0, time.UTC)
	for _, c := range []struct {
		t    time.Time
		want string
	}{
		{now.Add(-time.Hour), "today"},
		{now.Add(-24 * time.Hour), "yesterday"},
		{time.Date(2030, 1, 4, 23, 0, 0, 0, time.UTC), "on Jan 4"},
		{time.Date(2029, 12, 31, 8, 0, 0, 0, time.UTC), "on Dec 31, 2029"},
	} {
		if got := PlainWhen(c.t, now); got != c.want {
			t.Errorf("PlainWhen(%v) = %q, want %q", c.t, got, c.want)
		}
	}
	if h := PlainWhenHTML("Sent ", now, now); !strings.HasSuffix(h, `>Sent today</time>`) || strings.Contains(h, "UTC<") {
		t.Errorf("PlainWhenHTML: %s", h)
	}
}
