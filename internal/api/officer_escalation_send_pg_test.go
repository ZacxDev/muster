package api

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/users"
)

// The SEND-TIME half of the one escalation rule (Q1): the escalation sweep
// asks only people who can see the officer, read when it sends — not when
// escalate_to was stored. Owner 1 manages declTeam (declOther is its viewer);
// reachOutsider shares no team with owner 1 (seedReach).

// sendRig is a builtin officer (MUSTER_OFFICERS, owner 1) on a server that
// reads teams (Users) and raises Attention asks, with the people of seedReach
// and declOther/declTeam.
func sendRig(t *testing.T) (*connFixture, *Server, *builtin.Store, *bytes.Buffer) {
	t.Helper()
	f := newConnFixture(t)
	logs := &bytes.Buffer{}
	srv, store := builtinOfficerServer(t, f, `[{"id":"`+builtinOfficerID+`","backend":"builtin"}]`, logs)
	srv.ext.Users = users.NewPG(f.pool)
	dsn := dbtest.DSN(t)
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, kind, role, display_name) OVERRIDING SYSTEM VALUE VALUES
		(987301, 'person', 'member', 'send-other'), (987302, 'team', 'member', 'send-team') ON CONFLICT DO NOTHING`)
	t.Cleanup(func() { dbtest.OwnerExec(t, dsn, `DELETE FROM users WHERE id IN (987301, 987302)`) })
	seedReach(t)
	// Registered last, so it runs first: an ask a red run raised for any of
	// these people must go before seedReach deletes them.
	t.Cleanup(func() {
		dbtest.OwnerExec(t, dsn, `DELETE FROM attention_asks WHERE owner_id IN (987301, 987302, 987303, 987305)`)
	})
	return f, srv, store, logs
}

// officerAsksOf is uid's open Attention asks a builtin officer raised.
func officerAsksOf(t *testing.T, s *Server, uid int64) []attention.Ask {
	t.Helper()
	open, err := s.ext.Attention.Open(s.attentionPrincipal(context.Background(), uid))
	if err != nil {
		t.Fatal(err)
	}
	var out []attention.Ask
	for _, a := range open {
		if strings.HasPrefix(a.Asker.Label, "officer ") {
			out = append(out, a)
		}
	}
	return out
}

// escalateOne enqueues subject, has instance a claim it and complete it
// escalated with summary, and returns the item id.
func escalateOne(t *testing.T, srv *Server, store *builtin.Store, inst agents.Agent, subject, summary string) int64 {
	t.Helper()
	ctx := dbtest.AsOperator(context.Background())
	id, _, err := store.Enqueue(ctx, 1, builtin.NewItem{Officer: builtinOfficerID, Kind: "release.post", SubjectKey: subject}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if out := srv.dispatchAgentTool(ctx, inst, officers.ToolClaim, `{}`); isToolErr(out) || !strings.Contains(out, `"id":`+strconv.FormatInt(id, 10)) {
		t.Fatalf("claim %s: %s", subject, out)
	}
	if out := srv.dispatchAgentTool(ctx, inst, officers.ToolComplete, `{"item_id":"`+strconv.FormatInt(id, 10)+`","outcome":"escalated","summary":"`+summary+`"}`); isToolErr(out) {
		t.Fatalf("complete %s: %s", subject, out)
	}
	return id
}

// TestAStoredOutOfReachEscalationIsNotDeliveredAtSendTime: escalate_to was
// stored (past the write doors, as a list saved before the rule would be)
// naming declOther — a viewer of owner 1's team, IN reach — and
// reachOutsider, who shares no team with owner 1. The escalation asks the
// owner and declOther and NOT the outsider, whose notice is given up with one
// log line naming the officer and the recipient id and none of the item's
// text. CONTROL: the in-reach recipient is asked.
func TestAStoredOutOfReachEscalationIsNotDeliveredAtSendTime(t *testing.T) {
	f, srv, store, logs := sendRig(t)
	ctx := dbtest.AsOperator(context.Background())
	if err := store.PutType(ctx, 1, builtin.Type{Key: "marketing", Name: "Marketing", Charter: "c"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutDef(ctx, 1, builtin.Def{Officer: builtinOfficerID, Enabled: true, Mode: "live", EscalateTo: []int64{declOther, reachOutsider}}); err != nil {
		t.Fatal(err)
	}
	a := startPoolInstance(t, f, srv, builtinOfficerID, "", 1, "fixture-send-reach-token")
	briefInstance(t, srv, a)
	item := escalateOne(t, srv, store, a, "send-1", "secret-summary-words")
	srv.SweepOfficerAttention(context.Background())
	srv.SweepOfficerAttention(context.Background())
	if got := officerAsksOf(t, srv, 1); len(got) != 1 {
		t.Fatalf("the owner's asks: %d, want 1", len(got))
	}
	if got := officerAsksOf(t, srv, declOther); len(got) != 1 {
		t.Fatalf("control: the in-reach recipient (a viewer of the owner's team) got %d asks, want 1", len(got))
	}
	if got := officerAsksOf(t, srv, reachOutsider); len(got) != 0 {
		t.Fatalf("the out-of-reach stored recipient got %d asks, want none: a stored escalate_to id outside the reach is still delivered", len(got))
	}
	var given int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM officer_items i, jsonb_array_elements(i.notices) n
		WHERE i.id = $1 AND (n->>'recipient')::bigint = 987303 AND n->'suppressed' = 'true'::jsonb AND n->'settled' = 'true'::jsonb`, item).Scan(&given)
	if given != 1 {
		t.Fatalf("the outsider's notice after the sweeps: %d given up, want 1 (suppressed, settled)", given)
	}
	var dropped []string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "is dropped") {
			dropped = append(dropped, l)
		}
	}
	want := "officers: " + builtinOfficerID + ": item " + strconv.FormatInt(item, 10) + ": the escalation to user 987303 is dropped: that user cannot see the officer"
	if len(dropped) != 1 || dropped[0] != want {
		t.Fatalf("the drop log over two sweeps: %q, want exactly [%q]", dropped, want)
	}
}

// TestAMemberRemovedAfterSaveStopsReceivingEscalations: reach is read when the
// escalation is SENT. declOther is on owner 1's team when escalate_to is saved
// and receives the first escalation; removed from the team, they do not
// receive the next. The team arm too: a team officer's notice to a member
// who has since left the team is dropped, while the team's own and a current
// member's (user 1, its manager) are raised.
func TestAMemberRemovedAfterSaveStopsReceivingEscalations(t *testing.T) {
	f, srv, store, logs := sendRig(t)
	ctx := dbtest.AsOperator(context.Background())
	if err := store.PutType(ctx, 1, builtin.Type{Key: "marketing", Name: "Marketing", Charter: "c"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutDef(ctx, 1, builtin.Def{Officer: builtinOfficerID, Enabled: true, Mode: "live", EscalateTo: []int64{declOther}}); err != nil {
		t.Fatal(err)
	}
	a := startPoolInstance(t, f, srv, builtinOfficerID, "", 1, "fixture-send-removed-token")
	briefInstance(t, srv, a)
	escalateOne(t, srv, store, a, "send-before", "before")
	srv.SweepOfficerAttention(context.Background())
	if got := officerAsksOf(t, srv, declOther); len(got) != 1 {
		t.Fatalf("control: while a team member, declOther got %d asks, want 1", len(got))
	}
	dbtest.OwnerExec(t, dbtest.DSN(t), `DELETE FROM team_members WHERE team_id = 987302 AND user_id = 987301`)
	after := escalateOne(t, srv, store, a, "send-after", "after")
	srv.SweepOfficerAttention(context.Background())
	got := officerAsksOf(t, srv, declOther)
	for _, g := range got {
		if strings.Contains(g.Question, "escalated item "+strconv.FormatInt(after, 10)) {
			t.Fatalf("declOther, removed from the team after the officer was saved, was still asked about item %d", after)
		}
	}
	if len(got) != 1 {
		t.Fatalf("declOther's asks after removal: %d, want only the one from before", len(got))
	}
	if n := len(officerAsksOf(t, srv, 1)); n != 2 {
		t.Fatalf("the owner's asks: %d, want 2 (one per escalation)", n)
	}

	// The team arm: declTeam's officer escalated to the team, to declOther (no
	// longer a member) and to user 1 (still its manager). The team and user 1
	// are asked; declOther is not.
	sys := authz.System(context.Background())
	if err := store.PutType(sys, declTeam, builtin.Type{Key: "mk", Name: "Marketing", Charter: "c"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutDef(sys, declTeam, builtin.Def{Officer: "mk:a", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, tbl := range []string{"attention_asks", "officer_items", "officer_defs", "officer_types"} {
			dbtest.OwnerExec(t, dbtest.DSN(t), `DELETE FROM `+tbl+` WHERE owner_id = 987302`)
		}
	})
	var titem int64
	if err := f.pool.QueryRow(sys, `INSERT INTO officer_items (owner_id, officer, kind, subject_key, status, outcome, summary, notices)
		VALUES (987302, 'mk:a', 'launch', 'send-team', 'escalated', 'escalated', 'team summary',
		'[{"recipient":987302,"suppressed":false,"attention_id":null,"settled":false},{"recipient":987301,"suppressed":false,"attention_id":null,"settled":false},{"recipient":1,"suppressed":false,"attention_id":null,"settled":false}]'::jsonb) RETURNING id`).Scan(&titem); err != nil {
		t.Fatal(err)
	}
	srv.SweepOfficerAttention(context.Background())
	var teamAsks int
	_ = f.pool.QueryRow(sys, `SELECT count(*) FROM attention_asks WHERE owner_id = 987302`).Scan(&teamAsks)
	if teamAsks != 1 {
		t.Fatalf("control: the team officer's escalation to the team raised %d team asks, want 1", teamAsks)
	}
	for _, g := range officerAsksOf(t, srv, declOther) {
		if strings.Contains(g.Question, "escalated item "+strconv.FormatInt(titem, 10)) {
			t.Fatalf("a former member was asked about the team officer's item %d", titem)
		}
	}
	member := false
	for _, g := range officerAsksOf(t, srv, 1) {
		member = member || strings.Contains(g.Question, "escalated item "+strconv.FormatInt(titem, 10))
	}
	if !member {
		t.Fatalf("control: user 1, a CURRENT member (manager) of the team, was not asked about the team officer's item %d: the team officer's reach is not its members", titem)
	}
	if !strings.Contains(logs.String(), "officers: mk:a: item "+strconv.FormatInt(titem, 10)+": the escalation to user 987301 is dropped") {
		t.Fatalf("no drop line for the former member on the team officer:\n%s", logs.String())
	}
}

// TestAnEscalationWhoseEveryStoredRecipientIsOutOfReachStillReachesTheOwner:
// every escalate_to id is out of reach, so the sweep behaves as with an empty
// list — the owner alone is asked, nothing panics, the item stays escalated.
// And an item whose notices name ONLY out-of-reach people (no owner entry)
// is given up whole without crashing the sweep or holding the queue.
func TestAnEscalationWhoseEveryStoredRecipientIsOutOfReachStillReachesTheOwner(t *testing.T) {
	f, srv, store, _ := sendRig(t)
	ctx := dbtest.AsOperator(context.Background())
	if err := store.PutType(ctx, 1, builtin.Type{Key: "marketing", Name: "Marketing", Charter: "c"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutDef(ctx, 1, builtin.Def{Officer: builtinOfficerID, Enabled: true, Mode: "live", EscalateTo: []int64{reachOutsider, reachNobody}}); err != nil {
		t.Fatal(err)
	}
	a := startPoolInstance(t, f, srv, builtinOfficerID, "", 1, "fixture-send-all-dropped-token")
	briefInstance(t, srv, a)
	item := escalateOne(t, srv, store, a, "send-all", "all dropped")
	var only int64
	if err := f.pool.QueryRow(ctx, `INSERT INTO officer_items (owner_id, officer, kind, subject_key, status, outcome, summary, notices)
		VALUES (1, $1, 'k', 'send-only-outsider', 'escalated', 'escalated', 'x',
		'[{"recipient":987303,"suppressed":false,"attention_id":null,"settled":false}]'::jsonb) RETURNING id`, builtinOfficerID).Scan(&only); err != nil {
		t.Fatal(err)
	}
	srv.SweepOfficerAttention(context.Background())
	srv.SweepOfficerAttention(context.Background())
	owner := officerAsksOf(t, srv, 1)
	if len(owner) != 1 || !strings.Contains(owner[0].Question, "escalated item "+strconv.FormatInt(item, 10)) {
		t.Fatalf("the owner's asks: %+v, want exactly the one escalation", owner)
	}
	if n := len(officerAsksOf(t, srv, reachOutsider)); n != 0 {
		t.Fatalf("the outsider got %d asks, want none", n)
	}
	var status string
	var due int
	_ = f.pool.QueryRow(ctx, `SELECT status FROM officer_items WHERE id = $1`, only).Scan(&status)
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM officer_items i, jsonb_array_elements(i.notices) n
		WHERE i.id IN ($1, $2) AND n->'attention_id' = 'null'::jsonb AND n->'suppressed' = 'false'::jsonb`, item, only).Scan(&due)
	if status != officers.StatusEscalated || due != 0 {
		t.Fatalf("after the sweeps: the outsider-only item is %s with %d notices still due; want escalated, none due", status, due)
	}
}
