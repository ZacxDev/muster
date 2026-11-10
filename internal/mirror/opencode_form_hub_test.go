package mirror_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

// THE HUB'S OWN CHECKS ON AN OPENCODE FORM (opencode_form.go), isolated from
// the host's: the host here is a raw socket that checks nothing, so what
// reaches it is exactly what the hub let through.

// formHost lists work:@1 waiting on question FixtureQuestionsForm,
// advertising forms and answers.
func formHost(t *testing.T) (*rig, *rawHost) {
	t.Helper()
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "h")
	m := winHello("h", true, mirror.Window{Session: "work", ID: "@1", Index: 0, Active: true, Runtime: mirror.RuntimeOpenCode,
		AgentSession: ocSendSID, Status: mirror.StatusWaiting, StatusSince: 9,
		Pending: &mirror.Pending{Kind: mirror.PendingQuestion, Summary: "Which database?", PromptID: opencodetest.FixtureQuestionsForm}})
	m.StatusReport, m.OpenCodeAnswer, m.OpenCodeForms = true, true, true
	raw, err := r.rawConnect(t, tok, m)
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "h", "work")
	return r, raw
}

// TestTheHubNeverAsksForOrAnswersAnotherAsk: a form read or an answer naming
// an ask the window does not wait on NOW never reaches the host; the ask it
// does wait on is read, and answered with exactly the picks given.
func TestTheHubNeverAsksForOrAnswersAnotherAsk(t *testing.T) {
	r, raw := formHost(t)
	ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
	defer cancel()
	if _, err := r.svc.OpenCodeForm(ctx, "h", "work", "@1", opencodetest.FixtureProceedForm); !errors.Is(err, mirror.ErrOpenCodeAskGone) {
		t.Fatalf("a form read for another ask: %v", err)
	}
	other := fixtureForm()
	other.PromptID = opencodetest.FixtureProceedForm
	ok := []mirror.OpenCodePick{{Key: "q0", Picks: []int{0}}, {Key: "q1", Picks: []int{0}}}
	if err := r.svc.AnswerOpenCodeForm(ctx, "h", "work", "@1", other, ok); !errors.Is(err, mirror.ErrOpenCodeAskGone) {
		t.Fatalf("an answer for another ask: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := append(raw.of(mirror.TypeOpenCodeForm), raw.of(mirror.TypeAnswer)...); len(got) != 0 {
		t.Fatalf("the host received %+v", got)
	}
	// The control.
	go func() {
		eventually(t, "the form request", func() bool { return len(raw.of(mirror.TypeOpenCodeForm)) == 1 })
		f := fixtureForm()
		raw.write(t, mirror.Msg{Type: mirror.TypeOpenCodeFormed, ID: raw.of(mirror.TypeOpenCodeForm)[0].ID, Form: &f})
		eventually(t, "the answer", func() bool { return len(raw.of(mirror.TypeAnswer)) == 1 })
		raw.write(t, mirror.Msg{Type: mirror.TypeAnswered, ID: raw.of(mirror.TypeAnswer)[0].ID})
	}()
	f, err := r.svc.OpenCodeForm(ctx, "h", "work", "@1", opencodetest.FixtureQuestionsForm)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.svc.AnswerOpenCodeForm(ctx, "h", "work", "@1", f, ok); err != nil {
		t.Fatal(err)
	}
	a := raw.of(mirror.TypeAnswer)[0]
	if a.OpenCodeReply == nil || a.OpenCodeReply.PromptID != opencodetest.FixtureQuestionsForm || a.OpenCodeReply.Choice != "" ||
		len(a.OpenCodeReply.Picks) != 2 || a.OpenCodeReply.Picks[0].Key != "q0" || a.Window != "@1" {
		t.Fatalf("the answer sent: %+v (%+v)", a, a.OpenCodeReply)
	}
}

// TestAFormForAnotherAskIsNeverDrawnAsThisOne: a host answering a form read
// with ANOTHER ask's form is refused by the hub.
func TestAFormForAnotherAskIsNeverDrawnAsThisOne(t *testing.T) {
	r, raw := formHost(t)
	go func() {
		eventually(t, "the form request", func() bool { return len(raw.of(mirror.TypeOpenCodeForm)) == 1 })
		f := fixtureForm()
		f.PromptID = opencodetest.FixtureProceedForm
		raw.write(t, mirror.Msg{Type: mirror.TypeOpenCodeFormed, ID: raw.of(mirror.TypeOpenCodeForm)[0].ID, Form: &f})
	}()
	ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
	defer cancel()
	if _, err := r.svc.OpenCodeForm(ctx, "h", "work", "@1", opencodetest.FixtureQuestionsForm); !errors.Is(err, mirror.ErrOpenCodeAskGone) {
		t.Fatalf("another ask's form was accepted: %v", err)
	}
}
