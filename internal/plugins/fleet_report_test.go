package plugins

import "testing"

// MUSTER_FLEET_REPORT_SENDERS and MUSTER_FLEET_REPORT_PATHS (fleet_report.go,
// pr_sessions.go's AgentMailSenders). Fixture values are invented: no test
// passes by spelling a deployment's value.

func TestTheFleetReportSendersAreThePRListsGrammar(t *testing.T) {
	s, err := ParseAgentMailSenders(" standup-desk/report-ask ,")
	if err != nil || len(s) != 1 || !s.Allows(AgentMailPlugin, "standup-desk/report-ask", true) {
		t.Fatalf("got %v, %v", s, err)
	}
	if s.Allows(AgentMailPlugin, "standup-desk/report-ask", false) || s.Allows("discord-dm", "standup-desk/report-ask", true) {
		t.Fatal("an unverified sender, or another plugin's, is allowed")
	}
	for _, bad := range []string{"standup-desk", "standup-desk/*", "Standup-Desk/report-ask", "1:agent-mail:standup-desk/report-ask"} {
		if _, err := ParseAgentMailSenders(bad); err == nil {
			t.Errorf("%q parsed; want refused", bad)
		}
	}
}

func TestParseFleetReportPaths(t *testing.T) {
	p, err := ParseFleetReportPaths(" /srv/example-work/ , /opt/example-code,")
	if err != nil || len(p) != 2 || p[0] != "/srv/example-work" || p[1] != "/opt/example-code" {
		t.Fatalf("got %v, %v", p, err)
	}
	if p, err := ParseFleetReportPaths(""); err != nil || len(p) != 0 {
		t.Fatalf("empty: %v, %v", p, err)
	}
	for _, bad := range []string{"srv/example-work", "~/code", "/", "//", "/srv/../etc", "/srv/./x"} {
		if _, err := ParseFleetReportPaths(bad); err == nil {
			t.Errorf("%q parsed; want refused", bad)
		}
	}
}

// 🔴 A PREFIX MATCHES ON A PATH-COMPONENT BOUNDARY: inside, sibling-prefix,
// outside, unknown.
func TestFleetReportPathsMatchOnAComponentBoundary(t *testing.T) {
	p := FleetReportPaths{"/srv/example-work"}
	for _, in := range []string{"/srv/example-work", "/srv/example-work/", "/srv/example-work/app", "/srv/example-work/a/b/../c"} {
		if !p.Contains(in) {
			t.Errorf("%q: not contained; want contained", in)
		}
	}
	for _, out := range []string{"/srv/example-work-x", "/srv/example-workshop/app", "/srv/example", "/srv", "/other/srv/example-work",
		"/srv/example-work/../secret", "", "srv/example-work/app", "relative"} {
		if p.Contains(out) {
			t.Errorf("%q: contained; want not", out)
		}
	}
	var none FleetReportPaths
	if none.Contains("/srv/example-work") {
		t.Fatal("an empty list contains a directory")
	}
}
