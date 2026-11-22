package api

import (
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
)

// THE SOURCE RULE (asks_filter.go askSources.sourceFor), over every kind of
// thing the Asks page lists: an officer's question, its draft, the step-up
// card a SESSION filed to confirm that draft, a plain session's question, an
// agent's question and reply draft, and an officer's notice. Pinned as
// literal keys and names, never derived from the rule itself.
//
// 🔴 RED at the base: there is no source rule (no key, no chip).
func TestTheSourceRuleNamesWhoRaisedEachItem(t *testing.T) {
	const owner = 7
	agentID := int64(41)
	x := askSources{
		drafts: map[int64]draftSource{
			498: {owner: owner, officer: "marketing:acme", agentID: agentID, agent: "changelog-bot"},
			77:  {owner: owner, agentID: agentID, agent: "Changelog writer"},
		},
		officerName: func(o int64, id string) string {
			if o == owner && id == "marketing:acme" {
				return "Marketing"
			}
			return ""
		},
	}
	stepUp := func(draft int64) attention.Ask {
		return attention.Ask{ID: 946, Owner: owner, Asker: attention.Asker{System: true, Label: "step-up"},
			StepUp: &attention.StepUpCard{Title: "Post to X?", DraftID: draft}}
	}
	notice := askItem(attention.Ask{ID: 5, Owner: owner, Kind: attention.KindNotice, Asker: attention.Asker{System: true, Label: "officer product:acme"}, Question: "FYI"})
	notice.Kind = attentionKindNotice
	agentNotice := askItem(attention.Ask{ID: 6, Owner: owner, Kind: attention.KindNotice, Asker: attention.Asker{AgentID: agentID, Label: "changelog-bot"}, Question: "FYI"})
	agentNotice.Kind = attentionKindNotice
	for _, c := range []struct {
		name       string
		it         attentionItem
		key, label string
		typ        string
	}{
		{"an officer's question", askItem(attention.Ask{ID: 1, Owner: owner, Asker: attention.Asker{System: true, Label: "officer marketing:acme"}, Question: "Tagline?"}),
			"officer:7:marketing:acme", "Marketing", askTypeQuestion},
		{"a session-filed step-up card for the officer's draft", askItem(stepUp(498)), "officer:7:marketing:acme", "Marketing", askTypeConfirm},
		{"a step-up card for the agent's reply draft (muster's card, not the agent's ask)", askItem(stepUp(77)), "session::muster", "muster", askTypeConfirm},
		{"a step-up card whose draft the reader cannot read", askItem(stepUp(999)), "session::muster", "muster", askTypeConfirm},
		{"a plain session's question", askItem(attention.Ask{ID: 2, Owner: owner, Asker: attention.Asker{MirrorHost: "ws", MirrorSession: focusSID, Label: "Release notes"}, Question: "Publish?"}),
			"session:ws:Release notes", "Release notes", askTypeQuestion},
		{"an agent's question", askItem(attention.Ask{ID: 3, Owner: owner, Asker: attention.Asker{AgentID: agentID, Label: "changelog-bot"}, Question: "Which release?"}),
			"agentid:41", "changelog-bot", askTypeQuestion},
		{"an agent's notice (not one of its asks: no chip)", agentNotice, "", "", askTypeNotice},
		{"an officer's notice (no declared name: its words)", notice, "officer:7:product:acme", "Product — Acme", askTypeNotice},
		{"the officer's draft on the Drafts tab", attentionItem{Key: draftKey(498), Kind: attentionKindPlugin}, "officer:7:marketing:acme", "Marketing", askTypeDraft},
	} {
		got := x.ofItem(c.it)
		if got.key != c.key || got.label != c.label {
			t.Errorf("%s: source %q %q, want %q %q", c.name, got.key, got.label, c.key, c.label)
		}
		if typ := itemType(c.it); typ != c.typ {
			t.Errorf("%s: type %q, want %q", c.name, typ, c.typ)
		}
	}
	// A draft is the same rule: the officer's, the agent's.
	for id, want := range map[int64]string{498: "officer:7:marketing:acme", 77: "agentid:41"} {
		if got := x.ofDraft(x.drafts[id]).key; got != want {
			t.Errorf("draft %d: source %q, want %q", id, got, want)
		}
	}
	// THE SAME OFFICER ID UNDER TWO OWNERS (a member and their team) is two
	// sources, never one chip (round 1, G1); the same session name on two
	// hosts likewise.
	team := x.sourceFor(9, "marketing:acme", 0, "", "", "").key
	if team == x.ofDraft(x.drafts[498]).key {
		t.Errorf("two owners' marketing officers share the key %q", team)
	}
	a := askItem(attention.Ask{ID: 8, Owner: owner, Asker: attention.Asker{MirrorHost: "ws", MirrorSession: focusSID, Label: "Release notes"}, Question: "Q?"})
	b := askItem(attention.Ask{ID: 9, Owner: owner, Asker: attention.Asker{MirrorHost: "lap", MirrorSession: focusSID, Label: "Release notes"}, Question: "Q?"})
	if ka, kb := x.ofItem(a).key, x.ofItem(b).key; ka == kb {
		t.Errorf("two hosts' sessions of one name share the key %q", ka)
	}
}

// TestAFilterKeepsOnlyItsTypeAndSource: the filter's keep over items and
// drafts, each dimension alone and both together.
func TestAFilterKeepsOnlyItsTypeAndSource(t *testing.T) {
	x := askSources{drafts: map[int64]draftSource{10: {owner: 1, officer: "marketing:acme"}}}
	mkt := askItem(attention.Ask{ID: 1, Owner: 1, Asker: attention.Asker{System: true, Label: "officer marketing:acme"}, Question: "Q?"})
	card := askItem(attention.Ask{ID: 2, Owner: 1, Asker: attention.Asker{System: true, Label: "step-up"}, StepUp: &attention.StepUpCard{Title: "Post?", DraftID: 10}})
	other := askItem(attention.Ask{ID: 3, Owner: 1, Asker: attention.Asker{System: true, Label: "officer product:acme"}, Question: "P?"})
	keep := func(f asksFilter, it attentionItem) bool { return f.keepItem(x, it) }
	src := asksFilter{from: "officer:1:marketing:acme"}
	if !keep(src, mkt) || !keep(src, card) || keep(src, other) {
		t.Errorf("the source filter: officer's question %v, its draft's step-up card %v, another officer's %v", keep(src, mkt), keep(src, card), keep(src, other))
	}
	typ := asksFilter{typ: askTypeConfirm}
	if keep(typ, mkt) || !keep(typ, card) {
		t.Errorf("the type filter: a question kept %v, a confirmation kept %v", keep(typ, mkt), keep(typ, card))
	}
	both := asksFilter{typ: askTypeQuestion, from: "officer:1:marketing:acme"}
	if !keep(both, mkt) || keep(both, card) || keep(both, other) {
		t.Errorf("type and source together: %v %v %v", keep(both, mkt), keep(both, card), keep(both, other))
	}
	if !src.keepDraft(x, x.drafts[10]) || typ.keepDraft(x, x.drafts[10]) || !(asksFilter{typ: askTypeDraft}).keepDraft(x, x.drafts[10]) {
		t.Error("the filter over a draft")
	}
}
