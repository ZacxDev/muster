package main

import (
	"strings"
	"testing"
)

// TestTheFleetReportVariablesAreReadAndABadValueRefusesToBoot:
// MUSTER_FLEET_REPORT_SENDERS (MUSTER_PR_MAIL_SENDERS's grammar) and
// MUSTER_FLEET_REPORT_PATHS reach the config; a value that does not parse, and
// senders with no paths (fail closed), refuse to boot naming the variable.
// Fixture values are invented placeholders.
func TestTheFleetReportVariablesAreReadAndABadValueRefusesToBoot(t *testing.T) {
	base := map[string]string{envDatabase: "postgres://x/y", envUIPassword: strings.Repeat("p", 40)}
	load := func(senders, paths string) (config, error) {
		return loadConfig(func(k string) string {
			switch k {
			case envFleetReportSenders:
				return senders
			case envFleetReportPaths:
				return paths
			}
			return base[k]
		})
	}
	cfg, err := load("standup-desk/report-ask", "/srv/example-work,/opt/example-code/")
	if err != nil {
		t.Fatalf("control: a valid pair refused: %v", err)
	}
	if !cfg.FleetReportSenders["standup-desk/report-ask"] || len(cfg.FleetReportSenders) != 1 ||
		strings.Join(cfg.FleetReportPaths, ",") != "/srv/example-work,/opt/example-code" {
		t.Fatalf("parsed %v %v", cfg.FleetReportSenders, cfg.FleetReportPaths)
	}
	if cfg, err := load("", ""); err != nil || len(cfg.FleetReportSenders) != 0 {
		t.Fatalf("unset: %v %v; want off", cfg.FleetReportSenders, err)
	}
	if _, err := load("", "/srv/example-work"); err != nil {
		t.Fatalf("paths alone (inert) refused: %v", err)
	}
	for _, c := range []struct{ senders, paths, names string }{
		{"standup-desk", "/srv/example-work", envFleetReportSenders},
		{"Standup-Desk/report-ask", "/srv/example-work", envFleetReportSenders},
		{"standup-desk/report-ask", "", envFleetReportPaths},
		{"standup-desk/report-ask", "srv/example-work", envFleetReportPaths},
		{"standup-desk/report-ask", "/", envFleetReportPaths},
	} {
		_, err := load(c.senders, c.paths)
		if err == nil || !strings.Contains(err.Error(), c.names) {
			t.Errorf("senders %q paths %q: %v; want a refusal naming %s", c.senders, c.paths, err, c.names)
		}
	}
}

// D1 (operator decision): there is no members variable any more, so nothing
// about members can refuse a boot — senders and paths alone boot.
func TestTheFleetReportNeedsNoMembersVariable(t *testing.T) {
	for _, name := range []string{"MUSTER_FLEET_REPORT_MEMBERS"} {
		if _, declared := declaredEnvConstants(t)[name]; declared {
			t.Fatalf("%s is declared again", name)
		}
	}
	_, err := loadConfig(func(k string) string {
		return map[string]string{envDatabase: "postgres://x/y", envUIPassword: strings.Repeat("p", 40),
			envFleetReportSenders: "standup-desk/report-ask", envFleetReportPaths: "/srv/example-work"}[k]
	})
	if err != nil {
		t.Fatalf("senders and paths with no members refused: %v", err)
	}
}
