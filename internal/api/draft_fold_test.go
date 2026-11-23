package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
)

// TestADraftsRequestsFoldIntoOneItemCountedOnce: two open requests for one
// draft are ONE item, the newest; a changed card and another draft's request
// stay their own. THE ONE OPEN COUNT counts a request beside its draft's item
// once, and counts it when the list (a filter) keeps the request but not the
// draft.
func TestADraftsRequestsFoldIntoOneItemCountedOnce(t *testing.T) {
	req := func(ask, draft int64, changed bool) attentionItem {
		return attentionItem{Kind: attentionKindAsk, Key: attentionAskKey(ask), AskID: ask, systemAsk: true,
			stepUp: &attention.StepUpCard{Tool: pluginDraftDecideTool, DraftID: draft, Changed: changed}}
	}
	draft := attentionItem{Kind: attentionKindPlugin, Key: draftKey(52)}
	other := attentionItem{Kind: attentionKindAsk, Key: attentionAskKey(8101), AskID: 8101}
	items := []attentionItem{other, req(8103, 52, false), draft, req(8107, 52, false), req(8105, 52, true), req(8109, 61, false)}
	got := foldDraftRequests(items)
	var keys []string
	for _, it := range got {
		keys = append(keys, it.Key)
	}
	want := []string{"ask-8101", "draft-52", "ask-8107", "ask-8105", "ask-8109"}
	if len(keys) != len(want) {
		t.Fatalf("folded %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("folded %v, want %v", keys, want)
		}
	}
	// other, draft 52 (its request uncounted), the changed card, draft 61's request.
	if n := asksPendingOf(context.Background(), got, 0); n != 4 {
		t.Errorf("the count is %d, want 4", n)
	}
	if n := asksPendingOf(context.Background(), []attentionItem{req(8107, 52, false)}, 0); n != 1 {
		t.Errorf("a request kept without its draft counts %d, want 1", n)
	}
	if n := asksPendingOf(context.Background(), []attentionItem{req(8107, 52, false), draft}, 0); n != 1 {
		t.Errorf("a request beside its draft counts %d, want 1", n)
	}
}

// TestFoldedRequestsAreEachDrawnWhenTheirDraftIsNot (audit G1): two requests
// for one draft fold into one item, but when that item cannot be drawn as its
// draft's card (no readable, pending draft: stepUpDraft nil) the feed draws
// every request as its own card — none is hidden.
//
// RED AT 17f69e4: the older request was dropped and never drawn.
func TestFoldedRequestsAreEachDrawnWhenTheirDraftIsNot(t *testing.T) {
	req := func(ask int64) attentionItem {
		return attentionItem{Kind: attentionKindAsk, Key: attentionAskKey(ask), AskID: ask, systemAsk: true, Text: "Post it?",
			stepUp: &attention.StepUpCard{Tool: pluginDraftDecideTool, Title: "Post it? · Draft #52", Why: "W", Preview: "P", DraftID: 52}}
	}
	feed, _ := askFeedHTML(foldDraftRequests([]attentionItem{req(8103), req(8107)}), "", 0, time.Now())
	for _, id := range []string{"8103", "8107"} {
		if n := strings.Count(feed, `<li id="ask-`+id+`"`); n != 1 {
			t.Errorf("request %s is drawn %d times, want once:\n%s", id, n, feed)
		}
	}
}

// TestAnExpiredUnsweptRequestIsNotADraftsRequest (audit G3): an open ask past
// its expiry that no sweep has moved is closed, as the store reads it.
//
// RED AT 17f69e4: it was read as an open request.
func TestAnExpiredUnsweptRequestIsNotADraftsRequest(t *testing.T) {
	a := attention.Ask{ID: 731, Status: attention.Open, ExpiresAt: time.Now().Add(-time.Minute), Asker: attention.Asker{System: true, Label: hostDecisionLabel},
		StepUp: &attention.StepUpCard{Tool: pluginDraftDecideTool, DraftID: 52}}
	if r := draftRequestOf(a); r != nil {
		t.Errorf("an expired open ask is a request: %+v", r)
	}
	a.ExpiresAt = time.Now().Add(time.Hour)
	if r := draftRequestOf(a); r == nil {
		t.Error("control: an unexpired open ask is not a request")
	}
}
