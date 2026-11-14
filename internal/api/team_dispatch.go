package api

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/fence"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// TEAM DISPATCH: A TEAM'S CHIEF STARTS A TEAM-OWNED AGENT (team sharing phase
// 4, slice 4.7; plan §4 "Members' say-so"; operator decisions: members may
// dispatch — operators and above — and "a member's own typed message counts
// (with a fresh sign-in, else an approval card); ONE SHARED ROOM").
//
// The team's chief runs agent_dispatch as the team acting principal, so the
// agent it starts is owned by the team (agents' owner_id defaults to
// muster.uid) and can sit only on a team task (the composite key onto notes).
// What changes is WHOSE SAY-SO counts, since no person is the team:
//
//   - THE TYPED TURN (owner_asked): chief_owner_release.go's
//     typerSpeaksForOwner — a member who is an operator or above, on a fresh
//     read, whose sign-in was fresh when they typed (the marker is set only
//     then), re-read again when the chief calls; the lane checks unchanged.
//   - THE APPROVAL CARD: answered only from a signed-in BROWSER, by a member
//     who is an operator or above on a fresh read, with a FRESH sign-in at the
//     moment of the answer (teamCardRefusal, before anything is recorded; the
//     card stays open on a refusal). The phone's notification action and the
//     paired-phone answer route are refused by name. When the chief completes
//     the start, the answerer is read again (teamCardAnswerer): a member
//     removed since is refused.
//   - THE VERBATIM ATTENTION ANSWER as an agent's instructions (ownerAnswer):
//     muster records no sign-in time on an answer, so on a team's task it is
//     never taken as instructions on its own — the chief's brief goes on an
//     approval card instead (teamOwnerAnswerRefusal).
//
// Every start's audit row (task.chief_dispatch) names the person whose say-so
// it was (DetailRequestedByUser), and the push names them too
// (teamDispatchPushBody). Team push FAN-OUT to the members is phase 2b's: the
// push goes to the team owner through the one push call (pushTask).
// ---------------------------------------------------------------------------

// The team card refusals, one spelling each: what the answering member reads.
const (
	TeamCardNotFromPhone = "this is a team's dispatch card: it is answered only in muster in a browser, with a recent sign-in — " +
		"not from a phone or a notification. Open it in muster in a browser"
	TeamCardBrowserOnly  = "this is a team's dispatch card: it is answered only in muster in a browser, by a person signed in there"
	TeamCardOperatorOnly = "this is a team's dispatch card: only an operator or a manager of the team approves an agent start"
	TeamCardStepUp       = "this is a team's dispatch card: approving it needs a recent sign-in. Confirm it's you, then answer again"
	teamCardUnreadable   = "this card could not be checked just now, so the answer was not taken; try again"
)

// The chief's sentences when a team's card answer does not hold up when the
// start is completed (teamCardAnswerer).
const agentDispatchTeamCardAnswerer = "the member who approved that card is no longer an operator of your team, so muster cannot " +
	"take it as the team's approval; do not start it, and ask the team again with a new request_key"

// teamCardRefusal is the answer path's gate for a TEAM's dispatch card (one
// whose hold the team's chief filed): handled reports that ask id is one, and
// err is why the answer is refused (nil: it may be recorded). An ask of the
// caller's own, any ask of a team that is not a dispatch card (an ask_owner
// question: an operator answers those, row-level security decides), and a
// principal with no team memberships on its door are not handled here. The
// phone's door has its own named refusal (refuseTeamDispatchCardOnPhone,
// device_actions.go), which this calls.
func (s *Server) teamCardRefusal(ctx context.Context, id int64) (bool, error) {
	p, ok := authz.From(ctx)
	if !ok || p.All() || p.Team || p.TeamScope() == "" {
		return false, nil
	}
	if p.Via == authz.ViaDevice {
		err := s.refuseTeamDispatchCardOnPhone(ctx, id)
		return err != nil, err
	}
	team, card, err := s.teamDispatchCard(ctx, id)
	switch {
	case err != nil:
		return true, err
	case !card:
		return false, nil
	case p.Via != authz.ViaSession:
		return true, errReleaseRefused(TeamCardBrowserOnly)
	case !s.teamOperatorNow(ctx, p.UserID, team):
		return true, errReleaseRefused(TeamCardOperatorOnly)
	case !stepup.Fresh(ctx, s.stepUpNow()):
		return true, errReleaseRefused(TeamCardStepUp)
	}
	return true, nil
}

// teamDispatchCard reports whether ask id — read as ctx's person — is a
// TEAM's dispatch card, and which team: an owner card of a team the person
// is a member of, whose hold the team's chief filed. err (already the
// answerer's refusal) is a hold that could not be read: the card is not
// answered on a guess.
//
// 🔴 THE HOLD IS READ AS THE TEAM (attentionPrincipal: the team acting
// principal through the attention door). host_dispatches is personal, so the
// member's own principal sees no team hold — and a card that looked like no
// dispatch card would slip past every check. Only whether a hold names this
// ask is used; nothing read is returned.
func (s *Server) teamDispatchCard(ctx context.Context, id int64) (int64, bool, error) {
	p, ok := authz.From(ctx)
	st := s.hostDispatches()
	if !ok || p.All() || p.Team || p.TeamScope() == "" || s.ext.Attention == nil || st == nil {
		return 0, false, nil
	}
	a, err := s.ext.Attention.Get(ctx, id)
	if err != nil || a.Owner <= 0 || a.Owner == p.UserID || !a.OwnerCard {
		return 0, false, nil
	}
	if _, in := p.TeamScope().RoleIn(a.Owner); !in {
		return 0, false, nil
	}
	_, err = st.CardHoldByAsk(s.attentionPrincipal(ctx, a.Owner), id)
	switch {
	case errors.Is(err, plugins.ErrHoldNotFound):
		return 0, false, nil
	case err != nil:
		s.logger.Printf("team dispatch: read the hold of team %d's card %d: %v", a.Owner, id, err)
		return a.Owner, false, errReleaseRefused(teamCardUnreadable)
	}
	return a.Owner, true, nil
}

// teamCardAnswerer is "" when ask — a card the chief's completing call binds
// to — is not a team's, or was answered by a person who is STILL an operator
// or above of the team; else the chief's refusal. Read when the
// start is completed, so a member removed (or made a viewer) after their tap
// approves nothing. The answer's freshness was the answer path's
// (teamCardRefusal); it is not recorded, so it is not read again here.
func (s *Server) teamCardAnswerer(ctx context.Context, ask attention.Ask) string {
	// Its DOOR is not re-judged here: completeHeld already refused every
	// answer that is not an owner's door (agentDispatchHeldDoor) and the
	// phone's on an approval card (agentDispatchHeldPushAction), which leaves
	// the browser.
	if !s.ownerIsTeam(ctx, ask.Owner) {
		return ""
	}
	if !s.teamOperatorNow(ctx, ask.AnsweredByUser, ask.Owner) {
		return agentDispatchTeamCardAnswerer
	}
	return ""
}

// agentDispatchTeamAnswerNotInstructions refuses, on a TEAM's task, a prompt
// that is only an attention answer's words (ownerAnswer's path).
const agentDispatchTeamAnswerNotInstructions = "on a team's task, an answer on the Attention view is not taken as an agent's " +
	"instructions by itself (muster does not record whether its sign-in was recent): call again with a request_key, and " +
	"muster asks the team to approve your brief on a card"

// teamOwnerAnswerRefusal is ownerAnswer's refusal for a team's task: "" when
// owner is a person. muster stores no sign-in time with an answer, so it
// cannot tell whether a member's answer came with the fresh step-up a team's
// dispatch needs (plan §4): fail closed — the verbatim-answer path is shut for
// teams, and with a request_key agent_dispatch raises the approval card, whose
// answer is checked fresh (teamCardRefusal).
func (s *Server) teamOwnerAnswerRefusal(ctx context.Context, owner int64) string {
	if s.ownerIsTeam(ctx, owner) {
		return agentDispatchTeamAnswerNotInstructions
	}
	return ""
}

// teamDispatchTitle is a team start's push title.
const teamDispatchTitle = "Team agent started"

// teamDispatchPushBody is THE sentence a team's start is pushed with:
// "<person> asked <team>'s chief to start <agent> on task #N" (or "from a
// request" with no task). Outside words — a display name, a team name — are
// cut to one line.
func teamDispatchPushBody(person, team, agentName string, taskID int64) string {
	one := func(v string) string { return fence.Sanitize(mirror.OneLine(v, 60)) }
	where := "from a request in its chat"
	if taskID > 0 {
		where = "on task #" + strconv.FormatInt(taskID, 10)
	}
	return one(person) + " asked " + one(team) + "'s chief to start " + agentName + " " + where
}

// personName is the display name of person, "user <id>" when it has none or
// cannot be read.
func (s *Server) personName(ctx context.Context, person int64) string {
	if pn, ok := s.ext.Users.(personNamer); ok && person > 0 {
		if u, err := pn.ByID(ctx, person); err == nil && strings.TrimSpace(u.DisplayName) != "" {
			return u.DisplayName
		}
	}
	return "user " + strconv.FormatInt(person, 10)
}

// pushTeamDispatch tells team (the agent's owner) that person's say-so started
// rec. Reaching the team's members is phase 2b's fan-out; this is the one
// push call it fans out.
func (s *Server) pushTeamDispatch(ctx context.Context, team, person int64, rec agents.Agent, taskID int64) {
	body := teamDispatchPushBody(s.personName(ctx, person), s.teamNames(ctx, []int64{team})[team], rec.Name, taskID)
	n := RouterNotification{Type: taskNotificationType, Title: teamDispatchTitle, Body: body,
		Tag: "team-dispatch-" + rec.Name, Data: map[string]string{"url": ui.AgentPath(rec.Name)}}
	if taskID > 0 {
		n.ID = strconv.FormatInt(taskID, 10)
	}
	s.pushTask(team, push.EventAgent, n)
}
