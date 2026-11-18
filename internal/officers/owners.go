package officers

import (
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// TEAM OFFICERS (docs/design/team-sharing.md, phase 3): an officer's
// MUSTER_OFFICERS `owner` may be a TEAM (users.kind = 'team'). Its instances,
// its builtin store rows, its asks and its outbound drafts are then the
// team's, and its runtime acts as the team (authz.ViaOfficer is a team door).
//
// CheckOwners is THE team rule over officers, given how each owner id reads
// in users: at boot over MUSTER_OFFICERS (api.EnableOfficers), and over every
// DB-declared officer at its create or replace (checkDeclaredOwner) and at
// each roster load (resolveDeclared, after an auto runtime is resolved). It
// refuses:
//   - an owner that names nobody (ErrOwnerUnknown): its every pass would act
//     as a user that does not exist, and an escalation to it reaches nobody;
//   - a TEAM owner of a "runtime": "host" officer that declares `connectors`
//     (ErrTeamHostOfficerConnectors; operator decision G3, team sharing
//     2b.5). A team's connector requests would be connector_grants rows the
//     team owns, which are personal and readable by no member, so nobody
//     could decide one. Refused until team connector requests have a decider.
//
// A team MAY own a host officer (team sharing 2b.5): its host is a team host,
// one connected with the team's mirror token. Its runtime acts as the team
// (authz.ViaOfficer is team-acting), so every spawn, close and listing reaches
// only the team's (team, host) — never a member's personal host of the same
// name (the hub key). Boot cannot know which hosts will connect, so the host
// is not checked here.
// ---------------------------------------------------------------------------

// OwnerKind is how one owner id reads in users.
type OwnerKind int

const (
	// OwnerMissing: no users row has that id.
	OwnerMissing OwnerKind = iota
	// OwnerPerson: a person.
	OwnerPerson
	// OwnerTeam: a team.
	OwnerTeam
)

// ErrOwnerUnknown: an officer's owner names no user.
var ErrOwnerUnknown = errors.New("officers: an officer's owner names no user")

// ErrTeamHostOfficerConnectors: a team owns a host officer that declares
// connectors (operator decision G3).
var ErrTeamHostOfficerConnectors = errors.New(`officers: a team's "runtime": "host" officer cannot declare connectors: ` +
	`a team's connector requests have no one to decide them (team sharing, decision G3); remove connectors, or run it in pods`)

// CheckOwners checks every officer's owner, read through kindOf (one call per
// distinct owner).
func CheckOwners(list []Officer, kindOf func(owner int64) (OwnerKind, error)) error {
	kinds := map[int64]OwnerKind{}
	for _, o := range list {
		k, seen := kinds[o.Owner]
		if !seen {
			var err error
			if k, err = kindOf(o.Owner); err != nil {
				return fmt.Errorf("officer %s: reading owner %d: %w", o.ID, o.Owner, err)
			}
			kinds[o.Owner] = k
		}
		switch {
		case k == OwnerMissing:
			return fmt.Errorf("officer %s: owner %d: %w", o.ID, o.Owner, ErrOwnerUnknown)
		case k == OwnerTeam && o.OnHost() && len(o.Connectors) > 0:
			return fmt.Errorf("officer %s: owner %d is a team: %w", o.ID, o.Owner, ErrTeamHostOfficerConnectors)
		}
	}
	return nil
}
