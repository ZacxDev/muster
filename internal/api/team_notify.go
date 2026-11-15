package api

import (
	"context"

	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/users"
)

// ---------------------------------------------------------------------------
// THE TEAM FAN-OUT (team sharing phase 2b, part C; docs/design/team-sharing.md
// "C. The rest of the fan-out"). A push names ONE owner (sendPush), and a team
// has no devices, so a team-owned notification reached nobody. goNotify is
// where every notification is decided, and it is the ONE place the fan-out
// happens: when the notification's owner is a team, goNotify reads the team's
// recipients here (teamPushRecipients) and then runs the WHOLE per-owner
// decision — the member's codename switch, the team prefix, the title rule,
// the member's own mutes and rate cap, sendPush and the router's rule — once
// for each of them. No call site loops over members: a site that did would
// send each member a second copy (TestATeamNotificationReachesEachMemberOnce).
//
// Who: the team's current OPERATORS and MANAGERS whose own notify switch is
// on (team_members.notify, 0133), persons only, not disabled, read at send
// time (users.PGStore.TeamPushTargets). A viewer, someone who has left, a
// disabled person and a member who turned the switch off get nothing.
// A withdrawal ("resolved") reaches the current operators and managers
// whatever their switch says (operator decision G1), and a person removed,
// downgraded or disabled gets one for each of the team's open items
// (withdrawTeamItemsFrom, team_withdraw.go).
//
// A personal owner is unchanged: their one push, as before.
// ---------------------------------------------------------------------------

// teamPushReader is the users store's team fan-out read, asserted (not added
// to users.Store) so a store without it — the test fakes — fans out to nobody
// and sends a personal push exactly as before.
type teamPushReader interface {
	TeamPushTargets(ctx context.Context, owner int64) (users.TeamPushTargets, error)
}

// teamPushPrefixSep joins a team's name to a team push's title
// ("platform · Ana is asking"): the " · " muster's other short labels use.
const teamPushPrefixSep = " · "

// teamPushRecipients is who a notification of event owned by owner goes to,
// and the prefix its title takes: [owner] and "" for a person (and for an
// owner the read cannot classify — logged; a team has no devices, so for a
// team that sends nothing, exactly as before 2b); the team's recipients and
// "<team> · " for a team. skip is left out (a team task comment's author).
// managers narrows a team's recipients to its managers (Managers: switch on),
// for a push only a manager can act on; it never touches a person.
func (s *Server) teamPushRecipients(ctx context.Context, owner int64, event string, skip int64, managers bool) ([]int64, string) {
	r, ok := s.ext.Users.(teamPushReader)
	if !ok {
		return []int64{owner}, ""
	}
	if _, person := s.personOwners.Load(owner); person {
		// A known person (users.kind never changes): their own push, no read.
		return []int64{owner}, ""
	}
	t, err := r.TeamPushTargets(ctx, owner)
	if err != nil {
		// Sent to the owner alone: for a TEAM (no devices) that is nobody.
		// Counted by what muster knows: a known team's push is dropped
		// (team_read_failed); an owner whose kind was never read may be a
		// person, whose push did go (owner_kind_unread).
		reason := "owner_kind_unread"
		if _, team := s.teamOwners.Load(owner); team {
			reason = "team_read_failed"
		}
		metrics.PushSuppressed.WithLabelValues(reason).Inc()
		s.logger.Printf("push: read owner %d's team recipients: %v (sent to the owner alone)", owner, err)
		return []int64{owner}, ""
	}
	if !t.Team {
		s.personOwners.Store(owner, struct{}{})
		return []int64{owner}, ""
	}
	s.teamOwners.Store(owner, struct{}{})
	who := t.Notify
	switch {
	case event == push.EventResolved:
		who = t.Withdraw
	case managers:
		who = t.Managers
	}
	out := make([]int64, 0, len(who))
	for _, m := range who {
		if m != skip {
			out = append(out, m)
		}
	}
	return out, mirror.OneLine(t.Name, users.TeamNameMax) + teamPushPrefixSep
}

// teamAuditor is the users store's team-scoped audit writer
// (users.PGStore.AuditTeam), asserted like teamPushReader.
type teamAuditor interface {
	AuditTeam(ctx context.Context, by users.Actor, team int64, action, object string, detail map[string]any) error
}

// auditTeam writes one audit_log row naming the acting person (by) and the
// team acted on — a host spawn's lend, a team officer's charter edit (design
// §4: "its audit row names the manager"). The action has already happened;
// a failed write is logged, never silent.
func (s *Server) auditTeam(ctx context.Context, by users.Actor, team int64, action, object string, detail map[string]any) {
	s.auditTeamLevel(ctx, by, team, action, object, detail, false)
}

// auditTeamLevel is auditTeam with a failed write logged at ERROR when
// security is set (a manager's credential rotation or delete: a row an
// auditor must be able to find).
func (s *Server) auditTeamLevel(ctx context.Context, by users.Actor, team int64, action, object string, detail map[string]any, security bool) {
	a, ok := s.ext.Users.(teamAuditor)
	if !ok || team <= 0 {
		return
	}
	if err := a.AuditTeam(ctx, by, team, action, object, detail); err != nil {
		level := ""
		if security {
			level = "ERROR "
		}
		s.logger.Printf("%saudit: %s %s for team %d by user %d: %v", level, action, object, team, by.UserID, err)
	}
}
