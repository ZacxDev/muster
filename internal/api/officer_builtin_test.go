package api

import (
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// TestThePoolsPushSkipsABuiltinOfficerAndTheUsageCollectorDoesNot: the pools
// reporter and the usage collector are configured with officersServing. A
// builtin backend keeps no pools ledger, so the pools reporter never pushes
// to one; it DOES book its instances' Claude usage (officer_llm_usage,
// builtin.Backend.PutUsage), so the usage collector covers it as it covers an
// HQ officer. It used to skip builtin officers too, and their model spend
// was recorded nowhere.
func TestThePoolsPushSkipsABuiltinOfficerAndTheUsageCollectorDoesNot(t *testing.T) {
	hq := officers.Officer{ID: "triage:example"}
	bi := officers.Officer{ID: "marketing:example", Backend: officers.BackendBuiltin}
	if got := officersServing([]officers.Officer{hq, bi}, officers.CapPools); len(got) != 1 || got[0].ID != hq.ID {
		t.Fatalf("officers pushed pools: %v, want only the HQ officer", got)
	}
	if got := officersServing([]officers.Officer{hq, bi}, officers.CapUsage); len(got) != 2 || got[0].ID != hq.ID || got[1].ID != bi.ID {
		t.Fatalf("officers whose usage is collected: %v, want the HQ officer and the builtin one", got)
	}
}
