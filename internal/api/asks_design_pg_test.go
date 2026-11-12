package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sessionstate"
)

// TestTheAsksPageIsAFlatListWithNoIntroAndNoBackLink: the page draws its
// heading and its groups and nothing around them — no bordered page card, no
// "Everything waiting on you" paragraph, no "Back to muster" link. RED at
// origin/main: all three were drawn.
func TestTheAsksPageIsAFlatListWithNoIntroAndNoBackLink(t *testing.T) {
	f := newAttnFixture(t)
	f.ask(t, f.opTok, "Ship it?")
	page := f.page(t, "/attention")
	if !strings.Contains(page, `<section class="flex flex-col gap-3" data-testid="attention-page">`) || !strings.Contains(page, `>Asks</h1>`) {
		t.Fatalf("no flat page section with its heading:\n%s", page)
	}
	for _, bad := range []string{"Everything waiting on you", "Back to muster", "rounded-2xl border border-line bg-s1/70"} {
		if strings.Contains(page, bad) {
			t.Errorf("the page still draws %q", bad)
		}
	}
	if !strings.Contains(askList(page), `data-asks-group="questions"`) {
		t.Errorf("the ask is not under a group label:\n%s", askList(page))
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
		// The row is counted on the Waiting group's label (its window is
		// covered, so the row is the group's one entry).
		if m := groupLabel.FindAllStringSubmatch(askList(page), -1); len(m) != 1 || m[0][1] != "waiting" || m[0][3] != "1" {
			t.Errorf("group labels %q, want waiting 1", m)
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
