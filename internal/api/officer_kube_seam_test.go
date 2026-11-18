package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// officer_kube_read END TO END, through dispatch: the binding, the grant, the
// held item, the read against a TLS API server with its own CA, and the audit
// row — the seam between internal/api and internal/kuberead that neither
// package's own tests build.
// ---------------------------------------------------------------------------

// kubeConnStub is the connector store the seam needs: one grant (status set by
// the test) and the audit rows written.
type kubeConnStub struct {
	connectors.Store
	mu     sync.Mutex
	status string
	audit  []connectors.AuditEntry
}

func (s *kubeConnStub) LatestGrant(_ context.Context, agentID int64, connector string) (connectors.Grant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return connectors.Grant{ID: 5, AgentID: agentID, Connector: connector, Status: s.status}, nil
}

func (s *kubeConnStub) Audit(_ context.Context, e connectors.AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = append(s.audit, e)
	return nil
}

// AuditID is Audit returning the row's id (1, 2, …), as PGStore's does.
func (s *kubeConnStub) AuditID(ctx context.Context, e connectors.AuditEntry) (int64, error) {
	_ = s.Audit(ctx, e)
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.audit)), nil
}

func (s *kubeConnStub) rows() []connectors.AuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]connectors.AuditEntry(nil), s.audit...)
}

// seamAPI is a TLS API server whose certificate names only
// kubernetes.default.svc, signed by its own CA: it mints a JWT-shaped token
// with a jti on the token route and answers one ConfigMap list carrying
// canary to a minted token.
type seamAPI struct {
	srv    *httptest.Server
	caPEM  string
	mu     sync.Mutex
	mints  int
	reads  []string
	tokens map[string]bool
}

func newSeamAPI(t *testing.T, canary string) *seamAPI {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture-ca"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"kubernetes.default.svc"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(leafKey)
	leaf, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	a := &seamAPI{caPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})), tokens: map[string]bool{}}
	a.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/token") {
			a.mints++
			enc := base64.RawURLEncoding.EncodeToString
			tok := enc([]byte(`{"alg":"RS256"}`)) + "." + enc([]byte(fmt.Sprintf(`{"jti":"seam-jti-%d"}`, a.mints))) + "." + enc([]byte("sig"))
			a.tokens[tok] = true
			_ = json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{"token": tok}})
			return
		}
		a.reads = append(a.reads, r.URL.Path)
		if !a.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")] {
			http.Error(w, `{"message":"not minted"}`, http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": "ConfigMapList", "items": []any{
			map[string]any{"metadata": map[string]any{"name": "app-cfg"}, "data": map[string]any{"k": canary}}}})
	}))
	a.srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	a.srv.StartTLS()
	t.Cleanup(a.srv.Close)
	return a
}

func (a *seamAPI) counts() (int, int) { a.mu.Lock(); defer a.mu.Unlock(); return a.mints, len(a.reads) }

// TestOfficerKubeReadEndToEnd: a judgement instance is refused while its
// kube-read grant is pending and while it holds no item — reaching no cluster
// either time — and, approved and holding an item, reads a ConfigMap list
// through a freshly minted token. The call's audit row is kind `query` (as
// postgres_query's), its connector kube-read, and
// names the officer, the item, the cluster, the path and the jti, and carries
// nothing of the answer.
func TestOfficerKubeReadEndToEnd(t *testing.T) {
	const canary = "seam-answer-canary-a51e"
	api := newSeamAPI(t, canary)
	hq := hqtest.New(map[string]string{offKTok: offK})
	t.Cleanup(hq.Close)
	store := &officerAgents{}
	prov := &officerProv{store: store, phase: map[string]provision.Phase{}}
	conn := &kubeConnStub{status: connectors.StatusPending}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
	s.UseExtensions(Extensions{Agents: store, Provisioner: prov, Gateway: &repromptChat{}, SessionLiveness: stubLiveness{}, Kinds: stubKinds{pick: "acct-one"},
		Connectors: conn})
	raw := `[{"id":"` + offK + `","hq_url":"` + hq.BaseURL() + `","tiers":{"routine":{"model":"haiku"},"judgement":{"model":"opus"}},` +
		`"kube":{"clusters":[{"name":"prod-a","service_account":"officers/officer-anomaly-prod-a"}]}}]`
	list, err := officers.Parse(raw, func(k string) string {
		return map[string]string{
			officers.TokenEnvPrefix + officers.TokenEnvSuffix(offK): offKTok,
			officers.KubeEndpointEnvPrefix + "PROD_A":               api.srv.Listener.Addr().String(),
			officers.KubeCAEnvPrefix + "PROD_A":                     api.caPEM,
			officers.KubeMinterEnvPrefix + "PROD_A":                 "fixture-minter-token-5d0a",
			"MUSTER_KUBE_READ_ALLOWLIST_PROD_A":                     kubeRigAllowlist,
		}[k]
	}, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnableOfficers(OfficersConfig{Officers: list, InstallSession: discardSession, NoShipResume: true}); err != nil {
		t.Fatal(err)
	}
	judge := agents.Agent{ID: 981, Name: offKJudge + "1", Kind: agents.KindClaudeCode, Status: agents.StatusRunning, OwnerID: 1, Namespace: "ns"}
	store.mu.Lock()
	store.rows = append(store.rows, judge)
	store.mu.Unlock()
	ctx := context.Background()
	args := `{"cluster":"prod-a","verb":"list","group":"","version":"v1","resource":"configmaps","namespace":"web"}`

	if out := s.dispatchAgentTool(ctx, judge, officers.ToolKubeRead, args); !isToolErr(out) || !strings.Contains(out, "no approved grant") {
		t.Fatalf("a pending grant: %s, want a refusal naming the grant", out)
	}
	conn.mu.Lock()
	conn.status = connectors.StatusApproved
	conn.mu.Unlock()
	if out := s.dispatchAgentTool(ctx, judge, officers.ToolKubeRead, args); !isToolErr(out) || !strings.Contains(out, "no claimed item") {
		t.Fatalf("no item held: %s, want a refusal naming the item", out)
	}
	if m, r := api.counts(); m+r != 0 || len(conn.rows()) != 0 {
		t.Fatalf("a refused call reached the cluster (%d mints, %d reads) or wrote %d audit rows", m, r, len(conn.rows()))
	}

	item := hq.AddTieredItem(offK, "anomaly.alert", "node-a", officers.TierJudgement)
	hq.SetClaim(item, judge.Name, time.Now().Add(10*time.Minute))
	out := s.dispatchAgentTool(ctx, judge, officers.ToolKubeRead, args)
	if isToolErr(out) || !strings.Contains(out, canary) || !strings.Contains(out, "seam-jti-1") || !strings.Contains(out, `"audit_id":1`) {
		t.Fatalf("an approved instance holding an item: %s, want the list, its jti and audit_id 1 (the row's id, to cite)", out)
	}
	if m, r := api.counts(); m != 1 || r != 1 {
		t.Fatalf("%d mints and %d reads, want 1 and 1", m, r)
	}
	rows := conn.rows()
	if len(rows) != 1 {
		t.Fatalf("%d audit rows, want 1: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.Kind != "query" || row.AgentName != judge.Name || row.Connector != officers.KubeConnector || row.Host != "prod-a" ||
		row.Method != "list" || row.Path != "/api/v1/namespaces/web/configmaps" || row.Status != 200 {
		t.Errorf("audit row %+v", row)
	}
	for _, want := range []string{`"officer":"` + offK + `"`, `"item":"` + item + `"`, `"jti":"seam-jti-1"`, `"count":1`} {
		if !strings.Contains(row.Detail, want) {
			t.Errorf("audit detail %s lacks %s", row.Detail, want)
		}
	}
	b, _ := json.Marshal(row)
	if strings.Contains(string(b), canary) {
		t.Fatalf("the audit row carries the answer: %s", b)
	}
}
