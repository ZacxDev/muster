package api

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
)

// A TEAM CHIEF'S OTHER SAY-SO USES THE TEAM DISPATCH RULE (team sharing 2b.4,
// operator decision 2026-11-15). On a team's chief, end_session_propose, a
// text route (route_to_session) and agent_dispatch's gate release take a
// typed turn as the say-so only on the rule team dispatch uses: a fresh typed
// turn from a person who is an operator or above of the agent's team, the
// step-up fresh when typed, the role re-read when the chief calls; otherwise
// the call falls back to its card path. ONE predicate decides all four:
// ownerTurnFor (typerSpeaksForOwner, typerSignedOutSince), pinned below as a
// caller ledger.

// ownerTurnCallers is the functions in internal/api that take a typed turn as
// the say-so — call ownerTurnFor — with what for. 🔴 IT FAILS ONLY WHEN THE
// SET SHRINKS (round 0 on #703, D2): a use that stops calling the predicate
// has dropped its say-so check. A NEW caller is the safe case (it inherits
// the team rule by calling it), so growth is not a failure; an open-coded
// say-so never calls it and no walk of its callers could see one.
var ownerTurnCallers = map[string]string{
	"Server.toolAgentDispatch":     "agent_dispatch's owner_asked (typed say-so, and again under the reservation's lock)",
	"Server.verifyGateRelease":     "agent_dispatch's gate release (owner_asked on a gated task)",
	"Server.toolEndSessionPropose": "end_session_propose: a line typed in the chief's chat names the session",
	"Server.toolRouteToSession":    "route_to_session with text: the request routed in the owner's (the team's) name",
	"Server.toolHostDispatch":      "host_dispatch's owner_asked (a host agent start)",
}

// typerPredicateCallers is every caller of typerSpeaksForOwner, THE test of
// whether a typer speaks for an agent's owner (a team: an operator or above,
// fresh). Two: the marker, set at typing, and ownerTurnFor, at the call.
var typerPredicateCallers = map[string]string{
	"Server.markOwnerTurn": "at typing: a member's marker is set only for a fresh operator or above",
	"Server.ownerTurnFor":  "at the tool call: the role re-read",
}

// TestEverySaySoIsTheOneTypedTurnPredicate is both ledgers' walk (an
// invariant: the base already routed every use through ownerTurnFor; this
// pins it): ownerTurnFor's callers may grow, never shrink; typerSpeaksForOwner's
// is exact (a direct caller would skip ownerTurnFor's sign-out and lane checks).
func TestEverySaySoIsTheOneTypedTurnPredicate(t *testing.T) {
	if n := apiCallers(t, "", "requestOwner"); len(n) != 0 {
		// requestOwner is a plain function: a selector walk must NOT see it.
		t.Fatalf("instrument check FAILED: the selector walk counted plain calls: %v", n)
	}
	if n := apiCallers(t, "s", "accountOfChecked"); len(n) < 3 {
		t.Fatalf("instrument check FAILED: the walk found %d callers of s.accountOfChecked", len(n))
	}
	callers := apiCallers(t, "s", "ownerTurnFor")
	for fn, why := range ownerTurnCallers {
		if callers[fn] == 0 {
			t.Errorf("%s no longer asks ownerTurnFor (%s): its say-so check is gone", fn, why)
		}
	}
	ledgerMatches(t, "typerSpeaksForOwner", apiCallers(t, "s", "typerSpeaksForOwner"), typerPredicateCallers)
}

// TestATeamChiefsEndAndRouteTakeTheTeamDispatchRule: on the team's chief,
// end_session_propose and a text route both take B's FRESH typed turn (the
// predicate passes, and each then refuses only on the session it was handed,
// which does not exist), refuse it once B is removed — read again at the
// call — each in its own team words, and refuse a STALE typed turn (no
// marker). Nothing is filed on a refusal: no card.
//
// 🔴 RED at base for end_session_propose's words: every refusal was
// endSessionNoOwner ("your owner's own request"), which a team's chief cannot
// act on; and for a stale turn's route refusal, which said "your owner typed".
func TestATeamChiefsEndAndRouteTakeTheTeamDispatchRule(t *testing.T) {
	td := newTeamDispatchWorld(t)
	end := func() string {
		return td.w.srv.toolEndSessionPropose(td.teamCtx, td.chief, `{"session":"agent:no-such-agent-`+td.w.suffix+`"}`)
	}
	route := func() string {
		return td.w.srv.toolRouteToSession(td.teamCtx, td.chief, `{"text":"run the team's build","owner_asked":true,"subject":"mirror:0f0e0d0c-0b0a-4908-8706-050403020100"}`)
	}
	asks := func() int {
		var n int
		if err := td.w.pool.QueryRow(td.w.sys, `SELECT count(*) FROM attention_asks WHERE owner_id = $1`, td.team).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := asks()

	_, clear := td.typed(t, td.w.b.uid, true, "end that session, and route this")
	if got := end(); strings.Contains(got, endSessionNoOwner) || strings.Contains(got, "no longer one") || !strings.Contains(got, "is not one of your owner's sessions") {
		t.Fatalf("end_session_propose on B's fresh typed turn: %s; want past the say-so, refused on the session", truncate(got, 300))
	}
	if got := route(); strings.Contains(got, "owner_asked") || strings.Contains(got, "bad_arguments") {
		t.Fatalf("a text route on B's fresh typed turn: %s; want past the say-so", truncate(got, 300))
	}
	td.removeB(t)
	if got := end(); !strings.Contains(got, "ending a session needs a request typed by an operator or a manager of your team: the member who typed the message you are answering is no longer one") {
		t.Fatalf("end_session_propose after B's removal: %s; want the team's typer refusal", truncate(got, 300))
	}
	if got := route(); !strings.Contains(got, "is no longer an operator of your team") {
		t.Fatalf("a text route after B's removal: %s; want the team's typer refusal", truncate(got, 300))
	}
	clear()

	// A stale typed turn marks nothing: both refused in the TEAM's words
	// (round 0 on #703: never "your owner's own request"), no card.
	td.typed(t, td.w.a.uid, false, "end that session")
	if got := end(); !strings.Contains(got, opsStaleEnd) || strings.Contains(got, "your owner") {
		t.Fatalf("end_session_propose on a STALE typed turn: %s; want %q", truncate(got, 300), opsStaleEnd)
	}
	if got := route(); !strings.Contains(got, opsStaleRoute) || strings.Contains(got, "your owner") {
		t.Fatalf("a text route on a STALE typed turn: %s; want %q", truncate(got, 300), opsStaleRoute)
	}
	if n := asks(); n != before {
		t.Fatalf("a refused end_session_propose filed %d card(s)", n-before)
	}
}

// The team's stale-turn sentences, pinned literally: end_session_propose's and
// a route's.
const (
	opsStaleEnd = "end_session_propose: you are not answering a message an operator or a manager of your team typed in your " +
		"muster chat with a recent sign-in (a stale sign-in, a viewer's line, a plugin message, a relayed answer, a note from " +
		"muster, a turn you started yourself or anything typed in your terminal never counts), so muster cannot file a card " +
		"to end a session; ask the team in the chat"
	opsStaleRoute = "owner_asked: you are not answering a message an operator or a manager of your team typed in your muster " +
		"chat with a recent sign-in (a stale sign-in, a viewer's line, a plugin message, a relayed answer, a note from muster, " +
		"a turn you started yourself or anything typed in your terminal never counts), so muster cannot route text in the " +
		"team's name; ask the team in the chat, or use task_route_attention"
)

// TestATeamChiefsGateReleaseTakesTheTeamDispatchRule: verifyGateRelease on a
// TEAM task the team's chief gated (task_create_gated's ledger row): B's fresh
// typed turn releases (the release names B's row); once B is removed — read
// again at the call — it is refused as the team's typer; a stale typed turn
// is refused in the team's words. An invariant of the base for the release
// itself (one predicate), red at base for the stale turn's words.
func TestATeamChiefsGateReleaseTakesTheTeamDispatchRule(t *testing.T) {
	td := newTeamDispatchWorld(t)
	if td.w.srv.ext.Plugins == nil {
		td.w.srv.ext.Plugins = plugins.NewPG(td.w.pool)
	}
	led, ok := td.w.srv.ext.Plugins.(gatedTaskLedger)
	if !ok {
		t.Fatal("instrument check: the plugin store keeps no gated-task ledger")
	}
	gate := "gate:team-review-" + td.w.suffix
	n, err := notes.NewPG(td.w.pool).Create(td.teamCtx, notes.Note{Title: "gated team work " + td.w.suffix, Body: "Gated.", Tags: []string{gate}})
	if err != nil {
		t.Fatal(err)
	}
	if err := led.RecordGatedTaskFiled(td.teamCtx, n.ID, td.chief.ID, gate); err != nil {
		t.Fatal(err)
	}
	msg, clear := td.typed(t, td.w.b.uid, true, "go ahead with the gated team task")
	rel, why := td.w.srv.verifyGateRelease(td.teamCtx, td.chief, n)
	if why != "" || rel == nil || rel.msg.ID != msg.ID || rel.msg.TypedBy != td.w.b.uid {
		t.Fatalf("the gate release on B's fresh typed turn: %+v %q; want released on B's row %d", rel, why, msg.ID)
	}
	td.removeB(t)
	if _, why := td.w.srv.verifyGateRelease(td.teamCtx, td.chief, n); why != chiefReleaseTyperNotOperator {
		t.Fatalf("the gate release after B's removal: %q; want %q", why, chiefReleaseTyperNotOperator)
	}
	clear()
	td.typed(t, td.w.a.uid, false, "go ahead")
	_, why = td.w.srv.verifyGateRelease(td.teamCtx, td.chief, n)
	if why != chiefReleaseNoOwnerTurn {
		t.Fatalf("the gate release on a STALE typed turn: %q; want the predicate's %q", why, chiefReleaseNoOwnerTurn)
	}
	if got := td.w.srv.ownerTurnRefusal(td.teamCtx, td.chief, why, sayGated); strings.Contains(got, "your owner") ||
		!strings.Contains(got, "an operator or a manager of your team") || !strings.Contains(got, "release the gate on the team's word") {
		t.Fatalf("the gate release's refusal to the team's chief: %q; want the team's words", got)
	}
}
