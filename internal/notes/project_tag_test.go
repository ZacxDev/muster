package notes

import (
	"strings"
	"testing"
)

// --- `project:` reserved namespace ------------------------------------------
//
// These pin the THREE properties the project tag adds on top of the existing tag
// grammar, all of which live in the ONE normalize/validate path every write path
// already shares (NormalizeTag → NormalizeTags → ValidateTags):
//
//  1. slug normalization strips leading/trailing '-' (so `Foo Bar`, `foo-bar` and
//     `FOO   BAR` converge on ONE slug — without this the project list fragments
//     into near-duplicates and the picker degrades into noise),
//  2. the slug charset is NARROWER than the general tag charset ('/' is legal in a
//     descriptive tag but not in a project slug),
//  3. AT MOST ONE `project:` tag per task, rejected with an error naming BOTH
//     values.
//
// Expected values below are LITERAL on purpose — never derived from the
// implementation under test.

// TestNormalizeProjectSlug pins that project slugs converge. Every input row is a
// form a human would plausibly type into the extension's project combobox, and
// they must all land on the SAME canonical tag.
//
// REGRESSION DETECTOR — watched RED at fb6ca657 (pre-feature): the leading/
// trailing-'-' strip did not exist, so `project:-foo-` normalized to itself.
func TestNormalizeProjectSlug(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"already canonical", "project:foo-bar", "project:foo-bar"},
		{"spaces become a dash", "project:Foo Bar", "project:foo-bar"},
		{"whitespace runs collapse to ONE dash", "project:FOO   BAR", "project:foo-bar"},
		{"tabs and newlines collapse too", "project:Foo\t\nBar", "project:foo-bar"},
		{"outer whitespace is trimmed", "  project:Foo  ", "project:foo"},
		{"namespace itself is case-folded", "Project:Foo", "project:foo"},
		{"leading dash is stripped", "project:-foo", "project:foo"},
		{"trailing dash is stripped", "project:foo-", "project:foo"},
		{"both ends stripped", "project:-foo-", "project:foo"},
		{"repeated end dashes stripped", "project:---foo---", "project:foo"},
		{"internal dashes are PRESERVED (only the ends are stripped)", "project:foo--bar", "project:foo--bar"},
		{"a dash-only slug collapses to an empty value (ValidateTags then rejects)", "project:---", "project:"},
		{"dots and underscores survive", "project:foo.bar_baz-1", "project:foo.bar_baz-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeTag(tc.in); got != tc.want {
				t.Fatalf("NormalizeTag(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizeProjectSlugDoesNotTouchOtherTags is the other side of the
// trade-off: the '-'-stripping is PROJECT-ONLY. A descriptive tag (or any other
// reserved namespace) keeps today's exact behaviour, because "existing producers
// see zero contract change" is a hard requirement of this feature.
//
// INVARIANT GUARD — passes either way by design; it stops the project
// normalization from being widened into a global rule.
func TestNormalizeProjectSlugDoesNotTouchOtherTags(t *testing.T) {
	cases := []struct{ in, want string }{
		{"-foo-", "-foo-"},
		{"initiative:-foo-", "initiative:-foo-"},
		{"runbook:-deploy-", "runbook:-deploy-"},
		{"gate:-blocked-", "gate:-blocked-"},
		{"auto:-dispatch-", "auto:-dispatch-"},
		{"civitai:-frontend-", "civitai:-frontend-"},
		{"a/b-c", "a/b-c"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := NormalizeTag(tc.in); got != tc.want {
				t.Fatalf("NormalizeTag(%q) = %q, want %q — the '-' strip must be project-only", tc.in, got, tc.want)
			}
		})
	}
}

// TestProjectSlugCharset pins that the project slug charset is NARROWER than the
// general tag charset: it must match [a-z0-9][a-z0-9._-]* — so it may not contain
// '/' (legal in a descriptive tag) and may not START with '.', '_' or '-'.
//
// REGRESSION DETECTOR — watched RED at fb6ca657: `project:a/b` and `project:_foo`
// both passed ValidateTags, because only the general charset applied.
func TestProjectSlugCharset(t *testing.T) {
	t.Run("legal slugs pass", func(t *testing.T) {
		for _, tag := range []string{
			"project:orbit",
			"project:remix",
			"project:foo-bar",
			"project:foo.bar",
			"project:foo_bar",
			"project:a",
			"project:0",
			"project:9lives",
			"project:acme-infra.v2_x",
		} {
			if err := ValidateTags([]string{tag}); err != nil {
				t.Fatalf("ValidateTags(%q) = %v, want nil", tag, err)
			}
		}
	})

	bad := []struct {
		name string
		tag  string
		// mustMention: the error has to name the offending tag so the producer
		// knows exactly what to fix.
		mustMention string
	}{
		{"slash is not allowed in a project slug", "project:a/b", "project:a/b"},
		{"cannot start with an underscore", "project:_foo", "project:_foo"},
		{"cannot start with a dot", "project:.foo", "project:.foo"},
		{"empty slug", "project:", "empty namespace or value"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTags([]string{tc.tag})
			if err == nil {
				t.Fatalf("ValidateTags(%q) = nil, want an error", tc.tag)
			}
			if !strings.Contains(err.Error(), tc.mustMention) {
				t.Fatalf("error %q must mention %q", err.Error(), tc.mustMention)
			}
		})
	}
}

// TestProjectSlugCharsetStillAllowsDescriptiveSlash is the paired invariant: the
// narrower charset applies ONLY to `project:`. A descriptive `a/b` tag and an
// `initiative:a/b` are still legal.
//
// INVARIANT GUARD — pins that the narrowing did not leak into the general rule.
func TestProjectSlugCharsetStillAllowsDescriptiveSlash(t *testing.T) {
	for _, tag := range []string{"a/b", "initiative:a/b", "runbook:deploy/prod", "civitai:_frontend"} {
		if err := ValidateTags([]string{tag}); err != nil {
			t.Fatalf("ValidateTags(%q) = %v, want nil — only project: is narrowed", tag, err)
		}
	}
}

// TestAtMostOneProjectTag pins the single-project rule and, critically, that the
// error NAMES BOTH values — "which project is this task in" must never be
// ambiguous, and a producer that sent two needs to see which two.
//
// REGRESSION DETECTOR — watched RED at fb6ca657: two project tags validated fine.
func TestAtMostOneProjectTag(t *testing.T) {
	t.Run("one project tag is fine", func(t *testing.T) {
		if err := ValidateTags([]string{"bug", "project:orbit", "runbook:deploy"}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("two project tags are rejected, naming both", func(t *testing.T) {
		// NormalizeTags sorts, so the pair arrives as (orbit, remix).
		tags := NormalizeTags([]string{"project:remix", "project:orbit", "bug"})
		err := ValidateTags(tags)
		if err == nil {
			t.Fatalf("ValidateTags(%q) = nil, want an error", tags)
		}
		msg := err.Error()
		for _, want := range []string{"project:orbit", "project:remix"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("error %q must name %q — the whole point is to say WHICH two conflict", msg, want)
			}
		}
	})

	t.Run("three project tags still name the first two deterministically", func(t *testing.T) {
		tags := NormalizeTags([]string{"project:c", "project:a", "project:b"})
		err := ValidateTags(tags)
		if err == nil {
			t.Fatal("want an error for three project tags")
		}
		if !strings.Contains(err.Error(), "project:a") || !strings.Contains(err.Error(), "project:b") {
			t.Fatalf("error %q must name the first two sorted values (project:a, project:b)", err.Error())
		}
	})

	t.Run("duplicate project tags dedupe to one and pass", func(t *testing.T) {
		tags := NormalizeTags([]string{"project:Orbit", "project:orbit", "  project:ORBIT  "})
		if len(tags) != 1 {
			t.Fatalf("NormalizeTags = %q, want exactly 1 tag after dedupe", tags)
		}
		if err := ValidateTags(tags); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("the error message stays bounded", func(t *testing.T) {
		// 40-rune slugs keep each tag at 48 runes — comfortably under MaxTagRunes,
		// so this exercises the DUPLICATE-project error rather than tripping the
		// length error first.
		tags := []string{"project:" + strings.Repeat("a", 40), "project:" + strings.Repeat("b", 40)}
		err := ValidateTags(tags)
		if err == nil {
			t.Fatal("want an error")
		}
		if !strings.Contains(err.Error(), "at most one") {
			t.Fatalf("error %q should be the duplicate-project error, not the length error", err.Error())
		}
		if n := len([]rune(err.Error())); n > 400 {
			t.Fatalf("error message is %d runes; it must stay bounded", n)
		}
	})
}

// TestProjectName covers the accessor the API/UI layers read.
func TestProjectName(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		name, ok := ProjectName([]string{"bug", "project:orbit", "runbook:deploy"})
		if !ok || name != "orbit" {
			t.Fatalf("ProjectName = (%q,%v), want (orbit,true)", name, ok)
		}
	})
	t.Run("absent", func(t *testing.T) {
		name, ok := ProjectName([]string{"bug", "runbook:deploy"})
		if ok || name != "" {
			t.Fatalf("ProjectName = (%q,%v), want (\"\",false)", name, ok)
		}
	})
	t.Run("no tags at all", func(t *testing.T) {
		if _, ok := ProjectName(nil); ok {
			t.Fatal("ProjectName(nil) must report false")
		}
	})
}

// TestProjectsFromVocabulary pins the ORDERING contract of the project list —
// count DESC, then slug ASC — including the TIE-BREAK, which is the part a
// picker's stability actually depends on. Expected values are literal.
//
// INVARIANT GUARD — the function is new, so there is no prior behaviour for it to
// regress from; it pins the contract the extension and the UI filter both read.
func TestProjectsFromVocabulary(t *testing.T) {
	t.Run("filters to project tags, strips the prefix, orders count DESC then name ASC", func(t *testing.T) {
		// Deliberately supplied in a WRONG order, so a pass cannot come from the
		// input already being sorted.
		in := []TagCount{
			{Tag: "bug", Count: 99},           // not a project → excluded
			{Tag: "project:remix", Count: 3},  //
			{Tag: "initiative:x", Count: 50},  // not a project → excluded
			{Tag: "project:orbit", Count: 12}, //
			{Tag: "projectile", Count: 7},     // prefix-adjacent, NOT a project
			{Tag: "project:alpha", Count: 3},  // ties with remix → alpha first
		}
		got := ProjectsFromVocabulary(in)
		want := []ProjectCount{
			{Name: "orbit", Count: 12},
			{Name: "alpha", Count: 3},
			{Name: "remix", Count: 3},
		}
		if len(got) != len(want) {
			t.Fatalf("ProjectsFromVocabulary = %+v, want %+v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("ProjectsFromVocabulary = %+v, want %+v", got, want)
			}
		}
	})

	t.Run("the tie-break is name ASC, proven with equal counts only", func(t *testing.T) {
		got := ProjectsFromVocabulary([]TagCount{
			{Tag: "project:zeta", Count: 5},
			{Tag: "project:alpha", Count: 5},
			{Tag: "project:mid", Count: 5},
		})
		want := []string{"alpha", "mid", "zeta"}
		if len(got) != 3 {
			t.Fatalf("got %+v", got)
		}
		for i, w := range want {
			if got[i].Name != w {
				t.Fatalf("tie-break order = %+v, want %v", got, want)
			}
		}
	})

	t.Run("empty input yields an EMPTY NON-NIL slice (must encode as [], never null)", func(t *testing.T) {
		got := ProjectsFromVocabulary(nil)
		if got == nil {
			t.Fatal("ProjectsFromVocabulary(nil) must not be nil — a null would break the extension's list handling")
		}
		if len(got) != 0 {
			t.Fatalf("got %+v, want empty", got)
		}
	})

	t.Run("no project tags in a non-empty vocabulary still yields empty non-nil", func(t *testing.T) {
		got := ProjectsFromVocabulary([]TagCount{{Tag: "bug", Count: 1}, {Tag: "gate:x", Count: 2}})
		if got == nil || len(got) != 0 {
			t.Fatalf("got %+v (nil=%v), want an empty non-nil slice", got, got == nil)
		}
	})

	t.Run("a bare project: entry is skipped rather than yielding an empty name", func(t *testing.T) {
		got := ProjectsFromVocabulary([]TagCount{{Tag: "project:", Count: 4}, {Tag: "project:ok", Count: 1}})
		if len(got) != 1 || got[0].Name != "ok" {
			t.Fatalf("got %+v, want only {ok 1}", got)
		}
	})
}

// TestProjectTagLengthBoundIsTheGeneralTagCap documents (and pins) a DELIBERATE
// deviation from the feature brief. The brief asked for "slug max 64 chars", but
// the pre-existing MaxTagRunes cap already bounds the WHOLE tag at 64 runes, and
// `project:` costs 8 of them — so the real slug ceiling is 56. Adding a second,
// slug-specific length rule would put ONE rule in TWO places (the exact mistake
// this codebase just spent six audit rounds unwinding), and the extra rule would
// be unreachable anyway. So: no new length rule; this test pins where the real
// boundary is, measured at BOTH sides of it.
//
// INVARIANT GUARD — passes pre-change too (the general cap already existed); it
// exists so the 56 is documented rather than discovered.
func TestProjectTagLengthBoundIsTheGeneralTagCap(t *testing.T) {
	const nsCost = len("project:") // 8
	maxSlug := MaxTagRunes - nsCost
	if maxSlug != 56 {
		t.Fatalf("expected the project slug ceiling to be 56, got %d (MaxTagRunes=%d)", maxSlug, MaxTagRunes)
	}
	ok := "project:" + strings.Repeat("a", maxSlug)
	if err := ValidateTags([]string{ok}); err != nil {
		t.Fatalf("a %d-char slug must pass (whole tag = %d runes): %v", maxSlug, len(ok), err)
	}
	tooLong := "project:" + strings.Repeat("a", maxSlug+1)
	err := ValidateTags([]string{tooLong})
	if err == nil {
		t.Fatalf("a %d-char slug must be rejected (whole tag = %d runes)", maxSlug+1, len(tooLong))
	}
	if !strings.Contains(err.Error(), "too long") {
		t.Fatalf("error %q should be the general length error", err.Error())
	}
}
