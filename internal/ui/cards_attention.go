package ui

// CardAttention is what a card's ATTENTION BADGE shows: how many items of the
// attention queue are this card's (an agent's or a mirrored window's asks, its
// waiting session, its held question, its reply drafts), the most urgent one's
// kind, and where that item is on the queue page.
//
// 🔴 IT IS NOT DERIVED HERE, AND IT MUST NOT BE. internal/api composes it from
// attentionItems — the ONE read the queue page, the sidebar badge and the
// chief's attention_list make — so a card and the queue cannot disagree about
// what is waiting.
type CardAttention struct {
	// Count is the card's items; the badge shows only for Count > 0.
	Count int
	// Kind is the most urgent item's kind ("session", "question", "ask",
	// "plugin": internal/api's attention kinds), which picks the icon.
	Kind string
	// Href opens that item on the queue page (/attention#<its key>).
	Href string
	// Keys are the card's items' keys, newest first: what a test compares
	// with the queue's.
	Keys []string
	// Kinds is every kind among the card's items, each once, in the order
	// first seen: what the list's "Waiting on …" filter chips read
	// (waitTokens). Kind alone is only the most urgent one.
	Kinds []string
}

// AttentionKindIcons is each attention kind's badge icon and the words a screen
// reader hears for it. internal/api's TestEveryAttentionKindHasABadgeIcon pins
// that every kind a card can count is here.
var AttentionKindIcons = map[string]struct{ Icon, Words string }{
	"session":  {"🔒", "a session waiting at a prompt"},
	"question": {"💬", "a session asking a question"},
	"ask":      {"❓", "an agent's question"},
	"plugin":   {"✉️", "a reply draft to approve"},
}
