package api

import (
	"html"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// TestTheOpenCodeRelayFormConfirmsInPlace: relaying an answer into an opencode
// window from the Asks list is step-up gated (handleAttentionOpenCodeAnswer),
// so its form is marked data-step-up-form with its own sentence — on the Asks
// page, which always draws the dialog (renderAsksPage), it confirms in place
// and the tapped choice (a name=choice button) is kept by the form script
// (e2e/stepup-form.test.mjs).
//
// 🔴 RED at ea84f2ab: the form was unmarked, and a stale relay answered into
// a 403 page with no confirm.
func TestTheOpenCodeRelayFormConfirmsInPlace(t *testing.T) {
	it := attentionItem{card: attentionCard{ref: ui.MirrorRef{Host: "build-01", Session: "main", Window: "@2"}},
		openCode: &openCodeAsk{kind: mirror.PendingPermission, promptID: "per_0001", choices: []string{"once", "reject"}}}
	body := openCodeAskHTML(it)
	tag := formTag(body, attentionOpenCodePath)
	if tag == "" {
		t.Fatalf("CONTROL: no relay form drawn:\n%s", body)
	}
	if !strings.Contains(tag, "data-step-up-form") || !strings.Contains(tag, `data-step-up-why="`+html.EscapeString(OpenCodeAnswerWhy())+`"`) {
		t.Errorf("the relay form is not marked data-step-up-form with its sentence: %s", tag)
	}
}
