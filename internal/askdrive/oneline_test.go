package askdrive

import (
	"context"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/askq"
)

// ONE-LINE BOXED QUESTIONS. Claude Code boxes a question by its LENGTH (over
// 80 characters: gutterOver), not by whether it wraps — so in a pane wider
// than the question, a boxed question is ONE line, "│ " and no right edge.
// The boxrule-* and oneline-* testdata screens are REAL: tmux capture-pane -p
// -J of Claude Code 2.1.289 in a scratch session on a private tmux server,
// synthetic text, at the width the name ends in. The live refusals were
// exactly this: "the screen asks "│ Ship …?", not "Ship …?"".

var (
	// The 80/81 boundary, and a 114-character question: boxrule-*-100/-160.
	ob80 = askq.Question{Question: "Should the nightly report also be sent to the shared channel as a short summary?", Header: "Short",
		Options: []askq.Option{{Label: "Yes", Description: "Send it"}, {Label: "No", Description: "Skip it"}}}
	ob81 = askq.Question{Question: "Should the nightly report also be sent to the shared channel as a longer summary?", Header: "Longer",
		Options: []askq.Option{{Label: "Yes", Description: "Send it"}, {Label: "No", Description: "Skip it"}}}
	ob114 = askq.Question{Question: "Should the weekly digest of the widget package changes go out on Monday mornings, or on Friday afternoons instead?", Header: "Digest",
		Options: []askq.Option{{Label: "Monday", Description: "Start of week"}, {Label: "Friday", Description: "End of week"}}}
	boxrule3 = []askq.Question{ob80, ob81, ob114}
	// A two-question ask, BOTH questions boxed on one line (oneline-*-100).
	obReport = askq.Question{Question: ob81.Question, Header: "Report", Options: ob81.Options}
	obNotes  = askq.Question{Question: "Should the release notes for the widget package also list every fixed issue by its number?", Header: "Notes",
		Options: []askq.Option{{Label: "List them", Description: "Every issue"}, {Label: "Summary", Description: "Just a count"}}}
	oneline2 = []askq.Question{obReport, obNotes}
)

// TestTheCLIBoxesByLength pins gutters against the real captures: a question
// is drawn "│ "-first exactly when gutters says so — 80 characters bare, 81
// and over boxed — at 100 and 160 columns. (An invariant of the rule against
// the CLI, not the regression test: that is the next one.)
func TestTheCLIBoxesByLength(t *testing.T) {
	for _, c := range []struct {
		file string
		q    askq.Question
	}{
		{"boxrule-80-100", ob80}, {"boxrule-81-100", ob81},
		{"boxrule-80-160", ob80}, {"boxrule-81-160", ob81}, {"boxrule-114-160", ob114},
		{"oneline-q1-100", obReport}, {"oneline-q2-100", obNotes},
	} {
		scr := fixture(t, c.file)
		bare, boxed := strings.Contains(scr, "\n"+c.q.Question+"\n"), strings.Contains(scr, "\n│ "+c.q.Question+"\n")
		if bare == boxed {
			t.Fatalf("%s: the question is not on one line, bare or boxed", c.file)
		}
		if boxed != gutters(c.q.Question) {
			t.Errorf("%s: the CLI boxed=%v a %d-character question, gutters says %v", c.file, boxed, len(c.q.Question), gutters(c.q.Question))
		}
	}
}

// TestAOneLineBoxedQuestionIsReadAsItsText is the live failure: a question
// boxed on one line, as question 1 and as question 2, read with the "│ " off
// — and its control, the same screen is not a neighbouring state.
func TestAOneLineBoxedQuestionIsReadAsItsText(t *testing.T) {
	cases := []struct {
		file    string
		qs      []askq.Question
		want    Want
		answers map[string]string
	}{
		{"oneline-q1-100", oneline2, Want{Kind: Question, Q: 0, Cursor: 1}, nil},
		{"oneline-q2-100", oneline2, Want{Kind: Question, Q: 1, Cursor: 1}, nil},
		{"oneline-review-100", oneline2, Want{Kind: Review}, map[string]string{obReport.Question: "No", obNotes.Question: "Summary"}},
		{"boxrule-80-100", boxrule3, Want{Kind: Question, Q: 0, Cursor: 1}, nil},
		{"boxrule-81-100", boxrule3, Want{Kind: Question, Q: 1, Cursor: 1}, nil},
		{"boxrule-80-160", boxrule3, Want{Kind: Question, Q: 0, Cursor: 1}, nil},
		{"boxrule-81-160", boxrule3, Want{Kind: Question, Q: 1, Cursor: 1}, nil},
		{"boxrule-114-160", boxrule3, Want{Kind: Question, Q: 2, Cursor: 1}, nil},
		{"boxrule-review-160", boxrule3, Want{Kind: Review}, map[string]string{ob80.Question: "No", ob81.Question: "No", ob114.Question: "Friday"}},
	}
	for _, c := range cases {
		s := Parse(fixture(t, c.file))
		if err := Match(s, c.want, c.qs, c.answers); err != nil {
			t.Errorf("%s: %v\n  parsed %q", c.file, err, s.Question)
		}
		if strings.ContainsAny(s.Question, vbars) {
			t.Errorf("%s: the box is still in the question: %q", c.file, s.Question)
		}
		if c.want.Kind == Question {
			other := c.want
			other.Cursor = 2
			if Match(s, other, c.qs, nil) == nil {
				t.Errorf("%s: also matched %s", c.file, other)
			}
		}
	}
}

// TestAOneLineBoxedAskIsDrivenOnTheRealScreens walks two real asks through
// Drive, screen by screen as the CLI drew them after each key, choosing the
// SECOND option every time: both questions boxed on one line (100 columns),
// and the 80/81/114 boundary ask (160 columns). Every key is sent, the
// submit last.
func TestAOneLineBoxedAskIsDrivenOnTheRealScreens(t *testing.T) {
	for _, c := range []struct {
		name    string
		qs      []askq.Question
		screens []string
		keys    []string
	}{
		{"two one-line boxed questions", oneline2, []string{"oneline-q1-100", "oneline-q2-100", "oneline-review-100", "single-done"}, []string{"2", "2", "1"}},
		{"bare, boxed, boxed", boxrule3, []string{"boxrule-80-160", "boxrule-81-160", "boxrule-114-160", "boxrule-review-160", "single-done"}, []string{"2", "2", "2", "1"}},
	} {
		var scr []string
		for _, f := range c.screens {
			scr = append(scr, fixture(t, f))
		}
		sp := &scriptPane{screens: scr}
		cs := make([]Choice, len(c.qs))
		for i := range cs {
			cs[i] = Choice{Picks: []int{1}}
		}
		if err := Drive(context.Background(), sp, c.qs, cs, fast); err != nil {
			t.Fatalf("%s: %v (sent %q)", c.name, err, sp.sent)
		}
		if strings.Join(sp.sent, " ") != strings.Join(c.keys, " ") {
			t.Fatalf("%s: sent %q, want %q", c.name, sp.sent, c.keys)
		}
	}
}

// TestAOneLineBoxedAskIsDrivenThroughTheFake: the same, through the fake in a
// 160-column pane (it boxes by the CLI's rule, so its questions are one line
// boxed — checked), the second option of each question, and the answers it
// submitted are the ones asked for.
func TestAOneLineBoxedAskIsDrivenThroughTheFake(t *testing.T) {
	for _, qs := range [][]askq.Question{oneline2, boxrule3} {
		p := newFakePickerWidth(qs, 160)
		// The control: question 1 is drawn as the CLI draws it.
		q0 := qs[0].Question
		drawn := "\n" + q0 + "\n"
		if gutters(q0) {
			drawn = "\n│ " + q0 + "\n"
		}
		if !strings.Contains(p.Screen(), drawn) {
			t.Fatalf("the fake did not draw %q on one line as %q:\n%s", q0, drawn, p.Screen())
		}
		fp := &fakePane{p: p}
		cs := make([]Choice, len(qs))
		for i := range cs {
			cs[i] = Choice{Picks: []int{1}}
		}
		if err := Drive(context.Background(), fp, qs, cs, fast); err != nil {
			t.Fatalf("%v\nkeys %q\nscreen:\n%s", err, fp.sent, p.Screen())
		}
		got, cancelled, done := p.Result()
		if !done || cancelled {
			t.Fatalf("not submitted: %v", got)
		}
		for _, q := range qs {
			if got[q.Question] != q.Options[1].Label {
				t.Fatalf("%q answered %q, want %q", q.Question, got[q.Question], q.Options[1].Label)
			}
		}
	}
}

// TestABarOfItsOwnIsKeptByTheLengthRule: one line, only a left "│ " — the
// box exactly when the text after it is long enough to be boxed (over 80),
// else the question's own text, bar and all. The boundary both ways.
func TestABarOfItsOwnIsKeptByTheLengthRule(t *testing.T) {
	t80, t81 := strings.Repeat("x", 80), strings.Repeat("x", 81)
	if got, _ := unbox([]string{"│ " + t81}); got[0] != t81 {
		t.Errorf("an 81-character text behind a bar kept it: %q", got[0])
	}
	if got, _ := unbox([]string{"│ " + t80}); got[0] != "│ "+t80 {
		t.Errorf("an 80-character text behind a bar lost it: %q", got[0])
	}
	// A long question whose own text starts "│ " is boxed in turn: "│ │ …".
	if got, _ := unbox([]string{"│ │ " + t80}); got[0] != "│ "+t80 {
		t.Errorf("a boxed question starting with its own bar: %q", got[0])
	}
}
