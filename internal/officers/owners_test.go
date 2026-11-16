package officers

import (
	"errors"
	"strings"
	"testing"
)

// ownersFixture is the kinds CheckOwners reads: person 7, team 12, nobody at
// 99 (pairwise distinct, and none equal to a constant the rule names). reads
// counts the reads per owner.
func ownersFixture() (kindOf func(int64) (OwnerKind, error), reads map[int64]int) {
	kinds := map[int64]OwnerKind{7: OwnerPerson, 12: OwnerTeam}
	reads = map[int64]int{}
	return func(id int64) (OwnerKind, error) {
		reads[id]++
		return kinds[id], nil
	}, reads
}

func ownersPod(id string, owner int64) Officer { return Officer{ID: id, Owner: owner} }

func ownersHost(id string, owner int64, connectors ...string) Officer {
	return Officer{ID: id, Owner: owner, Runtime: RuntimeHost, Host: "build-01", Connectors: connectors}
}

// TestCheckOwnersAdmitsATeamHostOfficer (team sharing 2b.5): a TEAM may own a
// "runtime": "host" officer — its host is a team host. Every owner is read
// once. CONTROLS: a team's pod officer and a person's officers of either kind
// are admitted, and an owner that names nobody is still refused.
//
// 🔴 RED at the base (c96acc08): CheckOwners refused a team's host officer
// (ErrTeamHostOfficer, "until team hosts").
func TestCheckOwnersAdmitsATeamHostOfficer(t *testing.T) {
	for _, c := range []struct {
		what string
		list []Officer
		want error
	}{
		{"a team's host officer", []Officer{ownersPod("marketing:team", 12), ownersHost("ops:team", 12)}, nil},
		{"a team's pod officer (control)", []Officer{ownersPod("marketing:team", 12), ownersPod("support:team", 12)}, nil},
		{"a team's pod officer with connectors (control: G3 is the host's)", []Officer{{ID: "data:team", Owner: 12, Connectors: []string{"warehouse"}}}, nil},
		{"a person's pod and host officers, with connectors (control)", []Officer{ownersPod("marketing:a", 7), ownersHost("ops:a", 7, "warehouse")}, nil},
		{"an owner that names nobody", []Officer{ownersPod("marketing:ghost", 99)}, ErrOwnerUnknown},
	} {
		kindOf, reads := ownersFixture()
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
	if err := CheckOwners([]Officer{ownersPod("marketing:a", 7)}, func(int64) (OwnerKind, error) { return OwnerMissing, boom }); !errors.Is(err, boom) {
		t.Errorf("an unreadable owner: %v; want the read's error", err)
	}
}

// TestCheckOwnersRefusesATeamHostOfficerWithConnectors (team sharing 2b.5,
// operator decision G3): a TEAM's host officer that declares `connectors` is
// refused, naming the officer and ErrTeamHostOfficerConnectors — whatever its
// place in the roster (after an admitted officer of the same team, too). The
// CONTROLS are the same officer without connectors, a team's POD officer with
// them and a PERSON's host officer with them: each admitted.
//
// 🔴 RED at the base (c96acc08): there is no ErrTeamHostOfficerConnectors;
// the base refused every team host officer alike.
func TestCheckOwnersRefusesATeamHostOfficerWithConnectors(t *testing.T) {
	kindOf, _ := ownersFixture()
	err := CheckOwners([]Officer{ownersHost("ops:team", 12), ownersHost("data:team", 12, "warehouse")}, kindOf)
	if !errors.Is(err, ErrTeamHostOfficerConnectors) || !strings.Contains(err.Error(), "data:team") {
		t.Fatalf("a team host officer with connectors: %v; want ErrTeamHostOfficerConnectors naming data:team", err)
	}
	for _, c := range []struct {
		what string
		o    Officer
	}{
		{"the same team host officer without connectors", ownersHost("data:team", 12)},
		{"a team's pod officer with connectors", Officer{ID: "data:team", Owner: 12, Connectors: []string{"warehouse"}}},
		{"a person's host officer with connectors", ownersHost("data:a", 7, "warehouse")},
	} {
		kindOf, _ := ownersFixture()
		if err := CheckOwners([]Officer{c.o}, kindOf); err != nil {
			t.Errorf("CONTROL %s: %v; want admitted", c.what, err)
		}
	}
}
