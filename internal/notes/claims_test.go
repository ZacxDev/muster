package notes

import (
	"reflect"
	"testing"
	"time"
)

// TestClaimsPinnedCases is THE claim predicate's table (claims.go): every
// case a literal, each fixture's names distinct from each other and from the
// constants the rule spells ("upstream (", "session "), so a mutant that
// hardcodes one cannot pass for another.
func TestClaimsPinnedCases(t *testing.T) {
	at := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	claimed := SessionLink{SessionID: "sess-aaaa1111-x", Role: RoleWorked, ClaimedAt: &at, ClaimedBy: "lathe bench"}
	unnamed := SessionLink{SessionID: "sess-bbbb2222-y", Role: RoleWorked, ClaimedAt: &at}
	worked := SessionLink{SessionID: "sess-cccc3333-z", Role: RoleWorked}
	read := SessionLink{SessionID: "sess-dddd4444-w", Role: RoleRead}
	ref := func(status, label string, cl ...RefClaimant) *Reference {
		return &Reference{Key: "tracker:9", Snapshot: RefSnapshot{Status: status, StatusLabel: label, Claimants: cl}}
	}
	for _, c := range []struct {
		name string
		n    Note
		want []Claim
	}{
		{"nothing", Note{}, nil},
		{"a session that only worked or read it is no claim", Note{Sessions: []SessionLink{worked, read}}, nil},
		{"a session's claim, by its name", Note{Sessions: []SessionLink{worked, claimed}},
			[]Claim{{Source: ClaimSession, Who: "lathe bench", SessionID: "sess-aaaa1111-x"}}},
		{"a session's claim with no name: the id's prefix", Note{Sessions: []SessionLink{unnamed}},
			[]Claim{{Source: ClaimSession, Who: "session sess-bbb", SessionID: "sess-bbbb2222-y"}}},
		{"an open reference with no claimants field", Note{Ref: ref(StatusOpen, "to discuss")}, nil},
		{"an open reference with an empty claimants list", Note{Ref: ref(StatusOpen, "to discuss", []RefClaimant{}...)}, nil},
		{"ready for review upstream is no claim", Note{Ref: ref(StatusReadyForReview, "pending review")}, nil},
		{"an upstream claimant", Note{Ref: ref(StatusOpen, "to do", RefClaimant{Kind: "agent", Name: "press-bot", Session: "s-9"})},
			[]Claim{{Source: ClaimUpstream, Who: "press-bot", SessionID: "s-9", Kind: "agent"}}},
		{"a nameless upstream claimant is dropped", Note{Ref: ref(StatusOpen, "to do", RefClaimant{Kind: "person"})}, nil},
		{"in progress upstream, no claimant: the status is the claim", Note{Ref: ref(StatusInProgress, "paused")},
			[]Claim{{Source: ClaimUpstream, Who: "upstream (paused)"}}},
		{"in progress upstream, no label: the status", Note{Ref: ref(StatusInProgress, "")},
			[]Claim{{Source: ClaimUpstream, Who: "upstream (in_progress)"}}},
		{"in progress upstream WITH a claimant: the claimant only", Note{Ref: ref(StatusInProgress, "in progress", RefClaimant{Name: "Q. Smith"})},
			[]Claim{{Source: ClaimUpstream, Who: "Q. Smith"}}},
		{"both systems: sessions first", Note{Sessions: []SessionLink{claimed}, Ref: ref(StatusOpen, "to do", RefClaimant{Name: "Q. Smith"})},
			[]Claim{{Source: ClaimSession, Who: "lathe bench", SessionID: "sess-aaaa1111-x"}, {Source: ClaimUpstream, Who: "Q. Smith"}}},
	} {
		got := Claims(c.n)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
		if Claimed(c.n) != (len(c.want) > 0) {
			t.Errorf("%s: Claimed = %v", c.name, Claimed(c.n))
		}
	}
}
