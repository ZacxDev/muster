package api

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/asklog"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/ui"
)

// TestEverySurfaceShowsTheSameOpenCount: ONE state — an agent's open ask, the
// fixture's waiting window, a pending reply draft, and the ask log holding
// four stale rows that never got an outcome — and every surface that says how
// much is open says the same literal 3: the Asks page's header, the sidebar's
// More badge (/attention/badge), the ☰ menu badge (/ui/nav/waiting) and the
// Log tab's "Open" pill. And the Open feed is not "All clear" while a draft
// waits.
//
// RED at origin/main d1ff2dbe: the header said 2 (drafts left out), the ☰
// counted the sidebar's waiting rows, and the Log's Open pill counted the
// log's own unresolved rows (the four stale ones and the live ask's).
func TestEverySurfaceShowsTheSameOpenCount(t *testing.T) {
	f := newAttnFixture(t, func(ext *Extensions, pool *pgxpool.Pool) {
		dbtest.OwnerExec(t, dbtest.DSN(t), `TRUNCATE asks`)
		ext.AskLog = &AskLog{Store: asklog.NewPG(pool)}
	})
	f.ask(t, f.opTok, "Ship it?")
	f.pendingDraft(t, "Thanks, it ships Friday.")
	// Rows the log never closed: history, not what is open now.
	for _, q := range []string{"one?", "two?", "three?", "four?"} {
		if _, _, err := f.srv.askLogStore().Open(asUser(1), asklog.Ask{Kind: asklog.KindAttention, Runtime: asklog.RuntimeAgent, Question: q,
			AskedAt: time.Now().Add(-48 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	const want = "3"

	page := f.page(t, "/attention")
	if !strings.Contains(page, `data-asks-open-count aria-live="polite">`+want+` open</span>`) {
		t.Errorf("the page header: %s", regexp.MustCompile(`data-asks-open-count[^<]*<`).FindString(page))
	}
	if strings.Contains(askList(page), `data-testid="attention-empty"`) {
		t.Errorf("the Open feed draws All clear while a draft waits")
	}
	if got := regexp.MustCompile(`data-testid="attention-badge" data-count="(\d+)"`).FindStringSubmatch(f.page(t, ui.AttentionBadgePath)); got == nil || got[1] != want {
		t.Errorf("the More badge: %v", got)
	}
	if got := regexp.MustCompile(`id="nav-waiting-badge" data-nav-waiting="" data-count="(\d+)"`).FindStringSubmatch(f.page(t, ui.NavWaitingPath)); got == nil || got[1] != want {
		t.Errorf("the ☰ badge: %v", got)
	}
	logPanel := f.page(t, "/attention?tab=log")
	if got := regexp.MustCompile(`data-count="open">Open (\d+)</span>`).FindStringSubmatch(logPanel); got == nil || got[1] != want {
		t.Errorf("the Log's Open pill: %v", got)
	}
	// The SAME definition, called directly: what the four surfaces drew.
	if n, err := f.srv.asksPendingCount(f.srv.attentionPrincipal(context.Background(), 1)); err != nil || n != 3 {
		t.Errorf("asksPendingCount = %d, %v", n, err)
	}
}

// TestTheOpenFeedSaysDraftsWaitInsteadOfAllClear: with no card on the Open
// feed and a reply draft pending, the feed draws the drafts' empty state (a
// link to the Drafts tab, the count in words), never All clear; with nothing
// at all it is All clear. Both carry the drafts marker the feed's script
// counts. RED at origin/main: All clear beside "Drafts (1)".
func TestTheOpenFeedSaysDraftsWaitInsteadOfAllClear(t *testing.T) {
	draft := designItems()[5:6]
	out, n := askFeedHTML(draft, "", 0, designNow)
	if n != 0 || strings.Contains(out, "All clear") || !strings.Contains(out, `data-asks-empty="drafts"`) ||
		!strings.Contains(out, `<span data-asks-drafts-n>1</span> reply draft waits for your decision.`) ||
		!strings.Contains(out, `href="/attention?tab=drafts"`) || !strings.Contains(out, `<li hidden aria-hidden="true" data-asks-drafts="1"></li>`) {
		t.Fatalf("a draft alone: %s", out)
	}
	two := append(append([]attentionItem{}, draft...), draft...)
	if out, _ := askFeedHTML(two, "", 0, designNow); !strings.Contains(out, `<span data-asks-drafts-n>2</span> reply drafts wait`) || !strings.Contains(out, `data-asks-drafts="2"`) {
		t.Errorf("two drafts: %s", out)
	}
	if out, _ := askFeedHTML(nil, "", 0, designNow); !strings.Contains(out, ">All clear</p>") || strings.Contains(out, "data-asks-drafts") {
		t.Errorf("nothing at all: %s", out)
	}
}
