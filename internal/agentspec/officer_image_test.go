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

// TestThePullSecretReachesTheSpecOnlyBesideTheSessionsImage: a session with
// its own image and a pull Secret puts the Secret in the spec; the same pull
// Secret on a session WITHOUT an image (an officer pool on the deployment's
// image) reaches no spec field, so the deployment's image is never pulled
// with it. The zero session has none.
func TestThePullSecretReachesTheSpecOnlyBesideTheSessionsImage(t *testing.T) {
	const custom = "ghcr.io/example-org/officer-runner:9f8e7d6"
	spec := mustBuild(t, ccAgent(), ccConfig(), Options{ClaudeCodeSession: ClaudeCodeSession{
		Image: custom, ImagePullSecret: "officer-image-pull", BypassPermissions: true}})
	if spec.ImagePullSecret != "officer-image-pull" {
		t.Fatalf("with the session's image, ImagePullSecret = %q, want %q", spec.ImagePullSecret, "officer-image-pull")
	}
	spec = mustBuild(t, ccAgent(), ccConfig(), Options{ClaudeCodeSession: ClaudeCodeSession{
		ImagePullSecret: "officer-image-pull", BypassPermissions: true}})
	if spec.ImagePullSecret != "" || spec.Runtime.Image != "ghcr.io/example-org/claude-code-agent:0a1b2c3" {
		t.Fatalf("on the deployment's image %q, ImagePullSecret = %q, want none", spec.Runtime.Image, spec.ImagePullSecret)
	}
	spec = mustBuild(t, ccAgent(), ccConfig(), Options{})
	if spec.ImagePullSecret != "" {
		t.Fatalf("the zero session's ImagePullSecret = %q", spec.ImagePullSecret)
	}
}
