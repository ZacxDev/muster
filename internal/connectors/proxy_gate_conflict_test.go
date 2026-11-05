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

// afterGate admits every request and attaches an after-forward hook that
// records the status it is told and how many upstream hits had landed then.
type afterGate struct {
	host  string
	up    *upstream
	calls *[]int
	hits  *[]int32
}

func (g afterGate) Connector() string { return officers.GitHubConnector }
func (g afterGate) Describe() connectors.Connector {
	return connectors.Connector{Name: officers.GitHubConnector, Hosts: []string{g.host}, Scheme: connectors.SchemeBasic, Username: "x-access-token"}
}
func (g afterGate) Admit(r *http.Request, _, _ string) (*http.Request, error) {
	return connectors.WithAfterForward(r, func(_ context.Context, status int) {
		*g.calls = append(*g.calls, status)
		*g.hits = append(*g.hits, g.up.hits.Load())
	}), nil
}
func (g afterGate) Credential(context.Context, connectors.Caller, string, string) (string, bool, error) {
	return gateSecret, false, nil
}
func (g afterGate) Rejected(connectors.Caller) {}

// TestTheProxyCallsAGatesAfterForwardHookWithTheUpstreamStatus: an admitted
// request's hook runs ONCE, after the upstream answered, with its status (200,
// then a scripted 422); a refused request (the conflict gate) runs none.
func TestTheProxyCallsAGatesAfterForwardHookWithTheUpstreamStatus(t *testing.T) {
	f, caller, _ := gatedFixture(t)
	var calls []int
	var hits []int32
	caller.Gate = afterGate{host: f.host, up: f.up, calls: &calls, hits: &hits}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.SplitN(strings.TrimPrefix(r.URL.EscapedPath(), "/connect/"), "/", 3)
		f.proxy.Serve(w, r.WithContext(dbtest.AsOperator(r.Context())), caller, parts[0], parts[1], parts[2])
	})
	if w := gatedDo(h, "POST", "/connect/github/"+f.host+"/repos/example-org/example-web/pulls", `{"title":"t"}`); w.Code != http.StatusOK {
		t.Fatalf("an admitted write: %d %s", w.Code, w.Body)
	}
	f.up.handle = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnprocessableEntity) }
	gatedDo(h, "POST", "/connect/github/"+f.host+"/repos/example-org/example-web/pulls", `{"title":"t"}`)
	if len(calls) != 2 || calls[0] != http.StatusOK || calls[1] != http.StatusUnprocessableEntity || hits[0] != 1 || hits[1] != 2 {
		t.Fatalf("hook calls %v at upstream hits %v; want [200 422] at [1 2]", calls, hits)
	}
	caller.Gate = conflictGate{host: f.host}
	calls = nil
	gatedDo(h, "POST", "/connect/github/"+f.host+"/repos/example-org/example-web/pulls", `{"title":"t"}`)
	if len(calls) != 0 {
		t.Fatalf("a refused request ran the hook %d time(s)", len(calls))
	}
}
