package plugins

import (
	"errors"
	"testing"
)

func ruleNew(p Plugin, sender string) RuleNew {
	return RuleNew{PluginID: p.ID, PluginName: p.Name, Match: RuleMatch{Sender: sender, RequireVerified: true},
		Action: RuleSettle, SettleReason: "a newsletter", ChiefAgent: 1, SourceMessage: 1}
}

// TestARuleIsApprovedOnceAndOnlyByItsOwner: a proposal is approved at most
// once (a second approval, or a decline after it, is ErrRuleDecided and
// changes nothing); another owner cannot read, approve, decline, disable or
// delete it (ErrRuleNotFound); and only an active rule is in ActiveRules.
func TestARuleIsApprovedOnceAndOnlyByItsOwner(t *testing.T) {
	s, _, _ := pgStore(t)
	_, p := mustCreate(t, s, 2, "rules-inbox")
	r, err := s.CreateRuleProposal(session(2), ruleNew(p, "digest@news.example.test"))
	if err != nil || r.State != RuleProposed || !r.ApprovedAt.IsZero() {
		t.Fatalf("CreateRuleProposal = %+v, %v; want a proposed rule with no approval", r, err)
	}
	if got, err := s.ActiveRules(session(2), p.ID); err != nil || len(got) != 0 {
		t.Fatalf("a proposed rule in ActiveRules: %v %v", got, err)
	}
	// Another owner sees and decides nothing.
	if _, err := s.RuleByID(session(3), r.ID); !errors.Is(err, ErrRuleNotFound) {
		t.Fatalf("another owner reads the rule: %v", err)
	}
	if _, err := s.ApproveRule(session(3), r.ID); !errors.Is(err, ErrRuleNotFound) {
		t.Fatalf("another owner approves the rule: %v; want ErrRuleNotFound", err)
	}
	if _, err := s.DeclineRule(session(3), r.ID); !errors.Is(err, ErrRuleNotFound) {
		t.Fatalf("another owner declines the rule: %v; want ErrRuleNotFound", err)
	}
	if got, _ := s.RuleByID(session(2), r.ID); got.State != RuleProposed {
		t.Fatalf("after another owner's tries: %s; want still proposed", got.State)
	}
	// The owner approves it: once.
	a, err := s.ApproveRule(session(2), r.ID)
	if err != nil || a.State != RuleActive || a.ApprovedAt.IsZero() || a.DecidedAt.IsZero() {
		t.Fatalf("ApproveRule = %+v, %v", a, err)
	}
	if _, err := s.ApproveRule(session(2), r.ID); !errors.Is(err, ErrRuleDecided) {
		t.Fatalf("a second approval: %v; want ErrRuleDecided", err)
	}
	if _, err := s.DeclineRule(session(2), r.ID); !errors.Is(err, ErrRuleDecided) {
		t.Fatalf("a decline after approval: %v; want ErrRuleDecided", err)
	}
	if got, err := s.ActiveRules(session(2), p.ID); err != nil || len(got) != 1 || got[0].ID != r.ID || !got[0].ApprovedAt.Equal(a.ApprovedAt) {
		t.Fatalf("ActiveRules after approval: %+v %v", got, err)
	}
	// Disabled: out of ActiveRules; another owner can neither enable nor delete it.
	if err := s.SetRuleEnabled(session(2), r.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ActiveRules(session(2), p.ID); len(got) != 0 {
		t.Fatalf("a disabled rule in ActiveRules: %+v", got)
	}
	if err := s.SetRuleEnabled(session(3), r.ID, true); !errors.Is(err, ErrRuleNotFound) {
		t.Fatalf("another owner enables it: %v", err)
	}
	if err := s.DeleteRule(session(3), r.ID); !errors.Is(err, ErrRuleNotFound) {
		t.Fatalf("another owner deletes it: %v", err)
	}
	if err := s.SetRuleEnabled(session(2), r.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRule(session(2), r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RuleByID(session(2), r.ID); !errors.Is(err, ErrRuleNotFound) {
		t.Fatalf("a deleted rule: %v", err)
	}
}

// TestAPluginPrincipalCannotWriteARule: a plugin token acts as its owner, and
// still cannot propose, approve, decline, disable, enable or delete a rule
// (ErrRulePlugin), while the owner's own session can.
func TestAPluginPrincipalCannotWriteARule(t *testing.T) {
	s, _, _ := pgStore(t)
	_, p := mustCreate(t, s, 2, "rules-plugin")
	if _, err := s.CreateRuleProposal(plugin(2), ruleNew(p, "a@example.test")); !errors.Is(err, ErrRulePlugin) {
		t.Fatalf("a plugin proposes a rule: %v; want ErrRulePlugin", err)
	}
	r, err := s.CreateRuleProposal(session(2), ruleNew(p, "a@example.test"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApproveRule(plugin(2), r.ID); !errors.Is(err, ErrRulePlugin) {
		t.Fatalf("a plugin approves a rule: %v; want ErrRulePlugin", err)
	}
	if _, err := s.DeclineRule(plugin(2), r.ID); !errors.Is(err, ErrRulePlugin) {
		t.Fatalf("a plugin declines a rule: %v; want ErrRulePlugin", err)
	}
	if got, _ := s.RuleByID(session(2), r.ID); got.State != RuleProposed {
		t.Fatalf("after the plugin's tries: %s; want still proposed", got.State)
	}
	if _, err := s.ApproveRule(session(2), r.ID); err != nil {
		t.Fatal(err)
	}
	for name, try := range map[string]func() error{
		"disable": func() error { return s.SetRuleEnabled(plugin(2), r.ID, false) },
		"delete":  func() error { return s.DeleteRule(plugin(2), r.ID) },
	} {
		if err := try(); !errors.Is(err, ErrRulePlugin) {
			t.Errorf("a plugin %ss a rule: %v; want ErrRulePlugin", name, err)
		}
	}
	// The dispatch's counters run on the plugin's principal (it acts as the owner).
	if err := s.RecordRuleHit(plugin(2), r.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRuleFallback(plugin(2), r.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.RuleByID(session(2), r.ID); got.Hits != 1 || got.Fallbacks != 1 || got.LastHitAt.IsZero() || got.State != RuleActive {
		t.Fatalf("after one hit and one fallback: %+v", got)
	}
	// Another owner's plugin counts nothing on this owner's rule.
	if err := s.RecordRuleHit(plugin(3), r.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.RuleByID(session(2), r.ID); got.Hits != 1 {
		t.Fatalf("another owner's hit counted: %d", got.Hits)
	}
}

// TestADuplicateRuleIsRefused: the same plugin and match is refused while a
// rule with it is proposed, active or disabled — and taken again once that one
// is declined or deleted.
func TestADuplicateRuleIsRefused(t *testing.T) {
	s, _, _ := pgStore(t)
	_, p := mustCreate(t, s, 2, "rules-dup")
	r, err := s.CreateRuleProposal(session(2), ruleNew(p, "dup@example.test"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRuleProposal(session(2), ruleNew(p, "dup@example.test")); !errors.Is(err, ErrRuleExists) {
		t.Fatalf("a duplicate of a proposed rule: %v; want ErrRuleExists", err)
	}
	if _, err := s.CreateRuleProposal(session(2), ruleNew(p, "other@example.test")); err != nil {
		t.Fatalf("another sender: %v", err)
	}
	if _, err := s.DeclineRule(session(2), r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRuleProposal(session(2), ruleNew(p, "dup@example.test")); err != nil {
		t.Fatalf("the same match after a decline: %v; want stored", err)
	}
	// Another owner's identical rule is theirs.
	_, q := mustCreate(t, s, 3, "rules-dup")
	if _, err := s.CreateRuleProposal(session(3), ruleNew(q, "dup@example.test")); err != nil {
		t.Fatalf("another owner's rule with the same match: %v", err)
	}
}
