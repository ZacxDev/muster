package connectors_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
)

// queuedGate answers every write ErrGateQueued (a draft officer's create the
// open cap held back) and admits reads.
type queuedGate struct{ host string }

func (g queuedGate) Connector() string { return officers.GitHubConnector }
func (g queuedGate) Describe() connectors.Connector {
	return connectors.Connector{Name: officers.GitHubConnector, Hosts: []string{g.host}, Scheme: connectors.SchemeBasic, Username: "x-access-token"}
}
func (g queuedGate) Admit(r *http.Request, _, _ string) (*http.Request, error) {
	if r.Method == http.MethodGet {
		return r, nil
	}
	return nil, fmt.Errorf("%w: held at position 2 of 2", connectors.ErrGateQueued)
}
func (g queuedGate) Credential(context.Context, connectors.Caller, string, string) (string, bool, error) {
	return gateSecret, false, nil
}
func (g queuedGate) Rejected(connectors.Caller) {}

// TestAGateQueuedWriteIsA202ThatNeverReachesTheUpstream: a write the gate
// queued is answered 202 {"queued": true, "message": …} — not a 403 or 409
// the agent would escalate — and the upstream sees nothing; a read through
// the same gate is served (control). ErrGateQueued is NOT an ErrGateRefused.
func TestAGateQueuedWriteIsA202ThatNeverReachesTheUpstream(t *testing.T) {
	f, caller, _ := gatedFixture(t)
	caller.Gate = queuedGate{host: f.host}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.SplitN(strings.TrimPrefix(r.URL.EscapedPath(), "/connect/"), "/", 3)
		f.proxy.Serve(w, r.WithContext(dbtest.AsOperator(r.Context())), caller, parts[0], parts[1], parts[2])
	})
	w := gatedDo(h, "POST", "/connect/github/"+f.host+"/repos/example-org/example-web/pulls", `{"title":"t"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("a queued write: %d %s, want 202", w.Code, w.Body)
	}
	var body connectors.QueuedBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || !body.Queued || !strings.Contains(body.Message, "held at position 2 of 2") {
		t.Fatalf("a queued write's body: %s (%v), want queued true and the gate's message", w.Body, err)
	}
	if strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("a queued write's body carries an error field: %s", w.Body)
	}
	if n := f.up.hits.Load(); n != 0 {
		t.Fatalf("the upstream received %d queued request(s)", n)
	}
	if w := gatedDo(h, "GET", "/connect/github/"+f.host+"/repos/example-org/example-web/pulls/5", ""); w.Code != http.StatusOK {
		t.Fatalf("control: a read through the same gate: %d %s", w.Code, w.Body)
	}
	if errors.Is(fmt.Errorf("%w: x", connectors.ErrGateQueued), connectors.ErrGateRefused) {
		t.Fatal("ErrGateQueued wraps ErrGateRefused: a queued write would read as a refusal")
	}
}
