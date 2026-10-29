package agentspec

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// TestAShippingOfficersPodSpecCarriesNoAppKeyOrWebhook (round-1 G3) builds a
// SHIPPING officer instance's pod spec as the runtime does — its kickoff text
// and its session (every tool allowed, the gated tools pre-allowed) — and
// asserts that neither of the officer's shipping secrets, the GitHub App's
// private key nor the escalation webhook, nor its HQ token, appears anywhere
// in it.
//
// ⚠ AN INVARIANT GUARD, NOT REGRESSION COVERAGE, mirroring
// TestAnOfficerInstancesPodSpecCarriesNoOfficerToken: nothing on the spec path
// was ever handed these secrets, so no prior code fails it. It pins the
// property for the change that would route one into the pod. The positive
// control proves the scan sees a credential the spec does carry.
func TestAShippingOfficersPodSpecCarriesNoAppKeyOrWebhook(t *testing.T) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
	const (
		tok  = "fixture-officer-token-shipscan-71c2"
		hook = "https://hooks.example.test/escalate/fixture-shipscan-9e0d"
	)
	env := map[string]string{
		"MUSTER_OFFICER_TOKEN_ENGINEERING_EXAMPLE_WEB":            tok,
		"MUSTER_OFFICER_GITHUB_APP_KEY_ENGINEERING_EXAMPLE_WEB":   keyPEM,
		"MUSTER_OFFICER_ESCALATE_WEBHOOK_ENGINEERING_EXAMPLE_WEB": hook,
	}
	raw := `[{"id":"engineering:example-web","hq_url":"https://hq.example.com/api/v1","repo":"example-org/example-web",
	  "ship":{"stage":"release","github_app":{"id":9001,"account":"example-org:4242"},"required_checks":["ci / build"],
	  "review_check":"pr-review","rollout":{"health_url":"https://svc.example.test/healthz","rollback":"pr"}}}]`
	list, err := officers.Parse(raw, func(k string) string { return env[k] }, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	o := list[0]
	if o.Ship.AppKey() == nil || o.Ship.EscalateWebhook() != hook {
		t.Fatal("the fixture officer does not hold the secrets this test looks for")
	}
	a := ccAgent()
	a.Name = officers.InstanceName(o.ID, 1)
	a.NoteText = officers.KickoffText(o, 1)
	a.PendingNote = a.NoteText
	var tools []string
	for _, n := range []string{officers.ToolBrief, officers.ToolItems, officers.ToolClaim, officers.ToolHeartbeat, officers.ToolComplete,
		officers.ToolRelease, officers.ToolMergePR, officers.ToolTagRelease, officers.ToolRolloutStatus} {
		if officers.ToolOffered(o, n) {
			tools = append(tools, n)
		}
	}
	spec := mustBuild(t, a, ccConfig(), Options{ClaudeCodeSession: ClaudeCodeSession{AllowMCPTools: tools, BypassPermissions: true}})
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var files strings.Builder
	for _, f := range spec.Files {
		files.Write(f.Content)
	}
	if spec.Sidecar != nil {
		for _, f := range spec.Sidecar.Files {
			files.Write(f.Content)
		}
	}
	haystack := string(b) + files.String() + fmt.Sprintf("%+v", spec)
	if !strings.Contains(haystack, ccTokenBeta) {
		t.Fatal("positive control failed: the scan does not see the account token the spec carries")
	}
	if !strings.Contains(haystack, officers.ToolMergePR) {
		t.Fatal("positive control failed: the spec is not a shipping officer's (no gated tool in it)")
	}
	body := strings.Join(strings.Split(keyPEM, "\n")[1:4], "")
	for name, secret := range map[string]string{"the App key": body, "the escalation webhook": hook, "the HQ token": tok} {
		if strings.Contains(haystack, secret) {
			t.Errorf("%s reached the shipping officer instance's pod spec", name)
		}
	}
}
