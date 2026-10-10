package main

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// scriptedCLI is a terminal that behaves like the CLI from ccd's point of view:
// a paste produces a UserPromptSubmit hook, a transcript, and a Stop or
// StopFailure hook, in that order — with the hook payloads shaped like the
// recorded ones in testdata/hook_*.json.
type scriptedCLI struct {
	t      *testing.T
	srv    *server
	dir    string // CLAUDE_CONFIG_DIR
	script cliScript

	mu       sync.Mutex
	pasted   []string
	panes    []string // the pane each Paste and Enter addressed
	enters   int
	aliveErr error
}

// cliScript decides what the "CLI" does with one prompt.
type cliScript struct {
	transcript []byte // written before the stop hook; promptId must be fixturePromptID
	stopEvent  string // "Stop" or "StopFailure"; "" = never stops
	stopError  string // StopFailure's `error`
	lastMsg    string // StopFailure's `last_assistant_message`
	noSubmit   bool   // the paste never becomes a prompt (a dialog has focus)
	// transcriptPath overrides where the hooks say the transcript is.
	transcriptPath string
	// reported, when set, maps the pasted text to the `prompt` UserPromptSubmit
	// carries (the real CLI's whitespace changes); nil reports it verbatim.
	reported func(string) string
}

// scriptedPane is the pane id newScripted's server is told the CLI runs in.
const scriptedPane = "%7"

const fixtureSessionID = "11111111-2222-4333-8444-555555555555"

func (c *scriptedCLI) transcriptFile() string {
	return filepath.Join(c.dir, "projects", "-data-workspace", fixtureSessionID+".jsonl")
}

func (c *scriptedCLI) Paste(ctx context.Context, pane, text string) error {
	c.mu.Lock()
	c.pasted = append(c.pasted, text)
	c.panes = append(c.panes, pane)
	c.mu.Unlock()
	if c.script.noSubmit {
		return nil
	}
	path := c.transcriptFile()
	if c.script.transcriptPath != "" {
		path = c.script.transcriptPath
	}
	reported := text
	if c.script.reported != nil {
		reported = c.script.reported(text)
	}
	go func() {
		c.srv.onHook(hookEvent{Event: "UserPromptSubmit", SessionID: fixtureSessionID,
			TranscriptPath: path, PromptID: fixturePromptID, Prompt: reported})
		if err := os.MkdirAll(filepath.Dir(c.transcriptFile()), 0o700); err != nil {
			c.t.Error(err)
			return
		}
		if err := os.WriteFile(c.transcriptFile(), c.script.transcript, 0o600); err != nil {
			c.t.Error(err)
			return
		}
		if c.script.stopEvent == "" {
			return
		}
		c.srv.onHook(hookEvent{Event: c.script.stopEvent, SessionID: fixtureSessionID, TranscriptPath: path,
			PromptID: fixturePromptID, Error: c.script.stopError, LastAssistantMessage: c.script.lastMsg})
	}()
	return nil
}

func (c *scriptedCLI) Enter(_ context.Context, pane string) error {
	c.mu.Lock()
	c.enters++
	c.panes = append(c.panes, pane)
	c.mu.Unlock()
	return nil
}

func (c *scriptedCLI) Alive(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.aliveErr
}

// newScripted builds a server around a scripted CLI whose session has STARTED,
// with the bearer derived from knownHooksToken.
func newScripted(t *testing.T, script cliScript) (*server, *scriptedCLI) {
	t.Helper()
	dir := t.TempDir()
	bearer, err := resolveBearer(knownHooksToken, "")
	if err != nil {
		t.Fatal(err)
	}
	cli := &scriptedCLI{t: t, dir: dir, script: script}
	srv := newServer(serverConfig{Bearer: bearer, ConfigDir: dir, SubmitTimeout: 2 * time.Second,
		TurnTimeout: 5 * time.Second, TranscriptGrace: 200 * time.Millisecond}, cli, newAuthTracker())
	cli.srv = srv
	srv.inputPane = func() (string, bool) { return scriptedPane, true }
	srv.onHook(hookEvent{Event: "SessionStart", SessionID: fixtureSessionID, Source: "startup"})
	return srv, cli
}

func gatewayServer(t *testing.T, srv *server) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(srv.gatewayHandler())
	t.Cleanup(ts.Close)
	return ts
}
