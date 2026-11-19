package mirrorhost

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// watchHarness is one sessionsWatch with its refreshes counted and its log
// lines kept.
type watchHarness struct {
	w     *sessionsWatch
	fired atomic.Int64
	mu    sync.Mutex
	logs  []string
	stop  func()
}

func newWatchHarness(t *testing.T) *watchHarness {
	t.Helper()
	h := &watchHarness{}
	h.w = newSessionsWatch(func() { h.fired.Add(1) }, func(f string, a ...any) {
		h.mu.Lock()
		h.logs = append(h.logs, fmt.Sprintf(f, a...))
		h.mu.Unlock()
	}, nil)
	h.stop = h.w.start(context.Background())
	t.Cleanup(func() {
		// stop returns once the watch's goroutine has exited (and closed
		// fsnotify's watcher): a stop that hangs is a leak.
		done := make(chan struct{})
		go func() { h.stop(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the watch did not stop")
		}
	})
	return h
}

func (h *watchHarness) logged() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.logs...)
}

// quiet waits until no refresh has fired for twice the throttle, and returns
// the count: what a following assertion compares against.
func (h *watchHarness) quiet() int64 {
	n := h.fired.Load()
	for {
		time.Sleep(2 * sessionsWatchEvery)
		if m := h.fired.Load(); m == n {
			return n
		} else {
			n = m
		}
	}
}

func writeSessJSON(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestTheSessionsWatchFiresOnEachKindOfChange: a sessions file created,
// rewritten, renamed into place (an atomic write) and removed each asks for a
// refresh, promptly — well within a second. A file that is not *.json does
// not: Claude Code keeps only <pid>.json there.
func TestTheSessionsWatchFiresOnEachKindOfChange(t *testing.T) {
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "sessions")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	h := newWatchHarness(t)
	h.w.want([]string{cfg})
	base := h.quiet() // the add's own refresh

	for _, c := range []struct {
		name string
		do   func()
	}{
		{"create", func() { writeSessJSON(t, dir, "100.json", `{"status":"busy"}`) }},
		{"rewrite", func() { writeSessJSON(t, dir, "100.json", `{"status":"waiting"}`) }},
		{"rename into place", func() {
			writeSessJSON(t, cfg, "tmp", `{"status":"idle"}`)
			if err := os.Rename(filepath.Join(cfg, "tmp"), filepath.Join(dir, "100.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{"remove", func() {
			if err := os.Remove(filepath.Join(dir, "100.json")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		start := time.Now()
		c.do()
		deadline := start.Add(time.Second)
		for h.fired.Load() == base {
			if time.Now().After(deadline) {
				t.Fatalf("%s: no refresh within %v", c.name, time.Since(start))
			}
			time.Sleep(2 * time.Millisecond)
		}
		t.Logf("%s: refresh after %v", c.name, time.Since(start))
		base = h.quiet()
	}

	// The negative case, and its control: a non-json file fires nothing.
	writeSessJSON(t, dir, "notes.txt", "x")
	if n := h.quiet(); n != base {
		t.Fatalf("a non-json file asked for %d refresh(es); want none", n-base)
	}
}

// TestABurstOfWritesCoalesces: a CLI rewriting its file hundreds of times in
// a second asks for at most one refresh per sessionsWatchEvery, never one per
// write — and still at least one, so the last state is read.
func TestABurstOfWritesCoalesces(t *testing.T) {
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "sessions")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	h := newWatchHarness(t)
	h.w.want([]string{cfg})
	base := h.quiet()

	// Writes as fast as they go for a second: many per interval, so a watch
	// that refreshes per write, or per settle, overshoots the limit by far.
	writes := 0
	start := time.Now()
	for time.Since(start) < time.Second {
		writeSessJSON(t, dir, "100.json", fmt.Sprintf(`{"status":"busy","n":%d}`, writes))
		writes++
	}
	took := time.Since(start)
	got := h.quiet() - base
	// One per interval over the burst, plus the one pending at its end.
	limit := int64(took/sessionsWatchEvery) + 2
	if got < 1 || got > limit {
		t.Fatalf("%d writes over %v asked for %d refresh(es); want 1..%d (one per %v)", writes, took, got, limit, sessionsWatchEvery)
	}
	t.Logf("%d writes over %v -> %d refresh(es) (limit %d)", writes, took, got, limit)
}

// TestTheThrottleSpacesASustainedBurst drives the throttle on a simulated
// clock (no sleeping, so host load cannot change what it sees): a change
// every millisecond for two seconds. The first refresh comes at the settle,
// then one every interval WHILE the writes go on — never one per write, and
// never postponed until they stop (a debounce that re-arms on every change
// would fire once, after the burst). Each check below is one of those.
func TestTheThrottleSpacesASustainedBurst(t *testing.T) {
	th := throttle{settle: sessionsWatchSettle, every: sessionsWatchEvery}
	t0 := time.Unix(1_792_000_000, 0)
	var fires []time.Duration
	var deadline time.Time // zero: none armed
	const burst = 2 * time.Second
	for at := time.Duration(0); at <= burst+time.Second; at += time.Millisecond {
		now := t0.Add(at)
		if !deadline.IsZero() && !now.Before(deadline) {
			deadline = time.Time{}
			th.fired(now)
			fires = append(fires, at)
		}
		if at < burst {
			if d, arm := th.poke(now); arm {
				deadline = now.Add(d) // what the watch's timer does: a re-arm replaces it
			}
		}
	}
	if len(fires) == 0 || fires[0] != sessionsWatchSettle {
		t.Fatalf("refreshes at %v; want the first at the settle (%v)", fires, sessionsWatchSettle)
	}
	for i := 1; i < len(fires); i++ {
		if gap := fires[i] - fires[i-1]; gap < sessionsWatchEvery {
			t.Fatalf("refreshes at %v: %v apart, under the interval %v", fires, gap, sessionsWatchEvery)
		}
	}
	during := 0
	for _, f := range fires {
		if f < burst {
			during++
		}
	}
	// 50 ms, then every 250 ms: 50, 300, …, 1800 — 8 inside the burst.
	if during != 8 {
		t.Fatalf("refreshes at %v: %d during the %v burst, want 8 (the first at the settle, then one per %v)", fires, during, burst, sessionsWatchEvery)
	}
	if len(fires) != 9 {
		t.Fatalf("refreshes at %v: %d in all, want 9 (the burst's 8, then one for its last writes)", fires, len(fires))
	}
}

// TestAMissingSessionsDirFallsBackAndIsWatchedOnceItExists: a config dir
// whose sessions directory does not exist yet is no error — the watch logs
// once, keeps running, and adds the directory on a later want once it
// exists; a directory removed and recreated is watched again the same way.
func TestAMissingSessionsDirFallsBackAndIsWatchedOnceItExists(t *testing.T) {
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "sessions")
	h := newWatchHarness(t)
	h.w.want([]string{cfg})
	h.w.want([]string{cfg}) // the next refresh's: still missing, not logged again
	base := h.quiet()
	if logs := h.logged(); len(logs) != 1 || !strings.Contains(logs[0], "not watching "+dir) {
		t.Fatalf("logged %q, want one line naming %s", logs, dir)
	}

	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	h.w.want([]string{cfg})
	base = h.quiet()
	writeSessJSON(t, dir, "100.json", `{}`)
	waitUntil(t, "a refresh once the directory exists", func() bool { return h.fired.Load() > base })

	// Removed and recreated: the removal itself asks for a refresh, and the
	// next want watches the new directory.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	h.quiet()
	h.w.want([]string{cfg})
	base = h.quiet()
	writeSessJSON(t, dir, "200.json", `{}`)
	waitUntil(t, "a refresh in the recreated directory", func() bool { return h.fired.Load() > base })
	if logs := h.logged(); len(logs) != 1 {
		t.Fatalf("logged %q, want only the first missing-directory line", logs)
	}
}

// TestSessionsWatchWant: config dirs become their sessions directories,
// deduplicated, and a relative or empty one is dropped.
func TestSessionsWatchWant(t *testing.T) {
	w := newSessionsWatch(func() {}, func(string, ...any) {}, nil)
	w.want([]string{"/a/cfg", "", "rel/cfg", "/a/cfg/", "/b"})
	w.want([]string{"/c"}) // replaces the set not taken yet
	got := <-w.dirs
	if len(got) != 1 || got[0] != "/c/sessions" {
		t.Fatalf("got %q, want only the latest set", got)
	}
	w.want([]string{"/a/cfg", "", "rel/cfg", "/a/cfg/", "/b"})
	got = <-w.dirs
	if want := []string{"/a/cfg/sessions", "/b/sessions"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestClaudeConfigDirsReadsThePanes: the default and every Claude pane's own.
func TestClaudeConfigDirsReadsThePanes(t *testing.T) {
	raw := []WindowInfo{{Claude: &ClaudePane{ConfigDir: "/x"}}, {}, {Claude: &ClaudePane{}}, {Claude: &ClaudePane{ConfigDir: "/y"}}}
	if got := fmt.Sprint(claudeConfigDirs("/d", raw)); got != "[/d /x /y]" {
		t.Fatalf("got %s", got)
	}
	var _ ClaudeConfigDirer = Tmux{}
	if d := (Tmux{Claude: ClaudeLocator{Home: "/h"}}).ClaudeConfigDir(); d != "/h" {
		t.Fatalf("Tmux.ClaudeConfigDir = %q", d)
	}
}
