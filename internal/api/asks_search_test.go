package api

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/asklog"
	"github.com/ZacxDev/muster/internal/attention"
)

// TestParseAskQuery: every way an id is typed is an id — a bare number (either
// kind's), "#N", "ask N", "Ask #N", "draft N" — and anything else is text,
// kept as typed (trimmed). A number too long for an id, a "#" alone, a word
// beside the number and a zero are text, never an id.
func TestParseAskQuery(t *testing.T) {
	for _, c := range []struct {
		in   string
		want askQuery
	}{
		{"859", askQuery{AskID: 859, DraftID: 859}},
		{"#859", askQuery{AskID: 859, DraftID: 859}},
		{"  # 859 ", askQuery{AskID: 859, DraftID: 859}},
		{"ask 859", askQuery{AskID: 859}},
		{"Ask #859", askQuery{AskID: 859}},
		{"ASK#859", askQuery{AskID: 859}},
		{"draft 484", askQuery{DraftID: 484}},
		{"Draft #484", askQuery{DraftID: 484}},
		{"draft#484", askQuery{DraftID: 484}},
		{"0", askQuery{Text: "0"}},
		{"#", askQuery{Text: "#"}},
		{"ask", askQuery{Text: "ask"}},
		{"ask 859 now", askQuery{Text: "ask 859 now"}},
		{"859a", askQuery{Text: "859a"}},
		{"task 859", askQuery{Text: "task 859"}},
		{"1234567890123456789", askQuery{Text: "1234567890123456789"}},
		{"  Deploy the Zebra build ", askQuery{Text: "Deploy the Zebra build"}},
		{"50%", askQuery{Text: "50%"}},
	} {
		if got := parseAskQuery(c.in); got != c.want {
			t.Errorf("parseAskQuery(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
	if got := parseAskQuery(strings.Repeat("é", attention.MaxSearchTextRunes+5)).Text; len([]rune(got)) != attention.MaxSearchTextRunes {
		t.Errorf("a long query keeps %d runes, want %d", len([]rune(got)), attention.MaxSearchTextRunes)
	}
}

// TestAStepUpTitleNamesItsDraftLabelled: muster's own title carries the draft
// id as "· Draft #484" (the operator's call: never a bare "#484", which reads
// as an ask's), and a card stored with the old bare suffix is drawn with the
// labelled one — only for ITS draft id; a title ending in another number is
// left as it is. RED before: "Post to X? · #484".
func TestAStepUpTitleNamesItsDraftLabelled(t *testing.T) {
	if got := stepUpWithDraftID("Post to X?", 484); got != "Post to X? · Draft #484" {
		t.Errorf("stepUpWithDraftID = %q", got)
	}
	if got := stepUpWithDraftID("Post to X?", 0); got != "Post to X?" {
		t.Errorf("no draft: %q", got)
	}
	for _, c := range []struct {
		title string
		draft int64
		want  string
	}{
		{"Post to X? · #484", 484, "Post to X? · Draft #484"},
		{"Post to X? · Draft #484", 484, "Post to X? · Draft #484"},
		{"Post to X? · #484", 485, "Post to X? · #484"},
		{"Answer an ask as you?", 0, "Answer an ask as you?"},
	} {
		if got := stepUpTitleText(&attention.StepUpCard{Title: c.title, DraftID: c.draft}); got != c.want {
			t.Errorf("stepUpTitleText(%q, %d) = %q, want %q", c.title, c.draft, got, c.want)
		}
	}
}

// TestALogCardShowsItsAsksIdOnlyWhenItHasOne: an agent's question in the log
// is a muster ask, so its card says "Ask #<that ask's id>" (its SourceRef) —
// never the log row's own id, which is not an ask's. A question in chat has
// no muster ask and no chip.
func TestALogCardShowsItsAsksIdOnlyWhenItHasOne(t *testing.T) {
	now := time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)
	agent := askLogCardHTML(asklog.Ask{ID: 17, Kind: asklog.KindAttention, SourceRef: "859", Question: "Go?", AskedAt: now}, now, time.Hour)
	if got := chipsIn(agent); strings.Join(got, ",") != "Ask #859|ask|859|Ask #859" {
		t.Errorf("an agent's question's chips: %v", got)
	}
	chat := askLogCardHTML(asklog.Ask{ID: 18, Kind: asklog.KindPlainText, SourceRef: "", Question: "Go?", AskedAt: now}, now, time.Hour)
	if got := chipsIn(chat); len(got) != 0 {
		t.Errorf("a question in chat's chips: %v, want none", got)
	}
	bad := askLogCardHTML(asklog.Ask{ID: 19, Kind: asklog.KindAttention, SourceRef: "x1", Question: "Go?", AskedAt: now}, now, time.Hour)
	if got := chipsIn(bad); len(got) != 0 {
		t.Errorf("an unreadable ref's chips: %v, want none", got)
	}
}

// TestSearchResultsEscapeWhatWasTyped: the query is drawn back escaped — in
// the empty line and in the box.
func TestSearchResultsEscapeWhatWasTyped(t *testing.T) {
	out := asksSearchHTML(`<b>"x"</b>`, asksSearchResult{}, time.Now())
	if strings.Contains(out, "<b>") || !strings.Contains(out, "&lt;b&gt;&#34;x&#34;&lt;/b&gt;") {
		t.Errorf("the empty line: %s", out)
	}
}
