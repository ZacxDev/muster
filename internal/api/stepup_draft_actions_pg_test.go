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
		askStepUpDraftLine,
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
	if shared := draftDecisionHTML(dc, time.Now(), cardPlace{back: attentionPath}); !strings.Contains(card, shared) {
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
	if !strings.Contains(card, askStepUpDraftRejectLine) {
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
// posted as the browser posts it, approves the draft and returns to the Asks
// page (not the Drafts tab); the card is answered with what happened and
// where, leaves the Open feed, and the phone's Confirm afterwards is refused
// as closed — the draft is approved once.
//
// 🔴 RED AT origin/main: the card stayed open after the draft was decided.
func TestApprovingAStepUpCardsDraftSettlesTheCard(t *testing.T) {
	f := newStepUpDraftFixture(t, "approve")
	form := url.Values{"return": {attentionPath}, "body_sha256": {sha(f.body)},
		"media_sha256": {sha("launch/hero-7") + "," + sha("launch/demo-8")}}
	w := f.ownerForm(t, "/plugins/drafts/"+strconv.FormatInt(f.draft.ID, 10)+"/approve", form, sessionCookieFor(f.srv))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != attentionPath {
		t.Fatalf("the approve: %d to %q, want 303 to %s (%s)", w.Code, w.Header().Get("Location"), attentionPath, w.Body)
	}
	if r := f.row(t, f.draft.ID); r.status != "approved" || r.via != "browser" {
		t.Fatalf("the draft after the approve: %+v", r)
	}
	got, err := f.srv.ext.Attention.Get(asUser(f.owner), f.card)
	if err != nil || got.Status != attention.Answered || got.Answer != "Approved in the web UI" {
		t.Fatalf("the card after the draft's approve: %+v %v", got, err)
	}
	if c := feedCard(f.page(t, attentionPath), f.card); c != "" {
		t.Fatalf("the settled card is still on the Open feed: %s", c)
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
// is what is seen. A thread (several X posts) shows only its first post on
// the Open feed, so its feed card has NO Approve — a "Review and approve" link
// to the card's page instead, and Reject — while its page, which draws every
// post, has Approve. A body past the card's preview bound is an excerpt on
// every face, so its page has no Approve either — a link to the Drafts tab's
// card, which draws the whole text. A single short post keeps Approve on the
// feed (TestAStepUpCardAboutADraftDrawsTheDraftsDecision).
//
// 🔴 RED AT cdc28ce8e: both faces drew Approve whatever they showed.
func TestAStepUpCardDrawsApproveOnlyWhereTheWholeDraftShows(t *testing.T) {
	t.Run("a thread", func(t *testing.T) {
		f := newStepUpDraftFixtureFor(t, "approve", "x", longWords("first", 200)+"\n---8<---\n"+longWords("second", 200)+"\n---8<---\n"+longWords("third", 200))
		card := feedCard(f.page(t, attentionPath), f.card)
		if card == "" || !strings.Contains(card, "data-stepup-thread-more") {
			t.Fatalf("the fixture is not a thread on the feed: %s", card)
		}
		if approveForm(card, f.draft.ID) {
			t.Error("the thread's feed card draws Approve, which binds posts it does not show")
		}
		if !strings.Contains(card, `href="`+askPagePath(f.card)+`"`) || !strings.Contains(card, "data-draft-review>Review and approve</a>") {
			t.Errorf("the thread's feed card has no Review and approve link to its page: %s", card)
		}
		if !strings.Contains(card, "data-draft-reject>Reject</button>") || !strings.Contains(card, askStepUpDraftReviewLine) {
			t.Errorf("the thread's feed card lost Reject or its line")
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
		if card := feedCard(f.page(t, attentionPath), f.card); approveForm(card, f.draft.ID) {
			t.Error("the excerpted card's feed face draws Approve")
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
		if card := feedCard(f.page(t, attentionPath), other.ID); card == "" || approveForm(card, f.draft.ID) {
			t.Errorf("a card showing another text draws Approve (or is missing): %q", card)
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
