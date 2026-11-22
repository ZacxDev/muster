package api

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/users"
)

// THE STEP-UP CARDS' DRAFTS, READ IN ONE BATCH WITH NO SWEEP
// (stepUpDraftsFor over plugins.DraftCardsByID). A read that does not sweep
// sees each draft as stored, so what the sweep would have settled first must
// be settled here, in the read's own terms:
//   - a draft past its expiry that no sweep has expired yet draws no decision
//     (plugins.DraftTimeDecided);
//   - a draft the reader can READ but may not DECIDE draws none
//     (draftDeciderRefusal);
//   - a sibling reply whose lease lapsed unswept still warns that the sender
//     may get both (plugins unconfirmedSQL).

// TestAnExpiredButUnsweptDraftGetsNoControls: the card's draft is pending in
// its row but past its expiry (no sweep has run): neither the Open feed's card
// nor the card's page draws its Approve or Reject. The control: before the
// expiry moved, both drew them.
func TestAnExpiredButUnsweptDraftGetsNoControls(t *testing.T) {
	f := newStepUpDraftFixture(t, "approve")
	if !strings.Contains(f.page(t, askPagePath(f.card)), "data-draft-approve") {
		t.Fatal("control: the card's page draws no Approve before the draft expired")
	}
	// A draft's expiry is immutable (0016's trigger): it is moved here past
	// that guard, as time itself would move it.
	dbtest.OwnerExec(t, dbtest.DSN(t), `ALTER TABLE plugin_reply_drafts DISABLE TRIGGER plugin_reply_drafts_body_immutable; `+
		`UPDATE plugin_reply_drafts SET expires_at = now() - interval '1 minute' WHERE id = `+strconv.FormatInt(f.draft.ID, 10)+`; `+
		`ALTER TABLE plugin_reply_drafts ENABLE TRIGGER plugin_reply_drafts_body_immutable`)
	if r := f.row(t, f.draft.ID); r.status != "pending" {
		t.Fatalf("the draft is not left unswept: %+v", r)
	}
	for face, html := range map[string]string{
		"the card's page":      f.page(t, askPagePath(f.card)),
		"the Open feed's card": feedCard(f.page(t, attentionPath), f.card),
	} {
		if strings.Contains(html, "data-draft-approve") || strings.Contains(html, "data-draft-reject") {
			t.Errorf("%s: draws a decision on an expired draft", face)
		}
	}
}

// TestADraftTheReaderMayReadButNotDecideGetsNoControls: a team's draft is
// readable by the team's OPERATOR (row-level security, 0127) but only its
// manager decides it: the batch reads it (it names the card's chip) and
// offers no decision on it. The manager, the control, is offered one.
func TestADraftTheReaderMayReadButNotDecideGetsNoControls(t *testing.T) {
	f := newAttnFixture(t)
	sys := authz.System(context.Background())
	person := func(n string) int64 {
		var id int64
		if err := f.pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, n+strconv.FormatInt(time.Now().UnixNano(), 36)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = f.pool.Exec(sys, `DELETE FROM users WHERE id = $1`, id) })
		return id
	}
	mgr, op := person("sd-mgr-"), person("sd-op-")
	us := users.NewPG(f.pool)
	team, err := us.CreateTeam(sys, users.Actor{UserID: 1, Via: authz.ViaSession}, "sd-team-"+strconv.FormatInt(time.Now().UnixNano(), 36), mgr)
	if err != nil {
		t.Fatal(err)
	}
	if err := us.SetTeamMember(sys, users.Actor{UserID: 1, Via: authz.ViaSession}, team.ID, op, authz.TeamOperator); err != nil {
		t.Fatal(err)
	}
	var agent, plugin, draft int64
	if err := f.pool.QueryRow(sys, `INSERT INTO agents (owner_id, name, namespace) VALUES ($1, $2, $2) RETURNING id`, team.ID, "sd-agent-"+strconv.FormatInt(team.ID, 10)).Scan(&agent); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(sys, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope) VALUES ($1, 'sd-x', 'X', md5($2) || md5($2), 'sd-x') RETURNING id`,
		team.ID, strconv.FormatInt(team.ID, 10)).Scan(&plugin); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(sys, `INSERT INTO plugin_reply_drafts (owner_id, agent_id, agent_name, body, expires_at, plugin_id, venue, target_kind, officer, check_state, checked_at)
		VALUES ($1, $2, 'a', 'The team post.', now() + interval '1 hour', $3, 'x', 'post', 'marketing:team', 'sendable', now()) RETURNING id`, team.ID, agent, plugin).Scan(&draft); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM plugin_reply_drafts WHERE owner_id = $1`, `DELETE FROM plugins WHERE owner_id = $1`, `DELETE FROM agents WHERE owner_id = $1`} {
			_, _ = f.pool.Exec(sys, q, team.ID)
		}
	})
	as := func(uid int64) context.Context {
		a, err := us.Account(sys, uid)
		if err != nil {
			t.Fatal(err)
		}
		return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: a.Role, Via: authz.ViaSession, Teams: a.Teams})
	}
	decide, read := f.srv.stepUpDraftsFor(as(op), []int64{draft})
	if _, ok := read[draft]; !ok {
		t.Fatalf("control: the team's operator cannot read the team's draft %d at all", draft)
	}
	if decide[draft] != nil {
		t.Errorf("the team's operator is offered a decision on the team's draft (only its manager decides)")
	}
	if decide, _ := f.srv.stepUpDraftsFor(as(mgr), []int64{draft}); decide[draft] == nil {
		t.Error("control: the team's manager is offered no decision on the team's draft")
	}
}

// TestALapsedSiblingLeaseStillWarnsOnTheCardsPage: a reply draft's card, whose
// sibling reply to the same message was approved, claimed, and its lease
// lapsed with no sweep since (status still 'approved'): the card's page — read
// through the batch, which does not sweep — still warns that the sender may
// get both. 🔴 RED at b3e33cd99: the warning counted only a stored
// 'unconfirmed', which only a sweep writes.
func TestALapsedSiblingLeaseStillWarnsOnTheCardsPage(t *testing.T) {
	f := newAttnFixture(t)
	sys := authz.System(context.Background())
	var plugin, msg, sibling, draft int64
	if err := f.pool.QueryRow(sys, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope) VALUES (1, 'sd-inbox', 'Inbox', md5('sd-inbox') || md5('i'), 'sd-inbox') RETURNING id`).Scan(&plugin); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(sys, `INSERT INTO plugin_messages (owner_id, plugin_id, external_id, sender, body, status, delivered_at, agent_id)
		VALUES (1, $1, 'sd-1', 'someone', 'Noon?', 'delivered', now(), $2) RETURNING id`, plugin, f.op.ID).Scan(&msg); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(sys, `INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body, expires_at, status, decided_at, approved_at, claimed_at, lease_until)
		VALUES (1, $1, $2, $3, 'Yes, noon.', now() + interval '1 hour', 'approved', now() - interval '10 minutes', now() - interval '10 minutes', now() - interval '5 minutes', now() - interval '1 minute') RETURNING id`,
		msg, f.op.ID, f.op.Name).Scan(&sibling); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(sys, `INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body, expires_at)
		VALUES (1, $1, $2, $3, 'Noon works.', now() + interval '1 hour') RETURNING id`, msg, f.op.ID, f.op.Name).Scan(&draft); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(sys, `DELETE FROM plugin_reply_drafts WHERE message_id = $1`, msg)
		_, _ = f.pool.Exec(sys, `DELETE FROM plugin_messages WHERE id = $1`, msg)
		_, _ = f.pool.Exec(sys, `DELETE FROM plugins WHERE id = $1`, plugin)
	})
	card, err := f.attn.Create(asUser(1), attention.New{Asker: attention.Asker{System: true, Label: hostDecisionLabel}, Options: []string{hostDecisionConfirm, hostDecisionRefuse},
		StepUp: &attention.StepUpCard{Tool: pluginDraftDecideTool, Title: "Reply? · Draft #" + strconv.FormatInt(draft, 10), Why: "a reply",
			Asker: "work:1", Preview: "Noon works.", DraftID: draft, Details: "A session asks to approve the draft."}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(sys, `DELETE FROM attention_asks WHERE id = $1`, card.ID) })
	var status string
	if err := f.pool.QueryRow(sys, `SELECT status FROM plugin_reply_drafts WHERE id = $1`, sibling).Scan(&status); err != nil || status != "approved" {
		t.Fatalf("the sibling is not left unswept: %q %v", status, err)
	}
	page := f.page(t, askPagePath(card.ID))
	if !strings.Contains(page, "data-draft-approve") {
		t.Fatalf("control: the card's page draws no decision on the reply draft")
	}
	if !strings.Contains(page, "data-draft-twice") {
		t.Error("the card's page does not warn that the sender may get both (a sibling's lease lapsed, unswept)")
	}
}
