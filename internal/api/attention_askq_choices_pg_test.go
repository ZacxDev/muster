package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/askq"
)

// TestAHeldQuestionsChoicesAreInItsPushNotInAttentionList: one held question's
// choices are listed in its notification body, but NOT serialised as the
// item's `options` in attention_list — those are answers attention_answer
// takes, and a held question is answered on its question card. RED at #313's
// round-0 head (options serialised).
func TestAHeldQuestionsChoicesAreInItsPushNotInAttentionList(t *testing.T) {
	f := newAttnFixture(t)
	qs := []askq.Question{{Question: "Which database?", Options: []askq.Option{{Label: "postgres"}, {Label: "sqlite"}}}}
	if _, err := f.srv.Questions().Open(askq.Key{Owner: 1, Source: askSourceMirror, Scope: "ws", Session: focusSID, ToolUseID: "toolu_choices"},
		qs, askq.NewID()); err != nil {
		t.Fatal(err)
	}
	items, err := f.srv.attentionItems(asUser(1))
	if err != nil {
		t.Fatal(err)
	}
	var held *attentionItem
	for i := range items {
		if items[i].Kind == attentionKindQuestion {
			held = &items[i]
		}
	}
	if held == nil {
		t.Fatalf("no held question item: %+v", items)
	}
	if got := attentionPushBody(*held); got != "Which database?\nOptions: postgres · sqlite" {
		t.Fatalf("the push body: %q", got)
	}
	b, _ := json.Marshal(held)
	if strings.Contains(string(b), `"options"`) {
		t.Fatalf("attention_list serialises the held question's options: %s", b)
	}
}
