package api

import (
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/plugins"
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
	officer := "marketing:acme"
	agentID := int64(41)
	x := askSources{
		agents: map[int64]agents.Agent{agentID: {ID: agentID, Name: "changelog-bot", DisplayName: "Changelog writer"}},
		drafts: map[int64]plugins.Draft{
			498: {ID: 498, OwnerID: owner, Officer: &officer, AgentID: &agentID, AgentName: "changelog-bot"},
			77:  {ID: 77, OwnerID: owner, AgentID: &agentID, AgentName: "changelog-bot"},
		},
		officerName: func(o int64, id string) string {
			if o == owner && id == officer {
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
	for _, c := range []struct {
		name       string
		it         attentionItem
		key, label string
		typ        string
	}{
		{"an officer's question", askItem(attention.Ask{ID: 1, Owner: owner, Asker: attention.Asker{System: true, Label: "officer " + officer}, Question: "Tagline?"}),
			"officer:marketing:acme", "Marketing", askTypeQuestion},
		{"a session-filed step-up card for the officer's draft", askItem(stepUp(498)), "officer:marketing:acme", "Marketing", askTypeConfirm},
		{"a step-up card for the agent's reply draft", askItem(stepUp(77)), "agent:changelog-bot", "Changelog writer", askTypeConfirm},
		{"a step-up card whose draft the reader cannot read", askItem(stepUp(999)), "session:muster", "muster", askTypeConfirm},
		{"a plain session's question", askItem(attention.Ask{ID: 2, Owner: owner, Asker: attention.Asker{MirrorHost: "ws", MirrorSession: focusSID, Label: "Release notes"}, Question: "Publish?"}),
			"session:Release notes", "Release notes", askTypeQuestion},
		{"an agent's question", askItem(attention.Ask{ID: 3, Owner: owner, Asker: attention.Asker{AgentID: agentID, Label: "changelog-bot"}, Question: "Which release?"}),
			"agent:changelog-bot", "Changelog writer", askTypeQuestion},
		{"an agent no longer readable", askItem(attention.Ask{ID: 4, Owner: owner, Asker: attention.Asker{AgentID: 9999, Label: "gone-bot"}, Question: "Still there?"}),
			"session:gone-bot", "gone-bot", askTypeQuestion},
		{"an officer's notice (no declared name: its words)", notice, "officer:product:acme", "Product — Acme", askTypeNotice},
		{"the officer's draft on the Drafts tab", attentionItem{Key: draftKey(498), Kind: attentionKindPlugin}, "officer:marketing:acme", "Marketing", askTypeDraft},
	} {
		got := x.ofItem(c.it)
		if got.key != c.key || got.label != c.label {
			t.Errorf("%s: source %q %q, want %q %q", c.name, got.key, got.label, c.key, c.label)
		}
		if typ := itemType(c.it); typ != c.typ {
			t.Errorf("%s: type %q, want %q", c.name, typ, c.typ)
		}
	}
	// A draft card is the same rule: the officer's, the agent's.
	for id, want := range map[int64]string{498: "officer:marketing:acme", 77: "agent:changelog-bot"} {
		if got := x.ofDraft(x.drafts[id]).key; got != want {
			t.Errorf("draft %d: source %q, want %q", id, got, want)
		}
	}
	// The agent's source carries its id (an agent:<name> filter matches by it).
	if got := x.ofItem(askItem(stepUp(77))).agentID; got != agentID {
		t.Errorf("the agent's source carries agent %d, want %d", got, agentID)
	}
}

// TestAFilterKeepsOnlyItsTypeAndSource: the filter's keep over items and
// drafts, each dimension alone and both together.
func TestAFilterKeepsOnlyItsTypeAndSource(t *testing.T) {
	officer := "marketing:acme"
	x := askSources{drafts: map[int64]plugins.Draft{10: {ID: 10, Officer: &officer}}}
	mkt := askItem(attention.Ask{ID: 1, Asker: attention.Asker{System: true, Label: "officer " + officer}, Question: "Q?"})
	card := askItem(attention.Ask{ID: 2, Asker: attention.Asker{System: true, Label: "step-up"}, StepUp: &attention.StepUpCard{Title: "Post?", DraftID: 10}})
	other := askItem(attention.Ask{ID: 3, Asker: attention.Asker{System: true, Label: "officer product:acme"}, Question: "P?"})
	keep := func(f asksFilter, it attentionItem) bool { return f.keepItem(x, it, nil) }
	src := asksFilter{from: "officer:" + officer}
	if !keep(src, mkt) || !keep(src, card) || keep(src, other) {
		t.Errorf("the source filter: officer's question %v, its draft's step-up card %v, another officer's %v", keep(src, mkt), keep(src, card), keep(src, other))
	}
	typ := asksFilter{typ: askTypeConfirm}
	if keep(typ, mkt) || !keep(typ, card) {
		t.Errorf("the type filter: a question kept %v, a confirmation kept %v", keep(typ, mkt), keep(typ, card))
	}
	both := asksFilter{typ: askTypeQuestion, from: "officer:" + officer}
	if !keep(both, mkt) || keep(both, card) || keep(both, other) {
		t.Errorf("type and source together: %v %v %v", keep(both, mkt), keep(both, card), keep(both, other))
	}
	if !src.keepDraft(x, x.drafts[10]) || typ.keepDraft(x, x.drafts[10]) || !(asksFilter{typ: askTypeDraft}).keepDraft(x, x.drafts[10]) {
		t.Error("the filter over a draft")
	}
}
