package sendqueue

import (
	"testing"
	"time"
)

// THE SESSION A QUEUED MESSAGE WAS WRITTEN FOR (migration 0105): stored with
// the row, read back, carried by a Retry; a row with none (every row before
// the column, a Claude Code window's) reads "", and a value the wire would
// not carry is refused before anything is stored.
func TestAQueuedRowKeepsTheSessionItWasWrittenFor(t *testing.T) {
	s, pool := pgStore(t)
	named := mustEnqueue(t, s, 2, Item{Target: win, Body: "for X", StepUpAt: time.Now(), AgentSession: "ses_fixtureXsession"})
	if got, err := s.Get(as(2), named.ID); err != nil || got.AgentSession != "ses_fixtureXsession" {
		t.Fatalf("read back %+v (%v)", got, err)
	}
	none := mustEnqueue(t, s, 2, Item{Target: win, Body: "for nobody", StepUpAt: time.Now()})
	var stored *string
	if err := pool.QueryRow(as(2), `SELECT agent_session FROM send_queue WHERE id = $1`, none.ID).Scan(&stored); err != nil || stored != nil {
		t.Fatalf("a row naming no session stored %v (%v); want NULL", stored, err)
	}
	if none.AgentSession != "" {
		t.Fatalf("a row naming no session reads %q", none.AgentSession)
	}
	if _, err := s.Enqueue(as(2), Item{Target: win, Body: "bad", StepUpAt: time.Now(), AgentSession: "not a session!"}); err == nil {
		t.Fatal("a session id the wire would not carry was stored")
	}
	failRow(t, s, named.ID, false)
	again, err := s.Retry(as(2), named.ID, time.Now(), 0)
	if err != nil || again.AgentSession != "ses_fixtureXsession" {
		t.Fatalf("a Retry: %+v (%v); want it written for the same session", again, err)
	}
}
