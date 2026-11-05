package connectors_test

import (
	"context"
	"testing"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/db"
)

// TestAKubeAuditRowIsStoredAndSurvivesRLSOn: officer_kube_read's audit row
// (kind `kube`, migration 0084) is accepted by connector_audit's kind CHECK,
// and the RLS re-apply (db.RLSOn) still succeeds with one present — it used
// to re-apply 0075, whose CHECK lists only the kinds before `kube` and would
// refuse the row (the latent trap 0075's rollback note names).
func TestAKubeAuditRowIsStoredAndSurvivesRLSOn(t *testing.T) {
	ctx, pool := testPool(t)
	s := newStore(t, pool, key(7))
	e := connectors.AuditEntry{Kind: "kube", AgentName: "officer-anomaly-prod-a-1", Connector: connectors.KubeReadName,
		Method: "list", Host: "prod-a", Path: "/api/v1/namespaces/web/pods", Status: 200,
		Detail: `{"officer":"anomaly:prod-a","item":"41","jti":"jti-1","count":3,"bytes":512,"ms":12}`}
	id, err := connectors.AuditWithID(ctx, connectors.WithVirtual(s, connectors.KubeReadVirtual(), allowAll), e)
	if err != nil || id <= 0 {
		t.Fatalf("a kube audit row: id %d, err %v; want it stored with a positive id", id, err)
	}
	got, err := s.ListAudit(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != id || got[0].Kind != "kube" || got[0].Path != e.Path || got[0].Detail != e.Detail {
		t.Fatalf("audit rows %+v, want the one kube row", got)
	}
	if err := db.RLSOff(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := db.RLSOn(context.Background(), pool); err != nil {
		t.Fatalf("RLSOn with a kube audit row present: %v", err)
	}
}
