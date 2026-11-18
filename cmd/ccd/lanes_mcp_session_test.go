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
)

// 🔴 A CHAT LANE SAYS WHO IT IS (lanes.go). A turn carrying
// X-Muster-CC-MCP-Session: 1 (muster sets it on an officer's chat lane only)
// starts its CLI with an MCP config of its OWN — the user-scope servers `ccd
// seed` registered, muster's carrying X-Muster-CC-Session: <the lane's key> —
// and --strict-mcp-config, so every MCP call the lane makes names its session.
// A turn without it (the chief's lanes, a job's) starts exactly as before.
func TestAChatLaneStartsWithAnMCPConfigNamingItsSession(t *testing.T) {
	r := newLaneRig(t, 3)
	claude := map[string]any{"hasCompletedOnboarding": true, "mcpServers": map[string]any{
		"muster": map[string]any{"type": "http", "url": "http://muster.example/mcp", "headers": map[string]any{"X-Muster-Token": "fixture-agent-token-3c1d"}},
		"other":  map[string]any{"type": "http", "url": "http://other.example/mcp"},
	}}
	b, _ := json.Marshal(claude)
	if err := os.WriteFile(filepath.Join(r.l.configDir, ".claude.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	panes := map[string]*argvPane{}
	var mu sync.Mutex
	r.l.newTerm = func(target string) laneTerminal {
		p := &argvPane{}
		mu.Lock()
		panes[target] = p
		mu.Unlock()
		return p
	}
	turn := func(key, tag string) []string {
		t.Helper()
		req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"input":"hello","stream":false}`))
		req.Header.Set("Authorization", "Bearer b")
		req.Header.Set(headerCCSession, key)
		if tag != "" {
			req.Header.Set("X-Muster-CC-MCP-Session", tag)
			req.Header.Set("X-Muster-CC-Lane-Secret", "fixture-lane-secret-0a1b2c3d4e5f60718293a4b5c6d7e8f9")
		}
		w := httptest.NewRecorder()
		r.s.gatewayHandler().ServeHTTP(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("precondition: %s's turn answered %d %s, want 503 not_ready", key, w.Code, w.Body.String())
		}
		mu.Lock()
		p := panes["cc-"+laneID(key)]
		mu.Unlock()
		deadline := time.Now().Add(10 * time.Second)
		for p == nil || p.started() == nil {
			if time.Now().After(deadline) {
				t.Fatalf("precondition: %s's lane never started its CLI", key)
			}
			time.Sleep(time.Millisecond)
			mu.Lock()
			p = panes["cc-"+laneID(key)]
			mu.Unlock()
		}
		return p.started()
	}
	argv := turn("officer-chat:c-41", "1")
	i := slices.Index(argv, "--mcp-config")
	if i < 0 || i+1 >= len(argv) || !slices.Contains(argv, "--strict-mcp-config") {
		t.Fatalf("a chat lane started %q; want --mcp-config <file> and --strict-mcp-config", argv)
	}
	raw, err := os.ReadFile(argv[i+1])
	if err != nil {
		t.Fatalf("its MCP config: %v", err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	m := cfg.MCPServers["muster"]
	if m.URL != "http://muster.example/mcp" || m.Headers["X-Muster-Token"] != "fixture-agent-token-3c1d" || m.Headers[headerCCSession] != "officer-chat:c-41" {
		t.Fatalf("the lane's muster server %+v; want its url and token, and %s: officer-chat:c-41", m, headerCCSession)
	}
	if _, kept := cfg.MCPServers["other"]; !kept || len(cfg.MCPServers) != 2 {
		t.Fatalf("the lane's servers %v; want every user-scope server kept", cfg.MCPServers)
	}
	for _, c := range []struct{ key, v string }{{"agent:chief:webchat:1", ""}, {"job-7", "0"}} {
		if argv := turn(c.key, c.v); slices.Contains(argv, "--mcp-config") || slices.Contains(argv, "--strict-mcp-config") {
			t.Errorf("X-Muster-CC-MCP-Session %q started %q; only \"1\" may change a lane's MCP servers", c.v, argv)
		}
	}
}
