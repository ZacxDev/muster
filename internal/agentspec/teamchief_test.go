package agentspec

import (
	"strings"
	"testing"
)

// TestATeamsChiefIsToldOnlyWhatItsToolsEnforce: the team chief's section
// states the limits its tools actually have — no lifecycle tools, and no
// dispatch until team dispatch arrives — and no longer claims rules nothing
// enforces (a manager-only lifecycle "not yours on a member's say-so", "one
// member does not outrank another"). The closing rules are pinned WHOLE, so a
// reword is a deliberate change to this test.
//
// 🔴 RED at 23eb2f32: the section claimed both unenforced rules and said
// nothing of dispatch.
func TestATeamsChiefIsToldOnlyWhatItsToolsEnforce(t *testing.T) {
	got := TeamChiefSection("platform-crew")
	const rules = `- When two members ask for conflicting things, say so to both and let them
  settle it; muster does not rank one member's message above another's.
- You have no tools to start, stop or delete the team's agents, or to turn
  yourself on or off: the team's managers do that from muster's pages.
- You cannot start new agents for the team yet: team dispatch arrives in a later
  release, and agent_dispatch refuses it. Say so when a member asks for one.
`
	if !strings.HasSuffix(got, rules) {
		t.Errorf("the team section does not end with the enforced rules, verbatim; it ends:\n%s", got[max(0, len(got)-600):])
	}
	for _, claim := range []string{"say-so", "does not outrank", "not yours to do"} {
		if strings.Contains(got, claim) {
			t.Errorf("the team section still claims %q, which no tool enforces", claim)
		}
	}
}
