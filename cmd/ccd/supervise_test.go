package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// fakePane is a cliPane whose process exits on the first poll after each start,
// with a scripted status — a CLI that dies at once, every time.
type fakePane struct {
	mu       sync.Mutex
	starts   [][]string // argv of Start, then of each Respawn
	running  bool
	status   int
	onStart  func(n int) // after the n-th start (1-based)
	failResp bool
}

func (p *fakePane) Start(_ context.Context, _ string, argv []string) (string, error) {
	p.mu.Lock()
	p.starts = append(p.starts, argv)
	p.running = true
	n := len(p.starts)
	p.mu.Unlock()
	if p.onStart != nil {
		p.onStart(n)
	}
	return "%0", nil
}

func (p *fakePane) Respawn(ctx context.Context, pane, dir string, argv []string) error {
	if p.failResp {
		return os.ErrPermission
	}
	_, err := p.Start(ctx, dir, argv)
	return err
}

func (p *fakePane) Exited(context.Context, string) (bool, int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		p.running = false
		return true, p.status, nil
	}
	return true, p.status, nil
}

// testSupervisor uses a fake clock: `after` fires at once and records every wait
// that is not a poll, and each wait advances the clock by its duration plus
// `extra` (time the CLI spent running).
func testSupervisor(t *testing.T, pane cliPane, cfg, ws string, extra time.Duration) (*supervisor, *[]time.Duration) {
	t.Helper()
	s := newSupervisor(pane, "claude", cfg, ws)
	var mu sync.Mutex
	clock := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	var delays []time.Duration
	s.poll = time.Nanosecond
	s.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	s.after = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		defer mu.Unlock()
		if d != s.poll {
			delays = append(delays, d)
		}
		clock = clock.Add(d + extra)
		c := make(chan time.Time, 1)
		c <- clock
		return c
	}
	return s, &delays
}

func writeTranscript(t *testing.T, cfg, projDir string) {
	t.Helper()
	d := filepath.Join(cfg, "projects", projDir)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "11111111-2222-4333-8444-555555555555.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// 🔴 FRESH vs CONTINUE IS DECIDED BY LOOKING, AND ONLY AT THIS WORKSPACE'S
// PROJECT DIRECTORY. The encoded names are written out literally (not derived
// from nonAlnum) so a wrong encoding cannot agree with itself.
func TestTheFirstStartContinuesOnlyWhenThisWorkspaceHasATranscript(t *testing.T) {
	cases := []struct {
		name string
		prep func(t *testing.T, cfg string)
		want []string
	}{
		{"fresh volume", func(*testing.T, string) {}, []string{"claude"}},
		{"transcript for this workspace", func(t *testing.T, cfg string) { writeTranscript(t, cfg, "-data-my-ws-v2") }, []string{"claude", "--continue"}},
		{"transcript for ANOTHER workspace only", func(t *testing.T, cfg string) { writeTranscript(t, cfg, "-data-other") }, []string{"claude"}},
		{"project dir with no transcript", func(t *testing.T, cfg string) {
			if err := os.MkdirAll(filepath.Join(cfg, "projects", "-data-my-ws-v2"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, []string{"claude"}},
		{"slash-only encoding would miss it", func(t *testing.T, cfg string) { writeTranscript(t, cfg, "-data-my_ws.v2") }, []string{"claude"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := t.TempDir()
			c.prep(t, cfg)
			s := newSupervisor(nil, "claude", cfg, "/data/my_ws.v2")
			if got, _ := s.argv(); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("argv %q, want %q", got, c.want)
			}
		})
	}
}

// /exit (or any exit) is followed by a restart IN THE PANE that resumes the
// conversation the first run created — never a second, fresh one.
func TestARestartResumesTheConversationTheFirstRunCreated(t *testing.T) {
	cfg := t.TempDir()
	pane := &fakePane{}
	s, _ := testSupervisor(t, pane, cfg, "/data/workspace", time.Hour) // runs far apart: no crash loop
	ctx, cancel := context.WithCancel(context.Background())
	// The first run (fresh) writes a transcript, as the CLI does on its first prompt.
	pane.onStart = func(n int) {
		if n == 1 {
			writeTranscript(t, cfg, "-data-workspace")
		}
		if n == 3 {
			cancel()
		}
	}
	if err := s.run(ctx); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"claude"}, {"claude", "--continue"}, {"claude", "--continue"}}
	if !reflect.DeepEqual(pane.starts, want) {
		t.Fatalf("starts %q, want %q", pane.starts, want)
	}
	if st := s.state(); st.CLI != cliRunning || st.Mode != modeContinue || st.Starts != 3 {
		t.Fatalf("state %+v", st)
	}
}

// 🔴 THE BACKOFF AND THE CRASH-LOOP THRESHOLD. A CLI that dies at once: waits of
// 1s, 2s, 4s, 8s between starts, then on the 5th exit within 2 minutes the
// supervisor stops and reports crash_loop.
func TestFiveExitsWithinTheWindowIsACrashLoopAfterExponentialBackoff(t *testing.T) {
	pane := &fakePane{status: 7}
	s, delays := testSupervisor(t, pane, t.TempDir(), "/data/workspace", 0)
	// A supervisor that never declares the crash loop would restart for ever;
	// stop it well past the threshold so the assertions below report it.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pane.onStart = func(n int) {
		if n == 20 {
			cancel()
		}
	}
	if err := s.run(ctx); err != nil {
		t.Fatal(err)
	}
	st := s.state()
	if st.CLI != cliCrashLoop || st.Starts != 5 || len(pane.starts) != 5 || st.LastExit == nil || *st.LastExit != 7 {
		t.Fatalf("state %+v, %d starts", st, len(pane.starts))
	}
	if want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}; !reflect.DeepEqual(*delays, want) {
		t.Fatalf("backoff %v, want %v", *delays, want)
	}
}

// The window is real: exits further apart than it never accumulate to a crash
// loop, and each restart waits only the minimum.
func TestExitsSpreadBeyondTheWindowNeverCrashLoop(t *testing.T) {
	pane := &fakePane{}
	s, delays := testSupervisor(t, pane, t.TempDir(), "/data/workspace", 3*time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	pane.onStart = func(n int) {
		if n == 12 {
			cancel()
		}
	}
	if err := s.run(ctx); err != nil {
		t.Fatal(err)
	}
	if st := s.state(); st.CLI == cliCrashLoop || st.Starts != 12 {
		t.Fatalf("state %+v", st)
	}
	for _, d := range *delays {
		if d != time.Second {
			t.Fatalf("backoff %v grew although every exit was alone in its window", *delays)
		}
	}
}

// The backoff is capped.
func TestBackoffIsCapped(t *testing.T) {
	s := newSupervisor(nil, "claude", t.TempDir(), "/w")
	s.crashExits = 100
	clock := time.Unix(0, 0)
	s.now = func() time.Time { return clock }
	var last time.Duration
	for i := 0; i < 10; i++ {
		last, _ = s.recordExit(1)
	}
	if last != s.backoffMax {
		t.Fatalf("10th backoff %v, want the cap %v", last, s.backoffMax)
	}
}

// A respawn that keeps failing leaves the pane dead, so it too ends in crash_loop.
func TestARespawnThatKeepsFailingEndsInCrashLoop(t *testing.T) {
	pane := &fakePane{failResp: true}
	s, _ := testSupervisor(t, pane, t.TempDir(), "/data/workspace", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.run(ctx); err != nil {
		t.Fatal(err)
	}
	if st := s.state(); st.CLI != cliCrashLoop || st.Starts != 1 {
		t.Fatalf("state %+v", st)
	}
}
