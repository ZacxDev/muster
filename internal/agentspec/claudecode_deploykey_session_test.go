package agentspec

import (
	"strings"
	"testing"
)

// TestTheSessionsEnvironmentNamesNoDeployKey: a deploy-key pool worker's
// environment — what ccd, and through it every lane's tools, inherit — holds
// no path to the key or its known_hosts and no reference to the Secret. The
// key is the key holder's alone (the k8s render test pins where it is
// mounted).
func TestTheSessionsEnvironmentNamesNoDeployKey(t *testing.T) {
	spec := mustBuild(t, deployKeyAgent("private-review", "acme/private-app"), deployKeyConfig(), Options{})
	for _, name := range []string{"CCD_REPO_DEPLOY_KEY", "CCD_REPO_KNOWN_HOSTS"} {
		if v, ok := envValue(spec, name); ok {
			t.Errorf("%s=%q is in the session's environment", name, v)
		}
	}
	for _, e := range spec.Env {
		if strings.Contains(e.Value, "ssh-privatekey") || strings.Contains(e.Value, "private-app-deploy-key") {
			t.Errorf("%s=%q points at the deploy key", e.Name, e.Value)
		}
	}
	// Control: the worker still clones its repository (CCD_REPO is set), so an
	// empty environment is not why the checks above pass.
	if v, _ := envValue(spec, "CCD_REPO"); v != "acme/private-app" {
		t.Fatalf("control: CCD_REPO = %q, want acme/private-app", v)
	}
}
