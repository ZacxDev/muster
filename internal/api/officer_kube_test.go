package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// officer_kube_read IS FOR JUDGEMENT INSTANCES ONLY: a tiered officer with a
// `kube` block offers it to its judgement pool, and its routine instances —
// cheaper models working untriaged items — are neither offered it nor served
// when they call it. A plain agent and an officer without `kube` are refused
// the same way.
// ---------------------------------------------------------------------------

const (
	offK      = "anomaly:prod-a"
	offKTok   = "fixture-officer-token-kube-8f2c"
	offKJudge = "officer-anomaly-prod-a-"
	offKRout  = "officer-anomaly-prod-a-r"
)

func kubeRigCA(t *testing.T) string {
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

func newKubeOfficerRig(t *testing.T) *officerRig {
	t.Helper()
	ca := kubeRigCA(t)
	return newOfficerRigWith(t, map[string]string{offATok: offA, offKTok: offK}, func(hqURL string) []officers.Officer {
		raw := `[{"id":"` + offA + `","hq_url":"` + hqURL + `","max_instances":2},` +
			`{"id":"` + offK + `","hq_url":"` + hqURL + `","tiers":{"routine":{"model":"haiku","max_instances":2},"judgement":{"model":"opus","max_instances":1}},` +
			`"kube":{"clusters":[{"name":"prod-a","service_account":"officers/officer-anomaly-prod-a"}]}}]`
		list, err := officers.Parse(raw, func(k string) string {
			return map[string]string{
				officers.TokenEnvPrefix + officers.TokenEnvSuffix(offA): offATok,
				officers.TokenEnvPrefix + officers.TokenEnvSuffix(offK): offKTok,
				officers.KubeEndpointEnvPrefix + "PROD_A":               "192.0.2.9:6443",
				officers.KubeCAEnvPrefix + "PROD_A":                     ca,
				officers.KubeMinterEnvPrefix + "PROD_A":                 "fixture-minter-token-0c9e",
			}[k]
		}, "muster-agent-")
		if err != nil {
			t.Fatalf("parse the kube rig's officers: %v", err)
		}
		return list
	})
}

// TestOfficerKubeReadIsOfferedToJudgementInstancesOnly: the judgement
// instance is offered officer_kube_read (and its session pre-allows it); the
// routine instance, the other officer's instance and a plain agent are not
// offered it, and each is refused when it calls it anyway — before any grant
// is filed and before any cluster is dialled.
func TestOfficerKubeReadIsOfferedToJudgementInstancesOnly(t *testing.T) {
	r := newKubeOfficerRig(t)
	ctx := context.Background()
	judge := r.addNamed(offKJudge+"1", 971)
	routine := r.addNamed(offKRout+"1", 972)
	other := r.addNamed(officers.InstanceName(offA, 1), 973)
	plain := r.addNamed("brave-otter", 974)

	if !officerToolNameSet(r.s.MCPToolDefsFor(ctx, judge))[officers.ToolKubeRead] {
		t.Fatalf("the judgement instance is not offered %s", officers.ToolKubeRead)
	}
	sess, _ := r.s.OfficerClaudeCodeSession(judge)
	if !strings.Contains(","+strings.Join(sess.AllowMCPTools, ",")+",", ","+officers.ToolKubeRead+",") {
		t.Fatalf("the judgement instance's session does not pre-allow %s: %v", officers.ToolKubeRead, sess.AllowMCPTools)
	}
	args := `{"cluster":"prod-a","verb":"list","group":"","version":"v1","resource":"pods","namespace":"web"}`
	for _, a := range []agents.Agent{routine, other, plain} {
		if officerToolNameSet(r.s.MCPToolDefsFor(ctx, a))[officers.ToolKubeRead] {
			t.Errorf("%s is offered %s", a.Name, officers.ToolKubeRead)
		}
		out := r.s.dispatchAgentTool(ctx, a, officers.ToolKubeRead, args)
		if !isToolErr(out) {
			t.Errorf("%s calling %s: %s, want a refusal", a.Name, officers.ToolKubeRead, out)
		}
	}
	if !strings.Contains(r.s.dispatchAgentTool(ctx, routine, officers.ToolKubeRead, args), "not one of officer") {
		t.Errorf("the routine instance's refusal does not say the tool is not its officer's")
	}
}
