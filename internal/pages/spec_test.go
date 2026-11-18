package pages

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// allBlocks is one block of every type, each with the fields it reads.
const allBlocks = `[
 {"type":"markdown","text":"# Launch\nPick a date."},
 {"type":"question","id":"q1","label":"Anything else?","required":true,"multiline":true},
 {"type":"choice","id":"c1","label":"Which date?","options":["May 1","May 8"],"required":true},
 {"type":"form","id":"f1","label":"Contact","fields":[
   {"name":"email","label":"Email","type":"email","required":true},
   {"name":"seats","type":"number"},
   {"name":"day","type":"date"},
   {"name":"plan","type":"select","options":["basic","pro"]},
   {"name":"agree","type":"checkbox","required":true},
   {"name":"note","type":"textarea"},
   {"name":"company","type":"text"}]},
 {"type":"upload","id":"u1","label":"Your logo","multiple":true},
 {"type":"approval","id":"a1","label":"Ship it?","comment":true},
 {"type":"html","html":"<div id=x></div><script>muster.submit({hi:1})</script>"}
]`

func TestParseAcceptsEveryBlockType(t *testing.T) {
	blocks, size, err := Parse([]byte(allBlocks))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(blocks) != 7 {
		t.Fatalf("got %d blocks, want 7", len(blocks))
	}
	enc, _ := json.Marshal(blocks)
	if size != len(enc) {
		t.Errorf("size %d, want the stored JSON's %d", size, len(enc))
	}
	if !HasHTML(blocks) {
		t.Error("HasHTML is false for a page with an html block")
	}
}

// TestParseRefusesWhatABlockDoesNotTake: every refusal names what is wrong,
// and each is a distinct mistake an agent makes.
func TestParseRefusesWhatABlockDoesNotTake(t *testing.T) {
	for _, c := range []struct{ name, blocks, want string }{
		{"empty", `[]`, "at least one block"},
		{"not a list", `{"type":"markdown"}`, "not a list"},
		{"unknown type", `[{"type":"video"}]`, "unknown block type"},
		{"unknown field", `[{"type":"markdown","text":"x","colour":"red"}]`, "colour"},
		{"a field of another type", `[{"type":"markdown","text":"x","options":["a"]}]`, "does not take options"},
		{"markdown with an id", `[{"type":"markdown","id":"m","text":"x"}]`, "takes no id"},
		{"no id", `[{"type":"question","label":"q"}]`, "needs an id"},
		{"bad id", `[{"type":"question","id":"1q","label":"q"}]`, "must be a letter"},
		{"custom is reserved", `[{"type":"question","id":"Custom","label":"q"}]`, "reserved"},
		{"duplicate id", `[{"type":"question","id":"q","label":"a"},{"type":"approval","id":"Q","label":"b"}]`, "used twice"},
		{"no label", `[{"type":"question","id":"q"}]`, "needs a label"},
		{"choice without options", `[{"type":"choice","id":"c","label":"x"}]`, "needs options"},
		{"duplicate option", `[{"type":"choice","id":"c","label":"x","options":["a","a"]}]`, "listed twice"},
		{"form without fields", `[{"type":"form","id":"f"}]`, "needs fields"},
		{"bad field type", `[{"type":"form","id":"f","fields":[{"name":"a","type":"color"}]}]`, "unknown field type"},
		{"duplicate field", `[{"type":"form","id":"f","fields":[{"name":"a","type":"text"},{"name":"a","type":"email"}]}]`, "used twice"},
		{"select without options", `[{"type":"form","id":"f","fields":[{"name":"a","type":"select"}]}]`, "needs options"},
		{"empty html", `[{"type":"html","html":"  "}]`, "needs html"},
		{"too large", `[{"type":"html","html":"` + strings.Repeat("a", MaxHTMLBytes+1) + `"}]`, "at most"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := Parse([]byte(c.blocks))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Parse(%s) = %v, want ErrInvalid containing %q", c.name, err, c.want)
			}
		})
	}
}

func TestParseRefusesAVersionOverTheCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < 3; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"type":"html","html":"` + strings.Repeat("x", MaxHTMLBytes-10) + `"}`)
	}
	b.WriteString("]")
	if _, _, err := Parse([]byte(b.String())); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "KiB") {
		t.Fatalf("a version over %d bytes: %v", MaxVersionBytes, err)
	}
}

func TestCleanTitle(t *testing.T) {
	if got, err := CleanTitle("  Launch plan  "); err != nil || got != "Launch plan" {
		t.Fatalf("CleanTitle = %q, %v", got, err)
	}
	for _, bad := range []string{"", "   ", strings.Repeat("a", MaxTitleRunes+1), "a\nb", "evil‮exe"} {
		if _, err := CleanTitle(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("CleanTitle(%q) = nil error", bad)
		}
	}
}

func mustBlocks(t *testing.T) []Block {
	t.Helper()
	b, _, err := Parse([]byte(allBlocks))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestCheckSubmitStoresWhatEachComponentSends pins the stored shape of every
// component's submit: the literal JSON, so a change of shape is a decision.
func TestCheckSubmitStoresWhatEachComponentSends(t *testing.T) {
	blocks := mustBlocks(t)
	for _, c := range []struct{ component, data, want string }{
		{"q1", `{"answer":"  late May works  "}`, `{"answer":"late May works"}`},
		{"c1", `{"choices":["May 8"]}`, `{"choices":["May 8"]}`},
		{"f1", `{"fields":{"email":"a@example.com","seats":"3","day":"2026-05-08","plan":"pro","agree":true,"note":"hi","company":"Example"}}`,
			`{"fields":{"agree":true,"company":"Example","day":"2026-05-08","email":"a@example.com","note":"hi","plan":"pro","seats":3}}`},
		{"f1", `{"fields":{"email":"a@example.com","agree":true}}`,
			`{"fields":{"agree":true,"company":"","day":"","email":"a@example.com","note":"","plan":"","seats":null}}`},
		{"a1", `{"decision":"decline","comment":"not yet"}`, `{"decision":"decline","comment":"not yet"}`},
		{"custom", `{"hi":[1,2]}`, `{"value":{"hi":[1,2]}}`},
		{"custom", ``, `{"value":null}`},
	} {
		got, err := CheckSubmit(blocks, c.component, json.RawMessage(c.data))
		if err != nil {
			t.Errorf("CheckSubmit(%s, %s): %v", c.component, c.data, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("CheckSubmit(%s, %s) = %s, want %s", c.component, c.data, got, c.want)
		}
	}
}

func TestCheckSubmitRefusesWhatAComponentDoesNotSend(t *testing.T) {
	blocks := mustBlocks(t)
	for _, c := range []struct{ name, component, data, want string }{
		{"unknown component", "zz", `{}`, "no component"},
		{"an upload", "u1", `{}`, "send a file"},
		{"required answer", "q1", `{"answer":"  "}`, "required"},
		{"extra key", "q1", `{"answer":"x","admin":true}`, "sends {answer}"},
		{"long answer", "q1", `{"answer":"` + strings.Repeat("a", maxAnswerRunes+1) + `"}`, "longer"},
		{"not an option", "c1", `{"choices":["June"]}`, "not one of"},
		{"two of a single choice", "c1", `{"choices":["May 1","May 8"]}`, "choose one"},
		{"required choice", "c1", `{"choices":[]}`, "required"},
		{"unknown field", "f1", `{"fields":{"email":"a@example.com","agree":true,"role":"admin"}}`, "no field"},
		{"missing required field", "f1", `{"fields":{"agree":true}}`, "required"},
		{"bad email", "f1", `{"fields":{"email":"nobody","agree":true}}`, "not an email"},
		{"bad number", "f1", `{"fields":{"email":"a@example.com","agree":true,"seats":"many"}}`, "not a number"},
		{"bad date", "f1", `{"fields":{"email":"a@example.com","agree":true,"day":"2026-02-30"}}`, "not a real date"},
		{"bad select", "f1", `{"fields":{"email":"a@example.com","agree":true,"plan":"enterprise"}}`, "not one of"},
		{"unticked required checkbox", "f1", `{"fields":{"email":"a@example.com","agree":false}}`, "ticked"},
		{"two lines in text", "f1", `{"fields":{"email":"a@example.com","agree":true,"company":"a\nb"}}`, "one line"},
		{"bad decision", "a1", `{"decision":"maybe"}`, "approve or decline"},
		{"custom too large", "custom", `"` + strings.Repeat("a", MaxSubmitBytes) + `"`, "larger"},
		{"custom not JSON", "custom", `{`, "not JSON"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := CheckSubmit(blocks, c.component, json.RawMessage(c.data))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("CheckSubmit = %v, want ErrInvalid containing %q", err, c.want)
			}
		})
	}
}

// TestCustomSubmitNeedsAnHTMLBlock: muster.submit's default component is
// accepted only from a page that has custom script.
func TestCustomSubmitNeedsAnHTMLBlock(t *testing.T) {
	blocks, _, err := Parse([]byte(`[{"type":"question","id":"q1","label":"x"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CheckSubmit(blocks, CustomComponent, json.RawMessage(`{}`)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("custom submit to a page with no html block: %v", err)
	}
	if UploadTarget(blocks, CustomComponent) || UploadTarget(blocks, "q1") {
		t.Error("UploadTarget accepts a page with no upload block and no html block")
	}
	all := mustBlocks(t)
	if !UploadTarget(all, "u1") || !UploadTarget(all, CustomComponent) || UploadTarget(all, "q1") {
		t.Error("UploadTarget: want u1 and custom, not q1")
	}
}

func TestTokens(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewToken()
	if a == b || !TokenShapeOK(a) || len(a) != 43 {
		t.Fatalf("tokens %q %q", a, b)
	}
	for _, bad := range []string{"", "short", a + "x", strings.Repeat("!", 43), "../" + a[3:]} {
		if TokenShapeOK(bad) {
			t.Errorf("TokenShapeOK(%q)", bad)
		}
	}
	if h := TokenHash("abc"); h != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("TokenHash(abc) = %s", h)
	}
	if LinkPath(a) != "/s/"+a {
		t.Errorf("LinkPath = %s", LinkPath(a))
	}
}
