package api

import (
	"strings"
	"testing"
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

// ownerTurnCallers is EVERY function in internal/api that takes a typed turn
// as the say-so — calls ownerTurnFor — with what for. It fails when the set
// grows OR shrinks: a new use is a decision, and it inherits the team rule
// only by calling this predicate rather than open-coding one.
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
// pins it). Its positive control: the same walk finds requestOwner's callers.
func TestEverySaySoIsTheOneTypedTurnPredicate(t *testing.T) {
	if n := apiCallers(t, "", "requestOwner"); len(n) != 0 {
		// requestOwner is a plain function: a selector walk must NOT see it.
		t.Fatalf("instrument check FAILED: the selector walk counted plain calls: %v", n)
	}
	if n := apiCallers(t, "s", "accountOfChecked"); len(n) < 3 {
		t.Fatalf("instrument check FAILED: the walk found %d callers of s.accountOfChecked", len(n))
	}
	ledgerMatches(t, "ownerTurnFor", apiCallers(t, "s", "ownerTurnFor"), ownerTurnCallers)
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
// act on. The route's and the predicate's behaviour are invariants of the
// base, pinned here.
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

	// A stale typed turn marks nothing: both refused, no card.
	td.typed(t, td.w.a.uid, false, "end that session")
	if got := end(); !strings.Contains(got, endSessionNoOwner) {
		t.Fatalf("end_session_propose on a STALE typed turn: %s; want %q", truncate(got, 300), endSessionNoOwner)
	}
	if n := asks(); n != before {
		t.Fatalf("a refused end_session_propose filed %d card(s)", n-before)
	}
}
