//go:build chart

package modulegate

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

// ---------------------------------------------------------------------------
// EVERY VARIABLE THE HELM CHART SETS IS A ROW OF docs/configuration.md — in the
// table of the binary that reads it.
//
// The chart (charts/muster) turns values into environment variables. A name it
// misspells, or one the code stopped reading, is a value that does nothing:
// the server boots and the operator's setting is silently gone. The envdoc gate
// (envdoc_test.go) already pins configuration.md's tables to what the code
// reads, both ways; this pins the chart to those tables, so the chain is
// chart -> doc -> code.
//
// 🔴 PER BINARY, NOT PER FILE. MUSTER_URL is a documented row, of
// muster-preview; set on the SERVER it does nothing. So each container is
// checked against its own binary's sections: the server container against
// "muster-server: …" and "Tracing". (The chart runs no other muster binary
// today; one added goes in chartContainerSections with its own sections.)
//
// 🔴 A CONTAINER THIS TEST DOES NOT KNOW IS A FAILURE. The bundled third-party
// services (postgres, the speech-to-text and S3 images, GoTrue) read their own
// variables and are ledgered in thirdPartyContainers; a NEW muster container
// that is in neither list cannot pass unchecked.
//
// What it does not see, stated: names passed whole through envFrom (the
// per-name MUSTER_AGENT_CC_TOKEN_<NAME> / MUSTER_OFFICER_TOKEN_<NAME> Secrets an
// operator makes; their keys are the operator's), and a value's CONTENT (that
// is tests/chart/boot_config_check.py, which boots the real server on it).
//
// Run: go test -tags chart -run TestChart ./internal/modulegate/  (helm on PATH)
// ---------------------------------------------------------------------------

// chartContainerSections: muster's own containers, by container name, and
// the configuration.md sections ("## " headings, by prefix) whose rows they may set.
var chartContainerSections = map[string][]string{
	"muster": {"muster-server", "Tracing"},
}

// thirdPartyContainers: every other container the chart renders, with whose
// variables it reads. A ledger: one that appears or vanishes fails.
var thirdPartyContainers = map[string]string{
	"postgres": "the postgres image's own (POSTGRES_*, PGDATA)",
	"dbinit":   "psql's (PG*) and the script's own AUTH_PASSWORD, in GoTrue's init container",
	"gotrue":   "GoTrue's (GOTRUE_*, API_EXTERNAL_URL, DATABASE_URL, PORT)",
	"stt":      "the speaches image's (API_KEY, HF_HUB_CACHE, ENABLE_UI) and the probe's STT_WHISPER_MODEL",
	"s3":       "the store image's (MINIO_*) and the start script's MUSTER_S3_BUCKET and HOME",
}

func TestChartEnvIsDocumentedForTheBinaryThatReadsIt(t *testing.T) {
	root := moduleRoot(t)
	docs := sectionedConfigDoc(t, filepath.Join(root, configDoc))

	sets, err := filepath.Glob(filepath.Join(root, "charts/muster/ci/*.yaml"))
	if err != nil || len(sets) < 4 {
		t.Fatalf("instrument check: want the chart's value sets under charts/muster/ci, found %v (%v)", sets, err)
	}
	seen := map[string]map[string]bool{} // container -> names, over every set
	containersSeen := map[string]bool{}
	for _, set := range sets {
		for _, c := range renderedContainers(t, root, set) {
			containersSeen[c.name] = true
			sections, ours := chartContainerSections[c.name]
			if !ours {
				if _, known := thirdPartyContainers[c.name]; !known {
					t.Errorf("%s: container %q is neither one of muster's (chartContainerSections) nor a ledgered "+
						"third-party one (thirdPartyContainers) — say which", filepath.Base(set), c.name)
				}
				continue
			}
			for _, bad := range undocumentedFor(c.env, docs, sections) {
				t.Errorf("%s: container %q sets %s, which no row of configuration.md's %v sections lists",
					filepath.Base(set), c.name, bad, sections)
			}
			if seen[c.name] == nil {
				seen[c.name] = map[string]bool{}
			}
			for _, n := range c.env {
				seen[c.name][n] = true
			}
		}
	}
	for name := range thirdPartyContainers {
		if !containersSeen[name] {
			t.Errorf("thirdPartyContainers lists %q, which no value set renders — remove it, or render it in a set", name)
		}
	}

	// 🔴 POSITIVE CONTROL: the sets reach the variables this test exists for. A
	// renderer that lost the env (or a set that stopped enabling features)
	// would pass with nothing to check.
	for c, want := range map[string][]string{
		"muster": {"DATABASE_URL", "MUSTER_AGENT_PROVISIONER", "MUSTER_AGENT_NETPOL_FROM_POD_LABELS",
			"MUSTER_AGENT_NAMESPACE_CLUSTERROLE", "MUSTER_VAPID_PRIVATE_KEY", "MUSTER_GITHUB_APP_ACCOUNTS",
			"MUSTER_S3_ENDPOINT", "MUSTER_STT_URL", "MUSTER_GOTRUE_URL", "MUSTER_OFFICERS", "MUSTER_PLUGINS"},
	} {
		for _, n := range want {
			if !seen[c][n] {
				t.Errorf("positive control FAILED: no value set renders %s on container %q", n, c)
			}
		}
	}
	if len(seen["muster"]) < 50 {
		t.Errorf("positive control FAILED: only %d distinct variable(s) on the server container across the sets", len(seen["muster"]))
	}
	t.Logf("%d value set(s); server container: %d distinct variables", len(sets), len(seen["muster"]))
}

// TestChartEnvCheckerRefusesWhatItMust is the checker's negative control,
// without the chart: an invented name, and a real row of ANOTHER binary.
func TestChartEnvCheckerRefusesWhatItMust(t *testing.T) {
	docs := sectionedConfigDoc(t, filepath.Join(moduleRoot(t), configDoc))
	server := chartContainerSections["muster"]
	got := undocumentedFor([]string{"MUSTER_PORT", "MUSTER_NOT_A_VARIABLE", "MUSTER_URL", "MUSTER_AGENT_CC_TOKEN_X"}, docs, server)
	if strings.Join(got, ",") != "MUSTER_NOT_A_VARIABLE,MUSTER_URL" {
		t.Errorf("undocumentedFor = %v, want [MUSTER_NOT_A_VARIABLE MUSTER_URL] (an invented name, and the preview "+
			"gateway's documented MUSTER_URL on the server; MUSTER_PORT and a documented <NAME> prefix pass)", got)
	}
}

type renderedContainer struct {
	name string
	env  []string
}

// renderedContainers runs `helm template` on one value set and returns every
// container (init containers included) of every workload it renders.
func renderedContainers(t *testing.T, root, values string) []renderedContainer {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Fatalf("helm is not on PATH: this test renders the chart (the CI job installs it)")
	}
	cmd := exec.Command(helm, "template", "t", filepath.Join(root, "charts/muster"), "-n", "muster", "-f", values)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template -f %s: %v\n%s", values, err, stderr.String())
	}
	var got []renderedContainer
	dec := k8syaml.NewYAMLOrJSONDecoder(&out, 4096)
	for {
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode %s: %v", values, err)
		}
		pod := dig(obj, "spec", "template", "spec")
		if pod == nil {
			continue
		}
		for _, field := range []string{"initContainers", "containers"} {
			list, _ := pod[field].([]any)
			for _, c := range list {
				cm := c.(map[string]any)
				rc := renderedContainer{name: cm["name"].(string)}
				envs, _ := cm["env"].([]any)
				for _, e := range envs {
					rc.env = append(rc.env, e.(map[string]any)["name"].(string))
				}
				got = append(got, rc)
			}
		}
	}
	if len(got) == 0 {
		t.Fatalf("instrument check: %s rendered no container", values)
	}
	return got
}

func dig(m map[string]any, path ...string) map[string]any {
	for _, p := range path {
		next, ok := m[p].(map[string]any)
		if !ok {
			return nil
		}
		m = next
	}
	return m
}

type docSection struct{ names, prefixes map[string]bool }

// sectionedConfigDoc reads configuration.md's tables per "## " section, through
// the envdoc gate's own row parser (documentedEnv), so both read rows alike.
func sectionedConfigDoc(t *testing.T, path string) map[string]docSection {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]docSection{}
	parts := strings.Split("\n"+string(b), "\n## ")
	for _, part := range parts[1:] {
		heading, body, _ := strings.Cut(part, "\n")
		p := filepath.Join(t.TempDir(), "section.md")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		n, pf := documentedEnv(t, p)
		out[heading] = docSection{n, pf}
	}
	if len(out) < 8 {
		t.Fatalf("instrument check: only %d section(s) read from %s", len(out), path)
	}
	return out
}

// undocumentedFor returns, sorted, the names no row of the sections whose
// heading starts with one of `want` lists (a name or a `PREFIX_<NAME>` row).
func undocumentedFor(env []string, docs map[string]docSection, want []string) []string {
	var bad []string
	for _, n := range env {
		ok := false
		for heading, s := range docs {
			for _, w := range want {
				if strings.HasPrefix(heading, w) && (s.names[n] || coveredByPrefix(n, s.prefixes)) {
					ok = true
				}
			}
		}
		if !ok {
			bad = append(bad, n)
		}
	}
	sort.Strings(bad)
	return bad
}
