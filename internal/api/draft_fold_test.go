package api

import (
	"context"
	"testing"

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
