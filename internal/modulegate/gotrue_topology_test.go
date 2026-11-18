package modulegate

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// EVERY gotrue CONFIGURATION THE REPOSITORY SHIPS OR TESTS WITH DECLARES A
// CLIENT TOPOLOGY.
//
// muster-server refuses to boot in gotrue mode unless MUSTER_TRUSTED_PROXIES
// (with MUSTER_CLIENT_IP_HEADER) or MUSTER_DIRECT_CLIENTS=1 is set, and the
// chart refuses to render it. A test install, an e2e server or a documented
// recipe that sets gotrue mode without one fails only where it runs — the kind
// leg ran for the first time after the rule landed and failed there. This
// test is the relationship, checked over every tracked file that can carry
// such a configuration:
//
//   - charts/muster/ci/*.yaml: auth.mode gotrue => clientAddress.directClients
//     true or clientAddress.trustedProxies non-empty (parsed);
//   - shell, Python, TypeScript, YAML, env files: in each file, at least as
//     many declarations as places that turn gotrue mode on (comment lines in
//     shell and env files are skipped unless they are a commented-out example
//     assignment, which a reader uncomments);
//   - Markdown: the same, per fenced code block (prose that NAMES the mode is
//     not a configuration).
//
// gotrueExempt names the files that set gotrue mode WITHOUT a declaration on
// purpose, each with its reason; it is asserted both ways. (The chart's
// render test is NOT here: each of its refusal cases is balanced by cases
// that declare one, so it passes the count.)

var (
	gotrueOn = regexp.MustCompile(`auth\.mode=gotrue|mode: *gotrue\b|MUSTER_AUTH_MODE *[=:] *['"]?gotrue\b`)
	topology = regexp.MustCompile(`clientAddress\.directClients=true|clientAddress\.trustedProxies|MUSTER_DIRECT_CLIENTS *[=:]|MUSTER_TRUSTED_PROXIES *[=:]`)
)

var gotrueExempt = map[string]string{
	"tests/install_guide_test.py": "a fake `docker compose config` output, read only by install.sh's mode detection; nothing boots it",
}

// topologyDefects is every place in a file's content that sets gotrue mode
// without a matching declaration (by the rules above). path decides the rule.
func topologyDefects(path string, content []byte) []string {
	if strings.HasPrefix(path, "charts/muster/ci/") && strings.HasSuffix(path, ".yaml") {
		var v struct {
			Auth struct {
				Mode string `yaml:"mode"`
			} `yaml:"auth"`
			ClientAddress struct {
				DirectClients  bool     `yaml:"directClients"`
				TrustedProxies []string `yaml:"trustedProxies"`
			} `yaml:"clientAddress"`
		}
		if err := yaml.Unmarshal(content, &v); err != nil {
			return []string{path + ": does not parse: " + err.Error()}
		}
		if v.Auth.Mode == "gotrue" && !v.ClientAddress.DirectClients && len(v.ClientAddress.TrustedProxies) == 0 {
			return []string{path + ": auth.mode gotrue with no clientAddress.directClients or clientAddress.trustedProxies"}
		}
		return nil
	}
	count := func(text string) []string {
		on, decl := 0, 0
		for _, line := range strings.Split(text, "\n") {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "#") && !regexp.MustCompile(`^#\s*[A-Z_]+=`).MatchString(t) &&
				(strings.HasSuffix(path, ".sh") || strings.Contains(path, ".env")) {
				continue // a shell or env comment that is not a commented-out example
			}
			on += len(gotrueOn.FindAllString(line, -1))
			decl += len(topology.FindAllString(line, -1))
		}
		if on > decl {
			return []string{path + ": sets gotrue mode " + strconv.Itoa(on) + " time(s) but declares a client topology " + strconv.Itoa(decl) + " time(s)"}
		}
		return nil
	}
	if strings.HasSuffix(path, ".md") {
		var out []string
		fence := regexp.MustCompile("(?s)```[^\n]*\n(.*?)```")
		for _, m := range fence.FindAllStringSubmatch(string(content), -1) {
			out = append(out, count(m[1])...)
		}
		return out
	}
	return count(string(content))
}

// inTopologyScope: the places a gotrue INSTALL or recipe lives — test and e2e
// harnesses, scripts, the Compose deploy, the CI workflows, the chart's CI value
// sets and every document. The chart's own templates and values.yaml are the
// rule (they name gotrue to refuse it), not an install of it.
func inTopologyScope(rel string) bool {
	for _, p := range []string{"tests/", "e2e/", "scripts/", "deploy/", "docs/", ".github/", "charts/muster/ci/"} {
		if strings.HasPrefix(rel, p) {
			return true
		}
	}
	return !strings.Contains(rel, "/") && strings.HasSuffix(rel, ".md") // INSTALL.md, README.md, …
}

func TestEveryGoTrueConfigurationDeclaresAClientTopology(t *testing.T) {
	// The instrument can go red, and can stay green, on realistic shapes.
	for _, c := range []struct {
		path, content string
		bad           bool
	}{
		{"tests/chart/x.sh", "svc=(--set auth.mode=gotrue --set gotrue.enabled=true)\n", true},
		{"tests/chart/x.sh", "svc=(--set auth.mode=gotrue\n  --set clientAddress.directClients=true)\n", false},
		{"e2e/tests/x.spec.ts", "    MUSTER_AUTH_MODE: 'gotrue',\n", true},
		{"e2e/tests/x.spec.ts", "    MUSTER_AUTH_MODE: 'gotrue',\n    MUSTER_DIRECT_CLIENTS: '1',\n", false},
		{"docs/x.md", "prose `MUSTER_AUTH_MODE=gotrue`\n\n```sh\nMUSTER_AUTH_MODE=gotrue\n```\n", true},
		{"docs/x.md", "prose `MUSTER_AUTH_MODE=gotrue` only\n", false},
		{"deploy/install.sh", "# Accounts mode (MUSTER_AUTH_MODE=gotrue): a comment\n", false},
		{"charts/muster/ci/x.yaml", "auth:\n  mode: gotrue\n", true},
		{"charts/muster/ci/x.yaml", "auth:\n  mode: gotrue\nclientAddress:\n  directClients: true\n", false},
	} {
		if got := len(topologyDefects(c.path, []byte(c.content))) > 0; got != c.bad {
			t.Fatalf("CONTROL %s %q: defect=%v, want %v", c.path, c.content, got, c.bad)
		}
	}

	root := moduleRoot(t)
	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var defects []string
	seenExempt := map[string]bool{}
	scanned, gotrueFiles := 0, 0
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" || strings.HasSuffix(rel, ".go") || !inTopologyScope(rel) {
			continue // Go configurations are pinned by the boot tests (cmd/muster-server)
		}
		ext := filepath.Ext(rel)
		switch {
		case ext == ".sh", ext == ".py", ext == ".ts", ext == ".yml", ext == ".yaml", ext == ".md",
			strings.Contains(filepath.Base(rel), ".env"):
		default:
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue // a tracked file deleted in the working tree
		}
		scanned++
		if gotrueOn.Match(b) {
			gotrueFiles++
		}
		d := topologyDefects(rel, b)
		if _, ok := gotrueExempt[rel]; ok {
			seenExempt[rel] = true
			if len(d) == 0 {
				t.Errorf("%s is exempt (%s) but now declares a topology everywhere it sets gotrue: drop the exemption", rel, gotrueExempt[rel])
			}
			continue
		}
		defects = append(defects, d...)
	}
	for rel := range gotrueExempt {
		if !seenExempt[rel] {
			t.Errorf("gotrueExempt names %s, which is not a tracked file any more", rel)
		}
	}
	// Positive control on the real tree: the scan saw gotrue configurations.
	if gotrueFiles < 10 {
		t.Fatalf("CONTROL: only %d of %d scanned files set gotrue mode; the scan is not seeing the e2e servers and chart values", gotrueFiles, scanned)
	}
	sort.Strings(defects)
	for _, d := range defects {
		t.Errorf("%s — muster-server refuses to boot (and the chart to render) gotrue mode with no client topology: "+
			"declare MUSTER_DIRECT_CLIENTS=1 / clientAddress.directClients=true (no proxy) or the trusted proxies", d)
	}
	t.Logf("%d file(s) scanned, %d set gotrue mode", scanned, gotrueFiles)
}
