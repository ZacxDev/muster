package api

import (
	"context"
	"io"
	"log"
	"maps"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// SESSION NAMES FIRST, THE TMUX ADDRESS BEHIND AN ⓘ (operator rule,
// 2026-11-17: muster is for people who are not engineers, so its own chrome
// shows no address, id or code in its top layer). A mirrored session is named
// by its name — or, with none, its codename without tmux's window index — and
// "on <host>"; its exact tmux address ("tmux fern:1 @ atlas") is inside the
// ⓘ details sheet (ui.InfoSheet), with a Copy button.

// jargonInTopLayer is what a top-layer text must not hold: a tmux target's
// "tmux ", a place's " @ " and a window index — ":<digit>" after a name, not
// after a digit (a clock time, "01:10 UTC", is not one).
var jargonInTopLayer = regexp.MustCompile(`\btmux |\s@\s|[^\s\d]:[0-9]`)

// topLayerTexts is what a person reads on markup without opening anything:
// every text node and every tooltip, aria-label and placeholder — but not the
// ⓘ details sheets ([data-info-sheet], the one place the address belongs),
// nor scripts, styles and templates, nor data-* attributes (never drawn).
func topLayerTexts(t *testing.T, markup string) []string {
	t.Helper()
	z := html.NewTokenizer(strings.NewReader(markup))
	var out []string
	skip := 0 // depth inside a skipped subtree
	var stack []bool
	for {
		switch z.Next() {
		case html.ErrorToken:
			if z.Err() != io.EOF {
				t.Fatalf("parse: %v", z.Err())
			}
			return out
		case html.TextToken:
			if skip == 0 {
				if s := strings.TrimSpace(string(z.Text())); s != "" {
					out = append(out, s)
				}
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			skipped := slices.Contains([]string{"script", "style", "template"}, tok.Data)
			for _, a := range tok.Attr {
				switch a.Key {
				case "data-info-sheet":
					skipped = true
				case "title", "aria-label", "placeholder":
					if skip == 0 && !skipped {
						out = append(out, a.Val)
					}
				}
			}
			if tok.Type == html.SelfClosingTagToken || isVoid(tok.Data) {
				continue
			}
			stack = append(stack, skipped)
			if skipped {
				skip++
			}
		case html.EndTagToken:
			if n := len(stack); n > 0 {
				if stack[n-1] {
					skip--
				}
				stack = stack[:n-1]
			}
		}
	}
}

func isVoid(tag string) bool {
	return slices.Contains([]string{"area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "source", "track", "wbr"}, tag)
}

// assertNoJargon fails for every top-layer text of markup that spells a tmux
// address, a place's " @ " or a window index.
func assertNoJargon(t *testing.T, what, markup string) {
	t.Helper()
	texts := topLayerTexts(t, markup)
	if len(texts) == 0 {
		t.Fatalf("%s: CONTROL: no top-layer text at all — the scan sees nothing", what)
	}
	for _, s := range texts {
		if jargonInTopLayer.MatchString(s) {
			t.Errorf("%s: the top layer says %q", what, s)
		}
	}
}

// TestTheJargonScanSeesAnAddress: the scan's own control — it finds an
// address in a top-layer text, a tooltip and an aria-label, and ignores one
// inside an ⓘ sheet, a script and a data attribute.
func TestTheJargonScanSeesAnAddress(t *testing.T) {
	for _, bad := range []string{
		`<p>tmux fern:1 @ atlas is asking</p>`,
		`<a title="Fix it · tmux fern:1 @ atlas">Fix it</a>`,
		`<button aria-label="fern:1">x</button>`,
		`<span>retry-test:1</span>`,
	} {
		hit := false
		for _, s := range topLayerTexts(t, bad) {
			hit = hit || jargonInTopLayer.MatchString(s)
		}
		if !hit {
			t.Errorf("the scan missed the address in %s", bad)
		}
	}
	ok := `<p>Fix it · on atlas</p><div data-info-sheet=""><p>Where it runs</p><code>tmux fern:1 @ atlas</code></div><script>var a="tmux x:1 @ y"</script><a data-switcher-code="fern:1">fern</a>`
	for _, s := range topLayerTexts(t, ok) {
		if jargonInTopLayer.MatchString(s) {
			t.Errorf("the scan flagged %q in %s", s, ok)
		}
	}
}

// TestNoTopLayerOfTheAsksSurfacesSpellsATmuxAddress: the Asks page's cards
// (an ask from a labelled window, one from an unlabelled window, and both
// windows waiting), a session page's ask card, and an ask's own page name the
// session in words; the address is only in their ⓘ.
//
// REGRESSION TEST for the jargon audit's F001/F002/F121/F122/U-02: red at
// origin/main ("tmux scratch3:5 @ ws" under every card, "tmux build:7 @ ws
// asks" on the ask page).
//
// Not covered: an ask whose session no window shows any more (whereAsks' 93)
// is still named by its stored label ("pear:2 @ ws (not shown …)").
func TestNoTopLayerOfTheAsksSurfacesSpellsATmuxAddress(t *testing.T) {
	asks := whereAsks()
	s := whereServer(t, asks...)
	ctx := s.attentionPrincipal(context.Background(), 1)
	items, err := s.attentionItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var shown []attentionItem
	for _, it := range items {
		if it.AskID == 91 || it.AskID == 92 || it.Kind == attentionKindSession {
			shown = append(shown, it)
		}
	}
	if len(shown) < 4 {
		t.Fatalf("CONTROL: %d items (two asks, two waiting windows), want 4: %+v", len(shown), shown)
	}
	feed := attentionItemsHTML(shown, time.Now())
	assertNoJargon(t, "the Asks feed", feed)
	for _, target := range []string{"tmux scratch3:5 @ ws", "tmux build:7 @ ws"} {
		if !strings.Contains(feed, `data-info-code="">`+target+`</code>`) || !strings.Contains(feed, `data-copy-text="`+target+`"`) {
			t.Errorf("the Asks feed has no ⓘ holding %q with a Copy button", target)
		}
	}
	for _, it := range shown {
		if it.AskID == 91 {
			block := attentionItemHTML(it, time.Now(), "/mirror/view?host=ws&session=scratch3&window=%4037")
			assertNoJargon(t, "a session page's ask card", block)
			if !strings.Contains(block, `data-info-code="">tmux scratch3:5 @ ws</code>`) {
				t.Errorf("a session page's ask card has no ⓘ holding its target:\n%s", block)
			}
		}
	}
	for _, a := range asks[:2] {
		rec := httptest.NewRecorder()
		s.renderAskPage(rec, httptest.NewRequest("GET", askPagePath(a.ID), nil).WithContext(ctx), a)
		body := rec.Body.String()
		card := body[strings.Index(body, `<article`):]
		card = card[:strings.Index(card, `</article>`)]
		assertNoJargon(t, "the ask page's card", card)
		title := body[strings.Index(body, "<title>"):strings.Index(body, "</title>")]
		assertNoJargon(t, "the ask page's <title>", title+"</title>")
	}
}

// TestNoTopLayerOfASessionsPagesSpellsATmuxAddress: the sidebar's rows, a
// named and an unnamed window's Rendered view header (its title, its
// full-title popover, its rename dialog) say no tmux address and no window
// index; the header's ⓘ holds the window's exact address.
//
// REGRESSION TEST for the jargon audit's F003/F005/F010/F021/U-14/U-15: red at
// origin/main ("mango:1", "main:2", "· tmux main:1 @ ws" in the popover and
// the rename note, "Billing migration · mango:1 · tmux main:1 @ ws" as a
// tooltip).
func TestNoTopLayerOfASessionsPagesSpellsATmuxAddress(t *testing.T) {
	store := &memFocus{rows: map[focus.Subject]focus.Focus{
		focus.MirrorSubject(focusSID): {Subject: focus.MirrorSubject(focusSID), Title: "Fixing the Stripe webhook retry",
			UpdatedAt: time.Now(), Name: focus.Name{Auto: "Billing migration"}},
	}}
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}, windows: namedWindows()}
	s, ts := focusMirrorServer(t, fm, store, nil)
	_, list := mirrorGet(t, s, ts, ui.NavAgentsPath)
	for _, w := range []string{"@1", "@2"} {
		assertNoJargon(t, "the sidebar row of "+w, cardFor(t, list, ref("main", w).ViewPath()))
	}
	for w, address := range map[string]string{"@1": "tmux main:1 @ ws", "@2": "tmux main:2 @ ws"} {
		_, page := mirrorGet(t, s, ts, ref("main", w).ViewPath())
		head := page[strings.Index(page, "<header"):]
		head = head[:strings.Index(head, "</header>")]
		assertNoJargon(t, "the Rendered view header of "+w, head)
		if !strings.Contains(head, `data-info-code="">`+address+`</code>`) {
			t.Errorf("%s: the header's ⓘ does not hold %q", w, address)
		}
		rename := page[strings.Index(page, `id="`+ui.SessionRenameID+`"`):]
		rename = rename[:strings.Index(rename, "</form>")]
		assertNoJargon(t, "the rename dialog of "+w, `<div `+rename)
		title := page[strings.Index(page, "<title>"):strings.Index(page, "</title>")]
		assertNoJargon(t, "the <title> of "+w, title+"</title>")
	}
}

// TestTwoWindowsOfOneSessionAreTwoNamesOnTheirCards: two unnamed windows of
// ONE tmux session waiting at once are told apart on their Asks cards exactly
// as the session lists tell them apart ("fern · window 1", "fern · window 2":
// the list's qualifier, mirror.Ordered) — never both "fern · on ws". The
// ⓘ of each holds its own target.
func TestTwoWindowsOfOneSessionAreTwoNamesOnTheirCards(t *testing.T) {
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	fm := &fakeMirror{online: true, interactive: true, statusReport: true, v2: &fakeMirrorV2{}, windows: []mirror.Window{
		{Session: "fern", ID: "@4", Index: 1, Active: true, Claude: true, ClaudeSession: "5a1b2c3d-4444-4444-8444-000000000001", Status: mirror.StatusWaiting, StatusSince: attnSince},
		{Session: "fern", ID: "@9", Index: 2, Claude: true, ClaudeSession: "5a1b2c3d-4444-4444-8444-000000000002", Status: mirror.StatusWaiting, StatusSince: attnSince},
	}}
	s.UseExtensions(Extensions{Users: stubUsers{}, Mirror: fm})
	items := s.waitingSessions(s.attentionPrincipal(context.Background(), 1))
	got := map[string]string{}
	for _, it := range items {
		title, src, _ := attentionTitle(it)
		got[it.target] = title + " | " + src
	}
	want := map[string]string{
		"tmux fern:1 @ ws": "fern · window 1 | on ws",
		"tmux fern:2 @ ws": "fern · window 2 | on ws",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("cards: %v\nwant %v", got, want)
	}
}

// TestAnAskCardsPlaceSheetIsTheOneComponent: the ⓘ an ask's card draws is
// ui.SessionPlaceSheet's, byte for byte, keyed by the card — one component on
// every surface.
func TestAnAskCardsPlaceSheetIsTheOneComponent(t *testing.T) {
	it := attentionItem{Key: "ask-7", Kind: attentionKindAsk, AskID: 7, target: "tmux fern:1 @ atlas", Source: "tmux fern:1 @ atlas",
		at: mirror.Place{Code: "fern:1", Host: "atlas", Addr: "fern:1"}, Name: "Billing migration"}
	got := attentionPlaceSheetHTML(it, it.Key)
	want := ui.InfoSheetHTML(ui.SessionPlaceSheet("place-ask-7", "Billing migration", "atlas", "tmux fern:1 @ atlas"))
	if got != want {
		t.Fatalf("the card's ⓘ:\n%s\nwant\n%s", got, want)
	}
	if attentionPlaceSheetHTML(attentionItem{Key: "ask-8", Source: "Worker Seven"}, "ask-8") != "" {
		t.Error("an item that names no window drew an ⓘ")
	}
}
