package api

import (
	"context"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/users"
)

// ---------------------------------------------------------------------------
// THE MINIMAL TEAM FAN-OUT (team sharing phase 3; operator decision on #668's
// round 0, Q1). A push names ONE owner (sendPush), and a team has no devices,
// so a TEAM officer's asks, escalations and outbound drafts reached nobody.
// Exactly those three kinds now ALSO go to each of the team's current
// operators and managers, read from team_members at send time
// (users.PGStore.TeamNotifyMembers): a viewer gets nothing, nor does someone
// who has left or is disabled. A personal item is unchanged — a person has no
// members, so the list is empty.
//
// NOT HERE (phase 2b's fan-out): every other team-owned notification, the
// withdrawal of these pushes from members' devices when the ask is answered
// or the draft decided (it is sent to the owner, the team, as today), a
// member's per-team notify switch, and team-labelled titles.
// ---------------------------------------------------------------------------

// teamNotifyReader is the users store's team fan-out read, asserted (not added
// to users.Store) so a store without it — the test fakes — fans out to nobody.
type teamNotifyReader interface {
	TeamNotifyMembers(ctx context.Context, team int64) ([]int64, error)
}

// teamOfficerRecipients is the members a team officer's notification for
// owner also goes to: none when owner is not a team, or when they cannot be
// read (logged; the owner's own push is unaffected).
func (s *Server) teamOfficerRecipients(ctx context.Context, owner int64) []int64 {
	r, ok := s.ext.Users.(teamNotifyReader)
	if !ok || owner <= 0 {
		return nil
	}
	ids, err := r.TeamNotifyMembers(ctx, owner)
	if err != nil {
		s.logger.Printf("push: read team %d's members for a team officer's notification: %v", owner, err)
		return nil
	}
	return ids
}

// teamAuditor is the users store's team-scoped audit writer
// (users.PGStore.AuditTeam), asserted like teamNotifyReader.
type teamAuditor interface {
	AuditTeam(ctx context.Context, by users.Actor, team int64, action, object string, detail map[string]any) error
}

// auditTeam writes one audit_log row naming the acting person (by) and the
// team acted on — a host spawn's lend, a team officer's charter edit (design
// §4: "its audit row names the manager"). The action has already happened;
// a failed write is logged, never silent.
func (s *Server) auditTeam(ctx context.Context, by users.Actor, team int64, action, object string, detail map[string]any) {
	a, ok := s.ext.Users.(teamAuditor)
	if !ok || team <= 0 {
		return
	}
	if err := a.AuditTeam(ctx, by, team, action, object, detail); err != nil {
		s.logger.Printf("audit: %s %s for team %d by user %d: %v", action, object, team, by.UserID, err)
	}
}

// officerAskFiled is askFiled for an OFFICER's ask or escalation notice, plus
// the team fan-out: when its owner is a team, the same notification goes to
// each of the team's operators and managers.
func (s *Server) officerAskFiled(a attention.Ask) {
	s.askFiled(a)
	if !s.notifiable() {
		return
	}
	ctx := context.Background()
	members := s.teamOfficerRecipients(ctx, a.Owner)
	if len(members) == 0 {
		return
	}
	it := s.askPushItem(s.attentionPrincipal(ctx, a.Owner), a)
	for _, m := range members {
		s.attentionNotify(m, it)
	}
}
