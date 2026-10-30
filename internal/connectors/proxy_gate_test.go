package connectors_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officership"
)

// gateSecret is the credential a gated caller's requests must carry — and
// the stored connector's fakeSecret is the one they must NEVER carry.
const gateSecret = "fixture-officer-app-token-5d1f"

// gatedFixture: the proxy fixture with a stored `github` connector on the fake
// upstream, approved for agent A, and agent A gated by a REAL push gate
// (internal/officership) serving that host. The seam under test is the
// proxy's: the gate decides BEFORE a credential exists, and the credential is
// the gate's.
func gatedFixture(t *testing.T) (*fixture, connectors.Caller, http.Handler) {
	t.Helper()
	f := newFixture(t, connectors.SchemeBasic)
	if _, err := f.store.CreateConnector(f.ctx, connectors.NewConnector{Name: officers.GitHubConnector, Hosts: []string{f.host},
		Scheme: connectors.SchemeBasic, Username: "x-access-token", Secret: fakeSecret}); err != nil {
		t.Fatal(err)
	}
	g, _, err := f.store.RequestGrant(f.ctx, f.agentA.AgentID, officers.GitHubConnector, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DecideGrant(f.ctx, g.ID, true, "operator", ""); err != nil {
		t.Fatal(err)
	}
	ship := &officers.Ship{Stage: officers.StageMerge, Base: "main", BranchPrefix: "officer/"}
	caller := f.agentA
	caller.Gate = &officership.PushGate{Repo: "example-org/example-web", Ship: ship, GitHost: "git.fake.test", APIHost: f.host,
		Cred: func(context.Context, connectors.Caller, string, string) (string, bool, error) {
			return gateSecret, false, nil
		}}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.SplitN(strings.TrimPrefix(r.URL.EscapedPath(), "/connect/"), "/", 3)
		f.proxy.Serve(w, r.WithContext(dbtest.AsOperator(r.Context())), caller, parts[0], parts[1], parts[2])
	})
	return f, caller, h
}

func gatedDo(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// TestAGatedCallersWriteIsRefusedBeforeAnyCredentialAndNeverReachesTheUpstream:
// the merge endpoint and a ref write, refused 403 by the write policy, with
// the upstream seeing nothing.
func TestAGatedCallersWriteIsRefusedBeforeAnyCredentialAndNeverReachesTheUpstream(t *testing.T) {
	f, _, h := gatedFixture(t)
	for _, c := range []struct{ method, path, body string }{
		{"PUT", "/connect/github/" + f.host + "/repos/example-org/example-web/pulls/5/merge", `{"merge_method":"squash"}`},
		{"POST", "/connect/github/" + f.host + "/repos/example-org/example-web/git/refs", `{"ref":"refs/tags/v9.0.0","sha":"x"}`},
		{"POST", "/connect/github/" + f.host + "/graphql", `{"query":"mutation{}"}`},
	} {
		w := gatedDo(h, c.method, c.path, c.body)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "write policy") {
			t.Errorf("%s %s: %d %s, want 403 from the write policy", c.method, c.path, w.Code, w.Body)
		}
	}
	if n := f.up.hits.Load(); n != 0 {
		t.Fatalf("the upstream received %d refused request(s)", n)
	}
}

// TestAGatedCallersAdmittedRequestCarriesTheGatesCredentialOnly: a read and
// an allowed write reach the upstream with the GATE's token — never the
// stored connector's secret — and a gated caller cannot name another
// connector to reach a different credential.
func TestAGatedCallersAdmittedRequestCarriesTheGatesCredentialOnly(t *testing.T) {
	f, _, h := gatedFixture(t)
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+gateSecret))
	for _, c := range []struct{ method, rest, body string }{
		{"GET", "repos/example-org/example-web/pulls/5", ""},
		{"POST", "repos/example-org/example-web/pulls", `{"title":"t","head":"officer/5-x","base":"main"}`},
	} {
		w := gatedDo(h, c.method, "/connect/github/"+f.host+"/"+c.rest, c.body)
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", c.method, c.rest, w.Code, w.Body)
		}
		if got := f.up.last(t).Header.Get("Authorization"); got != want {
			t.Fatalf("%s %s: upstream Authorization = %q, want the gate's credential", c.method, c.rest, got)
		}
	}
	// The stored connector `svc`, APPROVED for this agent, is still not
	// reachable by a gated caller: the gate serves its own connector only.
	f.approve(t, f.agentA)
	before := f.up.hits.Load()
	w := gatedDo(h, "GET", "/connect/svc/"+f.host+"/repos/example-org/example-web", "")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "serves connector github only") {
		t.Fatalf("a gated caller on another connector: %d %s", w.Code, w.Body)
	}
	if f.up.hits.Load() != before {
		t.Fatal("a gated caller's request on another connector reached the upstream")
	}
}
