package agentspec

import "testing"

// TestAnOfficersImageReplacesTheDeploymentsOnlyForThatSession: a session
// naming an image runs it; the zero session runs the deployment's
// ClaudeCodeConfig.Image. Literal expectations, distinct from each other.
func TestAnOfficersImageReplacesTheDeploymentsOnlyForThatSession(t *testing.T) {
	const custom = "ghcr.io/example-org/officer-runner:9f8e7d6"
	spec := mustBuild(t, ccAgent(), ccConfig(), Options{ClaudeCodeSession: ClaudeCodeSession{Image: custom, BypassPermissions: true}})
	if spec.Runtime.Image != custom {
		t.Fatalf("Runtime.Image = %q, want the session's %q", spec.Runtime.Image, custom)
	}
	spec = mustBuild(t, ccAgent(), ccConfig(), Options{ClaudeCodeSession: ClaudeCodeSession{BypassPermissions: true}})
	if spec.Runtime.Image != "ghcr.io/example-org/claude-code-agent:0a1b2c3" {
		t.Fatalf("Runtime.Image = %q, want the deployment's", spec.Runtime.Image)
	}
}
