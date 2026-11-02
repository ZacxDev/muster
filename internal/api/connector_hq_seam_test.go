package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/connectors"
)

// TestAnHQClientThroughTheProxyIsServedWithTheConnectorsTokenNeverTheAgents is
// the seam the claude-code chief's HQ MCP server stands on (internal/podagent
// hqmcp.go): `hq mcp` runs in the pod with HQ_URL = <connect>/connect/hq/<host>
// and HQ_TOKEN = the agent's OWN muster token, so every HQ call arrives here as
// the hq client builds it — base URL + "/api/v1/…", `Authorization: Bearer
// <agent token>`, JSON in and out. It is driven through the real handler, the
// real proxy and a real upstream:
//
//   - before the agent holds a grant, the first call is refused 403 with a JSON
//     {"error": …} naming the pending request (the hq client surfaces that field),
//     and it FILES that request — the only way the chief's request for the
//     connector ever reaches its owner;
//   - once approved, the upstream sees the CONNECTOR's bearer and never the
//     agent's token (in any header, the query or the body), the path below the
//     host prefix, the query, the JSON body and the hq client's own headers
//     (Idempotency-Key, X-HQ-Agent, User-Agent) unchanged; and the client gets
//     the upstream's status, JSON body and content type back — for a success
//     and for an error the hq client parses.
func TestAnHQClientThroughTheProxyIsServedWithTheConnectorsTokenNeverTheAgents(t *testing.T) {
	const (
		agentToken = "hq-seam-agent-token-5b17c0e9"
		hqSecret   = "hq_FAKE-connector-secret-9d3a41"
	)
	type seen struct {
		method, path, query, body string
		header                    http.Header
	}
	var mu sync.Mutex
	var got []seen
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, seen{r.Method, r.URL.Path, r.URL.RawQuery, string(b), r.Header.Clone()})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/tasks/missing-7f" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"task not found","code":"not_found"}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"tsk_31c8","title":"process the mail","assignees":["owner-login"]}`)
	}))
	t.Cleanup(up.Close)
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	host := "hq.fake.test:" + port

	f := newConnFixture(t)
	f.srv = New(nil, AuthConfig{HookToken: connTestHookToken, ServiceToken: connTestServiceToken, UIPassword: connTestPassword},
		log.New(io.Discard, "", 0))
	f.srv.UseExtensions(Extensions{Agents: agents.NewPG(f.pool), Connectors: f.store, ConnectorPolicy: connectors.Policy{
		AllowLoopback: true, InsecureHTTP: true,
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
	}})
	front := httptest.NewServer(f.srv.Handler())
	t.Cleanup(front.Close)
	id := f.seedAgent(t, "hq-seam-agent", agentToken)
	if _, err := f.store.CreateConnector(f.ctx, connectors.NewConnector{Name: "hq", Hosts: []string{host},
		Scheme: connectors.SchemeBearer, Secret: hqSecret}); err != nil {
		t.Fatal(err)
	}

	// The hq client's request, as internal/hqmcp's do() builds it.
	base := front.URL + "/connect/hq/" + host
	call := func(method, path string, body []byte) (int, string, string) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequest(method, base+path, rd)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+agentToken)
		req.Header.Set("User-Agent", "hq/v0.0.0-seam (mcp)")
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Idempotency-Key", "hq-mcp:5e1f")
		req.Header.Set("X-HQ-Agent", "ag_seam")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Content-Type"), string(b)
	}
	create := []byte(`{"title":"process the mail","assign":"me"}`)

	// No grant yet: refused, with a JSON error the hq client shows, and the
	// request is FILED for the owner.
	code, _, body := call("POST", "/api/v1/tasks?view=compact", create)
	var refusal struct {
		Error string `json:"error"`
		Grant string `json:"grant"`
	}
	if err := json.Unmarshal([]byte(body), &refusal); code != http.StatusForbidden || err != nil ||
		refusal.Grant != connectors.StatusPending || !strings.Contains(refusal.Error, "pending") {
		t.Fatalf("before approval: %d %s, want 403 with a JSON error naming the pending request", code, body)
	}
	mu.Lock()
	if len(got) != 0 {
		t.Fatalf("the upstream was reached %d time(s) before the grant was approved", len(got))
	}
	mu.Unlock()
	g, err := f.store.LatestGrant(f.ctx, id, "hq")
	if err != nil || g.Status != connectors.StatusPending {
		t.Fatalf("the first call filed no pending request: %+v %v", g, err)
	}
	if _, err := f.store.DecideGrant(f.ctx, g.ID, true, "operator", ""); err != nil {
		t.Fatal(err)
	}

	code, ctype, body := call("POST", "/api/v1/tasks?view=compact", create)
	if code != http.StatusCreated || ctype != "application/json" || body != `{"id":"tsk_31c8","title":"process the mail","assignees":["owner-login"]}` {
		t.Fatalf("approved create: %d %q %s", code, ctype, body)
	}
	code, _, body = call("GET", "/api/v1/tasks/missing-7f", nil)
	if code != http.StatusNotFound || body != `{"error":"task not found","code":"not_found"}` {
		t.Fatalf("approved get of a missing task: %d %s, want the upstream's 404 and JSON error", code, body)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("the upstream saw %d requests, want 2", len(got))
	}
	want := []seen{
		{method: "POST", path: "/api/v1/tasks", query: "view=compact", body: string(create)},
		{method: "GET", path: "/api/v1/tasks/missing-7f"},
	}
	for i, s := range got {
		if s.method != want[i].method || s.path != want[i].path || s.query != want[i].query || s.body != want[i].body {
			t.Errorf("upstream request %d = %s %s ?%s %q, want %s %s ?%s %q", i, s.method, s.path, s.query, s.body,
				want[i].method, want[i].path, want[i].query, want[i].body)
		}
		if v := s.header.Values("Authorization"); len(v) != 1 || v[0] != "Bearer "+hqSecret {
			t.Errorf("upstream request %d: Authorization = %q, want exactly the connector's bearer", i, v)
		}
		for name, vals := range s.header {
			for _, v := range vals {
				if strings.Contains(v, agentToken) {
					t.Errorf("upstream request %d: header %s carries the agent's muster token", i, name)
				}
			}
		}
		if strings.Contains(s.query, agentToken) || strings.Contains(s.body, agentToken) {
			t.Errorf("upstream request %d: the agent's muster token is in the query or body", i)
		}
		for name, v := range map[string]string{"Idempotency-Key": "hq-mcp:5e1f", "X-Hq-Agent": "ag_seam",
			"User-Agent": "hq/v0.0.0-seam (mcp)", "Accept": "application/json"} {
			if s.header.Get(name) != v {
				t.Errorf("upstream request %d: %s = %q, want the hq client's %q", i, name, s.header.Get(name), v)
			}
		}
	}
}
