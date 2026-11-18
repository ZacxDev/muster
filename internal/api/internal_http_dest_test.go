package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
)

// reconfigure points f's connector at base (path prefix /v1/, the same token),
// as a restart with a changed MUSTER_INTERNAL_HTTP_CONNECTORS would, over the
// SAME grant store.
func (f *ihToolFixture) reconfigure(t *testing.T, base string) *connectors.InternalHTTP {
	t.Helper()
	ih, err := connectors.NewInternalHTTP(connectors.InternalHTTPSettings{Name: "app-driver", BaseURL: base}, ihToolToken)
	if err != nil {
		t.Fatal(err)
	}
	store := connectors.WithVirtual(f.gs, ih, authz.IsAdmin)
	f.s.ext.Connectors = store
	f.s.connectProxy = &connectors.Proxy{Store: store}
	return ih
}

// TestAnInternalHTTPApprovalIsForTheConfiguredDestination: the owner's
// approval records the connector's destination, not the agent's repository;
// after a restart that points the connector at another service the approval
// no longer serves — the call is refused, the grant re-opened and NEITHER
// service receives anything — while a restart with the same configuration
// leaves it approved.
func TestAnInternalHTTPApprovalIsForTheConfiguredDestination(t *testing.T) {
	f := newIHToolFixture(t, connectors.StatusPending)
	f.gs.repo = ""
	if _, _, err := f.s.decideConnectorGrant(f.ctx, 1, true, connectors.DecisionSource{}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	approvedFor := f.gs.repo
	if !strings.HasPrefix(approvedFor, "internal_http:") || approvedFor == f.a.Repo {
		t.Fatalf("the approval recorded %q, want the connector's destination (not the repository %q)", approvedFor, f.a.Repo)
	}
	if out := f.call(`{"connector":"app-driver","method":"GET","path":"/v1/x"}`); out["status"] != float64(200) {
		t.Fatalf("the approved call: %v", out)
	}

	// The same configuration after a restart: still approved.
	f.reconfigure(t, f.svcURL)
	if out := f.call(`{"connector":"app-driver","method":"GET","path":"/v1/x"}`); out["status"] != float64(200) {
		t.Fatalf("same config after a restart: %v, want still approved", out)
	}
	if f.gs.reopened != 0 {
		t.Fatalf("same config re-opened the grant %d times", f.gs.reopened)
	}

	// Another service under the same name: refused, re-opened, nothing sent.
	var otherHits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHits.Add(1)
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(other.Close)
	before := f.hits.Load()
	f.reconfigure(t, other.URL)
	out := f.call(`{"connector":"app-driver","method":"GET","path":"/v1/x"}`)
	if e, _ := out["error"].(string); !strings.Contains(e, "no approved grant") {
		t.Fatalf("base_url changed: %v, want the no-grant refusal", out)
	}
	if f.gs.reopened != 1 || f.gs.status != connectors.StatusPending {
		t.Fatalf("base_url changed: reopened %d, status %s, want re-opened once", f.gs.reopened, f.gs.status)
	}
	if otherHits.Load() != 0 || f.hits.Load() != before {
		t.Fatalf("a refused call reached a service (other %d, original +%d)", otherHits.Load(), f.hits.Load()-before)
	}
}

// TestTheGrantDescriptionNamesNoAddress: what connector_requests_list and
// connector_request_decide say an internal_http approval grants — read by the
// owner's mirrored session, a model — carries neither the service's host nor
// its port, while the Connectors tab's note (a human's page) still names it.
func TestTheGrantDescriptionNamesNoAddress(t *testing.T) {
	ih, err := connectors.NewInternalHTTP(connectors.InternalHTTPSettings{Name: "app-driver",
		BaseURL: "http://svc-a.example.test:8080", PathPrefix: "/v1/"}, ihToolToken)
	if err != nil {
		t.Fatal(err)
	}
	f := newIHToolFixture(t, connectors.StatusApproved)
	store := connectors.WithVirtual(f.gs, ih, authz.IsAdmin)
	f.s.ext.Connectors = store
	g := connectors.Grant{ID: 1, AgentID: f.a.ID, Connector: "app-driver", Status: connectors.StatusPending}
	desc, _ := f.s.grantsDescription(f.ctx, g, ih.Describe(), f.a.Repo, nil, false)
	if desc == "" || strings.Contains(desc, "svc-a") || strings.Contains(desc, "8080") || !strings.Contains(desc, "/v1/") {
		t.Fatalf("grants description %q: want the prefix and no address", desc)
	}
	if note := internalHTTPManagedNote(ih); !strings.Contains(note, "http://svc-a.example.test:8080") {
		t.Fatalf("positive control: the Connectors tab note %q no longer names the service", note)
	}
}
