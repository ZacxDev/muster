package agentspec

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/ZacxDev/muster/internal/podagent"
)

// TestTheDeployKeySmokePinsProductionsFlags: the image's deploy-key smoke
// (images/claude-code-agent/deploykey-smoke.sh, run as the production uids on
// the real image in CI) uses the key holder's exact ssh command, the mirror's
// path and the holder's uid that the spec builds — so a change to any of them
// that the image cannot run fails there, not on a worker.
func TestTheDeployKeySmokePinsProductionsFlags(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "images", "claude-code-agent", "deploykey-smoke.sh"))
	if err != nil {
		t.Fatal(err)
	}
	pins := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^([A-Z_]+)='?([^'\n]*)'?$`).FindAllStringSubmatch(string(b), -1) {
		pins[m[1]] = m[2]
	}
	ssh, err := podagent.DeployKeySSHCommand(DeployKeyPath, KnownHostsPath)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"SSH_CMD":     ssh,
		"MIRROR":      MirrorPath,
		"HOLDER_UID":  strconv.Itoa(DeployKeyHolderUID),
		"SESSION_UID": strconv.Itoa(ClaudeCodeUID),
	} {
		if pins[name] != want {
			t.Errorf("the smoke pins %s=%q; production uses %q", name, pins[name], want)
		}
	}
}
