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
// CheckOwners is the boot check over the declared roster, given how each
// owner id reads in users. It refuses:
//   - an owner that names nobody (ErrOwnerUnknown): its every pass would act
//     as a user that does not exist, and an escalation to it reaches nobody;
//   - a TEAM owner of a "runtime": "host" officer (ErrTeamHostOfficer): a host
//     officer's sessions run on a mirror host its owner's token connects, and
//     a team mirror token is phase 2b's. Until then the hub never lists a
//     team's host, so every spawn would be refused at the host.
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

// ErrTeamHostOfficer: a team owns a host officer.
var ErrTeamHostOfficer = errors.New(`officers: a team cannot own a "runtime": "host" officer until team hosts (team sharing phase 2b)`)

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
		case k == OwnerTeam && o.OnHost():
			return fmt.Errorf("officer %s: owner %d is a team: %w", o.ID, o.Owner, ErrTeamHostOfficer)
		}
	}
	return nil
}
