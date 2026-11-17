package api

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/asklog"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// TestEverySurfaceShowsTheSameOpenCount: ONE state — an agent's open ask, the
// fixture's waiting window, a pending reply draft, and the ask log holding
// four stale rows that never got an outcome — and every surface that says how
// much is open says the same literal 3: the Asks page's header, the sidebar's
// More badge (/attention/badge) and the ☰ menu badge (/ui/nav/waiting). (The
// Log tab's By-outcome "Open" pill is the log's own breakdown: 5.) And the Open feed is not "All clear" while a draft
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
	// The Log's By-outcome pills are the LOG's own breakdown (they add up to its
	// Total): its "Open" is the rows with no outcome yet — the four stale rows
	// and the live ask's — never the live open count (audit of #787, 🟡4).
	logPanel := f.page(t, "/attention?tab=log")
	if got := regexp.MustCompile(`data-count="open">Open (\d+)</span>`).FindStringSubmatch(logPanel); got == nil || got[1] != "5" {
		t.Errorf("the Log's Open pill: %v, want the log's own 5", got)
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
		!strings.Contains(out, `<span data-asks-drafts-n>1</span> draft waits for your decision.`) ||
		!strings.Contains(out, `href="/attention?tab=drafts"`) || !strings.Contains(out, `<li hidden aria-hidden="true" data-asks-drafts="1"></li>`) {
		t.Fatalf("a draft alone: %s", out)
	}
	two := append(append([]attentionItem{}, draft...), draft...)
	if out, _ := askFeedHTML(two, "", 0, designNow); !strings.Contains(out, `<span data-asks-drafts-n>2</span> drafts wait`) || !strings.Contains(out, `data-asks-drafts="2"`) {
		t.Errorf("two drafts: %s", out)
	}
	if out, _ := askFeedHTML(nil, "", 0, designNow); !strings.Contains(out, ">All clear</p>") || strings.Contains(out, "data-asks-drafts") {
		t.Errorf("nothing at all: %s", out)
	}
}

// blockAgent marks agent id as blocked at a permission prompt, as POST
// /agent/blocked records it (the "awaiting approval" badge).
func blockAgent(s *Server, id int64) {
	l := &s.approvals
	l.mu.Lock()
	defer l.mu.Unlock()
	l.init()
	now := s.now()
	l.blocked[id] = blockedReport{since: now, seen: now}
}

// TestAnAgentAwaitingApprovalIsOneOpenItem (audit of #787, 🟡1): an agent
// blocked at a permission prompt with no ask open is waiting on its owner — the
// sidebar marks its row, the old ☰ counted it — so the ONE OPEN COUNT counts it
// (the ☰ badge and the page header both say 1) and the Open tab lists it, as a
// waiting card whose View in session opens the agent. RED before: the count
// read only the asks queue, so the ☰ said 0 for a session blocked on you.
func TestAnAgentAwaitingApprovalIsOneOpenItem(t *testing.T) {
	f := newAttnFixture(t)
	f.setWindows(mirror.StatusIdle) // no waiting window: the blocked agent is the one item
	blockAgent(f.srv, f.op.ID)
	if got := regexp.MustCompile(`id="nav-waiting-badge" data-nav-waiting="" data-count="(\d+)"`).FindStringSubmatch(f.page(t, ui.NavWaitingPath)); got == nil || got[1] != "1" {
		t.Errorf("the ☰ badge: %v, want 1", got)
	}
	page := f.page(t, "/attention")
	if !strings.Contains(page, `data-asks-open-count aria-live="polite">1 open</span>`) {
		t.Errorf("the page header: %s", regexp.MustCompile(`data-asks-open-count[^<]*<`).FindString(page))
	}
	card := itemLi(t, askList(page), "approval-"+strconv.FormatInt(f.op.ID, 10))
	for _, want := range []string{`data-kind="session"`, `href="/agents/` + f.op.Name + `"`, `data-asks-view-session`} {
		if !strings.Contains(card, want) {
			t.Errorf("the blocked agent's card lacks %q:\n%s", want, card)
		}
	}
	// One item per agent: an open ask of the same agent is the card, not two.
	f.ask(t, f.opTok, "Ship it?")
	if page := f.page(t, "/attention"); strings.Contains(askList(page), `id="`+"approval-"+strconv.FormatInt(f.op.ID, 10)+`"`) ||
		!strings.Contains(page, `data-asks-open-count aria-live="polite">1 open</span>`) {
		t.Errorf("a blocked agent with an open ask is drawn or counted twice:\n%s", askList(page))
	}
}

// TestAPairingRequestIsNotADraft (audit of #787, 🟡2): a plugin's pairing
// request is an attention item of the plugin kind, as a reply draft is, but
// the Drafts tab does not list it. It is never counted as a draft ("1 draft
// waits", a link to a tab that does not show it): it is its own card on the
// Open feed, linking to where it is approved. And the empty state says
// "drafts", not "reply drafts" (an officer's post is a draft too). RED before:
// a pairing request alone drew "1 reply draft waits for your decision.".
func TestAPairingRequestIsNotADraft(t *testing.T) {
	pair := attentionItem{Key: pairKey(9), Kind: attentionKindPlugin, Source: "plugin pairing", Text: "A plugin named inbox asks for a token",
		Since: designNow.Add(-time.Minute), Link: pairLink(9)}
	out, n := askFeedHTML([]attentionItem{pair}, "", 0, designNow)
	if strings.Contains(out, "data-asks-drafts") || strings.Contains(out, "draft") || n != 1 {
		t.Errorf("a pairing request alone (%d cards) is counted as a draft:\n%s", n, out)
	}
	card := itemLi(t, out, pairKey(9))
	if !strings.Contains(card, `data-asks-card`) || !strings.Contains(card, `href="`+pairLink(9)+`"`) {
		t.Errorf("the pairing request is not a card linking to its approval:\n%s", card)
	}
	// Beside a draft: one draft, and the pairing request its card.
	out, _ = askFeedHTML([]attentionItem{pair, designItems()[5]}, "", 0, designNow)
	if !strings.Contains(out, `data-asks-drafts="1"`) || !strings.Contains(out, `id="`+pairKey(9)+`"`) {
		t.Errorf("a draft and a pairing request:\n%s", out)
	}
}
