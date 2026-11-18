package pages

import (
	"reflect"
	"testing"
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
