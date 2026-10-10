package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The image's REAL template, not a copy: the seed and the hook wiring are only
// as right as the file the image ships.
const imageSettingsTemplate = "../../images/claude-code-agent/settings.json"

func readObj(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func seedInto(t *testing.T, cfg, ws string) {
	t.Helper()
	var stderr bytes.Buffer
	if err := seed(seedOpts{configDir: cfg, workspace: ws, settingsTemplate: imageSettingsTemplate}, &stderr); err != nil {
		t.Fatal(err)
	}
}

// 🔴 A LEDGER OF THE HOOKS THE IMAGE REGISTERS: exactly these events, each running
// `ccd hook <Event>`. It fails when the set GROWS (a new event ccd would receive
// unasked) or SHRINKS (dropping SessionStart leaves /healthz never ready; dropping
// StopFailure leaves a failed turn waiting out the whole turn timeout).
func TestTheImageRegistersExactlyTheHooksCcdNeeds(t *testing.T) {
	hooks, _ := readObj(t, imageSettingsTemplate)["hooks"].(map[string]any)
	var events []string
	for ev, v := range hooks {
		events = append(events, ev)
		groups, _ := v.([]any)
		if len(groups) != 1 {
			t.Fatalf("%s: %d matcher groups, want 1", ev, len(groups))
		}
		hs, _ := groups[0].(map[string]any)["hooks"].([]any)
		if len(hs) != 1 {
			t.Fatalf("%s: %d hooks, want 1", ev, len(hs))
		}
		h := hs[0].(map[string]any)
		if h["type"] != "command" || h["command"] != "ccd hook "+ev {
			t.Fatalf("%s: hook %v, want command `ccd hook %s`", ev, h, ev)
		}
	}
	sort.Strings(events)
	want := []string{"SessionStart", "Stop", "StopFailure", "UserPromptSubmit"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("registered events %v, want exactly %v", events, want)
	}
}

func TestSeedOnAnEmptyDirSkipsOnboardingAndTrustsTheWorkspace(t *testing.T) {
	cfg, ws := t.TempDir(), filepath.Join(t.TempDir(), "workspace")
	seedInto(t, cfg, ws)
	st := readObj(t, filepath.Join(cfg, ".claude.json"))
	if st["hasCompletedOnboarding"] != true || st["theme"] != "dark" {
		t.Fatalf(".claude.json: %v", st)
	}
	proj := st["projects"].(map[string]any)[ws].(map[string]any)
	if proj["hasTrustDialogAccepted"] != true {
		t.Fatalf("trust: %v", proj)
	}
	if fi, err := os.Stat(filepath.Join(cfg, ".claude.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf(".claude.json mode: %v %v", fi.Mode(), err)
	}
	if _, err := os.Stat(ws); err != nil {
		t.Fatal("workspace not created")
	}
	if err := checkSeed(seedOpts{configDir: cfg, workspace: ws, settingsTemplate: imageSettingsTemplate}); err != nil {
		t.Fatal(err)
	}
}

// On a persistent volume the CLI has written its own state into .claude.json;
// re-seeding at the next start must keep it.
func TestReseedPreservesTheCLIsOwnState(t *testing.T) {
	cfg, ws := t.TempDir(), "/data/workspace-test"
	ws = filepath.Join(t.TempDir(), "ws")
	prior := map[string]any{
		"numStartups": 7.0, "theme": "light",
		"projects": map[string]any{
			ws:       map[string]any{"lastSessionId": "abc", "hasTrustDialogAccepted": false},
			"/other": map[string]any{"hasTrustDialogAccepted": true},
		},
	}
	b, _ := json.Marshal(prior)
	if err := os.WriteFile(filepath.Join(cfg, ".claude.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"model":"opus","hooks":{"PreToolUse":[]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	seedInto(t, cfg, ws)
	st := readObj(t, filepath.Join(cfg, ".claude.json"))
	projects := st["projects"].(map[string]any)
	mine := projects[ws].(map[string]any)
	if st["numStartups"] != 7.0 || st["theme"] != "light" || mine["lastSessionId"] != "abc" ||
		mine["hasTrustDialogAccepted"] != true || projects["/other"] == nil {
		t.Fatalf("state after reseed: %v", st)
	}
	set := readObj(t, filepath.Join(cfg, "settings.json"))
	if set["model"] != "opus" {
		t.Fatal("a settings key the template does not name was dropped")
	}
	if _, ok := set["hooks"].(map[string]any)["PreToolUse"]; ok {
		t.Fatal("the template's `hooks` did not replace the file's (it is image-owned)")
	}
}

func TestSeedMovesACorruptStateFileAside(t *testing.T) {
	cfg, ws := t.TempDir(), filepath.Join(t.TempDir(), "ws")
	if err := os.WriteFile(filepath.Join(cfg, ".claude.json"), []byte(`{truncated`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if err := seed(seedOpts{configDir: cfg, workspace: ws, settingsTemplate: imageSettingsTemplate}, &stderr); err != nil {
		t.Fatal(err)
	}
	m, _ := filepath.Glob(filepath.Join(cfg, ".claude.json.corrupt-*"))
	if len(m) != 1 || !strings.Contains(stderr.String(), "moved to") {
		t.Fatalf("aside=%v stderr=%q", m, stderr.String())
	}
	if readObj(t, filepath.Join(cfg, ".claude.json"))["hasCompletedOnboarding"] != true {
		t.Fatal("not reseeded after moving the corrupt file aside")
	}
}

func TestCheckSeedCatchesEachMissingPiece(t *testing.T) {
	cfg, ws := t.TempDir(), filepath.Join(t.TempDir(), "ws")
	o := seedOpts{configDir: cfg, workspace: ws, settingsTemplate: imageSettingsTemplate}
	seedInto(t, cfg, ws)
	if err := checkSeed(o); err != nil {
		t.Fatalf("control: %v", err)
	}
	mutate := func(file string, f func(map[string]any)) {
		m := readObj(t, filepath.Join(cfg, file))
		f(m)
		b, _ := json.Marshal(m)
		if err := os.WriteFile(filepath.Join(cfg, file), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mutate(".claude.json", func(m map[string]any) { m["hasCompletedOnboarding"] = false })
	if err := checkSeed(o); err == nil || !strings.Contains(err.Error(), "theme picker") {
		t.Fatalf("onboarding: %v", err)
	}
	seedInto(t, cfg, ws)
	mutate(".claude.json", func(m map[string]any) { delete(m["projects"].(map[string]any), ws) })
	if err := checkSeed(o); err == nil || !strings.Contains(err.Error(), "trust dialog") {
		t.Fatalf("trust: %v", err)
	}
	seedInto(t, cfg, ws)
	mutate("settings.json", func(m map[string]any) { delete(m, "hooks") })
	if err := checkSeed(o); err == nil || !strings.Contains(err.Error(), `"hooks"`) {
		t.Fatalf("hooks: %v", err)
	}
}

func TestSeedRefusesARelativeWorkspace(t *testing.T) {
	err := seed(seedOpts{configDir: t.TempDir(), workspace: "relative/ws", settingsTemplate: imageSettingsTemplate}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "exact absolute cwd") {
		t.Fatalf("err = %v", err)
	}
}
