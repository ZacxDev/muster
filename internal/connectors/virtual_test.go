package connectors_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/connectors"
)

// stubVirtual is a virtual connector that records every credential request.
type stubVirtual struct {
	hosts  []string
	mu     sync.Mutex
	asked  []string
	refuse bool
	err    error
}

func (v *stubVirtual) Describe() connectors.Connector {
	return connectors.Connector{Name: "github", Platform: "github-app", Hosts: v.hosts,
		Scheme: connectors.SchemeBasic, Username: "x-access-token"}
}
func (v *stubVirtual) Credential(_ context.Context, c connectors.Caller, host, rest string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.asked = append(v.asked, c.Repo+" "+host+"/"+rest)
	if v.refuse {
		return "", errors.Join(connectors.ErrTargetRefused, errors.New("not this agent's repository"))
	}
	if v.err != nil {
		return "", v.err
	}
	return "virtual-minted-token-0123456789", nil
}
func (v *stubVirtual) Problem(int64) string { return "" }

// listingStub is a stored connector store holding one row named github and one
// named svc.
type listingStub struct {
	connectors.Store
	touched []string
}

func (listingStub) ListConnectors(context.Context) ([]connectors.Connector, error) {
	return []connectors.Connector{{Name: "github", Platform: "github"}, {Name: "svc", Platform: "custom"}}, nil
}
func (listingStub) GetConnector(_ context.Context, name string) (connectors.Connector, error) {
	return connectors.Connector{Name: name, Platform: "stored"}, nil
}
func (listingStub) CreateConnector(_ context.Context, nc connectors.NewConnector) (connectors.Connector, error) {
	return connectors.Connector{Name: nc.Name}, nil
}
func (listingStub) RotateSecret(context.Context, string, string) error { return nil }
func (listingStub) DeleteConnector(context.Context, string) error      { return nil }

// TestTheVirtualConnectorShadowsTheStoredRowOfItsName: every name-keyed reader
// sees the virtual github; the stored row of that name is hidden, and the
// operator writes to it are refused; every other name passes through.
func TestTheVirtualConnectorShadowsTheStoredRowOfItsName(t *testing.T) {
	ctx := context.Background()
	v := &stubVirtual{hosts: []string{"api.github.com", "github.com"}}
	s := connectors.WithVirtual(listingStub{}, v)

	list, err := s.ListConnectors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "github" || list[0].Platform != "github-app" || list[1].Name != "svc" {
		t.Fatalf("list = %+v, want the virtual github (once) and svc", list)
	}
	if c, _ := s.GetConnector(ctx, "github"); c.Platform != "github-app" {
		t.Fatalf("GetConnector(github) = %+v, want the virtual one", c)
	}
	if c, _ := s.GetConnector(ctx, "svc"); c.Platform != "stored" {
		t.Fatalf("GetConnector(svc) = %+v, want the stored one", c)
	}
	for name, err := range map[string]error{
		"create": func() error { _, err := s.CreateConnector(ctx, connectors.NewConnector{Name: "github"}); return err }(),
		"rotate": s.RotateSecret(ctx, "github", "whatever-secret"),
		"delete": s.DeleteConnector(ctx, "github"),
	} {
		if !errors.Is(err, connectors.ErrManaged) || !errors.Is(err, connectors.ErrInvalid) {
			t.Errorf("%s github: err = %v, want ErrManaged (an ErrInvalid)", name, err)
		}
	}
	if _, err := s.CreateConnector(ctx, connectors.NewConnector{Name: "svc2"}); err != nil {
		t.Errorf("control: another name is refused: %v", err)
	}
	if _, _, err := s.Credential(ctx, "github"); err == nil {
		t.Error("Credential(github) returned a stored secret for the virtual name")
	}
	if got, ok := connectors.VirtualOf(s, "github"); !ok || got != v {
		t.Error("VirtualOf(github) did not find it")
	}
	if _, ok := connectors.VirtualOf(s, "svc"); ok {
		t.Error("VirtualOf(svc) found a virtual connector")
	}
	if connectors.WithVirtual(listingStub{}, nil) == nil {
		t.Error("WithVirtual(nil) dropped the store")
	}
}

// TestTheProxyAsksTheVirtualConnectorOnlyForAnAllowlistedHost, and maps its
// refusal to 403 and its failure to 502 with the reason.
func TestTheProxyAsksTheVirtualConnectorOnlyForAnAllowlistedHost(t *testing.T) {
	up := newUpstream(t)
	host := "api.fake.test:" + up.port()
	v := &stubVirtual{hosts: []string{host}}
	p := &connectors.Proxy{Store: connectors.WithVirtual(approvedStub{}, v), Policy: testPolicy()}
	do := func(h, rest string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/connect/github/"+h+"/"+rest, nil)
		p.Serve(w, r, connectors.Caller{AgentID: 1, AgentName: "a", Repo: "example-org/widget"}, "github", h, rest)
		return w
	}
	if w := do("evil.fake.test", "x"); w.Code != http.StatusForbidden || len(v.asked) != 0 {
		t.Fatalf("an off-allowlist host: %d, credential asked %d time(s)", w.Code, len(v.asked))
	}
	w := do(host, "repos/example-org/widget")
	if w.Code != http.StatusOK || up.last(t).Header.Get("Authorization") == "" {
		t.Fatalf("own repo: %d %s", w.Code, w.Body)
	}
	if v.asked[0] != "example-org/widget "+host+"/repos/example-org/widget" {
		t.Fatalf("the virtual connector was asked %q; the caller's repo and the target must reach it", v.asked[0])
	}
	v.refuse = true
	if w := do(host, "repos/other/x"); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "not this agent's repository") {
		t.Fatalf("a refused target: %d %s", w.Code, w.Body)
	}
	v.refuse, v.err = false, errors.New("GitHub refused the GitHub App's credentials (401)")
	if w := do(host, "repos/example-org/widget"); w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "(401)") {
		t.Fatalf("a mint failure: %d %s", w.Code, w.Body)
	}
}
