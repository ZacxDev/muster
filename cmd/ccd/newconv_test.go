package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// A NEW CONVERSATION BEFORE A TURN (headerCCNewConversation,
// startNewConversation): what ccd reports about it, and which SessionStart it
// takes as the CLI's /clear.

// TestHealthzSaysItStartsNewConversations: the rollout check an operator does
// by hand after moving a fresh_session_per_item officer onto a new image —
// /healthz says `"new_conversation": true`, read here as raw JSON from ccd's
// own handler, the way an operator's curl sees it. A ccd that predates the
// header has no such field.
func TestHealthzSaysItStartsNewConversations(t *testing.T) {
	srv, _ := newScripted(t, cliScript{})
	_, body, _ := getHealth(t, gatewayServer(t, srv).URL, "/healthz")
	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("/healthz body: %v", err)
	}
	if raw["new_conversation"] != true {
		t.Fatalf(`/healthz does not report "new_conversation": true (got %v) — an operator's rollout check would read this ccd as one that ignores the header; body %s`,
			raw["new_conversation"], body)
	}
}

// clearCLI is a terminal that records what ccd types and, when ccd pastes
// /clear, runs onClear — the hooks the CLI (or something else in its pane)
// sends while ccd waits.
type clearCLI struct {
	mu      sync.Mutex
	typed   []string
	onClear func()
}

func (c *clearCLI) Paste(_ context.Context, _ paneRef, text string) error {
	c.mu.Lock()
	c.typed = append(c.typed, "paste:"+text)
	c.mu.Unlock()
	if text == clearCommand && c.onClear != nil {
		c.onClear()
	}
	return nil
}

func (c *clearCLI) Enter(context.Context, paneRef) error {
	c.mu.Lock()
	c.typed = append(c.typed, "enter")
	c.mu.Unlock()
	return nil
}

func (c *clearCLI) Alive(context.Context, paneRef) error { return nil }

// 🔴 ONLY THE PRIMARY'S OWN /clear STARTS THE NEW CONVERSATION ccd WAITS FOR: a
// SessionStart whose source is "clear" AND that the primary CLI's own process
// fired (notFiredByPrimary, the check the primary's recording uses). The /proc
// fixture has the primary CLI (500), its hook (600), a `claude` nested inside it
// (650) and the nested one's hook (700). While ccd waits after typing /clear:
//
//   - the nested CLI's SessionStart, source "clear", a new id: NOT the clear;
//   - the primary's own SessionStart with another source ("startup"): NOT the
//     clear;
//   - positive control: the primary's own SessionStart, source "clear", a new
//     id: the clear.
//
// Taking either of the first two would type the turn into the OLD
// conversation, the one the primary is still in.
func TestTheNewConversationWaitTakesOnlyThePrimarysOwnClear(t *testing.T) {
	old := procRoot
	procRoot = fakeProc(t, map[int]proc{
		500: {1, "/opt/cli/claude"},
		600: {500, "/opt/cli/ccd"},
		650: {500, "/opt/cli/claude"},
		700: {650, "/opt/cli/ccd"},
	})
	t.Cleanup(func() { procRoot = old })

	for _, tc := range []struct {
		name    string
		hookPID int
		source  string
		want    bool
	}{
		{"a nested CLI's clear", 700, "clear", false},
		{"the primary's start of another source", 600, "startup", false},
		{"the primary's own clear (positive control)", 600, "clear", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bearer, err := resolveBearer(knownHooksToken, "")
			if err != nil {
				t.Fatal(err)
			}
			cli := &clearCLI{}
			srv := newServer(serverConfig{Bearer: bearer, ConfigDir: dir, SubmitTimeout: 400 * time.Millisecond,
				TurnTimeout: 2 * time.Second, TranscriptGrace: 100 * time.Millisecond}, cli, newAuthTracker())
			srv.inputPane = func() (paneRef, bool) { return scriptedPane, true }
			srv.recordsPrimary = true
			srv.primaryPID = func(context.Context) (int, error) { return 500, nil }
			oldPath := filepath.Join(dir, "old.jsonl")
			if err := os.WriteFile(oldPath, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			srv.onHook(hookEvent{Event: "SessionStart", SessionID: "old-session", TranscriptPath: oldPath, Source: "startup"})
			cli.onClear = func() {
				body, _ := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "session_id": "new-session",
					"transcript_path": filepath.Join(dir, "new.jsonl"), "source": tc.source})
				req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/hook/SessionStart?pid=%d", tc.hookPID), bytes.NewReader(body))
				srv.handleHook(httptest.NewRecorder(), req)
			}
			f := srv.startNewConversation(context.Background(), scriptedPane)
			if got := f == nil; got != tc.want {
				t.Fatalf("hook pid %d source %q: new conversation confirmed = %v (failure %v), want %v", tc.hookPID, tc.source, got, f, tc.want)
			}
			if cli.typed[0] != "paste:"+clearCommand {
				t.Fatalf("ccd typed %q, want /clear first", cli.typed)
			}
		})
	}
}
