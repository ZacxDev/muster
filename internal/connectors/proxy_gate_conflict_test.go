package connectors_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	// results, when set, receives every ForwardResult and the hook ctx's
	// error; callers every Caller Admit saw in the request's context.
	results chan afterCall
	callers *[]connectors.Caller
}

type afterCall struct {
	res    connectors.ForwardResult
	ctxErr error
}

func (g afterGate) Connector() string { return officers.GitHubConnector }
func (g afterGate) Describe() connectors.Connector {
	return connectors.Connector{Name: officers.GitHubConnector, Hosts: []string{g.host}, Scheme: connectors.SchemeBasic, Username: "x-access-token"}
}
func (g afterGate) Admit(r *http.Request, _, _ string) (*http.Request, error) {
	if g.callers != nil {
		c, _ := connectors.CallerOf(r.Context())
		*g.callers = append(*g.callers, c)
	}
	return connectors.WithAfterForward(r, func(ctx context.Context, res connectors.ForwardResult) {
		if g.results != nil {
			g.results <- afterCall{res: res, ctxErr: ctx.Err()}
			return
		}
		*g.calls = append(*g.calls, res.Status)
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

// TestAGatesAfterForwardHookRunsWhenTheClientGoesAway (R2): the upstream
// answers its head, the client drops the connection before the body ends,
// and ReverseProxy aborts the handler (panic(http.ErrAbortHandler)). The hook
// still runs, once, with ctx NOT cancelled, the upstream's status and the
// body it had read so far (Complete false).
func TestAGatesAfterForwardHookRunsWhenTheClientGoesAway(t *testing.T) {
	f, caller, _ := gatedFixture(t)
	results := make(chan afterCall, 4)
	caller.Gate = afterGate{host: f.host, up: f.up, results: results}
	release := make(chan struct{})
	f.up.handle = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("0030\x02Counting objects: 100% (3/3), done.\n"))
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write([]byte("000eunpack ok\n0000"))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.SplitN(strings.TrimPrefix(r.URL.EscapedPath(), "/connect/"), "/", 3)
		f.proxy.Serve(w, r.WithContext(dbtest.AsOperator(r.Context())), caller, parts[0], parts[1], parts[2])
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/connect/github/"+f.host+"/example-org/example-web.git/git-receive-pack",
		strings.NewReader("0000"))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	_, _ = resp.Body.Read(buf) // the head and the first bytes arrived
	cancel()
	_ = resp.Body.Close()
	time.Sleep(50 * time.Millisecond)
	close(release)
	select {
	case c := <-results:
		if !c.res.Forwarded || c.res.Status != http.StatusOK || c.res.Complete || !strings.Contains(string(c.res.Body), "Counting objects") ||
			c.ctxErr != nil {
			t.Fatalf("hook got %+v (body %q), ctx err %v; want forwarded, 200, incomplete, the bytes read, ctx live", c.res, c.res.Body, c.ctxErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the after-forward hook never ran after the client went away")
	}
	select {
	case c := <-results:
		t.Fatalf("the hook ran twice: %+v", c.res)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestAGatedRequestCarriesItsCallerAndItsWholeBodyToTheHook: Admit sees the
// caller Serve resolved (connectors.CallerOf); a completed answer reaches the
// hook whole (Complete), and a request refused after Admit (no credential)
// runs the hook with Forwarded false.
func TestAGatedRequestCarriesItsCallerAndItsWholeBodyToTheHook(t *testing.T) {
	f, caller, _ := gatedFixture(t)
	results := make(chan afterCall, 4)
	var callers []connectors.Caller
	caller.Gate = afterGate{host: f.host, up: f.up, results: results, callers: &callers}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.SplitN(strings.TrimPrefix(r.URL.EscapedPath(), "/connect/"), "/", 3)
		f.proxy.Serve(w, r.WithContext(dbtest.AsOperator(r.Context())), caller, parts[0], parts[1], parts[2])
	})
	f.up.handle = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":7}`))
	}
	if w := gatedDo(h, "POST", "/connect/github/"+f.host+"/repos/example-org/example-web/pulls", `{"title":"t"}`); w.Code != http.StatusCreated {
		t.Fatalf("an admitted write: %d %s", w.Code, w.Body)
	}
	c := <-results
	if !c.res.Forwarded || c.res.Status != http.StatusCreated || !c.res.Complete || string(c.res.Body) != `{"number":7}` {
		t.Fatalf("hook got %+v (body %q)", c.res, c.res.Body)
	}
	if len(callers) != 1 || callers[0].AgentName != caller.AgentName || callers[0].AgentID != caller.AgentID {
		t.Fatalf("Admit saw callers %+v, want [%s]", callers, caller.AgentName)
	}
	caller.Gate = credFailGate{afterGate{host: f.host, up: f.up, results: results}}
	gatedDo(h, "POST", "/connect/github/"+f.host+"/repos/example-org/example-web/pulls", `{"title":"t"}`)
	select {
	case c := <-results:
		if c.res.Forwarded {
			t.Fatalf("a request refused after Admit: hook got %+v, want Forwarded false", c.res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a request refused after Admit ran no hook (its create lock would never be released)")
	}
}

// credFailGate admits, then cannot mint a credential.
type credFailGate struct{ afterGate }

func (g credFailGate) Credential(context.Context, connectors.Caller, string, string) (string, bool, error) {
	return "", false, fmt.Errorf("fixture: no credential")
}
