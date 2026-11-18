package mirror_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestAWindowEndingATurnCallsTheStatusHookButNotTheWaitingHook: a Claude window
// going busy → idle — where a turn ends, and the ask log reads a plain-text
// question — calls the status hook and NOT the waiting hook (whose set did not
// change); idle → busy calls it again; a message that leaves every status as
// it was (a new StatusSince on the same status is still the same status) calls
// neither. The waiting hook still fires for a window that starts waiting.
func TestAWindowEndingATurnCallsTheStatusHookButNotTheWaitingHook(t *testing.T) {
	r := newRig(t)
	r.svc.SetNotifyDebounce(5 * time.Millisecond)
	var statuses, waits atomic.Int64
	r.svc.SetStatusNotify(func(owner int64) {
		if owner == 1 {
			statuses.Add(1)
		}
	})
	r.svc.SetWaitingNotify(func(owner int64) {
		if owner == 1 {
			waits.Add(1)
		}
	})
	tok, _ := r.store.mint(t, 1, "laptop")
	win := func(status string, since int64) mirror.Window {
		return mirror.Window{Session: "work", ID: "@1", Index: 0, Active: true, Claude: true, ClaudeSession: "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b",
			Status: status, StatusSince: since}
	}
	raw, err := r.rawConnect(t, tok, winHello("laptop", false, win(mirror.StatusBusy, 1)))
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "laptop", "work")
	time.Sleep(120 * time.Millisecond)
	s0, w0 := statuses.Load(), waits.Load()
	send := func(w mirror.Window) {
		raw.write(t, mirror.Msg{Type: mirror.TypeSessions, Windows: []mirror.Window{w}})
		time.Sleep(120 * time.Millisecond)
	}
	send(win(mirror.StatusIdle, 2))
	eventually(t, "the status hook on a turn's end", func() bool { return statuses.Load() == s0+1 })
	if got := waits.Load(); got != w0 {
		t.Fatalf("busy → idle called the waiting hook %d time(s)", got-w0)
	}
	send(win(mirror.StatusIdle, 3)) // the same status
	send(win(mirror.StatusBusy, 4))
	eventually(t, "the status hook on a turn's start", func() bool { return statuses.Load() == s0+2 })
	time.Sleep(120 * time.Millisecond)
	if got := statuses.Load(); got != s0+2 {
		t.Fatalf("the status hook ran %d times, want 2 (an unchanged status calls nothing)", got-s0)
	}
	send(win(mirror.StatusWaiting, 5))
	eventually(t, "both hooks on starting to wait", func() bool { return statuses.Load() == s0+3 && waits.Load() == w0+1 })
}
