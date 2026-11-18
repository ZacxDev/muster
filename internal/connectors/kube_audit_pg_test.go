package connectors_test

import (
	"testing"

	"github.com/ZacxDev/muster/internal/connectors"
)

// TestAKubeReadAuditRowIsAQueryRow: officer_kube_read's audit row is kind
// `query`, as postgres_query's is (connector_audit's kind CHECK needs no new
// kind; the connector column, kube-read, is what tells the rows apart), and
// it is stored with a positive id the instance can cite.
func TestAKubeReadAuditRowIsAQueryRow(t *testing.T) {
	ctx, pool := testPool(t)
	s := newStore(t, pool, key(7))
	e := connectors.AuditEntry{Kind: "query", AgentName: "officer-anomaly-prod-a-1", Connector: connectors.KubeReadName,
		Method: "list", Host: "prod-a", Path: "/api/v1/namespaces/web/pods", Status: 200,
		Detail: `{"officer":"anomaly:prod-a","item":"41","jti":"jti-1","count":3,"bytes":512,"ms":12}`}
	id, err := connectors.AuditWithID(ctx, connectors.WithVirtual(s, connectors.KubeReadVirtual(), allowAll), e)
	if err != nil || id <= 0 {
		t.Fatalf("a kube-read audit row: id %d, err %v; want it stored with a positive id", id, err)
	}
	got, err := s.ListAudit(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != id || got[0].Kind != "query" || got[0].Connector != connectors.KubeReadName ||
		got[0].Path != e.Path || got[0].Detail != e.Detail {
		t.Fatalf("audit rows %+v, want the one kube-read query row", got)
	}
}
