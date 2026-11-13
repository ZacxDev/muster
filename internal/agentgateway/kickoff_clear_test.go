package agentgateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// rowClearer is Config.Kickoff over ONE in-memory agent row, applying the same
// predicate as agents.PGStore.ClearKickoffFailure (kicked_off AND kickoff_error
// set). The SQL itself is pinned against Postgres by
// internal/agents TestClearKickoffFailure*; this fake exists so the assertion
// below can be the user-visible one — agents.KickoffFailed on the row — rather
// than a call count.
type rowClearer struct {
	mu    sync.Mutex
	row   agents.Agent
	calls int
}

func (c *rowClearer) ClearKickoffFailure(_ context.Context, id int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if id == c.row.ID && c.row.KickedOff && c.row.KickoffError != "" {
		c.row.KickoffError = ""
	}
	return nil
}

func (c *rowClearer) failed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return agents.KickoffFailed(c.row)
}

// failedKickoffAgent is an agent whose first turn timed out: stamped, with the
// failure recorded — the exact row that carried a "kickoff failed" badge for ever.
func failedKickoffAgent() agents.Agent {
	a := fixtureAgent()
	a.KickedOff = true
	a.KickoffError = "kickoff turn failed after it was handed to the gateway: context deadline exceeded"
	return a
}

// TestALaterCompletedTurnClearsAStaleKickoffFailure is the regression for the
// badge that outlived every later success. A first turn failed (recorded, never
// retried — at-most-once), the agent then completed a later turn through the
// gateway, and agents.KickoffFailed must now read false. A turn that FAILED, or
// came back blank, must leave the record exactly as it was.
//
// Every turn muster runs goes through Send or SendWithTools (Chat and
// ChatWithTools are Resolve + those), so the four doors below are the whole set,
// including the toolless 404 fallback to chat-completions.
func TestALaterCompletedTurnClearsAStaleKickoffFailure(t *testing.T) {
	const said = "on it, task picked up"
	completed := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotImplemented)
			return
		}
		writeResponsesStream(w, said)
	}
	blank := func(w http.ResponseWriter, r *http.Request) { writeResponsesStream(w, "  ") }
	broken := func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"type":"internal","message":"boom"}}`, http.StatusInternalServerError)
	}
	legacyCompleted := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"" + said + "\"}}]}\n\ndata: [DONE]\n\n"))
	}
	// The legacy transport returns the text it streamed TOGETHER WITH the read
	// error when the connection is cut mid-stream (agents.ChatStream returns
	// full.String(), sc.Err()), so a non-blank reply is not by itself a
	// completed turn — this case is what makes the error conjunct reachable.
	legacyCut := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"half an answer \"}}]}\n\n"))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}

	for _, c := range []struct {
		name        string
		handler     http.HandlerFunc
		shape       string
		wantErr     bool
		stillFailed bool
	}{
		{"completed/chat", completed, "chat", false, false},
		{"completed/tools", completed, "tools", false, false},
		{"completed via the 404 fallback/chat", legacyCompleted, "chat", false, false},
		{"blank reply/chat", blank, "chat", false, true},
		{"blank reply/tools", blank, "tools", false, true},
		{"failed turn/chat", broken, "chat", true, true},
		{"failed turn/tools", broken, "tools", true, true},
		{"partial reply then cut/chat", legacyCut, "chat", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(c.handler)
			defer srv.Close()
			gw := newGateway(t, srv)
			clr := &rowClearer{row: failedKickoffAgent()}
			gw.kickoff = clr
			if !clr.failed() {
				t.Fatal("instrument check FAILED: the fixture row does not read as KickoffFailed before the turn")
			}
			var (
				reply string
				err   error
			)
			if c.shape == "chat" {
				reply, err = gw.Chat(context.Background(), clr.row, "k", "status?", nil)
			} else {
				reply, err = gw.ChatWithTools(context.Background(), clr.row, "k", "", "status?", nil, nil, nil)
			}
			if (err != nil) != c.wantErr {
				t.Fatalf("turn error = %v, want error: %v", err, c.wantErr)
			}
			if got := clr.failed(); got != c.stillFailed {
				if c.stillFailed {
					t.Fatalf("a turn that did NOT complete (reply %q, err %v) erased the kickoff failure record", reply, err)
				}
				t.Fatalf("KickoffFailed still true after a later turn COMPLETED: the stale \"kickoff failed\" "+
					"badge outlives every success (clear calls: %d)", clr.calls)
			}
		})
	}
}

// TestNoKickoffClearerIsANoOp: Config.Kickoff is optional, and a gateway built
// without it (no database) must still complete turns.
func TestNoKickoffClearerIsANoOp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeResponsesStream(w, "fine")
	}))
	defer srv.Close()
	reply, err := newGateway(t, srv).Chat(context.Background(), failedKickoffAgent(), "k", "hi", nil)
	if err != nil || reply != "fine" {
		t.Fatalf("reply %q, err %v", reply, err)
	}
}
