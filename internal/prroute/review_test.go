package prroute

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// ParseReview: the review_id and finding ids HQ's PR-review respond channel
// takes, read from the same last json block ParseMail reads, each kept only
// in its grammar (they are printed outside the untrusted fence).

func reviewJSON(extra string) string {
	return `{"repo":"` + mailRepo + `","pr":4817,"head_sha":"` + mailSHA + `"` + extra + `}`
}

func TestParseReviewReadsTheIDsFromTheLastBlock(t *testing.T) {
	body := block("```", "json", reviewJSON(`,"review_id":"decoy_2213_r9","findings":[{"id":"f_dddddddddddd"}]`)) +
		"\n" + block("```", "json", reviewJSON(`,"review_id":"widget-api_4817_r2","findings":[`+
		`{"id":"f_0a1b2c3d4e5f","severity":"high","file":"a.go"},`+
		`{"id":"f_9e8d7c6b5a40"},`+
		`{"id":"F_0A1B2C3D4E5F","severity":"low"},`+
		`{"id":"f_0a1b2c3d4e5","severity":"low"},`+
		`{"id":12,"severity":"low"},`+
		`{"id":"f_123456789abc","severity":"High!"}]`))
	got, ok := ParseReview(body)
	want := Review{Ref: Ref{Repo: mailRepo, PR: mailPR, HeadSHA: mailSHA}, ReviewID: "widget-api_4817_r2",
		Findings: []Finding{{ID: "f_0a1b2c3d4e5f", Severity: "high"}, {ID: "f_9e8d7c6b5a40"}, {ID: "f_123456789abc"}},
		Listed:   6, FindingsKnown: true}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseReview = %+v, %v; want %+v", got, ok, want)
	}
}

func TestParseReviewIsParseMailsVerdict(t *testing.T) {
	// No repo: ParseMail refuses, so ParseReview does, whatever ids it holds.
	if _, ok := ParseReview(block("```", "json", `{"pr":4817,"head_sha":"`+mailSHA+`","findings":[{"id":"f_0a1b2c3d4e5f"}]}`)); ok {
		t.Fatal("ParseReview accepted a block ParseMail refuses")
	}
	// A bad review_id and a findings field of another shape: the Ref stands, no ids.
	got, ok := ParseReview(block("```", "json", reviewJSON(`,"review_id":"has space","findings":"none"`)))
	if !ok || got.ReviewID != "" || len(got.Findings) != 0 || got.Repo != mailRepo {
		t.Fatalf("ParseReview = %+v, %v; want the Ref, no review id, no findings", got, ok)
	}
}

// Listed counts the findings array whatever its ids: a round whose findings
// all have malformed ids still HAS findings; an empty array has none. 🔴 Only
// a PRESENT ARRAY is known (FindingsKnown): a missing key, null, an object or
// a string is unknown, never "no findings".
func TestParseReviewListedCountsTheArray(t *testing.T) {
	for _, c := range []struct {
		name, extra string
		known       bool
		listed      int
	}{
		{"empty array", `,"findings":[]`, true, 0},
		{"empty array, spaced", `,"findings": [ ]`, true, 0},
		{"one finding", `,"findings":[{"id":"f_0a1b2c3d4e5f"}]`, true, 1},
		{"malformed ids still count", `,"findings":[{"id":"NOT-AN-ID"},{"severity":"low"}]`, true, 2},
		{"missing key", ``, false, 0},
		{"object-shaped", `,"findings":{"id":"f_0a1b2c3d4e5f"}`, false, 0},
		{"null", `,"findings":null`, false, 0},
		{"string", `,"findings":"none"`, false, 0},
	} {
		got, ok := ParseReview(block("```", "json", reviewJSON(c.extra)))
		if !ok || got.FindingsKnown != c.known || got.Listed != c.listed {
			t.Errorf("%s: FindingsKnown %v Listed %d (%v), want %v and %d", c.name, got.FindingsKnown, got.Listed, ok, c.known, c.listed)
		}
	}
}

func TestParseReviewBoundsTheFindings(t *testing.T) {
	var fs []string
	for i := 0; i < MaxReviewFindings+7; i++ {
		fs = append(fs, `{"id":"f_`+strings.Repeat("0", 12-len(strconv.Itoa(i)))+strconv.Itoa(i)+`"}`)
	}
	got, ok := ParseReview(block("```", "json", reviewJSON(`,"findings":[`+strings.Join(fs, ",")+`]`)))
	if !ok || len(got.Findings) != MaxReviewFindings || got.Findings[0].ID != "f_000000000000" {
		t.Fatalf("%d findings (%v); want the first %d", len(got.Findings), ok, MaxReviewFindings)
	}
}
