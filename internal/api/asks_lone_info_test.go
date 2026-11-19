package api

import (
	"context"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/ui"
)

// asksFooterOf is a card's footer row, "" for none.
var asksFooterOf = regexp.MustCompile(`(?s)<div class="` + regexp.QuoteMeta(ui.AsksCardFooterClass) + `" data-asks-footer>(.*?)</div>\n`)

// TestALoneAsksInfoSitsInTheFooterNotARowOfItsOwn: the v0.250.0 phone check
// (F4): an ask card with no context words drew its ⓘ (where the session runs)
// on a 44px row of its own, one icon at the end of an empty line. It is now
// in the card's footer, right after "View in session". Red at origin/main,
// whose card had a data-asks-place row and a footer of the link alone.
//
// Controls: a card WITH context words keeps the ⓘ at the end of that line
// (the footer is the link alone), and a card with no footer link keeps the
// ⓘ's row (there is nowhere else to put it).
func TestALoneAsksInfoSitsInTheFooterNotARowOfItsOwn(t *testing.T) {
	now := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	const view = "/mirror/view?host=atlas&amp;session=fern"
	ask := func(focus, link string) attentionItem {
		return attentionItem{Kind: attentionKindAsk, Key: "ask-7", AskID: 7, Text: "Per API key or per client IP?",
			Options: []string{"Per API key", "Per client IP"}, Since: now.Add(-11 * time.Minute),
			Focus: focus, target: "tmux fern:1 @ atlas", sessionLink: link}
	}
	trigger := `<button type="button" id="place-ask-7-open"`
	viewLink := `<a href="` + view + `" class="` + ui.AsksCardLinkClass + `" data-asks-view-session>` + askViewInSession + `</a>`

	lone := askFeedCardHTML(ask("", "/mirror/view?host=atlas&session=fern"), now)
	if strings.Contains(lone, `data-asks-place`) {
		t.Errorf("a card with no context words still draws the ⓘ on a row of its own:\n%s", lone)
	}
	foot := asksFooterOf.FindStringSubmatch(lone)
	if foot == nil {
		t.Fatalf("no footer:\n%s", lone)
	}
	if !strings.HasPrefix(foot[1], viewLink+trigger) || strings.Count(lone, trigger) != 1 {
		t.Errorf("the footer is not \"View in session\" then the ⓘ:\n%s", foot[1])
	}

	worded := askFeedCardHTML(ask("Adding rate limits to the public API", "/mirror/view?host=atlas&session=fern"), now)
	if !strings.Contains(worded, `data-asks-place`) || strings.Count(worded, trigger) != 1 {
		t.Errorf("CONTROL: a card with context words lost its ⓘ beside them:\n%s", worded)
	}
	if foot := asksFooterOf.FindStringSubmatch(worded); foot == nil || strings.Contains(foot[1], trigger) {
		t.Errorf("CONTROL: the ⓘ beside the context words is in the footer too:\n%s", worded)
	}

	unlinked := askFeedCardHTML(ask("", ""), now)
	if !strings.Contains(unlinked, `data-asks-place`) || strings.Count(unlinked, trigger) != 1 {
		t.Errorf("CONTROL: a card with no footer link lost its ⓘ:\n%s", unlinked)
	}
}

// askPageFooter is the ask page's footer row.
var askPageFooter = regexp.MustCompile(`(?s)<div class="` + regexp.QuoteMeta(ui.AsksCardFooterClass) + ` gap-x-4 border-t border-line pt-2" data-asks-footer>(.*?)</div>\n`)

// TestTheAskPagesLoneInfoSitsInItsFooter: the ask page is the feed's card,
// expanded, and puts a lone ⓘ (no context words) in its footer after "View
// in session" the same way (asksCardContextRow). The fixture's asks that name
// a window carry no context words, so each one's ⓘ moves. Red at
// origin/main.
func TestTheAskPagesLoneInfoSitsInItsFooter(t *testing.T) {
	asks := whereAsks()
	s := whereServer(t, asks...)
	moved := 0
	for _, a := range asks {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", askPagePath(a.ID), nil).WithContext(s.attentionPrincipal(context.Background(), 1))
		s.renderAskPage(rec, r, a)
		body := rec.Body.String()
		trigger := regexp.MustCompile(`<button type="button" id="place-[^"]*-open"`).FindString(body)
		if trigger == "" {
			continue
		}
		foot := askPageFooter.FindStringSubmatch(body)
		if foot == nil {
			t.Fatalf("ask %d: no footer:\n%s", a.ID, body)
		}
		if strings.Contains(body, `data-asks-place`) || !strings.Contains(foot[1], `</a>`+trigger) {
			t.Errorf("ask %d: the lone ⓘ is not in the footer after its link:\nfooter: %s", a.ID, foot[1])
		}
		moved++
	}
	if moved == 0 {
		t.Fatal("CONTROL: no ask in the fixture has an ⓘ — the test sees nothing")
	}
}
