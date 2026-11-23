package api

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/pages"
)

// requestWorld is the step-up draft fixture with session wakes on, and a
// SECOND request for the same draft from the same session (the session called
// plugin_draft_decide twice): two open cards, one draft.
func requestWorld(t *testing.T) (*stepUpDraftFixture, *wakeMirror, *attention.PGStore, int64) {
	t.Helper()
	f := newStepUpDraftFixture(t, "approve")
	att := attention.NewPG(f.pool)
	m := &wakeMirror{routeMirror: f.mirror, owner: f.owner, sid: otherRouteSID, since: time.Now().Add(-time.Hour).UnixMilli()}
	wakesOn(f.srv, f.pool, m, att, pages.NewPG(f.pool))
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM session_wakes WHERE owner_id = $1`, f.owner)
	})
	args := map[string]any{"id": f.draft.ID, "decision": "approve", "body_sha256": sha(f.body),
		"media_sha256": []string{sha("launch/hero-7"), sha("launch/demo-8")}}
	text, isErr := f.mcp(t, f.tok, otherRouteSID, "plugin_draft_decide", args)
	if isErr {
		t.Fatalf("the second decide: %s", text)
	}
	second := stepUpCardOf(t, text)
	if second <= f.card {
		t.Fatalf("fixture: the second card %d is not newer than %d", second, f.card)
	}
	return f, m, att, second
}

// TestTheOpenDraftCardKeepsTheRequestsDismissAndViewInSession (audit Y1): the
// request drawn as its draft's card on the Open feed keeps what its own card
// offered — "View in session" and "Dismiss without answering" with its form.
// Dismiss closes EVERY open request for the draft (the card stood for both),
// leaves the draft pending, and tells the session that raised them, once,
// that it was dismissed.
//
// RED AT 17f69e4: the merged card had no footer, and a Dismiss closed only
// the one card.
func TestTheOpenDraftCardKeepsTheRequestsDismissAndViewInSession(t *testing.T) {
	f, m, att, second := requestWorld(t)
	sid := strconv.FormatInt(second, 10)
	card := feedCard(f.page(t, attentionPath), second)
	if card == "" || !strings.Contains(card, "data-asks-request-draft") {
		t.Fatalf("the newest request is not drawn as its draft's card:\n%s", card)
	}
	if feedCard(f.page(t, attentionPath), f.card) != "" {
		t.Fatalf("the older request is drawn as well")
	}
	for _, want := range []string{
		`aria-label="Dismiss without answering"`,
		`<form id="ask-` + sid + `-dismiss" method="POST" action="/attention/asks/` + sid + `/cancel" hidden`,
		`data-asks-view-session>` + askViewInSession + `</a>`,
		`data-asks-footer`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("the merged card lacks %q:\n%s", want, card)
		}
	}
	w := f.ownerForm(t, "/attention/asks/"+sid+"/cancel", url.Values{}, sessionCookieFor(f.srv))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("the dismiss: %d %s", w.Code, w.Body)
	}
	for _, id := range []int64{f.card, second} {
		if a, err := att.Get(f.asOwner, id); err != nil || a.Status != attention.Cancelled {
			t.Errorf("request %d after the dismiss: %+v %v; want cancelled", id, a.Status, err)
		}
	}
	if r := f.row(t, f.draft.ID); r.status != "pending" {
		t.Errorf("the draft after the dismiss: %+v; want still pending", r)
	}
	probeTick(t, f.routeFixture)
	want := "muster: you dismissed step-up card #" + sid + ", which this session's plugin_draft_decide call raised, without confirming it: " +
		"the call did not run, and nothing was done."
	if got := m.taken(); len(got) != 1 || got[0].text != want {
		t.Errorf("the session took %+v; want exactly %q", got, want)
	}
	if c := feedCard(f.page(t, attentionPath), second); c != "" && !strings.Contains(c, "data-asks-settled") {
		t.Errorf("the dismissed request is still open on the feed:\n%s", c)
	}
}

// TestTwoRequestsFromOneSessionAreToldOnce (audit G2): the owner's approval of
// a draft that one session asked about twice settles both cards and tells
// that session ONE line, not one per card.
//
// RED AT 17f69e4: two pastes, one per card.
func TestTwoRequestsFromOneSessionAreToldOnce(t *testing.T) {
	f, m, att, second := requestWorld(t)
	w := f.ownerForm(t, "/plugins/drafts/"+strconv.FormatInt(f.draft.ID, 10)+"/approve", url.Values{"body_sha256": {sha(f.body)},
		"media_sha256": {sha("launch/hero-7") + "," + sha("launch/demo-8")}}, sessionCookieFor(f.srv))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("the approve: %d %s", w.Code, w.Body)
	}
	for _, id := range []int64{f.card, second} {
		if a, err := att.Get(f.asOwner, id); err != nil || a.Status != attention.Answered {
			t.Errorf("request %d after the approve: %+v %v; want answered", id, a.Status, err)
		}
	}
	probeTick(t, f.routeFixture)
	got := m.taken()
	if len(got) != 1 || !strings.Contains(got[0].text, "was settled when you approved the draft it is about on another door") {
		t.Errorf("the session took %d lines %+v; want exactly one", len(got), got)
	}
}

// TestTheOpenDraftCardIsReadLikeTheDraftsTabsCard (audit Y2): the request's
// draft on the Open feed is read through the same reader as the Drafts tab's
// card, so the two draw the same head — who drafted it — for one draft.
//
// RED AT 17f69e4: the Open card's draft skipped the name reads.
func TestTheOpenDraftCardIsReadLikeTheDraftsTabsCard(t *testing.T) {
	f := newDraftToolFixture(t)
	att := attention.NewPG(f.pool)
	f.srv.ext.Attention = att
	const body = "Noon works for me."
	id := f.draft(t, "rd-head", "someone", true, body)
	dc, err := f.plugins.DraftCard(f.asOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	post := draftPostCard(dc)
	a, err := att.Create(f.asOwner, attention.New{Asker: attention.Asker{System: true, Label: hostDecisionLabel}, Options: []string{hostDecisionConfirm, hostDecisionRefuse},
		StepUp: &attention.StepUpCard{Tool: pluginDraftDecideTool, Title: "Send it? · Draft #" + strconv.FormatInt(id, 10), Why: "W", Asker: "work:1",
			Preview: post.Preview, Venue: post.Venue, DraftID: id, Details: "D"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM attention_asks WHERE id = $1`, a.ID)
	})
	w := f.call(t, http.MethodGet, attentionPath, nil, map[string]string{"Cookie": sessionCookieFor(f.srv)})
	if w.Code != http.StatusOK {
		t.Fatalf("GET the Asks page: %d", w.Code)
	}
	page := w.Body.String()
	head := regexp.MustCompile(`data-draft-agent>([^<]*)<`)
	open := head.FindStringSubmatch(feedCard(page, a.ID))
	tab := head.FindStringSubmatch(draftCardOf(pageBetween(page, "asks-tabs-panel-drafts", ""), id))
	if open == nil || tab == nil {
		t.Fatalf("a card has no head: open %v, drafts %v", open, tab)
	}
	if open[1] != tab[1] {
		t.Errorf("the Open card names the drafter %q, the Drafts tab %q", open[1], tab[1])
	}
}
