package api

import (
	"context"
	"io"
	"log"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
)

// AN OFFICER PUSHES ONLY FOR A DECISION, TO ITS CARD (officer_push.go).
//
// Every id below is pairwise distinct (asks 4711/4723/4739, drafts
// 5302/5317/5329, grants 6421/6437, pages 7103/7109, instance 811) and none
// is a number any URL or tag constant spells, so a mutant that hardcodes one
// card's address cannot pass another's assertion. Every test that asserts a
// ZERO also observes a decision push on the same device in the same test —
// the positive control: the zero is not a pusher wired to nothing.

// officerPushEP is the rig owner's (user 1's) one device.
var officerPushEP = ep("officer-push")

// officerPushRig is the officer rig (two officers of owner 1, offA and offB)
// with a real push service over a recording pusher, owner 1's device claimed.
func officerPushRig(t *testing.T) (*officerRig, *payloadPusher) {
	t.Helper()
	r := newOfficerRig(t)
	store, pusher := &memPush{}, &payloadPusher{}
	claim(t, store, 1, officerPushEP)
	ext := r.s.ext
	ext.Users = stubUsers{}
	ext.Push = push.NewService(store, pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))
	r.s.UseExtensions(ext)
	return r, pusher
}

// sent is every payload owner 1's device got, after the in-flight pushes.
func (r *officerRig) sent(p *payloadPusher) []push.Payload {
	r.s.WaitForPushes()
	return p.to(officerPushEP)
}

// officerPushControl is the positive control: officer offA's outreach batch
// for day 2000-01-02 (a decision) IS observed on the device, at its card.
func officerPushControl(t *testing.T, r *officerRig, p *payloadPusher) {
	t.Helper()
	before := len(r.sent(p))
	r.s.notifyBatchAwaiting(1, officerBatchDraft(offA))
	got := r.sent(p)
	if len(got) != before+1 || got[len(got)-1].Data["url"] != "/attention?tab=drafts#outreach-1-release-alpha-web-2000-01-02" {
		t.Fatalf("positive control: a decision push was not observed (%d → %+v): this test's zero would be vacuous", before, got)
	}
}

func officerStr(s string) *string { return &s }

// officerBatchDraft is a follow officer o filed on 2000-01-02.
func officerBatchDraft(o string) plugins.Draft {
	pid := int64(9)
	return plugins.Draft{ID: 5390, OwnerID: 1, PluginID: &pid, TargetKind: officerStr(plugins.TargetFollow), Officer: officerStr(o),
		CreatedAt: time.Date(2000, 1, 2, 9, 0, 0, 0, time.UTC)}
}

// TestEveryOfficerPushIsClassified: the classification, pinned as literals —
// a path is added to, removed from or flipped in officerPushes only by
// editing this list too. The set is compared both ways, so it fails when the
// table GROWS (an unreviewed new path) and when it SHRINKS.
func TestEveryOfficerPushIsClassified(t *testing.T) {
	want := map[officerPush]bool{
		"ask": true, "escalation": true, "draft": true, "outreach_batch": true, "connector_request": true,
		"budget": true, "cap_hold": true,
		"draft_refused": false, "budget_unreadable": false, "unclaimed": false, "page_published": false, "page_shared": false,
		"page_updated": false, "notify_owner": false,
	}
	for k, d := range want {
		if got, ok := officerPushes[k]; !ok || got != d {
			t.Errorf("officer push %q: classified %v (present %v), want decides=%v", k, got, ok, d)
		}
	}
	if !maps.Equal(officerPushes, want) {
		have := slices.Sorted(maps.Keys(officerPushes))
		t.Errorf("officerPushes has %v, want exactly the %d paths above", have, len(want))
	}
}

// TestOfficerDecisionCardsAreTheirCards: each decision's card, literally; an
// information path has none.
func TestOfficerDecisionCardsAreTheirCards(t *testing.T) {
	for _, c := range []struct {
		k    officerPush
		ref  officerCardRef
		want string
	}{
		{officerPushAsk, officerCardRef{ID: "ask-4711"}, "/attention/open/ask-4711"},
		{officerPushEscalation, officerCardRef{ID: "ask-4723"}, "/attention/open/ask-4723"},
		{officerPushDraft, officerCardRef{ID: "5302"}, "/attention?tab=drafts#draft-5302"},
		{officerPushOutreachBatch, officerCardRef{ID: "1:" + offB + "@2000-01-03"}, "/attention?tab=drafts#outreach-1-" + strings.ReplaceAll(offB, ":", "-") + "-2000-01-03"},
		{officerPushConnectorRequest, officerCardRef{ID: "6421"}, "/connectors#connector-request-6421"},
		{officerPushConnectorRequest, officerCardRef{ID: "6437", Team: 908}, "/teams/908/connectors#connector-request-6437"},
		{officerPushBudget, officerCardRef{ID: "scout:docs", Owner: 1}, "/officers/scout:docs?owner=1"},
		{officerPushCapHold, officerCardRef{ID: "ops watch", Owner: 908}, "/officers/ops%20watch?owner=908"},
		{officerPushBudgetUnreadable, officerCardRef{ID: "scout:docs", Owner: 1}, ""},
		{officerPushDraftRefused, officerCardRef{ID: "5317"}, ""},
	} {
		if got := officerDecisionCard(c.k, c.ref); got != c.want {
			t.Errorf("officerDecisionCard(%s, %+v) = %q, want %q", c.k, c.ref, got, c.want)
		}
	}
}

// TestAnOfficersQuestionAndEscalationPushTheirAskCards: an officer's question
// and an escalation each push once, opening the ask's card; a host instance's
// unclaimed notice (dismiss-only) pushes nothing.
func TestAnOfficersQuestionAndEscalationPushTheirAskCards(t *testing.T) {
	r, p := officerPushRig(t)
	ask := func(id int64, notice bool) attention.Ask {
		return attention.Ask{ID: id, Owner: 1, Status: attention.Open, Kind: map[bool]attention.Kind{true: attention.KindNotice}[notice],
			Asker: attention.Asker{System: true, Label: officerAskLabel(offA)}, Question: "Which repo holds the release notes?", CreatedAt: time.Now()}
	}
	r.s.officerAskFiled(officerPushUnclaimed, ask(4739, true))
	if got := r.sent(p); len(got) != 0 {
		t.Fatalf("an officer's unclaimed notice (information) pushed %+v, want nothing", got)
	}
	r.s.officerAskFiled(officerPushAsk, ask(4711, false))
	r.s.officerAskFiled(officerPushEscalation, ask(4723, false))
	got := r.sent(p)
	var urls []string
	for _, pl := range got {
		urls = append(urls, pl.Data["url"])
	}
	slices.Sort(urls) // each push is on its own goroutine: arrival order is not theirs
	if want := []string{"/attention/open/ask-4711", "/attention/open/ask-4723"}; !slices.Equal(urls, want) {
		t.Fatalf("an officer's question and escalation pushed to %v, want %v", urls, want)
	}
}

// TestAnOfficersDraftPushesItsCardAndNamesTheOfficer: an officer's DM draft
// pushes to its draft card, titled as a message to approve and naming the
// officer — never "your chief drafted a reply". Control: a chief's reply
// draft keeps its own title.
func TestAnOfficersDraftPushesItsCardAndNamesTheOfficer(t *testing.T) {
	r, p := officerPushRig(t)
	pid := int64(9)
	d := plugins.Draft{ID: 5302, OwnerID: 1, PluginID: &pid, TargetKind: officerStr(plugins.TargetDM), Officer: officerStr(offB)}
	r.s.notifyDraftAwaitingNoting(context.Background(), 1, d, plugins.Message{}, "")
	got := r.sent(p)
	if len(got) != 1 {
		t.Fatalf("an officer's draft pushed %d notification(s), want 1: %+v", len(got), got)
	}
	n := got[0]
	if n.Data["url"] != "/attention?tab=drafts#draft-5302" {
		t.Errorf("an officer's draft opens %q, want its card /attention?tab=drafts#draft-5302", n.Data["url"])
	}
	if n.Title != "A message awaits your approval" || !strings.Contains(n.Body, offB) || strings.Contains(strings.ToLower(n.Body), "chief") {
		t.Errorf("an officer's draft reads %q / %q, want the message title and a body naming officer %s, not the chief", n.Title, n.Body, offB)
	}
	r.s.notifyDraftAwaitingNoting(context.Background(), 1, plugins.Draft{ID: 5329, OwnerID: 1, MessageID: 77}, plugins.Message{}, "")
	if got := r.sent(p); len(got) != 2 || got[1].Title != DraftPushTitle || got[1].Data["url"] != "/attention?tab=drafts#draft-5329" {
		t.Errorf("control: a chief's reply draft pushed %+v, want %q to its card", got[1:], DraftPushTitle)
	}
}

// TestAnOfficersOutreachBatchPushesItsCard: the batch push opens the batch's
// card (the control helper asserts it for offA; offB's day differs).
func TestAnOfficersOutreachBatchPushesItsCard(t *testing.T) {
	r, p := officerPushRig(t)
	d := officerBatchDraft(offB)
	d.CreatedAt = time.Date(2000, 1, 3, 9, 0, 0, 0, time.UTC)
	r.s.notifyBatchAwaiting(1, d)
	got := r.sent(p)
	want := "/attention?tab=drafts#outreach-1-" + strings.ReplaceAll(offB, ":", "-") + "-2000-01-03"
	if len(got) != 1 || got[0].Title != OutreachBatchPushTitle || got[0].Data["url"] != want {
		t.Fatalf("an officer's outreach batch pushed %+v, want one %q to %q", got, OutreachBatchPushTitle, want)
	}
}

// TestAnOfficersRefusedDraftIsWithdrawnNotPushed: the plugin refused an
// officer's draft — it went back to the officer. The owner's device gets only
// the WITHDRAWAL of that draft's "awaits your approval" (resolved, its tag),
// never a new notification. Control: a chief's refused reply still pushes.
func TestAnOfficersRefusedDraftIsWithdrawnNotPushed(t *testing.T) {
	r, p := officerPushRig(t)
	pl := plugins.Plugin{ID: 9, OwnerID: 1, Label: "Mailer"}
	r.s.notifyDraftRefused(pl, plugins.Draft{ID: 5317, OwnerID: 1, Officer: officerStr(offA)}, refusalToOfficer)
	got := r.sent(p)
	if len(got) != 1 || got[0].Type != push.TypeResolved || got[0].Tag != "plugin-draft-5317" {
		t.Fatalf("an officer's refused draft sent %+v, want only the withdrawal of plugin-draft-5317", got)
	}
	r.s.notifyDraftRefused(pl, plugins.Draft{ID: 5329, OwnerID: 1}, refusalToChief)
	if got := r.sent(p); len(got) != 2 || got[1].Title != DraftRefusedPushTitle || got[1].Type == push.TypeResolved {
		t.Fatalf("control: a chief's refused reply sent %+v, want %q", got[1:], DraftRefusedPushTitle)
	}
}

// TestAnOfficersBudgetReachedPushesItsPageAndUnreadableDoesNot: the daily
// budget reached holds the officer for the day — a decision (raise the cap,
// lift the hold): ONE push, opening the officer's page. Its spend unreadable
// is muster's own failure: nothing is pushed (the reached push, observed on
// the same device, is the positive control for that zero).
func TestAnOfficersBudgetReachedPushesItsPageAndUnreadableDoesNot(t *testing.T) {
	r, p := officerPushRig(t)
	o := officers.Officer{ID: offA, Owner: 1}
	r.s.officerBudgetNotice(o, officers.BudgetAlert{Unreadable: true, Budget: 10, Waiting: 2})
	if got := r.sent(p); len(got) != 0 {
		t.Fatalf("an officer's spend unreadable (information) pushed %+v, want nothing", got)
	}
	r.s.officerBudgetNotice(o, officers.BudgetAlert{Spent: 10.49, Budget: 10, Waiting: 3, ResetAt: time.Date(2000, 1, 5, 0, 0, 0, 0, time.UTC)})
	got := r.sent(p)
	if len(got) != 1 || got[0].Tag != "officer-budget-"+offA+"-2000-01-05" || got[0].Data["url"] != "/officers/"+offA+"?owner=1" {
		t.Fatalf("an officer's budget reached pushed %+v, want one on its day's tag opening /officers/%s?owner=1", got, offA)
	}
}

// TestAnOfficersPagePushesNothing: a page an officer's instance published
// pushes nothing; the same page by any other agent pushes "Page ready".
func TestAnOfficersPagePushesNothing(t *testing.T) {
	r, p := officerPushRig(t)
	page := pages.Page{ID: 7103, OwnerID: 1, Title: "Release digest", Creator: pages.Creator{AgentID: 811, Label: "release officer"}}
	r.s.pagePublished(page, true)
	if got := r.sent(p); len(got) != 0 {
		t.Fatalf("an officer's page (information) pushed %+v, want nothing", got)
	}
	page.ID, page.Creator.AgentID = 7109, officerOrdinaryID
	r.s.pagePublished(page, false)
	if got := r.sent(p); len(got) != 1 || !strings.HasPrefix(got[0].Title, "Page ready") {
		t.Fatalf("control: another agent's page pushed %+v, want one \"Page ready\"", got)
	}
}

// TestAnOfficersConnectorRequestPushesItsCard: an officer instance's request
// pushes to its card on the Connectors page; agentIsOfficer is the binding
// rule's answer (an instance yes; an ordinary agent, the chief and no agent
// no).
func TestAnOfficersConnectorRequestPushesItsCard(t *testing.T) {
	r, p := officerPushRig(t)
	inst := r.addInstance(r.a, 1, 811)
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleMember, Via: authz.ViaConnect})
	for id, want := range map[int64]bool{inst.ID: true, officerOrdinaryID: false, officerChiefID: false, 0: false} {
		if got := r.s.agentIsOfficer(ctx, id, 1); got != want {
			t.Errorf("agentIsOfficer(%d, owner 1) = %v, want %v", id, got, want)
		}
	}
	// The same officer instance, asked about for ANOTHER owner (a member who
	// can read a team's agent, say): not that owner's officer.
	if r.s.agentIsOfficer(ctx, inst.ID, 2) {
		t.Errorf("agentIsOfficer(%d, owner 2) = true: an instance of owner 1's officer decided owner 2's push", inst.ID)
	}
	r.s.onConnectorRequested(ctx, connectors.Grant{ID: 6421, AgentID: inst.ID, AgentName: inst.Name, Connector: "github"})
	got := r.sent(p)
	if len(got) != 1 || got[0].Data["url"] != "/connectors#connector-request-6421" {
		t.Fatalf("an officer's connector request pushed %+v, want one to /connectors#connector-request-6421", got)
	}
}

// TestAnOfficersPageVersionPushesNothing: a new version an officer's instance
// published pushes nothing; the same version by any other agent pushes
// "Page updated".
func TestAnOfficersPageVersionPushesNothing(t *testing.T) {
	r, p := officerPushRig(t)
	ref := pageUpdatedRef{id: 7203, owner: 1, version: 4, title: "Release digest", creator: pages.Creator{AgentID: 811, Label: "release officer"}, byOfficer: true}
	r.s.pageUpdated(toolCaller{asker: attention.Asker{AgentID: 811}}, ref)
	if got := r.sent(p); len(got) != 0 {
		t.Fatalf("an officer's page version (information) pushed %+v, want nothing", got)
	}
	ref.id, ref.byOfficer, ref.creator.AgentID = 7209, false, officerOrdinaryID
	r.s.pageUpdated(toolCaller{asker: attention.Asker{AgentID: officerOrdinaryID}}, ref)
	if got := r.sent(p); len(got) != 1 || !strings.HasPrefix(got[0].Title, "Page updated") {
		t.Fatalf("control: another agent's page version pushed %+v, want one \"Page updated\"", got)
	}
}

// TestAnOfficersNotifyOwnerSendsNothing: an officer instance's notify_owner is
// answered "suppressed" (reason officer) and reaches no device; an ordinary
// agent's identical call is delivered.
func TestAnOfficersNotifyOwnerSendsNothing(t *testing.T) {
	r, p := officerPushRig(t)
	inst := r.addInstance(r.a, 1, 811)
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleMember, Via: authz.ViaConnect})
	res := notifyResult(t, r.s.toolNotifyOwner(ctx, toolCaller{agent: &inst, asker: attention.Asker{AgentID: inst.ID}}, `{"title":"Digest is ready"}`))
	if res["status"] != "suppressed" || res["reason"] != pushDropOfficer {
		t.Fatalf("an officer's notify_owner answered %+v, want suppressed/%s", res, pushDropOfficer)
	}
	if got := r.sent(p); len(got) != 0 {
		t.Fatalf("an officer's notify_owner pushed %+v, want nothing", got)
	}
	ord, err := r.s.ext.Agents.Get(ctx, officerOrdinaryID)
	if err != nil {
		t.Fatal(err)
	}
	res = notifyResult(t, r.s.toolNotifyOwner(ctx, toolCaller{agent: &ord, asker: attention.Asker{AgentID: ord.ID}}, `{"title":"Digest is ready"}`))
	if got := r.sent(p); res["status"] != "delivered" || len(got) != 1 || got[0].Title != "Digest is ready" {
		t.Fatalf("control: an ordinary agent's notify_owner answered %+v and pushed %+v, want one delivered", res, got)
	}
}
