package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/users"
)

// Outreach drafts through muster's doors (plugins outreach.go,
// outreach_batch.go): every server-side refusal at the filing, the
// approval's and the claim's re-check, the claim's payload and the result's
// record on the person, and the outreach batch that binds the exact set its
// owner ticked.

const outVenue = "example.social"

// outreach files one outreach draft from the instance; args are merged over
// {plugin, venue}. It answers the draft id (0 when refused) and the raw answer.
func (w *outboundWorld) outreach(args map[string]any) (int64, string) {
	w.t.Helper()
	args["plugin"], args["venue"] = outboundPluginName, outVenue
	b, _ := json.Marshal(args)
	return w.postDraft(string(b))
}

// refusedCode is an officer_post_draft answer's guard code ("" for none).
func outreachRefused(out string) string {
	var m struct {
		Refused string `json:"refused"`
	}
	_ = json.Unmarshal([]byte(out), &m)
	return m.Refused
}

// flags sets a contact's marks through the owner's door.
func (w *outboundWorld) flags(handle, body string) {
	w.t.Helper()
	rec := w.do("PUT", "/api/officers/defs/"+builtinOfficerID+"/contacts/"+outVenue+"/"+handle+"/flags", body, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+connTestHookToken)
	})
	if rec.Code != http.StatusOK {
		w.t.Fatalf("flags %s %s: %d %s", handle, body, rec.Code, rec.Body)
	}
}

// sendDM files a DM to handle citing because, checks it, approves its exact
// text, claims it and reports it sent: the draft id.
func (w *outboundWorld) sendDM(handle, because, body string) int64 {
	w.t.Helper()
	id, out := w.outreach(map[string]any{"target": map[string]any{"kind": "dm", "handle": handle}, "because_ref": because, "body": body})
	if id == 0 {
		w.t.Fatalf("file the dm: %s", out)
	}
	if rec := w.check(w.tok, id, true, ""); rec.Code != http.StatusOK {
		w.t.Fatalf("check: %d %s", rec.Code, rec.Body)
	}
	if rec := w.approve(id, bindingForm(body)); rec.Code != http.StatusSeeOther {
		w.t.Fatalf("approve: %d %s", rec.Code, rec.Body)
	}
	rec := w.claim()
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"draft_id":`+strconv.FormatInt(id, 10)) {
		w.t.Fatalf("claim: %d %s", rec.Code, rec.Body)
	}
	if rec := w.result(id, `{"ok":true,"external_ref":"dm-`+strconv.FormatInt(id, 10)+`"}`); rec.Code != http.StatusOK {
		w.t.Fatalf("result: %d %s", rec.Code, rec.Body)
	}
	return id
}

// TestAnOutreachFilingIsRefusedByEachGuard: officer_post_draft files nothing
// for a do-not-contact, bot or hostile person, a dm/follow/like without
// because_ref or with one that does not qualify, a second outreach while one
// is out, and a second contact with no engagement of theirs since — each
// with its own code; the same filing passes with the guard's condition met.
func TestAnOutreachFilingIsRefusedByEachGuard(t *testing.T) {
	w := newOutboundWorld(t, true)
	for _, h := range []string{"dnc-a", "bot-a", "hos-a", "ann", "ben", "cat"} {
		w.engagement(h, "reply", "r-"+h, "how does it work?")
	}
	w.engagement("ann", "like", "post-1", "")
	w.flags("dnc-a", `{"dnc":true}`)
	w.flags("bot-a", `{"bot":true}`)
	w.flags("hos-a", `{"hostile":true}`)
	dm := func(h, because string) map[string]any {
		a := map[string]any{"target": map[string]any{"kind": "dm", "handle": h}, "body": "Thanks for asking: it retries with backoff."}
		if because != "" {
			a["because_ref"] = because
		}
		return a
	}
	follow := func(h, because string) map[string]any {
		return map[string]any{"target": map[string]any{"kind": "follow", "handle": h}, "because_ref": because, "reason": "asked a good question"}
	}
	for _, c := range []struct {
		what string
		args map[string]any
		code string
	}{
		{"dm to a do-not-contact person", dm("dnc-a", "r-dnc-a"), "do_not_contact"},
		{"follow of a bot", follow("bot-a", "r-bot-a"), "bot"},
		{"like of a hostile person's reply", map[string]any{"target": map[string]any{"kind": "like", "ref": "r-hos-a"}, "because_ref": "r-hos-a", "reason": "r"}, "hostile"},
		{"dm citing their like", dm("ann", "post-1"), "no_qualifying_engagement"},
		{"dm citing another person's reply", dm("ann", "r-ben"), "no_qualifying_engagement"},
		{"dm to someone who never engaged", dm("nobody", "r-ann"), "unknown_person"},
	} {
		id, out := w.outreach(c.args)
		if id != 0 || outreachRefused(out) != c.code {
			t.Errorf("%s: want refused %q and nothing filed, got draft %d: %s", c.what, c.code, id, out)
		}
	}
	// No because_ref at all: refused before the guard, as an invalid filing
	// (no guard code: the store's own check, not the person's).
	invalid := func(what string, args map[string]any, want string) {
		t.Helper()
		if id, out := w.outreach(args); id != 0 || outreachRefused(out) != "" || !strings.Contains(out, want) {
			t.Errorf("%s: want an invalid filing naming %q, got draft %d: %s", what, want, id, out)
		}
	}
	invalid("a dm with no because_ref", dm("ann", ""), "cites because_ref")
	invalid("a follow with no because_ref", map[string]any{"target": map[string]any{"kind": "follow", "handle": "ann"}, "reason": "r"}, "cites because_ref")
	invalid("a like with no because_ref", map[string]any{"target": map[string]any{"kind": "like", "ref": "r-ann"}, "reason": "r"}, "cites because_ref")
	invalid("a follow with a body", map[string]any{"target": map[string]any{"kind": "follow", "handle": "ann"}, "because_ref": "r-ann", "reason": "r",
		"body": "hello"}, "carries no body")
	invalid("a follow with no reason", map[string]any{"target": map[string]any{"kind": "follow", "handle": "ann"}, "because_ref": "r-ann"}, "gives reason")
	// The control: a qualifying dm is filed; a second outreach while it is out is refused.
	if id, out := w.outreach(dm("ann", "r-ann")); id == 0 {
		t.Fatalf("control: a qualifying dm was refused: %s", out)
	}
	if id, out := w.outreach(follow("ann", "r-ann")); id != 0 || outreachRefused(out) != "outreach_outstanding" {
		t.Errorf("a second outreach while one is out: %d %s", id, out)
	}
	// Twice without an engagement in between, then the cooldown.
	w.sendDM("ben", "r-ben", "It retries with backoff.")
	if id, out := w.outreach(dm("ben", "r-ben")); id != 0 || outreachRefused(out) != "twice_without_engagement" {
		t.Errorf("a second dm with no engagement since: %d %s", id, out)
	}
	w.engagement("ben", "dm_in", "d-ben", "thanks, and what about timeouts?")
	if id, out := w.outreach(follow("ben", "r-ben")); id != 0 || outreachRefused(out) != "cooldown" {
		t.Errorf("inside the cooldown citing the old engagement: %d %s", id, out)
	}
	if id, out := w.outreach(dm("ben", "d-ben")); id == 0 {
		t.Errorf("answering their newer dm inside the cooldown was refused: %s", out)
	}
}

// TestAnOutreachClaimCarriesItsTargetAndASendRecordsIt: the claim of a dm
// carries its kind, handle and because_ref; its ok result records dm_out on
// the person and moves last_contacted.
func TestAnOutreachClaimCarriesItsTargetAndASendRecordsIt(t *testing.T) {
	w := newOutboundWorld(t, true)
	w.engagement("dot", "reply", "r-dot", "is there a free tier?")
	const body = "Yes, there is a free tier."
	id, out := w.outreach(map[string]any{"target": map[string]any{"kind": "dm", "handle": "@Dot"}, "because_ref": "r-dot", "body": body})
	if id == 0 {
		t.Fatalf("file: %s", out)
	}
	w.check(w.tok, id, true, "")
	if rec := w.approve(id, bindingForm(body)); rec.Code != http.StatusSeeOther {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body)
	}
	rec := w.claim()
	var c map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &c)
	tg, _ := c["target"].(map[string]any)
	if tg["kind"] != "dm" || tg["handle"] != "dot" || c["because_ref"] != "r-dot" || c["body"] != body {
		t.Fatalf("the claim: %s", rec.Body)
	}
	if rec := w.result(id, `{"ok":true,"external_ref":"conv-1"}`); rec.Code != http.StatusOK {
		t.Fatalf("result: %d %s", rec.Code, rec.Body)
	}
	var kind, ref string
	var contacted bool
	if err := w.f.pool.QueryRow(authz.System(w.ctx), `SELECT e.kind, e.ref, c.last_contacted IS NOT NULL FROM officer_contact_events e
		JOIN officer_contacts c ON c.id = e.contact_id WHERE e.draft_id = $1`, id).Scan(&kind, &ref, &contacted); err != nil {
		t.Fatalf("no outreach event for the sent dm: %v", err)
	}
	if kind != "dm_out" || ref != "conv-1" || !contacted {
		t.Fatalf("the outreach event: %s %s contacted %v", kind, ref, contacted)
	}
}

// TestAPersonMarkedAfterFilingIsNeverReached: a pending dm to a person who
// then says stop cannot be approved; an approved one is withdrawn at the
// claim, never handed out.
func TestAPersonMarkedAfterFilingIsNeverReached(t *testing.T) {
	w := newOutboundWorld(t, true)
	w.engagement("eve", "reply", "r-eve", "what is it?")
	w.engagement("fox", "reply", "r-fox", "what is it?")
	const body = "It is a small tool."
	pend, _ := w.outreach(map[string]any{"target": map[string]any{"kind": "dm", "handle": "eve"}, "because_ref": "r-eve", "body": body})
	appr, _ := w.outreach(map[string]any{"target": map[string]any{"kind": "dm", "handle": "fox"}, "because_ref": "r-fox", "body": body})
	if pend == 0 || appr == 0 {
		t.Fatal("control: the dms were not filed")
	}
	w.check(w.tok, pend, true, "")
	w.check(w.tok, appr, true, "")
	if rec := w.approve(appr, bindingForm(body)); rec.Code != http.StatusSeeOther {
		t.Fatalf("approve fox: %d %s", rec.Code, rec.Body)
	}
	w.engagement("eve", "dm_in", "d-eve", "please stop messaging me")
	w.engagement("fox", "block", "b-fox", "")
	if rec := w.approve(pend, bindingForm(body)); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "do-not-contact") {
		t.Fatalf("approving a dm to a person who said stop: %d", rec.Code)
	}
	if rec := w.claim(); strings.Contains(rec.Body.String(), `"draft_id":`+strconv.FormatInt(appr, 10)) {
		t.Fatalf("the claim handed out a dm to a person who blocked us: %s", rec.Body)
	}
	if d := w.draft(appr); d.Status != plugins.DraftRejected || !strings.Contains(d.Detail, "do-not-contact") {
		t.Fatalf("the withdrawn dm: %s %q", d.Status, d.Detail)
	}
}

// followsFor files a follow for each handle — each engaged first, its
// engagement item claimed and the follow filed for it (item_id) — and checks
// it sendable: the draft ids and their items.
func (w *outboundWorld) followsFor(handles ...string) (ids, items []int64) {
	w.t.Helper()
	for _, h := range handles {
		_, out := w.engagement(h, "reply", "r-"+h, "nice work")
		item := int64(out["item_id"].(float64))
		if got := w.srv.dispatchAgentTool(w.ctx, w.agent, officers.ToolClaim, `{"item_id":"`+strconv.FormatInt(item, 10)+`"}`); isToolErr(got) {
			w.t.Fatalf("claim %d: %s", item, got)
		}
		id, res := w.outreach(map[string]any{"target": map[string]any{"kind": "follow", "handle": h}, "because_ref": "r-" + h,
			"reason": "builder, engaged twice", "item_id": strconv.FormatInt(item, 10)})
		if id == 0 {
			w.t.Fatalf("follow %s: %s", h, res)
		}
		if rec := w.check(w.tok, id, true, ""); rec.Code != http.StatusOK {
			w.t.Fatalf("check: %d %s", rec.Code, rec.Body)
		}
		ids, items = append(ids, id), append(items, item)
	}
	return ids, items
}

// decideBatchForm is the batch card's form.
func (w *outboundWorld) decideBatchForm(batch, sha string, approve ...int64) *httptest.ResponseRecorder {
	form := url.Values{"batch_sha256": {sha}}
	for _, a := range approve {
		form.Add("approve", strconv.FormatInt(a, 10))
	}
	return w.do("POST", "/plugins/outreach-batches/"+batch+"/decide", form.Encode(), w.operatorForm)
}

// TestTheOutreachBatchBindsTheTickedSet: the day's follows are ONE batch on
// the card; an individual approval of one is refused; a decision on a stale
// set (an entry joined since) or naming a draft outside the batch decides
// nothing; the decision approves exactly the ticked ids, rejects every other
// "not selected", and finishes the un-ticked entry's item quietly — done
// no_action, no draft.refused, no escalation.
func TestTheOutreachBatchBindsTheTickedSet(t *testing.T) {
	w := newOutboundWorld(t, true)
	ids, items := w.followsFor("gil", "hal")
	batch := plugins.BatchID(1, builtinOfficerID, w.draft(ids[0]).CreatedAt.UTC().Format("2006-01-02"))
	page := w.do("GET", "/attention?tab=drafts", "", func(r *http.Request) { r.Header.Set("Cookie", sessionCookieFor(w.srv)) }).Body.String()
	if !strings.Contains(page, `data-outreach-batch="`+batch+`"`) || strings.Contains(page, `data-draft-id="`+strconv.FormatInt(ids[0], 10)+`"`) {
		t.Fatalf("the drafts tab does not draw the follows as one batch card")
	}
	if !strings.Contains(page, `name="batch_sha256" value="`+plugins.BatchSHA256(ids)+`"`) {
		t.Fatalf("the batch card does not bind the entries it draws")
	}
	if rec := w.approve(ids[0], bindingForm("")); rec.Code != http.StatusConflict {
		t.Fatalf("an individual approval of a follow: %d, want 409", rec.Code)
	}
	stale := plugins.BatchSHA256(ids)
	more, moreItems := w.followsFor("ivo") // joins after the owner looked
	ids, items = append(ids, more...), append(items, moreItems...)
	if rec := w.decideBatchForm(batch, stale, ids[0]); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "changed since you looked") {
		t.Fatalf("a decision on a stale set: %d", rec.Code)
	}
	fresh := plugins.BatchSHA256(ids)
	if rec := w.decideBatchForm(batch, fresh, ids[0], 999999); rec.Code != http.StatusBadRequest {
		t.Fatalf("ticking a draft outside the batch: %d", rec.Code)
	}
	for _, id := range ids {
		if got := w.draft(id).Status; got != plugins.DraftPending {
			t.Fatalf("a refused decision decided draft %d: %s", id, got)
		}
	}
	if rec := w.decideBatchForm(batch, fresh, ids[0], ids[2]); rec.Code != http.StatusSeeOther {
		t.Fatalf("the decision: %d %s", rec.Code, rec.Body)
	}
	for i, want := range []plugins.DraftStatus{plugins.DraftApproved, plugins.DraftRejected, plugins.DraftApproved} {
		got := w.draft(ids[i])
		if got.Status != want {
			t.Errorf("draft %d: %s, want %s", ids[i], got.Status, want)
		}
		if want == plugins.DraftRejected && (got.DecisionReason == nil || *got.DecisionReason != plugins.NotSelectedReason) {
			t.Errorf("the un-ticked draft's reason: %v", got.DecisionReason)
		}
	}
	if _, _, ok := w.refusedItemOf(ids[1]); ok {
		t.Fatal("an un-ticked entry queued a draft.refused item")
	}
	if st, outcome, summary, _ := w.itemRow(items[1]); st != "done" || outcome != "no_action" || !strings.Contains(summary, "not selected") {
		t.Fatalf("the un-ticked entry's item: %s %s %q; want done no_action, not selected", st, outcome, summary)
	}
	if st, _, _, _ := w.itemRow(items[0]); st != "waiting" {
		t.Fatalf("a ticked entry's item: %s; want still waiting on its draft", st)
	}
	// A ticked person who may no longer be contacted refuses the whole
	// decision of a later batch.
	nids, _ := w.followsFor("jan", "kay")
	w.engagement("kay", "block", "b-kay", "")
	nbatch := plugins.BatchID(1, builtinOfficerID, w.draft(nids[0]).CreatedAt.UTC().Format("2006-01-02"))
	// (the same UTC day: the batch is the decided one's remainder)
	if rec := w.decideBatchForm(nbatch, plugins.BatchSHA256(nids), nids[0], nids[1]); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "untick it") {
		t.Fatalf("ticking a person who blocked us: %d", rec.Code)
	}
	for _, id := range nids {
		if got := w.draft(id).Status; got != plugins.DraftPending {
			t.Fatalf("a refused decision decided draft %d: %s", id, got)
		}
	}
	// The plugin collects both approved follows, each with its handle.
	for range 2 {
		rec := w.claim()
		if !strings.Contains(rec.Body.String(), `"kind":"follow"`) || !strings.Contains(rec.Body.String(), `"because_ref":"r-`) {
			t.Fatalf("a claim: %s", rec.Body)
		}
	}
}

// TestTheBatchToolsAreTheOwnersMirroredSessionsOnly: an agent (or any caller
// that is not one of the owner's mirrored sessions) is refused both batch
// tools before anything is read.
func TestTheBatchToolsAreTheOwnersMirroredSessionsOnly(t *testing.T) {
	w := newOutboundWorld(t, true)
	for _, tool := range []string{pluginOutboundBatchGetTool, pluginOutboundBatchDecideTool} {
		out := w.srv.dispatchAgentTool(w.ctx, w.agent, tool, `{"batch_id":"`+builtinOfficerID+`@2000-01-01","batch_sha256":"`+strings.Repeat("a", 64)+`","approve_ids":[]}`)
		if !isToolErr(out) || !strings.Contains(out, "mirrored Claude Code sessions") {
			t.Errorf("%s from an agent: %s", tool, out)
		}
	}
}

// TestOutreachAuditRoundOne: the card shows the PERSON's own words even when
// someone else wrote in the same thread; a batch-approved follow to a person
// who then blocks is withdrawn at the claim and its item finished quietly; a
// follow rejected on its own queues no draft.refused; and a send whose
// person was erased after the claim is still recorded sent.
func TestOutreachAuditRoundOne(t *testing.T) {
	w := newOutboundWorld(t, true)
	ids, items := w.followsFor("lia", "max", "ned")
	if rec := w.do("POST", "/plugin/v1/messages", `{"external_id":"other-1","sender":"mallory","body":"EVIL WORDS","kind":"reply","venue":"`+outVenue+`","thread":"r-lia"}`,
		bearerTok(w.tok)); rec.Code != http.StatusAccepted {
		t.Fatalf("the other sender's message: %d %s", rec.Code, rec.Body)
	}
	page := w.do("GET", "/attention?tab=drafts", "", func(r *http.Request) { r.Header.Set("Cookie", sessionCookieFor(w.srv)) }).Body.String()
	if strings.Contains(page, "EVIL WORDS") || !strings.Contains(page, "nice work") {
		t.Fatalf("the batch card shows another sender's words as the person's engagement")
	}
	batch := plugins.BatchID(1, builtinOfficerID, w.draft(ids[0]).CreatedAt.UTC().Format("2006-01-02"))
	// a single reject of a follow: quiet
	if rec := w.rejectForm(ids[2], ""); rec.Code != http.StatusSeeOther {
		t.Fatalf("reject ned: %d", rec.Code)
	}
	if _, _, ok := w.refusedItemOf(ids[2]); ok {
		t.Fatal("a follow rejected on its own queued a draft.refused item")
	}
	if st, outcome, _, _ := w.itemRow(items[2]); st != "done" || outcome != "no_action" {
		t.Fatalf("the rejected follow's item: %s %s", st, outcome)
	}
	if rec := w.decideBatchForm(batch, plugins.BatchSHA256(ids[:2]), ids[0], ids[1]); rec.Code != http.StatusSeeOther {
		t.Fatalf("decide: %d %s", rec.Code, rec.Body)
	}
	w.engagement("lia", "block", "b-lia", "")
	// max is claimed first or second; collect both claims
	var got []string
	for range 2 {
		got = append(got, w.claim().Body.String())
	}
	if strings.Contains(strings.Join(got, ""), `"draft_id":`+strconv.FormatInt(ids[0], 10)) {
		t.Fatalf("a follow to a person who blocked us was handed out")
	}
	if st, outcome, _, _ := w.itemRow(items[0]); st != "done" || outcome != "no_action" {
		t.Fatalf("the withdrawn follow's item: %s %s; want done no_action", st, outcome)
	}
	// max was claimed: erase him, then the plugin reports the send.
	rec := w.do("DELETE", "/api/officers/defs/"+builtinOfficerID+"/contacts/"+outVenue+"/max", "", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+connTestHookToken)
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("erase: %d %s", rec.Code, rec.Body)
	}
	if rec := w.result(ids[1], `{"ok":true,"external_ref":"f-1"}`); rec.Code != http.StatusOK {
		t.Fatalf("the result of a send to an erased person: %d %s", rec.Code, rec.Body)
	}
	if d := w.draft(ids[1]); d.Status != plugins.DraftSent {
		t.Fatalf("the send to an erased person: %s", d.Status)
	}
}

// TestATeamsOutreachBatchIsItsManagersToDecide: a team officer's follows are
// their own batch (the owner is in its id, so a person's officer of the same
// id never shares the card); the team's MANAGER decides it from their signed-in
// session; the team itself, its operator, and the manager through a door
// that carries no team (their mirrored session) are refused 403 before
// anything is decided.
func TestATeamsOutreachBatchIsItsManagersToDecide(t *testing.T) {
	w := newOutboundWorld(t, true)
	sys := authz.System(w.ctx)
	pool := w.f.pool
	if _, err := pool.Exec(sys, `SELECT setval(pg_get_serial_sequence('users', 'id'), (SELECT max(id) FROM users))`); err != nil {
		t.Fatal(err)
	}
	person := func(n string) int64 {
		var id int64
		if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, n+strconv.FormatInt(time.Now().UnixNano(), 10)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	mgr, op := person("ob-mgr"), person("ob-op")
	us := users.NewPG(pool)
	team, err := us.CreateTeam(sys, users.Actor{UserID: 1, Via: authz.ViaSession}, "ob-team-"+strconv.FormatInt(time.Now().UnixNano(), 10), mgr)
	if err != nil {
		t.Fatal(err)
	}
	if err := us.SetTeamMember(sys, users.Actor{UserID: 1, Via: authz.ViaSession}, team.ID, op, authz.TeamOperator); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM plugin_reply_drafts WHERE owner_id = $1`, `DELETE FROM officer_contacts WHERE owner_id = $1`,
			`DELETE FROM officer_defs WHERE owner_id = $1`, `DELETE FROM officer_types WHERE owner_id = $1`,
			`DELETE FROM plugins WHERE owner_id = $1`, `DELETE FROM agents WHERE owner_id = $1`} {
			_, _ = pool.Exec(sys, q, team.ID)
		}
	})
	var agent, plugin, contact int64
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(pool.QueryRow(sys, `INSERT INTO agents (owner_id, name, namespace) VALUES ($1, $2, $2) RETURNING id`, team.ID, "ob-team-agent-"+strconv.FormatInt(team.ID, 10)).Scan(&agent))
	must(pool.QueryRow(sys, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope, capabilities) VALUES ($1, 'ob-team-relay', 'r', md5($2) || md5($2), 'r', '{messages,draft_checks}') RETURNING id`,
		team.ID, strconv.FormatInt(team.ID, 10)).Scan(&plugin))
	must(w.store.PutType(sys, team.ID, builtin.Type{Key: "marketing", Name: "Team"}))
	must(w.store.PutDef(sys, team.ID, builtin.Def{Officer: builtinOfficerID, Enabled: true}))
	must(pool.QueryRow(sys, `INSERT INTO officer_contacts (owner_id, officer, venue, handle) VALUES ($1, $2, $3, 'tzed') RETURNING id`, team.ID, builtinOfficerID, outVenue).Scan(&contact))
	var draft int64
	must(pool.QueryRow(sys, `INSERT INTO plugin_reply_drafts (owner_id, agent_id, agent_name, body, expires_at, check_state, checked_at, plugin_id, venue, target_kind, target_ref,
			officer, because_ref, contact_id, outreach_reason, requested_by_user)
		VALUES ($1, $2, 'a', '', now() + interval '1 day', 'sendable', now(), $3, $4, 'follow', 'tzed', $5, 'r1', $6, 'r', $1) RETURNING id`,
		team.ID, agent, plugin, outVenue, builtinOfficerID, contact).Scan(&draft))
	d, err := w.ps.Draft(sys, draft)
	must(err)
	batch := plugins.BatchID(team.ID, builtinOfficerID, d.CreatedAt.UTC().Format("2006-01-02"))
	if mine := plugins.BatchID(1, builtinOfficerID, d.CreatedAt.UTC().Format("2006-01-02")); mine == batch {
		t.Fatal("a person's and a team's officer of one id share a batch id")
	}
	member := func(uid int64, via authz.Via) context.Context {
		a, err := us.Account(sys, uid)
		must(err)
		return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: a.Role, Via: via, Teams: a.Teams})
	}
	teamActing := authz.WithPrincipal(context.Background(), authz.Principal{UserID: team.ID, Team: true, Role: authz.RoleMember, Via: authz.ViaAgent})
	dec := plugins.BatchDecision{BatchSHA256: plugins.BatchSHA256([]int64{draft}), Approve: []int64{draft}}
	for what, ctx := range map[string]context.Context{"the team itself": teamActing, "the team's operator": member(op, authz.ViaSession)} {
		if _, refused := w.srv.decideBatchAs(ctx, batch, dec); refused == nil || refused.code != http.StatusForbidden {
			t.Errorf("%s decided the team's batch: %+v", what, refused)
		}
	}
	if got, _ := w.ps.Draft(sys, draft); got.Status != plugins.DraftPending {
		t.Fatalf("a refused decision decided the draft: %s", got.Status)
	}
	if _, refused := w.srv.decideBatchAs(member(mgr, authz.ViaSession), batch, dec); refused != nil {
		t.Fatalf("the team's manager deciding the team's batch: %+v", refused)
	}
	if got, _ := w.ps.Draft(sys, draft); got.Status != plugins.DraftApproved {
		t.Fatalf("the manager's decision: %s", got.Status)
	}
}
