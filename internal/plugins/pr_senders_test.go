package plugins

import "testing"

// MUSTER_PR_MAIL_SENDERS (pr_sessions.go): whose mail is read as a pull-request
// review. Fixture addresses are invented, and distinct from the one the
// operator configures, so no test passes by spelling the deployment's value.

func TestParsePRMailSenders(t *testing.T) {
	s, err := ParsePRMailSenders(" review-gate/pr-check , other-org/lint-bot ,")
	if err != nil || len(s) != 2 || !s["review-gate/pr-check"] || !s["other-org/lint-bot"] {
		t.Fatalf("got %v, %v", s, err)
	}
	if s, err := ParsePRMailSenders(""); err != nil || len(s) != 0 {
		t.Fatalf("empty: %v, %v; want an empty set (the feature off)", s, err)
	}
	for _, bad := range []string{"review-gate", "review-gate/*", "Review-Gate/pr-check", "x@example.invalid",
		"review-gate/pr check", "1:agent-mail:review-gate/pr-check", "review-gate/pr-check/extra"} {
		if _, err := ParsePRMailSenders(bad); err == nil {
			t.Errorf("%q parsed; want refused (an exact agent-mail <owner>/<handle> only)", bad)
		}
	}
}

func TestPRMailSendersAllowOnlyAVerifiedAgentMailSenderOnTheList(t *testing.T) {
	s, err := ParsePRMailSenders("review-gate/pr-check")
	if err != nil {
		t.Fatal(err)
	}
	// Control: the one combination that is allowed, so every refusal below is
	// the guard's and not a set that matches nothing.
	if !s.Allows(AgentMailPlugin, "review-gate/pr-check", true) {
		t.Fatal("control: a verified listed sender through agent mail is refused")
	}
	cases := []struct {
		name, plugin, sender string
		verified             bool
	}{
		{"unverified", AgentMailPlugin, "review-gate/pr-check", false},
		{"another plugin", "discord-dm", "review-gate/pr-check", true},
		{"another handle of the same owner", AgentMailPlugin, "review-gate/lint-bot", true},
		{"an unlisted owner", AgentMailPlugin, "elsewhere/pr-check", true},
		{"a look-alike in a display name", AgentMailPlugin, "review-gate/pr-check <x@example.invalid>", true},
		{"upper case", AgentMailPlugin, "Review-Gate/pr-check", true},
	}
	for _, c := range cases {
		if s.Allows(c.plugin, c.sender, c.verified) {
			t.Errorf("%s: allowed", c.name)
		}
	}
	var none PRMailSenders
	if none.Allows(AgentMailPlugin, "review-gate/pr-check", true) {
		t.Fatal("an empty list allows a sender")
	}
}
