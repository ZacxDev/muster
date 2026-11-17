package api

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
)

// A HELD QUESTION'S WHY (operator decision on #767 round 0: "Context now"): a
// session's held AskUserQuestion is drawn with the same fallback line an
// attention_ask without context gets — the session's focus title and the last
// thing its Claude said before the question (its window's Pending.Last), with
// secrets redacted and bounded as attention.CleanWhy bounds them — at render
// time. Each card carries ITS session's words only, and another owner's queue
// carries none of them.
func TestAHeldQuestionShowsItsSessionsFocusAndLastWordsAndNoOtherSessions(t *testing.T) {
	f := newAttnFixture(t)
	secret := "ghp_" + strings.Repeat("a", 36)
	long := strings.Repeat("y", attention.MaxWhyLastRunes+80)
	f.mirror.mu.Lock()
	f.mirror.windows = []mirror.Window{
		{Session: "main", ID: "@1", Index: 1, Active: true, Claude: true, ClaudeSession: focusSID, Status: mirror.StatusWaiting, StatusSince: attnSince, Activity: 1_700_000_000,
			Pending: &mirror.Pending{Summary: "Which database?", Tool: "AskUserQuestion", Last: "I compared both; the token " + secret + " is in .env. Which one?"}},
		{Session: "main", ID: "@2", Index: 2, Claude: true, ClaudeSession: secondSID, Status: mirror.StatusWaiting, StatusSince: attnSince, Activity: 1_700_000_000,
			Pending: &mirror.Pending{Summary: "Ship?", Tool: "AskUserQuestion", Last: "Other session words " + long}},
	}
	f.mirror.mu.Unlock()
	if _, err := focus.NewPG(f.pool).Upsert(asUser(1), focus.MirrorSubject(focusSID), focus.Write{Title: "Pick the store for the importer", Source: focus.SourceSummarizer}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []struct{ sid, tool string }{{focusSID, "toolu_why1"}, {secondSID, "toolu_why2"}} {
		if _, err := f.srv.Questions().Open(askq.Key{Owner: 1, Source: askSourceMirror, Scope: "ws", Session: s.sid, ToolUseID: s.tool},
			[]askq.Question{{Question: "Which?", Options: []askq.Option{{Label: "a"}, {Label: "b"}}}}, askq.NewID()); err != nil {
			t.Fatal(err)
		}
	}
	body := f.page(t, "/attention/items")
	li := func(key string) string {
		i := strings.Index(body, `<li id="`+key+`"`)
		if i < 0 {
			t.Fatalf("no card %s in %s", key, body)
		}
		s := body[i:]
		return s[:strings.Index(s, "</li>")]
	}
	one, two := li(sessionKey("ws", "main", "@1")), li(sessionKey("ws", "main", "@2"))
	if !strings.Contains(one, `data-ask-why="session"`) || !strings.Contains(one, "Pick the store for the importer") ||
		!strings.Contains(one, "I compared both; the token «redacted» is in .env. Which one?") {
		t.Fatalf("the held question's card does not carry its focus and last words:\n%s", one)
	}
	if strings.Contains(body, secret) {
		t.Fatal("a secret in the session's last words reached the page")
	}
	if strings.Contains(one, "Other session words") || strings.Contains(two, "I compared both") || strings.Contains(two, "Pick the store") {
		t.Fatalf("a card carries another session's words:\n%s\n---\n%s", one, two)
	}
	// Bounded: never past an ask's fallback bound (the hub already caps a
	// window's last words at mirror.MaxPendingLast, below it).
	if !strings.Contains(two, "Other session words yyy") || strings.Contains(two, strings.Repeat("y", min(attention.MaxWhyLastRunes, mirror.MaxPendingLast))) {
		t.Fatalf("the last words are not bounded:\n%.400s", two)
	}
	// Another owner's queue: none of these words, whatever it holds.
	if _, err := f.srv.Questions().Open(askq.Key{Owner: f.member, Source: askSourceMirror, Scope: "ws", Session: focusSID, ToolUseID: "toolu_why3"},
		[]askq.Question{{Question: "Theirs?"}}, askq.NewID()); err != nil {
		t.Fatal(err)
	}
	items, _ := f.srv.attentionItems(asUser(f.member))
	for _, it := range items {
		if strings.Contains(it.heldLast+it.Detail+it.Text, "compared both") || strings.Contains(it.heldLast, "Other session") {
			t.Fatalf("another owner's queue carries the operator's session's words: %+v", it)
		}
	}
	// Never in the summary API (its "never Claude's last words" rule).
	if raw := f.call(t, "GET", "/api/attention/summary", nil, f.operator()).Body.String(); strings.Contains(raw, "compared both") || strings.Contains(raw, "Other session words") {
		t.Fatalf("the summary carries a held question's last words: %s", raw)
	}
}
