package ui

import (
	"strings"
	"testing"
)

// TestEveryViewPageNamesItsUser: the four VIEW pages (internal/api
// viewDocument) carry the user marker a restored page compares against its own
// fresh copy (restoreJS) — the different-user check that holds where
// Clear-Site-Data is not honoured. A page with no user draws none.
func TestEveryViewPageNamesItsUser(t *testing.T) {
	feat := Features{SnapUser: 9, SnapBuild: "v9"}
	nav := NavChrome{Feat: feat}
	pages := map[string]string{
		"agent page":      renderString(t, AgentDetailPage(AgentDetailView{Name: "keen-wren"}, feat)),
		"rendered view":   renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Nav: nav})),
		"mirror terminal": renderString(t, AgentTerminalPage(AgentTerminalView{Name: "main", Nav: nav})),
		"agent terminal":  renderString(t, AgentTerminalPage(AgentTerminalView{Name: "keen-wren", Nav: nav})),
	}
	want := `<meta name="` + UserMarkerName + `" content="9">`
	for name, page := range pages {
		if n := strings.Count(page, want); n != 1 {
			t.Errorf("%s: the user marker %s appears %d times; want once", name, want, n)
		}
		if !strings.Contains(page, UpdatingChipAttr) {
			t.Errorf("%s: no Updating… chip — restoreJS re-requests a page only when it draws one", name)
		}
	}
	// Another user's page names THAT user (the comparison has two values).
	other := renderString(t, AgentTerminalPage(AgentTerminalView{Name: "main", Nav: NavChrome{Feat: Features{SnapUser: 12}}}))
	if !strings.Contains(other, `<meta name="`+UserMarkerName+`" content="12">`) {
		t.Error("the marker does not carry the page's own user")
	}
	if bare := renderString(t, AgentTerminalPage(AgentTerminalView{Name: "main"})); strings.Contains(bare, `<meta name="`+UserMarkerName+`"`) {
		t.Error("a page with no user draws a user marker")
	}
}
