package main

import (
	"errors"
	"fmt"
	"testing"
)

// TestAFailureAfterThePasteIsNeverNotReady: muster re-sends a `503 not_ready`
// because it promises nothing was pasted. A pane that dies between the paste and
// its Enter must therefore NOT surface as errPaneNotLive (which the server maps
// to not_ready), or the re-send pastes the prompt twice.
func TestAFailureAfterThePasteIsNeverNotReady(t *testing.T) {
	enterErr := fmt.Errorf("%w: pane %%3 is dead", errPaneNotLive)
	got := afterPaste(enterErr)
	if errors.Is(got, errPaneNotLive) {
		t.Fatalf("a post-paste failure still reads as errPaneNotLive (-> not_ready): %v", got)
	}
	if !errors.Is(got, errPastedNotSubmitted) {
		t.Fatalf("a post-paste failure is not marked as pasted-not-submitted: %v", got)
	}
	if afterPaste(nil) != nil {
		t.Fatal("a successful Enter became an error")
	}
	// Control: the pre-paste check's own failure IS errPaneNotLive.
	if !errors.Is(enterErr, errPaneNotLive) {
		t.Fatal("control: the fixture is not errPaneNotLive")
	}
}
