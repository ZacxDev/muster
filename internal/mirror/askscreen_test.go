package mirror

import (
	"errors"
	"strings"
	"testing"
)

// TestAnAnsweredRefusalCarriesTheHostsScreen: the host's Msg.Screen reaches
// AnswerWindow's caller (AskScreen), capped from the bottom at a line start,
// alongside the keys and the refusal's kind; a typed answer carries none.
func TestAnAnsweredRefusalCarriesTheHostsScreen(t *testing.T) {
	hc := &hostConn{answers: map[uint32]chan error{}}
	ch := make(chan error, 1)
	hc.answers[7] = ch
	big := "top\n" + strings.Repeat("y", MaxAskScreen) + "\n❯ 1. Red"
	hc.answered(7, "lost it", AskRefusalLost, 3, big)
	err := <-ch
	if !errors.Is(err, ErrAskLost) || AskKeys(err) != 3 {
		t.Fatalf("err %v keys %d", err, AskKeys(err))
	}
	if got := AskScreen(err); got != AskScreenCutMark+"❯ 1. Red" {
		t.Fatalf("screen %q, want the last line (a cut is at a line start)", got)
	}
	small := make(chan error, 1)
	hc.answers[8] = small
	hc.answered(8, "", "", 0, "ignored")
	if err := <-small; err != nil || AskScreen(err) != "" {
		t.Fatalf("a typed answer: %v %q", err, AskScreen(err))
	}
	if got := CapAskScreen("a\nb"); got != "a\nb" {
		t.Fatalf("a small screen is cut: %q", got)
	}
}
