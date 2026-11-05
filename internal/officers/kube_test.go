package officers_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// THE `kube` BLOCK: which clusters an officer's JUDGEMENT instances may read
// with officer_kube_read, and as which ServiceAccount. Each cluster's endpoint,
// CA bundle, minter token and read allowlist come from the server's deployment
// environment (MUSTER_KUBE_READ_{ENDPOINT,CA,MINTER_TOKEN,ALLOWLIST}_<CLUSTER>),
// never from the officer declaration and never from the model.
// ---------------------------------------------------------------------------

func fixtureCAPEM(t *testing.T) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture-ca"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

const kubeBlock = `,"kube":{"clusters":[{"name":"prod-a","service_account":"officers/officer-gpu-health-prod-a"}]}`

// allowlistVar is the cluster's allowlist variable, spelled out (not built
// from a constant) so the test pins the name the deployment sets.
const allowlistVar = "MUSTER_KUBE_READ_ALLOWLIST_PROD_A"

// fixtureAllowlist is a made-up allowlist: no real deployment's resources.
const fixtureAllowlist = `[{"apiGroup":"","resources":["pods","pods/log","configmaps"],"scope":"namespaced"},` +
	`{"apiGroup":"","resources":["nodes"],"scope":"cluster"},` +
	`{"apiGroup":"widgets.example.com","resources":["widgets","gadgets"],"scope":"namespaced"}]`

func kubeEnv(t *testing.T) map[string]string {
	return map[string]string{
		officers.TokenEnvPrefix + officers.TokenEnvSuffix(tierID): tierTok,
		officers.KubeEndpointEnvPrefix + "PROD_A":                 "192.0.2.9:6443",
		officers.KubeCAEnvPrefix + "PROD_A":                       fixtureCAPEM(t),
		officers.KubeMinterEnvPrefix + "PROD_A":                   "fixture-minter-token-4e1a",
		allowlistVar:                                              fixtureAllowlist,
	}
}

func parseKube(t *testing.T, fields string, env0 map[string]string) ([]officers.Officer, error) {
	t.Helper()
	raw := `[{"id":"` + tierID + `","hq_url":"https://hq.example.com/api/v1"` + fields + `}]`
	return officers.Parse(raw, env(env0), "muster-agent-")
}

// TestTheKubeBlockGivesJudgementInstancesTheTool: with tiers and a kube
// block, the judgement pool is offered officer_kube_read and may request the
// kube-read connector; the routine pool is offered neither. The cluster's
// endpoint and minter token come from the environment.
func TestTheKubeBlockGivesJudgementInstancesTheTool(t *testing.T) {
	list, err := parseKube(t, ","+tiersBlock+kubeBlock, kubeEnv(t))
	if err != nil {
		t.Fatalf("a valid kube block was refused: %v", err)
	}
	judge, routine := pools(t, list[0])
	if !officers.ToolOffered(judge, officers.ToolKubeRead) || !officers.MayRequestConnector(judge, officers.KubeConnector) {
		t.Fatal("the judgement pool is not offered officer_kube_read and the kube-read connector")
	}
	if officers.ToolOffered(routine, officers.ToolKubeRead) || officers.MayRequestConnector(routine, officers.KubeConnector) {
		t.Fatal("the ROUTINE pool is offered officer_kube_read or the kube-read connector: judgement only")
	}
	cs := judge.KubeClusters()
	if len(cs) != 1 || cs[0].Name != "prod-a" || cs[0].Endpoint != "192.0.2.9:6443" || cs[0].ServiceAccount != "officers/officer-gpu-health-prod-a" ||
		cs[0].MinterToken() != "fixture-minter-token-4e1a" || len(cs[0].CA) == 0 {
		t.Fatalf("clusters %+v", cs)
	}
	if strings.Contains(list[0].String()+judge.GoString(), "fixture-minter-token-4e1a") {
		t.Fatal("the officer's description prints the minter token")
	}
	// No kube block: no tool, no connector.
	plain, err := parseKube(t, ","+tiersBlock, kubeEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	pj, _ := pools(t, plain[0])
	if officers.ToolOffered(pj, officers.ToolKubeRead) || officers.MayRequestConnector(pj, officers.KubeConnector) {
		t.Fatal("an officer without a kube block is offered officer_kube_read")
	}
}

// TestKubeBlockRefusals: each refusal names what is wrong, and a missing
// secret names the variable to set.
func TestKubeBlockRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, fields, drop, want string
	}{
		{"no tiers (judgement only)", kubeBlock, "", "tiers"},
		{"no endpoint", "," + tiersBlock + kubeBlock, officers.KubeEndpointEnvPrefix + "PROD_A", officers.KubeEndpointEnvPrefix + "PROD_A"},
		{"no CA", "," + tiersBlock + kubeBlock, officers.KubeCAEnvPrefix + "PROD_A", officers.KubeCAEnvPrefix + "PROD_A"},
		{"no minter token", "," + tiersBlock + kubeBlock, officers.KubeMinterEnvPrefix + "PROD_A", officers.KubeMinterEnvPrefix + "PROD_A"},
		{"a bad service account", "," + tiersBlock + `,"kube":{"clusters":[{"name":"prod-a","service_account":"officer-x"}]}`, "", "service_account"},
		{"a bad cluster name", "," + tiersBlock + `,"kube":{"clusters":[{"name":"Prod_A","service_account":"officers/x"}]}`, "", "name"},
		{"a cluster twice", "," + tiersBlock + `,"kube":{"clusters":[{"name":"prod-a","service_account":"officers/x"},{"name":"prod-a","service_account":"officers/y"}]}`, "", "twice"},
		{"no clusters", "," + tiersBlock + `,"kube":{"clusters":[]}`, "", "clusters"},
		{"an unknown key", "," + tiersBlock + `,"kube":{"clusters":[{"name":"prod-a","service_account":"officers/x","endpoint":"192.0.2.1:443"}]}`, "", "endpoint"},
		{"kube-read listed as a connector", "," + tiersBlock + kubeBlock + `,"connectors":["kube-read"]`, "", "kube-read"},
	} {
		e := kubeEnv(t)
		if tc.drop != "" {
			delete(e, tc.drop)
		}
		_, err := parseKube(t, tc.fields, e)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want a refusal containing %q", tc.name, err, tc.want)
		}
	}
	bad := kubeEnv(t)
	bad[officers.KubeCAEnvPrefix+"PROD_A"] = "not a certificate"
	if _, err := parseKube(t, ","+tiersBlock+kubeBlock, bad); err == nil || !strings.Contains(err.Error(), "CA") {
		t.Errorf("a CA that is not PEM: err = %v", err)
	}
	badEP := kubeEnv(t)
	badEP[officers.KubeEndpointEnvPrefix+"PROD_A"] = "https://192.0.2.9:6443/api"
	if _, err := parseKube(t, ","+tiersBlock+kubeBlock, badEP); err == nil || !strings.Contains(err.Error(), "host:port") {
		t.Errorf("an endpoint that is a URL: err = %v", err)
	}
}

// TestAMissingOrEmptyAllowlistDisablesTheCluster: the deployment supplies
// each cluster's read allowlist. With none (unset, blank, or an empty list)
// that cluster's kube-read is DISABLED — fail closed, never "allow all": the
// officer still loads, but the cluster is not offered, and with no other
// cluster neither is the tool nor the kube-read connector.
func TestAMissingOrEmptyAllowlistDisablesTheCluster(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value *string
	}{
		{"unset", nil},
		{"blank", ptr("  ")},
		{"an empty list", ptr("[]")},
		{"null", ptr("null")},
	} {
		e := kubeEnv(t)
		delete(e, allowlistVar)
		if tc.value != nil {
			e[allowlistVar] = *tc.value
		}
		list, err := parseKube(t, ","+tiersBlock+kubeBlock, e)
		if err != nil {
			t.Errorf("%s: the officer was refused (%v): a missing allowlist disables the cluster, it does not stop the officer", tc.name, err)
			continue
		}
		judge, _ := pools(t, list[0])
		if cs := judge.KubeClusters(); len(cs) != 0 {
			t.Errorf("%s: the cluster is still offered (%v): fail closed", tc.name, cs)
		}
		if officers.ToolOffered(judge, officers.ToolKubeRead) || officers.MayRequestConnector(judge, officers.KubeConnector) {
			t.Errorf("%s: officer_kube_read or the kube-read connector is still offered with no allowlist", tc.name)
		}
	}

	// Two clusters, one with an allowlist: only that one is offered.
	two := `,"kube":{"clusters":[{"name":"prod-a","service_account":"officers/x"},{"name":"prod-b","service_account":"officers/y"}]}`
	e := kubeEnv(t)
	e[officers.KubeEndpointEnvPrefix+"PROD_B"] = "192.0.2.10:6443"
	e[officers.KubeCAEnvPrefix+"PROD_B"] = fixtureCAPEM(t)
	e[officers.KubeMinterEnvPrefix+"PROD_B"] = "fixture-minter-token-b"
	list, err := parseKube(t, ","+tiersBlock+two, e)
	if err != nil {
		t.Fatal(err)
	}
	judge, _ := pools(t, list[0])
	cs := judge.KubeClusters()
	if len(cs) != 1 || cs[0].Name != "prod-a" {
		t.Fatalf("clusters %v, want only prod-a (prod-b has no allowlist)", cs)
	}
	if _, ok := judge.KubeCluster("prod-b"); ok {
		t.Fatal("prod-b, with no allowlist, is still a readable cluster")
	}
	if !officers.ToolOffered(judge, officers.ToolKubeRead) {
		t.Fatal("positive control: prod-a has an allowlist, and the tool is not offered")
	}
}

func ptr(s string) *string { return &s }

// TestAnAllowlistIsValidatedStrictly: a malformed allowlist is a load
// refusal naming the variable — an unknown field, a "*" resource or group,
// any subresource but core pods/log (*/status included), a never-granted
// resource, a missing or unknown scope, a duplicate.
func TestAnAllowlistIsValidatedStrictly(t *testing.T) {
	for _, tc := range []struct{ name, value, want string }{
		{"a wildcard resource", `[{"apiGroup":"widgets.example.com","resources":["*"],"scope":"namespaced"}]`, "*"},
		{"a wildcard among resources", `[{"apiGroup":"","resources":["pods","*"],"scope":"namespaced"}]`, "*"},
		{"a wildcard group", `[{"apiGroup":"*","resources":["widgets"],"scope":"namespaced"}]`, "*"},
		{"a status subresource", `[{"apiGroup":"apps","resources":["deployments/status"],"scope":"namespaced"}]`, "deployments/status"},
		{"a wildcard subresource", `[{"apiGroup":"apps","resources":["*/status"],"scope":"namespaced"}]`, "*/status"},
		{"pods/status", `[{"apiGroup":"","resources":["pods/status"],"scope":"namespaced"}]`, "pods/status"},
		{"pods/log outside core", `[{"apiGroup":"widgets.example.com","resources":["pods/log"],"scope":"namespaced"}]`, "pods/log"},
		{"pods/exec", `[{"apiGroup":"","resources":["pods/exec"],"scope":"namespaced"}]`, "pods/exec"},
		{"secrets", `[{"apiGroup":"","resources":["secrets"],"scope":"namespaced"}]`, "secrets"},
		{"rbac", `[{"apiGroup":"rbac.authorization.k8s.io","resources":["clusterroles"],"scope":"cluster"}]`, "rbac"},
		{"an unknown field", `[{"apiGroup":"","resources":["pods"],"scope":"namespaced","verbs":["get","list","watch"]}]`, "verbs"},
		{"no scope", `[{"apiGroup":"","resources":["pods"]}]`, "scope"},
		{"an unknown scope", `[{"apiGroup":"","resources":["pods"],"scope":"global"}]`, "scope"},
		{"no resources", `[{"apiGroup":"","resources":[],"scope":"namespaced"}]`, "resources"},
		{"a duplicate", `[{"apiGroup":"","resources":["pods"],"scope":"namespaced"},{"apiGroup":"","resources":["pods"],"scope":"namespaced"}]`, "twice"},
		{"not a list", `{"resources":[]}`, allowlistVar},
		{"not JSON", `pods,nodes`, allowlistVar},
	} {
		e := kubeEnv(t)
		e[allowlistVar] = tc.value
		_, err := parseKube(t, ","+tiersBlock+kubeBlock, e)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), allowlistVar) {
			t.Errorf("%s: err = %v, want a refusal naming %s and containing %q", tc.name, err, allowlistVar, tc.want)
		}
	}
	// Positive control: the fixture itself loads.
	if _, err := parseKube(t, ","+tiersBlock+kubeBlock, kubeEnv(t)); err != nil {
		t.Fatalf("positive control: the fixture allowlist was refused: %v", err)
	}
}

// TestTheReadBudgetIsTwoHundredPerItem: the 201st read of one item is
// refused with ErrReadBudget, naming the cap; another item and another
// officer have their own counts.
func TestTheReadBudgetIsTwoHundredPerItem(t *testing.T) {
	b := officers.NewReadBudget(officers.ReadsPerItem, time.Now)
	for i := 0; i < 200; i++ {
		if _, err := b.Spend("anomaly:prod-a", "41"); err != nil {
			t.Fatalf("read %d of 200 refused: %v", i+1, err)
		}
	}
	_, err := b.Spend("anomaly:prod-a", "41")
	if !errors.Is(err, officers.ErrReadBudget) || !strings.Contains(err.Error(), "200") {
		t.Fatalf("the 201st read: err %v, want ErrReadBudget naming 200", err)
	}
	if _, err := b.Spend("anomaly:prod-a", "42"); err != nil {
		t.Fatalf("another item was refused: %v", err)
	}
	if _, err := b.Spend("fleet-health:fleet", "41"); err != nil {
		t.Fatalf("another officer's item of the same id was refused: %v", err)
	}
}
