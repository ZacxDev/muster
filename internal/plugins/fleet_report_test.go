package plugins

import "testing"

// MUSTER_FLEET_REPORT_SENDERS and MUSTER_FLEET_REPORT_MEMBERS (fleet_report.go,
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

func TestParseFleetReportMembers(t *testing.T) {
	m, err := ParseFleetReportMembers(" 7:Example-Member , 12:other-login,")
	if err != nil || len(m) != 2 || m[7] != "Example-Member" || m[12] != "other-login" {
		t.Fatalf("got %v, %v", m, err)
	}
	if m, err := ParseFleetReportMembers(""); err != nil || len(m) != 0 {
		t.Fatalf("empty: %v, %v", m, err)
	}
	for _, bad := range []string{"example-member", "0:example-member", "-1:example-member", "x:example-member",
		"7:", "7:-leading", "7:trailing-", "7:has space", "7:" + "a123456789012345678901234567890123456789", "7:a,7:b"} {
		if _, err := ParseFleetReportMembers(bad); err == nil {
			t.Errorf("%q parsed; want refused", bad)
		}
	}
}

func TestFleetReportMembersMatchTheOwnersLoginOnly(t *testing.T) {
	m := FleetReportMembers{7: "Example-Member"}
	if !m.Is(7, "example-member") || !m.Is(7, "EXAMPLE-MEMBER") {
		t.Fatal("control: the owner's own login (any case) does not match")
	}
	for _, c := range []struct {
		uid   int64
		login string
	}{{7, "other-login"}, {8, "example-member"}, {7, ""}, {7, "example-member "}} {
		if m.Is(c.uid, c.login) {
			t.Errorf("%d %q matched", c.uid, c.login)
		}
	}
}
