package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/notes"
)

func renderTaskDetail(t *testing.T, v TaskCardView) string {
	t.Helper()
	var b bytes.Buffer
	if err := RenderTaskDetail(&b, v, featuresAllOn); err != nil {
		t.Fatalf("render task detail: %v", err)
	}
	return b.String()
}

func detailFixture() TaskCardView {
	return TaskCardView{Note: notes.Note{
		ID: 613, Title: "Wire the detail page", Body: "the body prose nobody else spells",
		Status: notes.StatusOpen, CreatedAt: time.Unix(1_700_000_000, 0),
		Comments: []notes.Comment{{ID: 1, NoteID: 613, Author: "agent-x", Body: "a comment nobody else spells"}},
		Sessions: []notes.SessionLink{{SessionID: "sess-aaaa1111", Role: notes.RoleWorked, DetailAvailable: true}},
	}}
}

// TestTaskDetailPageRendersTheWholeTask: the page is the ONE place a task's
// body, session thread, comments and comment form are rendered, so all four are
// pinned together — the board card renders none of them.
func TestTaskDetailPageRendersTheWholeTask(t *testing.T) {
	out := renderTaskDetail(t, detailFixture())

	for _, want := range []string{
		"<!doctype html>",                     // a standalone document
		`id="task-613"`,                       // the card every mutation route targets
		"data-task-body",                      // the detail-only body block
		"the body prose nobody else spells",   // the rendered body
		"a comment nobody else spells",        // the thread
		`hx-post="/tasks/613/comments"`,       // the add-comment form
		"data-task-session",                   // the session thread rows
		`hx-patch="/tasks/613/status"`,        // the status control still works here
		`id="task-modal-body"`,                // Edit's swap target must EXIST on this page
		`id="agent-modal-body"`,               // …and Dispatch's
		"/static/vendor/idiomorph-ext.min.js", // morph:outerHTML swaps need it loaded
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the task detail page is missing %q\n---\n%s", want, out)
		}
	}
	// hx-ext="morph" must be ACTIVE on an ancestor of the card, or every
	// `hx-swap="morph:outerHTML"` on it silently degrades to a plain swap — which
	// re-creates the nested comment form and drops its htmx submit binding, so the
	// NEXT comment posts as a native form navigation.
	if !strings.Contains(out, `hx-ext="morph"`) {
		t.Errorf("the detail page must enable the morph extension\n---\n%s", out)
	}
}

// TestTaskDetailPageWiresItsMutationSurface.
//
// 🔴 EVERY MUTATION ON THIS PAGE USED TO FAIL SILENTLY. htmx does not swap a
// non-2xx response, and the listeners that turn a failure into a toast
// (htmx:responseError / htmx:sendError / htmx:timeout) live in exactly one
// place — resyncScript, which the shell renders and this page did not. So a task
// dismissed in another tab, then given a new Status here, answered 404: no swap,
// no toast, and the <select> still reading "Complete". The page also had no
// timeout (the board inherits 15s from #tasks-list) and no liveness at all, so a
// comment posted by an agent never appeared — on the very page a "New comment on
// task #N" push opens.
//
// The four things asserted here are one feature: a mutation surface you can
// trust to tell you when it failed, and to show you what changed underneath it.
func TestTaskDetailPageWiresItsMutationSurface(t *testing.T) {
	out := renderTaskDetail(t, detailFixture())

	for _, tc := range []struct{ want, why string }{
		{"htmx:responseError", "a failed mutation (404/409/500) must raise a toast, not fail silently"},
		{"htmx:sendError", "an offline/aborted request must raise a toast"},
		{"htmx:timeout", "the 15s cap below must be VISIBLE when it fires, or it is just a silent abort"},
		{`hx-request="{&#34;timeout&#34;:15000}"`, "every request this page issues must be BOUNDED, as on the board"},
		// 🔴 THE EXTENSION SCRIPT IS PART OF THE FEATURE, NOT A DETAIL. Deleting
		// `Script(Src("/static/vendor/sse.js"))` was the ONE mutation of 19 that
		// survived the whole Go suite: hx-ext="sse" and sse-connect are inert
		// attributes without it, so liveness reverts to write-only — the exact
		// symptom this test exists for — and every other assertion here stays
		// green. Pin the script, not just the attributes it activates.
		{"/static/vendor/sse.js", "hx-ext=\"sse\" and sse-connect are silent no-ops unless the SSE extension is loaded"},
		{`hx-ext="sse"`, "…and the extension must be ENABLED on the wrapper, or sse-connect is just an attribute"},
		{`sse-connect="/events"`, "the page must receive out-of-band task changes"},
		{"sse:task.changed from:body", "…and re-fetch its card when one arrives"},
		{"muster:resync from:body", "…and after a background/foreground cycle, which SSE alone can miss"},
		{`hx-get="/ui/tasks/613/card"`, "the re-fetch needs a route that returns THIS card"},
	} {
		if !strings.Contains(out, tc.want) {
			t.Errorf("the detail page is missing %q — %s\n---\n%s", tc.want, tc.why, out)
		}
	}

	// 🔴 The liveness attributes must NOT be on the card itself. Every mutation
	// replaces #task-613 with markup from noteCard, which (correctly) carries none
	// of them — so a version that put them on the card would work exactly once and
	// then go dead, silently.
	card := out[strings.Index(out, `id="task-613"`):]
	card = card[:strings.Index(card, ">")+1]
	for _, bad := range []string{"sse-connect", "hx-trigger", "hx-request"} {
		if strings.Contains(card, bad) {
			t.Errorf("%s is on the <article> itself, so the first morph swap deletes it:\n%s", bad, card)
		}
	}
}

// TestTaskDetailPageBackLinkIsNotBoosted.
//
// 🔴 THE REASON WRITTEN HERE USED TO BE BACKWARDS, and it is why the
// X-Muster-View leak shipped. It said a boosted trip to /tasks "swaps only the
// <body>" and so would arrive at a shell WITHOUT this document's body-level
// attributes. htmx boosts with swap=innerHTML on document.body: an innerHTML
// swap replaces a node's CHILDREN and leaves its ATTRIBUTES alone, so those
// attributes SURVIVE onto the board. That is what leaked the detail shape.
//
// The link stays unboosted because it is the one link whose whole job is to
// LEAVE this document — a plain navigation re-parses the real shell with no
// inheritance question at all. The eight sidebar tabs boost across that same
// seam by design; what keeps THEM safe is that nothing stampable is left on
// <body> (TestNoDocumentsBodyCarriesAnythingABoostedNavCanStrand).
func TestTaskDetailPageBackLinkIsNotBoosted(t *testing.T) {
	out := renderTaskDetail(t, detailFixture())

	i := strings.Index(out, "data-task-back")
	if i < 0 {
		t.Fatalf("the detail page renders no back link\n---\n%s", out)
	}
	open := strings.LastIndex(out[:i], "<a ")
	tag := out[open : i+strings.Index(out[i:], ">")+1]
	if !strings.Contains(tag, `href="/tasks"`) {
		t.Errorf("the back link must point at the board, got:\n%s", tag)
	}
	if !strings.Contains(tag, `hx-boost="false"`) {
		t.Errorf(`the back link must carry hx-boost="false", got:`+"\n%s", tag)
	}
}

// TestTaskDetailPageHeadingFallsBackToTheTaskNumber: an untitled, directory-less
// task still gets a heading with TEXT. An <h1> that renders nothing fails axe's
// page-has-heading-one exactly like having no <h1> at all, and
// TestEveryDocumentHasExactlyOneH1 asserts both halves — but only against ONE
// fixture, so the empty-label case needs its own.
func TestTaskDetailPageHeadingFallsBackToTheTaskNumber(t *testing.T) {
	out := renderTaskDetail(t, TaskCardView{Note: notes.Note{ID: 614, Status: notes.StatusOpen}})
	if !strings.Contains(out, "Task #614") {
		t.Errorf("an untitled, directory-less task must still get a named heading\n---\n%s", out)
	}
	// And the title falls back through TaskTitle to the directory when there is
	// one — the label every other surface uses, so the page agrees with them.
	out2 := renderTaskDetail(t, TaskCardView{Note: notes.Note{ID: 615, Directory: "civitai · TICKET-9", Status: notes.StatusOpen}})
	if !strings.Contains(out2, "civitai · TICKET-9") {
		t.Errorf("the heading must fall back to TaskTitle's directory label\n---\n%s", out2)
	}
}

// TestTaskDetailPageIsRenderedInDetailShapeEvenIfTheCallerForgot: the page sets
// Detail itself. A caller passing a bare view must not get a page whose "body"
// is a snippet and a link back to itself.
func TestTaskDetailPageIsRenderedInDetailShapeEvenIfTheCallerForgot(t *testing.T) {
	v := detailFixture()
	v.Detail = false // the caller forgot
	out := renderTaskDetail(t, v)
	if !strings.Contains(out, "data-task-body") {
		t.Errorf("TaskDetailPage must force the detail shape regardless of the passed view\n---\n%s", out)
	}
	if strings.Contains(out, `href="/tasks/613"`) {
		t.Errorf("the detail page must not render a card link to itself\n---\n%s", out)
	}
}
