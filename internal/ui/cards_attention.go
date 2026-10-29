package ui

// CardAttention is an entry's ATTENTION: how many items of the attention queue
// are this entry's (an agent's or a mirrored window's asks, its waiting
// session, its held question, its reply drafts), the most urgent one's kind,
// and where that item is on the queue page. The sidebar's row draws it
// (navAttentionPill); the name is the retired Agents list card's badge's.
//
// 🔴 IT IS NOT DERIVED HERE, AND IT MUST NOT BE. internal/api composes it from
// attentionItems — the ONE read the queue page, the sidebar badge and the
// chief's attention_list make — so an entry and the queue cannot disagree about
// what is waiting.
type CardAttention struct {
	// Count is the entry's items; the row's pill shows only for Count > 0.
	Count int
	// Kind is the most urgent item's kind ("session", "question", "ask",
	// "plugin": internal/api's attention kinds), which picks the icon.
	Kind string
	// Href opens that item on the queue page (/attention#<its key>). Nothing
	// draws it now: the retired card's badge was a link to it, and a sidebar row
	// is itself a link, so its pill cannot be one.
	Href string
	// Keys are the entry's items' keys, newest first: what a test compares
	// with the queue's.
	Keys []string
	// Kinds is every kind among the entry's items, each once, in the order
	// first seen: what the waiting predicate reads (waitTokens). Kind alone is
	// only the most urgent one.
	Kinds []string
}

// AttentionKindIcons is each attention kind's badge icon and the words a screen
// reader hears for it. internal/api's TestEveryAttentionKindHasABadgeIcon pins
// that every kind an entry can count is here.
var AttentionKindIcons = map[string]struct{ Icon, Words string }{
	"session":  {"🔒", "a session waiting at a prompt"},
	"question": {"💬", "a session asking a question"},
	"ask":      {"❓", "an agent's question"},
	"plugin":   {"✉️", "a reply draft to approve"},
}
