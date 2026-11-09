package mirrorhost

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestEveryWindowReadReachesOnWindows: each per-window refresh read hands
// Config.OnWindows the raw listing it read (previews take their window
// listing from here), and a failed read hands it nothing — so a tmux hiccup
// is never mistaken for every window closing.
func TestEveryWindowReadReachesOnWindows(t *testing.T) {
	raw := []WindowInfo{{Session: "work", ID: "@3", Pane: "%1", PanePID: 100}, {Session: "docs", ID: "@5", Pane: "%2", PanePID: 300}}
	b := &windowsBackend{windows: raw}
	var got [][]WindowInfo
	h := &hostSide{c: &Client{cfg: Config{Backend: b, OnWindows: func(ws []WindowInfo) { got = append(got, ws) }}, log: log.New(io.Discard, "", 0)},
		ctx: context.Background(), version: mirror.ProtocolVersion}
	for i := 0; i < 2; i++ {
		if _, ok := h.read(); !ok {
			t.Fatal("the read failed")
		}
	}
	if len(got) != 2 || len(got[1]) != 2 || got[1][0].ID != "@3" || got[1][1].PanePID != 300 {
		t.Fatalf("OnWindows got %+v; want the listing twice", got)
	}
	b.mu.Lock()
	b.err = errors.New("tmux went away")
	b.mu.Unlock()
	if _, ok := h.read(); ok {
		t.Fatal("instrument: the failing read succeeded")
	}
	if len(got) != 2 {
		t.Fatalf("a failed read reached OnWindows: %+v", got[2:])
	}
}
