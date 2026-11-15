package previewgw

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestResolveOwnerNamesATeamByItsID (team sharing 2b.3b): a team owner is
// named team:<id>, and only a live answer whose owner IS that team matches —
// never a person whose login spells the team's name, and a login never
// matches a team's answer. The team's answer is muster's /self for a team
// viewer plugin (owner_kind "team"), parsed from its JSON.
//
// 🔴 RED at 595f5df0: team:<id> was a login no one had, so no host token
// could be minted for a team.
func TestResolveOwnerNamesATeamByItsID(t *testing.T) {
	var team Self
	if err := json.Unmarshal([]byte(`{"plugin_id":4,"plugin_name":"tv","owner_id":9041,"owner_login":"platform","owner_kind":"team","capabilities":["viewer"]}`), &team); err != nil {
		t.Fatal(err)
	}
	if !team.IsTeam() {
		t.Fatalf("/self parsed as %+v; want a team", team)
	}
	person := viewerSelf(9007, "platform")
	answers := []SelfAnswer{{Self: person, Verdict: SelfLive}, {Self: team, Verdict: SelfLive}}
	if s, err := ResolveOwner("team:9041", answers); err != nil || s.OwnerID != 9041 || !s.IsTeam() {
		t.Fatalf("team:9041: %+v, %v; want the team", s, err)
	}
	if s, err := ResolveOwner("platform", answers); err != nil || s.OwnerID != 9007 {
		t.Fatalf("the login platform: %+v, %v; want the person, never the team of that name", s, err)
	}
	if _, err := ResolveOwner("team:9007", answers); err == nil || !strings.Contains(err.Error(), "team 9007") {
		t.Fatalf("team:9007 names a PERSON's id: %v; want refused", err)
	}
	if _, err := ResolveOwner("team:x", answers); err == nil || !strings.Contains(err.Error(), "names no team") {
		t.Fatalf("team:x: %v; want refused", err)
	}
}
