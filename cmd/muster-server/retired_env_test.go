package main

import (
	"strings"
	"testing"
)

// MUSTER_AGENT_PRIVILEGE_APPLY armed the privilege tier, which was removed. The
// live deployment manifest sets it ("1"), and the image that stops reading it
// boots against that manifest before anyone deletes the line — so a stale value
// must neither refuse boot nor be silently swallowed. See retiredEnvNames.

// TestABootWithTheRetiredPrivilegeApplyVariableStillLoadsAndValidates: with no
// provisioner named, the variable used to be a FATAL boot refusal
// (validateProvisioner: "armed applier, no driver"). It must now load, validate
// and produce exactly one warning naming it.
func TestABootWithTheRetiredPrivilegeApplyVariableStillLoadsAndValidates(t *testing.T) {
	for _, prov := range []string{""} {
		env := map[string]string{
			"MUSTER_AGENT_PRIVILEGE_APPLY": "1",
			"DATABASE_URL":                 "postgres://u:p@127.0.0.1:1/x",
			"MUSTER_AGENT_PROVISIONER":     prov,
		}
		get := func(k string) string { return env[k] }
		cfg, err := loadConfig(get)
		if err != nil {
			t.Fatalf("provisioner %q: loadConfig: %v", prov, err)
		}
		if err := cfg.validate(); err != nil {
			t.Fatalf("provisioner %q: validate refused a boot carrying the retired variable: %v", prov, err)
		}
		warnings := retiredEnvWarnings(cfg)
		if len(warnings) != 1 || !strings.Contains(warnings[0], "MUSTER_AGENT_PRIVILEGE_APPLY") ||
			!strings.Contains(warnings[0], "no longer read") {
			t.Fatalf("provisioner %q: want exactly one warning naming MUSTER_AGENT_PRIVILEGE_APPLY, got %q", prov, warnings)
		}
	}
}

// TestNoRetiredVariableNoWarning is the control: an environment without the
// retired name (or with it EMPTY, the way a manifest that blanks it reads) logs
// nothing — otherwise the warning is unconditional text, not a report.
func TestNoRetiredVariableNoWarning(t *testing.T) {
	for _, v := range []string{"", "  "} {
		env := map[string]string{"DATABASE_URL": "postgres://u:p@127.0.0.1:1/x", "MUSTER_AGENT_PRIVILEGE_APPLY": v}
		cfg, err := loadConfig(func(k string) string { return env[k] })
		if err != nil {
			t.Fatal(err)
		}
		if w := retiredEnvWarnings(cfg); len(w) != 0 {
			t.Errorf("value %q: want no warning, got %q", v, w)
		}
	}
}
