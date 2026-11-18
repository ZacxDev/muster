package push

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestButtonsCostTheChipsNothing: adding the signed-action buttons
// (AskActions) must not make a push drop chips the same push kept without them
// (#313's payload): each chip's text is in the push ONCE (`ask`), and its
// button names it by index. Measured at three points under a full title and
// body: three options and three suggestions of 60 CJK characters (the audit's
// case: #313 kept 3 suggestions, #314's round-1 head 0), of 130 CJK characters
// (3 bytes each, near the 140-character chip bound, so the push sits close to
// its 3993 bytes and even ONE repeat of each option's text overflows it), and
// three short ASCII ones (nothing dropped either way). RED at #314's round-1
// head, whose buttons repeated each chip's text twice (label and text).
func TestButtonsCostTheChipsNothing(t *testing.T) {
	cjk := func(seed rune, n int) string { return strings.Repeat(string(seed), n) }
	for name, chips := range map[string][2][]string{
		"60-rune CJK":  {{cjk('甲', 60), cjk('乙', 60), cjk('丙', 60)}, {cjk('丁', 60), cjk('戊', 60), cjk('己', 60)}},
		"130-rune CJK": {{cjk('甲', 130), cjk('乙', 130), cjk('丙', 130)}, {cjk('丁', 130), cjk('戊', 130), cjk('己', 130)}},
		"short ASCII":  {{"ship it", "wait", "ask ops"}, {"Ship after the dry run", "Wait for Monday", "Ask ops first"}},
	} {
		kept := func(actions bool) (int, int) {
			p := Payload{Type: "attention", Title: strings.Repeat("t", MaxNativeTitle), Body: strings.Repeat("問", MaxNativeBody), AskActions: actions,
				Ask: &V2Ask{ID: 41, Options: chips[0], Suggestions: chips[1]}}
			b, err := nativeBody(p)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			var v PayloadV2
			if err := json.Unmarshal(b, &v); err != nil {
				t.Fatal(err)
			}
			return len(v.Ask.Options), len(v.Ask.Suggestions)
		}
		bo, bs := kept(false)
		ho, hs := kept(true)
		if ho < bo || hs < bs {
			t.Errorf("%s: with buttons the push keeps %d options and %d suggestions; without, %d and %d", name, ho, hs, bo, bs)
		}
		if bo != 3 || bs != 3 {
			t.Errorf("CONTROL %s: the base payload itself kept only %d options and %d suggestions", name, bo, bs)
		}
	}
}
