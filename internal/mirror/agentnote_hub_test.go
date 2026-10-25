package mirror_test

import (
	"sync/atomic"
	"testing"
	"time"
)

// openCodeWindows is one session's windows as JSON: @1 an opencode window
// with no view, whose note is note.
func openCodeWindows(note string) string {
	return `[{"session":"work","id":"@1","index":0,"active":true,"runtime":"opencode","agentNote":"` + note + `"}]`
}

// TestANoteOnlyChangeReachesTheCards: an opencode window whose ONLY change is
// its note (it was not paired; now its title is ambiguous) nudges the open
// views and the hub holds the new note — so the card shows why the window has
// no view now, not why it had none before.
func TestANoteOnlyChangeReachesTheCards(t *testing.T) {
	r := newRig(t)
	r.svc.SetNotifyDebounce(5 * time.Millisecond)
	var nudges atomic.Int64
	r.svc.SetNotify(func(owner int64) {
		if owner == 1 {
			nudges.Add(1)
		}
	})
	tok, _ := r.store.mint(t, 1, "ws")
	h := connectJSON(t, r, tok, `{"type":"hello","version":2,"host":"ws","statusReport":true,"windows":`+openCodeWindows("first reason")+`}`)
	r.online(t, 1, "ws", "work")
	note := func() string {
		for _, e := range hostView(t, r, 1, "ws").Entries() {
			return e.AgentNote
		}
		return ""
	}
	eventually(t, "the first note", func() bool { return note() == "first reason" })
	time.Sleep(120 * time.Millisecond)
	base := nudges.Load()
	if base == 0 {
		t.Fatal("instrument check: connecting nudged nobody")
	}
	h.sendWindows(t, openCodeWindows("second reason"))
	eventually(t, "the hub to hold the new note", func() bool { return note() == "second reason" })
	eventually(t, "a nudge for the note-only change", func() bool { return nudges.Load() > base })
}
