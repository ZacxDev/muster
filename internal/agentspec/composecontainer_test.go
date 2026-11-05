package agentspec

import (
	"errors"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/provision/compose"
)

// composeChiefAgent is the Compose chief's row: the reserved slug, claude-code,
// its fixed account ("chief", which may hold no token).
func composeChiefAgent() agents.Agent {
	a := ccAgent()
	a.Name = agents.ChiefName
	a.CCAccount = "chief"
	a.HooksToken = llmHookToken
	return a
}

// TestTheComposeContainerAsksForNoConfinementItCannotHave: a spec for the
// Compose container carries no network isolation and no resource limits — the
// compose driver can enforce neither and refuses a spec asking for them — and
// the compose driver accepts it. Control: the same build without
// ComposeContainer asks for both, and that driver refuses it.
func TestTheComposeContainerAsksForNoConfinementItCannotHave(t *testing.T) {
	cfg := ccConfig()
	cfg.ConnectURL, cfg.ConnectPort = llmConnectURL, 18191
	cfg.ClaudeCode.Accounts["chief"] = "sk-ant-oat01-FAKE-compose-chief-77e1"
	d, err := compose.New(compose.Config{Root: t.TempDir(), Admit: func(provision.Ref) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}

	cfg.ComposeContainer = true
	spec := mustBuild(t, composeChiefAgent(), cfg, Options{Instructions: "# muster supervisor\n"})
	if spec.Network.Isolate || len(spec.Network.PublicEgressTCPPorts) != 0 || len(spec.Network.ControllerEgressTCPPorts) != 0 {
		t.Fatalf("the Compose container's spec asks for network confinement: %+v", spec.Network)
	}
	if !spec.Resources.IsZero() {
		t.Fatalf("the Compose container's spec sets resource limits: %+v", spec.Resources)
	}
	if err := d.Create(t.Context(), spec); err != nil {
		t.Fatalf("the compose driver refused the Compose chief's own spec: %v", err)
	}

	cfg.ComposeContainer = false
	pod := mustBuild(t, composeChiefAgent(), cfg, Options{Instructions: "# muster supervisor\n"})
	if !pod.Network.Isolate || pod.Resources.IsZero() {
		t.Fatalf("control: a pod spec lost its confinement (network %+v, resources %+v)", pod.Network, pod.Resources)
	}
	if err := d.Create(t.Context(), pod); !errors.Is(err, provision.ErrUnsupported) {
		t.Fatalf("control: the compose driver accepted a pod spec asking for confinement: %v", err)
	}
}

// TestARoutedSessionNeedsNoTokenAndAnUnroutedOneDoes: the Compose chief on the
// OpenRouter key has its account but no token; on the LLM route that builds
// (no token placed), and without the route it is refused naming the account.
func TestARoutedSessionNeedsNoTokenAndAnUnroutedOneDoes(t *testing.T) {
	cfg := ccConfig()
	cfg.ConnectURL, cfg.ConnectPort = llmConnectURL, 18191
	cfg.ComposeContainer = true
	a := composeChiefAgent()
	if _, ok := cfg.ClaudeCode.Accounts[a.CCAccount]; ok {
		t.Fatal("fixture: the chief's account must hold no token here")
	}
	spec := mustBuild(t, a, cfg, Options{ClaudeCodeSession: ClaudeCodeSession{LLMRoute: true}})
	if v, ok := envValue(spec, EnvLLMRoute); !ok || v != LLMRouteMuster {
		t.Fatalf("CCD_LLM_ROUTE = %q (set %v)", v, ok)
	}
	if _, ok := secretValue(spec, EnvClaudeOAuthToken); ok {
		t.Fatal("a routed chief carries a subscription token")
	}
	_, err := Build(a, cfg, Options{})
	if err == nil || !strings.Contains(err.Error(), `Claude account "chief", which is not configured`) {
		t.Fatalf("an unrouted session with no token: %v, want the missing-account refusal", err)
	}
}
