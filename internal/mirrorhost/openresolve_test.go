package mirrorhost

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE OPEN'S SHARED WINDOW READ (openresolve.go), without tmux: every runtime
// an open asks shares ONE read, and the moment the open has answered every
// read is fresh again. The real-tmux half — a stale listing never opens the
// wrong pane — is TestTmuxAStaleListingNeverOpensTheWrongPane (-tags tmuxit).

// revalidatingBackend is a windowsBackend that revalidates every entry as
// given (or refuses all), counting its calls.
type revalidatingBackend struct {
	*windowsBackend
	ok    bool
	calls int
}

func (b *revalidatingBackend) RevalidateWindow(_ context.Context, was WindowInfo) (WindowInfo, bool) {
	b.calls++
	return was, b.ok
}

func hostOf(b Backend) *hostSide {
	return &hostSide{c: &Client{cfg: Config{Backend: b, Refresh: 5 * time.Second}}, ctx: context.Background()}
}

var openWindowFixture = WindowInfo{Session: "main", ID: "@3", Pane: "%3", PanePID: 30, Claude: &ClaudePane{Pane: "%3", PID: 31, Gen: "g", Source: SourceSessions}}

func TestAnOpenReadsItsWindowOnceThenFreshAgain(t *testing.T) {
	b := &windowsBackend{windows: []WindowInfo{openWindowFixture}}
	h := hostOf(b)
	// No open: every read is a whole listing, as before.
	for i := 0; i < 2; i++ {
		if _, err := h.window(context.Background(), "main", "@3"); err != nil {
			t.Fatal(err)
		}
	}
	if b.reads != 2 {
		t.Fatalf("CONTROL: %d listings for two reads outside an open, want 2", b.reads)
	}
	ctx, _ := h.beginResolve(context.Background(), 7)
	for i := 0; i < 3; i++ { // opencode's claim, Hermes's, Claude Code's tail
		if _, err := h.window(ctx, "main", "@3"); err != nil {
			t.Fatal(err)
		}
	}
	if b.reads != 3 {
		t.Fatalf("an open's three runtimes made %d listings, want ONE shared", b.reads-2)
	}
	if how := openResolveOf(ctx).outcome(); how != resolveFresh {
		t.Fatalf("resolved %q, want %q (no listing to revalidate)", how, resolveFresh)
	}
	// The first reply spends it: a re-resolve afterwards reads afresh.
	h.reply(7)
	if _, err := h.window(ctx, "main", "@3"); err != nil {
		t.Fatal(err)
	}
	if b.reads != 4 {
		t.Fatalf("a read after the open answered made %d listing(s), want 1: the memo outlived the open", b.reads-3)
	}
}

func TestAnOpenStartsFromTheRevalidatedListing(t *testing.T) {
	for _, c := range []struct {
		name      string
		ok        bool
		age       time.Duration
		was       WindowInfo
		wantHow   string
		wantCalls int
	}{
		{"revalidated", true, 0, openWindowFixture, resolveRevalidated, 1},
		{"refused by the backend", false, 0, openWindowFixture, resolveFresh, 1},
		{"a listing too old", true, time.Minute, openWindowFixture, resolveFresh, 0},
		{"not a Claude Code window", true, 0, WindowInfo{Session: "main", ID: "@3", Pane: "%3", PanePID: 30}, resolveFresh, 0},
		{"a guessed transcript", true, 0, WindowInfo{Session: "main", ID: "@3", Pane: "%3", PanePID: 30,
			Claude: &ClaudePane{Pane: "%3", PID: 31, Gen: "g", Source: SourceGuessed}}, resolveFresh, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := &revalidatingBackend{windowsBackend: &windowsBackend{windows: []WindowInfo{openWindowFixture}}, ok: c.ok}
			h := hostOf(b)
			h.snap.Store(&windowSnap{at: time.Now().Add(-c.age), windows: []WindowInfo{c.was}})
			ctx, _ := h.beginResolve(context.Background(), 1)
			if _, err := h.window(ctx, "main", "@3"); err != nil {
				t.Fatal(err)
			}
			if how := openResolveOf(ctx).outcome(); how != c.wantHow || b.calls != c.wantCalls {
				t.Fatalf("resolved %q with %d revalidation(s), want %q with %d", how, b.calls, c.wantHow, c.wantCalls)
			}
			wantReads := 0
			if c.wantHow == resolveFresh {
				wantReads = 1
			}
			if b.reads != wantReads {
				t.Fatalf("%d whole listing(s), want %d", b.reads, wantReads)
			}
		})
	}
}

// TestAViewStillFollowsAClearAfterItsOpen: the shared read is the OPEN's.
// After the first batch the view's re-resolve reads the window afresh, so a
// /clear (another transcript in the same pane) still starts the view over.
func TestAViewStillFollowsAClearAfterItsOpen(t *testing.T) {
	dir := t.TempDir()
	tr := func(name, marker string) string {
		p := filepath.Join(dir, name+".jsonl")
		row, _ := json.Marshal(map[string]any{"type": "user", "promptId": "p", "message": map[string]any{"content": marker}})
		if err := os.WriteFile(p, append(row, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	w := func(path, gen string) WindowInfo {
		return WindowInfo{Session: "main", ID: "@3", Index: 0, Active: true, Pane: "%3", PanePID: 30, Command: "claude",
			Claude: &ClaudePane{Pane: "%3", PID: 31, Transcript: path, Gen: gen, Source: SourceSessions}}
	}
	b := &windowsBackend{windows: []WindowInfo{w(tr("a", "marker-before"), "ga")}}
	d := newDrivenMuster(t)
	d.run(t, b, nil, false)
	d.send(t, mirror.Msg{Type: mirror.TypeWatch, ID: 5, Session: "main", Window: "@3"})
	waitUntil(t, "the open's batch", func() bool { evs, _, _ := watchEvents(d, 5); return strings.Contains(evText(evs), "marker-before") })
	b.set(w(tr("b", "marker-after"), "gb"))
	waitUntil(t, "the view starting over on the new transcript", func() bool {
		evs, resets, _ := watchEvents(d, 5)
		return resets >= 2 && strings.Contains(evText(evs), "marker-after")
	})
}
