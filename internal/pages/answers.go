package pages

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A RESPONSE AS ITS OWNER READS IT: labelled fields, named by the page's own
// form definition (the version the response was sent through), never the
// stored JSON. The JSON and its ids are the owner's screen's admin-only ⓘ.
//
// Every value is a stranger's words: the caller escapes each one for its
// medium; nothing here is markup.

// AnswerLine is one labelled value of a response.
type AnswerLine struct {
	Label string
	Value string
}

// Answer is one response, described.
type Answer struct {
	// Heading names the part of the page it was sent through: the block's own
	// label, else what kind of part it is.
	Heading string
	Lines   []AnswerLine
}

// maxAnswerLines bounds the lines a custom script's data is drawn as.
const maxAnswerLines = 40

// emptyAnswer is a value the viewer left out.
const emptyAnswer = "(left empty)"

// customHeading is the heading of what a page's own script sent.
const customHeading = "Sent by the page's own script"

// DescribeAnswer describes a submit (KindSubmit) response's data as the
// fields of blocks' component — blocks is the version it was sent through.
// A component the version does not have, or data that is not its shape, is
// described from the data alone (DescribeData).
//
// blocks nil is a version that could not be read (no version has no blocks):
// the response is then "A response to an older version", from its data.
//
// Every label, value and the heading are bounded (maxAnswerLabelRunes,
// maxAnswerValueRunes): the caller draws them as they come.
func DescribeAnswer(blocks []Block, component string, data []byte) Answer {
	a := describeAnswer(blocks, component, data)
	a.Heading = cutRunes(a.Heading, maxAnswerLabelRunes)
	a.Lines = bounded(a.Lines)
	return a
}

// unreadVersionHeading heads a response whose version could not be read.
const unreadVersionHeading = "A response to an older version"

func describeAnswer(blocks []Block, component string, data []byte) Answer {
	if blocks == nil && component != CustomComponent {
		var v any
		_ = json.Unmarshal(data, &v)
		return Answer{Heading: unreadVersionHeading, Lines: DescribeData(v)}
	}
	if component == CustomComponent {
		var v struct {
			Value any `json:"value"`
		}
		_ = json.Unmarshal(data, &v)
		return Answer{Heading: customHeading, Lines: DescribeData(v.Value)}
	}
	b, ok := Find(blocks, component)
	if !ok {
		var v any
		_ = json.Unmarshal(data, &v)
		return Answer{Heading: "A part this version of the page no longer has", Lines: DescribeData(v)}
	}
	heading := b.Label
	switch b.Type {
	case BlockQuestion:
		var in struct {
			Answer string `json:"answer"`
		}
		_ = json.Unmarshal(data, &in)
		return Answer{Heading: orHeading(heading, "A question"), Lines: []AnswerLine{{Label: "Answer", Value: orEmpty(in.Answer)}}}
	case BlockChoice:
		var in struct {
			Choices []string `json:"choices"`
		}
		_ = json.Unmarshal(data, &in)
		v := strings.Join(in.Choices, ", ")
		if v == "" {
			v = "Nothing chosen"
		}
		return Answer{Heading: orHeading(heading, "A choice"), Lines: []AnswerLine{{Label: "Chose", Value: v}}}
	case BlockForm:
		var in struct {
			Fields map[string]any `json:"fields"`
		}
		_ = json.Unmarshal(data, &in)
		lines := make([]AnswerLine, 0, len(b.Fields))
		for _, f := range b.Fields {
			lines = append(lines, AnswerLine{Label: fieldName(f), Value: fieldValue(f, in.Fields[f.Name])})
		}
		return Answer{Heading: orHeading(heading, "A form"), Lines: lines}
	case BlockApproval:
		var in struct {
			Decision string `json:"decision"`
			Comment  string `json:"comment"`
		}
		_ = json.Unmarshal(data, &in)
		d := "Declined"
		if in.Decision == "approve" {
			d = "Approved"
		}
		lines := []AnswerLine{{Label: "Decision", Value: d}}
		if c := strings.TrimSpace(in.Comment); c != "" {
			lines = append(lines, AnswerLine{Label: "Comment", Value: c})
		}
		return Answer{Heading: orHeading(heading, "An approval"), Lines: lines}
	}
	var v any
	_ = json.Unmarshal(data, &v)
	return Answer{Heading: orHeading(heading, "A part of the page"), Lines: DescribeData(v)}
}

// UploadHeading names the upload part of blocks a file came through: its
// label, else "A file"; blocks nil (its version unread) is
// unreadVersionHeading.
func UploadHeading(blocks []Block, component string) string {
	if blocks == nil {
		return unreadVersionHeading
	}
	if b, ok := Find(blocks, component); ok {
		return cutRunes(orHeading(b.Label, "A file"), maxAnswerLabelRunes)
	}
	return "A file"
}

// fieldValue is one form field's stored value in words: a checkbox Yes/No, a
// number as written, text as sent, "(left empty)" for nothing.
func fieldValue(f Field, v any) string {
	if f.Type == FieldCheckbox {
		if b, _ := v.(bool); b {
			return "Yes"
		}
		return "No"
	}
	return orEmpty(scalarText(v))
}

// The bounds of a described response. 🔴 A RESPONSE IS A STRANGER'S, AND
// THE OWNER'S SCREEN DRAWS EVERY ONE ON EVERY LOAD: a submit is up to 32 KiB
// of arbitrary JSON (a page's own script's), so describing it must cost at
// most linear time in its size and produce bounded output — a nesting
// thousands deep is cut at maxAnswerDepth, never walked or re-encoded per
// level.
const (
	// maxAnswerDepth is the deepest a script's data is followed; a list or
	// object deeper than this is one "(nested data)" line.
	maxAnswerDepth = 4
	// maxAnswerLabelRunes and maxAnswerValueRunes bound each line.
	maxAnswerLabelRunes = 200
	maxAnswerValueRunes = 2000
)

// nestedData is a value deeper than maxAnswerDepth, as the screen says it.
const nestedData = "(nested data)"

// DescribeData is data no form defines (a page's own script's) as labelled
// lines: an object's keys as labels (a nested one's joined with " · "), a
// list of plain values joined with ", ", anything else as one "Value" line;
// past maxAnswerDepth levels, "(nested data)". At most maxAnswerLines lines;
// past that, one line says how many more. Linear in the data's size: the path
// is a slice joined only at a leaf, and nothing is re-encoded.
func DescribeData(v any) []AnswerLine {
	var out []AnswerLine
	more := 0
	add := func(path []string, value string) {
		if len(out) >= maxAnswerLines {
			more++
			return
		}
		label := "Value"
		if len(path) > 0 {
			label = strings.Join(path, " · ")
		}
		out = append(out, AnswerLine{Label: label, Value: orEmpty(value)})
	}
	var walk func(path []string, v any)
	walk = func(path []string, v any) {
		switch x := v.(type) {
		case map[string]any:
			if len(path) >= maxAnswerDepth {
				add(path, nestedData)
				return
			}
			if len(x) == 0 {
				add(path, "")
				return
			}
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(append(path[:len(path):len(path)], k), x[k])
			}
		case []any:
			if len(path) >= maxAnswerDepth {
				add(path, nestedData)
				return
			}
			// Plain (every element a scalar) is decided BEFORE any element
			// is turned into text: a nested element is walked, never encoded.
			plain := true
			for _, e := range x {
				switch e.(type) {
				case map[string]any, []any:
					plain = false
				}
			}
			if plain {
				parts := make([]string, 0, len(x))
				for _, e := range x {
					parts = append(parts, scalarText(e))
				}
				add(path, strings.Join(parts, ", "))
				return
			}
			for i, e := range x {
				walk(append(path[:len(path):len(path)], strconv.Itoa(i+1)), e)
			}
		default:
			add(path, scalarText(x))
		}
	}
	walk(nil, v)
	if more > 0 {
		out = append(out, AnswerLine{Label: "More", Value: strconv.Itoa(more) + " more not shown here"})
	}
	return out
}

// bounded is lines with each label and value cut to its bound (cutRunes).
func bounded(lines []AnswerLine) []AnswerLine {
	for i := range lines {
		lines[i].Label = cutRunes(lines[i].Label, maxAnswerLabelRunes)
		lines[i].Value = cutRunes(lines[i].Value, maxAnswerValueRunes)
	}
	return lines
}

// cutRunes is s cut to n runes, with an ellipsis when cut.
func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// scalarText is a JSON scalar in words.
func scalarText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(x)
	case bool:
		if x {
			return "Yes"
		}
		return "No"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	}
	// A list or object (a form field never holds one: CheckSubmit) is not
	// re-encoded here: DescribeData walks those.
	return nestedData
}

func orEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return emptyAnswer
	}
	return s
}

func orHeading(label, kind string) string {
	if strings.TrimSpace(label) != "" {
		return label
	}
	return kind
}
