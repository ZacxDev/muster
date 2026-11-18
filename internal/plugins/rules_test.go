package plugins

import (
	"strings"
	"testing"
	"time"
)

// Invented fixtures throughout: no value here is a real sender, thread or id.

var ruleT0 = time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)

func ruleMsg(mut func(*Message)) Message {
	thread := "thr-0042"
	m := Message{ID: 501, PluginID: 7, OwnerID: 3, ExternalID: "news-2026-03-04-0001", Sender: "digest@news.example.test",
		SenderVerified: true, Subject: "Weekly digest: widgets", Body: "hello", Thread: &thread, CreatedAt: ruleT0.Add(time.Hour)}
	if mut != nil {
		mut(&m)
	}
	return m
}

func activeRule(mut func(*Rule)) Rule {
	r := Rule{ID: 11, PluginID: 7, PluginName: "example-mail", Action: RuleSettle, SettleReason: "a newsletter",
		State: RuleActive, ApprovedAt: ruleT0, Match: RuleMatch{Sender: "digest@news.example.test", RequireVerified: true}}
	if mut != nil {
		mut(&r)
	}
	return r
}

// TestRuleMatchesPinnedCases is the matcher's table, each case a literal: the
// fields that match exactly or by prefix; and every case that must NOT match.
func TestRuleMatchesPinnedCases(t *testing.T) {
	for _, c := range []struct {
		name string
		rule Rule
		msg  Message
		want bool
	}{
		{"the exact sender", activeRule(nil), ruleMsg(nil), true},
		{"another sender", activeRule(nil), ruleMsg(func(m *Message) { m.Sender = "other@news.example.test" }), false},
		{"the sender, differently cased, is another sender", activeRule(nil), ruleMsg(func(m *Message) { m.Sender = "Digest@news.example.test" }), false},
		{"the sender, unverified, when the rule requires verification", activeRule(nil), ruleMsg(func(m *Message) { m.SenderVerified = false }), false},
		{"an unverified sender a settle rule from an unverified message allows", activeRule(func(r *Rule) { r.Match.RequireVerified = false }),
			ruleMsg(func(m *Message) { m.SenderVerified = false }), true},
		{"a ROUTE rule never takes an unverified sender, whatever it requires", activeRule(func(r *Rule) {
			r.Action, r.SettleReason, r.RouteSession, r.Match.RequireVerified = RuleRoute, "", "ses-0001", false
		}), ruleMsg(func(m *Message) { m.SenderVerified = false }), false},
		{"a route rule takes a verified sender", activeRule(func(r *Rule) { r.Action, r.SettleReason, r.RouteSession = RuleRoute, "", "ses-0001" }),
			ruleMsg(nil), true},
		{"an external_id prefix", activeRule(func(r *Rule) {
			r.Match = RuleMatch{Sender: "digest@news.example.test", RequireVerified: true, ExternalPrefix: "news-2026-"}
		}), ruleMsg(nil), true},
		{"an external_id prefix ALONE matches nothing (unanchored)", activeRule(func(r *Rule) { r.Match = RuleMatch{ExternalPrefix: "news-2026-"} }), ruleMsg(nil), false},
		{"a subject prefix ALONE matches nothing (unanchored)", activeRule(func(r *Rule) { r.Match = RuleMatch{SubjectPrefix: "Weekly digest"} }), ruleMsg(nil), false},
		{"a ROUTE rule on a subject prefix alone takes no sender at all", activeRule(func(r *Rule) {
			r.Action, r.SettleReason, r.RouteSession, r.Match = RuleRoute, "", "ses-0001", RuleMatch{SubjectPrefix: "Weekly digest", RequireVerified: true}
		}), ruleMsg(nil), false},
		{"a thread rule takes any sender on that thread", activeRule(func(r *Rule) { r.Match = RuleMatch{Thread: "thr-0042"} }),
			ruleMsg(func(m *Message) { m.Sender, m.SenderVerified = "someone@else.example.test", false }), true},
		{"an external_id prefix that is not one", activeRule(func(r *Rule) {
			r.Match = RuleMatch{Sender: "digest@news.example.test", RequireVerified: true, ExternalPrefix: "ews-2026"}
		}), ruleMsg(nil), false},
		{"a subject prefix", activeRule(func(r *Rule) {
			r.Match = RuleMatch{Sender: "digest@news.example.test", RequireVerified: true, SubjectPrefix: "Weekly digest"}
		}), ruleMsg(nil), true},
		{"a subject prefix in the middle is no prefix", activeRule(func(r *Rule) {
			r.Match = RuleMatch{Sender: "digest@news.example.test", RequireVerified: true, SubjectPrefix: "digest:"}
		}), ruleMsg(nil), false},
		{"the thread", activeRule(func(r *Rule) { r.Match = RuleMatch{Thread: "thr-0042"} }), ruleMsg(nil), true},
		{"no thread on the message", activeRule(func(r *Rule) { r.Match = RuleMatch{Thread: "thr-0042"} }), ruleMsg(func(m *Message) { m.Thread = nil }), false},
		{"every field must match", activeRule(func(r *Rule) { r.Match.SubjectPrefix = "Monthly" }), ruleMsg(nil), false},
		{"a DISABLED rule", activeRule(func(r *Rule) { r.State = RuleDisabled }), ruleMsg(nil), false},
		{"a PROPOSED rule", activeRule(func(r *Rule) { r.State, r.ApprovedAt = RuleProposed, time.Time{} }), ruleMsg(nil), false},
		{"a message stored BEFORE the approval", activeRule(nil), ruleMsg(func(m *Message) { m.CreatedAt = ruleT0.Add(-time.Second) }), false},
		{"a message stored AT the approval", activeRule(nil), ruleMsg(func(m *Message) { m.CreatedAt = ruleT0 }), false},
		{"another plugin's message", activeRule(nil), ruleMsg(func(m *Message) { m.PluginID = 8 }), false},
		{"a task event", activeRule(nil), ruleMsg(func(m *Message) { id := int64(9); m.TaskID = &id }), false},
		{"a rule matching on nothing", activeRule(func(r *Rule) { r.Match = RuleMatch{} }), ruleMsg(nil), false},
	} {
		if got := RuleMatches(c.rule, c.msg); got != c.want {
			t.Errorf("%s: RuleMatches = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestFirstRuleMatchTakesTheOldestThatMatches.
func TestFirstRuleMatchTakesTheOldestThatMatches(t *testing.T) {
	disabled := activeRule(func(r *Rule) { r.ID, r.State = 10, RuleDisabled })
	other := activeRule(func(r *Rule) { r.ID, r.Match.Sender = 11, "nobody@news.example.test" })
	first := activeRule(func(r *Rule) { r.ID = 12 })
	second := activeRule(func(r *Rule) {
		r.ID, r.Match = 13, RuleMatch{Sender: "digest@news.example.test", RequireVerified: true, SubjectPrefix: "Weekly"}
	})
	got, ok := FirstRuleMatch([]Rule{disabled, other, first, second}, ruleMsg(nil))
	if !ok || got.ID != 12 {
		t.Fatalf("FirstRuleMatch = %d %v; want rule 12 (10 disabled, 11 another sender)", got.ID, ok)
	}
	if _, ok := FirstRuleMatch([]Rule{disabled, other}, ruleMsg(nil)); ok {
		t.Fatal("a match among a disabled rule and a non-matching one")
	}
}

// TestProposeRuleCopiesFromTheMessageAndRefusesWhatItCannotMatch: every
// matched value is the message's own; the refusals each name their reason.
func TestProposeRuleCopiesFromTheMessageAndRefusesWhatItCannotMatch(t *testing.T) {
	p := Plugin{ID: 7, Name: "example-mail"}
	n, err := ProposeRule(p, ruleMsg(nil), RuleRequest{MatchSender: true, SubjectPrefix: "Weekly digest", Action: RuleSettle, Reason: "  a newsletter "})
	if err != nil {
		t.Fatal(err)
	}
	want := RuleNew{PluginID: 7, PluginName: "example-mail", Action: RuleSettle, SettleReason: "a newsletter", SourceMessage: 501,
		Match: RuleMatch{Sender: "digest@news.example.test", SubjectPrefix: "Weekly digest", RequireVerified: true}}
	if n != want {
		t.Fatalf("ProposeRule = %+v\nwant %+v", n, want)
	}
	// An unverified message: a settle rule that does NOT require verification.
	n, err = ProposeRule(p, ruleMsg(func(m *Message) { m.SenderVerified = false }), RuleRequest{MatchSender: true, Action: RuleSettle, Reason: "noise"})
	if err != nil || n.Match.RequireVerified {
		t.Fatalf("a settle rule from an unverified message: %+v %v; want one that does not require verification", n, err)
	}
	for _, c := range []struct {
		name string
		msg  Message
		req  RuleRequest
		want string
	}{
		{"a route from an unverified sender", ruleMsg(func(m *Message) { m.SenderVerified = false }),
			RuleRequest{MatchSender: true, Action: RuleRoute, Session: "ses-0001"}, "muster never routes an unverified sender by rule"},
		{"nothing to match on", ruleMsg(nil), RuleRequest{Action: RuleSettle, Reason: "x"}, "match on the message's sender or its thread"},
		{"an external_id prefix alone", ruleMsg(nil), RuleRequest{ExternalPrefix: "news-2026-", Action: RuleSettle, Reason: "x"}, "match on the message's sender or its thread"},
		{"a subject prefix alone", ruleMsg(nil), RuleRequest{SubjectPrefix: "Weekly digest", Action: RuleSettle, Reason: "x"}, "match on the message's sender or its thread"},
		{"a prefix that is not the message's", ruleMsg(nil), RuleRequest{MatchSender: true, ExternalPrefix: "other-2026-", Action: RuleSettle, Reason: "x"}, "external_id_prefix"},
		{"a seven-byte prefix", ruleMsg(nil), RuleRequest{MatchSender: true, ExternalPrefix: "news-20", Action: RuleSettle, Reason: "x"}, "external_id_prefix must be 8 to 200 bytes"},
		{"a subject prefix that is not the message's", ruleMsg(nil), RuleRequest{MatchSender: true, SubjectPrefix: "Daily", Action: RuleSettle, Reason: "x"}, "subject_prefix"},
		{"a thread the message lacks", ruleMsg(func(m *Message) { m.Thread = nil }), RuleRequest{MatchThread: true, Action: RuleSettle, Reason: "x"}, "no thread"},
		{"a settle with no reason", ruleMsg(nil), RuleRequest{MatchSender: true, Action: RuleSettle}, "reason"},
		{"a route with no session", ruleMsg(nil), RuleRequest{MatchSender: true, Action: RuleRoute}, "names the session"},
		{"an unknown action", ruleMsg(nil), RuleRequest{MatchSender: true, Action: "drop"}, `action is "route" or "settle"`},
		{"a task event", ruleMsg(func(m *Message) { id := int64(9); m.TaskID = &id }), RuleRequest{MatchSender: true, Action: RuleSettle, Reason: "x"}, "task event"},
		{"another plugin's message", ruleMsg(func(m *Message) { m.PluginID = 8 }), RuleRequest{MatchSender: true, Action: RuleSettle, Reason: "x"}, "not that plugin's"},
	} {
		_, err := ProposeRule(p, c.msg, c.req)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v; want a refusal naming %q", c.name, err, c.want)
		}
	}
}

// TestRuleSummaryNamesEveryField: the card and the Rules page read it.
func TestRuleSummaryNamesEveryField(t *testing.T) {
	got := RuleSummary(RuleMatch{Sender: "a@example.test", Thread: "t-1", ExternalPrefix: "news-2026-", SubjectPrefix: "Weekly", RequireVerified: true})
	want := `the sender is exactly "a@example.test" and its plugin verified it, the thread is exactly "t-1", the external id starts with "news-2026-", the subject starts with "Weekly"`
	if got != want {
		t.Fatalf("RuleSummary =\n%s\nwant\n%s", got, want)
	}
	// Exact about the sender in every case the owner can be shown.
	for m, want := range map[RuleMatch]string{
		{Sender: "a@example.test"}:             `the sender is exactly "a@example.test" (not verified: anyone can send as it)`,
		{Thread: "t-1", RequireVerified: true}: `the sender is anyone its plugin verified, the thread is exactly "t-1"`,
		{Thread: "t-1"}:                        `the sender is anyone, verified or not, the thread is exactly "t-1"`,
	} {
		if got := RuleSummary(m); got != want {
			t.Errorf("RuleSummary(%+v) = %q, want %q", m, got, want)
		}
	}
}
