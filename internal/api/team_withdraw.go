package api

import (
	"context"
	"strconv"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/users"
)

// ---------------------------------------------------------------------------
// THE WITHDRAWAL SWEEP (team sharing phase 2b.1, operator decision G1).
//
// A team's notification is withdrawn from the team's CURRENT operators and
// managers (goNotify's fan-out, team_notify.go). A person who stops being one
// between the push and its withdrawal — removed from the team, downgraded to
// viewer, or disabled — would keep it on their devices. So each of those
// changes, once it has committed, sends that person a withdrawal for each of
// the team's open asks, pending drafts and pending outreach batches: bounded
// by the open set. There is no recipient table (G1's rejected option b). A
// person who never had a push gets a withdrawal for it, which shows nothing.
//
// The changes and their one entry here each:
//   - a removal (an admin's, a manager's): teamMembershipEnded;
//   - a role set or re-role to below operator: teamRoleSet;
//   - a disable: userDisabledChanged, for every team the person operates or
//     manages.
// ---------------------------------------------------------------------------

// teamDraftLister is the plugin store's read of one owner's pending drafts
// (plugins.PGStore.PendingOwnedBy), asserted so a store without it sweeps no
// drafts.
type teamDraftLister interface {
	PendingOwnedBy(ctx context.Context, owner int64) ([]plugins.Draft, error)
}

// myTeamsReader is the account store's read of a person's teams
// (users.PGStore.MyTeams), asserted like teamDraftLister.
type myTeamsReader interface {
	MyTeams(ctx context.Context, user int64) ([]users.MyTeam, error)
}

// teamMembershipEnded is a person removed from team: the sweep.
func (s *Server) teamMembershipEnded(team, person int64) {
	s.withdrawTeamItemsFrom(team, person)
}

// teamRoleSet is a person's role in team set to role: the sweep when the new
// role is below operator (a viewer receives no team push). An add at viewer
// sweeps too, which closes nothing the person was shown.
func (s *Server) teamRoleSet(team, person int64, role authz.TeamRole) {
	if role.Valid() && !role.AtLeast(authz.TeamOperator) {
		s.withdrawTeamItemsFrom(team, person)
	}
}

// userDisabledChanged is everything muster does in this process once a
// person's account is disabled or enabled (the store has committed it): the
// cached user, their queued messages (the delivery re-checks the account as
// well: sendQueueOwnerRefusal) and, on a disable, the withdrawal sweep for
// every team they operate or manage.
func (s *Server) userDisabledChanged(ctx context.Context, id int64, disabled bool) {
	s.gt.forgetUser(id)
	s.gt.setDisabled(id, disabled)
	if !disabled {
		return
	}
	s.cancelQueuedFor(ctx, id, SendQueueCancelDisabled)
	if !s.notifiable() {
		return
	}
	r, ok := s.ext.Users.(myTeamsReader)
	if !ok {
		return
	}
	teams, err := r.MyTeams(context.WithoutCancel(ctx), id)
	if err != nil {
		s.logger.Printf("push: read disabled user %d's teams for the withdrawal sweep: %v", id, err)
		return
	}
	for _, t := range teams {
		if t.Role.AtLeast(authz.TeamOperator) {
			s.withdrawTeamItemsFrom(t.ID, id)
		}
	}
}

// withdrawTeamItemsFrom sends person, in the background, a withdrawal for each
// of team's open asks, pending drafts and pending outreach batches, by the
// tags their pushes carried (attentionTag, draftTag, batchTag). The reads act
// as the team (attentionPrincipal: ViaAttention, team-acting), so they see
// exactly the team's rows. Best effort: a failed read is logged.
func (s *Server) withdrawTeamItemsFrom(team, person int64) {
	if !s.notifiable() || team <= 0 || person <= 0 || person == team {
		return
	}
	s.pushInFlight.Add(1)
	safeGo(s.logger, "team withdraw", func() {
		defer s.pushInFlight.Done()
		ctx, cancel := context.WithTimeout(context.Background(), pushBroadcastTimeout)
		defer cancel()
		ctx = s.attentionPrincipal(ctx, team)
		withdraw := func(id, tag string) {
			s.goNotify(person, push.EventResolved, RouterNotification{Type: push.TypeResolved, ID: id, Tag: tag}, nil)
		}
		if s.ext.Attention != nil {
			open, err := s.ext.Attention.Open(ctx)
			if err != nil {
				s.logger.Printf("push: team %d's open asks for user %d's withdrawal sweep: %v", team, person, err)
			}
			for _, a := range open {
				if a.Owner == team {
					key := attentionAskKey(a.ID)
					withdraw(key, attentionTag(key))
				}
			}
		}
		l, ok := s.ext.Plugins.(teamDraftLister)
		if !ok {
			return
		}
		drafts, err := l.PendingOwnedBy(ctx, team)
		if err != nil {
			s.logger.Printf("push: team %d's pending drafts for user %d's withdrawal sweep: %v", team, person, err)
			return
		}
		batches := map[string]bool{}
		for _, d := range drafts {
			if d.IsOutbound() && plugins.IsBatchKind(d.Target().Kind) {
				// A follow or a like is pushed once per batch (notifyBatchAwaiting).
				if d.Officer != nil {
					batches[plugins.BatchID(d.OwnerID, *d.Officer, d.CreatedAt.UTC().Format("2006-01-02"))] = true
				}
				continue
			}
			withdraw(strconv.FormatInt(d.ID, 10), draftTag(d.ID))
		}
		for id := range batches {
			withdraw(id, batchTag(id))
		}
	})
}
