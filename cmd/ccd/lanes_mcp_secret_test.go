package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
)

// chatLaneRig is a lane rig whose .claude.json registers muster's server,
// recording each lane's started argv.
type chatLaneRig struct {
	*laneRig
	mu    sync.Mutex
	panes map[string]*argvPane
}

func newChatLaneRig(t *testing.T, max int) *chatLaneRig {
	t.Helper()
	r := &chatLaneRig{laneRig: newLaneRig(t, max), panes: map[string]*argvPane{}}
	b, _ := json.Marshal(map[string]any{"hasCompletedOnboarding": true, "mcpServers": map[string]any{
		"muster": map[string]any{"type": "http", "url": "http://muster.example/mcp", "headers": map[string]any{"X-Muster-Token": "fixture-agent-token-3c1d"}}}})
	if err := os.WriteFile(filepath.Join(r.l.configDir, ".claude.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	r.l.newTerm = func(target string) laneTerminal {
		p := &argvPane{}
		r.mu.Lock()
		r.panes[target] = p
		r.mu.Unlock()
		return p
	}
	return r
}

// turn sends one chat-lane turn for key with secret; it answers the status
// and, once the lane's CLI started, its argv (nil when none started).
func (r *chatLaneRig) turn(t *testing.T, key, secret string) (int, []string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"input":"hello","stream":false}`))
	req.Header.Set("Authorization", "Bearer b")
	req.Header.Set(headerCCSession, key)
	req.Header.Set(headerCCMCPSession, "1")
	if secret != "" {
		req.Header.Set("X-Muster-CC-Lane-Secret", secret)
	}
	w := httptest.NewRecorder()
	r.s.gatewayHandler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		return w.Code, nil
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		r.mu.Lock()
		p := r.panes["cc-"+laneID(key)]
		r.mu.Unlock()
		if p != nil && p.started() != nil {
			return w.Code, p.started()
		}
		if time.Now().After(deadline) {
			t.Fatalf("precondition: %s's lane never started its CLI", key)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestAChatLanesConfigCarriesItsSecret (Y4a): the lane's MCP config sends
// muster the lane's secret beside its session key; a turn naming the lane
// with ANOTHER secret is refused (the secret is fixed for the lane's life),
// and a chat-lane turn with no secret starts nothing.
func TestAChatLanesConfigCarriesItsSecret(t *testing.T) {
	r := newChatLaneRig(t, 3)
	const secret = "fixture-lane-secret-0a1b2c3d4e5f60718293a4b5c6d7e8f9"
	code, argv := r.turn(t, "officer-chat:c-41", secret)
	i := slices.Index(argv, "--mcp-config")
	if code != http.StatusServiceUnavailable || i < 0 {
		t.Fatalf("the chat lane's turn: %d, argv %q; want its CLI started with --mcp-config", code, argv)
	}
	raw, err := os.ReadFile(argv[i+1])
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if h := cfg.MCPServers["muster"].Headers; h["X-Muster-CC-Lane-Secret"] != secret || h[headerCCSession] != "officer-chat:c-41" {
		t.Fatalf("the lane's muster headers %v; want its session key and its secret", h)
	}
	if code, _ := r.turn(t, "officer-chat:c-41", "another-secret-0123456789abcdef0123456789"); code != http.StatusBadRequest {
		t.Fatalf("a turn naming the running lane with another secret answered %d; want 400 (the secret is fixed)", code)
	}
	if code, argv := r.turn(t, "officer-chat:c-42", ""); code != http.StatusBadRequest || argv != nil {
		t.Fatalf("a chat-lane turn with no secret answered %d and started %q; want 400, nothing started", code, argv)
	}
}

// TestAStoppedChatLanesConfigIsRemoved (🟢): the lane's MCP config — which
// holds the agent's token and the lane's secret — is removed when the lane is
// stopped (here, evicted for another session at the cap).
func TestAStoppedChatLanesConfigIsRemoved(t *testing.T) {
	r := newChatLaneRig(t, 1)
	_, argv := r.turn(t, "officer-chat:c-41", "fixture-lane-secret-0a1b2c3d4e5f60718293a4b5c6d7e8f9")
	path := argv[slices.Index(argv, "--mcp-config")+1]
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("precondition: the lane's config: %v", err)
	}
	r.turn(t, "officer-chat:c-42", "fixture-lane-secret-1a1b2c3d4e5f60718293a4b5c6d7e8f9") // evicts c-41's lane
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the evicted lane's MCP config %s is still on disk", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestHealthzSaysItNamesChatSessions (Y4b): a ccd that runs lanes says
// mcp_session_lanes on /healthz, and muster's reader of that body sees it.
func TestHealthzSaysItNamesChatSessions(t *testing.T) {
	r := newLaneRig(t, 3)
	w := httptest.NewRecorder()
	r.s.handleHealthz(w, httptest.NewRequest("GET", "/healthz", nil))
	ok, err := agentgateway.ParseHealthzMCPSessionLanes(w.Body.Bytes())
	if err != nil || !ok {
		t.Fatalf("/healthz %s: mcp_session_lanes %v (%v); want true", w.Body.String(), ok, err)
	}
	if ok, err := agentgateway.ParseHealthzMCPSessionLanes([]byte(`{"ok":true,"toolless_lanes":true}`)); err != nil || ok {
		t.Fatalf("an older ccd's body read as mcp_session_lanes %v (%v); want false", ok, err)
	}
}
