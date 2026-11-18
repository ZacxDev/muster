package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/podagent"
)

// TestSeedWritesThePerAgentSessionSettings: CCD_ALLOW_MCP_TOOLS becomes
// settings.json permissions.allow — exactly "mcp__muster__<name>" per name, on
// top of the image template's permission posture — and CCD_MODEL becomes
// "model". checkSeed agrees with what seed wrote, and refuses a file seeded
// without them. A prior allow list (a session that edited its own settings)
// does not survive: the template's permissions are imposed, then ONLY muster's
// entries added.
func TestSeedWritesThePerAgentSessionSettings(t *testing.T) {
	cfg, ws := t.TempDir(), filepath.Join(t.TempDir(), "ws")
	if err := os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"permissions":{"allow":["Bash(*)","Write"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	o := seedOpts{configDir: cfg, workspace: ws, settingsTemplate: imageSettingsTemplate,
		allowMCPTools: "officer_brief,officer_claim", model: "opus"}
	if err := seed(o, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	set := readObj(t, filepath.Join(cfg, "settings.json"))
	perms, _ := json.Marshal(set["permissions"])
	want := `{"allow":["mcp__muster__officer_brief","mcp__muster__officer_claim"],"defaultMode":"default","disableAutoMode":"disable","disableBypassPermissionsMode":"disable"}`
	if string(perms) != want {
		t.Fatalf("permissions = %s\nwant          %s", perms, want)
	}
	if set["model"] != "opus" {
		t.Fatalf("model = %v, want opus", set["model"])
	}
	if err := checkSeed(o); err != nil {
		t.Fatalf("checkSeed disagrees with seed: %v", err)
	}
	plain := o
	plain.allowMCPTools, plain.model = "", ""
	if err := checkSeed(plain); err == nil {
		t.Fatal("checkSeed without the session settings accepted a file seeded with them")
	}
}

// TestSeedWithoutSessionSettingsIsUnchanged: no variables, no allow list and
// no model written — the image's posture exactly, a prior model kept.
func TestSeedWithoutSessionSettingsIsUnchanged(t *testing.T) {
	cfg, ws := t.TempDir(), filepath.Join(t.TempDir(), "ws")
	if err := os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"model":"kept"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	seedInto(t, cfg, ws)
	set := readObj(t, filepath.Join(cfg, "settings.json"))
	if perms, _ := json.Marshal(set["permissions"]); strings.Contains(string(perms), "allow") {
		t.Fatalf("an agent with no session settings got an allow list: %s", perms)
	}
	if set["model"] != "kept" {
		t.Fatalf("model = %v, want the prior value kept", set["model"])
	}
}

// TestSeedRefusesAnAllowListThatIsNotMusterToolNames: the channel can only
// allow muster's MCP tools — a value naming a built-in tool, a rule or another
// server is refused whole, before anything is written.
func TestSeedRefusesAnAllowListThatIsNotMusterToolNames(t *testing.T) {
	for _, v := range []string{"Bash", "Bash(*)", "officer_claim,Write", "mcp__other__x", "officer claim", "../x"} {
		cfg, ws := t.TempDir(), filepath.Join(t.TempDir(), "ws")
		o := seedOpts{configDir: cfg, workspace: ws, settingsTemplate: imageSettingsTemplate, allowMCPTools: v}
		if err := seed(o, &bytes.Buffer{}); err == nil {
			t.Errorf("%s=%q was accepted", podagent.EnvAllowMCPTools, v)
		}
		if _, err := os.Stat(filepath.Join(cfg, "settings.json")); err == nil {
			t.Errorf("%q: settings.json was written before the refusal", v)
		}
	}
	for _, v := range []string{"opus now", "\"opus\"", "../x", ""} {
		if v == "" {
			continue
		}
		o := seedOpts{configDir: t.TempDir(), workspace: filepath.Join(t.TempDir(), "ws"), settingsTemplate: imageSettingsTemplate, model: v}
		if err := seed(o, &bytes.Buffer{}); err == nil {
			t.Errorf("%s=%q was accepted", podagent.EnvModel, v)
		}
	}
}

// TestRunSeedReadsTheSessionSettingsFromTheEnvironment: the variables
// agentspec writes are the ones runSeed reads.
func TestRunSeedReadsTheSessionSettingsFromTheEnvironment(t *testing.T) {
	cfg, ws := t.TempDir(), filepath.Join(t.TempDir(), "ws")
	t.Setenv(podagent.EnvAllowMCPTools, "officer_items")
	t.Setenv(podagent.EnvModel, "sonnet")
	var out, errb bytes.Buffer
	if rc := runSeed([]string{"--config-dir", cfg, "--workspace", ws, "--settings-template", imageSettingsTemplate}, &out, &errb); rc != 0 {
		t.Fatalf("runSeed rc=%d: %s", rc, errb.String())
	}
	set := readObj(t, filepath.Join(cfg, "settings.json"))
	if perms, _ := json.Marshal(set["permissions"]); !strings.Contains(string(perms), `"allow":["mcp__muster__officer_items"]`) || set["model"] != "sonnet" {
		t.Fatalf("settings = %v", set)
	}
}

// TestRemovingTheModelRevertsToTheAccountDefault: an officer's model removed
// from its config must take effect, though the instance keeps its volume (and
// settings.json) across scale-to-zero. Seed REMOVES the model it set itself
// when CCD_MODEL is then unset; a model it did not set — or one changed in
// the pod since — is someone's choice and stays.
func TestRemovingTheModelRevertsToTheAccountDefault(t *testing.T) {
	cfg, ws := t.TempDir(), filepath.Join(t.TempDir(), "ws")
	with := seedOpts{configDir: cfg, workspace: ws, settingsTemplate: imageSettingsTemplate, model: "opus"}
	if err := seed(with, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	without := with
	without.model = ""
	if err := seed(without, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if m, ok := readObj(t, filepath.Join(cfg, "settings.json"))["model"]; ok {
		t.Fatalf("the model seed set (opus) survived CCD_MODEL being unset: model = %v", m)
	}
	if err := checkSeed(without); err != nil {
		t.Fatalf("checkSeed: %v", err)
	}
	// Seeded opus, then changed in the pod to sonnet: not seed's to remove.
	cfg2 := t.TempDir()
	w2 := with
	w2.configDir = cfg2
	if err := seed(w2, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	set := readObj(t, filepath.Join(cfg2, "settings.json"))
	set["model"] = "sonnet"
	b, _ := json.Marshal(set)
	if err := os.WriteFile(filepath.Join(cfg2, "settings.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	wo2 := w2
	wo2.model = ""
	if err := seed(wo2, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if m := readObj(t, filepath.Join(cfg2, "settings.json"))["model"]; m != "sonnet" {
		t.Fatalf("a model changed in the pod was removed: %v", m)
	}
}
