package podagent

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// smokePins reads the PINNED shell assignments (NAME='value') of the image's
// deploy-key smoke (images/claude-code-agent/deploykey-smoke.sh), which runs
// production's flags as the production uids on the real image in CI.
func smokePins(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "images", "claude-code-agent", "deploykey-smoke.sh"))
	if err != nil {
		t.Fatal(err)
	}
	pins := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^([A-Z_]+)='([^']*)'$`).FindAllStringSubmatch(string(b), -1) {
		pins[m[1]] = m[2]
	}
	for _, k := range []string{"SSH_CMD", "MIRROR", "GIT_PRE", "UPLOAD_PACK", "CLONE_FLAGS"} {
		if _, ok := pins[k]; !ok {
			t.Fatalf("the smoke pins no %s (found %v)", k, pins)
		}
	}
	return pins
}

// TestTheDeployKeySmokePinsTheCheckoutsMirrorFlags: the arguments the smoke
// clones and fetches the mirror with are what a mirror checkout's git runs
// with — so the smoke, on the real image, tests production's flags.
func TestTheDeployKeySmokePinsTheCheckoutsMirrorFlags(t *testing.T) {
	pins := smokePins(t)
	c, err := NewCheckout(CheckoutConfig{Repo: "o/r", Dir: "/w", Mirror: pins["MIRROR"]})
	if err != nil {
		t.Fatal(err)
	}
	if pins["GIT_PRE"] != "" {
		t.Errorf("the smoke pins arguments before git's subcommand (%q); the checkout puts none there", pins["GIT_PRE"])
	}
	want, err := MirrorUploadPack(pins["MIRROR"])
	if err != nil {
		t.Fatal(err)
	}
	if got := c.uploadPack(); got != want || got != pins["UPLOAD_PACK"] {
		t.Errorf("the checkout's --upload-pack is %q (MirrorUploadPack %q); the smoke pins %q", got, want, pins["UPLOAD_PACK"])
	}
	if args := c.remoteArgs("clone"); len(args) != 2 || args[1] != "--upload-pack="+pins["UPLOAD_PACK"] {
		t.Errorf("the clone's arguments are %q", args)
	}
}
