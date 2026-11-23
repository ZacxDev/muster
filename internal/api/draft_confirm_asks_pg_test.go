package api

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/ui"
)

// draftCardOf is draft id's card on the Drafts tab of page html ("" when
// absent).
func draftCardOf(page string, id int64) string {
	open := `<li id="` + draftCardAnchor(id) + `"`
	i := strings.Index(page, open)
	if i < 0 {
		return ""
	}
	j := strings.Index(page[i:], "</li>\n")
	if j < 0 {
		return page[i:]
	}
	return page[i : i+j]
}

// askChipLinks is every linked ask chip in card, in order: its href and label.
var askChipLinkRE = regexp.MustCompile(`<a href="([^"]*)" class="[^"]*" data-id-link data-id-kind="ask" data-id="(\d+)" aria-label="Open ([^"]*)">([^<]*)</a>`)

type askChip struct {
	href, label string
	id          int64
}

func askChipLinks(t *testing.T, card string) []askChip {
	t.Helper()
	var out []askChip
	for _, m := range askChipLinkRE.FindAllStringSubmatch(card, -1) {
		id, _ := strconv.ParseInt(m[2], 10, 64)
		if m[3] != m[4] {
			t.Errorf("a chip's name %q is not its label %q", m[3], m[4])
		}
		out = append(out, askChip{href: html.UnescapeString(m[1]), label: m[4], id: id})
	}
	return out
}

// TestTheDraftsTabNamesEachDraftsConfirmAsks is the operator's "looking at asks
// drafts page and i only see the draft # not the ask #": each draft card on
// the Drafts tab names its confirm asks — the owner's asks whose step-up card
// is bound to it — beside its "Draft #N" chip, each a link to that ask's page
// labelled "Ask #<id>": open ones first, then newest, at most three, then
// "+N". A draft with none draws "Draft #N" alone. Another owner's card on the
// same draft id is never drawn.
//
// Ask ids are moved far from the drafts' (one sequence each), so no fixture id
// equals another.
//
// 🔴 RED AT origin/main: the card drew only "Draft #N".
func TestTheDraftsTabNamesEachDraftsConfirmAsks(t *testing.T) {
	f := newDraftToolFixture(t)
	att := attention.NewPG(f.pool)
	f.srv.ext.Attention = att
	dsn := dbtest.DSN(t)
	dbtest.OwnerExec(t, dsn, `SELECT setval(pg_get_serial_sequence('attention_asks', 'id'), coalesce((SELECT max(id) FROM attention_asks), 0) + 700000)`)
	t.Cleanup(func() {
		dbtest.OwnerExec(t, dsn, `DELETE FROM attention_asks WHERE owner_id IN (`+strconv.FormatInt(f.owner, 10)+`, `+strconv.FormatInt(f.stranger, 10)+`)`)
	})
	bare := f.draft(t, "cf-bare", "someone", true, "No asks about this one.")
	one := f.draft(t, "cf-one", "someone", true, "One ask about this one.")
	many := f.draft(t, "cf-many", "someone", true, "Five asks about this one.")
	strangerCtx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.stranger, Role: authz.RoleMember, Via: authz.ViaSession})
	card := func(ctx context.Context, draft int64) attention.Ask {
		t.Helper()
		a, err := att.Create(ctx, attention.New{Asker: attention.Asker{System: true, Label: hostDecisionLabel}, Options: []string{hostDecisionConfirm, hostDecisionRefuse},
			StepUp: &attention.StepUpCard{Tool: pluginDraftDecideTool, Title: "Send it? · Draft #" + strconv.FormatInt(draft, 10), Why: "W", Preview: "P", Details: "D", DraftID: draft}})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	single := card(f.asOwner, one)
	openOld := card(f.asOwner, many)
	answeredOld := card(f.asOwner, many)
	openNew := card(f.asOwner, many)
	cancelled := card(f.asOwner, many)
	answeredNew := card(f.asOwner, many)
	theirs := card(strangerCtx, many) // the newest, and another owner's
	for _, id := range []int64{answeredOld.ID, answeredNew.ID} {
		if _, err := att.Answer(f.asOwner, id, "Approved in the web UI"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := att.Cancel(f.asOwner, cancelled.ID); err != nil {
		t.Fatal(err)
	}
	ids := []int64{bare, one, many, single.ID, openOld.ID, answeredOld.ID, openNew.ID, cancelled.ID, answeredNew.ID, theirs.ID}
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("fixture ids are not pairwise distinct: %v", ids)
		}
		seen[id] = true
	}

	w := f.call(t, http.MethodGet, "/attention?tab=drafts", nil, map[string]string{"Cookie": sessionCookieFor(f.srv)})
	if w.Code != http.StatusOK {
		t.Fatalf("GET the Drafts tab: %d", w.Code)
	}
	page := w.Body.String()

	b := draftCardOf(page, bare)
	if b == "" || !strings.Contains(b, ui.DraftIDChipHTML(bare)) {
		t.Fatalf("the bare draft's card, with its id chip:\n%s", b)
	}
	if strings.Contains(b, "data-draft-asks") || len(askChipLinks(t, b)) != 0 || strings.Contains(b, "data-id-more") {
		t.Errorf("a draft with no confirm ask draws ask chips:\n%s", b)
	}

	o := draftCardOf(page, one)
	if got := askChipLinks(t, o); len(got) != 1 || got[0] != (askChip{href: "/attention/asks/" + strconv.FormatInt(single.ID, 10), label: "Ask #" + strconv.FormatInt(single.ID, 10), id: single.ID}) {
		t.Errorf("one confirm ask: %+v\n%s", got, o)
	}
	if !strings.Contains(o, ui.DraftIDChipHTML(one)) || strings.Contains(o, "data-id-more") {
		t.Errorf("one confirm ask: the draft chip, and no remainder:\n%s", o)
	}

	m := draftCardOf(page, many)
	got := askChipLinks(t, m)
	want := []int64{openNew.ID, openOld.ID, answeredNew.ID} // open first, newest; then the newest closed
	var gotIDs []int64
	for _, c := range got {
		gotIDs = append(gotIDs, c.id)
		if c.href != askPagePath(c.id) || c.href != "/attention/asks/"+strconv.FormatInt(c.id, 10) || c.label != "Ask #"+strconv.FormatInt(c.id, 10) {
			t.Errorf("chip %+v: want href /attention/asks/%d and label Ask #%d", c, c.id, c.id)
		}
	}
	if !slices.Equal(gotIDs, want) {
		t.Errorf("five confirm asks drew %v; want %v (open first, newest; three)", gotIDs, want)
	}
	if !strings.Contains(m, `data-id-more="2" title="2 more">+2</span>`) {
		t.Errorf("five confirm asks: no +2 remainder:\n%s", m)
	}
	if !strings.Contains(m, ui.DraftIDChipHTML(many)) {
		t.Errorf("the draft chip is gone:\n%s", m)
	}
	if strings.Contains(page, `data-id="`+strconv.FormatInt(theirs.ID, 10)+`"`) || strings.Contains(page, askPagePath(theirs.ID)+`"`) {
		t.Errorf("another owner's card on draft %d is drawn", many)
	}
}

// TestADraftsOpenRequestIsOneCardAndTheOwnersDecisionClosesIt is the operator's
// "it feels redundant to have a draft and a confirm ask, i should just approve
// the draft directly": a session's plugin_draft_decide on the owner's machine
// files a step-up card (the phone must sign it). On the Asks page it is ONE
// card — the draft's own, on the Drafts tab, naming the request and its ask's
// chip — and not also its own card on the Open feed, nor counted twice. The
// gate is unchanged: the browser still cannot answer the card. The owner's own
// Approve on the draft's card decides the draft, closes the card, and the
// session that asked is told exactly once; the phone's Confirm afterwards is
// refused.
//
// 🔴 RED AT origin/main: the card was on the Open feed as well, and the
// draft's card did not name it.
func TestADraftsOpenRequestIsOneCardAndTheOwnersDecisionClosesIt(t *testing.T) {
	f := newStepUpDraftFixture(t, "approve")
	att := attention.NewPG(f.pool)
	m := &wakeMirror{routeMirror: f.mirror, owner: f.owner, sid: otherRouteSID, since: time.Now().Add(-time.Hour).UnixMilli()}
	wakesOn(f.srv, f.pool, m, att, pages.NewPG(f.pool))
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM session_wakes WHERE owner_id = $1`, f.owner)
	})
	cid, did := strconv.FormatInt(f.card, 10), strconv.FormatInt(f.draft.ID, 10)

	countRE := regexp.MustCompile(`data-asks-open-count[^>]*>(\d+) open<`)
	open := f.page(t, attentionPath)
	if c := feedCard(open, f.card); c != "" {
		t.Errorf("the request is its own card on the Open feed too:\n%s", c)
	}
	n := countRE.FindStringSubmatch(open)
	if n == nil {
		t.Fatalf("no open count on the page")
	}
	before, _ := strconv.Atoi(n[1])

	drafts := f.page(t, "/attention?tab=drafts")
	card := draftCardOf(drafts, f.draft.ID)
	if card == "" {
		t.Fatalf("the draft has no card on the Drafts tab")
	}
	filed, err := att.Get(f.asOwner, f.card)
	if err != nil || filed.StepUp == nil {
		t.Fatalf("the card: %+v %v", filed, err)
	}
	who := draftRequestSession
	if filed.StepUp.Asker != "" {
		who = html.EscapeString(filed.StepUp.Asker)
	}
	line := `<p class="break-words text-sm text-fg2" data-draft-request data-ask-id="` + cid + `">` + who + draftRequestApprove + draftRequestDecide + `</p>`
	if !strings.Contains(card, line) {
		t.Errorf("the draft's card does not name the request (%s):\n%s", line, card)
	}
	if chips := askChipLinks(t, card); len(chips) != 1 || chips[0].id != f.card || chips[0].href != "/attention/asks/"+cid {
		t.Errorf("the draft's card's ask chips: %+v; want Ask #%s", chips, cid)
	}
	if strings.Count(drafts, `data-draft-id="`+did+`"`) != 1 {
		t.Errorf("draft %s is drawn %d times on the Drafts page", did, strings.Count(drafts, `data-draft-id="`+did+`"`))
	}
	if !strings.Contains(card, `action="/plugins/drafts/`+did+`/approve"`) {
		t.Fatalf("the draft's card offers no Approve:\n%s", card)
	}

	// The gate: the browser cannot answer the card.
	if _, err := f.srv.answerAskFrom(probeOwner(f.routeFixture), f.card, hostDecisionConfirm, attention.AnswerSource{}); !errors.Is(err, errHostDecisionNeedsPhone) {
		t.Fatalf("a browser answer of the card: %v", err)
	}
	if a, err := att.Get(f.asOwner, f.card); err != nil || a.Status != attention.Open {
		t.Fatalf("the card after the browser's try: %+v %v", a, err)
	}

	// The owner approves the draft on its own card.
	w := f.ownerForm(t, "/plugins/drafts/"+did+"/approve", url.Values{"body_sha256": {sha(f.body)},
		"media_sha256": {sha("launch/hero-7") + "," + sha("launch/demo-8")}}, sessionCookieFor(f.srv))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("the approve: %d %s", w.Code, w.Body)
	}
	if r := f.row(t, f.draft.ID); r.status != "approved" {
		t.Fatalf("the draft after the approve: %+v", r)
	}
	a, err := att.Get(f.asOwner, f.card)
	if err != nil || a.Status != attention.Answered || a.Answer != "Approved in the web UI" {
		t.Fatalf("the card after the approve: %+v %v; want answered %q", a, err, "Approved in the web UI")
	}
	probeTick(t, f.routeFixture)
	wantPaste := "muster: step-up card #" + cid + ", which this session's plugin_draft_decide call raised, was settled when you approved the draft it is about on another door; " +
		"the call itself was not run. Read the draft with the list tools."
	if got := m.taken(); len(got) != 1 || got[0].text != wantPaste {
		t.Errorf("the session took %+v; want exactly %q", got, wantPaste)
	}
	var wakes int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM session_wakes WHERE owner_id = $1`, f.owner).Scan(&wakes); err != nil || wakes != 1 {
		t.Errorf("session wakes filed: %d %v; want 1", wakes, err)
	}
	// The phone's Confirm afterwards decides nothing.
	if _, err := f.srv.answerAskFrom(asDevice(f.owner), f.card, hostDecisionConfirm, attention.AnswerSource{}); err == nil {
		t.Error("the phone confirmed a card the owner's decision closed")
	}
	// Decided, the draft's card names no open request; the count did not grow.
	after := draftCardOf(f.page(t, "/attention?tab=drafts"), f.draft.ID)
	if strings.Contains(after, "data-draft-request") {
		t.Errorf("a decided draft still names the request:\n%s", after)
	}
	if n := countRE.FindStringSubmatch(f.page(t, attentionPath)); n == nil {
		t.Error("no open count after the approve")
	} else if c, _ := strconv.Atoi(n[1]); c >= before {
		t.Errorf("the open count went %d -> %d; want it to fall (the draft decided)", before, c)
	}
}
