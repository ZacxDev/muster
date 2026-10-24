package mirrorhost

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// statusWindows is windowsBackend whose Claude windows report a status.
type statusWindows struct{ *windowsBackend }

func (statusWindows) ReportsStatus() bool { return true }

// statusClaudeWindow is one Claude window writing transcript, at status (st, since).
func statusClaudeWindow(transcript, st string, since int64) WindowInfo {
	return WindowInfo{Session: "work", ID: "@1", Index: 1, Active: true, Pane: "%1", PanePID: 1, Command: "claude",
		Claude: &ClaudePane{Pane: "%1", PID: 1, Transcript: transcript, Gen: "g1", Busy: st == mirror.StatusBusy, Status: st, StatusSince: since}}
}

// TestAViewStreamNamesItsStatusTimeAndNudgesTheList: a Claude Code view's
// stream carries the time of the status its busy flag was read from
// (Msg.StatusSince) — on the backfill, on the prompt index and on every
// marker — and a status whose TIME moved while busy stayed the same (a turn
// that started and ended between two re-resolves) is a marker of its own.
// Each such marker is preceded by a nudge, so the window list naming the same
// time reaches muster now, not at the next Refresh (an hour here).
//
// Suggested replies read that time as the turn's own status token
// (internal/api's suggestTracker): without it, a turn end could only be told
// from the idle BEFORE the turn by guessing.
func TestAViewStreamNamesItsStatusTimeAndNudgesTheList(t *testing.T) {
	const s0, s1, s2 = int64(1_792_000_000_100), int64(1_792_000_000_200), int64(1_792_000_000_300)
	tr := filepath.Join(t.TempDir(), "g1.jsonl")
	if err := os.WriteFile(tr, []byte(`{"type":"user","promptId":"p","origin":{"kind":"human"},"message":{"role":"user","content":"go"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := &windowsBackend{windows: []WindowInfo{statusClaudeWindow(tr, mirror.StatusIdle, s0)}}
	d := newDrivenMuster(t)
	c, err := New(Config{URL: d.srv.URL, Token: mirror.TokenPrefix + "t", Host: "laptop", Backend: statusWindows{b},
		AllowHTTP: true, Refresh: time.Hour, ViewPoll: 5 * time.Millisecond, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.RunOnce(ctx) }()
	waitUntil(t, "the hello", func() bool { return len(d.of(mirror.TypeHello)) == 1 })
	d.send(t, mirror.Msg{Type: mirror.TypeWatch, ID: 3, Session: "work", Window: "@1"})

	events := func() []mirror.Msg { return d.of(mirror.TypeEvents) }
	waitUntil(t, "the prompt index", func() bool {
		for _, m := range events() {
			if m.PromptIndex {
				return true
			}
		}
		return false
	})
	for _, m := range events() {
		if m.StatusSince != s0 {
			t.Fatalf("a backfill message without the status time: %+v", m)
		}
	}
	listed := func(since int64) func() bool {
		return func() bool {
			for _, m := range d.of(mirror.TypeSessions) {
				for _, w := range m.Windows {
					if w.ID == "@1" && w.StatusSince == since {
						return true
					}
				}
			}
			return false
		}
	}
	marker := func(busy bool, since int64) func() bool {
		return func() bool {
			for _, m := range events() {
				if !m.PromptIndex && len(m.Events) == 0 && m.Busy == busy && m.StatusSince == since {
					return true
				}
			}
			return false
		}
	}

	// A short turn the re-resolve never saw busy: idle(s0) → idle(s1).
	b.set(statusClaudeWindow(tr, mirror.StatusIdle, s1))
	waitUntil(t, "an idle marker naming the new time", marker(false, s1))
	waitUntil(t, "the nudged window list naming it (Refresh is an hour)", listed(s1))

	// busy(s2): the busy marker names its time too.
	b.set(statusClaudeWindow(tr, mirror.StatusBusy, s2))
	waitUntil(t, "a busy marker naming its time", marker(true, s2))
	waitUntil(t, "the nudged list naming it", listed(s2))

	// A live batch (the turn's reply) carries the time as well.
	f, err := os.OpenFile(tr, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"assistant","message":{"content":[{"type":"text","text":"working on it"}]}}` + "\n")
	_ = f.Close()
	waitUntil(t, "the live reply naming the status time", func() bool {
		for _, m := range events() {
			if len(m.Events) > 0 && m.Events[0].Text == "working on it" {
				return m.Busy && m.StatusSince == s2
			}
		}
		return false
	})
}
