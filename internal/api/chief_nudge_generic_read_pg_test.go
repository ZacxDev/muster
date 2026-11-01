package api

import (
	"html"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefnudge"
)

// THE GENERIC ASK SHOWS THE CHIEF'S READ (chief_nudge.go, chiefnudge
// GenericAskRead): an ask-stall the chief would leave raises the generic ask,
// and that ask now carries the chief's OWN reason — never the classifier
// prefix muster builds around it in the row's reason — cut to a sentence or
// two. With no reason it is the generic ask exactly as before.

const (
	genericAskHead = "A session (Gold:1 · tmux main:1 @ ws) is waiting on you. Its last turn looks like it asked you something."
	genericAskTail = "\n\nChoosing an option sends nothing into the session; an answer you type is sent into it word for word."
)

// genericAsk runs one live pass on an ask-stall the chief leaves with reply,
// and returns the decision and the attention ask it raised.
func genericAsk(t *testing.T, reply string) (*nudgeFixture, chiefnudge.Decision, attention.Ask) {
	t.Helper()
	f := newNudgeFixture(t, true)
	j := withJev(t, f)
	j.set(200, 0.90, 0.05, 0.05)
	f.gw.set(reply)
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d", n)
	}
	d := f.one(t)
	a, err := f.attn.Get(asUser(authz.OperatorUserID), d.AskID)
	if err != nil || d.Outcome != chiefnudge.OutcomeAsked || strings.Join(a.Options, "|") != "Open session|Dismiss" {
		t.Fatalf("not the generic ask: %+v %+v %v", d, a, err)
	}
	return f, d, a
}

// TestChiefNudgeTheGenericAskShowsTheChiefsRead pins the whole ask, its row,
// and both places the owner reads it: the ask's page and the Attention list.
func TestChiefNudgeTheGenericAskShowsTheChiefsRead(t *testing.T) {
	const read = "It finished its draft and waits on the owner to review it, plus two sign-ups."
	f, d, a := genericAsk(t, `{"decision":"leave","reason":"`+read+`"}`)
	if want := genericAskHead + "\n\nThe chief's read: " + read + genericAskTail; a.Question != want {
		t.Fatalf("the ask:\n got %q\nwant %q", a.Question, want)
	}
	if want := "A session is waiting on you. Its last turn looks like it asked you something.\n\nThe chief's read: " + read; d.Question != want {
		t.Fatalf("the row's question:\n got %q\nwant %q", d.Question, want)
	}
	if strings.Contains(a.Question, "classifier") || strings.Contains(d.Question, "classifier") {
		t.Fatalf("muster's classifier prefix reached the ask: %q", a.Question)
	}
	if !strings.HasPrefix(d.Reason, "classifier: needs_input") {
		t.Fatalf("the row's reason lost muster's prefix (the control): %q", d.Reason)
	}
	for _, path := range []string{"/attention/asks/" + strconv.FormatInt(d.AskID, 10), "/attention"} {
		if body := f.page(t, path); !strings.Contains(body, html.EscapeString(read)) || !strings.Contains(body, "read: "+html.EscapeString(read)) {
			t.Fatalf("%s does not show the chief's read", path)
		}
	}
}

// TestChiefNudgeTheGenericAskWithNoReasonIsUnchanged: no reason, no line —
// the generic ask word for word as it was.
func TestChiefNudgeTheGenericAskWithNoReasonIsUnchanged(t *testing.T) {
	_, d, a := genericAsk(t, `{"decision":"leave","reason":"   "}`)
	if want := genericAskHead + genericAskTail; a.Question != want {
		t.Fatalf("the ask:\n got %q\nwant %q", a.Question, want)
	}
	if want := "A session is waiting on you. Its last turn looks like it asked you something."; d.Question != want {
		t.Fatalf("the row's question: %q", d.Question)
	}
}

// TestChiefNudgeTheGenericAsksReadIsCapped: a reason far over the cap is cut
// to 300 runes (299 and an ellipsis) on the ask.
func TestChiefNudgeTheGenericAsksReadIsCapped(t *testing.T) {
	long := strings.Repeat("waiting ", 120) // 960 runes
	_, _, a := genericAsk(t, `{"decision":"leave","reason":"`+long+`"}`)
	cutRead := string([]rune(strings.TrimSpace(long))[:299]) + "…"
	if want := genericAskHead + "\n\nThe chief's read: " + cutRead + genericAskTail; a.Question != want {
		t.Fatalf("the capped ask:\n got %q\nwant %q", a.Question, want)
	}
	if n := utf8.RuneCountInString(a.Question); n > attention.MaxQuestionRunes {
		t.Fatalf("the ask is %d runes, over %d", n, attention.MaxQuestionRunes)
	}
}
