package api

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers"
)

// TestAnMCPApprovalOfKubeReadNeverSaysTheGitHubAppGrantsNothing: a tiered
// officer with a `kube` block and NO `repo` files a kube-read request from its
// judgement instance, and a mirrored session approves it over MCP. kube-read
// is a virtual connector, so the grant is recorded with source 'app' — but it
// is not the GitHub App, and its approval IS usable (officer_kube_read does
// not need a repository). So the decision says what it grants (the
// kube-read description, as the listing does) and carries no GitHub App
// "approving grants NOTHING usable" warning.
//
// CONTROL: the listing, which already branched on kube-read
// (grantsDescription), describes the same request the same way.
func TestAnMCPApprovalOfKubeReadNeverSaysTheGitHubAppGrantsNothing(t *testing.T) {
	f := newConnFixture(t)
	ca := kubeRigCA(t)
	raw := `[{"id":"` + offK + `","hq_url":"https://hq.example.invalid/api/v1",` +
		`"tiers":{"routine":{"model":"haiku","max_instances":1},"judgement":{"model":"opus","max_instances":1}},` +
		`"kube":{"clusters":[{"name":"prod-a","service_account":"officers/officer-anomaly-prod-a"}]}}]`
	list, err := officers.Parse(raw, func(k string) string {
		return map[string]string{
			officers.TokenEnvPrefix + officers.TokenEnvSuffix(offK): offKTok,
			officers.KubeEndpointEnvPrefix + "PROD_A":               "192.0.2.9:6443",
			officers.KubeCAEnvPrefix + "PROD_A":                     ca,
			officers.KubeMinterEnvPrefix + "PROD_A":                 "fixture-minter-token-3e1b",
			"MUSTER_KUBE_READ_ALLOWLIST_PROD_A":                     kubeRigAllowlist,
		}[k]
	}, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	judge := f.seedAgent(t, offKJudge+"1", "kube-judge-agent-token-0123456789ab")

	ws := mirrorTok('a')
	s := New(nil, AuthConfig{HookToken: connTestHookToken, ServiceToken: connTestServiceToken, UIPassword: connTestPassword},
		log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Agents: agents.NewPG(f.pool), Connectors: connectors.WithVirtual(f.store, connectors.KubeReadVirtual(), authz.IsAdmin),
		ConnectorPolicy: fakeTestPolicy(), Provisioner: stubProvisioner{}, Kinds: stubKinds{pick: "acct-one"},
		Mirror:       &fakeMirror{online: true, interactive: true, statusReport: true, v2: &fakeMirrorV2{}, windows: attnWindows(mirror.StatusWaiting)},
		MirrorTokens: fakeMirrorTokens{ws: mirror.Token{ID: 1, OwnerID: 1, Host: "ws"}}})
	if _, err := s.EnableOfficers(OfficersConfig{Officers: list, InstallSession: discardSession, NoShipResume: true}); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()

	g, _, err := f.store.RequestGrant(f.ctx, judge, connectors.KubeReadName, "read the alerting pod's log")
	if err != nil {
		t.Fatal(err)
	}
	call := func(name string, args map[string]any) (string, bool) {
		t.Helper()
		w := serveCall(t, h, http.MethodPost, "/mirror/mcp", strings.NewReader(mcpToolCall(name, args)),
			map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + ws, mirrorSessionHeader: focusSID})
		var r mcpReply
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &r) != nil || r.Error != nil {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body)
		}
		return toolText(t, &r)
	}
	const kubeGrants = "officer_kube_read for this instance only"

	text, isErr := call("connector_requests_list", map[string]any{})
	if isErr || !strings.Contains(text, kubeGrants) || strings.Contains(text, "NOTHING usable") {
		t.Fatalf("control, connector_requests_list: %s", text)
	}

	text, isErr = call("connector_request_decide", map[string]any{"id": g.ID, "decision": "approve"})
	if isErr || !strings.Contains(text, `"status":"approved"`) {
		t.Fatalf("approve %d: %s", g.ID, text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("approve %d: %s: %v", g.ID, text, err)
	}
	if w, ok := out["warning"]; ok {
		t.Errorf("approving kube-read for an officer with no repo warns %q", w)
	}
	if gr, _ := out["grants"].(string); !strings.HasPrefix(gr, kubeGrants) {
		t.Errorf("the decision's grants = %q, want the kube-read description", out["grants"])
	}
	var src string
	if err := f.pool.QueryRow(authz.System(f.ctx), `SELECT source FROM connector_grants WHERE id = $1`, g.ID).Scan(&src); err != nil ||
		src != connectors.SourceApp {
		t.Fatalf("grant %d: source %q %v, want %q (a virtual connector's)", g.ID, src, err, connectors.SourceApp)
	}
}
