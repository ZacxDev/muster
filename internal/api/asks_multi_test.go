package api

import (
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/ui"
)

// MULTI-SELECT ASKS (migration 0175, attention.Ask.Multi): an asker may offer
// its options as CHECKBOXES — the owner picks any number — and may leave an
// optional note. The picks are recorded joined by "; " and the note appended
// after " — note: "; the single-select form and a lone free-text answer are
// byte-for-byte what they were.

// TestAMultiSelectAskDrawsCheckboxesAndOneSend: on the Asks feed's card, a
// multi-select ask draws a CHECKBOX per option and ONE Send — never one submit
// per option — plus the optional note box, and its Send is not gated on the
// free-text box (a pick with no text is a complete answer). The single-select
// form (the control) keeps its per-option buttons, its quiet gated Answer and
// no note box.
func TestAMultiSelectAskDrawsCheckboxesAndOneSend(t *testing.T) {
	it := attentionItem{Key: "ask-9", Kind: attentionKindAsk, AskID: 9, Text: "Which ones?",
		Options: []string{"alpha", "beta", "gamma"}, multi: true}
	form := askAnswerFormHTML(it, "9", askFormPlace{chips: true, gated: true})
	for _, want := range []string{
		`<input type="checkbox" name="option" value="alpha" class="` + ui.AsksOptionCheckBoxClass + `" data-attention-option>`,
		`<input type="checkbox" name="option" value="beta"`,
		`<input type="checkbox" name="option" value="gamma"`,
		`<button type="submit" id="answer-9-submit" class="` + ui.AsksCardSendClass + `">Send answer</button>`,
		`<textarea id="answer-9" name="answer"`,
		`<label for="note-9" class="` + ui.AsksNoteLabelClass + `">Note (optional)</label>`,
		`<textarea id="note-9" name="note" rows="1" placeholder="Add a note…" maxlength="` + strconv.Itoa(attention.MaxNoteRunes) + `" class="` + ui.AsksNoteClass + `" data-asks-note></textarea>`,
	} {
		if !strings.Contains(form, want) {
			t.Errorf("the multi-select answer form lacks %q:\n%s", want, form)
		}
	}
	if strings.Contains(form, `data-attention-option>alpha</button>`) {
		t.Errorf("the multi-select form still draws a submit button per option:\n%s", form)
	}
	if strings.Contains(form, "data-asks-send") {
		t.Errorf("the multi-select Send is gated on the free-text box; a pick alone must be sendable:\n%s", form)
	}

	// Control: the SAME options, single-select, are what they always were.
	single := it
	single.multi = false
	f := askAnswerFormHTML(single, "9", askFormPlace{chips: true, gated: true})
	for _, want := range []string{
		`<button type="submit" name="option" value="alpha" class="` + ui.AsksOptionClass + `" data-attention-option>alpha</button>`,
		`<button type="submit" id="answer-9-submit" class="` + ui.AsksCardSendQuietClass + ` ` + ui.AsksSendDisabledClass + `" aria-disabled="true" data-asks-send>Answer</button>`,
	} {
		if !strings.Contains(f, want) {
			t.Errorf("the single-select answer form lost %q:\n%s", want, f)
		}
	}
	for _, never := range []string{`type="checkbox"`, `name="note"`, `>Send answer</button>`} {
		if strings.Contains(f, never) {
			t.Errorf("the single-select form gained %q from the multi-select work:\n%s", never, f)
		}
	}
}

// TestTheAskPageOffersAMultiSelectCheckboxesAndNote: on the ask PAGE the
// options are checkboxes form-associated with the answer form (form="ask-answer"),
// one Send submits them, and the optional note is form-associated too — while
// the free-text row stays OPEN (a multi ask is never "quiet": the note belongs
// with it).
func TestTheAskPageOffersAMultiSelectCheckboxesAndNote(t *testing.T) {
	a := attention.Ask{ID: 61, Owner: 1, Asker: attention.Asker{AgentID: 7, Label: "Worker Seven"}, Question: "Which ones?",
		Options: []string{"alpha", "beta"}, Multi: true, Status: attention.Open, CreatedAt: time.Now()}
	s := whereServer(t, a)
	body := askPageBody(t, s, a, askPagePath(a.ID))
	for _, want := range []string{
		`<input type="checkbox" form="ask-answer" name="option" value="alpha" class="` + ui.AsksOptionCheckBoxClass + `" data-ask-option>`,
		`<button type="submit" form="ask-answer" class="` + ui.AsksMultiSendClass + `" data-ask-multi-send>Send answer</button>`,
		`<textarea id="ask-input-note" name="note" rows="1" form="ask-answer"`,
		`class="flex items-end gap-2" data-attention-answer>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the multi-select ask page lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "data-ask-other>") {
		t.Errorf("a multi-select ask collapsed its free-text row, hiding the note:\n%s", body)
	}
}

// TestAMultiSelectAnswersPicksAreJoinedAndItsNoteAppended: the answer posted
// from an ask's form — single-select, multi-select or free text — as
// answerFromForm reads it. Every value is a literal.
func TestAMultiSelectAnswersPicksAreJoinedAndItsNoteAppended(t *testing.T) {
	read := func(values url.Values) (string, bool) {
		r := httptest.NewRequest("POST", attentionPath+"/asks/9/answer", strings.NewReader(values.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return answerFromForm(r)
	}
	for _, c := range []struct {
		name   string
		in     url.Values
		want   string
		tapped bool
	}{
		{"a single option tapped", url.Values{"option": {"Deploy now"}}, "Deploy now", true},
		{"several boxes ticked", url.Values{"option": {"alpha", "beta", "gamma"}}, "alpha; beta; gamma", true},
		{"boxes ticked with a note", url.Values{"option": {"alpha", "beta"}, "note": {"use the staging key"}}, "alpha; beta — note: use the staging key", true},
		{"a note alone", url.Values{"note": {"just this"}}, "just this", false},
		{"free text alone", url.Values{"answer": {"ship it"}}, "ship it", false},
		{"an option wins over free text", url.Values{"option": {"alpha"}, "answer": {"ignored"}}, "alpha", true},
		{"blank picks fall back to free text", url.Values{"option": {"  ", ""}, "answer": {"ok"}}, "ok", false},
	} {
		got, tapped := read(c.in)
		if got != c.want || tapped != c.tapped {
			t.Errorf("%s: %q (tapped %v), want %q (tapped %v)", c.name, got, tapped, c.want, c.tapped)
		}
	}
}
