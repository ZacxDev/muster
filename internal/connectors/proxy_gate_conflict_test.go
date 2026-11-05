package connectors_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
)

// conflictGate refuses every write with ErrGateConflict (a content refusal,
// as a draft officer's push gate makes) and admits reads.
type conflictGate struct{ host string }

func (g conflictGate) Connector() string { return officers.GitHubConnector }
func (g conflictGate) Describe() connectors.Connector {
	return connectors.Connector{Name: officers.GitHubConnector, Hosts: []string{g.host}, Scheme: connectors.SchemeBasic, Username: "x-access-token"}
}
func (g conflictGate) Admit(r *http.Request, _, _ string) (*http.Request, error) {
	if r.Method == http.MethodGet {
		return r, nil
	}
	return nil, fmt.Errorf("%w: a forbidden path", connectors.ErrGateConflict)
}
func (g conflictGate) Credential(context.Context, connectors.Caller, string, string) (string, bool, error) {
	return gateSecret, false, nil
}
func (g conflictGate) Rejected(connectors.Caller) {}

// TestAGateConflictIsA409ThatNeverReachesTheUpstream: a content refusal is
// answered 409 (not the 403 of a policy refusal), says why, and the upstream
// sees nothing; a read through the same gate is served (control), and
// ErrGateConflict is still an ErrGateRefused for every other check.
func TestAGateConflictIsA409ThatNeverReachesTheUpstream(t *testing.T) {
	f, caller, _ := gatedFixture(t)
	caller.Gate = conflictGate{host: f.host}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.SplitN(strings.TrimPrefix(r.URL.EscapedPath(), "/connect/"), "/", 3)
		f.proxy.Serve(w, r.WithContext(dbtest.AsOperator(r.Context())), caller, parts[0], parts[1], parts[2])
	})
	w := gatedDo(h, "POST", "/connect/github/"+f.host+"/repos/example-org/example-web/pulls", `{"title":"t"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "a forbidden path") {
		t.Fatalf("a content refusal: %d %s, want 409 naming why", w.Code, w.Body)
	}
	if n := f.up.hits.Load(); n != 0 {
		t.Fatalf("the upstream received %d refused request(s)", n)
	}
	if w := gatedDo(h, "GET", "/connect/github/"+f.host+"/repos/example-org/example-web/pulls/5", ""); w.Code != http.StatusOK {
		t.Fatalf("control: a read through the same gate: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(connectors.ErrGateConflict.Error(), connectors.ErrGateRefused.Error()) {
		t.Fatal("ErrGateConflict does not wrap ErrGateRefused")
	}
}
