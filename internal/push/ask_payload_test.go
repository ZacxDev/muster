package push

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// An attention ask's chips in payload v2 (PayloadV2.Ask): the phone's only,
// whole or absent, and the first thing dropped when a push would not fit.

// TestAnAsksChipsReachOnlyTheNativePayload: a native device gets the ask's
// id, options and suggestions (at most MaxNativeChips of each, a chip over
// MaxNativeChip runes left out WHOLE, never cut) and the update flag; the
// browser's JSON carries none of them. RED at origin/main (no `ask` at all).
func TestAnAsksChipsReachOnlyTheNativePayload(t *testing.T) {
	long := strings.Repeat("l", MaxNativeChip+1)
	p := Payload{Type: "attention", ID: "ask-9", Tag: "attention-ask-9", Title: "chief is asking", Body: "Ship?\nOptions: a · b",
		Data: map[string]string{"url": "/attention/open/ask-9"}, Update: true,
		Ask: &V2Ask{ID: 9, Options: []string{"a", long, "b", "c", "d"}, Suggestions: []string{"s1", "s2"}}}
	sent := fanOut(t, p)
	var v map[string]any
	if err := json.Unmarshal([]byte(sent[epNtfy]), &v); err != nil {
		t.Fatal(err)
	}
	ask, _ := json.Marshal(v["ask"])
	if string(ask) != `{"id":9,"options":["a","b","c"],"suggestions":["s1","s2"]}` || v["update"] != true {
		t.Fatalf("native ask = %s, update = %v", ask, v["update"])
	}
	for _, ep := range []string{epFCM} {
		if strings.Contains(sent[ep], `"ask"`) || strings.Contains(sent[ep], `"update"`) || strings.Contains(sent[ep], "s1") {
			t.Fatalf("the browser payload carries the native extras: %s", sent[ep])
		}
	}
	// The control: without Ask/Update the native payload has neither key.
	plain := fanOut(t, Payload{Type: "attention", ID: "ask-9", Tag: "t", Title: "x"})
	if strings.Contains(plain[epNtfy], `"ask"`) || strings.Contains(plain[epNtfy], `"update"`) {
		t.Fatalf("CONTROL: a plain payload carries %s", plain[epNtfy])
	}
}

// TestTheWorstCaseAskStillFitsWithItsQuestion: every line at its bound and
// every character the costliest JSON escape (`<` is six bytes, <): the
// native payload still fits NativeMaxPlaintext, its title and body WHOLE (the
// question is never what the fit drops), the suggestions dropped first and
// then the options. Two measured points: the worst case (both dropped), and
// one where dropping the suggestions alone is enough (options kept).
func TestTheWorstCaseAskStillFitsWithItsQuestion(t *testing.T) {
	lt := func(n int) string { return strings.Repeat("<", n) }
	chip := []string{lt(MaxNativeChip), lt(MaxNativeChip - 1), lt(MaxNativeChip - 2)}
	worst := Payload{Type: "attention", ID: "ask-123456789", Tag: "attention-ask-123456789", Title: lt(MaxNativeTitle),
		Body: lt(MaxNativeBody), Data: map[string]string{"url": "/attention/open/ask-123456789"}, Update: true,
		Ask: &V2Ask{ID: 123456789, Options: chip, Suggestions: chip}}
	b, err := nativeBody(worst)
	if err != nil {
		t.Fatalf("the worst case does not fit: %v", err)
	}
	var v PayloadV2
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if len(b) > NativeMaxPlaintext || v.Title != worst.Title || v.Body != worst.Body || v.Ask == nil || v.Ask.ID != 123456789 ||
		len(v.Ask.Suggestions) != 0 || len(v.Ask.Options) != 0 {
		t.Fatalf("worst case: %d bytes, title %d, body %d runes, ask %+v", len(b), utf8.RuneCountInString(v.Title), utf8.RuneCountInString(v.Body), v.Ask)
	}
	// Dropping the suggestions alone fits: the options stay.
	mid := worst
	mid.Body = lt(100)
	mid.Ask = &V2Ask{ID: 1, Options: chip, Suggestions: chip}
	b, err = nativeBody(mid)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Ask.Suggestions) != 0 || len(v.Ask.Options) != 3 || v.Body != mid.Body {
		t.Fatalf("the middle case dropped the wrong chips: %+v", v.Ask)
	}
	// The control: a payload that fits keeps every chip.
	small := mid
	small.Ask = &V2Ask{ID: 1, Options: []string{"a"}, Suggestions: []string{"s"}}
	b, _ = nativeBody(small)
	if !strings.Contains(string(b), `"options":["a"],"suggestions":["s"]`) {
		t.Fatalf("CONTROL: a small ask lost its chips: %s", b)
	}
}
