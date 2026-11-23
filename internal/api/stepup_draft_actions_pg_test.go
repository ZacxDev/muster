package api

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// A STEP-UP CARD ABOUT A DRAFT CARRIES THE DRAFT'S OWN DECISION
// (stepup_draft_actions.go), over Postgres through the real routes: a host
// session's plugin_draft_decide on a host officer's machine files a card; the
// Open feed's card and the card's page draw the DRAFT's Approve and send
// (bound to the draft's body and media SHA-256) and Reject with its reason,
// posting to /plugins/drafts/{id}/… and returning to the Asks page — never the
// card's own Confirm/Refuse, which the browser still cannot answer. Approving
// there approves the draft, settles the card (it leaves the feed), and a later
// Confirm on the phone is refused as closed. Another owner's card that names
// the draft never gets the controls.

// stepUpDraftFixture is an outbound draft of the owner's, with two uploaded
// media and its plugin's check passed, and a step-up card a host session
// filed to approve it.
type stepUpDraftFixture struct {
	*draftToolFixture
	draft plugins.Draft
	card  int64
	body  string
}

const stepUpDraftBody = "The launch post, with its two media."

func newStepUpDraftFixture(t *testing.T, decision string) *stepUpDraftFixture {
	t.Helper()
	return newStepUpDraftFixtureFor(t, decision, "example.forum", stepUpDraftBody)
}

// newStepUpDraftFixtureFor is the fixture with the draft posted to venue with
// body.
func newStepUpDraftFixtureFor(t *testing.T, decision, venue, body string) *stepUpDraftFixture {
	t.Helper()
	f := newDraftToolFixture(t)
	f.srv.ext.Attention = attention.NewPG(f.pool)
	dsn := dbtest.DSN(t)
	dbtest.OwnerExec(t, dsn, `UPDATE plugins SET deliver_to = 'officer:example:main', capabilities = '{messages,draft_checks}' WHERE id = `+
		strconv.FormatInt(f.plugin.ID, 10))
	t.Cleanup(func() {
		dbtest.OwnerExec(t, dsn, `UPDATE plugins SET deliver_to = NULL WHERE id = `+strconv.FormatInt(f.plugin.ID, 10))
	})
	d, err := f.plugins.FileOutbound(f.asOwner, f.chief.ID, f.owner, f.chief.Name, plugins.NewOutbound{Officer: "example:main",
		Plugin: f.plugin.Name, Venue: venue, Target: plugins.OutboundTarget{Kind: plugins.TargetPost}, Body: body,
		Media: []string{"launch/hero-7", "launch/demo-8"}})
	if err != nil {
		t.Fatalf("file an outbound draft: %v", err)
	}
	for i, m := range []struct{ ref, ct string }{{"launch/hero-7", "image/webp"}, {"launch/demo-8", "video/mp4"}} {
		if _, _, err := f.plugins.UploadDraftMedia(f.asOwner, f.plugin.ID, d.ID, plugins.DraftMedia{Ref: m.ref, SHA256: sha(m.ref),
			ContentType: m.ct, Size: int64(30 + i), Key: plugins.MediaKeyPrefix + "s/" + strconv.Itoa(i)}); err != nil {
			t.Fatalf("upload %s: %v", m.ref, err)
		}
	}
	if _, _, err := f.plugins.CheckDraft(f.asOwner, f.plugin.ID, d.ID, plugins.DraftCheck{Sendable: true}); err != nil {
		t.Fatalf("check: %v", err)
	}
	declareHostOfficerOn(t, f.srv, f.owner, "tower")
	args := map[string]any{"id": d.ID, "decision": decision, "body_sha256": sha(body),
		"media_sha256": []string{sha("launch/hero-7"), sha("launch/demo-8")}}
	text, isErr := f.mcp(t, f.tok, otherRouteSID, "plugin_draft_decide", args)
	if isErr {
		t.Fatalf("decide on the host officer's machine: %s", text)
	}
	card := stepUpCardOf(t, text)
	if r := f.row(t, d.ID); r.status != "pending" {
		t.Fatalf("the draft after the gated decide: %+v", r)
	}
	return &stepUpDraftFixture{draftToolFixture: f, draft: d, card: card, body: body}
}

// page GETs path as the browser the fixture's session override names.
func (f *stepUpDraftFixture) page(t *testing.T, path string) string {
	t.Helper()
	w := f.call(t, http.MethodGet, path, nil, map[string]string{"Cookie": sessionCookieFor(f.srv)})
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
	}
	return w.Body.String()
}

// feedCard is ask id's card on the Open feed page html ("" when absent).
func feedCard(html string, id int64) string {
	open := `<li id="` + attentionAskKey(id) + `"`
	i := strings.Index(html, open)
	if i < 0 {
		return ""
	}
	j := strings.Index(html[i:], "</li>\n")
	if j < 0 {
		return html[i:]
	}
	return html[i : i+j]
}

// assertDraftControls checks one face's draft controls: the approve form
// posts to the draft's approve route with the draft's own body and media
// SHA-256 (computed HERE) and back, Reject posts to its reject route with its
// reason box and back, and the card's own answer form is absent.
func assertDraftControls(t *testing.T, face, html string, f *stepUpDraftFixture, back string) {
	t.Helper()
	id := strconv.FormatInt(f.draft.ID, 10)
	want := []string{
		`action="/plugins/drafts/` + id + `/approve"`,
		`<input type="hidden" name="body_sha256" value="` + sha(f.body) + `">`,
		`<input type="hidden" name="media_sha256" value="` + sha("launch/hero-7") + "," + sha("launch/demo-8") + `">`,
		`action="/plugins/drafts/` + id + `/reject"`,
		`name="reason"`,
		`<input type="hidden" name="return" value="` + back + `">`,
		`data-draft-approve>Approve and send</button>`,
		`data-draft-reject>Reject</button>`,
	}
	// The line above them: on the Open feed the request is its draft's own
	// card, naming what the session asked (one draft, one card:
	// asks_drafts.go); on the card's page the step-up card's own line.
	if strings.Contains(html, "data-asks-request-draft") {
		want = append(want, draftRequestApprove+draftRequestDecide)
	} else {
		want = append(want, askStepUpDraftLine)
	}
	for _, w := range want {
		if !strings.Contains(html, w) {
			t.Errorf("%s: missing %q", face, w)
		}
	}
	// 🔴 The card itself stays phone-only: no answer form to its own route.
	if strings.Contains(html, `/attention/asks/`+strconv.FormatInt(f.card, 10)+`/answer`) {
		t.Errorf("%s: the step-up card's own answer form is drawn", face)
	}
	for _, opt := range []string{`value="` + hostDecisionConfirm + `"`, `value="` + hostDecisionRefuse + `"`} {
		if strings.Contains(html, opt) {
			t.Errorf("%s: the card's option %s is drawn", face, opt)
		}
	}
}

// TestAStepUpCardAboutADraftDrawsTheDraftsDecision: the Open feed's card and
// the card's page draw the draft's approve and reject forms with the exact
// binding and a return to where they were drawn, and no answer to the card;
// the browser's answer to the card is still refused.
//
// 🔴 RED AT origin/main: both faces drew only "Confirm this on your phone."
func TestAStepUpCardAboutADraftDrawsTheDraftsDecision(t *testing.T) {
	f := newStepUpDraftFixture(t, "approve")
	card := feedCard(f.page(t, attentionPath), f.card)
	if card == "" {
		t.Fatal("the step-up card is not on the Open feed")
	}
	assertDraftControls(t, "the Open feed's card", card, f, attentionPath)
	// 🔴 THE SAME CONTROLS AS THE DRAFTS TAB'S: the card holds exactly what the
	// one shared function draws for this draft in this place.
	dc, err := f.plugins.DraftCard(f.asOwner, f.draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The place is the card's own: its id rides on the forms (a decision lands
	// on the card settled) and names the reason box, beside the server's mic.
	at := cardPlace{back: attentionPath, card: f.card, dictate: f.srv.dictatePath(), dictateOff: f.srv.dictateOff()}
	if shared := draftDecisionHTML(dc, time.Now(), at); !strings.Contains(card, shared) {
		t.Errorf("the feed card's controls are not draftDecisionHTML's:\n%s", shared)
	}
	page := f.page(t, askPagePath(f.card))
	assertDraftControls(t, "the card's page", page, f, askPagePath(f.card))
	// The page carries the confirm dialog the approve form's data-step-up-form needs.
	if !strings.Contains(page, ui.StepUpFormScript) {
		t.Error("the card's page has no step-up confirm dialog for the draft's approve")
	}
	// The browser's answer to the card itself is refused, as before.
	if _, err := f.srv.answerAskFrom(asUser(f.owner), f.card, hostDecisionConfirm, attention.AnswerSource{}); !errors.Is(err, errHostDecisionNeedsPhone) {
		t.Fatalf("a browser answer of the card: %v", err)
	}
}

// TestARejectStepUpCardLeadsWithTheDraftsReject: a "Reject this draft?" card
// draws Reject as the primary action, and Approve (the draft is pending and
// approvable) after it.
func TestARejectStepUpCardLeadsWithTheDraftsReject(t *testing.T) {
	f := newStepUpDraftFixture(t, "reject")
	card := feedCard(f.page(t, attentionPath), f.card)
	// CHANGED: the request is its draft's own card (one draft, one card), so
	// its line names the request.
	if !strings.Contains(card, draftRequestReject+".") || !strings.Contains(card, "data-asks-request-draft") {
		t.Fatalf("the reject card's line: %s", card)
	}
	ri, ai := strings.Index(card, "data-draft-reject>"), strings.Index(card, "data-draft-approve>")
	if ri < 0 || ai < 0 || ri > ai {
		t.Fatalf("reject at %d, approve at %d: want both, Reject first", ri, ai)
	}
	// Reject is drawn as the primary button.
	rejectBtn := card[strings.LastIndex(card[:ri], "<button"):ri]
	if !strings.Contains(rejectBtn, `class="`+ui.AsksPrimaryButtonClass+`"`) {
		t.Fatalf("Reject is not the primary action: %s", rejectBtn)
	}
}

// TestApprovingAStepUpCardsDraftSettlesTheCard: the feed's approve form,
// posted as the browser posts it (with its card), approves the draft and
// returns to the Asks page (not the Drafts tab) with the card drawn SETTLED in
// its old spot — answered with what happened and where — and the phone's
// Confirm afterwards is refused as closed — the draft is approved once. On
// the next load of the page it has left the Open feed.
//
// 🔴 RED AT origin/main: the card stayed open after the draft was decided.
// CHANGED with the settled card (asks_done.go): the approve used to land on
// the bare Asks page, where the card was simply gone — now it lands on
// settledLink and the card is there, settled; only a plain reload drops it.
func TestApprovingAStepUpCardsDraftSettlesTheCard(t *testing.T) {
	f := newStepUpDraftFixture(t, "approve")
	form := url.Values{"return": {attentionPath}, "card": {strconv.FormatInt(f.card, 10)}, "body_sha256": {sha(f.body)},
		"media_sha256": {sha("launch/hero-7") + "," + sha("launch/demo-8")}}
	w := f.ownerForm(t, "/plugins/drafts/"+strconv.FormatInt(f.draft.ID, 10)+"/approve", form, sessionCookieFor(f.srv))
	landing := "/attention?settled=" + strconv.FormatInt(f.card, 10) + "#ask-" + strconv.FormatInt(f.card, 10)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != landing {
		t.Fatalf("the approve: %d to %q, want 303 to %s (%s)", w.Code, w.Header().Get("Location"), landing, w.Body)
	}
	if r := f.row(t, f.draft.ID); r.status != "approved" || r.via != "browser" {
		t.Fatalf("the draft after the approve: %+v", r)
	}
	got, err := f.srv.ext.Attention.Get(asUser(f.owner), f.card)
	if err != nil || got.Status != attention.Answered || got.Answer != "Approved in the web UI" {
		t.Fatalf("the card after the draft's approve: %+v %v", got, err)
	}
	// Where the approve lands: the card, settled, saying what was decided.
	settled := feedCard(f.page(t, strings.SplitN(landing, "#", 2)[0]), f.card)
	if !strings.Contains(settled, "data-asks-settled") || !strings.Contains(settled, `data-asks-outcome-state data-attention-answer-text>Approved in the web UI</p>`) {
		t.Fatalf("the approve's landing does not draw the card settled: %s", settled)
	}
	if strings.Contains(settled, "data-draft-approve") || strings.Contains(settled, "data-draft-reject") {
		t.Fatalf("the settled card still offers the draft's decision: %s", settled)
	}
	// The next load of the page: it has left the Open feed.
	if c := feedCard(f.page(t, attentionPath), f.card); c != "" {
		t.Fatalf("the settled card is still on the Open feed after a reload: %s", c)
	}
	if _, err := f.srv.answerAskFrom(asDevice(f.owner), f.card, hostDecisionConfirm, attention.AnswerSource{}); !errors.Is(err, errHostDecisionClosed) {
		t.Fatalf("the phone's confirm after the web decision: %v, want closed", err)
	}
	if r := f.row(t, f.draft.ID); r.status != "approved" || r.via != "browser" {
		t.Fatalf("the draft after the late confirm: %+v (decided twice?)", r)
	}
}

// TestAnotherOwnersStepUpCardNeverGetsTheDraftControls: a card of ANOTHER
// member's that names the owner's draft (its id is all a card carries) draws
// no draft control for them — row-level security hides the draft — only the
// phone line.
func TestAnotherOwnersStepUpCardNeverGetsTheDraftControls(t *testing.T) {
	f := newStepUpDraftFixture(t, "approve")
	asStranger := authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.stranger, Role: authz.RoleMember, Via: authz.ViaSession})
	forged, err := f.srv.ext.Attention.Create(asStranger, attention.New{
		Asker: attention.Asker{System: true, Label: hostDecisionLabel},
		StepUp: &attention.StepUpCard{Tool: pluginDraftDecideTool, Title: "Post to example.forum? · Draft #" + strconv.FormatInt(f.draft.ID, 10),
			Why: "A stranger's card", Preview: "anything", DraftID: f.draft.ID, Asker: "work:2", Details: "A stranger's session asks."},
		Options: []string{hostDecisionConfirm, hostDecisionRefuse},
	})
	if err != nil {
		t.Fatalf("file the stranger's card: %v", err)
	}
	stranger := f.stranger
	f.srv.sessionUserOverride = func(*http.Request) int64 { return stranger }
	for face, html := range map[string]string{
		"the stranger's Open feed card": feedCard(f.page(t, attentionPath), forged.ID),
		"the stranger's card page":      f.page(t, askPagePath(forged.ID)),
	} {
		if html == "" {
			t.Fatalf("%s: not drawn", face)
		}
		if strings.Contains(html, "data-draft-approve") || strings.Contains(html, "data-draft-reject") ||
			strings.Contains(html, "/plugins/drafts/"+strconv.FormatInt(f.draft.ID, 10)) {
			t.Errorf("%s: draws the owner's draft controls", face)
		}
		if !strings.Contains(html, askStepUpPhoneLine) {
			t.Errorf("%s: no phone line", face)
		}
	}
	// And the owner still gets them on their own card.
	owner := f.owner
	f.srv.sessionUserOverride = func(*http.Request) int64 { return owner }
	assertDraftControls(t, "the owner's card", feedCard(f.page(t, attentionPath), f.card), f, attentionPath)
}

// approveForm reports whether html holds draft id's Approve form.
func approveForm(html string, id int64) bool {
	return strings.Contains(html, `action="/plugins/drafts/`+strconv.FormatInt(id, 10)+`/approve"`)
}

// longWords is n characters of words (synthetic, distinct per seed).
func longWords(seed string, n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		b.WriteString(seed + strconv.Itoa(i))
	}
	return b.String()[:n]
}

// TestAStepUpCardDrawsApproveOnlyWhereTheWholeDraftShows: 🔴 what is approved
// is what is seen. On the Open feed a request is its draft's own card, which
// draws the whole draft — every post of a thread, the whole of a long text —
// so its Approve binds what it shows (CHANGED: the feed drew the step-up card,
// a thread's first post only, with "Review and approve"). The card's page,
// which draws every post, has Approve. A body past the card's preview bound is an excerpt on
// every face, so its page has no Approve either — a link to the Drafts tab's
// card, which draws the whole text. A single short post keeps Approve on the
// feed (TestAStepUpCardAboutADraftDrawsTheDraftsDecision).
//
// 🔴 RED AT cdc28ce8e: both faces drew Approve whatever they showed.
func TestAStepUpCardDrawsApproveOnlyWhereTheWholeDraftShows(t *testing.T) {
	t.Run("a thread", func(t *testing.T) {
		f := newStepUpDraftFixtureFor(t, "approve", "x", longWords("first", 200)+"\n---8<---\n"+longWords("second", 200)+"\n---8<---\n"+longWords("third", 200))
		// CHANGED (one draft, one card: asks_drafts.go): the feed draws the
		// request as its draft's own card, which shows EVERY post, so it has
		// Approve — bound to the whole draft it shows.
		card := feedCard(f.page(t, attentionPath), f.card)
		if card == "" || !strings.Contains(card, "data-asks-request-draft") || strings.Contains(card, "data-stepup-thread-more") {
			t.Fatalf("the thread's request is not its draft's whole card on the feed: %s", card)
		}
		for _, post := range []string{"first0", "second0", "third0"} {
			if !strings.Contains(card, post) {
				t.Errorf("the thread's feed card does not show the post starting %s", post)
			}
		}
		if !approveForm(card, f.draft.ID) || !strings.Contains(card, `name="body_sha256" value="`+sha(f.body)+`"`) {
			t.Error("the thread's feed card has no Approve bound to the whole draft")
		}
		if page := f.page(t, askPagePath(f.card)); !approveForm(page, f.draft.ID) {
			t.Error("the thread's page, which draws every post, has no Approve")
		}
	})
	t.Run("an excerpt", func(t *testing.T) {
		f := newStepUpDraftFixtureFor(t, "approve", "example.forum", longWords("excerpt", 3000))
		page := f.page(t, askPagePath(f.card))
		if !strings.Contains(page, "data-stepup-excerpt") {
			t.Fatalf("the fixture's card is not an excerpt")
		}
		if approveForm(page, f.draft.ID) {
			t.Error("the excerpted card's page draws Approve, which binds text it does not show")
		}
		want := `href="` + html.EscapeString(draftTabLink(f.draft.ID)) + `"`
		if !strings.Contains(page, want) {
			t.Errorf("the excerpted card's page has no link to the draft's whole text (%s)", want)
		}
		if !strings.Contains(page, "data-draft-reject>Reject</button>") {
			t.Error("the excerpted card's page lost Reject")
		}
		// CHANGED: on the feed the request is its draft's own card, which
		// draws the whole text — so Approve is there, bound to all of it.
		if card := feedCard(f.page(t, attentionPath), f.card); !approveForm(card, f.draft.ID) || strings.Contains(card, "data-stepup-excerpt") ||
			!strings.Contains(card, `name="body_sha256" value="`+sha(f.body)+`"`) {
			t.Error("the excerpted request's feed card is not the draft's whole card with its Approve")
		}
	})
	t.Run("a card whose preview is not the draft", func(t *testing.T) {
		// Not an excerpt, one post — but what it shows is not the draft's
		// text (a card filed with another preview), so it is not what an
		// approval would bind.
		f := newStepUpDraftFixture(t, "approve")
		other, err := f.srv.ext.Attention.Create(f.asOwner, attention.New{
			Asker: attention.Asker{System: true, Label: hostDecisionLabel},
			StepUp: &attention.StepUpCard{Tool: pluginDraftDecideTool, Title: "Post to example.forum? · Draft #" + strconv.FormatInt(f.draft.ID, 10),
				Why: "Another preview", Preview: "Not the draft's text at all.", DraftID: f.draft.ID, Asker: "work:4", Details: "A session asks."},
			Options: []string{hostDecisionConfirm, hostDecisionRefuse},
		})
		if err != nil {
			t.Fatal(err)
		}
		// CHANGED: on the feed the two requests for the draft are ONE card,
		// its draft's own (one draft, one card) — the draft's own text, its
		// Approve bound to that text, never the other card's preview — with
		// both requests' chips.
		page := f.page(t, attentionPath)
		card := feedCard(page, other.ID)
		if card == "" || feedCard(page, f.card) != "" {
			t.Fatalf("the two requests are not one card on the feed (newest %d): %q / %q", other.ID, card, feedCard(page, f.card))
		}
		if strings.Contains(card, "Not the draft&#39;s text at all.") || strings.Contains(card, "Not the draft's text at all.") ||
			!strings.Contains(card, `name="body_sha256" value="`+sha(f.body)+`"`) {
			t.Errorf("the merged card shows the other card's preview, or its Approve is not bound to the draft's text: %s", card)
		}
		for _, id := range []int64{f.card, other.ID} {
			if !strings.Contains(card, `data-id-link data-id-kind="ask" data-id="`+strconv.FormatInt(id, 10)+`"`) {
				t.Errorf("the merged card has no chip for ask %d", id)
			}
		}
		if page := f.page(t, askPagePath(other.ID)); approveForm(page, f.draft.ID) {
			t.Error("its page draws Approve")
		}
	})
}

// TestABatchDecisionSettlesItsDraftsStepUpCards: an outreach batch decided on
// the Asks page settles the open step-up card about one of its drafts, as a
// single draft's decision does.
//
// 🔴 RED AT cdc28ce8e: the batch decision approved the follow and left its
// card open.
func TestABatchDecisionSettlesItsDraftsStepUpCards(t *testing.T) {
	w := newOutboundWorld(t, true)
	ids, _ := w.followsFor("kip")
	batch := plugins.BatchID(1, builtinOfficerID, w.draft(ids[0]).CreatedAt.UTC().Format("2006-01-02"))
	owner := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession})
	card, err := w.srv.ext.Attention.Create(owner, attention.New{
		Asker: attention.Asker{System: true, Label: hostDecisionLabel},
		StepUp: &attention.StepUpCard{Tool: pluginDraftDecideTool, Title: "Follow on X? · Draft #" + strconv.FormatInt(ids[0], 10),
			Why: "A session asks", Preview: "Follow kip", DraftID: ids[0], Asker: "work:3", Details: "A session asks to decide it."},
		Options: []string{hostDecisionConfirm, hostDecisionRefuse},
	})
	if err != nil {
		t.Fatalf("file the card: %v", err)
	}
	if rec := w.decideBatchForm(batch, plugins.BatchSHA256(ids), ids[0]); rec.Code != http.StatusSeeOther {
		t.Fatalf("the batch decision: %d %s", rec.Code, rec.Body)
	}
	if got := w.draft(ids[0]).Status; got != plugins.DraftApproved {
		t.Fatalf("the follow after the batch decision: %s", got)
	}
	got, err := w.srv.ext.Attention.Get(owner, card.ID)
	if err != nil || got.Status != attention.Answered || got.Answer != "Approved in the web UI" {
		t.Fatalf("the follow's step-up card after the batch decision: %+v %v", got, err)
	}
}
