package plugins

import (
	"encoding/json"
	"strconv"
	"testing"
)

// TestARefDecodesFromAStringOrItsExactDigits: RefText (and OutboundTarget's
// ref, which decodes through it) takes a JSON string, or a JSON number of
// digits only kept as exactly those digits — a 19-digit X post id included,
// which float64 rounds (2122495152350453825 → 2122495152350453824). The number
// and the string decode to the same value. A fraction, a sign or an exponent
// is refused; null leaves the value as it was.
func TestARefDecodesFromAStringOrItsExactDigits(t *testing.T) {
	const id = "2122495152350453825"
	if f, _ := json.Number(id).Float64(); strconv.FormatFloat(f, 'f', 0, 64) == id {
		t.Fatal("control: this id must be one float64 cannot hold, or the test proves nothing about precision")
	}
	for _, in := range []string{id, `"` + id + `"`, ` ` + id + ` `, `0`, `"t1_abc"`} {
		var r RefText
		if err := json.Unmarshal([]byte(in), &r); err != nil {
			t.Errorf("%s: refused: %v", in, err)
		}
	}
	var num, str RefText
	if err := json.Unmarshal([]byte(id), &num); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`"`+id+`"`), &str); err != nil {
		t.Fatal(err)
	}
	if num != id || str != id {
		t.Fatalf("the number decoded to %q and the string to %q; want both %q", num, str, id)
	}
	for _, bad := range []string{"2.122495152350453825e18", id + ".0", "-" + id, "1e3", "1E3", "true", "{}", "[]"} {
		var r RefText
		if err := json.Unmarshal([]byte(bad), &r); err == nil {
			t.Errorf("%s: want refused, got %q", bad, r)
		}
	}
	r := RefText("kept")
	if err := json.Unmarshal([]byte("null"), &r); err != nil || r != "kept" {
		t.Errorf("null: %q %v", r, err)
	}

	var tg OutboundTarget
	if err := json.Unmarshal([]byte(`{"kind":"like","ref":`+id+`,"handle":"h"}`), &tg); err != nil {
		t.Fatalf("a target whose ref is a number: %v", err)
	}
	if tg != (OutboundTarget{Kind: "like", Ref: id, Handle: "h"}) {
		t.Fatalf("the target: %+v", tg)
	}
	if err := json.Unmarshal([]byte(`{"kind":"like","ref":1e3}`), &tg); err == nil {
		t.Fatal("a target whose ref is an exponent: want refused")
	}
}
