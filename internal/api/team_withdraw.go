package api

import (
	"context"
	"strconv"
	"sync"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
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
// changes sends that person a withdrawal for each of the team's open items
// of every withdrawable kind: bounded by the open set. A removal and a
// downgrade sweep once the change has committed; a disable sweeps before it. There is no recipient table (G1's rejected option b). A
// person who never had a push gets a withdrawal for it, which shows nothing.
//
// The changes and their one entry here each:
//   - a removal (an admin's, a manager's): teamMembershipEnded;
//   - a role set or re-role to below operator: teamRoleSet;
//   - a disable: teamItemsWithdrawnBeforeDisable, for every team the person
//     operates or manages, BEFORE the disable (it deletes their phones).
//
// What it closes is one list, withdrawableTeamKinds: asks, drafts and
// outreach batches, tasks in review and officer cap holds.
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

// teamMembershipEnded is a person removed from team: the sweep, and their
// open terminals on the team's agents closed (closeTeamTerminals).
func (s *Server) teamMembershipEnded(team, person int64) {
	s.closeTeamTerminals(team, person)
	s.withdrawTeamItemsFrom(team, person)
}

// teamRoleSet is a person's role in team set to role: the sweep, and their
// open terminals on the team's agents closed, when the new role is below
// operator (a viewer receives no team push and opens no terminal). An add at
// viewer sweeps too, which closes nothing the person was shown.
func (s *Server) teamRoleSet(team, person int64, role authz.TeamRole) {
	if role.Valid() && !role.AtLeast(authz.TeamOperator) {
		s.closeTeamTerminals(team, person)
		s.withdrawTeamItemsFrom(team, person)
	}
}

// userDisabledChanged is everything muster does in this process once a
// person's account is disabled or enabled (the store has committed it): the
// cached user, and their queued messages (the delivery re-checks the account
// as well: sendQueueOwnerRefusal). The withdrawal sweep is NOT here: by now
// the person's paired phones are deleted and a disabled owner's native rows
// are refused (push.PGStore.ForOwner), so it runs BEFORE the disable
// (teamItemsWithdrawnBeforeDisable).
func (s *Server) userDisabledChanged(ctx context.Context, id int64, disabled bool) {
	s.gt.forgetUser(id)
	s.gt.setDisabled(id, disabled)
	if disabled {
		s.cancelQueuedFor(ctx, id, SendQueueCancelDisabled)
	}
}

// teamItemsWithdrawnBeforeDisable is the sweep for a person about to be
// disabled, for every team they operate or manage, run to completion BEFORE
// the disable commits: their browser subscriptions AND their paired phones
// still receive, which after the disable neither of the phone's walls lets
// through (SetDisabledAs deletes the device; ForOwner refuses a disabled
// owner's native rows). Bounded by the pushes' own timeout. A disable that is
// then refused (the last admin) has closed notifications that were still
// theirs: shown again by the next change to each item, and harmless under G1.
func (s *Server) teamItemsWithdrawnBeforeDisable(ctx context.Context, id int64) {
	if !s.notifiable() {
		return
	}
	r, ok := s.ext.Users.(myTeamsReader)
	if !ok {
		return
	}
	teams, err := r.MyTeams(context.WithoutCancel(ctx), id)
	if err != nil {
		s.logger.Printf("push: read user %d's teams for the withdrawal sweep before a disable: %v", id, err)
		return
	}
	var sent sync.WaitGroup
	for _, t := range teams {
		if t.Role.AtLeast(authz.TeamOperator) {
			s.withdrawTeamItemsNow(t.ID, id, &sent)
		}
	}
	sent.Wait()
}

// withdrawal is one notification a sweep closes: its ID and tag, as the push
// that showed it carried them.
type withdrawal struct{ id, tag string }

// withdrawableTeamKind is one kind of team-owned push that is later
// WITHDRAWN (a "resolved" with its tag), and how to read a team's items of
// that kind that are still open.
type withdrawableTeamKind struct {
	name string
	open func(s *Server, ctx context.Context, team int64) ([]withdrawal, error)
}

// withdrawableTeamKinds is THE ONE LIST of team push kinds the sweep closes
// on a departing member's devices. 🔴 IT MUST GROW WITH EVERY NEW KIND: a
// team push that is withdrawn later only from the CURRENT operators and
// managers stays forever on an ex-member's devices unless it is here.
// TestEveryPushIsClassifiedForTheTeamSweep classifies every push the package
// builds (the codename ledger's set) as one of these kinds or as never
// withdrawn from a team member, and fails when either set changes.
var withdrawableTeamKinds = []withdrawableTeamKind{
	{"asks", (*Server).openTeamAsks},
	{"drafts", (*Server).openTeamDrafts},
	{"tasks in review", (*Server).openTeamReviews},
	{"officer cap holds", (*Server).openTeamCapHolds},
}

// openTeamAsks is team's open asks (escalation notices included): their
// attention items' tags.
func (s *Server) openTeamAsks(ctx context.Context, team int64) ([]withdrawal, error) {
	if s.ext.Attention == nil {
		return nil, nil
	}
	open, err := s.ext.Attention.Open(ctx)
	var out []withdrawal
	for _, a := range open {
		if a.Owner == team {
			key := attentionAskKey(a.ID)
			out = append(out, withdrawal{key, attentionTag(key)})
		}
	}
	return out, err
}

// openTeamDrafts is team's pending drafts, and its pending outreach batches
// (a follow or a like is pushed once per batch: notifyBatchAwaiting).
func (s *Server) openTeamDrafts(ctx context.Context, team int64) ([]withdrawal, error) {
	l, ok := s.ext.Plugins.(teamDraftLister)
	if !ok {
		return nil, nil
	}
	drafts, err := l.PendingOwnedBy(ctx, team)
	if err != nil {
		return nil, err
	}
	var out []withdrawal
	batches := map[string]bool{}
	for _, d := range drafts {
		if d.IsOutbound() && plugins.IsBatchKind(d.Target().Kind) {
			if d.Officer != nil {
				id := plugins.BatchID(d.OwnerID, *d.Officer, d.CreatedAt.UTC().Format("2006-01-02"))
				if !batches[id] {
					batches[id] = true
					out = append(out, withdrawal{id, batchTag(id)})
				}
			}
			continue
		}
		out = append(out, withdrawal{strconv.FormatInt(d.ID, 10), draftTag(d.ID)})
	}
	return out, nil
}

// openTeamReviews is team's tasks in ready_for_review: their "ready for
// review" push (notifyTaskDone), withdrawn when the task leaves review.
func (s *Server) openTeamReviews(ctx context.Context, team int64) ([]withdrawal, error) {
	if s.ext.Notes == nil {
		return nil, nil
	}
	page, err := s.ext.Notes.ListPage(ctx, notes.ListFilter{Statuses: []string{notes.StatusReadyForReview}})
	if err != nil {
		return nil, err
	}
	var out []withdrawal
	for _, n := range page.Notes {
		if n.OwnerID == team {
			out = append(out, withdrawal{strconv.FormatInt(n.ID, 10), taskDoneTag(n.ID)})
		}
	}
	return out, nil
}

// openTeamCapHolds is team's officers' cap-hold notices this process showed
// and has not ended (officerCapHoldNotice records them).
func (s *Server) openTeamCapHolds(_ context.Context, team int64) ([]withdrawal, error) {
	s.capHoldsMu.Lock()
	defer s.capHoldsMu.Unlock()
	var out []withdrawal
	for tag := range s.capHolds[team] {
		out = append(out, withdrawal{tag, tag})
	}
	return out, nil
}

// withdrawTeamItemsFrom sends person, in the background, a withdrawal for each
// of team's open items of every withdrawable kind (withdrawableTeamKinds).
func (s *Server) withdrawTeamItemsFrom(team, person int64) {
	if !s.notifiable() || team <= 0 || person <= 0 || person == team {
		return
	}
	s.pushInFlight.Add(1)
	safeGo(s.logger, "team withdraw", func() {
		defer s.pushInFlight.Done()
		s.withdrawTeamItemsNow(team, person, nil)
	})
}

// withdrawTeamItemsNow reads team's open items, as the team
// (attentionPrincipal: ViaAttention, team-acting, so it sees exactly the
// team's rows), and sends person a withdrawal for each. With sent, the
// caller can wait until each withdrawal has been handed to the devices.
// Best effort: a failed read is logged and the other kinds still go.
func (s *Server) withdrawTeamItemsNow(team, person int64, sent *sync.WaitGroup) {
	if !s.notifiable() || team <= 0 || person <= 0 || person == team {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), pushBroadcastTimeout)
	defer cancel()
	ctx = s.attentionPrincipal(ctx, team)
	for _, k := range withdrawableTeamKinds {
		open, err := k.open(s, ctx, team)
		if err != nil {
			s.logger.Printf("push: team %d's open %s for user %d's withdrawal sweep: %v", team, k.name, person, err)
		}
		for _, w := range open {
			s.goNotify(person, push.EventResolved, RouterNotification{Type: push.TypeResolved, ID: w.id, Tag: w.tag, wait: sent}, nil)
		}
	}
}
