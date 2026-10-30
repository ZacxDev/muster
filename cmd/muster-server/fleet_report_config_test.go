package main

import (
	"strings"
	"testing"
)

// TestTheFleetReportVariablesAreReadAndABadValueRefusesToBoot:
// MUSTER_FLEET_REPORT_SENDERS (MUSTER_PR_MAIL_SENDERS's grammar) and
// MUSTER_FLEET_REPORT_MEMBERS reach the config; a value that does not parse,
// and senders with no members, refuse to boot naming the variable. Fixture
// values are invented.
func TestTheFleetReportVariablesAreReadAndABadValueRefusesToBoot(t *testing.T) {
	base := map[string]string{envDatabase: "postgres://x/y", envUIPassword: strings.Repeat("p", 40)}
	load := func(senders, members string) (config, error) {
		return loadConfig(func(k string) string {
			switch k {
			case envFleetReportSenders:
				return senders
			case envFleetReportMembers:
				return members
			}
			return base[k]
		})
	}
	cfg, err := load("standup-desk/report-ask", "7:Example-Member")
	if err != nil {
		t.Fatalf("control: a valid pair refused: %v", err)
	}
	if !cfg.FleetReportSenders["standup-desk/report-ask"] || len(cfg.FleetReportSenders) != 1 || cfg.FleetReportMembers[7] != "Example-Member" {
		t.Fatalf("parsed %v %v", cfg.FleetReportSenders, cfg.FleetReportMembers)
	}
	if cfg, err := load("", ""); err != nil || len(cfg.FleetReportSenders) != 0 {
		t.Fatalf("unset: %v %v; want off", cfg.FleetReportSenders, err)
	}
	if _, err := load("", "7:Example-Member"); err != nil {
		t.Fatalf("members alone (inert) refused: %v", err)
	}
	for _, c := range []struct{ senders, members, names string }{
		{"standup-desk", "7:Example-Member", envFleetReportSenders},
		{"Standup-Desk/report-ask", "7:Example-Member", envFleetReportSenders},
		{"standup-desk/report-ask", "", envFleetReportMembers},
		{"standup-desk/report-ask", "seven:Example-Member", envFleetReportMembers},
		{"standup-desk/report-ask", "7:not a login", envFleetReportMembers},
	} {
		_, err := load(c.senders, c.members)
		if err == nil || !strings.Contains(err.Error(), c.names) {
			t.Errorf("senders %q members %q: %v; want a refusal naming %s", c.senders, c.members, err, c.names)
		}
	}
}
