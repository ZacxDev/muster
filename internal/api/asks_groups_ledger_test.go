package api

import "testing"

// TestEveryAttentionKindHasAHome: a kind no group holds would vanish from the
// Ask tab silently. The ledger is every kind the queue can hold: each is in
// exactly one Ask-tab group, except the reply draft, whose home is the Reply
// drafts tab (askTabItems strips it from the Ask tab). A NEW kind with neither
// fails here.
func TestEveryAttentionKindHasAHome(t *testing.T) {
	held := map[string]string{attentionKindPlugin: "the Reply drafts tab"}
	for _, g := range askGroups {
		for _, k := range g.kinds {
			if prev, dup := held[k]; dup {
				t.Errorf("kind %q is in two groups (%s, %s)", k, prev, g.key)
			}
			held[k] = g.key
		}
	}
	kinds := []string{attentionKindAsk, attentionKindQuestion, attentionKindNotice, attentionKindSession, attentionKindTask, attentionKindPlugin}
	for _, k := range kinds {
		if held[k] == "" {
			t.Errorf("kind %q is in no group", k)
		}
	}
	if len(held) != len(kinds) {
		t.Errorf("groups hold %d kinds, the ledger names %d: %v", len(held), len(kinds), held)
	}
}
