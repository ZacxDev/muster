package notes

import (
	"strconv"
	"strings"
	"testing"
)

// TestMergeTaskTagsUnionsBothSets is the happy path: the merged set is the
// de-duplicated, sorted UNION of both tag sets.
func TestMergeTaskTagsUnionsBothSets(t *testing.T) {
	got, err := MergeTaskTags([]string{"bug", "frontend"}, []string{"frontend", "api"})
	if err != nil {
		t.Fatalf("MergeTaskTags: %v", err)
	}
	want := []string{"api", "bug", "frontend"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("merged = %v, want %v", got, want)
	}
}

// TestMergeTaskTagsSameProjectIsFine pins that the project guard fires on a
// CONFLICT, not on the mere presence of a project tag — the common case (two
// tasks in the same project) must merge.
func TestMergeTaskTagsSameProjectIsFine(t *testing.T) {
	got, err := MergeTaskTags([]string{"project:orbit", "bug"}, []string{"project:orbit", "api"})
	if err != nil {
		t.Fatalf("same-project merge must succeed, got: %v", err)
	}
	if n, _ := ProjectName(got); n != "orbit" {
		t.Fatalf("merged project = %q, want orbit", n)
	}
}

// TestMergeTaskTagsOneSidedProjectIsFine pins that a task with no project can
// merge with one that has a project (the union carries exactly one) — in BOTH
// directions, so the guard cannot be satisfied by an asymmetric read.
func TestMergeTaskTagsOneSidedProjectIsFine(t *testing.T) {
	for _, tc := range []struct{ w, l []string }{
		{[]string{"project:orbit"}, []string{"bug"}},
		{[]string{"bug"}, []string{"project:orbit"}},
	} {
		got, err := MergeTaskTags(tc.w, tc.l)
		if err != nil {
			t.Fatalf("MergeTaskTags(%v,%v): %v", tc.w, tc.l, err)
		}
		if n, ok := ProjectName(got); !ok || n != "orbit" {
			t.Fatalf("MergeTaskTags(%v,%v) project = %q/%v, want orbit", tc.w, tc.l, n, ok)
		}
	}
}

// TestMergeTaskTagsRefusesConflictingProjects is SHARP EDGE #1.
//
// 🔴 MergeTags unions without validating, so project:a + project:b yields BOTH.
// Nothing downstream errors: ProjectName resolves via TagValue, which returns
// whichever sorts FIRST — so without this guard the merged task silently lands in
// the alphabetically smaller project. The test asserts the refusal AND that both
// project names appear in the message, because "which two" is the only thing that
// makes the error actionable.
func TestMergeTaskTagsRefusesConflictingProjects(t *testing.T) {
	got, err := MergeTaskTags([]string{"project:alpha", "bug"}, []string{"project:beta", "api"})
	if err == nil {
		t.Fatalf("expected a refusal, got merged=%v", got)
	}
	msg := err.Error()
	for _, want := range []string{"different projects", "alpha", "beta"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q must name %q", msg, want)
		}
	}
	// It must NOT be reported as a tag-cap failure — the two guards have to be
	// distinguishable, or a mutation test on one can be killed by the other.
	if strings.Contains(msg, "max ") {
		t.Errorf("project conflict reported as a tag-cap failure: %q", msg)
	}
}

// TestMergeTaskTagsRefusesOverTagCap is SHARP EDGE #2: MaxTags is per-task, so
// two individually-legal tasks can union past it. 11 + 10 distinct tags = 21 > 20.
func TestMergeTaskTagsRefusesOverTagCap(t *testing.T) {
	mk := func(prefix string, n int) []string {
		out := make([]string, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, prefix+strconv.Itoa(i))
		}
		return out
	}
	winner := mk("w", 11)
	loser := mk("l", 10)
	if len(winner) > MaxTags || len(loser) > MaxTags {
		t.Fatalf("fixture invalid: each side must be individually legal (%d/%d, max %d)", len(winner), len(loser), MaxTags)
	}
	got, err := MergeTaskTags(winner, loser)
	if err == nil {
		t.Fatalf("expected a refusal at %d merged tags, got merged=%v", len(got), got)
	}
	msg := err.Error()
	if !strings.Contains(msg, "21") || !strings.Contains(msg, strconv.Itoa(MaxTags)) {
		t.Errorf("error %q must name the merged count (21) and the cap (%d)", msg, MaxTags)
	}
	// 🔴 Assert THIS guard's own wording, not merely "an error happened". The
	// ValidateTags backstop also rejects 21 tags — with "too many tags: 21
	// (max 20)", which contains both numbers. Without this assertion, deleting the
	// cap guard entirely would leave this test GREEN, killed by a different guard.
	for _, want := range []string{"would carry", "remove some tags first"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q must be the MERGE cap message (missing %q), not the generic ValidateTags one", msg, want)
		}
	}
	if strings.Contains(msg, "different projects") {
		t.Errorf("tag-cap failure reported as a project conflict: %q", msg)
	}
}

// TestMergeTaskTagsAtCapExactlyIsAllowed is the boundary on the OTHER side of the
// cap guard: exactly MaxTags must merge. A `>=` mutation would pass the
// over-cap test above and only die here.
func TestMergeTaskTagsAtCapExactlyIsAllowed(t *testing.T) {
	var winner, loser []string
	for i := 0; i < MaxTags; i++ {
		if i%2 == 0 {
			winner = append(winner, "t"+strconv.Itoa(i))
		} else {
			loser = append(loser, "t"+strconv.Itoa(i))
		}
	}
	got, err := MergeTaskTags(winner, loser)
	if err != nil {
		t.Fatalf("a merge to exactly %d tags must succeed, got: %v", MaxTags, err)
	}
	if len(got) != MaxTags {
		t.Fatalf("merged %d tags, want exactly %d", len(got), MaxTags)
	}
}

// TestMergeTaskTagsDedupesBelowTheCap pins that the cap is applied to the
// DE-DUPLICATED union, not to the concatenation: two 15-tag tasks that share 12
// tags merge to 18 and must be allowed.
func TestMergeTaskTagsDedupesBelowTheCap(t *testing.T) {
	var winner, loser []string
	for i := 0; i < 15; i++ {
		winner = append(winner, "t"+strconv.Itoa(i))
	}
	for i := 3; i < 18; i++ {
		loser = append(loser, "t"+strconv.Itoa(i))
	}
	got, err := MergeTaskTags(winner, loser)
	if err != nil {
		t.Fatalf("18 distinct tags must merge, got: %v", err)
	}
	if len(got) != 18 {
		t.Fatalf("merged %d tags, want 18", len(got))
	}
}

// TestMergeTaskTagsValidatesTheMergedSet pins the BACKSTOP: a grammar violation
// present on either side is reported against the merged set rather than being
// exempt from merges. It also asserts the "cannot merge:" prefix, so the message
// reads as a merge refusal and not as a stray tag error from somewhere else.
func TestMergeTaskTagsValidatesTheMergedSet(t *testing.T) {
	_, err := MergeTaskTags([]string{"ok"}, []string{"BAD TAG!"})
	if err == nil {
		t.Fatal("expected the merged set to be validated")
	}
	if !strings.HasPrefix(err.Error(), "cannot merge:") {
		t.Errorf("error %q must be prefixed as a merge refusal", err.Error())
	}
}

// TestMergeTaskTagsEmptyInputs pins the degenerate case: two untagged tasks merge
// to an empty (non-nil) set rather than erroring.
func TestMergeTaskTagsEmptyInputs(t *testing.T) {
	got, err := MergeTaskTags(nil, nil)
	if err != nil {
		t.Fatalf("MergeTaskTags(nil,nil): %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("merged = %v, want an empty non-nil slice", got)
	}
}
