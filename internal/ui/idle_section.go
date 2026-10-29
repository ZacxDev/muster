package ui

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// THE "IDLE" SECTION. A real operator's list is a hundred-odd entries, almost
// all of them live tmux windows mirrored from their hosts, and most of those
// untouched for days. Entries with no activity for MUSTER_AGENTS_IDLE_AFTER
// (14 days by default) are drawn LAST in the sidebar, under a collapsed
// "Idle (N)" disclosure (navIdleGroup), instead of among the working ones. (It
// began as the retired Agents list page's section.)
//
// 🔴 COLLAPSE ONLY. Nothing is closed, stopped or hidden on a host: an idle
// window is still listed, searchable, pinnable and openable — it is only drawn
// in the section, which starts collapsed.
//
// WHAT "ACTIVITY" IS: the entry's activeAt (list_order.go) — the SAME number the
// sidebar row's recency dot reads and the entries are ordered by, so the
// section can never disagree with the dot:
//
//   - a mirrored Claude Code window: its conversation's LAST MESSAGE — the
//     newest non-meta user or assistant row in its transcript, read by
//     muster-mirror (mirrorhost's lastmsg.go) and floored to a minute. Not
//     tmux's window activity: a tmux restart (a tmux-resurrect restore) resets
//     that for every window, and measured on one host it put all 67 windows
//     "active" since the restore, so nothing could ever collapse. While the
//     session is working (a turn running, waiting on the person, a background
//     shell) it is the host's now — busy counts as active, so a working window
//     is never idle. A host from before reports none, and the window falls
//     back to tmux's;
//   - any other mirrored window: when its host last saw it active (tmux's
//     window activity, floored to a minute);
//   - a dispatched agent: agents.LastActivity, the newer of its newest
//     transcript message and its row's updated_at.
//
// Not the focus's updated_at: the focus is rewritten by a periodic summarizer
// pass (MUSTER_FOCUS_INTERVAL) whether or not the session did anything, so it
// says when muster last looked, not when the window was last used.
//
// The rule (InIdleSection): past the threshold STRICTLY (an entry idle exactly
// the threshold is not idle), never when pinned, never when the activity is
// unknown (a host that did not say), never when the threshold is 0 (the section
// is off). It is decided by the SERVER, on every build of the entries, with the
// server's clock — so a window that becomes active again leaves the section on
// the sidebar's next refresh.
//
// HOW IT IS DRAWN: see navIdleGroup (nav_agents.go). The sidebar also keeps a
// row that is waiting on the operator, the chief or the page's own session out
// of the group (navInIdleGroup). The session switcher's search finds an idle
// entry without opening it.

// DefaultIdleSectionAfter is how long an entry must be idle to be drawn in the
// section when MUSTER_AGENTS_IDLE_AFTER is unset.
const DefaultIdleSectionAfter = 14 * 24 * time.Hour

// Bounds of a configured threshold (ParseIdleSectionAfter); 0 is "off".
const (
	MinIdleSectionAfter = time.Hour
	MaxIdleSectionAfter = 365 * 24 * time.Hour
)

// agentsIdleOpenKey is where the viewer's open/closed choice is kept
// (localStorage): "1" open; absent closed (the default).
const agentsIdleOpenKey = "muster.agents.idle.open"

// InIdleSection is the ONE rule: whether an entry last active at activeAt is
// drawn in the Idle section, as of now. See the file header.
func InIdleSection(now, activeAt time.Time, pinned bool, after time.Duration) bool {
	return after > 0 && !pinned && !activeAt.IsZero() && now.Sub(activeAt) > after
}

// MarkIdle sets each view's Idle by InIdleSection over its last activity
// (listEntry.activeAt), in place. internal/api calls it once per build of the
// entries, after the pins are marked.
func MarkIdle(agents []AgentCardView, mirrors []MirrorCardView, now time.Time, after time.Duration) {
	for i := range agents {
		agents[i].Idle = InIdleSection(now, agents[i].lastActive(), agents[i].Pinned, after)
	}
	for i := range mirrors {
		mirrors[i].Idle = InIdleSection(now, mirrors[i].ActiveAt, mirrors[i].Pinned, after)
	}
}

// ParseIdleSectionAfter reads MUSTER_AGENTS_IDLE_AFTER: "" is the default; "0"
// or "off" turns the section off; otherwise a whole number of days ("14d") or
// a Go duration ("336h"), from MinIdleSectionAfter to MaxIdleSectionAfter.
func ParseIdleSectionAfter(v string) (time.Duration, error) {
	v = strings.TrimSpace(strings.ToLower(v))
	switch v {
	case "":
		return DefaultIdleSectionAfter, nil
	case "0", "off":
		return 0, nil
	}
	var d time.Duration
	if n, ok := strings.CutSuffix(v, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days < 0 {
			return 0, fmt.Errorf("%q is not a whole number of days", v)
		}
		d = time.Duration(days) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(v); err != nil {
			return 0, fmt.Errorf("%q is neither a number of days (14d) nor a Go duration (336h)", v)
		}
	}
	if d < MinIdleSectionAfter || d > MaxIdleSectionAfter {
		return 0, fmt.Errorf("%q is outside %s–%s (0 or off turns the section off)", v, MinIdleSectionAfter, MaxIdleSectionAfter)
	}
	return d, nil
}

// IdleSectionAfterLabel is a threshold as the boot banner says it (the retired
// Agents list's Idle header said it too): whole days ("14 days"), else whole
// hours ("36 hours"), else the duration.
func IdleSectionAfterLabel(d time.Duration) string {
	plural := func(n int64, unit string) string {
		if n == 1 {
			return "1 " + unit
		}
		return strconv.FormatInt(n, 10) + " " + unit + "s"
	}
	switch {
	case d > 0 && d%(24*time.Hour) == 0:
		return plural(int64(d/(24*time.Hour)), "day")
	case d > 0 && d%time.Hour == 0:
		return plural(int64(d/time.Hour), "hour")
	}
	return d.String()
}
