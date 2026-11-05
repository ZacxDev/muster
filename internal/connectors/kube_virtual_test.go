package connectors_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/connectors"
)

// ---------------------------------------------------------------------------
// THE kube-read CONNECTOR: a virtual connector that exists so an officer
// instance's request for it can be FILED AND APPROVED like any other connector
// grant, and that the HTTP proxy never serves — officer_kube_read, run by
// muster-server itself, is its only consumer.
// ---------------------------------------------------------------------------

// TestTwoVirtualConnectorsAreBothFound: wrapped over the GitHub App's virtual
// connector, kube-read is found by name AND the App's github still is —
// VirtualOf must look past the outer wrapper, or wiring kube-read would make
// the App vanish from the proxy and every approval card.
func TestTwoVirtualConnectorsAreBothFound(t *testing.T) {
	gh := &stubVirtual{hosts: []string{"api.github.com"}}
	s := connectors.WithVirtual(connectors.WithVirtual(listingStub{}, gh, allowAll), connectors.KubeReadVirtual(), allowAll)
	ctx := context.Background()
	if v, ok := connectors.VirtualOf(ctx, s, "github"); !ok || v != connectors.Virtual(gh) {
		t.Fatalf("the App's github is not found under kube-read's wrapper: %v %v", v, ok)
	}
	if _, ok := connectors.VirtualOf(ctx, s, connectors.KubeReadName); !ok {
		t.Fatal("kube-read is not a virtual connector")
	}
	if _, ok := connectors.VirtualOf(ctx, s, "svc"); ok {
		t.Fatal("a stored connector reads as virtual")
	}
	c, err := s.GetConnector(ctx, connectors.KubeReadName)
	if err != nil || c.Scheme != connectors.SchemeKubeRead || c.Name != connectors.KubeReadName {
		t.Fatalf("GetConnector(kube-read) = %+v, %v; want the kube-read scheme", c, err)
	}
	if c, _ := s.GetConnector(ctx, "github"); c.Platform != "github-app" {
		t.Fatalf("github through both wrappers is %+v, want the App's", c)
	}
}

// TestTheProxyNeverServesKubeRead: an APPROVED agent's /connect/kube-read/…
// is refused and nothing reaches any upstream: the connector has no HTTP
// face.
func TestTheProxyNeverServesKubeRead(t *testing.T) {
	up := newUpstream(t)
	host := "api.fake.test:" + up.port()
	p := &connectors.Proxy{Store: connectors.WithVirtual(approvedStub{}, connectors.KubeReadVirtual(), allowAll), Policy: testPolicy()}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/connect/kube-read/"+host+"/api/v1/secrets", nil)
	p.Serve(w, r, connectors.Caller{AgentID: 1, AgentName: "a"}, connectors.KubeReadName, host, "api/v1/secrets")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "officer_kube_read") {
		t.Fatalf("/connect/kube-read: %d %s, want 403 naming officer_kube_read", w.Code, w.Body)
	}
	if up.hits.Load() != 0 {
		t.Fatalf("a kube-read request reached an upstream (%d)", up.hits.Load())
	}
}
