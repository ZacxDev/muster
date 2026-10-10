package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The supervisor keeps the interactive CLI running in its tmux pane, IN THE POD:
// an exit (an operator's /exit, a crash) is followed by a restart in the same
// pane, resuming the same conversation, with no pod restart.
//
// 🔴 RESUME, NEVER FORK. Every start is `claude --continue` when the config dir
// already holds a transcript for this workspace, and plain `claude` only when it
// holds none (a genuinely fresh volume). The decision is made by LOOKING at the
// transcripts (hasPriorSession), never by running `--continue` and reading its exit
// code: measured on the pinned CLI, `claude --continue` with nothing to continue
// prints "No conversation found to continue" and exits 1 — but so does any other
// failure, and `--continue || claude` turns every one of them into a NEW
// conversation that silently abandons the old one.
//
// 🔴 A CRASH LOOP IS A POD PROBLEM, AND IS HANDED TO KUBERNETES. Each restart
// waits backoffMin·2^(k-1) (capped at backoffMax), k being the number of exits
// within crashWindow. When crashExits exits fall within crashWindow, the
// supervisor stops restarting and reports `cli: crash_loop`; /healthz then
// answers non-200, so the liveness probe restarts the pod and the restart count
// is visible in `kubectl get pod`. It latches: without a liveness probe (a bare
// `docker run`) nothing restarts it.
type cliPane interface {
	// Start creates the session running argv in dir, with the pane kept (dead)
	// after its process exits, and returns the pane's id.
	Start(ctx context.Context, dir string, argv []string) (pane string, err error)
	// Respawn runs argv in the (dead) pane again.
	Respawn(ctx context.Context, pane, dir string, argv []string) error
	// Exited reports whether the pane's process has exited, and its status.
	Exited(ctx context.Context, pane string) (exited bool, status int, err error)
}

const (
	cliStarting   = "starting"
	cliRunning    = "running"
	cliRestarting = "restarting"
	cliCrashLoop  = "crash_loop"

	modeContinue = "continue"
	modeFresh    = "fresh"
)

// cliState is what /healthz reports about the supervised CLI.
type cliState struct {
	CLI      string `json:"cli"`
	Mode     string `json:"cli_mode,omitempty"` // the last start: "continue" or "fresh"
	Starts   int    `json:"cli_starts"`
	LastExit *int   `json:"cli_last_exit,omitempty"`
	Detail   string `json:"cli_detail,omitempty"`
}

type supervisor struct {
	pane      cliPane
	bin       string
	configDir string
	workspace string

	poll                   time.Duration
	backoffMin, backoffMax time.Duration
	crashExits             int
	crashWindow            time.Duration
	now                    func() time.Time
	after                  func(time.Duration) <-chan time.Time

	mu    sync.Mutex
	st    cliState
	exits []time.Time // within crashWindow of the latest
}

func newSupervisor(pane cliPane, bin, configDir, workspace string) *supervisor {
	return &supervisor{
		pane: pane, bin: bin, configDir: configDir, workspace: workspace,
		poll: time.Second, backoffMin: time.Second, backoffMax: 30 * time.Second,
		crashExits: 5, crashWindow: 2 * time.Minute,
		now: time.Now, after: time.After,
		st: cliState{CLI: cliStarting},
	}
}

func (s *supervisor) state() cliState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	if st.LastExit != nil {
		v := *st.LastExit
		st.LastExit = &v
	}
	return st
}

// nonAlnum is the CLI's own encoding of a working directory into its project
// directory name: every byte outside [A-Za-z0-9] becomes '-'. Measured on the
// pinned CLI (/data/workspace → projects/-data-workspace) and on a workstation
// CLI, where a cwd ending in /.claude became a name ending in --claude ('.' is
// replaced too, not only '/').
var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// hasPriorSession reports whether the config dir holds a transcript for the
// workspace — i.e. whether `claude --continue` has a conversation to resume.
// Measured on the pinned CLI: no transcript exists after startup alone; the first
// prompt writes one, and so does a typed /exit (and `--continue` then resumes
// that session). So a session that was started and never touched is, correctly,
// still fresh.
func hasPriorSession(configDir, workspace string) bool {
	dir := filepath.Join(configDir, "projects", nonAlnum.ReplaceAllString(workspace, "-"))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".jsonl") {
			return true
		}
	}
	return false
}

func (s *supervisor) argv() ([]string, string) {
	if hasPriorSession(s.configDir, s.workspace) {
		return []string{s.bin, "--continue"}, modeContinue
	}
	return []string{s.bin}, modeFresh
}

func (s *supervisor) started(mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.CLI, s.st.Mode, s.st.Detail = cliRunning, mode, ""
	s.st.Starts++
}

func (s *supervisor) setDetail(d string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Detail = d
}

// recordExit counts one exit and decides what follows it: the delay before the
// next start, or (crash = true) no next start at all.
func (s *supervisor) recordExit(status int) (delay time.Duration, crash bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	kept := s.exits[:0]
	for _, t := range s.exits {
		if now.Sub(t) < s.crashWindow {
			kept = append(kept, t)
		}
	}
	s.exits = append(kept, now)
	s.st.LastExit = &status
	k := len(s.exits)
	if k >= s.crashExits {
		s.st.CLI = cliCrashLoop
		s.st.Detail = fmt.Sprintf("the CLI exited %d times within %s (last status %d); not restarting it — "+
			"/healthz now fails so Kubernetes restarts the pod", k, s.crashWindow, status)
		return 0, true
	}
	delay = s.backoffMin
	for i := 1; i < k && delay < s.backoffMax; i++ {
		delay *= 2
	}
	delay = min(delay, s.backoffMax)
	s.st.CLI = cliRestarting
	return delay, false
}

// run starts the CLI and keeps it running until ctx ends or it crash-loops.
func (s *supervisor) run(ctx context.Context) error {
	argv, mode := s.argv()
	pane, err := s.pane.Start(ctx, s.workspace, argv)
	if err != nil {
		return err
	}
	s.started(mode)
	log.Printf("ccd: started %q in pane %s (%s)", strings.Join(argv, " "), pane, mode)
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return nil
		case <-s.after(s.poll):
		}
		exited, status, err := s.pane.Exited(ctx, pane)
		if err != nil {
			// The session itself is gone or tmux is not answering: /healthz's own
			// terminal check reports that. Keep looking.
			s.setDetail(err.Error())
			continue
		}
		if !exited {
			continue
		}
		delay, crash := s.recordExit(status)
		if crash {
			log.Printf("ccd: %s", s.state().Detail)
			return nil
		}
		log.Printf("ccd: the CLI exited with status %d; restarting in %s", status, delay)
		select {
		case <-ctx.Done():
			return nil
		case <-s.after(delay):
		}
		if ctx.Err() != nil {
			return nil
		}
		argv, mode = s.argv()
		if err := s.pane.Respawn(ctx, pane, s.workspace, argv); err != nil {
			// The pane stays dead, so the next poll counts this as another exit:
			// a respawn that keeps failing ends in crash_loop like a CLI that does.
			s.setDetail("respawn: " + err.Error())
			continue
		}
		s.started(mode)
		log.Printf("ccd: restarted %q (%s)", strings.Join(argv, " "), mode)
	}
	return nil
}
