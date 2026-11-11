package askdrive

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/ZacxDev/muster/internal/askq"
)

// A TRY AGAIN THAT RESETS THE PICKER (askq.Progress.Reset; resetSteps): a
// card's retry after a recoverable failure re-reads the screen and, when the
// picker still shows its first question with only the cursor moved (or, on a
// multi-select question, options ticked), puts it back to the first option and
// types the answer again. The control: the same picker, without Reset, is
// refused as touched — so it is the reset that answers it.

// movedCursor is a picker on color whose cursor someone (or muster's own lost
// keys) moved to row 3.
func movedCursor(t *testing.T) *FakePicker {
	t.Helper()
	p := NewFakePicker([]askq.Question{color})
	p.Type(KeyDown + KeyDown)
	if err := Match(Parse(p.Screen()), Want{Kind: Question, Q: 0, Cursor: 3}, []askq.Question{color}, nil); err != nil {
		t.Fatalf("the fixture: %v\n%s", err, p.Screen())
	}
	return p
}

func TestRetryResetsAMovedCursorThenTypes(t *testing.T) {
	qs, cs := []askq.Question{color}, []Choice{{Picks: []int{1}}}
	// Control: without Reset the moved picker is refused as touched, nothing
	// typed.
	ctl := &fakePane{p: movedCursor(t)}
	if _, err := DriveFrom(context.Background(), ctl, qs, cs, nil, fast); !errors.Is(err, ErrNotUntouched) || len(ctl.sent) != 0 {
		t.Fatalf("control: %v, sent %q", err, ctl.sent)
	}
	p := movedCursor(t)
	fp := &fakePane{p: p}
	keys, err := DriveFrom(context.Background(), fp, qs, cs, &Progress{Reset: true}, fast)
	if err != nil {
		t.Fatalf("the reset retry: %v (sent %q)\n%s", err, fp.sent, p.Screen())
	}
	if want := []string{KeyUp, KeyUp, "2"}; !slices.Equal(fp.sent, want) {
		t.Fatalf("sent %q, want %q", fp.sent, want)
	}
	if keys != 1 {
		t.Fatalf("reported %d keys of the plan, want 1 (the reset's own are not the plan's)", keys)
	}
	if answers, cancelled, done := p.Result(); !done || cancelled || !reflect.DeepEqual(answers, map[string]string{color.Question: "Blue"}) {
		t.Fatalf("submitted %v (cancelled %v)", answers, cancelled)
	}
}

// TestRetryUnticksAMultiSelectThenTypes: options ticked on a multi-select
// question are unticked by their digits, the cursor moved back, and the
// answer typed.
func TestRetryUnticksAMultiSelectThenTypes(t *testing.T) {
	qs, cs := []askq.Question{pets}, []Choice{{Picks: []int{2}}}
	p := NewFakePicker(qs)
	p.Type("1" + "2" + KeyDown)
	fp := &fakePane{p: p}
	if _, err := DriveFrom(context.Background(), fp, qs, cs, &Progress{Reset: true}, fast); err != nil {
		t.Fatalf("the reset retry: %v (sent %q)\n%s", err, fp.sent, p.Screen())
	}
	if want := []string{KeyUp, "2", "1"}; !slices.Equal(fp.sent[:3], want) {
		t.Fatalf("the reset sent %q, want %q first", fp.sent, want)
	}
	if answers, _, done := p.Result(); !done || !reflect.DeepEqual(answers, map[string]string{pets.Question: "Fish"}) {
		t.Fatalf("submitted %v", answers)
	}
}

// TestRetryLeavesTypedTextAndLaterQuestionsAlone: a picker with an answer of
// one's own typed in, or on a later question, is not reset: the drive refuses
// it, nothing sent.
func TestRetryLeavesTypedTextAndLaterQuestionsAlone(t *testing.T) {
	typed := NewFakePicker([]askq.Question{color})
	typed.Type("4" + "teal")
	later := NewFakePicker(three)
	later.Type("1")
	for name, c := range map[string]struct {
		p  *FakePicker
		qs []askq.Question
		cs []Choice
	}{
		"typed text":       {typed, []askq.Question{color}, []Choice{{Picks: []int{0}}}},
		"a later question": {later, three, threeChoices},
	} {
		fp := &fakePane{p: c.p}
		if _, err := DriveFrom(context.Background(), fp, c.qs, c.cs, &Progress{Reset: true}, fast); !errors.Is(err, ErrNotUntouched) || len(fp.sent) != 0 {
			t.Errorf("%s: %v, sent %q", name, err, fp.sent)
		}
	}
}

// TestRetryMovesOffTheTextRowBeforeUnticking: the audit's case (F1). A
// multi-select question with an option ticked and the cursor on the empty
// "Type something" row: there a digit is TEXT, so unticking first typed "1"
// into the box (the picker then read `4. [✔] 1`, the drive was lost, and
// nothing could recover it). The cursor moves up onto the first option first,
// then the tick is undone, then the answer is typed.
func TestRetryMovesOffTheTextRowBeforeUnticking(t *testing.T) {
	qs, cs := []askq.Question{pets}, []Choice{{Picks: []int{2}}}
	p := NewFakePicker(qs)
	p.Type("1" + KeyDown + KeyDown + KeyDown)
	if err := Match(Parse(p.Screen()), Want{Kind: Question, Q: 0, Cursor: 4, Checked: []int{1}}, qs, nil); err != nil {
		t.Fatalf("the fixture: %v\n%s", err, p.Screen())
	}
	fp := &fakePane{p: p}
	if _, err := DriveFrom(context.Background(), fp, qs, cs, &Progress{Reset: true}, fast); err != nil {
		t.Fatalf("the reset retry: %v (sent %q)\n%s", err, fp.sent, p.Screen())
	}
	if want := []string{KeyUp, KeyUp, KeyUp, "1"}; !slices.Equal(fp.sent[:4], want) {
		t.Fatalf("the reset sent %q, want %q first", fp.sent, want)
	}
	if answers, _, done := p.Result(); !done || !reflect.DeepEqual(answers, map[string]string{pets.Question: "Fish"}) {
		t.Fatalf("submitted %v", answers)
	}
}
