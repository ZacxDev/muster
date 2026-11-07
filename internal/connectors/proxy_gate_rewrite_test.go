package connectors_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
)

// rewriteGate admits every request with a response rewrite that reads the
// upstream's body and replaces it (or fails, when fail is set), and an
// after-forward hook recording what the upstream sent.
type rewriteGate struct {
	host    string
	fail    bool
	seen    *[]string
	results chan connectors.ForwardResult
}

func (g rewriteGate) Connector() string { return officers.GitHubConnector }
func (g rewriteGate) Describe() connectors.Connector {
	return connectors.Connector{Name: officers.GitHubConnector, Hosts: []string{g.host}, Scheme: connectors.SchemeBasic, Username: "x-access-token"}
}
func (g rewriteGate) Admit(r *http.Request, _, _ string) (*http.Request, error) {
	out := connectors.WithResponseRewrite(r, func(_ context.Context, resp *http.Response) error {
		b, _ := io.ReadAll(resp.Body)
		*g.seen = append(*g.seen, string(b))
		if g.fail {
			return fmt.Errorf("verification failed")
		}
		resp.Body = io.NopCloser(strings.NewReader("REWRITTEN by the gate"))
		return nil
	})
	return connectors.WithAfterForward(out, func(_ context.Context, res connectors.ForwardResult) { g.results <- res }), nil
}
func (g rewriteGate) Credential(context.Context, connectors.Caller, string, string) (string, bool, error) {
	return gateSecret, false, nil
}
func (g rewriteGate) Rejected(connectors.Caller) {}

// TestAGatesResponseRewriteRunsBeforeTheClientSeesTheAnswer: the proxy hands
// an admitted request's response rewrite the upstream's answer, and the
// client receives what the rewrite put there — never the upstream's bytes —
// while the after-forward hook still sees the upstream's. A failing rewrite is
// a 502 and nothing of the upstream's answer reaches the client.
func TestAGatesResponseRewriteRunsBeforeTheClientSeesTheAnswer(t *testing.T) {
	f, caller, _ := gatedFixture(t)
	f.up.handle = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "UPSTREAM said ok refs/heads/x")
	}
	var seen []string
	results := make(chan connectors.ForwardResult, 2)
	caller.Gate = rewriteGate{host: f.host, seen: &seen, results: results}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.SplitN(strings.TrimPrefix(r.URL.EscapedPath(), "/connect/"), "/", 3)
		f.proxy.Serve(w, r.WithContext(dbtest.AsOperator(r.Context())), caller, parts[0], parts[1], parts[2])
	})
	w := gatedDo(h, "POST", "/connect/github/"+f.host+"/example-org/example-web.git/git-receive-pack", "0000")
	if w.Code != http.StatusOK || w.Body.String() != "REWRITTEN by the gate" {
		t.Fatalf("the client got %d %q, want the gate's rewrite", w.Code, w.Body.String())
	}
	if len(seen) != 1 || seen[0] != "UPSTREAM said ok refs/heads/x" {
		t.Fatalf("the rewrite saw %q, want the upstream's answer", seen)
	}
	if res := <-results; string(res.Body) != "UPSTREAM said ok refs/heads/x" {
		t.Fatalf("the after-forward hook saw %q, want the upstream's bytes", res.Body)
	}
	caller.Gate = rewriteGate{host: f.host, fail: true, seen: &seen, results: results}
	w = gatedDo(h, "POST", "/connect/github/"+f.host+"/example-org/example-web.git/git-receive-pack", "0000")
	if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "UPSTREAM") {
		t.Fatalf("a failing rewrite: %d %q, want a 502 without the upstream's answer", w.Code, w.Body.String())
	}
	<-results
}
