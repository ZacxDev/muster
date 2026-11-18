package api

import (
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
)

// waitingWindowReads runs 4 focus passes over one Claude window whose host
// reports status (with StatusSince two hours ago) and, as a host does for a
// busy or waiting window (mirrorhost.WireWindows), a LastMessage of "now" on
// every pass, while its transcript never moves. It returns how many tail reads
// (WatchWindow opens) the 4 passes made.
func waitingWindowReads(t *testing.T, status string, statusReport bool) int {
	t.Helper()
	since := time.Now().Add(-2 * time.Hour).UnixMilli()
	fm := &fakeMirror{online: true, statusReport: statusReport, v2: &fakeMirrorV2{batches: []mirror.Msg{
		{Type: mirror.TypeEvents, Gen: focusSID, Reset: true, Events: []cctranscript.Event{
			{Kind: cctranscript.KindPrompt, Text: "may I run the migration?"},
			{Kind: cctranscript.KindText, Text: "Waiting for your answer."},
		}},
		{Type: mirror.TypeEvents, Gen: focusSID, PromptIndex: true},
	}}}
	// The pass clock, 15 minutes apart: the refresher's own and the host's.
	clock := time.Now()
	setWindow := func() {
		fm.mu.Lock()
		fm.windows = []mirror.Window{{Session: "main", ID: "@1", Index: 1, Active: true, Claude: true, ClaudeSession: focusSID,
			Status: status, StatusSince: since, LastMessage: clock.Unix(), Activity: clock.Unix()}}
		fm.mu.Unlock()
	}
	setWindow()
	s, _ := focusMirrorServer(t, fm, &memFocus{rows: map[focus.Subject]focus.Focus{}}, &recordingSummarizer{})
	s.ext.Agents = noAgents{} // this pass is about the one mirrored window
	r := s.refresher()
	r.Now = func() time.Time { return clock }
	for pass := 0; pass < 4; pass++ {
		clock = clock.Add(15 * time.Minute)
		setWindow() // the host's "now" moves with every pass
		if _, err := r.RefreshOwner(operatorCtx(), 1); err != nil {
			t.Fatal(err)
		}
	}
	fm.mu.Lock()
	defer fm.mu.Unlock()
	return len(fm.asked)
}

// TestAWaitingWindowIsReadOnceWhileItWaits: a Claude window waiting on the
// person since before the last read, whose tail that read found unchanged, is
// skipped — although its host reports its activity as "now". 4 passes, 1 read
// (the first, which writes its focus).
//
// Regression: at 8931ddcc (and on origin/main) its activity moved every pass,
// so it was read on all 4.
func TestAWaitingWindowIsReadOnceWhileItWaits(t *testing.T) {
	if n := waitingWindowReads(t, mirror.StatusWaiting, true); n != 1 {
		t.Errorf("a waiting window was read %d times over 4 passes, want 1", n)
	}
}

// TestABusyWindowIsStillReadEveryPass: a BUSY window can write transcript rows
// at any moment and nothing on the wire says it has not, so it stays a
// candidate on every pass. And a host that never advertised StatusReport is
// not believed about "waiting" (wire.go: Status): its window is read as
// before. The first case is the control for the waiting test's fixture: the
// same window, only its status differs.
func TestABusyWindowIsStillReadEveryPass(t *testing.T) {
	if n := waitingWindowReads(t, mirror.StatusBusy, true); n != 4 {
		t.Errorf("a busy window was read %d times over 4 passes, want 4", n)
	}
	if n := waitingWindowReads(t, mirror.StatusWaiting, false); n != 4 {
		t.Errorf("a waiting window on a host with no status report was read %d times, want 4", n)
	}
}
