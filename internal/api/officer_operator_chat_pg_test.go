package api

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/plugins"
)

// CHAT WITH AN OFFICER, ITS OWN-STATE TOOLS AND ITS CONFIRM CARDS (officers UX
// PR 2/4; officer_operator_chat.go, officer_own_state.go), through the real
// server, the real built-in and plugin stores and a real officer instance
// (outboundWorld).
//
// The literal ids, routes and words the tests read are pinned here, never
// derived from the code under test.
const (
	tChatSection   = "officer-chat"
	tChatPath      = "/officers/" + builtinOfficerID + "/chat"
	tOperatorLane  = "officer-chat:operator-1"
	tOtherOfficer  = "marketing:elsewhere"
	tSelfDecide    = "An officer never decides its own drafts or outreach: its owner approves them, signed in."
	tCardNotShown  = "This is not the card you were shown: reload the page and look at it again."
	tCardChanged   = "It changed since this card was drawn, so nothing was decided: ask it for a new card."
	tChatOnlyWords = "is offered only in a chat with your owner on your page in muster"
)

// laneCtx is a tool call from person uid's operator chat lane, as
// verifyChatLaneCall marks one whose lane secret matched.
func laneCtx(ctx context.Context, uid int64) context.Context {
	return withChatLaneCall(ctx, officers.ChatSessionKey("operator-"+strconv.FormatInt(uid, 10)))
}

// cardRow is one confirm card as the database holds it.
type cardRow struct {
	id                  int64
	owner               int64
	kind, sha, decision string
	draft               int64
}

func (w *outboundWorld) cards() []cardRow {
	w.t.Helper()
	rows, err := w.f.pool.Query(authz.System(w.ctx), `SELECT id, owner_id, kind, card_sha256, coalesce(decision, ''), coalesce(draft_id, 0)
		FROM officer_chat_cards WHERE officer = $1 ORDER BY id`, builtinOfficerID)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	var out []cardRow
	for rows.Next() {
		var c cardRow
		if err := rows.Scan(&c.id, &c.owner, &c.kind, &c.sha, &c.decision, &c.draft); err != nil {
			w.t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// wipeCards removes every confirm card the world's officers left.
func wipeCards(t *testing.T) {
	dbtest.OwnerExec(t, dbtest.DSN(t), `DELETE FROM officer_chat_cards`)
}

// cardFor asks for draft id's confirm card from user 1's chat: the card id.
func (w *outboundWorld) cardFor(args string) int64 {
	w.t.Helper()
	out := w.srv.dispatchAgentTool(laneCtx(w.ctx, 1), w.agent, officers.ToolConfirmCard, args)
	var got struct {
		CardID int64 `json:"card_id"`
	}
	if isToolErr(out) || json.Unmarshal([]byte(out), &got) != nil || got.CardID == 0 {
		w.t.Fatalf("officer_confirm_card %s: %s", args, out)
	}
	return got.CardID
}

func (w *outboundWorld) cardPost(card int64, verb string, form url.Values) *httptest.ResponseRecorder {
	return w.post(tChatPath+"/cards/"+strconv.FormatInt(card, 10)+"/"+verb, form)
}

// chatSection is the page's chat section, "" for none.
func chatSection(page string) string {
	return section(page, tChatSection, `id="officer-timeline"`)
}

// TestOwnStateReadsOnlyItsOwnOfficersRows: officer_my_state, called by the
// officer's own instance, answers ITS draft, refusal and schedule and none of
// another officer of the same owner, nor of the same officer id under another
// owner. CONTROLS: the same read for each of those two answers its OWN rows,
// so their absence above is the scope, not an empty fixture.
func TestOwnStateReadsOnlyItsOwnOfficersRows(t *testing.T) {
	w := newOutboundWorld(t, true)
	now := time.Now()
	mine := w.readyDraft()
	_, myRefusal := w.refusedDraft("Mine was refused.", "the window is closed")
	if _, err := w.store.PutSchedule(w.ctx, 1, builtinOfficerID, builtin.Schedule{Name: "mine-daily", Cron: "0 9 * * *", Kind: "manual.run", SubjectKey: "mine-daily", Enabled: true}, now); err != nil {
		t.Fatal(err)
	}
	// Another officer of user 1, and the same officer id under user 2: each
	// with a pending draft (filed by the world's officer, then moved), a
	// refusal and a schedule.
	sameOwner := w.readyDraft()
	w.draftSQL(sameOwner, `officer = '`+tOtherOfficer+`'`)
	sys := authz.System(w.ctx)
	otherOwner := w.readyDraft()
	w.draftSQL(otherOwner, `owner_id = `+strconv.FormatInt(w.other, 10)+`, plugin_id = (SELECT id FROM plugins WHERE name = 'example-other')`)
	for _, o := range []struct {
		owner   int64
		officer string
		tag     string
		draft   int64
	}{{1, tOtherOfficer, "elsewhere", sameOwner}, {w.other, builtinOfficerID, "outsider", otherOwner}} {
		if err := w.store.PutType(sys, o.owner, builtin.Type{Key: "marketing", Name: "Example"}); err != nil {
			t.Fatal(err)
		}
		if err := w.store.PutDef(sys, o.owner, builtin.Def{Officer: o.officer, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		payload, _ := json.Marshal(map[string]any{"draft_id": o.draft, "venue": "x", "by": "plugin", "detail": o.tag + " was refused", "target": map[string]any{"kind": "post"}})
		if _, _, err := w.store.Enqueue(sys, o.owner, builtin.NewItem{Officer: o.officer, Kind: builtin.DraftRefusedKind, SubjectKey: o.tag + "-refusal", Payload: payload}, now); err != nil {
			t.Fatal(err)
		}
		if _, err := w.store.PutSchedule(sys, o.owner, o.officer, builtin.Schedule{Name: o.tag + "-daily", Cron: "0 7 * * *", Kind: "manual.run", SubjectKey: o.tag, Enabled: true}, now); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		_, _ = w.f.pool.Exec(sys, `DELETE FROM officer_schedules WHERE officer = $1`, tOtherOfficer)
		_, _ = w.f.pool.Exec(sys, `DELETE FROM officer_items WHERE officer = $1`, tOtherOfficer)
		_, _ = w.f.pool.Exec(sys, `DELETE FROM officer_defs WHERE officer = $1`, tOtherOfficer)
		_, _ = w.f.pool.Exec(sys, `DELETE FROM officer_schedules WHERE owner_id = $1`, w.other)
	})

	type state struct {
		Drafts []struct {
			DraftID int64 `json:"draft_id"`
		} `json:"drafts"`
		Refusals []struct {
			ItemID int64  `json:"item_id"`
			Why    string `json:"why"`
		} `json:"refusals"`
		Schedules []struct {
			Name string `json:"name"`
		} `json:"schedules"`
		Budget map[string]any `json:"budget"`
	}
	read := func(out string) state {
		t.Helper()
		var s state
		if isToolErr(out) || json.Unmarshal([]byte(out), &s) != nil {
			t.Fatalf("officer_my_state: %s", out)
		}
		return s
	}
	got := read(w.call(officers.ToolMyState, `{}`))
	if len(got.Drafts) != 1 || got.Drafts[0].DraftID != mine {
		t.Errorf("drafts %+v; want only its own %d (not %d of %s, nor %d of user %d)", got.Drafts, mine, sameOwner, tOtherOfficer, otherOwner, w.other)
	}
	if len(got.Refusals) != 1 || got.Refusals[0].ItemID != myRefusal {
		t.Errorf("refusals %+v; want only its own item %d", got.Refusals, myRefusal)
	}
	if len(got.Schedules) != 1 || got.Schedules[0].Name != "mine-daily" {
		t.Errorf("schedules %+v; want only mine-daily", got.Schedules)
	}
	if got.Budget == nil {
		t.Errorf("no budget in the whole state")
	}
	// 🔴 Naming another officer is refused, never answered for the caller's.
	if out := w.call(officers.ToolMyState, `{"officer":"`+tOtherOfficer+`"}`); !isToolErr(out) || !strings.Contains(out, "cannot name another officer") {
		t.Errorf("officer_my_state naming %s: %s", tOtherOfficer, out)
	}

	// CONTROLS: the read for each other officer sees its own rows.
	for _, c := range []struct {
		o     officers.Officer
		draft int64
		tag   string
	}{
		{officers.Officer{ID: tOtherOfficer, Owner: 1, Backend: officers.BackendBuiltin}, sameOwner, "elsewhere"},
		{officers.Officer{ID: builtinOfficerID, Owner: w.other, Backend: officers.BackendBuiltin}, otherOwner, "outsider"},
	} {
		s := read(w.srv.officerMyState(w.ctx, c.o, `{}`))
		if len(s.Drafts) != 1 || s.Drafts[0].DraftID != c.draft {
			t.Errorf("control %s of %d: drafts %+v; want its own %d", c.o.ID, c.o.Owner, s.Drafts, c.draft)
		}
		if len(s.Refusals) != 1 || s.Refusals[0].Why != c.tag+" was refused" {
			t.Errorf("control %s of %d: refusals %+v", c.o.ID, c.o.Owner, s.Refusals)
		}
		if len(s.Schedules) != 1 || s.Schedules[0].Name != c.tag+"-daily" {
			t.Errorf("control %s of %d: schedules %+v", c.o.ID, c.o.Owner, s.Schedules)
		}
	}
}

// TestTheActingToolsRunOnlyInAnOperatorChat: officer_confirm_card,
// officer_refusal_refile and officer_refusal_drop are refused to the
// instance's primary and to an HQ chat lane; in user 1's operator chat they
// act — the drop is the page's own (owner_decision drop). A card for another
// officer's draft is refused as not its own.
func TestTheActingToolsRunOnlyInAnOperatorChat(t *testing.T) {
	w := newOutboundWorld(t, true)
	wipeCards(t)
	t.Cleanup(func() { wipeCards(t) })
	id := w.readyDraft()
	_, refusal := w.refusedDraft("A refused post.", "the window is closed")
	args := map[string]string{
		officers.ToolConfirmCard:   `{"draft_id":` + strconv.FormatInt(id, 10) + `}`,
		officers.ToolRefusalRefile: `{"item_id":` + strconv.FormatInt(refusal, 10) + `,"when":"1h"}`,
		officers.ToolRefusalDrop:   `{"item_id":` + strconv.FormatInt(refusal, 10) + `}`,
	}
	for name, a := range args {
		for where, ctx := range map[string]context.Context{
			"the primary":  w.ctx,
			"an HQ chat":   withChatLaneCall(w.ctx, officers.ChatSessionKey("4242")),
			"a forged key": withChatLaneCall(w.ctx, officers.ChatSessionKey("operator-01")),
		} {
			if out := w.srv.dispatchAgentTool(ctx, w.agent, name, a); !isToolErr(out) || !strings.Contains(out, tChatOnlyWords) {
				t.Errorf("%s from %s: %s; want the operator-chat-only refusal", name, where, out)
			}
		}
	}
	if n := len(w.cards()); n != 0 {
		t.Fatalf("a refused officer_confirm_card recorded %d cards", n)
	}
	// Another officer's draft is not its own.
	theirs := w.readyDraft()
	w.draftSQL(theirs, `officer = '`+tOtherOfficer+`'`)
	if out := w.srv.dispatchAgentTool(laneCtx(w.ctx, 1), w.agent, officers.ToolConfirmCard, `{"draft_id":`+strconv.FormatInt(theirs, 10)+`}`); !isToolErr(out) || !strings.Contains(out, "is not one of your drafts") {
		t.Errorf("a card for %s's draft: %s", tOtherOfficer, out)
	}
	// In the operator chat: the card, and the page's own drop.
	card := w.cardFor(args[officers.ToolConfirmCard])
	if cs := w.cards(); len(cs) != 1 || cs[0].id != card || cs[0].owner != 1 || cs[0].draft != id || cs[0].kind != "draft" {
		t.Fatalf("the card: %+v", cs)
	}
	if out := w.srv.dispatchAgentTool(laneCtx(w.ctx, 1), w.agent, officers.ToolRefusalDrop, args[officers.ToolRefusalDrop]); isToolErr(out) {
		t.Fatalf("drop in the operator chat: %s", out)
	}
	if it := w.itemFull(refusal); it.payload["owner_decision"] != "drop" || it.summary != tDroppedWords {
		t.Fatalf("after the chat's drop: %+v", it)
	}
}

// TestAnOfficerNeverDecidesItsOwnWork: every door that decides a draft, an
// outreach batch or a confirm card refuses an officer caller — the officer's
// principal, and any call an officer tool reached — with its own words, and
// nothing is decided. CONTROL: the owner's signed-in session decides the
// same draft and card.
func TestAnOfficerNeverDecidesItsOwnWork(t *testing.T) {
	w := newOutboundWorld(t, true)
	wipeCards(t)
	t.Cleanup(func() { wipeCards(t) })
	id := w.readyDraft()
	ids, _ := w.followsFor("ivy")
	batch := plugins.BatchID(1, builtinOfficerID, w.draft(ids[0]).CreatedAt.UTC().Format("2006-01-02"))
	card := w.cardFor(`{"draft_id":` + strconv.FormatInt(id, 10) + `}`)
	d := w.draft(id)
	session := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession})
	o, _, _ := w.srv.officerOf(w.agent)
	callers := map[string]context.Context{
		"the officer's principal":        w.srv.officerPrincipal(context.Background(), 1),
		"an agent token's principal":     authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaAgent}),
		"a call an officer tool reached": withOfficerCall(session, o),
	}
	for name, ctx := range callers {
		if _, r := w.srv.decideDraftAs(ctx, id, plugins.DraftDecision{Approve: true, BodySHA256: plugins.DraftBodySHA256(d.Body), MediaSHA256: d.MediaSHA256(),
			Via: plugins.DraftDecisionSource{Via: plugins.DraftViaBrowser}}); r == nil || r.code != http.StatusForbidden || r.msg != tSelfDecide {
			t.Errorf("decideDraftAs as %s: %+v; want the self-decide refusal", name, r)
		}
		if _, r := w.srv.decideBatchAs(ctx, batch, plugins.BatchDecision{BatchSHA256: plugins.BatchSHA256(ids), Approve: ids,
			Via: plugins.DraftDecisionSource{Via: plugins.DraftViaBrowser}}); r == nil || r.code != http.StatusForbidden || r.msg != tSelfDecide {
			t.Errorf("decideBatchAs as %s: %+v; want the self-decide refusal", name, r)
		}
		cs := w.cards()
		if _, r := w.srv.decideChatCard(ctx, 1, builtinOfficerID, card, cs[0].sha, true); r == nil || r.code != http.StatusForbidden || r.msg != tSelfDecide {
			t.Errorf("decideChatCard as %s: %+v; want the self-decide refusal", name, r)
		}
	}
	// The officer's own tools and credentials (invariant guards: refused
	// before the decision by their doors' own rules).
	for _, tool := range []struct{ name, args string }{
		{pluginDraftDecideTool, `{"id":` + strconv.FormatInt(id, 10) + `,"decision":"approve","body_sha256":"` + plugins.DraftBodySHA256(d.Body) + `"}`},
		{pluginOutboundBatchDecideTool, `{"batch_id":"` + batch + `","batch_sha256":"` + plugins.BatchSHA256(ids) + `","approve_ids":[` + strconv.FormatInt(ids[0], 10) + `]}`},
	} {
		if out := w.call(tool.name, tool.args); !isToolErr(out) {
			t.Errorf("the officer's own %s: %s; want a refusal", tool.name, out)
		}
	}
	cs := w.cards()
	for name, cred := range map[string]func(*http.Request){
		"the instance's token":  func(r *http.Request) { r.Header.Set("X-Muster-Token", w.agent.HooksToken) },
		"the shared hook token": func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+connTestHookToken) },
	} {
		rec := w.do("POST", tChatPath+"/cards/"+strconv.FormatInt(card, 10)+"/approve", url.Values{"card_sha256": {cs[0].sha}}.Encode(), func(r *http.Request) {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", "http://muster.test")
			cred(r)
		})
		if rec.Code < 400 {
			t.Errorf("%s approving the card: %d; want a refusal", name, rec.Code)
		}
	}
	if got := w.draft(id).Status; got != plugins.DraftPending {
		t.Fatalf("an officer caller decided its draft: %s", got)
	}
	for _, x := range ids {
		if got := w.draft(x).Status; got != plugins.DraftPending {
			t.Fatalf("an officer caller decided its batch: %d is %s", x, got)
		}
	}
	if cs := w.cards(); cs[0].decision != "" {
		t.Fatalf("an officer caller decided its card: %+v", cs[0])
	}
	// CONTROL: the owner's own click.
	if rec := w.cardPost(card, "approve", url.Values{"card_sha256": {cs[0].sha}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("control: the owner's approval of the card: %d %.400s", rec.Code, rec.Body.String())
	}
	if got := w.draft(id).Status; got != plugins.DraftApproved {
		t.Fatalf("control: after the owner's approval: %s", got)
	}
}

// TestAHostOfficerCannotDecideItsOwnWork: a host officer's own window, bound
// to its instance, is refused the mirrored sessions' decide tools
// (invariant guard: hostInstanceTool's set).
func TestAHostOfficerCannotDecideItsOwnWork(t *testing.T) {
	w := newHostOutboundWorld(t, true)
	id := w.readyDraft()
	d := w.draft(id)
	out := w.call(pluginDraftDecideTool, `{"id":`+strconv.FormatInt(id, 10)+`,"decision":"approve","body_sha256":"`+plugins.DraftBodySHA256(d.Body)+`"}`)
	if !isToolErr(out) {
		t.Fatalf("the host officer's own plugin_draft_decide: %s", out)
	}
	if got := w.draft(id).Status; got != plugins.DraftPending {
		t.Fatalf("the host officer decided its own draft: %s", got)
	}
}

// TestAConfirmCardApprovesExactlyWhatItShowed: a card drawn in user 1's chat
// shows the draft's exact text and media and binds them; a form for another
// card is refused; text changed after it was drawn, or media reordered, is
// refused and decides nothing (the page says so); the card as drawn approves
// exactly its SHA-256s, once.
func TestAConfirmCardApprovesExactlyWhatItShowed(t *testing.T) {
	w := newOutboundWorld(t, true)
	wipeCards(t)
	t.Cleanup(func() { wipeCards(t) })
	changed := w.readyDraft()
	reordered := w.readyDraft()
	kept := w.readyDraft()
	cardChanged := w.cardFor(`{"draft_id":` + strconv.FormatInt(changed, 10) + `}`)
	cardReordered := w.cardFor(`{"draft_id":` + strconv.FormatInt(reordered, 10) + `}`)
	cardKept := w.cardFor(`{"draft_id":` + strconv.FormatInt(kept, 10) + `}`)
	sha := map[int64]string{}
	for _, c := range w.cards() {
		sha[c.id] = c.sha
	}
	chat := chatSection(w.officerPage())
	for _, want := range []string{`data-chat-card="` + strconv.FormatInt(cardKept, 10) + `"`, `data-chat-card-body>` + html.EscapeString(twoMediaBody) + `</p>`,
		`name="card_sha256" value="` + sha[cardKept] + `"`, `action="` + tChatPath + `/cards/` + strconv.FormatInt(cardKept, 10) + `/approve"`, "launch/hero-1", "launch/demo-2"} {
		if !strings.Contains(chat, want) {
			t.Errorf("the chat lacks %q", want)
		}
	}
	// Another card's form.
	if rec := w.cardPost(cardKept, "approve", url.Values{"card_sha256": {sha[cardChanged]}}); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), html.EscapeString(tCardNotShown)) {
		t.Fatalf("approving with another card's sha: %d %.300s", rec.Code, rec.Body.String())
	}
	// Changed after the card was drawn.
	w.draftSQL(changed, `body = body || ' (edited)'`)
	w.draftSQL(reordered, `media_refs = ARRAY['launch/demo-2', 'launch/hero-1']`)
	for _, c := range []struct {
		card, draft int64
	}{{cardChanged, changed}, {cardReordered, reordered}} {
		rec := w.cardPost(c.card, "approve", url.Values{"card_sha256": {sha[c.card]}})
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), html.EscapeString(tCardChanged)) {
			t.Fatalf("approving card %d after its draft changed: %d %.400s", c.card, rec.Code, rec.Body.String())
		}
		if got := w.draft(c.draft).Status; got != plugins.DraftPending {
			t.Fatalf("a changed draft %d was decided: %s", c.draft, got)
		}
	}
	if !strings.Contains(chatSection(w.officerPage()), "It changed since this card was drawn") {
		t.Errorf("the chat does not say a changed card's draft changed")
	}
	// The card as drawn.
	rec := w.cardPost(cardKept, "approve", url.Values{"card_sha256": {sha[cardKept]}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != tOfficerBack+"?notice=card-approved#"+tChatSection {
		t.Fatalf("approving the card as drawn: %d %q %.300s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	d := w.draft(kept)
	if d.Status != plugins.DraftApproved || d.ApprovedBodySHA256 == nil || *d.ApprovedBodySHA256 != sumHex([]byte(twoMediaBody)) {
		t.Fatalf("after the card's approval: %s %v", d.Status, d.ApprovedBodySHA256)
	}
	if rec := w.cardPost(cardKept, "approve", url.Values{"card_sha256": {sha[cardKept]}}); rec.Code != http.StatusConflict {
		t.Fatalf("a second approval of the card: %d", rec.Code)
	}
	if !strings.Contains(chatSection(w.officerPage()), `data-chat-card-decided="approved"`) {
		t.Errorf("the chat does not show the card approved")
	}
}

// TestAConfirmCardApprovalNeedsARecentSignIn: a card's Approve on a stale
// sign-in is refused (the officer page, saying why), deciding nothing — and
// the page carries the step-up form script for the card (data-step-up-form).
func TestAConfirmCardApprovalNeedsARecentSignIn(t *testing.T) {
	w := newOutboundWorld(t, true)
	wipeCards(t)
	t.Cleanup(func() { wipeCards(t) })
	id := w.readyDraft()
	card := w.cardFor(`{"draft_id":` + strconv.FormatInt(id, 10) + `}`)
	page := w.officerPage()
	if !strings.Contains(chatSection(page), "data-step-up-form") || !strings.Contains(page, "musterStepUp") {
		t.Errorf("the page lacks the card's step-up form or dialog")
	}
	rec := w.do("POST", tChatPath+"/cards/"+strconv.FormatInt(card, 10)+"/approve", url.Values{"card_sha256": {w.cards()[0].sha}}.Encode(), func(r *http.Request) {
		w.operatorForm(r)
		r.Header.Set("Cookie", staleCookieFor(w.srv))
	})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), html.EscapeString(DraftStepUpRequired)) {
		t.Fatalf("a stale approval: %d %.300s", rec.Code, rec.Body.String())
	}
	if got := w.draft(id).Status; got != plugins.DraftPending {
		t.Fatalf("a stale approval decided the draft: %s", got)
	}
}

// TestABatchCardApprovesItsEntriesOrNothing: a batch card approves every
// entry it showed; once another entry has joined the batch, it decides
// nothing.
func TestABatchCardApprovesItsEntriesOrNothing(t *testing.T) {
	w := newOutboundWorld(t, true)
	wipeCards(t)
	t.Cleanup(func() { wipeCards(t) })
	ids, _ := w.followsFor("jo")
	batch := plugins.BatchID(1, builtinOfficerID, w.draft(ids[0]).CreatedAt.UTC().Format("2006-01-02"))
	card := w.cardFor(`{"batch_id":"` + batch + `"}`)
	sha := w.cards()[0].sha
	more, _ := w.followsFor("kai")
	rec := w.cardPost(card, "approve", url.Values{"card_sha256": {sha}})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), html.EscapeString(tCardChanged)) {
		t.Fatalf("a batch card after an entry joined: %d %.300s", rec.Code, rec.Body.String())
	}
	for _, x := range append(ids, more...) {
		if got := w.draft(x).Status; got != plugins.DraftPending {
			t.Fatalf("a changed batch was decided: %d is %s", x, got)
		}
	}
	fresh := w.cardFor(`{"batch_id":"` + batch + `"}`)
	var freshSHA string
	for _, c := range w.cards() {
		if c.id == fresh {
			freshSHA = c.sha
		}
	}
	if rec := w.cardPost(fresh, "approve", url.Values{"card_sha256": {freshSHA}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("the new batch card: %d %.300s", rec.Code, rec.Body.String())
	}
	for _, x := range append(ids, more...) {
		if got := w.draft(x).Status; got != plugins.DraftApproved {
			t.Fatalf("after the batch card: %d is %s", x, got)
		}
	}
}

// TestAChatAnswersPastItsBudgetAndItsSpendIsCounted: the officer is past its
// daily budget (held), and its owner's message is still sent as a turn of
// their own chat lane — with the lane's MCP session and secret, the header
// on its first turn — and the reply is drawn on the page, without the
// header; the turn ends by booking the instance's usage into
// officer_llm_usage. The lane's tool calls, verified by that secret, are the
// person's chat (officer_confirm_card answers).
func TestAChatAnswersPastItsBudgetAndItsSpendIsCounted(t *testing.T) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	src := fixedUsage{buckets: []cctranscript.UsageBucket{{Session: "lane-1", Hour: today.Add(time.Hour), APICalls: 7,
		InputTokens: 300, OutputTokens: 40, Model: "claude-sonnet-5-5"}}}
	w := newOutboundWorldWith(t, true, outboundFixture{officerExtra: `,"daily_budget_usd":2`, usage: src})
	wipeCards(t)
	t.Cleanup(func() { wipeCards(t) })
	llmCleanup(t, w.f, builtinOfficerID)
	// Past the cap: $5 booked today against $2.
	if _, err := w.f.pool.Exec(authz.System(w.ctx), `INSERT INTO officer_llm_usage (owner_id, officer, pool, instance, day, requests, cost_usd)
		VALUES (1, $1, 'judgement', 9, $2, 1, 5)`, builtinOfficerID, today); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.officerPage(), "data-run-held") {
		t.Fatal("fixture: the officer is not drawn held")
	}
	gw := &laneChat{reply: func() (string, error) { return "Two drafts wait on you.", nil }}
	w.srv.ext.Gateway = gw
	const msg = "What waits on me today?"
	rec := w.post(tChatPath, url.Values{"message": {msg}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != tOfficerBack+"?notice=chat-sent#"+tChatSection {
		t.Fatalf("send: %d %q %.300s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	w.srv.WaitForOfficerChats()
	sent := gw.sent()
	if len(sent) != 1 {
		t.Fatalf("turns sent: %+v; want one", sent)
	}
	turn := sent[0]
	if turn.sessionKey != tOperatorLane || turn.mcpSession != "1" || turn.laneSecret == "" || !strings.HasSuffix(turn.text, msg) ||
		!strings.Contains(turn.text, officers.ToolConfirmCard) {
		t.Fatalf("the turn: %+v", turn)
	}
	chat := chatSection(w.officerPage())
	for _, want := range []string{`data-chat-message="user"`, ">" + msg + "</p>", `data-chat-message="assistant"`, ">Two drafts wait on you.</p>"} {
		if !strings.Contains(chat, want) {
			t.Errorf("the chat lacks %q\n%.1500s", want, chat)
		}
	}
	if strings.Contains(chat, officers.ToolConfirmCard) || strings.Contains(chat, "data-chat-failed") {
		t.Errorf("the chat draws the turn's header, or a failure:\n%.1500s", chat)
	}
	// Counted: the flush booked the source's bucket for instance 1.
	var reqs int64
	if err := w.f.pool.QueryRow(authz.System(w.ctx), `SELECT coalesce(sum(requests), 0) FROM officer_llm_usage WHERE officer = $1 AND instance = 1 AND day = $2`,
		builtinOfficerID, today).Scan(&reqs); err != nil {
		t.Fatal(err)
	}
	if reqs != 7 {
		t.Fatalf("instance 1's requests booked today: %d; want the chat's 7", reqs)
	}
	// The lane's calls, verified by its secret, are user 1's chat.
	st := w.srv.ext.Agents.(agents.OfficerChatStore)
	sess, err := st.SessionByKeyAnyAgent(w.srv.officerPrincipal(w.ctx, 1), tOperatorLane)
	if err != nil || sess.AgentID != w.agent.ID {
		t.Fatalf("the lane's session: %+v %v", sess, err)
	}
	lane, refusal := w.srv.verifyChatLaneCall(w.ctx, w.agent, tOperatorLane, turn.laneSecret)
	if refusal != "" {
		t.Fatalf("the lane's secret: %s", refusal)
	}
	id := w.readyDraft()
	if out := w.srv.dispatchAgentTool(lane, w.agent, officers.ToolConfirmCard, `{"draft_id":`+strconv.FormatInt(id, 10)+`}`); isToolErr(out) {
		t.Fatalf("officer_confirm_card from the verified lane: %s", out)
	}
}
