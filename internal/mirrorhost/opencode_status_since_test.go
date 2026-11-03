package mirrorhost

import (
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestAnIdleSessionIsTimedFromItsLastUpdateOnConnect (REGRESSION): a session
// whose status has not changed since the event stream opened takes its status
// time from its last update (Session.Info's time.updated, Unix ms), so it can
// be judged before its next turn ends; before, its time was 0. A session
// running at the connect is timed by the connect (the control), and a later
// session.status moves the time on as before.
func TestAnIdleSessionIsTimedFromItsLastUpdateOnConnect(t *testing.T) {
	srv, src := ocServer(t, map[string]string{"ses_fixtureIdle": "Idle one", "ses_fixtureBusy": "Busy one"})
	srv.SetStatus("ses_fixtureBusy", "busy")
	_ = src.state()
	waitUntil(t, "the event stream", func() bool { s, _ := src.status("x"); return s != "" })
	st, since := src.status("ses_fixtureIdle")
	if st != mirror.StatusIdle || since != 1700000000000 { // the fake's time.updated
		t.Fatalf("the idle session: %q at %d, want idle at its last update 1700000000000", st, since)
	}
	if st, since := src.status("ses_fixtureBusy"); st != mirror.StatusBusy || since <= 1700000000000 {
		t.Fatalf("the running session: %q at %d, want busy timed by the connect", st, since)
	}
	srv.SetStatus("ses_fixtureIdle", "busy")
	waitUntil(t, "the status event", func() bool {
		st, s := src.status("ses_fixtureIdle")
		return st == mirror.StatusBusy && s > 1700000000000
	})
}
