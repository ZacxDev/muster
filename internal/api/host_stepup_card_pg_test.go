package api

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/plugins"
)

// A STEP-UP CARD FOR A DRAFT, THROUGH THE REAL MIRROR DOOR (host_stepup_card.go):
// plugin_draft_decide from a host officer's machine files a card read from the
// exact draft it names — its venue, text and media, its officer item's reason —
// and its push, its page and the phone's `confirms` all say that, never the
// boilerplate; a draft that does not match the call shows the refuse state.

// stepUpDraftRig is a draft tool fixture with an officer item, an outbound
// draft filed for it (one uploaded image) and a host officer declared on the
// fixture's host, so the door's plugin_draft_decide is gated.
type stepUpDraftRig struct {
	*draftToolFixture
	draft plugins.Draft
	body  string
}

const stepUpOfficer = "marketing:stepup"

func newStepUpDraftRig(t *testing.T, body string) *stepUpDraftRig {
	t.Helper()
	f := newDraftToolFixture(t)
	f.srv.ext.Attention = attention.NewPG(f.pool)
	dsn := dbtest.DSN(t)
	ctx := f.asOwner
	store := builtin.NewStore(f.pool)
	if err := store.PutType(ctx, f.owner, builtin.Type{Key: "marketing", Name: "Marketing", Charter: "Draft posts."}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutDef(ctx, f.owner, builtin.Def{Officer: stepUpOfficer, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	item, _, err := store.Enqueue(ctx, f.owner, builtin.NewItem{Officer: stepUpOfficer, Kind: "marketing.post", SubjectKey: "rebrand announcement"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f.srv.EnableOfficerStore(store)
	plug := strconv.FormatInt(f.plugin.ID, 10)
	dbtest.OwnerExec(t, dsn, `UPDATE plugins SET deliver_to = 'officer:`+stepUpOfficer+`', capabilities = '{messages,draft_checks}' WHERE id = `+plug)
	t.Cleanup(func() {
		dbtest.OwnerExec(t, dsn, `UPDATE plugins SET deliver_to = NULL WHERE id = `+plug)
		dbtest.OwnerExec(t, dsn, `DELETE FROM attention_asks WHERE asker_label = 'step-up'`)
	})
	d, err := f.plugins.FileOutbound(f.asOwner, f.chief.ID, f.owner, f.chief.Name, plugins.NewOutbound{Officer: stepUpOfficer,
		Plugin: f.plugin.Name, Venue: "x", Target: plugins.OutboundTarget{Kind: plugins.TargetPost}, Body: body, Media: []string{"launch/hero-1"}})
	if err != nil {
		t.Fatalf("file an outbound draft: %v", err)
	}
	// The draft is the item's (an instance holding a claim would have named
	// it; the binding is immutable, so the fixture sets it with the trigger
	// off for one statement).
	id := strconv.FormatInt(d.ID, 10)
	dbtest.OwnerExec(t, dsn, `ALTER TABLE plugin_reply_drafts DISABLE TRIGGER plugin_reply_drafts_outbound_immutable`)
	dbtest.OwnerExec(t, dsn, `UPDATE plugin_reply_drafts SET officer_item_id = `+strconv.FormatInt(item, 10)+` WHERE id = `+id)
	dbtest.OwnerExec(t, dsn, `ALTER TABLE plugin_reply_drafts ENABLE TRIGGER plugin_reply_drafts_outbound_immutable`)
	if _, _, err := f.plugins.UploadDraftMedia(f.asOwner, f.plugin.ID, d.ID, plugins.DraftMedia{Ref: "launch/hero-1", SHA256: sha("launch/hero-1"),
		ContentType: "image/webp", Size: 10, Key: plugins.MediaKeyPrefix + "x/0"}); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if _, _, err := f.plugins.CheckDraft(f.asOwner, f.plugin.ID, d.ID, plugins.DraftCheck{Sendable: true}); err != nil {
		t.Fatalf("check: %v", err)
	}
	declareHostOfficerOn(t, f.srv, f.owner, "tower")
	return &stepUpDraftRig{draftToolFixture: f, draft: d, body: body}
}

// decideCard calls plugin_draft_decide through the door with args (id added)
// and returns the card it filed, as stored.
func (r *stepUpDraftRig) decideCard(t *testing.T, args map[string]any) attention.Ask {
	t.Helper()
	args["id"] = r.draft.ID
	text, isErr := r.mcp(t, r.tok, otherRouteSID, pluginDraftDecideTool, args)
	if isErr {
		t.Fatalf("decide: %s", text)
	}
	a, err := r.srv.ext.Attention.Get(r.asOwner, stepUpCardOf(t, text))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (r *stepUpDraftRig) askPage(t *testing.T, id int64) string {
	t.Helper()
	w := r.call(t, http.MethodGet, askPagePath(id), nil, map[string]string{"Cookie": sessionCookieFor(r.srv)})
	if w.Code != http.StatusOK {
		t.Fatalf("the ask page: %d %s", w.Code, w.Body)
	}
	return w.Body.String()
}

// TestADraftStepUpCardShowsThePostAndWhy: the card's parts come from the
// draft and its officer item; its question (what the phone confirms) is the
// why and the post; its push and page say the same and fold the boilerplate.
func TestADraftStepUpCardShowsThePostAndWhy(t *testing.T) {
	const body = "Big news: acme is now acme-two.\nRead more on the blog."
	r := newStepUpDraftRig(t, body)
	a := r.decideCard(t, map[string]any{"decision": "approve", "body_sha256": sha(body), "media_sha256": []string{sha("launch/hero-1")}})
	c := a.StepUp
	if c == nil {
		t.Fatalf("the card has no parts: %q", a.Question)
	}
	if c.Title != "Post to X?" || c.Why != "Marketing: rebrand announcement" || c.Venue != "X" || c.Preview != body || c.Changed ||
		c.DraftID != r.draft.ID || c.Tool != pluginDraftDecideTool {
		t.Fatalf("the card's parts: %+v", c)
	}
	if len(c.Media) != 1 || c.Media[0] != (attention.StepUpMedium{N: 0, Ref: "launch/hero-1", Type: "image/webp", SHA256: sha("launch/hero-1")}) {
		t.Fatalf("the card's media: %+v", c.Media)
	}
	const question = "Post to X?\nMarketing: rebrand announcement\n\nBig news: acme is now acme-two.\nRead more on the blog.\n\nWith 1 image"
	if a.Question != question {
		t.Fatalf("the card's question:\n%q\nwant\n%q", a.Question, question)
	}
	// The boilerplate is on the record, in Details.
	if !strings.HasPrefix(c.Details, "A session on tower (") || !strings.Contains(c.Details, "plugin_draft_decide {") ||
		!strings.Contains(c.Details, "paired phone") || !regexp.MustCompile(`\[[0-9a-f]{12}\]$`).MatchString(c.Details) {
		t.Fatalf("the card's details:\n%s", c.Details)
	}
	// The phone's prompt confirms exactly the question.
	dev := authz.WithPrincipal(context.Background(), authz.Principal{UserID: r.owner, Role: authz.RoleAdmin, Via: authz.ViaDevice})
	if got := r.srv.stepUpCardConfirms(dev, strconv.FormatInt(a.ID, 10)); got != question {
		t.Fatalf("confirms:\n%q\nwant\n%q", got, question)
	}
	// The push: the title and the why and the post, nothing else.
	n := attentionNotification(askItem(a), false)
	if n.Title != "Post to X?" || n.Body != "Marketing: rebrand announcement · Big news: acme is now acme-two. Read more on the blog." {
		t.Fatalf("the push: %q / %q", n.Title, n.Body)
	}
	for _, never := range []string{"tower", "{", "paired phone", "plugin_draft_decide"} {
		if strings.Contains(n.Title+n.Body, never) {
			t.Errorf("the push carries %q", never)
		}
	}
	// The page: the post on its face, the boilerplate folded.
	page := r.askPage(t, a.ID)
	open := strings.Index(page, "<details class=\"rounded-lg bg-s1 px-3 py-2 ring-1 ring-inset ring-line\" data-stepup-details>")
	end := -1
	if open >= 0 {
		end = open + strings.Index(page[open:], "</details>")
	}
	if open < 0 || end < open {
		t.Fatalf("the page has no Details")
	}
	face := page[:open] + page[end:]
	for _, want := range []string{`data-stepup-title>Post to X?</h2>`, `data-stepup-why>Marketing: rebrand announcement</p>`,
		"data-stepup-preview-text>Big news: acme is now acme-two.\nRead more on the blog.</p>",
		`src="/plugins/drafts/` + strconv.FormatInt(r.draft.ID, 10) + `/media/0" data-stepup-image>`} {
		if !strings.Contains(face, want) {
			t.Errorf("the page's face lacks %s", want)
		}
	}
	for _, never := range []string{"A session on tower", "body_sha256", "paired phone"} {
		if strings.Contains(face, never) {
			t.Errorf("the page's face carries %q", never)
		}
	}
	if d := page[open:end]; !strings.Contains(d, "A session on tower (") || !strings.Contains(d, "body_sha256") {
		t.Errorf("Details lacks the boilerplate:\n%s", d)
	}
	// The decision still runs only from the phone, and runs as asked.
	if _, err := r.srv.answerAskFrom(dev, a.ID, hostDecisionConfirm, attention.AnswerSource{}); err != nil {
		t.Fatalf("the phone's confirm: %v", err)
	}
	if got := r.row(t, r.draft.ID); got.status != "approved" {
		t.Fatalf("the draft after the confirm: %+v", got)
	}
}

// TestAChangedDraftsStepUpCardSaysRefuse: a call naming a body or media
// SHA-256 the draft does not have files a card that shows nothing of the
// draft and says to refuse — and a Confirm of it decides nothing (the store
// refuses the mismatch).
func TestAChangedDraftsStepUpCardSaysRefuse(t *testing.T) {
	const body = "The real post."
	r := newStepUpDraftRig(t, body)
	const question = "This draft changed since it was asked\nRefuse: the draft no longer matches what the session asked you to decide."
	for name, args := range map[string]map[string]any{
		"another body":  {"decision": "approve", "body_sha256": sha("Another post."), "media_sha256": []string{sha("launch/hero-1")}},
		"another media": {"decision": "approve", "body_sha256": sha(body), "media_sha256": []string{sha("launch/other")}},
	} {
		a := r.decideCard(t, args)
		if a.StepUp == nil || !a.StepUp.Changed || a.Question != question || strings.Contains(a.Question, body) {
			t.Fatalf("%s: the card %q %+v", name, a.Question, a.StepUp)
		}
		page := r.askPage(t, a.ID)
		if !strings.Contains(page, "data-stepup-changed") || strings.Contains(page, "data-stepup-preview") || strings.Contains(page, body) {
			t.Fatalf("%s: the page shows the draft or no refuse state", name)
		}
		dev := authz.WithPrincipal(context.Background(), authz.Principal{UserID: r.owner, Role: authz.RoleAdmin, Via: authz.ViaDevice})
		if _, err := r.srv.answerAskFrom(dev, a.ID, hostDecisionConfirm, attention.AnswerSource{}); err != nil {
			t.Fatalf("%s: the phone's confirm: %v", name, err)
		}
		if got := r.row(t, r.draft.ID); got.status != "pending" {
			t.Fatalf("%s: a mismatched decision ran: %+v", name, got)
		}
	}
}

// TestARejectStepUpCardSaysSoPlainly: a reject decision's card says it rejects,
// with the reason, and still shows which draft.
func TestARejectStepUpCardSaysSoPlainly(t *testing.T) {
	const body = "A post to reject."
	r := newStepUpDraftRig(t, body)
	a := r.decideCard(t, map[string]any{"decision": "reject", "body_sha256": sha(body), "reason": "wrong tone"})
	const question = "Reject this draft?\nReject this draft: wrong tone\n\nA post to reject.\n\nWith 1 image"
	if a.Question != question {
		t.Fatalf("the reject card:\n%q\nwant\n%q", a.Question, question)
	}
}

// TestARouteReplyStepUpCardIsASentence: a non-draft tool's card is one plain
// summary with who asked, never the call's JSON.
func TestARouteReplyStepUpCardIsASentence(t *testing.T) {
	f := newDraftToolFixture(t)
	f.srv.ext.Attention = attention.NewPG(f.pool)
	_, m := f.pluginMessage(t, "example-inbox", "x-stepup-rr", "someone@example.test", true)
	rid := f.route(t, m)
	declareHostOfficerOn(t, f.srv, f.owner, "tower")
	text, isErr := f.routeReplyCall(t, f.tok, routeSID, map[string]any{"route_id": rid, "body": "See you Friday."})
	if isErr {
		t.Fatalf("route_reply: %s", text)
	}
	a, err := f.srv.ext.Attention.Get(f.asOwner, stepUpCardOf(t, text))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbtest.OwnerExec(t, dbtest.DSN(t), `DELETE FROM attention_asks WHERE asker_label = 'step-up'`) })
	if a.StepUp == nil || a.StepUp.Title != "Send this answer to a routed message?" || a.StepUp.Preview != "See you Friday." ||
		a.Question != "Send this answer to a routed message?\nAsked by work:1\n\nSee you Friday." ||
		!strings.Contains(a.StepUp.Details, "route_reply {") || !strings.Contains(a.StepUp.Details, "A session on tower (work:1 @ tower)") {
		t.Fatalf("the route_reply card: %q %+v", a.Question, a.StepUp)
	}
}
