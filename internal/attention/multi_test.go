package attention

import (
	"errors"
	"testing"
)

// TestAMultiSelectAskNeedsOptions: Multi means "the asker's options are
// CHECKBOXES" (migration 0175), so an ask with none has nothing to tick — its
// page would draw a lone note box and no Send — and Clean refuses it. The same
// ask WITH options is the control, and the refusal is about Multi alone: a
// plain single-select ask with no options is untouched.
func TestAMultiSelectAskNeedsOptions(t *testing.T) {
	asker := Asker{AgentID: 7}
	for _, c := range []struct {
		name string
		opts []string
	}{
		{"no options at all", nil},
		{"only blank options", []string{"  ", ""}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Clean(New{Asker: asker, Question: "q", Multi: true, Options: c.opts}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Clean: %v, want ErrInvalid", err)
			}
		})
	}
	n, err := Clean(New{Asker: asker, Question: "q", Multi: true, Options: []string{"a", "b"}})
	if err != nil || !n.Multi || len(n.Options) != 2 {
		t.Fatalf("a multi ask with options: multi=%v options=%v err=%v", n.Multi, n.Options, err)
	}
	if _, err := Clean(New{Asker: asker, Question: "q"}); err != nil {
		t.Fatalf("a single-select ask with no options: %v", err)
	}
}
