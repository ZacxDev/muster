package api

import "testing"

// TestEveryAttentionKindHasAHome: every kind in this test's own list (kinds,
// below) has exactly one home — one Ask-tab group, or, for the reply draft,
// the Reply drafts tab (askTabItems strips it from the Ask tab) — and no group
// holds a kind the list does not name. A kind in the list with no home fails;
// a new kind constant nobody adds to the list is NOT caught here.
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
