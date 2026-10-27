package ui

import (
	"fmt"
	"strconv"
	"sync/atomic"
	"time"
)

// LIVE CONNECTIONS COME BACK BY THEMSELVES (operator ask: "Autoreconnect when
// returning or reopening a stale page (ex returning to app after it went to
// sleep)").
//
// Every live connection a page holds — the Rendered view's transcript socket
// (mirror_view.go), a terminal's socket (terminal.go), an agent chat's socket
// (agents_detail.go), the htmx /events streams (restore.go) and the asks list's
// own /events (attention.go) — reconnects with no tap when:
//
//   - the page is shown again (visibilitychange → visible), restored from the
//     back/forward cache (pageshow, persisted), the network comes back
//     (online) or the window takes focus: the WAKE triggers, which restoreJS
//     coalesces into one wake per moment and hands to every participant;
//   - its socket closed: it retries with backoff and jitter (liveRetryJS)
//     while the page is visible and online, and waits for a wake while not —
//     a hidden page does not hammer the server;
//   - its socket LOOKS open but is dead. A phone that slept, or a network that
//     changed under it, can leave a socket OPEN on the page's side with
//     nothing on the other. Every one of these servers sends a heartbeat at
//     least every LiveHeartbeat (a `{"type":"ping"}` frame, or an SSE
//     `muster.ping` event), so a socket that has heard nothing for
//     LiveWakeStale at a wake, or for LiveStale at any watchdog tick
//     (LiveTickMs, while visible), is replaced.
//
// 🔴 A SOCKET IS ONLY JUDGED STALE ONCE IT HAS HEARD ONE HEARTBEAT. A page
// talking to a build that sends none (a rollout in progress) would otherwise
// replace a healthy, quiet socket every LiveStale. Until its first heartbeat a
// socket is replaced only when it is closed.
//
// One connection at a time: a reconnect closes the socket it replaces (its
// handlers detached first, so its close is not a second reconnect), and a
// socket still CONNECTING is left to finish rather than doubled.
//
// Each reconnect is an open of its own in the page's telemetry
// (faro.go's musterOpen) tagged reconnect=true and reconnect_reason — the
// wake's reason (visibility, pageshow, online, focus), stale (the watchdog)
// or retry (a closed socket's backoff) — so Tempo can tell it from a first
// open.

// DefaultLiveHeartbeat is how often a live stream's server sends a heartbeat
// when MUSTER_LIVE_HEARTBEAT is unset.
const DefaultLiveHeartbeat = 15 * time.Second

// MinLiveHeartbeat and MaxLiveHeartbeat bound MUSTER_LIVE_HEARTBEAT.
const (
	MinLiveHeartbeat = 250 * time.Millisecond
	MaxLiveHeartbeat = 5 * time.Minute
)

var liveHeartbeat atomic.Int64

func init() { liveHeartbeat.Store(int64(DefaultLiveHeartbeat)) }

// LiveHeartbeat is the interval every live stream's server sends a heartbeat
// at, and the one the page's staleness thresholds are derived from (the page
// reads it off appScript's tag, data-heartbeat).
func LiveHeartbeat() time.Duration { return time.Duration(liveHeartbeat.Load()) }

// SetLiveHeartbeat sets LiveHeartbeat, process-wide. It refuses a value
// outside MinLiveHeartbeat–MaxLiveHeartbeat.
func SetLiveHeartbeat(d time.Duration) error {
	if d < MinLiveHeartbeat || d > MaxLiveHeartbeat {
		return fmt.Errorf("live heartbeat %s is outside %s–%s", d, MinLiveHeartbeat, MaxLiveHeartbeat)
	}
	liveHeartbeat.Store(int64(d))
	return nil
}

// ParseLiveHeartbeat parses MUSTER_LIVE_HEARTBEAT: "" is 0 (keep the default),
// anything else a Go duration within the bounds.
func ParseLiveHeartbeat(v string) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%q: %w (want a Go duration, e.g. 15s)", v, err)
	}
	if d < MinLiveHeartbeat || d > MaxLiveHeartbeat {
		return 0, fmt.Errorf("%q is outside %s–%s", v, MinLiveHeartbeat, MaxLiveHeartbeat)
	}
	return d, nil
}

// LiveStale is how long a socket that has heard a heartbeat may go silent
// before the watchdog replaces it: two heartbeats missed, and a margin.
func LiveStale(hb time.Duration) time.Duration { return 2*hb + liveMargin(hb) }

// LiveWakeStale is the same at a wake: one heartbeat missed, and a margin. A
// page waking from sleep has not been able to hear anything, so the bound is
// tighter than the watchdog's.
func LiveWakeStale(hb time.Duration) time.Duration { return hb + liveMargin(hb) }

// liveMargin is a third of a heartbeat, at most five seconds.
func liveMargin(hb time.Duration) time.Duration {
	m := hb / 3
	if m > 5*time.Second {
		m = 5 * time.Second
	}
	return m
}

// LiveTickMs is how often a visible page's watchdog looks at its sockets.
const LiveTickMs = 1000

// LiveRetryBaseMs and LiveRetryCapMs shape a closed socket's backoff: base ×
// 2^attempt, capped, then jittered to between half and all of it.
const (
	LiveRetryBaseMs = 500
	LiveRetryCapMs  = 15000
)

// LiveManualAfter is how many reconnects in a row may fail before a view
// offers its manual Reconnect (it keeps retrying on its own all the same).
const LiveManualAfter = 3

// LivePingFrame is a socket's heartbeat, exactly as its server writes it (a
// text frame): a page takes it as "alive" and nothing else.
const LivePingFrame = `{"type":"ping"}`

// LivePingEvent is the /events stream's heartbeat event (its data is "{}").
const LivePingEvent = "muster.ping"

// LiveHeartbeatAttr is appScript's tag attribute carrying LiveHeartbeat in
// milliseconds.
const LiveHeartbeatAttr = "data-heartbeat"

// liveRetryJS defines liveDelay(attempt): the backoff, with jitter, before
// reconnect attempt n (1-based). Each participant's script carries it, since
// each runs before appScript.
var liveRetryJS = `
  function liveDelay(n) {
    var d = Math.min(` + strconv.Itoa(LiveRetryCapMs) + `, ` + strconv.Itoa(LiveRetryBaseMs) + ` * Math.pow(2, Math.max(0, n - 1)));
    return Math.round(d * (0.5 + Math.random() * 0.5));
  }
  // liveCanRetry: a closed socket retries on its own only while the page is
  // shown and online; otherwise the next wake reconnects it.
  function liveCanRetry() { return !document.hidden && navigator.onLine !== false; }
`
