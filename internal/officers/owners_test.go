package officers

import (
	"errors"
	"testing"
)

// TestCheckOwnersAdmitsATeamOfficerAndRefusesAHostOne (team sharing phase 3):
// a TEAM may own a pod officer; a team owning a "runtime": "host" officer is
// refused (team hosts are phase 2b), and so is an owner that names nobody.
// CONTROLS: a person owns either kind; each owner is read once.
func TestCheckOwnersAdmitsATeamOfficerAndRefusesAHostOne(t *testing.T) {
	const person, team, ghost = 7, 12, 99
	kinds := map[int64]OwnerKind{person: OwnerPerson, team: OwnerTeam}
	reads := map[int64]int{}
	kindOf := func(id int64) (OwnerKind, error) {
		reads[id]++
		return kinds[id], nil
	}
	pod := func(id string, owner int64) Officer { return Officer{ID: id, Owner: owner} }
	host := func(id string, owner int64) Officer {
		return Officer{ID: id, Owner: owner, Runtime: RuntimeHost, Host: "build-01"}
	}
	for _, c := range []struct {
		what string
		list []Officer
		want error
	}{
		{"a team's pod officer", []Officer{pod("marketing:team", team), pod("support:team", team)}, nil},
		{"a person's pod and host officers (control)", []Officer{pod("marketing:a", person), host("ops:a", person)}, nil},
		{"a team's host officer", []Officer{pod("marketing:team", team), host("ops:team", team)}, ErrTeamHostOfficer},
		{"an owner that names nobody", []Officer{pod("marketing:ghost", ghost)}, ErrOwnerUnknown},
	} {
		reads = map[int64]int{}
		err := CheckOwners(c.list, kindOf)
		if !errors.Is(err, c.want) || (c.want == nil) != (err == nil) {
			t.Errorf("%s: %v; want %v", c.what, err, c.want)
		}
		for id, n := range reads {
			if n != 1 {
				t.Errorf("%s: owner %d read %d times; want once", c.what, id, n)
			}
		}
	}
	boom := errors.New("users unreadable")
	if err := CheckOwners([]Officer{pod("marketing:a", person)}, func(int64) (OwnerKind, error) { return OwnerMissing, boom }); !errors.Is(err, boom) {
		t.Errorf("an unreadable owner: %v; want the read's error", err)
	}
}
