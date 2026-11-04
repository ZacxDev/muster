package opencodehost

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

// TestAnOversizeEventIsSkippedAndTheStreamGoesOn (🟡-1): one event line over
// maxEventLine used to END the stream (bufio.Scanner's ErrTooLong); now it is
// discarded as it is read, the reader is told one was skipped
// (EventSkipped), and the next event still arrives on the SAME stream. An
// event just under the bound still arrives whole (the control).
func TestAnOversizeEventIsSkippedAndTheStreamGoesOn(t *testing.T) {
	srv := opencodetest.New(fixturePassword)
	defer srv.Close()
	tok := srv.Issue()
	cl, err := NewClient(srv.URL, func() string { return tok })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan Event, 16)
	ended := make(chan error, 1)
	go func() { ended <- cl.Events(ctx, nil, func(e Event) { got <- e }) }()
	deadline := time.Now().Add(5 * time.Second)
	for srv.Subscribers() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	frame := func(pad int) string {
		b, _ := json.Marshal(map[string]any{"id": "evt_big", "type": "session.message.content.updated",
			"data": map[string]any{"sessionID": "ses_fixtureA", "pad": strings.Repeat("x", pad)}})
		return string(b)
	}
	srv.EmitFrame(frame(maxEventLine - 200))   // under the bound
	srv.EmitFrame(frame(maxEventLine + 1<<20)) // over it
	srv.EmitFrame(opencodetest.FixtureFrame("permission.asked", 0))
	want := []string{"server.connected", "session.message.content.updated", EventSkipped, "permission.asked"}
	for i, w := range want {
		select {
		case e := <-got:
			if e.Type != w {
				t.Fatalf("event %d: %q, want %q", i, e.Type, w)
			}
		case err := <-ended:
			t.Fatalf("the stream ended at event %d (%v): an oversize line must not end it", i, err)
		case <-time.After(10 * time.Second):
			t.Fatalf("event %d never came", i)
		}
	}
}

// The turn's lifecycle frames (REAL ones) are the session's status: the v2
// server publishes no session.status.
func TestTheLifecycleFramesAreAStatus(t *testing.T) {
	for typ, want := range map[string]string{"session.execution.started": "busy", "session.execution.interrupted": "idle"} {
		var e Event
		if err := json.Unmarshal([]byte(opencodetest.AbortFrame(typ, 0)), &e); err != nil {
			t.Fatal(err)
		}
		if got := e.Status(); got != want || e.SessionID() != opencodetest.AbortPermissionSession {
			t.Errorf("%s: status %q session %q, want %q", typ, got, e.SessionID(), want)
		}
	}
	for _, typ := range []string{"session.execution.succeeded", "session.execution.failed"} {
		if got := (Event{Type: typ}).Status(); got != "idle" {
			t.Errorf("%s: %q, want idle", typ, got)
		}
	}
	if got := (Event{Type: "session.status", Data: json.RawMessage(`{"status":{"type":"retry"}}`)}).Status(); got != "retry" {
		t.Errorf("session.status is still read: %q", got)
	}
	if got := (Event{Type: "permission.asked"}).Status(); got != "" {
		t.Errorf("another event has a status: %q", got)
	}
}
