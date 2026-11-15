package connectors_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/connectors"
)

// TestAnInternalHTTPGrantIsForOneDestination: an approval records the
// connector's destination (GrantScope), and after a restart that changes the
// base URL or the path prefix ReconcileGrant re-opens it; the same
// configuration (a fresh connector, as after any restart) leaves it approved,
// and so does a token rotation. A grant recorded with a repository instead —
// any approval made before destinations were recorded — fails closed.
func TestAnInternalHTTPGrantIsForOneDestination(t *testing.T) {
	ctx := context.Background()
	mk := func(base, prefix, token string) *connectors.InternalHTTP {
		t.Helper()
		ih, err := connectors.NewInternalHTTP(connectors.InternalHTTPSettings{Name: "app-driver", BaseURL: base,
			PathPrefix: prefix}, token)
		if err != nil {
			t.Fatal(err)
		}
		return ih
	}
	approvedOn := mk("http://svc-a.example.test:8080", "/v1/", ihToken)
	recorded := connectors.GrantScope(ctx, connectors.WithVirtual(&reopenStub{}, approvedOn, allowAll), "app-driver",
		"example-org/widget")
	if recorded != approvedOn.Destination() || !strings.HasPrefix(recorded, "internal_http:") {
		t.Fatalf("GrantScope recorded %q, want the connector's destination", recorded)
	}
	if strings.Contains(recorded, "svc-a") {
		t.Fatalf("the recorded destination carries the address: %q", recorded)
	}
	for _, c := range []struct {
		name     string
		now      *connectors.InternalHTTP
		recorded string
		reopen   bool
	}{
		{"same config after a restart", mk("http://svc-a.example.test:8080", "/v1/", ihToken), recorded, false},
		{"token rotated", mk("http://svc-a.example.test:8080", "/v1/", ihToken+"-rotated"), recorded, false},
		{"base_url host changed", mk("http://svc-b.example.test:8080", "/v1/", ihToken), recorded, true},
		{"base_url port changed", mk("http://svc-a.example.test:9090", "/v1/", ihToken), recorded, true},
		{"base_url scheme changed", mk("https://svc-a.example.test:8080", "/v1/", ihToken), recorded, true},
		{"path_prefix changed", mk("http://svc-a.example.test:8080", "/v2/", ihToken), recorded, true},
		{"recorded with a repository", mk("http://svc-a.example.test:8080", "/v1/", ihToken), "example-org/widget", true},
		{"recorded with nothing", mk("http://svc-a.example.test:8080", "/v1/", ihToken), "", true},
	} {
		base := &reopenStub{}
		s := connectors.WithVirtual(base, c.now, allowAll)
		g := connectors.Grant{ID: 31, Connector: "app-driver", Status: connectors.StatusApproved,
			Source: connectors.SourceApp, Repo: c.recorded}
		_, why, err := connectors.ReconcileGrant(ctx, s, g, "example-org/widget")
		if err != nil || (why != "") != c.reopen || len(base.reopened) != map[bool]int{true: 1, false: 0}[c.reopen] {
			t.Errorf("%s: reason %q err %v reopened %v, want reopen %v", c.name, why, err, base.reopened, c.reopen)
		}
		if c.reopen && why != connectors.ReopenedDestinationChanged {
			t.Errorf("%s: reason %q, want the destination reason", c.name, why)
		}
	}
}
