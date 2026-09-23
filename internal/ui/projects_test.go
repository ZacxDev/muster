package ui

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/notes"
)

// --- Project filter row -------------------------------------------------------
//
// The payoff surface for the `project:` tag. It deliberately reuses the EXISTING
// tag-filter mechanism (`data-tag-filter="project:<slug>"`), so these tests pin
// that reuse — if a chip ever stopped carrying that attribute it would silently
// become inert, which is the failure mode a "new parallel filter" would create.

func projectCards() []TaskCardView {
	return []TaskCardView{{Note: notes.Note{ID: 1, Body: "x", Tags: []string{"project:muster"}}}}
}

// TestProjectFilterRowRenders pins the chip markup: bare slug as the label, the
// count beside it, and the FULL `project:<slug>` tag in data-tag-filter (that
// attribute is what the existing delegated click handler reads).
//
// REGRESSION DETECTOR — watched RED at fb6ca657: projectFilterRow did not exist,
// so no #project-filter-row and no data-project-filter appeared at all.
func TestProjectFilterRowRenders(t *testing.T) {
	out := renderTasks(t, TasksView{
		Cards: projectCards(),
		Projects: []notes.ProjectCount{
			{Name: "muster", Count: 12},
			{Name: "remix", Count: 3},
		},
	})

	if !strings.Contains(out, `id="project-filter-row"`) {
		t.Fatalf("missing the project filter row; body=%s", out)
	}
	for _, want := range []string{
		`data-tag-filter="project:muster"`,
		`data-tag-filter="project:remix"`,
		`data-project-filter="muster"`,
		`data-project-filter="remix"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("project row must contain %s; body=%s", want, out)
		}
	}
	// The chip LABELS with the bare slug — `project:` is a storage detail.
	if !strings.Contains(out, `>muster<`) {
		t.Fatalf("chip must label with the bare slug; body=%s", out)
	}
	// The count is shown so the row conveys where the work actually is.
	if !strings.Contains(out, `>12<`) {
		t.Fatalf("chip must show its count; body=%s", out)
	}
}

// TestProjectFilterRowHiddenWhenUnused: a queue that has never used a project
// renders NO project toolbar — the same discipline as the tag filter row.
//
// INVARIANT GUARD.
func TestProjectFilterRowHiddenWhenUnused(t *testing.T) {
	out := renderTasks(t, TasksView{
		Cards:      []TaskCardView{{Note: notes.Note{ID: 1, Body: "x"}}},
		Vocabulary: []notes.TagCount{{Tag: "bug", Count: 1}},
	})
	if strings.Contains(out, "project-filter-row") {
		t.Fatalf("no projects in use must render no project row; body=%s", out)
	}
}

// TestProjectFilterRowMarksActive pins that the active project is marked
// aria-pressed="true" and that inactive ones are "false".
//
// REGRESSION DETECTOR — watched RED at fb6ca657 (no row existed).
func TestProjectFilterRowMarksActive(t *testing.T) {
	out := renderTasks(t, TasksView{
		Cards:      projectCards(),
		Projects:   []notes.ProjectCount{{Name: "muster", Count: 2}, {Name: "remix", Count: 1}},
		ActiveTags: []string{"project:muster"},
	})
	if !strings.Contains(out, `data-active-project="muster"`) {
		t.Fatalf("row must record the active project; body=%s", out)
	}
	// The active chip is pressed…
	clawIdx := strings.Index(out, `data-project-filter="muster"`)
	remixIdx := strings.Index(out, `data-project-filter="remix"`)
	if clawIdx < 0 || remixIdx < 0 {
		t.Fatalf("both chips must render; body=%s", out)
	}
	if !strings.Contains(out[clawIdx:remixIdx], `aria-pressed="true"`) {
		t.Fatalf("the active project chip must be aria-pressed=true; body=%s", out)
	}
	if !strings.Contains(out[remixIdx:], `aria-pressed="false"`) {
		t.Fatalf("an inactive project chip must be aria-pressed=false; body=%s", out)
	}
}

// TestProjectFilterRowShowsActiveProjectMissingFromVocabulary: filtering by a
// project whose last task was just dismissed must STILL render its chip, or the
// user has no way to unselect it and the queue looks permanently empty.
//
// REGRESSION DETECTOR — watched RED at fb6ca657 (no row existed).
func TestProjectFilterRowShowsActiveProjectMissingFromVocabulary(t *testing.T) {
	out := renderTasks(t, TasksView{
		Cards:      []TaskCardView{},
		Projects:   nil, // vocabulary is empty — the last tagged task is gone
		ActiveTags: []string{"project:ghost"},
	})
	if !strings.Contains(out, `data-project-filter="ghost"`) {
		t.Fatalf("an active-but-unknown project must still render a chip; body=%s", out)
	}
	if !strings.Contains(out, `data-tag-filter="project:ghost"`) {
		t.Fatalf("its chip must still carry the filter tag; body=%s", out)
	}
}

// TestProjectFilterRowRendersInFilteredEmptyState: the control must survive the
// filtered-empty branch. A filter UI that disappears exactly when the filter
// matched nothing strands the user — the same trap the filtered-empty state
// itself exists to avoid.
//
// REGRESSION DETECTOR — watched RED at fb6ca657 (no row existed).
func TestProjectFilterRowRendersInFilteredEmptyState(t *testing.T) {
	out := renderTasks(t, TasksView{
		Cards:      []TaskCardView{}, // nothing matched
		Projects:   []notes.ProjectCount{{Name: "muster", Count: 4}},
		ActiveTags: []string{"project:muster"},
	})
	// EXACT id match: a substring check would be satisfied by e.g.
	// `project-filter-row-DISABLED`, which is how a mutant survived this guard once.
	if !strings.Contains(out, `id="project-filter-row"`) {
		t.Fatalf("the project row must render in the filtered-empty state; body=%s", out)
	}
	if !strings.Contains(out, `data-project-filter="muster"`) {
		t.Fatalf("the filtered-empty project row must still carry its chips; body=%s", out)
	}
	// And the filtered-empty state itself is still distinct from all-clear.
	if strings.Contains(out, "No tasks yet") {
		t.Fatalf("filtered-empty must not render the all-clear copy; body=%s", out)
	}
}

// TestProjectTagsAreNotDuplicatedInTheTagRow: a project has ONE chip, in the
// project row. Rendering it in the general tag row too would give one filter two
// controls that toggle the same state — and clicking them in sequence would look
// broken.
//
// ⚠ HONEST LABEL — this guards a regression the NEW project row INTRODUCES, so
// it PASSES pre-change (at fb6ca657 there was only one row, so the count was
// trivially 1 — verified). It is mutation-proved instead: dropping the
// `project:` skip in tagFilterRow turns it red.
func TestProjectTagsAreNotDuplicatedInTheTagRow(t *testing.T) {
	out := renderTasks(t, TasksView{
		Cards: projectCards(),
		Vocabulary: []notes.TagCount{
			{Tag: "project:muster", Count: 2},
			{Tag: "bug", Count: 1},
		},
		Projects: []notes.ProjectCount{{Name: "muster", Count: 2}},
	})
	if n := strings.Count(out, `data-tag-filter="project:muster"`); n != 1 {
		t.Fatalf("a project must have exactly ONE filter chip, found %d; body=%s", n, out)
	}
	// The general tag row still renders ordinary tags.
	if !strings.Contains(out, `data-tag-filter="bug"`) {
		t.Fatalf("descriptive tags must still render in the tag row; body=%s", out)
	}
}

// TestActiveProjectIsNotDuplicatedInTheTagRow is the same rule for the
// "active tag missing from the vocabulary" path in tagFilterRow, which is a
// SECOND place that appends chips and would otherwise re-introduce the duplicate.
//
// ⚠ HONEST LABEL — same as above: passes pre-change (one row only), and is
// mutation-proved by dropping the `project:` skip in tagFilterRow's
// active-tag-not-in-vocabulary loop, which is a SECOND append site and therefore
// a second chance to reintroduce the duplicate.
func TestActiveProjectIsNotDuplicatedInTheTagRow(t *testing.T) {
	out := renderTasks(t, TasksView{
		Cards:      projectCards(),
		Vocabulary: []notes.TagCount{{Tag: "bug", Count: 1}},
		Projects:   []notes.ProjectCount{{Name: "muster", Count: 1}},
		ActiveTags: []string{"project:muster"},
	})
	if n := strings.Count(out, `data-tag-filter="project:muster"`); n != 1 {
		t.Fatalf("active project must have exactly ONE chip, found %d; body=%s", n, out)
	}
}

// TestProjectChipStylingIsDistinctFromRouting pins that a project tag on a CARD
// renders as its own visual class — reserved, but explicitly NOT a routing chip,
// so it must not carry the routing "this changes behaviour" promise.
//
// REGRESSION DETECTOR — watched RED at fb6ca657: `project:` hit tagChipClass'
// default branch and rendered with the muted descriptive styling and the generic
// "Tag: project:muster" title.
func TestProjectChipStylingIsDistinctFromRouting(t *testing.T) {
	out := renderTagCard(t, notes.Note{ID: 1, Body: "x", Tags: []string{"project:muster"}})

	if !strings.Contains(out, `title="Project: muster"`) {
		t.Fatalf("a project chip must carry its own title; body=%s", out)
	}
	// NOT routing: the kind attribute stays descriptive, which is what keeps the
	// in-progress routing lock from applying to a project.
	if !strings.Contains(out, `data-tag-kind="descriptive"`) {
		t.Fatalf("a project chip must NOT be marked routing; body=%s", out)
	}
	if strings.Contains(out, "Routing: ") {
		t.Fatalf("a project chip must not claim routing behaviour; body=%s", out)
	}
}

// --- Single-select project chips + the shared chip-editor normalizer ---------

// renderTagScript renders tagScript() so the embedded JS rules can be asserted.
func renderTagScript(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	if err := tagScript().Render(&b); err != nil {
		t.Fatalf("render tagScript: %v", err)
	}
	return b.String()
}

// TestProjectChipsDeclareTheExclusiveGroup pins that every project chip carries
// `data-tag-filter-exclusive="project:"`, which is what makes project selection
// SINGLE-select.
//
// Without it the chips reuse the multi-select AND filter for a mutually-exclusive
// concept: clicking two projects yields active=[project:a, project:b], a
// guaranteed-empty list, with only the FIRST rendering aria-pressed="true"
// because projectFilterRow takes the first project tag it finds as "the" active
// one. The behaviour itself is EXECUTED and asserted in
// extension/tests/filter-toggle.test.js.
//
// REGRESSION TEST: RED against f50f0d1e (the attribute did not exist).
func TestProjectChipsDeclareTheExclusiveGroup(t *testing.T) {
	out := renderTasks(t, TasksView{
		Cards: projectCards(),
		Projects: []notes.ProjectCount{
			{Name: "muster", Count: 12},
			{Name: "remix", Count: 3},
		},
	})

	// EXACT attribute value, and EVERY chip must carry it — a `project:` group
	// that only some chips join would silently be multi-select again.
	want := `data-tag-filter-exclusive="` + notes.NSProject + `:"`
	if n := strings.Count(out, want); n != 2 {
		t.Fatalf("found %d occurrences of %s, want one per project chip (2); body=%s", n, want, out)
	}
	// The general tag chips must NOT join it — they are legitimately multi-select.
	out2 := renderTasks(t, TasksView{
		Cards:      []TaskCardView{{Note: notes.Note{ID: 1, Body: "x", Tags: []string{"alpha", "beta"}}}},
		ActiveTags: []string{"alpha"},
	})
	if strings.Contains(out2, "data-tag-filter-exclusive") {
		t.Fatalf("ordinary tag chips must stay multi-select; body=%s", out2)
	}
}

// TestTagScriptUsesTheSharedToggleAndNormalizer pins that the inline handler
// DELEGATES to the two embedded rule files rather than re-spelling them. Both are
// executed against real inputs by the vitest suite, so keeping the delegation is
// what keeps those tests meaningful.
//
// REGRESSION TEST: RED against f50f0d1e (neither file nor call existed; the
// normalizer was inline and the toggle was hand-inlined multi-select).
func TestTagScriptUsesTheSharedToggleAndNormalizer(t *testing.T) {
	out := renderTagScript(t)
	for _, want := range []string{
		// The embedded sources really are spliced in.
		"function toggleFilter(cur, tag, group) {",
		"function normalize(raw) {",
		// ...and the handler CALLS the shared toggle with the group attribute,
		// rather than keeping its own copy of the rule.
		//
		// The trailing `statusOf()` is the STATUS-COMPOSITION half and belongs in
		// this same literal: apply() now takes (tags, status), and a call that
		// passed the toggled tags with no status would silently CLEAR the status
		// chip on every tag click — the exact composition bug the status filter
		// exists to avoid, and one no tag-only assertion could see.
		`apply(toggleFilter(get(), tag, chip.getAttribute('data-tag-filter-exclusive')), statusOf());`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("tagScript is missing %q — the rule has been re-spelled inline again", want)
		}
	}
	// The pre-existing inline normalizer must be GONE, or two copies race again.
	if strings.Contains(out, `String(raw || '').toLowerCase().split(/\s+/)`) {
		t.Fatalf("the old inline chip-editor normalizer is still present; body=%s", out)
	}
}

// TestChipEditorNormalizerUsesTheProjectNamespaceConstant ties the namespace
// literal inside the shared JS to notes.NSProject, so renaming the Go constant
// cannot leave the browser-side rule pointing at a namespace that no longer
// exists — and pins the leading/trailing-dash strip that was missing (a chip
// DISPLAYED as `project:-foo-` used to SAVE as the different tag `project:foo`).
//
// REGRESSION TEST: RED against f50f0d1e (js/tag-normalize.js did not exist).
func TestChipEditorNormalizerUsesTheProjectNamespaceConstant(t *testing.T) {
	if !strings.Contains(tagNormalizeJS, `t.slice(0, i) === '`+notes.NSProject+`'`) {
		t.Fatalf("js/tag-normalize.js does not key its project rule off notes.NSProject (%q):\n%s", notes.NSProject, tagNormalizeJS)
	}
	if !strings.Contains(tagNormalizeJS, `t = '`+notes.NSProject+`:' + t.slice(i + 1).replace(/^-+/, '').replace(/-+$/, '');`) {
		t.Fatalf("js/tag-normalize.js does not strip a project slug's leading/trailing '-':\n%s", tagNormalizeJS)
	}
}
