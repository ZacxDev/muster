package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/fleet"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/push"
)

// THE FLEET PUSH'S TAP, AND WHAT IT SAYS (docs/chief.md "Fleet proposals").
//
// Before: the batched push said "Your chief proposed what to send your
// sessions…" (no proposal, no session) and opened `/?chief=open` — which home
// redirected to the chief's own chat page, dropping the parameter, so the
// panel and its digest never opened; 18 of 19 proposals expired undecided.
// Its Attention notice linked nowhere. Every name below is invented.

// pushesTo waits for owner's n-th push and returns them all.
func pushesTo(t *testing.T, cp *morningPush, owner int64, n int) []push.Payload {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := cp.to(owner)
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited for push %d; got %d: %+v", n, len(got), got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTheFleetPushNamesTheNewestProposalAndOpensIt: the push names what the
// newest proposal is and the session it is for (its name as the digest card
// shows it, capped, with no straight quote and no control character), "+N
// more." for the others — never a message's text or its reason — and its tap
// opens the digest's Proposed group on that proposal. No action, the tag that
// replaces it.
//
// Red at origin/main: the body was the generic sentence and the url
// "/?chief=open".
func TestTheFleetPushNamesTheNewestProposalAndOpensIt(t *testing.T) {
	f := newProposeFixture(t)
	cp := &morningPush{}
	f.srv.ext.Push = cp
	s1, k1, _ := f.stopped(t, f.owner)
	if _, err := f.focus.SetManualName(f.asOwner, focus.MirrorSubject(k1), "Billing migration"); err != nil {
		t.Fatal(err)
	}
	first := f.propose(t, s1, fleet.ClassContinue)
	f.pass()
	p := pushesTo(t, cp, f.owner, 1)[0]
	if p.Title != "1 proposal waiting" || p.Tag != "fleet-proposals" || p.AskActions || p.Ask != nil {
		t.Fatalf("the push: %+v", p)
	}
	if want := "Continue 'Billing migration'? Nothing is sent until you approve it."; p.Body != want {
		t.Errorf("the body: %q\nwant %q", p.Body, want)
	}
	if want := "/tasks?chief=open&fleet=proposed&proposal=" + strconv.FormatInt(first, 10); p.Data["url"] != want || len(p.Data) != 1 {
		t.Errorf("the url: %v, want only url=%q", p.Data, want)
	}

	// A second, on a session whose name is long and has a quote and a tab:
	// the newest is named, cut to 40 runes, and the first is "+1 more".
	s2, k2, _ := f.stopped(t, f.owner)
	if _, err := f.focus.SetManualName(f.asOwner, focus.MirrorSubject(k2), "Bob's ledger\treconciliation for the quarterly close"); err != nil {
		t.Fatal(err)
	}
	second := f.propose(t, s2, fleet.ClassStopNote)
	dateBackPush(t, f, 31*time.Minute)
	f.pass()
	p = pushesTo(t, cp, f.owner, 2)[1]
	if p.Title != "2 proposals waiting" {
		t.Errorf("the title: %q", p.Title)
	}
	if want := "Stop 'Bob’s ledger reconciliation for the qua…' after this step? +1 more. Nothing is sent until you approve them."; p.Body != want {
		t.Errorf("the body: %q\nwant %q", p.Body, want)
	}
	if want := "/tasks?chief=open&fleet=proposed&proposal=" + strconv.FormatInt(second, 10); p.Data["url"] != want {
		t.Errorf("the url: %q, want %q", p.Data["url"], want)
	}
	// No session content: neither proposal's text nor its reason.
	for _, id := range []int64{first, second} {
		a := f.action(t, id)
		if strings.Contains(p.Body, a.Text) || strings.Contains(p.Body, a.Reason) {
			t.Errorf("the body carries proposal %d's text or reason: %q", id, p.Body)
		}
	}
}

// TestAChiefOpenLinkReachesThePanel: `/?chief=open…` (a push already on a
// phone) goes to the board with its query — whose shell draws the chief
// panel — not to the chief's own chat page, which has none; a plain `/` is
// still the chief's chat.
//
// Red at origin/main: 302 to the chief's chat page, the query dropped.
func TestAChiefOpenLinkReachesThePanel(t *testing.T) {
	withChief := &homeAgents{rows: homeRows(true), latest: map[int64]int64{homeChiefID: homeChiefSess, homeWorkerID: 55}}
	s, h := homeServer(t, withChief)
	for path, want := range map[string]string{
		"/?chief=open":                           "/tasks?chief=open",
		"/?chief=open&fleet=proposed&proposal=9": "/tasks?chief=open&fleet=proposed&proposal=9",
		"/":                                      chiefHref,
	} {
		rec := homeGet(s, h, path, nil)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != want {
			t.Errorf("GET %s = %d → %q, want 302 → %q", path, rec.Code, rec.Header().Get("Location"), want)
		}
	}
	// Where it lands draws the panel (the chief's chat page does not).
	rec := homeGet(s, h, "/tasks?chief=open&fleet=proposed&proposal=9", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="chief-panel"`) || !strings.Contains(rec.Body.String(), `id="panel-tasks"`) {
		t.Errorf("GET the push's landing = %d; want 200, the board with the chief panel", rec.Code)
	}
}

// TestTheFleetNoticeLinksTheDigest: the "N proposals waiting" Attention
// item's link (its card's head) and its tap open the digest's Proposed group.
//
// Red at origin/main: no link at all, and the tap opened the notice's own page.
func TestTheFleetNoticeLinksTheDigest(t *testing.T) {
	f := newProposeFixture(t)
	s1, _, _ := f.stopped(t, f.owner)
	f.propose(t, s1, fleet.ClassContinue)
	f.pass()
	open, err := f.srv.ext.Attention.Open(f.asOwner)
	if err != nil {
		t.Fatal(err)
	}
	var notice attention.Ask
	for _, a := range open {
		if a.Kind == attention.KindNotice && a.Asker.Label == "Fleet proposals" {
			notice = a
		}
	}
	if notice.ID == 0 {
		t.Fatalf("no fleet notice among %+v", open)
	}
	const want = "/tasks?chief=open&fleet=proposed"
	items := []attentionItem{f.srv.askItemNow(f.asOwner, notice)}
	f.srv.withAskBoxes(f.asOwner, items, true)
	if items[0].sessionLink != want {
		t.Errorf("the notice's link: %q, want %q", items[0].sessionLink, want)
	}
	if got := f.srv.openTarget(f.asOwner, attentionAskKey(notice.ID)); got != want {
		t.Errorf("the notice's tap: %q, want %q", got, want)
	}
}

// fleetCardFor is the digest card GET'd with query, as the owner.
func (f *proposeFixture) fleetCardFor(t *testing.T, query string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/ui/chief/fleet"+query, nil)
	rec := httptest.NewRecorder()
	f.srv.handleFleetDigest(rec, req.WithContext(f.asOwner))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET the card%s: %d %s", query, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

var (
	proposedDetails  = regexp.MustCompile(`<details data-fleet-group="proposed"[^>]*>`)
	proposedSheet    = regexp.MustCompile(`<div id="fleet-sheet-proposed"[^>]*>`)
	highlightedCards = regexp.MustCompile(`<li data-fleet-proposal="(\d+)"[^>]*data-fleet-highlight[^>]*>`)
	goneLine         = regexp.MustCompile(`<p data-fleet-gone[^>]*>([^<]*)(?:<time data-fleet-clock="(\d+)"[^>]*>[^<]*</time>)?([^<]*)</p>`)
)

// TestAFleetLinkOpensTheProposedGroupOnItsProposal: the card asked for with
// ?fleet=proposed&proposal=<id> draws the Proposed group open (the desktop's
// <details>, the phone's sheet) and that proposal highlighted; one that no
// longer waits draws the line saying so — rejected, expired at its 12 h — with
// the newest waiting one highlighted, or none. Without the parameter, nothing
// is open or highlighted. Each card says when it expires.
//
// Red at origin/main: the parameter was ignored (the group drawn closed).
func TestAFleetLinkOpensTheProposedGroupOnItsProposal(t *testing.T) {
	f := newProposeFixture(t)
	s1, _, _ := f.stopped(t, f.owner)
	s2, _, _ := f.stopped(t, f.owner)
	s3, _, _ := f.stopped(t, f.owner)
	older := f.propose(t, s1, fleet.ClassContinue)
	f.at = f.at.Add(time.Minute)
	middle := f.propose(t, s2, fleet.ClassHandoff)
	f.at = f.at.Add(time.Minute)
	newest := f.propose(t, s3, fleet.ClassStopNote)
	id := func(n int64) string { return strconv.FormatInt(n, 10) }
	highlighted := func(card string) []string {
		var out []string
		for _, m := range highlightedCards.FindAllStringSubmatch(card, -1) {
			out = append(out, m[1])
		}
		return out
	}
	open := func(t *testing.T, card string) {
		t.Helper()
		d, s := proposedDetails.FindString(card), proposedSheet.FindString(card)
		if !strings.Contains(d, ` open=""`) || s == "" || strings.Contains(s, ` hidden=""`) {
			t.Errorf("the Proposed group is not open: details %q, sheet %q", d, s)
		}
	}

	// Control: no parameter, nothing open or highlighted.
	card := f.fleetCardFor(t, "")
	if d := proposedDetails.FindString(card); strings.Contains(d, ` open=""`) || len(highlighted(card)) != 0 || goneLine.MatchString(card) {
		t.Fatalf("a plain card: details %q, highlighted %v", d, highlighted(card))
	}
	// Each card says when it expires: 12 h after it was made, the clock in UTC
	// until the panel script draws the reader's.
	if !strings.Contains(card, `expires in 11h (<time data-fleet-clock="`+strconv.FormatInt(f.action(t, older).CreatedAt.Add(12*time.Hour).UnixMilli(), 10)+`"`) {
		t.Errorf("the oldest card's expiry line is missing: %.3000s", card)
	}

	// The middle one, named: open, it alone highlighted (on both drawings).
	card = f.fleetCardFor(t, "?fleet=proposed&proposal="+id(middle))
	open(t, card)
	if got := highlighted(card); len(got) != 2 || got[0] != id(middle) || got[1] != id(middle) {
		t.Errorf("highlighted %v, want proposal %d (desktop and phone)", got, middle)
	}
	// None named (the notice's link): the newest.
	card = f.fleetCardFor(t, "?fleet=proposed")
	open(t, card)
	if got := highlighted(card); len(got) != 2 || got[0] != id(newest) {
		t.Errorf("no proposal named: highlighted %v, want the newest %d", got, newest)
	}

	// Rejected: the line, when, and the newest highlighted.
	f.srv.fleetReject(f.asOwner, middle, fleet.ViaWeb)
	card = f.fleetCardFor(t, "?fleet=proposed&proposal="+id(middle))
	open(t, card)
	m := goneLine.FindStringSubmatch(card)
	if m == nil || m[1] != "That proposal was already rejected at " || m[2] == "" || m[3] != "." {
		t.Errorf("the rejected line: %q", m)
	}
	if got := highlighted(card); len(got) != 2 || got[0] != id(newest) {
		t.Errorf("after the reject: highlighted %v, want the newest %d", got, newest)
	}

	// 13 h on, everything has expired: the line says when (its 12 h, not
	// when the draw noticed), and "None." — nothing highlighted.
	created := f.action(t, older).CreatedAt
	f.at = f.at.Add(13 * time.Hour)
	card = f.fleetCardFor(t, "?fleet=proposed&proposal="+id(older))
	open(t, card)
	m = goneLine.FindStringSubmatch(card)
	if m == nil || m[1] != "That proposal expired at " || m[2] != strconv.FormatInt(created.Add(12*time.Hour).UnixMilli(), 10) || m[3] != "." {
		t.Errorf("the expired line: %q (want the time %d)", m, created.Add(12*time.Hour).UnixMilli())
	}
	if got := highlighted(card); len(got) != 0 || !strings.Contains(card, "None.") {
		t.Errorf("all expired: highlighted %v", got)
	}
	// None named and none waiting.
	card = f.fleetCardFor(t, "?fleet=proposed")
	if m := goneLine.FindStringSubmatch(card); m == nil || m[1] != "No proposal is waiting now." || m[2] != "" {
		t.Errorf("none waiting: %q", m)
	}
	// Another owner's id, or none such: no waiting, no time.
	card = f.fleetCardFor(t, "?fleet=proposed&proposal=999999999")
	if m := goneLine.FindStringSubmatch(card); m == nil || m[1] != "That proposal is no longer waiting." || m[2] != "" {
		t.Errorf("an unknown id: %q", m)
	}
}
