package api

// ---------------------------------------------------------------------------
// THE ONE PLACE A TYPED-TURN REFUSAL BECOMES WORDS (team sharing 2b.4, round
// 0 on #703). ownerTurnFor is the one predicate every say-so asks — an
// agent_dispatch's owner_asked (and its gate release), a text route,
// end_session_propose — and it answers a refusal as one of its constants
// (chief_owner_release.go). ownerTurnWords turns that constant into the
// sentence the chief reads, by the use it refused and by whose chief it is.
//
//   - A PERSON's chief: each use's own sentences, unchanged — a gated
//     task's are the constants themselves; an ungated task's and a
//     no-task start's ownerAskedNoTaskSibling (agent_dispatch.go); a
//     route's routeOwnerAskedSibling (session_routes.go); an end's
//     endSessionNoOwner.
//   - A TEAM's chief: no person is the team, so "your owner typed" is never
//     what the chief can act on. Every refusal that says it is worded for
//     the team (teamTurnWords: an operator or a manager of the team, with a
//     recent sign-in), with the use's action and its fallback; the two
//     member refusals (removed or demoted, signed out since) were team
//     words already, in every use.
// ---------------------------------------------------------------------------

import (
	"context"
	"fmt"

	"github.com/ZacxDev/muster/internal/agents"
)

// sayUse is the say-so a typed-turn refusal refused.
type sayUse int

const (
	sayGated   sayUse = iota // agent_dispatch's gate release, on a task the chief gated
	sayNoTask                // agent_dispatch with no task
	sayUngated               // agent_dispatch on an ungated task
	sayRoute                 // route_to_session with text
	sayEnd                   // end_session_propose
)

// sayUseOf is agent_dispatch's task kind as a sayUse.
func sayUseOf(k ownerAskedTask) sayUse {
	switch k {
	case ownerAskedGated:
		return sayGated
	case ownerAskedUngated:
		return sayUngated
	}
	return sayNoTask
}

// sayFrame is how a team's sentence opens, what muster cannot do, and where
// the chief turns instead, by use.
var sayFrame = map[sayUse]struct{ prefix, act, fallback string }{
	sayGated:   {"owner_asked: ", "release the gate on the team's word", "; ask the team to release it on the board, or use task_route_attention"},
	sayNoTask:  {"owner_asked: ", "take this as the team's request", "; call again with a request_key to put an approval card in front of the team"},
	sayUngated: {"owner_asked: ", "take this as the team's request", "; call again with a request_key to put an approval card in front of the team, or use task_route_attention"},
	sayRoute:   {"owner_asked: ", "route text in the team's name", "; ask the team in the chat, or use task_route_attention"},
	sayEnd:     {"end_session_propose: ", "file a card to end a session", "; ask the team in the chat"},
}

// teamTurnWords is, for each refusal that names "your owner", its team
// sentence: %[1]s the frame's prefix, %[2]s its act, %[3]s its fallback.
var teamTurnWords = map[string]string{
	chiefReleaseNoOwnerTurn: "%[1]syou are not answering a message an operator or a manager of your team typed in your muster chat " +
		"with a recent sign-in (a stale sign-in, a viewer's line, a plugin message, a relayed answer, a note from muster, a turn you " +
		"started yourself or anything typed in your terminal never counts), so muster cannot %[2]s%[3]s",
	chiefReleaseNotOwnersTurn: "%[1]sthe message you are answering was not typed by an operator or a manager of your team, so " +
		"muster cannot %[2]s%[3]s",
	chiefReleaseOutsideTurn: "%[1]smuster is also running a turn for you that no member typed (a plugin message, a relayed answer, " +
		"a nudge), so muster cannot tell which of them asked for this and cannot %[2]s; ask the team again in the chat once it is over",
	chiefReleaseOtherCLIBusy: "%[1]sanother of your sessions is working at the same time as the member's turn, so muster cannot " +
		"tell that this call comes from it and cannot %[2]s; ask the team again once only that turn is running",
	chiefReleaseUnaccountedCLI: "%[1]sa Claude Code process in your pod is running without a session record, so muster cannot " +
		"tell whether it is working at the same time as the member's turn and cannot %[2]s; ask the team again in the chat",
	chiefReleaseCLIsUnknown: "%[1]smuster could not read which of your sessions are working (your pod did not answer, or its " +
		"image is too old to say), so it cannot %[2]s; try again, or tell the team",
	chiefReleaseNotOverMCP: "%[1]sthis session is not a Claude Code session, and only a Claude Code session can show muster that " +
		"a member typed this, so it cannot %[2]s%[3]s",
	chiefReleaseUnverifiable: "%[1]sthe member's turn could not be read, so muster cannot %[2]s; try again later",
	chiefReleaseChanged:      "%[1]sthe member's turn or the task's gate changed while this was being checked; call again",
}

// ownerTurnWords is THE one translation of ownerTurnFor's refusal why into
// the chief's sentence, for use, on a team's chief (team) or a person's.
func ownerTurnWords(why string, use sayUse, team bool) string {
	if team {
		if tmpl, ok := teamTurnWords[why]; ok {
			f := sayFrame[use]
			return fmt.Sprintf(tmpl, f.prefix, f.act, f.fallback)
		}
	}
	switch use {
	case sayGated:
		return why
	case sayNoTask, sayUngated:
		sib, ok := ownerAskedNoTaskSibling[why]
		if !ok {
			return why // not an owner's-turn sentence (chiefReleaseNotChiefsGate is a gated task's only)
		}
		if use == sayUngated {
			return sib + ownerAskedUngatedTail
		}
		return sib
	case sayRoute:
		if msg, ok := routeOwnerAskedSibling[why]; ok {
			return msg
		}
		return why
	case sayEnd:
		switch why {
		case chiefReleaseTyperNotOperator:
			return endSessionTyperNotOperator
		case chiefReleaseTyperSignedOut:
			return endSessionTyperSignedOut
		}
		return endSessionNoOwner
	}
	return why
}

// ownerTurnRefusal is why worded for use on chief: ownerTurnWords, on whether
// chief's owner is a team (ownerIsTeam; an owner that cannot be read is
// worded as a team's, the side that never says "your owner" to a team).
func (s *Server) ownerTurnRefusal(ctx context.Context, chief agents.Agent, why string, use sayUse) string {
	return ownerTurnWords(why, use, s.ownerIsTeam(ctx, agentOwnerID(chief)))
}
