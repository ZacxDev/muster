package ui

import (
	"strings"
	"testing"
)

// TestTheNewTaskFormsRepoPickerFollowsItsOwner (team sharing 4.8): with an
// Owner select on the new-task form, the repository picker fetches its list
// WITH the select's value and again when it changes — so the list is the
// chosen owner's (/ui/agents/repos?owner=), never just the viewer's. With no
// select, the picker is the plain one: fetched once, on load, with nothing
// included.
func TestTheNewTaskFormsRepoPickerFollowsItsOwner(t *testing.T) {
	with := renderNode(t, NotesModalBodyFor(nil, false, "", []TaskOwnerOption{{ID: 16, Label: "platform"}}))
	if !strings.Contains(with, `hx-include="#task-owner"`) || !strings.Contains(with, `hx-trigger="load, change from:#task-owner"`) ||
		!strings.Contains(with, `id="task-owner"`) {
		t.Errorf("the new-task form with an Owner select: its repository picker does not follow the select:\n%s", with)
	}
	without := renderNode(t, NotesModalBodyFor(nil, false, "", nil))
	if strings.Contains(without, "hx-include") || strings.Contains(without, "change from:") {
		t.Errorf("the new-task form with no Owner select: the picker follows something:\n%s", without)
	}
	if got := repoOptionsURL(16); got != "/ui/agents/repos?owner=16" {
		t.Errorf("a team task's picker URL: %q", got)
	}
	if got := repoOptionsURL(0); got != "/ui/agents/repos" {
		t.Errorf("an ownerless picker URL: %q", got)
	}
}
