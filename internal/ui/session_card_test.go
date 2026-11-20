package ui

import (
	"regexp"
	"strings"
	"testing"
)

// cardFixture is an index holding ONE session's card, under the keys the
// server gives it (internal/api sessionCards): its conversation id and its raw
// tmux target. Every field is distinct from every other and from the keys, so
// an assertion on one cannot pass by finding another.
func cardFixture() *MentionIndex {
	c := SessionCard{Title: "Billing migration", Computer: "studio-mac", Status: SessionCardBusy,
		Href: "/mirror/view?host=studio-mac&session=heron&window=%401", Address: "tmux heron:1 @ studio-mac"}
	return NewMentionIndex(nil, nil).WithSessionCards(func() map[string]SessionCard {
		return map[string]SessionCard{"mirror:cs-heron-0001": c, "tmux heron:1 @ studio-mac": c}
	})
}

// cardCount is how many session cards html draws.
func cardCount(html string) int { return strings.Count(html, `data-session-card="`) }

// TestASessionMarkerDrawsTheOwnersCard: the chief's marker for a session the
// index holds draws its card — title, computer, status dot, a link that opens
// the session — and the address only in the ⓘ sheet, which comes after the
// paragraph (a <div> may not sit in a <p>). The marker's raw text is gone.
func TestASessionMarkerDrawsTheOwnersCard(t *testing.T) {
	html := markdownHTMLWith("Ask [[session:mirror:cs-heron-0001]] about it.", cardFixture())
	if n := cardCount(html); n != 1 {
		t.Fatalf("%d cards, want 1:\n%s", n, html)
	}
	for _, want := range []string{
		`data-session-card="busy"`,
		`data-session-status="busy"`,
		`<span data-session-card-title="" class="min-w-0 truncate font-medium">Billing migration</span>`,
		`>studio-mac</span>`,
		`href="/mirror/view?host=studio-mac&amp;session=heron&amp;window=%401"`,
		`data-session-card-open=""`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the card lacks %s:\n%s", want, html)
		}
	}
	if strings.Contains(html, "[[session:") || strings.Contains(html, "cs-heron-0001") {
		t.Errorf("the marker's raw text or id is drawn:\n%s", html)
	}
	p := regexp.MustCompile(`(?s)<p class="my-1">.*?</p>`).FindString(html)
	if strings.Contains(p, "tmux heron:1") {
		t.Errorf("the address is in the paragraph, not behind the ⓘ:\n%s", p)
	}
	if !strings.Contains(strings.TrimPrefix(html, p), `data-info-code="">tmux heron:1 @ studio-mac<`) {
		t.Errorf("the ⓘ sheet does not carry the address after the paragraph:\n%s", html)
	}
	if !strings.HasPrefix(html, `<p class="my-1">Ask <span data-session-card=`) || !strings.Contains(p, `</span> about it.</p>`) {
		t.Errorf("the card is not inline in its sentence:\n%s", html)
	}
}

// TestAMarkerNoCardAnswersForIsPlainWords: a marker naming nothing the index
// holds — a ghost id, another owner's session (absent from the viewer's index
// by construction: internal/api's TestASessionMarkerResolvesOnlyTheOwnersOwn
// pins that half) — draws "a session": no card, no id. The control in the
// same message does draw.
func TestAMarkerNoCardAnswersForIsPlainWords(t *testing.T) {
	html := markdownHTMLWith("[[session:mirror:ghost-9]] and [[session:mirror:cs-heron-0001]]", cardFixture())
	if n := cardCount(html); n != 1 {
		t.Errorf("%d cards, want 1 (the control):\n%s", n, html)
	}
	if !strings.HasPrefix(html, `<p class="my-1">a session and <span data-session-card=`) {
		t.Errorf("the ghost marker is not the plain words %q:\n%s", sessionCardUnknown, html)
	}
	if strings.Contains(html, "ghost-9") {
		t.Errorf("the ghost id is drawn:\n%s", html)
	}
}

// TestARawSessionReferenceUpgradesOnlyOnAnExactMatch: an old reply's raw tmux
// target or conversation id — bare or alone in a code span — is drawn as the
// card when it is EXACTLY a key; trailing sentence punctuation stays outside
// it; a near miss is left as written.
func TestARawSessionReferenceUpgradesOnlyOnAnExactMatch(t *testing.T) {
	for _, c := range []struct {
		name, src string
		cards     int
		keep      string // text that must still be drawn as written
	}{
		{"tmux bare", "see tmux heron:1 @ studio-mac.", 1, "</span>.</p>"},
		{"tmux in code", "see `tmux heron:1 @ studio-mac`", 1, ""},
		{"id bare", "see mirror:cs-heron-0001, then", 1, "</span>, then"},
		{"id in code", "`mirror:cs-heron-0001`", 1, ""},
		{"tmux wrong index", "see tmux heron:2 @ studio-mac", 0, "tmux heron:2 @ studio-mac"},
		{"tmux wrong host", "see tmux heron:1 @ work-laptop", 0, "tmux heron:1 @ work-laptop"},
		{"tmux longer host", "see tmux heron:1 @ studio-macx", 0, "tmux heron:1 @ studio-macx"},
		{"code near miss", "`tmux heron:2 @ studio-mac`", 0, "<code"},
		{"id near miss", "mirror:cs-heron-00012", 0, "mirror:cs-heron-00012"},
		{"id inside a word", "xmirror:cs-heron-0001", 0, "xmirror:cs-heron-0001"},
	} {
		html := markdownHTMLWith(c.src, cardFixture())
		if n := cardCount(html); n != c.cards {
			t.Errorf("%s: %d cards, want %d:\n%s", c.name, n, c.cards, html)
		}
		if c.keep != "" && !strings.Contains(html, c.keep) {
			t.Errorf("%s: %q not drawn as written:\n%s", c.name, c.keep, html)
		}
	}
}

// TestCardsAreTheChiefsOnly: an index without session cards (every chat but
// the chief's, the mirrored view) draws none, marker or raw reference.
func TestCardsAreTheChiefsOnly(t *testing.T) {
	html := markdownHTMLWith("[[session:mirror:cs-heron-0001]] tmux heron:1 @ studio-mac", NewMentionIndex(nil, nil))
	if n := cardCount(html); n != 0 {
		t.Errorf("%d cards without session cards on:\n%s", n, html)
	}
}

// TestACardsStatusDotSaysEachStatus: busy, idle and waiting draw distinct dots,
// and an unknown status is idle.
func TestACardsStatusDotSaysEachStatus(t *testing.T) {
	seen := map[string]bool{}
	for st, want := range map[string]string{SessionCardBusy: "busy", SessionCardIdle: "idle", SessionCardWaiting: "waiting", "weird": "idle"} {
		inline, _ := sessionCardHTML(SessionCard{Title: "T", Href: "/x", Status: st}, "sc-t")
		if !strings.Contains(inline, `data-session-status="`+want+`"`) {
			t.Errorf("%s: no %s dot: %s", st, want, inline)
		}
		cls := regexp.MustCompile(`data-session-status="[a-z]+"[^>]*class="([^"]+)"`).FindStringSubmatch(inline)
		if cls == nil {
			t.Fatalf("%s: no dot class: %s", st, inline)
		}
		if st != "weird" {
			if seen[cls[1]] {
				t.Errorf("%s: dot class %q shared with another status", st, cls[1])
			}
			seen[cls[1]] = true
		}
	}
}
