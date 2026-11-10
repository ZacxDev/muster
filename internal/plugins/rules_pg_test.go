package plugins

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
)

// ruleAsk is an open card of owner's, as a rule proposal's card is (an
// attention ask; a mirrored asker here, so no agent is needed).
func ruleAsk(t *testing.T, pool *pgxpool.Pool, owner int64) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(authz.System(context.Background()), `INSERT INTO attention_asks (owner_id, mirror_host, mirror_session, question, expires_at)
		VALUES ($1, 'h', 's', 'Approve the rule?', now() + interval '1 day') RETURNING id`, owner).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func ruleNew(t *testing.T, pool *pgxpool.Pool, p Plugin, sender string) RuleNew {
	return RuleNew{PluginID: p.ID, PluginName: p.Name, Match: RuleMatch{Sender: sender, RequireVerified: true},
		Action: RuleSettle, SettleReason: "a newsletter", ChiefAgent: 1, SourceMessage: 1, AskID: ruleAsk(t, pool, p.OwnerID)}
}

// TestARuleIsApprovedOnceAndOnlyByItsOwner: a proposal is approved at most
// once (a second approval, or a decline after it, is ErrRuleDecided and
// changes nothing); another owner cannot read, approve, decline, disable or
// delete it (ErrRuleNotFound); and only an active rule is in ActiveRules.
func TestARuleIsApprovedOnceAndOnlyByItsOwner(t *testing.T) {
	s, pool, _ := pgStore(t)
	_, p := mustCreate(t, s, 2, "rules-inbox")
	r, err := s.CreateRuleProposal(session(2), ruleNew(t, pool, p, "digest@news.example.test"))
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
	if _, err := s.DeleteRule(session(3), r.ID); !errors.Is(err, ErrRuleNotFound) {
		t.Fatalf("another owner deletes it: %v", err)
	}
	if err := s.SetRuleEnabled(session(2), r.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteRule(session(2), r.ID); err != nil {
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
	s, pool, _ := pgStore(t)
	_, p := mustCreate(t, s, 2, "rules-plugin")
	if _, err := s.CreateRuleProposal(plugin(2), ruleNew(t, pool, p, "a@example.test")); !errors.Is(err, ErrRulePlugin) {
		t.Fatalf("a plugin proposes a rule: %v; want ErrRulePlugin", err)
	}
	r, err := s.CreateRuleProposal(session(2), ruleNew(t, pool, p, "a@example.test"))
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
		"delete":  func() error { _, err := s.DeleteRule(plugin(2), r.ID); return err },
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
	s, pool, _ := pgStore(t)
	_, p := mustCreate(t, s, 2, "rules-dup")
	r, err := s.CreateRuleProposal(session(2), ruleNew(t, pool, p, "dup@example.test"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRuleProposal(session(2), ruleNew(t, pool, p, "dup@example.test")); !errors.Is(err, ErrRuleExists) {
		t.Fatalf("a duplicate of a proposed rule: %v; want ErrRuleExists", err)
	}
	if _, err := s.CreateRuleProposal(session(2), ruleNew(t, pool, p, "other@example.test")); err != nil {
		t.Fatalf("another sender: %v", err)
	}
	if _, err := s.DeclineRule(session(2), r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRuleProposal(session(2), ruleNew(t, pool, p, "dup@example.test")); err != nil {
		t.Fatalf("the same match after a decline: %v; want stored", err)
	}
	// Another owner's identical rule is theirs.
	_, q := mustCreate(t, s, 3, "rules-dup")
	if _, err := s.CreateRuleProposal(session(3), ruleNew(t, pool, q, "dup@example.test")); err != nil {
		t.Fatalf("another owner's rule with the same match: %v", err)
	}
}

// TestAProposalWhoseCardWentAwayIsDeclined: a proposed rule whose card was
// cancelled, answered elsewhere or expired no longer waits for a tap — Rules
// shows it declined — and holds neither its match (the same match is proposed
// again) nor a slot of MaxOpenRuleProposals (as many proposals as the cap can
// be made after that many cards went away). A proposal whose card is still
// open keeps both.
func TestAProposalWhoseCardWentAwayIsDeclined(t *testing.T) {
	s, pool, _ := pgStore(t)
	_, p := mustCreate(t, s, 2, "rules-stale")
	sys := authz.System(context.Background())
	open, err := s.CreateRuleProposal(session(2), ruleNew(t, pool, p, "open@example.test"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRuleProposal(session(2), ruleNew(t, pool, p, "open@example.test")); !errors.Is(err, ErrRuleExists) {
		t.Fatalf("the same match while its card is open: %v; want ErrRuleExists", err)
	}
	gone := func(how string) Rule {
		t.Helper()
		r, err := s.CreateRuleProposal(session(2), ruleNew(t, pool, p, how+"@example.test"))
		if err != nil {
			t.Fatal(err)
		}
		q := map[string]string{
			"cancelled": `UPDATE attention_asks SET status = 'cancelled', resolved_at = now() WHERE id = $1`,
			"expired":   `UPDATE attention_asks SET created_at = now() - interval '2 days', expires_at = now() - interval '1 second' WHERE id = $1`,
		}[how]
		if _, err := pool.Exec(sys, q, r.AskID); err != nil {
			t.Fatal(err)
		}
		return r
	}
	cancelled, expired := gone("cancelled"), gone("expired")
	list, err := s.Rules(session(2))
	if err != nil {
		t.Fatal(err)
	}
	states := map[int64]string{}
	for _, r := range list {
		states[r.ID] = r.State
	}
	if states[open.ID] != RuleProposed || states[cancelled.ID] != RuleDeclined || states[expired.ID] != RuleDeclined {
		t.Fatalf("after the cards went away: open %s, cancelled %s, expired %s; want proposed, declined, declined",
			states[open.ID], states[cancelled.ID], states[expired.ID])
	}
	if _, err := s.CreateRuleProposal(session(2), ruleNew(t, pool, p, "cancelled@example.test")); err != nil {
		t.Fatalf("the same match after its card was cancelled: %v; want it proposed again", err)
	}
	// The cap counts only proposals whose card is open: fill it with gone ones.
	for i := 0; i < MaxOpenRuleProposals; i++ {
		r, err := s.CreateRuleProposal(session(2), ruleNew(t, pool, p, "fill-"+time.Now().Format("150405.000000000")+"@example.test"))
		if err != nil {
			t.Fatalf("proposal %d after %d cards went away: %v", i, i, err)
		}
		if _, err := pool.Exec(sys, `UPDATE attention_asks SET status = 'cancelled', resolved_at = now() WHERE id = $1`, r.AskID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateRuleProposal(session(2), ruleNew(t, pool, p, "after-the-fill@example.test")); err != nil {
		t.Fatalf("a proposal after %d cards went away: %v; want it taken", MaxOpenRuleProposals, err)
	}
}

// TestDeletingAProposalAnswersItsCard: DeleteRule takes a rule in any state
// and answers it as it was — its card, for the caller to cancel.
func TestDeletingAProposalAnswersItsCard(t *testing.T) {
	s, pool, _ := pgStore(t)
	_, p := mustCreate(t, s, 2, "rules-delete")
	r, err := s.CreateRuleProposal(session(2), ruleNew(t, pool, p, "del@example.test"))
	if err != nil {
		t.Fatal(err)
	}
	gone, err := s.DeleteRule(session(2), r.ID)
	if err != nil || gone.ID != r.ID || gone.State != RuleProposed || gone.AskID != r.AskID {
		t.Fatalf("DeleteRule = %+v, %v; want the proposed rule and its card", gone, err)
	}
}
