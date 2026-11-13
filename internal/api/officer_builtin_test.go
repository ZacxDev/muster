package api

import (
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// TestThePoolsAndUsagePushesSkipABuiltinOfficer: the pools reporter and the
// usage collector are configured with officersServing, so an officer whose
// backend keeps no pools or usage ledger (a builtin one) is never pushed to
// on their cadence; an HQ officer still is.
func TestThePoolsAndUsagePushesSkipABuiltinOfficer(t *testing.T) {
	hq := officers.Officer{ID: "triage:example"}
	bi := officers.Officer{ID: "marketing:example", Backend: officers.BackendBuiltin}
	for _, c := range []string{officers.CapPools, officers.CapUsage} {
		got := officersServing([]officers.Officer{hq, bi}, c)
		if len(got) != 1 || got[0].ID != hq.ID {
			t.Fatalf("officers pushed %s: %v, want only the HQ officer", c, got)
		}
	}
}
