package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
)

// ---------------------------------------------------------------------------
// internal_http_request, the tool: the model names a connector, a method and
// a path — and nothing else is accepted. No database: the grant is a stub, the
// service a loopback fake.
// ---------------------------------------------------------------------------

const ihToolToken = "fixture-tool-token-41d0e2"

// ihGrantStore is a connector Store with one grant per agent at a fixed
// status, recording standing grants created. Methods the tool does not call
// panic (the nil embedded interface).
type ihGrantStore struct {
	connectors.Store
	status   string
	mu       sync.Mutex
	standing []string
}

func (s *ihGrantStore) LatestGrant(_ context.Context, agent int64, name string) (connectors.Grant, error) {
	return connectors.Grant{ID: 1, AgentID: agent, Connector: name, Status: s.status, Source: connectors.SourceApp}, nil
}
func (s *ihGrantStore) Audit(context.Context, connectors.AuditEntry) error { return nil }
func (s *ihGrantStore) GetConnector(_ context.Context, name string) (connectors.Connector, error) {
	return connectors.Connector{Name: name, Scheme: connectors.SchemeBearer, Hosts: []string{"api.example.com"}}, nil
}
func (s *ihGrantStore) CreateStandingGrant(_ context.Context, connector, _, _ string, _ int64) (connectors.StandingGrant, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.standing = append(s.standing, connector)
	return connectors.StandingGrant{ID: 9, Connector: connector}, true, nil
}
func (s *ihGrantStore) ListPendingGrants(context.Context) ([]connectors.Grant, error) {
	return nil, nil
}

// ihAgents is an agents.Store that answers Get only.
type ihAgents struct{ agents.Store }

func (ihAgents) Get(_ context.Context, id int64) (agents.Agent, error) {
	return agents.Agent{ID: id, Name: "worker-1", Repo: "example-org/widget"}, nil
}

type ihToolFixture struct {
	s    *Server
	ctx  context.Context
	hits atomic.Int32
	got  atomic.Value // last request: method + " " + RequestURI + " " + Authorization
	a    agents.Agent
	gs   *ihGrantStore
}

func newIHToolFixture(t *testing.T, status string) *ihToolFixture {
	t.Helper()
	f := &ihToolFixture{a: agents.Agent{ID: 3, Name: "worker-1", Repo: "example-org/widget"}}
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.got.Store(r.Method + " " + r.URL.RequestURI() + " " + r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"auth":"`+r.Header.Get("Authorization")+`"}`)
	}))
	t.Cleanup(svc.Close)
	ih, err := connectors.NewInternalHTTP(connectors.InternalHTTPSettings{Name: "app-driver", BaseURL: svc.URL}, ihToolToken)
	if err != nil {
		t.Fatal(err)
	}
	f.gs = &ihGrantStore{status: status}
	store := connectors.WithVirtual(f.gs, ih, authz.IsAdmin)
	f.s = &Server{logger: log.New(io.Discard, "", 0)}
	f.s.ext.Connectors = store
	f.s.ext.Agents = ihAgents{}
	f.s.connectProxy = &connectors.Proxy{Store: store}
	f.ctx = authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession})
	return f
}

func (f *ihToolFixture) call(args string) map[string]any {
	var out map[string]any
	_ = json.Unmarshal([]byte(f.s.toolInternalHTTP(f.ctx, f.a, args)), &out)
	return out
}

// TestTheToolTakesNoDestinationFromTheModel: an argument that tries to name
// the destination or a header — url, base_url, host, headers — is refused
// outright, not ignored, and nothing is sent; the one accepted shape reaches
// the configured service with the server's token.
func TestTheToolTakesNoDestinationFromTheModel(t *testing.T) {
	f := newIHToolFixture(t, connectors.StatusApproved)
	for _, extra := range []string{`"url":"http://evil.example/v1/x"`, `"base_url":"http://evil.example"`,
		`"host":"evil.example"`, `"headers":{"Authorization":"Bearer mine"}`, `"scheme":"https"`} {
		out := f.call(`{"connector":"app-driver","method":"GET","path":"/v1/x",` + extra + `}`)
		if e, _ := out["error"].(string); !strings.Contains(e, "never an argument") {
			t.Errorf("with %s: %v, want a refusal", extra, out)
		}
	}
	if n := f.hits.Load(); n != 0 {
		t.Fatalf("refused calls reached the service %d times", n)
	}
	out := f.call(`{"connector":"app-driver","method":"GET","path":"/v1/sessions","query":{"id":"a1"}}`)
	if out["status"] != float64(200) {
		t.Fatalf("the accepted call: %v", out)
	}
	if got := f.got.Load().(string); got != "GET /v1/sessions?id=a1 Bearer "+ihToolToken {
		t.Fatalf("the service received %q", got)
	}
	if b, _ := json.Marshal(out); strings.Contains(string(b), ihToolToken) {
		t.Fatalf("the tool result carries the token: %s", b)
	}
}

// TestTheToolNeedsAnApprovedGrant: pending, denied and revoked grants are
// refused with no request sent.
func TestTheToolNeedsAnApprovedGrant(t *testing.T) {
	for _, st := range []string{connectors.StatusPending, connectors.StatusDenied, connectors.StatusRevoked} {
		f := newIHToolFixture(t, st)
		out := f.call(`{"connector":"app-driver","method":"GET","path":"/v1/x"}`)
		if e, _ := out["error"].(string); !strings.Contains(e, "no approved grant") {
			t.Errorf("grant %s: %v, want the no-grant refusal", st, out)
		}
		if f.hits.Load() != 0 {
			t.Errorf("grant %s: the service was reached", st)
		}
	}
}

// TestTheToolServesAdminsAgentsOnly: under a member principal the connector
// does not exist (WithVirtual's allow is authz.IsAdmin), so nothing is sent.
func TestTheToolServesAdminsAgentsOnly(t *testing.T) {
	f := newIHToolFixture(t, connectors.StatusApproved)
	f.ctx = authz.WithPrincipal(context.Background(), authz.Principal{UserID: 2, Role: authz.RoleMember, Via: authz.ViaSession})
	out := f.call(`{"connector":"app-driver","method":"GET","path":"/v1/x"}`)
	if e, _ := out["error"].(string); !strings.Contains(e, "not an internal_http connector") || f.hits.Load() != 0 {
		t.Fatalf("a member's agent: %v (hits %d)", out, f.hits.Load())
	}
}

// TestAnInternalHTTPApprovalIsNeverShared: approving an agent for an
// internal_http connector records no standing grant, and no standing key
// exists for one — while a stored connector's approval still is shared (the
// positive control: the same agent, the same store).
func TestAnInternalHTTPApprovalIsNeverShared(t *testing.T) {
	f := newIHToolFixture(t, connectors.StatusApproved)
	if _, _, ok := f.s.standingKey(f.ctx, "app-driver", f.a); ok {
		t.Error("standingKey has a key for an internal_http connector")
	}
	if _, _, ok := f.s.standingKey(f.ctx, "svc", f.a); !ok {
		t.Error("positive control: standingKey has no key for a stored connector")
	}
	approved := connectors.Grant{ID: 5, AgentID: f.a.ID, Status: connectors.StatusApproved, Source: connectors.SourceApp}
	approved.Connector = "app-driver"
	if sh := f.s.shareApproval(f.ctx, approved, connectors.DecisionSource{}); sh.Standing.ID != 0 {
		t.Errorf("approving app-driver was shared as standing grant %d", sh.Standing.ID)
	}
	stored := connectors.Grant{ID: 6, AgentID: f.a.ID, Status: connectors.StatusApproved, Source: connectors.SourceStored,
		Connector: "svc"}
	if sh := f.s.shareApproval(f.ctx, stored, connectors.DecisionSource{}); sh.Standing.ID == 0 {
		t.Error("positive control: a stored connector's approval was not shared")
	}
	if strings.Join(f.gs.standing, ",") != "svc" {
		t.Fatalf("standing grants created for %v, want only svc", f.gs.standing)
	}
}
