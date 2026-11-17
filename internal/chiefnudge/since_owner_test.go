package chiefnudge

import (
	"testing"
	"time"
)

// SinceOwner, the fleet's loop counter: the owner's newest own message, and
// how many of muster's own pastes came after it — by each of the three ways a
// paste is told apart (the injected set, a prefix, a fence), each counted
// once. Invented texts and times.
func TestSinceOwnerCountsMustersTextsAfterTheOwnersNewest(t *testing.T) {
	at := func(m int) time.Time { return time.Date(2000, 1, 2, 3, m, 0, 0, time.UTC) }
	fenced := "<<<routed untrusted nonce=0123456789abcdef count=1>>>\nreview the loader\n<<<end>>>"
	msgs := []Msg{
		{Role: "user", Text: "start the loader fix", ID: "line:1", At: at(1)},
		{Role: "assistant", Text: "on it"},
		{Role: "user", Text: "[Chief] earlier nudge", ID: "line:2", At: at(2)},
		{Role: "user", Text: "now the parser too", ID: "line:3", At: at(3)},
		{Role: "assistant", Text: "done, shall I continue?"},
		{Role: "user", Text: "exactly a stored paste", ID: "line:4", At: at(4)},
		{Role: "user", Text: "[Chief] go on", ID: "line:5", At: at(5)},
		{Role: "user", Text: fenced, ID: "line:6", At: at(6)},
		{Role: "user", Text: "an untimed line", ID: "line:7"},
		{Role: "user", Text: "   "},
	}
	last, ok, after := SinceOwner(msgs, []string{" exactly a stored paste "}, []string{"[Chief]"})
	if !ok || last.ID != "line:3" || !last.At.Equal(at(3)) || after != 3 {
		t.Fatalf("SinceOwner = %+v, %v, %d; want line:3 at 03:03 and 3 after it", last, ok, after)
	}
	// No own message read: every paste read is counted, and ok is false.
	if _, ok, after := SinceOwner(msgs[5:8], []string{"exactly a stored paste"}, []string{"[Chief]"}); ok || after != 3 {
		t.Fatalf("no own message: ok %v after %d; want false, 3", ok, after)
	}
	// The owner speaking last resets the count.
	tail := append(append([]Msg{}, msgs...), Msg{Role: "user", Text: "stop there", ID: "line:9", At: at(9)})
	if last, ok, after := SinceOwner(tail, []string{"exactly a stored paste"}, []string{"[Chief]"}); !ok || last.ID != "line:9" || after != 0 {
		t.Fatalf("owner last: %+v, %v, %d; want line:9 and 0", last, ok, after)
	}
}
