package plugins

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// likeOf decodes a like whose target.ref is the raw JSON ref.
func likeOf(ref string) (OutboundTarget, error) {
	var tg OutboundTarget
	err := json.Unmarshal([]byte(`{"kind":"like","ref":`+ref+`,"handle":"h"}`), &tg)
	return tg, err
}

// TestARefDecodesFromAStringOrASmallWholeNumber: target.ref (and because_ref,
// through the same DecodeRef) is a JSON string taken as is, or a JSON number of
// digits only, at most 2^53-1. A string keeps its exact digits at any length:
// a 19-digit X post id included. A fraction, a sign or an exponent is refused.
func TestARefDecodesFromAStringOrASmallWholeNumber(t *testing.T) {
	const id = "2122495152350453825"
	for in, want := range map[string]string{
		`"` + id + `"`:       id,
		` "` + id + `" `:     id,
		`"t1_abc"`:           "t1_abc",
		`0`:                  "0",
		`12345`:              "12345",
		`9007199254740991`:   "9007199254740991",
		`"9007199254740993"`: "9007199254740993",
		`null`:               "",
	} {
		tg, err := likeOf(in)
		if err != nil {
			t.Errorf("%s: refused: %v", in, err)
			continue
		}
		if tg != (OutboundTarget{Kind: "like", Ref: want, Handle: "h"}) {
			t.Errorf("%s: decoded to %+v, want ref %q", in, tg, want)
		}
	}
	for _, bad := range []string{"2.122495152350453825e18", "12.0", "-12", "1e3", "1E3", "true", "{}", "[]"} {
		if tg, err := likeOf(bad); err == nil {
			t.Errorf("%s: want refused, got %+v", bad, tg)
		} else if !strings.Contains(err.Error(), "target.ref") {
			t.Errorf("%s: the refusal does not name target.ref: %v", bad, err)
		}
	}
}

// TestARefNumberAboveTwoToTheFiftyThreeIsRefused: a JSON number above 2^53-1
// (JavaScript's Number.MAX_SAFE_INTEGER) may already have been rounded by the
// client that parsed it — Node turns 2122495152350453825 into
// 2122495152350453800 before muster sees it, and stores no trace of the true
// one. So it is refused, with the fix spelled out, and never stored. 2^53-1 is
// the last number taken; 2^53 is the first refused (it is what 2^53+1 rounds
// to).
func TestARefNumberAboveTwoToTheFiftyThreeIsRefused(t *testing.T) {
	if MaxSafeRefNumber != 9007199254740991 {
		t.Fatalf("MaxSafeRefNumber = %d, want 2^53-1 = 9007199254740991", MaxSafeRefNumber)
	}
	// The control the boundary rests on: 2^53+1 is not a float64, 2^53-1 is.
	if f, _ := strconv.ParseFloat("9007199254740993", 64); strconv.FormatFloat(f, 'f', 0, 64) == "9007199254740993" {
		t.Fatal("control: 2^53+1 must round in a float64, or the boundary is not where the test says")
	}
	if tg, err := likeOf("9007199254740991"); err != nil || tg.Ref != "9007199254740991" {
		t.Fatalf("2^53-1: want taken as its digits, got %+v %v", tg, err)
	}
	for _, n := range []string{"9007199254740992", "9007199254740993", "2122495152350453800", "2122495152350453825", "18446744073709551615", "18446744073709551616", "123456789012345678901234567890"} {
		tg, err := likeOf(n)
		if err == nil {
			t.Errorf("%s: want refused, got %+v", n, tg)
			continue
		}
		want := `target.ref ` + n + ` is a JSON number above 2^53-1, so your client may have rounded it (JavaScript does): ` +
			`send the id as a JSON string, e.g. "target": {"ref": "<id>"}`
		if err.Error() != want {
			t.Errorf("%s:\n got %q\nwant %q", n, err, want)
		}
	}
}
