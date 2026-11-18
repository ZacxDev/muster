package ui

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/notes"
)

// ---------------------------------------------------------------------------
// THE PRIVILEGE AND RUNBOOK SURFACES ARE GONE FROM EVERY DOCUMENT THE UI DRAWS.
//
// Privileges were superseded by connectors and runbooks were unused; both were
// removed whole. These assertions are about the RENDERED MARKUP, because that is
// what a user meets: a tab, a lazy mount, a form field or a disclosure that
// still names a removed route is a control that 404s when touched.
//
// removedMarkers are strings no rendered document may contain. Each is a route,
// a field name or a label that only the removed surfaces ever wrote.
// ---------------------------------------------------------------------------
var removedMarkers = []string{
	`/ui/runbooks`, `/ui/privileges`, `/ui/privilege-requests`, `/ui/agents/profiles`,
	`href="/runbooks"`, `href="/privileges"`, `/grants`,
	`name="grant_profile"`, `grant_profile`,
	`id="panel-runbooks"`, `id="panel-privileges"`, `id="privilege-requests"`,
	`Grant privileges`, `Privilege requests`, `Privilege profiles`,
	`>Privileges<`, `Edit Privileges`,
	`sse:privilege.created`,
	// The Repos tab and the GitHub OAuth connection it showed (the GitHub App
	// replaced both), and the light-theme toggle (dark is the only theme).
	`/ui/repos`, `href="/repos"`, `data-tab="repos"`, `id="panel-repos"`,
	`/github/connect`, `/github/disconnect`,
	`data-theme-toggle`, `musterSetTheme`,
	// The web UI's dispatch form, deleted by docs/design/chief-dispatch.md
	// phase 2 with nothing in its place (D8): the sidebar's "+", the home page's
	// "Dispatch an agent", a task card's Dispatch, both forms and "Save for
	// later", and the routes they used.
	`id="sidebar-deploy"`, `id="home-deploy"`, `/ui/agents/new`, `/ui/agents/notes`,
	`id="agent-modal"`, `value="save"`, `Save for later`, `data-dispatch-off-hint`,
	`hx-post="/agents"`,
}

func assertNoRemovedMarkers(t *testing.T, what, doc string) {
	t.Helper()
	if strings.TrimSpace(doc) == "" {
		t.Fatalf("%s rendered EMPTY; the absence checks below would prove nothing", what)
	}
	for _, m := range removedMarkers {
		if strings.Contains(doc, m) {
			t.Errorf("%s still renders %q — a removed surface", what, m)
		}
	}
}

// TestNoTabForPrivilegesOrRunbooks: the tab registry (which also derives the
// shell routes) carries neither key.
func TestNoTabForPrivilegesOrRunbooks(t *testing.T) {
	keys := TabKeys()
	if len(keys) == 0 {
		t.Fatal("TabKeys is empty; the check below would pass over nothing")
	}
	for _, k := range keys {
		if k == "runbooks" || k == "privileges" || k == "repos" {
			t.Errorf("tab %q is still registered", k)
		}
	}
}

// TestNoShellDocumentRendersARemovedSurface renders EVERY visible tab under
// every feature shape (the shell is one document; any tab's panel is in it).
func TestNoShellDocumentRendersARemovedSurface(t *testing.T) {
	for _, feat := range []Features{{}, {Connectors: true}} {
		for _, tab := range visibleTabs(feat) {
			doc := renderString(t, Page(tab.Key, feat))
			// Positive control: the shell does mount the task board, so an empty
			// or truncated render cannot pass the absence checks.
			if !strings.Contains(doc, `hx-get="/ui/tasks"`) {
				t.Fatalf("Page(%q) does not mount /ui/tasks; this render is not the shell", tab.Key)
			}
			assertNoRemovedMarkers(t, "Page("+tab.Key+")", doc)
		}
	}
}

// TestNoTaskFormRendersAPrivilegeField covers the forms that carried the
// "Grant privileges" field or its disclosure and still exist: the new-task
// modal's Advanced disclosure and the task edit form. (The two dispatch forms
// that also carried it were deleted, docs/design/chief-dispatch.md phase 2.)
func TestNoTaskFormRendersAPrivilegeField(t *testing.T) {
	cases := map[string]string{
		"NotesModalBody":     renderString(t, NotesModalBody(nil, false)),
		"NotesEditModalBody": renderString(t, NotesEditModalBody(NoteEditView{Note: notes.Note{ID: 4, Body: "x"}})),
	}
	for name, doc := range cases {
		// Positive control: each of these still draws its Advanced/Model controls,
		// so the absence below is the privilege field alone going, not the form.
		if !strings.Contains(doc, `name="model"`) {
			t.Fatalf("%s rendered no model field; it is not the form this test means", name)
		}
		assertNoRemovedMarkers(t, name, doc)
	}
}

// TestAStaleRunbookTagRendersAsAPlainTag: tasks written before the removal can
// still carry `runbook:<name>`. Nothing routes on it any more, so the card must
// render it as an ordinary descriptive chip — never as a routing chip promising
// a dispatch that no longer exists — and must render at all.
func TestAStaleRunbookTagRendersAsAPlainTag(t *testing.T) {
	out := renderTagCard(t, notes.Note{ID: 1, Body: "x", Tags: []string{"runbook:deploy"}})
	if !strings.Contains(out, `data-tag="runbook:deploy"`) {
		t.Fatalf("the stale tag is not rendered at all:\n%s", out)
	}
	if !strings.Contains(out, `data-tag-kind="descriptive"`) || strings.Contains(out, `data-tag-kind="routing"`) {
		t.Errorf("a stale runbook: tag must render as a DESCRIPTIVE chip:\n%s", out)
	}
	if strings.Contains(strings.ToLower(out), "dispatch via") {
		t.Errorf("the chip still promises a runbook dispatch:\n%s", out)
	}
	if notes.IsRoutingTag("runbook:deploy") {
		t.Error("notes.IsRoutingTag still treats runbook: as routing")
	}
}

// TestTheDispatchUIIsGone (docs/design/chief-dispatch.md phase 2, D8): the
// sidebar draws no "+" and no "not set up" hint, the home page no "Dispatch an
// agent", and an agent-less task card no Dispatch — on the widest shell. Each
// document carries a positive control first, so an empty render cannot pass.
func TestTheDispatchUIIsGone(t *testing.T) {
	var home strings.Builder
	if err := RenderAgentsHome(&home, featuresAllOn); err != nil {
		t.Fatal(err)
	}
	docs := map[string]struct{ doc, control string }{
		"sidebar":   {renderString(t, sidebar("", "", false, featuresAllOn)), `id="` + navAgentsLabelID + `"`},
		"home":      {home.String(), `id="` + HomeEmptyID + `"`},
		"task card": {renderString(t, noteCard(TaskCardView{Note: notes.Note{ID: 5, Body: "x", Status: notes.StatusOpen}})), `id="task-5"`},
		"task detail": {renderString(t, noteCard(TaskCardView{Note: notes.Note{ID: 6, Body: "y", Status: notes.StatusOpen}, Detail: true})),
			`id="task-6"`},
	}
	for name, d := range docs {
		if !strings.Contains(d.doc, d.control) {
			t.Fatalf("%s: control %q is missing, so this render is not the document under test", name, d.control)
		}
		assertNoRemovedMarkers(t, name, d.doc)
		// The task card's Dispatch button carried this title; nothing else does.
		if strings.Contains(d.doc, "Dispatch agent for this task") || strings.Contains(d.doc, "data-dispatch-gated") {
			t.Errorf("%s still draws a task card's Dispatch button", name)
		}
	}
	if strings.Contains(docs["home"].doc, ">Dispatch an agent<") {
		t.Error(`the home page still offers "Dispatch an agent"`)
	}
}
