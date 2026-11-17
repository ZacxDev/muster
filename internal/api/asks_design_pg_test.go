package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sessionstate"
	"github.com/ZacxDev/muster/internal/ui"
)

// TestTheAsksPageIsACardFeedWithItsCountAndScript: the page draws its heading
// with the open count, then the tabs — Open first, its feed of cards — the
// All clear template and the feed's script; nothing around
// them (no page card, no intro, no "Back to muster"). RED at origin/main: no
// count, no feed, no script, no toast.
func TestTheAsksPageIsACardFeedWithItsCountAndScript(t *testing.T) {
	f := newAttnFixture(t)
	f.ask(t, f.opTok, "Ship it?")
	page := f.page(t, "/attention")
	for _, want := range []string{
		`<section class="flex flex-col gap-3" data-testid="attention-page">`,
		`>Asks</h1>` + ui.AsksOpenCountHTML(2) + `</div>`, // the ask and the fixture's waiting window
		`<ul id="attention-items" class="` + ui.AsksFeedClass + `" data-testid="attention-items" data-asks-feed aria-live="polite">`,
		`<ul id="attention-answered" class="` + ui.AsksFeedClass + `" data-testid="attention-answered" data-asks-feed>`,
		`<template id="asks-empty-tpl">` + ui.AsksAllClearHTML + `</template>`,
		`data-asks-badge="question">Question</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	for _, bad := range []string{"Everything waiting on you", "Back to muster", "rounded-2xl border border-line bg-s1/70", `data-asks-group="`} {
		if strings.Contains(page, bad) {
			t.Errorf("the page still draws %q", bad)
		}
	}
}

// TestTheAsksPageLinksTheFeedScript: the page carries the card feed's static
// script (internal/ui's ledger test counts this page as the one that links
// it), once.
func TestTheAsksPageLinksTheFeedScript(t *testing.T) {
	f := newAttnFixture(t)
	page := f.page(t, "/attention")
	if n := strings.Count(page, ui.AsksFeedScriptHTML()); n != 1 {
		t.Fatalf("the page carries the feed script %d times, want 1", n)
	}
}

// waitingEpisodeRow opens a waiting (needs-you) episode for the fixture's
// window, whose host names pending (nil for none), and returns the Asks
// page's row for it.
func waitingEpisodeRow(t *testing.T, pending *mirror.Pending) (row, page string) {
	t.Helper()
	f := newStateFixture(t, false)
	f.status(mirror.StatusWaiting, 5*time.Second)
	f.mirror.mu.Lock()
	f.mirror.windows[0].Pending = pending
	f.mirror.mu.Unlock()
	octx := f.srv.attentionPrincipal(context.Background(), authz.OperatorUserID)
	ep, claimed, err := f.eps.Open(octx, sessionstate.Episode{Kind: sessionstate.KindMirror, Key: nudgeSID, State: sessionstate.StateNeedsYou,
		Waiting: true, Asked: true, Host: "ws", TmuxSession: "main", Window: "@1", Index: 1})
	if err != nil || !claimed {
		t.Fatalf("open: %v %v", claimed, err)
	}
	page = f.page(t, "/attention")
	key := episodeKey(ep.ID)
	i := strings.Index(page, `<li id="`+key+`"`)
	if i < 0 {
		t.Fatalf("no row for %s:\n%s", key, askList(page))
	}
	row = page[i:]
	return row[:strings.Index(row, "</li>")], page
}

// TestAWaitingEpisodeRowSaysWhatItWaitsOn: a "Stalled / waiting" row is one
// link through the tap route, in the Waiting group, and its line is the
// window's pending call — the host's own words, as the window's waiting item
// carries them — or, when the host names nothing, the fallback. RED at
// origin/main: the row held only its meta line and a "View in session" link.
func TestAWaitingEpisodeRowSaysWhatItWaitsOn(t *testing.T) {
	// Subtests: each fixture's episode is cleaned up before the next opens one.
	t.Run("pending call", func(t *testing.T) {
		row, page := waitingEpisodeRow(t, &mirror.Pending{Tool: "Bash", Summary: "Bash: npm test", Last: "Running the tests."})
		// The row is the feed's one entry (its window is covered), counted in
		// the open count, inside the feed's Waiting place.
		if !strings.Contains(page, ui.AsksOpenCountHTML(1)) || !strings.Contains(askList(page), `<li class="min-w-0" data-asks-episodes>`) {
			t.Errorf("the row is not the feed's one counted entry:\n%s", askList(page))
		}
		for _, want := range []string{`data-attention-episode-open>`, `href="/attention/open/episode-`, `data-attention-episode-state>Needs you</span>`,
			`data-attention-text>Bash: npm test</span>`} {
			if !strings.Contains(row, want) {
				t.Errorf("the row lacks %q:\n%s", want, row)
			}
		}
		if strings.Contains(row, askViewInSession) || strings.Contains(row, "<form") {
			t.Errorf("the row draws a text link or a form:\n%s", row)
		}
	})
	t.Run("fallback", func(t *testing.T) {
		if row, _ := waitingEpisodeRow(t, nil); !strings.Contains(row, `data-attention-text>`+sessionWaitingText+`</span>`) {
			t.Errorf("a row whose host names nothing lacks the fallback:\n%s", row)
		}
	})
}
