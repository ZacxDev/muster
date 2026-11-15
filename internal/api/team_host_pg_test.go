package api

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
)

// teamHostTools at the call (team sharing 2b.2), over a real Postgres
// (newTeamHostFixture: team_host_door_pg_test.go).

// rowsFingerprint is a count of every row the refused tools could write or
// change: asks (and the answered/cancelled ones), tasks and comments, agents,
// drafts, audit rows and mirror tokens.
func rowsFingerprint(t *testing.T, f *attnFixture) string {
	t.Helper()
	var parts []string
	for _, q := range []string{
		`SELECT count(*) FROM attention_asks`,
		`SELECT count(*) FROM attention_asks WHERE status <> 'open'`,
		`SELECT count(*) FROM notes`,
		`SELECT count(*) FROM agents`,
		`SELECT count(*) FROM audit_log`,
		`SELECT count(*) FROM plugin_reply_drafts`,
		`SELECT count(*) FROM plugin_reply_drafts WHERE status <> 'pending'`,
	} {
		var n int
		if err := f.pool.QueryRow(authz.System(context.Background()), q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		parts = append(parts, strconv.Itoa(n))
	}
	return strings.Join(parts, "/")
}

// TestATeamHostRefusesEachRefusedToolAtTheCall: EVERY tool teamHostTools
// refuses, called by name from a session the team host lists, answers the
// teamHostTools refusal as a tool error — listed or not on this deployment —
// and no row changes (no answer, no card, no draft decided, no agent touched).
// The CONTROL is a kept tool (attention_ask), which runs as the team.
func TestATeamHostRefusesEachRefusedToolAtTheCall(t *testing.T) {
	f, team, teamTok, _ := newTeamHostFixture(t)
	var refusedNames []string
	for n, r := range teamHostTools {
		if !r.keep {
			refusedNames = append(refusedNames, n)
		}
	}
	sort.Strings(refusedNames)
	if len(refusedNames) < 25 {
		t.Fatalf("instrument check FAILED: %d refused tools", len(refusedNames))
	}
	before := rowsFingerprint(t, f)
	args := map[string]any{"id": 1, "answer": "x", "question": "q?", "approve": true, "name": "x", "body": "x", "text": "x", "slug": "x", "decision": "approve"}
	for _, n := range refusedNames {
		_, r, body := f.mirrorRPC(t, mirrorAs(teamTok, focusSID), mcpToolCall(n, args))
		if r == nil || !strings.Contains(string(r.Result), `"isError":true`) || !strings.Contains(body, teamHostToolRefusal(n)) {
			t.Errorf("%s from a team host: %s; want the teamHostTools refusal", n, body)
		}
	}
	if after := rowsFingerprint(t, f); after != before {
		t.Fatalf("a refused tool changed rows: %s -> %s", before, after)
	}
	// CONTROL: a kept tool runs, as the team.
	_, r, body := f.mirrorRPC(t, mirrorAs(teamTok, focusSID), mcpToolCall(attentionAskTool, map[string]any{"question": "Ship build-01?", "options": []string{"yes", "no"}}))
	if text, isErr := toolText(t, r); r == nil || isErr {
		t.Fatalf("CONTROL: attention_ask from a team host: %s", body)
	} else {
		var a struct{ ID int64 }
		_ = json.Unmarshal([]byte(text), &a)
		var owner int64
		if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT owner_id FROM attention_asks WHERE id = $1`, a.ID).Scan(&owner); err != nil || owner != team {
			t.Fatalf("CONTROL: the team host's ask %d is owned by %d (%v); want the team %d", a.ID, owner, err, team)
		}
	}
}

// TestATeamHostCannotAnswerATeamAskThatIsNotItsOwn (the phase 4.7 seam,
// agreed with that session): 2b.2 adds NO answer path for team asks. A team
// host's session cannot answer, wait on or cancel any team-owned ask that is
// not its own question — a System card shaped like a dispatch approval card,
// a team agent's ask, and another session's ask on the same host alike:
// attention_answer is refused by teamHostTools, and attention_wait /
// attention_cancel are bound to the session's own asks (ownAsk). Every one
// stays open and unanswered. The CONTROL is the session's own ask, which it
// cancels.
func TestATeamHostCannotAnswerATeamAskThatIsNotItsOwn(t *testing.T) {
	f, team, teamTok, _ := newTeamHostFixture(t)
	asTeam := authz.WithPrincipal(context.Background(), teamActingPrincipal(team, authz.ViaAttention))
	worker := f.agentFor(t, team, "teamhost-worker-"+strconv.FormatInt(team, 10), "", "teamhost-worker-tok-"+strconv.FormatInt(team, 10))
	var ids []int64
	for _, n := range []attention.New{
		{Asker: attention.Asker{System: true, Label: "dispatch"}, Question: "Start a worker on build-01?", Options: []string{"approve", "deny"}},
		{Asker: attention.Asker{AgentID: worker.ID}, Question: "Which branch?", Options: []string{"main", "release"}},
		{Asker: attention.Asker{MirrorHost: "ws", MirrorSession: secondSID}, Question: "Another session's question?", Options: []string{"a", "b"}},
	} {
		a, err := f.attn.Create(asTeam, n)
		if err != nil {
			t.Fatalf("file %q as the team: %v", n.Question, err)
		}
		ids = append(ids, a.ID)
	}
	for _, id := range ids {
		_, r, body := f.mirrorRPC(t, mirrorAs(teamTok, focusSID), mcpToolCall(attentionAnswerTool, map[string]any{"id": id, "answer": "approve"}))
		if r == nil || !strings.Contains(body, teamHostToolRefusal(attentionAnswerTool)) {
			t.Fatalf("attention_answer on team ask %d from a team host: %s; want the teamHostTools refusal", id, body)
		}
		for _, tool := range []string{attentionWaitTool, attentionCancelTool} {
			_, r, body := f.mirrorRPC(t, mirrorAs(teamTok, focusSID), mcpToolCall(tool, map[string]any{"id": id, "wait_seconds": 0}))
			if text, isErr := toolText(t, r); r == nil || !isErr || !strings.Contains(text, "is not one of yours") {
				t.Fatalf("%s on team ask %d from a team host: %s; want it refused as not the session's own", tool, id, body)
			}
		}
		a, err := f.attn.Get(asTeam, id)
		if err != nil || a.Status != attention.Open || a.Answer != "" {
			t.Fatalf("team ask %d after the team host's tries: %+v, %v; want it open and unanswered", id, a, err)
		}
	}
	// CONTROL: the session's own ask is its to cancel.
	_, r, body := f.mirrorRPC(t, mirrorAs(teamTok, focusSID), mcpToolCall(attentionAskTool, map[string]any{"question": "Mine?", "options": []string{"y", "n"}}))
	text, isErr := toolText(t, r)
	var own struct{ ID int64 }
	if isErr || json.Unmarshal([]byte(text), &own) != nil || own.ID == 0 {
		t.Fatalf("CONTROL: attention_ask: %s", body)
	}
	_, r, body = f.mirrorRPC(t, mirrorAs(teamTok, focusSID), mcpToolCall(attentionCancelTool, map[string]any{"id": own.ID}))
	if _, isErr := toolText(t, r); isErr {
		t.Fatalf("CONTROL: the session could not cancel its own ask: %s", body)
	}
}
