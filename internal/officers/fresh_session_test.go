package officers_test

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// tiers.<pool>.fresh_session_per_item: every item an instance of the pool
// claims is worked in a NEW Claude Code conversation, opened with the
// officer's kickoff (so it calls officer_brief and loads the current charter),
// never an "as before" re-prompt into the conversation that worked the last
// item.
// ---------------------------------------------------------------------------

// freshJudgementNote is the whole note a fresh judgement instance's kickoff
// ends with, pinned so the one-item contract cannot be reworded away.
const freshJudgementNote = "\n\nOne item per conversation (tiers.judgement.fresh_session_per_item): muster starts every item you work in a NEW conversation, " +
	"so nothing from an earlier item is in this one — judge this item only on what you read now. Take ONE item. " +
	"Once you have completed, released or handed it on, or asked a person about it, stop and end your turn: do not claim another " +
	"(officer_claim refuses a second item in this conversation). muster starts a new conversation, with this same first message, for the next item."

// TestFreshSessionPerItemIsAPerPoolOption: the key is read per pool — on
// the judgement pool, on the routine pool, on both — and is off unless
// declared; an untiered officer has no pools to declare it on and is never
// fresh. Fixture pools set it on ONE side at a time, so a pool reading the
// other pool's value cannot pass.
func TestFreshSessionPerItemIsAPerPoolOption(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		block                    string
		wantJudgement, wantRoute bool
	}{
		{"judgement only", `"tiers":{"routine":{"model":"haiku","max_instances":3},"judgement":{"model":"opus","max_instances":2,"fresh_session_per_item":true}}`, true, false},
		{"routine only", `"tiers":{"routine":{"model":"haiku","max_instances":3,"fresh_session_per_item":true},"judgement":{"model":"opus","max_instances":2}}`, false, true},
		{"neither", tiersBlock, false, false},
	} {
		list, err := parseOfficer(t, ","+tc.block)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		judge, routine := pools(t, list[0])
		if judge.FreshSessionPerItem() != tc.wantJudgement || routine.FreshSessionPerItem() != tc.wantRoute {
			t.Fatalf("%s: judgement fresh=%v routine fresh=%v, want %v/%v", tc.name,
				judge.FreshSessionPerItem(), routine.FreshSessionPerItem(), tc.wantJudgement, tc.wantRoute)
		}
	}
	if (officers.Officer{ID: "triage:example-web", MaxInstances: 2}).FreshSessionPerItem() {
		t.Fatal("an untiered officer is fresh per item")
	}
}

// TestAFreshPoolIsPromptedWithTheKickoffNeverAsBefore: the wake loop's turn
// for an idle instance of a fresh pool IS the kickoff (it starts a new
// conversation, which has no "before"), and the kickoff ends with the
// one-item note. A pool without the key keeps its "as before" re-prompt and
// its kickoff without the note — ⚠ that half is an INVARIANT guard (green
// before and after this change).
func TestAFreshPoolIsPromptedWithTheKickoffNeverAsBefore(t *testing.T) {
	list, err := parseOfficer(t, `,"tiers":{"routine":{"model":"haiku","max_instances":3},"judgement":{"model":"opus","max_instances":2,"fresh_session_per_item":true}}`)
	if err != nil {
		t.Fatal(err)
	}
	judge, routine := pools(t, list[0])
	kick := officers.KickoffText(judge, 2)
	if !strings.HasSuffix(kick, freshJudgementNote) {
		t.Fatalf("a fresh judgement instance's kickoff does not end with the one-item note:\n%s", kick)
	}
	if got := officers.RepromptText(judge, 2); got != kick {
		t.Fatalf("a fresh pool's re-prompt is not its kickoff:\n%s", got)
	}
	if strings.Contains(officers.RepromptText(judge, 2), "as before") {
		t.Fatal("a fresh pool is re-prompted \"as before\"")
	}
	// INVARIANT: the routine pool (no key) is unchanged.
	if got := officers.RepromptText(routine, 1); !strings.HasPrefix(got, "New work for officer "+tierID+" (you are instance r1,") || !strings.Contains(got, "as before") {
		t.Fatalf("a pool without the key: re-prompt %q", got)
	}
	if strings.Contains(officers.KickoffText(routine, 1), "One item per conversation") {
		t.Fatal("a pool without the key was told it works one item per conversation")
	}
}
