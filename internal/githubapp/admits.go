package githubapp

import (
	"context"

	"github.com/ZacxDev/muster/internal/authz"
)

// Admits is THE rule of who may use the deployment's GitHub App, as a
// predicate over ctx's principal: an ADMIN, or the TEAM ACTING PRINCIPAL of a
// team in teams (MUSTER_TEAMS_ON_GITHUB_APP, team sharing phase 4, slice 4.8).
//
// 🔴 ONE RULE, ONE PLACE. cmd/muster-server passes it to
// connectors.WithVirtual (the `github` connector: the agent's status and
// request routes, the approve route, the proxy's token mint), and
// internal/api's githubApp applies it to every repository reader (the
// dispatch checks, repos_list, the default branch). A second spelling at
// either would let one door see repositories the other refuses.
//
//   - A person who is not an admin is never admitted, a member of a listed
//     team included: the App is lent to the TEAM's agents and its managers'
//     door (authz.ViaTeamAdmin), which act as the team, never to a member's
//     own principal. A member's personal chief and personal agents keep no
//     repositories.
//   - 🔴 A team is admitted only through an EXPLICIT set of doors,
//     teamAppDoors: its agents' own tokens on the API and MCP (authz.ViaAgent:
//     the team chief's repos_list and dispatch checks, a team agent's
//     connector status and request routes), the connect proxy's token mint
//     (authz.ViaConnect), and the manager's door (authz.ViaTeamAdmin: the
//     approval on /teams/{id}/connectors). Every OTHER door a team acts
//     through (authz's teamActingVias) is refused here, though WithPrincipal
//     admits a team at it: a team HOST (authz.ViaMirrorHost), the question
//     watcher (ViaAsk), the approval relay (ViaRelay), a team officer's
//     runtime and the per-owner background loops. None of them reads a
//     repository today; listing the doors means a team-host tool or loop
//     added later does not inherit the App silently — it is a line here.
//   - The System principal names no user and is not admitted.
//
// teams is read, never written; nil admits admins only (the rule before 4.8).
func Admits(teams map[int64]bool) func(context.Context) bool {
	return func(ctx context.Context) bool {
		if authz.IsAdmin(ctx) {
			return true
		}
		p, ok := authz.From(ctx)
		return ok && !p.All() && p.Team && p.UserID > 0 && teamAppDoors[p.Via] && teams[p.UserID]
	}
}

// teamAppDoors is the closed set of doors through which a listed team's
// acting principal is lent the App (Admits). Read, never written;
// TestAdmitsIsAnAdminOrAListedTeamsOwnPrincipal pins it door by door.
var teamAppDoors = map[authz.Via]bool{
	authz.ViaAgent:     true,
	authz.ViaConnect:   true,
	authz.ViaTeamAdmin: true,
}
