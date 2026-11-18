package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAnExistingChiefIsReseededIntoBypass: the claude-code chief's pod env
// (CCD_HQ_MCP=1 and, since PR #626, CCD_PERMISSION_MODE=bypassPermissions; no
// allow list) re-seeded over the settings.json its PREVIOUS start wrote — the
// image's default posture, bypass disabled — yields exactly the bypass posture:
// defaultMode bypassPermissions, the template's disableBypassPermissionsMode
// gone, the disclaimer consent recorded, the HQ entry kept. So an existing chief
// picks the bypass up on its next pod start. checkSeed agrees, and refuses the
// file when run without the mode.
//
// Positive control: the image template really carries
// disableBypassPermissionsMode, so "it does not block the bypass" is measured,
// not vacuous.
func TestAnExistingChiefIsReseededIntoBypass(t *testing.T) {
	tmpl, err := os.ReadFile(imageSettingsTemplate)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tmpl), `"disableBypassPermissionsMode": "disable"`) {
		t.Fatal("instrument check FAILED: the image template no longer sets disableBypassPermissionsMode")
	}
	stubHQBinary(t, false)
	t.Setenv(envConnectURL, "")

	o := hqSeedDir(t, "1")
	// The previous start: the chief before PR #626 (no permission mode).
	if err := seed(o, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	before := readObj(t, filepath.Join(o.configDir, "settings.json"))
	if p, _ := json.Marshal(before["permissions"]); !strings.Contains(string(p), `"disableBypassPermissionsMode":"disable"`) {
		t.Fatalf("control: the pre-bypass start's permissions = %s", p)
	}

	// The next start, with the chief's env as agentspec now builds it.
	o.permissionMode = "bypassPermissions"
	if err := seed(o, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	set := readObj(t, filepath.Join(o.configDir, "settings.json"))
	got, _ := json.Marshal(set["permissions"])
	if want := `{"allow":["mcp__hq__*"],"defaultMode":"bypassPermissions","disableAutoMode":"disable"}`; string(got) != want {
		t.Fatalf("permissions = %s\nwant          %s", got, want)
	}
	if set["skipDangerousModePermissionPrompt"] != true {
		t.Fatalf("the bypass disclaimer consent was not recorded: %v", set["skipDangerousModePermissionPrompt"])
	}
	if err := checkSeed(o); err != nil {
		t.Fatalf("checkSeed disagrees with the chief's bypass seed: %v", err)
	}
	noMode := o
	noMode.permissionMode = ""
	if err := checkSeed(noMode); err == nil {
		t.Fatal("checkSeed without the mode accepted the chief's bypass file")
	}
}
