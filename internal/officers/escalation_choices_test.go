package officers

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCheckChoicesRefusesWhatTheHQAPIRefuses: officer_complete's `choices`
// is refused locally under the HQ API's own limits — more than 4, a value
// that is not a stable id, a reserved value, a repeated value, a label empty
// or over 80 characters, an unknown key, a missing field, any outcome but
// escalated — and each refusal names what is wrong. The shapes the HQ API
// takes pass.
func TestCheckChoicesRefusesWhatTheHQAPIRefuses(t *testing.T) {
	label81 := strings.Repeat("x", 81)
	label80 := strings.Repeat("é", 80) // 80 characters, 160 bytes
	for _, c := range []struct {
		name, raw, outcome, want string
	}{
		{"five choices", `[{"value":"a","label":"A"},{"value":"b","label":"B"},{"value":"c","label":"C"},{"value":"d","label":"D"},{"value":"e","label":"E"}]`, "escalated", "at most 4 choices"},
		{"upper-case value", `[{"value":"Rollback","label":"Roll back"}]`, "escalated", "choices[0].value"},
		{"value too long", `[{"value":"` + strings.Repeat("a", 33) + `","label":"A"}]`, "escalated", "choices[0].value"},
		{"value starting with -", `[{"value":"-a","label":"A"}]`, "escalated", "choices[0].value"},
		{"reserved resolve", `[{"value":"resolve","label":"Resolve"}]`, "escalated", "HQ API's own buttons"},
		{"reserved other", `[{"value":"keep","label":"Keep"},{"value":"other","label":"Other"}]`, "escalated", "choices[1].value \"other\" is one of the HQ API's own"},
		{"repeated value", `[{"value":"keep","label":"Keep"},{"value":"keep","label":"Keep it"}]`, "escalated", "choices[1].value \"keep\" is repeated"},
		{"empty label", `[{"value":"keep","label":"   "}]`, "escalated", "choices[0].label must be 1 to 80"},
		{"label over 80", `[{"value":"keep","label":"` + label81 + `"}]`, "escalated", "choices[0].label must be 1 to 80"},
		{"missing label", `[{"value":"keep"}]`, "escalated", "needs both value and label"},
		{"unknown key", `[{"value":"keep","label":"Keep","style":"danger"}]`, "escalated", "no other keys"},
		{"not an array", `{"value":"keep","label":"Keep"}`, "escalated", "must be an array"},
		{"outcome done", `[{"value":"keep","label":"Keep"}]`, "done", "only with outcome escalated, not done"},
		{"outcome no_action", `[{"value":"keep","label":"Keep"}]`, "no_action", "only with outcome escalated, not no_action"},
		// Taken.
		{"four choices", `[{"value":"a","label":"A"},{"value":"b-2","label":"B"},{"value":"c_3","label":"C"},{"value":"9d","label":"D"}]`, "escalated", ""},
		{"label of 80 characters", `[{"value":"keep","label":"` + label80 + `"}]`, "escalated", ""},
		{"absent", ``, "done", ""},
		{"null", `null`, "done", ""},
		{"empty", `[]`, "no_action", ""},
	} {
		got := CheckChoices(json.RawMessage(c.raw), c.outcome)
		switch {
		case c.want == "" && got != "":
			t.Errorf("%s: refused (%s), want taken", c.name, got)
		case c.want != "" && !strings.Contains(got, c.want):
			t.Errorf("%s: %q, want a refusal containing %q", c.name, got, c.want)
		}
	}
}

// TestPROpenedSendsFromItemOnlyWhenKnown: the pr-opened body carries
// from_item, as a JSON number, when it is set, and no from_item key at all
// when it is 0 (an HQ API that predates the field refuses it).
func TestPROpenedSendsFromItemOnlyWhenKnown(t *testing.T) {
	p := PROpened{Repo: "example-org/example-repo", PR: 7, HeadSHA: strings.Repeat("a", 40), HeadRef: "officer/x/42-fix", Base: "main", Author: "example[bot]"}
	for _, c := range []struct {
		from int
		want string
	}{{0, ""}, {42, "42"}, {9001, "9001"}} {
		p.FromItem = c.from
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		got, has := m["from_item"]
		switch {
		case c.want == "" && has:
			t.Errorf("from_item %d: the body %s has a from_item key, want none", c.from, raw)
		case c.want != "" && string(got) != c.want:
			t.Errorf("from_item %d: the body %s has from_item %s, want %s", c.from, raw, got, c.want)
		}
		if _, ok := m["repo"]; !ok {
			t.Fatalf("the body read is wrong: no repo in %s", raw)
		}
	}
}
