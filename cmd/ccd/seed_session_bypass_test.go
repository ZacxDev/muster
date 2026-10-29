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

// TestSeedStartsEverySessionInBypassWhenThePodSaysSo pins BOTH postures whole
// (PR #337 round 1, operator decision A: officer instances run with every tool
// allowed; every other agent unchanged).
//
//   - CCD_PERMISSION_MODE=bypassPermissions (an officer instance): the
//     permissions object is exactly the officer allow list plus
//     defaultMode=bypassPermissions and disableAutoMode — the
//     disableBypassPermissionsMode refusal is gone (the CLI refuses the mode
//     while it is set) — and the disclaimer consent is recorded, so the pinned
//     CLI starts at its prompt in bypass with no dialog for a pasted turn to
//     answer.
//   - unset (a task agent, the chief): the image's posture exactly, bypass
//     disabled, no consent written.
//
// checkSeed agrees with each and refuses the other's file. A persisted
// settings.json left in another mode (or holding an allow list) is overwritten
// either way.
func TestSeedStartsEverySessionInBypassWhenThePodSaysSo(t *testing.T) {
	for _, c := range []struct {
		name, mode, wantPerms string
		wantSkip              bool
	}{
		{"officer: bypass", podagent.PermissionModeBypass,
			`{"allow":["mcp__muster__officer_brief"],"defaultMode":"bypassPermissions","disableAutoMode":"disable"}`, true},
		{"task agent: unchanged", "",
			`{"allow":["mcp__muster__officer_brief"],"defaultMode":"default","disableAutoMode":"disable","disableBypassPermissionsMode":"disable"}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, ws := t.TempDir(), filepath.Join(t.TempDir(), "ws")
			if err := os.WriteFile(filepath.Join(cfg, "settings.json"),
				[]byte(`{"permissions":{"defaultMode":"acceptEdits","allow":["Bash(*)"]},"model":"kept"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			o := seedOpts{configDir: cfg, workspace: ws, settingsTemplate: imageSettingsTemplate,
				allowMCPTools: "officer_brief", permissionMode: c.mode}
			if err := seed(o, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			set := readObj(t, filepath.Join(cfg, "settings.json"))
			if got, _ := json.Marshal(set["permissions"]); string(got) != c.wantPerms {
				t.Fatalf("permissions = %s\nwant          %s", got, c.wantPerms)
			}
			skip, has := set[settingsSkipBypassPrompt]
			if c.wantSkip && skip != true {
				t.Fatalf("%s = %v (present %v), want true", settingsSkipBypassPrompt, skip, has)
			}
			if !c.wantSkip && has {
				t.Fatalf("the default posture wrote the bypass consent %s = %v", settingsSkipBypassPrompt, skip)
			}
			if set["model"] != "kept" {
				t.Fatalf("seed dropped an unrelated key: %v", set)
			}
			if err := checkSeed(o); err != nil {
				t.Fatalf("checkSeed disagrees with seed: %v", err)
			}
			other := o
			other.permissionMode = map[string]string{"": podagent.PermissionModeBypass, podagent.PermissionModeBypass: ""}[c.mode]
			if err := checkSeed(other); err == nil {
				t.Fatalf("checkSeed with mode %q accepted the file seeded with mode %q", other.permissionMode, c.mode)
			}
		})
	}
}

// 🔴 FAIL CLOSED: a CCD_PERMISSION_MODE that is neither unset nor EXACTLY
// bypassPermissions is refused before anything is written — never read as
// bypass, never silently dropped.
func TestSeedRefusesAnyOtherSessionPermissionMode(t *testing.T) {
	for _, v := range []string{"bypass", "BypassPermissions", " bypassPermissions", "bypassPermissions\n", "true", "1", "dontAsk", "acceptEdits", "default", "plan"} {
		cfg, ws := t.TempDir(), filepath.Join(t.TempDir(), "ws")
		err := seed(seedOpts{configDir: cfg, workspace: ws, settingsTemplate: imageSettingsTemplate, permissionMode: v}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), podagent.EnvPermissionMode) {
			t.Errorf("%q: err = %v, want a refusal naming %s", v, err, podagent.EnvPermissionMode)
		}
		for _, f := range []string{".claude.json", "settings.json"} {
			if _, serr := os.Stat(filepath.Join(cfg, f)); serr == nil {
				t.Errorf("%q: seed wrote %s before refusing", v, f)
			}
		}
	}
}

// `ccd seed` takes the session mode from the pod's environment — the variable
// agentspec writes (podagent.EnvPermissionMode).
func TestRunSeedReadsTheSessionPermissionModeFromTheEnvironment(t *testing.T) {
	for _, c := range []struct {
		env, wantMode string
	}{{"", "default"}, {podagent.PermissionModeBypass, "bypassPermissions"}} {
		cfg, ws := t.TempDir(), filepath.Join(t.TempDir(), "ws")
		t.Setenv(podagent.EnvPermissionMode, c.env)
		var out, errb bytes.Buffer
		if rc := runSeed([]string{"--config-dir", cfg, "--workspace", ws, "--settings-template", imageSettingsTemplate}, &out, &errb); rc != 0 {
			t.Fatalf("env %q: rc %d: %s", c.env, rc, errb.String())
		}
		perms, _ := readObj(t, filepath.Join(cfg, "settings.json"))["permissions"].(map[string]any)
		if perms["defaultMode"] != c.wantMode {
			t.Fatalf("env %q: defaultMode = %v, want %s", c.env, perms["defaultMode"], c.wantMode)
		}
	}
}
