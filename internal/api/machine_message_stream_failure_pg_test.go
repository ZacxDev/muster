package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentoutcome"
	"github.com/ZacxDev/muster/internal/authz"
)

// A GATEWAY AGENT WHOSE RUNTIME FAILS THE TURN AND KEEPS THE STREAM OPEN ENDS
// THE MACHINE-DOOR REQUEST PROMPTLY: an error status to the caller, the agent's
// outcome FAILED with the runtime's words, no assistant row.
//
// 🔴 RED BEFORE THIS CHANGE (the incident): the runtime wrote `response.failed`
// and never ended the body (a gateway runtime's streaming handler does that
// when its run had already reported a lifecycle error before throwing); the
// transport read on for EOF, so the handler did not return, nothing was stored
// and the outcome stayed `running`. The transport shapes are pinned one by one
// in internal/agents/stream_failure_shapes_test.go; this is the same failure
// through the real handler, gateway and outcome tracker.
func TestAMachineMessageWhoseRuntimeFailsAndHoldsTheStreamEndsFailed(t *testing.T) {
	o := newOutcomeFixture(t, stubModelCatalog{})
	a := o.dispatchGateway(t, "provider refuses the turn", "openrouter/acme/acme-v4.1-pro")
	if a.Outcome != agentoutcome.Running {
		t.Fatalf("instrument check FAILED: outcome %q; want a tracked, running gateway agent", a.Outcome)
	}
	const providerWords = "FailoverError: 403 Key limit exceeded (daily limit)"
	release := make(chan struct{})
	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/responses") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseEvent("response.created", map[string]any{"response": map[string]any{"status": "in_progress"}}) +
			sseEvent("response.failed", map[string]any{"response": map[string]any{"status": "failed",
				"error": map[string]any{"code": "api_error", "message": providerWords}}})))
		w.(http.Flusher).Flush()
		select { // and the connection stays open
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(runtime.Close)
	t.Cleanup(func() { close(release) })
	restore := o.useRuntime(t, runtime, a)
	t.Cleanup(restore)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- o.machineMessageCtx(context.Background(), o.tokenPrincipal(), a, "Fix the bug.") }()
	var w *httptest.ResponseRecorder
	select {
	case w = <-done:
	case <-time.After(10 * time.Second):
		close(release) // let the handler finish so the fixture can tear down
		release = make(chan struct{})
		<-done
		t.Fatal("the machine message did not answer within 10s of the runtime's failure (the turn hangs)")
	}
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), providerWords) {
		t.Fatalf("machine message answered HTTP %d %s; want 502 carrying the runtime's words", w.Code, w.Body)
	}
	got := o.agent(t, a.ID)
	if got.Outcome != agentoutcome.Failed || !strings.Contains(got.OutcomeReason, providerWords) {
		t.Fatalf("outcome %q reason %q; want failed with the runtime's words", got.Outcome, got.OutcomeReason)
	}
	var assistant int
	if err := o.pool.QueryRow(authz.System(context.Background()),
		`SELECT count(*) FROM chat_messages WHERE agent_id = $1 AND role = 'assistant'`, a.ID).Scan(&assistant); err != nil {
		t.Fatal(err)
	}
	if assistant != 0 {
		t.Errorf("%d assistant rows stored for a failed turn; want 0", assistant)
	}
}
