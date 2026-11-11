package notes

import (
	"reflect"
	"testing"
	"time"
)

// TestClaimsPinnedCases is THE claim predicate's table (claims.go): every
// case a literal, each fixture's names distinct from each other and from the
// constant the rule spells ("session "), so a mutant that hardcodes one cannot
// pass for another.
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
		// 🔴 A STATUS NAMES NOBODY: in progress upstream, with no claimant, is no
		// claim (an owner's own in_progress; a plugin from before claimants).
		{"in progress upstream, no claimant: NO claim", Note{Ref: ref(StatusInProgress, "paused")}, nil},
		{"ready for review upstream, no claimant: no claim", Note{Ref: ref(StatusReadyForReview, "pending review")}, nil},
		{"an upstream claim with agent and session: shown by the agent", Note{Ref: ref(StatusOpen, "to do",
			RefClaimant{Agent: "ag_press01", AgentSession: "s-9"})},
			[]Claim{{Source: ClaimUpstream, Who: "ag_press01", SessionID: "s-9", Agent: "ag_press01"}}},
		{"a display name wins over the agent", Note{Ref: ref(StatusOpen, "to do",
			RefClaimant{Agent: "ag_press02", AgentSession: "s-8", Name: "press-bot"})},
			[]Claim{{Source: ClaimUpstream, Who: "press-bot", SessionID: "s-8", Agent: "ag_press02"}}},
		{"a session only: shown by its prefix", Note{Ref: ref(StatusOpen, "to do", RefClaimant{AgentSession: "77aa88bb99cc"})},
			[]Claim{{Source: ClaimUpstream, Who: "session 77aa88bb", SessionID: "77aa88bb99cc"}}},
		{"a claimant naming nobody is dropped", Note{Ref: ref(StatusInProgress, "in progress", RefClaimant{})}, nil},
		// 🔴 A CLAIM IS A SESSION'S PICKUP: an agent or a name without a
		// session is no claim (round 1).
		{"an agent without a session is no claim", Note{Ref: ref(StatusOpen, "to do", RefClaimant{Agent: "ag_press03"})}, nil},
		{"a name without a session is no claim", Note{Ref: ref(StatusOpen, "to do", RefClaimant{Agent: "ag_press04", Name: "R. Doe"})}, nil},
		{"both systems: sessions first", Note{Sessions: []SessionLink{claimed}, Ref: ref(StatusOpen, "to do", RefClaimant{Name: "Q. Smith", AgentSession: "s-7"})},
			[]Claim{{Source: ClaimSession, Who: "lathe bench", SessionID: "sess-aaaa1111-x"}, {Source: ClaimUpstream, Who: "Q. Smith", SessionID: "s-7"}}},
	} {
		if got := Claims(c.n); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}
