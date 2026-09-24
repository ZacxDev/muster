package ui

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/notes"
)

func mergeCards(n int) []TaskCardView {
	out := make([]TaskCardView, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, TaskCardView{Note: notes.Note{ID: int64(i), Title: "Task " + string(rune('A'+i-1)), Body: "b", Status: notes.StatusOpen}})
	}
	return out
}

func renderMergeScript(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	if err := taskMergeScript().Render(&b); err != nil {
		t.Fatalf("render: %v", err)
	}
	return b.String()
}

// TestMergeAffordanceHiddenBelowTwoTasks: merging needs two tasks, so offering
// "Select" over a one-task queue is a dead end the user only finds by tapping.
// The boundary is measured at BOTH sides (1 → absent, 2 → present) so an
// off-by-one in minMergeableTasks cannot pass.
func TestMergeAffordanceHiddenBelowTwoTasks(t *testing.T) {
	for _, tc := range []struct {
		cards int
		want  bool
	}{{0, false}, {1, false}, {2, true}, {3, true}} {
		out := renderTasks(t, TasksView{Cards: mergeCards(tc.cards)})
		got := strings.Contains(out, "data-select-toggle") && strings.Contains(out, "data-merge-bar")
		if got != tc.want {
			t.Errorf("with %d cards: merge affordance present = %v, want %v", tc.cards, got, tc.want)
		}
	}
}

// TestMergeBarPostsToMergeRouteWithoutSwapping pins ONE half of the wiring:
// hx-post to /tasks/merge with hx-swap="none", so the refresh comes from the
// server's HX-Trigger rather than from a body this route would have to render
// blind.
//
// 🔴 SCOPE — this docstring used to claim it "pins the wiring that keeps the
// active tag filter alive". It does not, and it never did. Filter survival takes
// tagScript's htmx:configRequest listener as well, which this test does not
// touch; htmx 2.0.4 captures the verb path at process time, so the hx-get
// attribute alone moves nothing. Reading as coverage while providing none is
// worse than no test, because it stops anyone looking — the listener is pinned
// by TestBoardRequestPathIsRewrittenFromTheContainersHxGet and, behaviourally,
// by e2e/tests/task-board-paging.spec.ts.
func TestMergeBarPostsToMergeRouteWithoutSwapping(t *testing.T) {
	out := renderTasks(t, TasksView{Cards: mergeCards(2)})
	// hx-disabled-elt's VALUE is pinned structurally by
	// TestMergeDisabledEltCoversEveryWinnerButton (it must cover BOTH buttons);
	// here we only require that the guard is present at all.
	for _, want := range []string{`hx-post="/tasks/merge"`, `hx-swap="none"`, `hx-disabled-elt=`} {
		if !strings.Contains(out, want) {
			t.Errorf("merge bar must carry %s", want)
		}
	}
	for _, want := range []string{`name="winner"`, `name="loser"`, `data-merge-keep="first"`, `data-merge-keep="second"`} {
		if !strings.Contains(out, want) {
			t.Errorf("merge form must carry %s", want)
		}
	}
}

// TestMergeControlsStartHidden: the checkboxes and the bar are always in the DOM
// (so toggling mode is pure class work, no refetch) but must not be VISIBLE until
// selection mode is on — otherwise every card grows a checkbox permanently.
func TestMergeControlsStartHidden(t *testing.T) {
	out := renderTasks(t, TasksView{Cards: mergeCards(2)})
	bar := regexp.MustCompile(`<div id="task-merge-bar"[^>]*class="([^"]*)"`).FindStringSubmatch(out)
	if bar == nil {
		t.Fatal("merge bar not found")
	}
	if !strings.Contains(bar[1], "hidden") {
		t.Errorf("merge bar class %q must start hidden", bar[1])
	}
	sel := regexp.MustCompile(`<label data-task-select="1"[^>]*class="([^"]*)"`).FindStringSubmatch(out)
	if sel == nil {
		t.Fatal("card select control not found")
	}
	if !strings.Contains(sel[1], "hidden") {
		t.Errorf("card select control class %q must start hidden", sel[1])
	}
	// aria-pressed is the toggle's STATE, read by the script and by assistive tech.
	if !strings.Contains(out, `aria-pressed="false"`) {
		t.Error("the Select toggle must start aria-pressed=false")
	}
}

// TestMergeControlsCarryFocusVisibleStyling. Keyboard selection is net-new in
// this app (no existing button has focus-visible styling), and this flow requires
// moving between several controls before committing — without a visible focus
// ring a keyboard user cannot tell which task they are about to check.
//
// 🔴 Asserted STRUCTURALLY, per control: it counts the controls added here and
// requires EVERY one to carry the ring. A bare "the output contains
// focus-visible" check would stay green if a later control shipped without it.
func TestMergeControlsCarryFocusVisibleStyling(t *testing.T) {
	out := renderTasks(t, TasksView{Cards: mergeCards(2)})
	// The controls a keyboard user tabs through: the toggle, both winner buttons,
	// and one checkbox per card.
	wants := []string{
		`data-select-toggle`,
		`data-merge-keep="first"`,
		`data-merge-keep="second"`,
		`data-task-select-box="1"`,
		`data-task-select-box="2"`,
	}
	tagRE := regexp.MustCompile(`<(?:button|input|label)[^>]*>`)
	for _, marker := range wants {
		found := false
		for _, tag := range tagRE.FindAllString(out, -1) {
			if !strings.Contains(tag, marker) {
				continue
			}
			found = true
			if !strings.Contains(tag, "focus-visible:ring-2") {
				t.Errorf("control %s has no focus-visible ring: %s", marker, tag)
			}
		}
		if !found {
			t.Errorf("control %s not rendered", marker)
		}
	}
}

// TestMergeCheckboxIsLabelledPerTask: an unlabelled checkbox in a row of
// identical checkboxes is unusable with a screen reader, and the id is the only
// thing distinguishing them.
func TestMergeCheckboxIsLabelledPerTask(t *testing.T) {
	out := renderTasks(t, TasksView{Cards: mergeCards(2)})
	for _, want := range []string{`aria-label="Select task #1 for merge"`, `aria-label="Select task #2 for merge"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
}

// TestMergeSelectControlCarriesLabel: the bar names the two candidates from
// data-task-select-label — the task's TITLE, falling back to its id.
//
// 🔴 The directory case is the point of this test, and it is a REGRESSION guard,
// not a preference. The first version of this control used ui.TaskTitle, whose
// fallback is the DIRECTORY — which put the raw path back onto every
// directory-only card, undoing the 0.7.x declutter that deliberately removed the
// directory chip. TestTaskTitlePrefersTitleOverDirectory caught it. Pinned here
// too, at the control that caused it, so the next person reaching for TaskTitle
// gets a failure that explains itself.
func TestMergeSelectControlCarriesLabel(t *testing.T) {
	titled := renderTagCard(t, notes.Note{ID: 5, Title: "Fix the thing", Body: "b"})
	if !strings.Contains(titled, `data-task-select-label="Fix the thing"`) {
		t.Errorf("a titled task must expose its title as the merge label; got %s", titled)
	}
	bare := renderTagCard(t, notes.Note{ID: 6, Body: "b"})
	if !strings.Contains(bare, `data-task-select-label="#6"`) {
		t.Errorf("a label-less task must fall back to its id; got %s", bare)
	}
	dir := renderTagCard(t, notes.Note{ID: 7, Directory: "/srv/app", Body: "b"})
	if !strings.Contains(dir, `data-task-select-label="#7"`) {
		t.Errorf("a directory-only task must fall back to its id, not leak the path; got %s", dir)
	}
	if strings.Contains(dir, "/srv/app") {
		t.Errorf("the merge label must not put the directory back on the card; got %s", dir)
	}
}

// TestMergeScriptBindsOnce. htmx re-executes in-body <script> tags on every
// hx-boost body swap while document-level listeners survive it, so an unguarded
// delegated handler stacks up and one click fires N times — exactly the bug that
// made a tag filter chip toggle twice (net zero) in 0.7.75.
func TestMergeScriptBindsOnce(t *testing.T) {
	js := renderMergeScript(t)
	if !strings.Contains(js, "window.__cgMergeInit") {
		t.Fatal("taskMergeScript must carry a first-run guard (window.__cgMergeInit)")
	}
	if !strings.Contains(js, "if (window.__cgMergeInit) return;") {
		t.Error("the guard must return BEFORE binding the delegated listeners")
	}
}

// TestMergeScriptRequiresExactlyTwo pins the script's own arity rule: the winner
// buttons are only revealed at exactly two selections, and a submit attempted at
// any other count is prevented client-side. The server enforces this too (the
// ids simply would not both be present) — this is the affordance, not the gate.
func TestMergeScriptRequiresExactlyTwo(t *testing.T) {
	js := renderMergeScript(t)
	if !strings.Contains(js, "sel.length !== 2") {
		t.Error("the script must branch on an exact selection count of two")
	}
	if !strings.Contains(js, "Select two tasks to merge.") {
		t.Error("the script must state the requirement in the bar")
	}
}

// TestMergeBarExplainsSupersede. "Merge" reads as destructive — users assume one
// task disappears. It does not: the loser is retired with a pointer, and nothing
// is deleted. Saying so in the bar is the difference between a control people use
// and one they avoid.
func TestMergeBarExplainsSupersede(t *testing.T) {
	out := renderTasks(t, TasksView{Cards: mergeCards(2)})
	if !strings.Contains(out, "nothing is deleted") {
		t.Error("the merge bar must state that nothing is deleted")
	}
	if !strings.Contains(out, "marked complete") {
		t.Error("the merge bar must state what happens to the other task")
	}
}

// TestMergeAffordanceDoesNotDisturbTheEmptyStates: the two empty states are
// deliberately different (filtered-empty ≠ all-clear) and neither should grow a
// merge control.
func TestMergeAffordanceDoesNotDisturbTheEmptyStates(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    TasksView
	}{
		{"all clear", TasksView{}},
		{"filtered empty", TasksView{ActiveTags: []string{"bug"}}},
	} {
		out := renderTasks(t, tc.v)
		if strings.Contains(out, "data-select-toggle") || strings.Contains(out, "data-merge-bar") {
			t.Errorf("%s state must not render a merge control; got %s", tc.name, out)
		}
	}
}

// TestDeleteStaysLastInTheActionRow. The merge checkbox is inserted into the
// card's action row, and DELETE's LAST position there is deliberate (it used to
// be first — the accidental-tap slot — while tearing down running agent pods).
// This pins that adding the checkbox did not disturb that ordering.
func TestDeleteStaysLastInTheActionRow(t *testing.T) {
	out := renderTagCard(t, notes.Note{ID: 3, Body: "b", Status: notes.StatusOpen})
	sel := strings.Index(out, "data-task-select=")
	edit := strings.Index(out, "data-edit-trigger")
	del := strings.Index(out, "data-delete-control")
	if sel < 0 || edit < 0 || del < 0 {
		t.Fatalf("expected select/edit/delete controls; got %s", out)
	}
	if !(sel < edit && edit < del) {
		t.Errorf("action-row order must be select < edit < delete, got %d/%d/%d", sel, edit, del)
	}
}

// TestMergeDisabledEltCoversEveryWinnerButton is the client half of the
// double-tap guard, asserted STRUCTURALLY rather than by spelling.
//
// 🔴 htmx's `find <sel>` is querySelector — the FIRST match only. Verified in the
// vendored bundle (web/static/vendor/htmx.min.js): the selector resolver has
// `r.indexOf("find ")===0 → e=u(f(t), …)` and `function u(e,t){… return
// e.querySelector(t)}`, while a BARE selector falls through to
// `c.querySelectorAll(e)`. This form renders TWO submit buttons, so
// hx-disabled-elt="find button" disabled exactly one of them — and tapping the
// SECOND button disabled the FIRST, leaving the tapped button live for a second
// tap.
//
// So this asserts the relationship, not a word: the configured selector must be a
// bare attribute selector, and EVERY submit button in the form must carry that
// attribute. It fails if a third button is added without it, and it fails for any
// single-match htmx modifier prefix.
func TestMergeDisabledEltCoversEveryWinnerButton(t *testing.T) {
	out := renderTasks(t, TasksView{Cards: mergeCards(2)})
	m := regexp.MustCompile(`hx-disabled-elt="([^"]*)"`).FindStringSubmatch(out)
	if m == nil {
		t.Fatal("the merge form must carry hx-disabled-elt (the double-tap guard)")
	}
	sel := m[1]
	// htmx's single-match / relative modifiers can never cover two siblings.
	for _, bad := range []string{"find ", "closest ", "next ", "previous ", "this"} {
		if strings.Contains(sel, bad) {
			t.Fatalf("hx-disabled-elt=%q uses htmx's %q form, which resolves to at most ONE element "+
				"(querySelector / a single relative hop) — it cannot disable both winner buttons", sel, bad)
		}
	}
	attr := regexp.MustCompile(`^\[([a-z-]+)\]$`).FindStringSubmatch(sel)
	if attr == nil {
		t.Fatalf("hx-disabled-elt=%q must be a bare [attribute] selector so htmx resolves it with "+
			"querySelectorAll (all matches), not querySelector", sel)
	}
	// Every submit button inside the merge form must carry that attribute.
	form := out[strings.Index(out, `hx-post="/tasks/merge"`):]
	form = form[:strings.Index(form, "</form>")]
	buttons := strings.Count(form, `type="submit"`)
	covered := strings.Count(form, attr[1]+`=`)
	if buttons < 2 {
		t.Fatalf("expected the merge form to render both winner buttons, found %d", buttons)
	}
	if covered != buttons {
		t.Errorf("hx-disabled-elt=%q covers %d of the form's %d submit buttons — a tap on an "+
			"uncovered button is unguarded against a plain double-tap", sel, covered, buttons)
	}
}

// TestMergeAffordanceIgnoresCompletedTasks. A completed task can be neither side
// of a merge (the server refuses both), so the UI must not offer it:
//
//   - a Done card renders NO select control, and
//   - the toolbar/bar gate on the count of MERGEABLE cards, not len(Cards).
//
// Both halves matter. Suppressing only the checkbox would leave a queue of two
// Done cards showing a "Select" toggle over nothing selectable — a dead end; and
// gating only the toolbar would still let a Done card be the second pick in a
// mixed queue.
func TestMergeAffordanceIgnoresCompletedTasks(t *testing.T) {
	mixed := []TaskCardView{
		{Note: notes.Note{ID: 1, Title: "Open one", Body: "b", Status: notes.StatusOpen}},
		{Note: notes.Note{ID: 2, Title: "Done one", Body: "b", Status: notes.StatusComplete}},
	}
	out := renderTasks(t, TasksView{Cards: mixed})
	// Both cards render…
	if !strings.Contains(out, `id="task-2"`) {
		t.Fatal("the completed card must still render — merge affordances are what's suppressed, not the task")
	}
	// …but only the open one is selectable.
	if strings.Contains(out, `data-task-select="2"`) {
		t.Error("a COMPLETE task must not render a merge select control — it can be neither winner nor loser")
	}
	if !strings.Contains(out, `data-task-select="1"`) {
		t.Error("an OPEN task must still render its merge select control")
	}
	// One mergeable card out of two total → no toolbar, no bar. Gating on
	// len(Cards) would render both here.
	if strings.Contains(out, "data-select-toggle") || strings.Contains(out, "data-merge-bar") {
		t.Error("with only ONE mergeable card the merge toolbar/bar must not render (they gate on the " +
			"mergeable count, not on len(Cards) which includes Done)")
	}

	// Boundary on the other side: two OPEN cards plus a Done one still offers it.
	out = renderTasks(t, TasksView{Cards: append([]TaskCardView{
		{Note: notes.Note{ID: 3, Title: "Open two", Body: "b", Status: notes.StatusOpen}},
	}, mixed...)})
	if !strings.Contains(out, "data-select-toggle") || !strings.Contains(out, "data-merge-bar") {
		t.Error("two mergeable cards must still offer the merge affordance even alongside a Done card")
	}
	if strings.Contains(out, `data-task-select="2"`) {
		t.Error("the Done card must stay unselectable in a mixed queue")
	}
}

// TestSelectionModeSuppressesTheCardNavigationOverlay is a THREE-SIDED SEAM
// guard, and none of the three sides can see the break alone.
//
// 🔴 THE HAZARD. A board card's whole surface is a stretched link to
// /tasks/{id} — an <a class="card-link"> whose ::after covers the <article>.
// While merge-selection mode is on, a card tap must NOT navigate away mid-merge.
// taskMergeScript's delegated click listener only preventDefault()s
// [data-select-toggle] and [data-merge-keep]; it never sees the anchor, so it
// cannot shield it. The suppression is therefore CSS:
//
//	taskMergeScript      writes `selecting` onto <html>
//	web/css/input.css    `.selecting .card-link::after { pointer-events: none }`
//	noteCard             emits `class="card-link …"`
//
// Rename the class on ANY ONE side and nothing errors: the selector simply
// never matches, the overlay keeps its pointer-events, and every card tap
// during a merge navigates away. Silent, and invisible to a rendered-HTML
// snapshot of any single component.
//
// ⚠ WHAT SUPPRESSION MEANS. The card tap becomes INERT; it does not tick the
// card's checkbox. The checkbox is inside the action row, which is already
// `relative z-10` (above the overlay), and nothing ticks it from a card-body
// click. Three comments — here, taskMergeScript's and input.css's — used to say
// the click "reaches the checkbox underneath"; there is no checkbox underneath.
//
// The class name is EXTRACTED from the JS rather than written down here, so
// this test cannot drift into pinning a word none of the three sides uses.
//
// ⚠ SCOPE. This pins that the three NAMES agree and that the SOURCE rule says
// what it claims. It does NOT prove the COMPILED, gitignored web/static/app.css
// carries the rule — nothing here compiles or applies CSS. That half is
// behavioural and belongs to the browser tier: e2e/tests/tasks-mobile.spec.ts
// reads getComputedStyle(card-link, "::after").pointerEvents with selection
// mode on and off, which is a claim about the bytes actually served.
func TestSelectionModeSuppressesTheCardNavigationOverlay(t *testing.T) {
	js := renderMergeScript(t)

	// 1. The JS side: setMode toggles a class on the DOCUMENT ELEMENT.
	//
	// documentElement and not body, deliberately: an hx-boost swap replaces the
	// body, which would silently drop the class mid-selection.
	m := regexp.MustCompile(`document\.documentElement\.classList\.toggle\('([a-zA-Z0-9_-]+)'`).FindStringSubmatch(js)
	if m == nil {
		t.Fatalf("taskMergeScript no longer marks selection mode on <html>; with no marker the CSS "+
			"suppression below can never match and a card tap NAVIGATES mid-merge\n---\n%s", js)
	}
	cls := m[1]

	// 2. The CSS side: the rule exists, is scoped to that class, targets the
	//    card link's ::after, and actually disables pointer events.
	css, err := os.ReadFile("../../web/css/input.css")
	if err != nil {
		t.Fatalf("read the Tailwind entrypoint: %v", err)
	}
	rule := regexp.MustCompile(`\.` + regexp.QuoteMeta(cls) + `\s+\.card-link::after\s*\{[^}]*pointer-events:\s*none[^}]*\}`)
	if !rule.Match(css) {
		t.Errorf("web/css/input.css has no `.%s .card-link::after { pointer-events: none }` rule. "+
			"taskMergeScript writes `%s` onto <html> expecting exactly that selector to fire; without "+
			"it the stretched card link stays live and a tap during merge selection navigates to the "+
			"task page instead of ticking its checkbox.", cls, cls)
	}

	// 3. The markup side: a board card really does carry `card-link`.
	var b bytes.Buffer
	if err := RenderNoteCard(&b, TaskCardView{Note: notes.Note{ID: 9, Title: "selectable", Body: "b", Status: notes.StatusOpen}}); err != nil {
		t.Fatalf("render card: %v", err)
	}
	card := b.String()
	link := regexp.MustCompile(`<a [^>]*data-task-link="9"[^>]*>`).FindString(card)
	if link == "" {
		t.Fatalf("the board card renders no navigation link\n---\n%s", card)
	}
	if !regexp.MustCompile(`class="[^"]*\bcard-link\b`).MatchString(link) {
		t.Errorf("the card link does not carry `card-link`, so the CSS above selects nothing:\n%s", link)
	}

	// 4. And the DETAIL shape must NOT — it renders no link at all, so a rule
	//    matching there would be selecting a phantom.
	var d bytes.Buffer
	if err := RenderNoteCard(&d, TaskCardView{Note: notes.Note{ID: 9, Title: "selectable", Body: "b", Status: notes.StatusOpen}, Detail: true}); err != nil {
		t.Fatalf("render detail card: %v", err)
	}
	if strings.Contains(d.String(), "card-link") {
		t.Errorf("the DETAIL card carries `card-link` — it is the destination, not a link to it")
	}
}
