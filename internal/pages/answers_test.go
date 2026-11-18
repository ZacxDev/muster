package pages

import (
	"encoding/json"
	"reflect"
	"runtime"
	"strings"
	"testing"
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
// every level), stored as CheckSubmit stores it, is described as exactly the
// few lines its first four levels hold — the rest one "(nested data)" — and
// with work linear in its size: the bytes allocated describing depth 4500
// are under 20 times those at depth 450 (ten times the data). Bytes
// allocated, not wall time: deterministic on a loaded machine. REGRESSION
// TEST: the earlier DescribeData re-encoded the whole subtree at every level
// and joined the label at every level — ~1 s of CPU per response at depth
// 4500, per load of the owner's page screen.
func TestADeeplyNestedResponseIsDescribedFastAndBounded(t *testing.T) {
	blocks, _, err := Parse([]byte(`[{"type":"html","html":"<p>x</p>"}]`))
	if err != nil {
		t.Fatal(err)
	}
	type shape struct {
		open, close string
		max         int // as deep as the 32 KiB submit bound allows
		want        []AnswerLine
	}
	n := nestedData
	for name, c := range map[string]shape{
		"lists":   {`[`, `]`, 4500, []AnswerLine{{"1 · 1 · 1 · 1", n}}},
		"objects": {`{"a":`, `}`, 4500, []AnswerLine{{"a · a · a · a", n}}},
		"lists with sibling": {`["x",`, `]`, 4500, []AnswerLine{{"1", "x"}, {"2 · 1", "x"}, {"2 · 2 · 1", "x"},
			{"2 · 2 · 2 · 1", "x"}, {"2 · 2 · 2 · 2", n}}},
		"objects with sibling": {`{"s":0,"a":`, `}`, 2500, []AnswerLine{{"a · a · a · a", n}, {"a · a · a · s", "0"},
			{"a · a · s", "0"}, {"a · s", "0"}, {"s", "0"}}},
	} {
		describe := func(depth int) (Answer, uint64) {
			payload := strings.Repeat(c.open, depth) + `"leaf"` + strings.Repeat(c.close, depth)
			data, err := CheckSubmit(blocks, CustomComponent, []byte(payload))
			if err != nil {
				t.Fatalf("%s at %d: the submit is refused (%v), so this would test nothing", name, depth, err)
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			a := DescribeAnswer(blocks, CustomComponent, data)
			runtime.ReadMemStats(&after)
			return a, after.TotalAlloc - before.TotalAlloc
		}
		deep, big := describe(c.max)
		_, small := describe(c.max / 10)
		if !reflect.DeepEqual(deep.Lines, c.want) {
			t.Errorf("%s: %d lines %q, want exactly %q", name, len(deep.Lines), deep.Lines, c.want)
		}
		if small == 0 || big > 20*small {
			t.Errorf("%s: %d bytes allocated at depth %d against %d at depth %d — not linear in the data", name, big, c.max, small, c.max/10)
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

// TestAResponseDrawsAtMostItsBudget: one response draws at most
// maxAnswerBudget runes of labels and values; the lines past it, with those
// the 40-line cap already left out, are one "N more not shown here" line.
func TestAResponseDrawsAtMostItsBudget(t *testing.T) {
	blocks, _, _ := Parse([]byte(`[{"type":"html","html":"<p>x</p>"}]`))
	describe := func(keys, valueRunes int) Answer {
		t.Helper()
		m := map[string]string{}
		for i := 0; i < keys; i++ {
			m[strings.Repeat("k", 197)+string(rune('A'+i/26))+string(rune('a'+i%26))] = strings.Repeat("v", valueRunes)
		}
		raw, _ := json.Marshal(m)
		data, err := CheckSubmit(blocks, CustomComponent, raw)
		if err != nil {
			t.Fatal(err)
		}
		return DescribeAnswer(blocks, CustomComponent, data)
	}
	// 17 keys of 199 runes, values of 1700: two lines fit (3798 runes).
	a := describe(17, 1700)
	if len(a.Lines) != 3 || a.Lines[2] != (AnswerLine{"More", "15 more not shown here"}) {
		t.Errorf("17 large lines: %d lines, last %q", len(a.Lines), a.Lines[len(a.Lines)-1])
	}
	// 50 lines of 200 runes: 20 fit; the other 20 drawn and the 10 the
	// 40-line cap left out are "30 more".
	if a := describe(50, 1); len(a.Lines) != 21 || a.Lines[20] != (AnswerLine{"More", "30 more not shown here"}) {
		t.Errorf("50 small lines: %d lines, last %q", len(a.Lines), a.Lines[len(a.Lines)-1])
	}
	used := 0
	for _, l := range a.Lines {
		used += utf8.RuneCountInString(l.Label) + utf8.RuneCountInString(l.Value)
	}
	if used > maxAnswerBudget+64 {
		t.Errorf("a response draws %d runes, past its budget of %d", used, maxAnswerBudget)
	}
}
