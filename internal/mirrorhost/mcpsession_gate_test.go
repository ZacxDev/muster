package mirrorhost

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestTheRecoveringClaudeCodeBoundaryIs2_1_289 pins the version boundary of
// the 400 for a missing session id: Claude Code 2.1.289 — the version whose
// bundled client was read and takes the 400's words as an expired session —
// and later are refused; anything earlier, anything that is not exactly
// Claude Code's MCP User-Agent, and no User-Agent at all are served the
// legacy way.
func TestTheRecoveringClaudeCodeBoundaryIs2_1_289(t *testing.T) {
	for ua, want := range map[string]bool{
		"claude-code/2.1.289":                       true,
		"claude-code/2.1.289 (cli)":                 true,
		"claude-code/2.1.289 (sdk-ts, agent-sdk/1)": true,
		"claude-code/2.1.290":                       true,
		"claude-code/2.1.1000":                      true,
		"claude-code/2.2.0":                         true,
		"claude-code/3.0.0":                         true,
		"claude-code/2.1.288":                       false,
		"claude-code/2.1.288 (cli)":                 false,
		"claude-code/2.0.999":                       false,
		"claude-code/1.99.999":                      false,
		"claude-code/2.1.289-beta":                  false,
		"claude-code/2.1":                           false,
		"Claude-Code/2.1.289":                       false,
		"xclaude-code/2.1.289":                      false,
		"node":                                      false,
		"":                                          false,
	} {
		if got := recoversFromMissingSession(ua); got != want {
			t.Errorf("%q: %v, want %v", ua, got, want)
		}
	}
}

// TestTheSessionTableForgetsTheLeastRecentlySeen: at maxMCPSessions, a new
// session evicts exactly the least recently seen one — which then answers
// 404, as after a restart — and keeps every other, the newest included.
func TestTheSessionTableForgetsTheLeastRecentlySeen(t *testing.T) {
	var tb sessionTable
	ids := make([]string, maxMCPSessions)
	for i := range ids {
		ids[i] = tb.create()
	}
	// Seen times set apart, so "least recently seen" is not a tie: the
	// oldest is in the middle, neither first nor last created.
	base := time.Now().Add(-time.Hour)
	oldest := ids[maxMCPSessions/2]
	tb.mu.Lock()
	for i, id := range ids {
		tb.m[id].seen = base.Add(time.Duration(i+1) * time.Second)
	}
	tb.m[oldest].seen = base
	tb.mu.Unlock()

	newest := tb.create()
	tb.mu.Lock()
	n := len(tb.m)
	tb.mu.Unlock()
	if n != maxMCPSessions {
		t.Fatalf("%d sessions after one past the cap; want %d", n, maxMCPSessions)
	}
	if tb.touch(oldest) {
		t.Fatal("the least recently seen session was kept")
	}
	for _, id := range append([]string{newest, ids[0], ids[len(ids)-1]}, ids[maxMCPSessions/2+1]) {
		if !tb.touch(id) {
			t.Fatalf("session %s was evicted, not the least recently seen", id)
		}
	}
}

// TestAnOpenStreamKeepsItsSessionSeen: a session that only holds its GET
// stream open is still active — each keep-alive refreshes it — so it is not
// the one evicted.
func TestAnOpenStreamKeepsItsSessionSeen(t *testing.T) {
	prev := listenKeepAlive
	listenKeepAlive = 20 * time.Millisecond
	t.Cleanup(func() { listenKeepAlive = prev })
	primary := newFakeInstance(t, nil, nil, true)
	p := proxyFor(primary.srv.URL)
	p.ToolsRefresh = time.Hour
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sess := openSession(t, p, "%7")
	id := sess[sessionHeader]
	resp := sessionStream(t, ctx, srv.URL, sess)
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
	old := time.Now().Add(-time.Hour)
	p.sessions.mu.Lock()
	p.sessions.m[id].seen = old
	p.sessions.mu.Unlock()
	waitUntil(t, "a keep-alive to refresh the session", func() bool {
		p.sessions.mu.Lock()
		defer p.sessions.mu.Unlock()
		return p.sessions.m[id].seen.After(old)
	})
}
