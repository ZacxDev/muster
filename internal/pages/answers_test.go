package pages

import (
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestAResponseIsDescribedByItsPagesOwnForm: each kind of part's response, as
// CheckSubmit stores it, comes out as the labelled fields its version's
// blocks define — the field's label (its name when it has none), in the
// form's order, a checkbox Yes/No, a number as written, a missing value
// "(left empty)" — never the stored JSON. Literal expectations.
func TestAResponseIsDescribedByItsPagesOwnForm(t *testing.T) {
	blocks, _, err := Parse([]byte(`[
 {"type":"question","id":"note","label":"Anything else?"},
 {"type":"choice","id":"pick","label":"Which subject line?","options":["Spring sale","Last call","Hello"],"multiple":true},
 {"type":"form","id":"contact","label":"Your details","fields":[
   {"name":"email","label":"Email","type":"email","required":true},
   {"name":"seats","type":"number"},
   {"name":"agree","label":"Send me news","type":"checkbox"},
   {"name":"company","label":"Company","type":"text"}]},
 {"type":"approval","id":"ok","label":"Go ahead?","comment":true},
 {"type":"upload","id":"logo","label":"Your logo"},
 {"type":"html","html":"<p>x</p>"}
]`))
	if err != nil {
		t.Fatal(err)
	}
	stored := func(component, data string) []byte {
		t.Helper()
		b, err := CheckSubmit(blocks, component, []byte(data))
		if err != nil {
			t.Fatalf("%s: %v", component, err)
		}
		return b
	}
	for _, c := range []struct {
		name, component, data string
		want                  Answer
	}{
		{"question", "note", `{"answer":"Ship it on Friday"}`,
			Answer{"Anything else?", []AnswerLine{{"Answer", "Ship it on Friday"}}}},
		{"choice of two", "pick", `{"choices":["Last call","Spring sale"]}`,
			Answer{"Which subject line?", []AnswerLine{{"Chose", "Last call, Spring sale"}}}},
		{"choice of none", "pick", `{"choices":[]}`,
			Answer{"Which subject line?", []AnswerLine{{"Chose", "Nothing chosen"}}}},
		{"form", "contact", `{"fields":{"email":"client@example.com","seats":12.5,"agree":true}}`,
			Answer{"Your details", []AnswerLine{{"Email", "client@example.com"}, {"seats", "12.5"}, {"Send me news", "Yes"}, {"Company", "(left empty)"}}}},
		{"approval", "ok", `{"decision":"decline","comment":"Not yet"}`,
			Answer{"Go ahead?", []AnswerLine{{"Decision", "Declined"}, {"Comment", "Not yet"}}}},
		{"the page's own script", CustomComponent, `{"picked":"May 8","from":{"who":"custom script","n":3},"tags":["a","b"]}`,
			Answer{"Sent by the page's own script", []AnswerLine{{"from · n", "3"}, {"from · who", "custom script"}, {"picked", "May 8"}, {"tags", "a, b"}}}},
	} {
		if got := DescribeAnswer(blocks, c.component, stored(c.component, c.data)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %#v\nwant %#v", c.name, got, c.want)
		}
	}
	// A part the version no longer has: described from the data alone.
	if got := DescribeAnswer(blocks, "gone", []byte(`{"answer":"hi"}`)); !reflect.DeepEqual(got,
		Answer{"A part this version of the page no longer has", []AnswerLine{{"answer", "hi"}}}) {
		t.Errorf("a missing part: %#v", got)
	}
	if got := UploadHeading(blocks, "logo"); got != "Your logo" {
		t.Errorf("an upload's heading: %q", got)
	}
}

// TestADeeplyNestedResponseIsDescribedFastAndBounded: a stranger's custom
// submit nested thousands deep (lists, objects, and both with a sibling at
// every level), stored as CheckSubmit stores it, is described in linear time
// and as bounded output. REGRESSION TEST: at depth 4500 the earlier
// DescribeData re-encoded the whole subtree at every level and joined the
// label at every level — measured ~1 s of CPU per response, per load of the
// owner's page screen.
func TestADeeplyNestedResponseIsDescribedFastAndBounded(t *testing.T) {
	blocks, _, err := Parse([]byte(`[{"type":"html","html":"<p>x</p>"}]`))
	if err != nil {
		t.Fatal(err)
	}
	const depth = 4500
	nest := func(open, close string) string {
		return strings.Repeat(open, depth) + `"leaf"` + strings.Repeat(close, depth)
	}
	for name, payload := range map[string]string{
		"lists":              nest(`[`, `]`),
		"objects":            nest(`{"a":`, `}`),
		"lists with sibling": nest(`["x",`, `]`),
		// 12 bytes a level: as deep as the 32 KiB submit bound allows.
		"objects with sibling": strings.Repeat(`{"s":0,"a":`, 2500) + `"leaf"` + strings.Repeat(`}`, 2500),
	} {
		data, err := CheckSubmit(blocks, CustomComponent, []byte(payload))
		if err != nil {
			t.Fatalf("%s: the submit is refused (%v), so this would test nothing", name, err)
		}
		// The fastest of three: a loaded machine slows a run, never speeds it.
		best := time.Hour
		var a Answer
		for i := 0; i < 3; i++ {
			start := time.Now()
			a = DescribeAnswer(blocks, CustomComponent, data)
			if d := time.Since(start); d < best {
				best = d
			}
		}
		if best > 50*time.Millisecond {
			t.Errorf("%s: described in %v, want well under 50ms (linear in the data)", name, best)
		}
		size, nested := 0, false
		for _, l := range a.Lines {
			size += len(l.Label) + len(l.Value)
			nested = nested || l.Value == nestedData
			if utf8.RuneCountInString(l.Label) > maxAnswerLabelRunes || utf8.RuneCountInString(l.Value) > maxAnswerValueRunes {
				t.Errorf("%s: a line past its bound: %d / %d runes", name, utf8.RuneCountInString(l.Label), utf8.RuneCountInString(l.Value))
			}
		}
		if len(a.Lines) > maxAnswerLines+1 || size > 4096 || !nested {
			t.Errorf("%s: %d lines, %d bytes, nested-data line %v; want a few short lines ending the nesting", name, len(a.Lines), size, nested)
		}
	}
}

// TestALongLabelOrValueIsCut: a value or a key past its bound is cut, with an
// ellipsis, never drawn whole.
func TestALongLabelOrValueIsCut(t *testing.T) {
	long := strings.Repeat("é", 5000)
	lines := bounded(DescribeData(map[string]any{long: long}))
	if len(lines) != 1 || utf8.RuneCountInString(lines[0].Label) != maxAnswerLabelRunes || utf8.RuneCountInString(lines[0].Value) != maxAnswerValueRunes ||
		!strings.HasSuffix(lines[0].Label, "…") || !strings.HasSuffix(lines[0].Value, "…") {
		t.Fatalf("cut: %d / %d runes", utf8.RuneCountInString(lines[0].Label), utf8.RuneCountInString(lines[0].Value))
	}
	// Through DescribeAnswer too: a page script's long key and value.
	blocks, _, _ := Parse([]byte(`[{"type":"html","html":"<p>x</p>"}]`))
	data, err := CheckSubmit(blocks, CustomComponent, []byte(`{"`+strings.Repeat("k", 500)+`":"`+strings.Repeat("v", 5000)+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if a := DescribeAnswer(blocks, CustomComponent, data); len(a.Lines) != 1 || utf8.RuneCountInString(a.Lines[0].Label) != maxAnswerLabelRunes ||
		utf8.RuneCountInString(a.Lines[0].Value) != maxAnswerValueRunes {
		t.Errorf("DescribeAnswer does not bound its lines: %d lines", len(a.Lines))
	}
	if got := DescribeAnswer(nil, "pick", []byte(`{"choices":["a"]}`)); got.Heading != "A response to an older version" {
		t.Errorf("an unread version's response is headed %q", got.Heading)
	}
}
