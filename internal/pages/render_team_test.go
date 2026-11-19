package pages

import (
	"strings"
	"testing"
)

// TestATeamMembersViewDrawsItsFileFieldOff: with UploadsOff set (a team
// member's own view of a team's page, 0162), an upload block is drawn
// disabled with that sentence and no upload button; without it, a server
// with no uploads says its own sentence, and one with uploads draws the field.
func TestATeamMembersViewDrawsItsFileFieldOff(t *testing.T) {
	b, _, err := Parse([]byte(`[{"type":"upload","id":"deck","label":"Your slides","submit_label":"Upload slides"}]`))
	if err != nil || len(b) != 1 {
		t.Fatalf("parse: %v %v", b, err)
	}
	team := renderBlock(b[0], RenderOptions{Uploads: false, UploadsOff: TeamUploadsOff})
	if !strings.Contains(team, `data-muster-status>Files can&#39;t be sent from a team page yet.</p>`) || strings.Contains(team, "Upload slides") ||
		!strings.Contains(team, `name="file" disabled`) {
		t.Errorf("a team member's view: the file field is not drawn off with the team sentence:\n%s", team)
	}
	if off := renderBlock(b[0], RenderOptions{}); !strings.Contains(off, "File uploads are not available on this server.") || strings.Contains(off, "team page") {
		t.Errorf("a server with no uploads does not say its own sentence:\n%s", off)
	}
	if on := renderBlock(b[0], RenderOptions{Uploads: true}); !strings.Contains(on, "Upload slides") || strings.Contains(on, `name="file" disabled`) {
		t.Errorf("a server with uploads does not draw the field:\n%s", on)
	}
}
