package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// THE HQ-DERIVED ASK TOOLS (officer_item_asks.go): officer_task_ask (civ-hq
// #509, a task-reconciler officer's) and officer_flood_block_propose (civ-hq
// #510, a web-performance officer's). One HQ call each, with the officer's own
// token, which muster holds and the instance never sees; HQ derives the
// person and applies the answer. Names, paths and codes are spelled as
// literals on purpose: they are the contract.
// ---------------------------------------------------------------------------

const (
	taskAskTool   = "officer_task_ask"
	floodTool     = "officer_flood_block_propose"
	offWP         = "web-performance:alpha-web"
	offWPTok      = "fixture-officer-token-webperf-6d0f"
	kindFinding   = "task-reconciler.finding"
	kindBotSurge  = "perf.bot_surge"
	wantFloodNote = "NO challenge is in place yet"
	wantTaskNote  = "NOTHING has been applied to the task yet"
)

// newItemAskRig is the rig over task-reconciler:hq (r.a) and
// web-performance:alpha-web (r.b). The release officer's token is known to
// the fake too, so a request carrying it would be ACCEPTED as that officer —
// which is what the token assertions below would catch.
func newItemAskRig(t *testing.T) *officerRig {
	t.Helper()
	return newOfficerRigWith(t, map[string]string{offTRTok: offTR, offWPTok: offWP, offATok: offA}, func(hqURL string) []officers.Officer {
		a := officers.Officer{ID: offTR, HQURL: hqURL, MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: time.Minute, Owner: 1}.WithToken(offTRTok)
		b := officers.Officer{ID: offWP, HQURL: hqURL, MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: time.Minute, Owner: 1}.WithToken(offWPTok)
		return []officers.Officer{a, b}
	})
}

// heldItemOf adds an item of kind for officer and has inst claim it.
func (r *officerRig) heldItemOf(t *testing.T, inst agents.Agent, officer, kind string) string {
	t.Helper()
	item := r.hq.AddItem(officer, kind, "subject:"+kind)
	if out := r.s.dispatchAgentTool(context.Background(), inst, officers.ToolClaim, `{"item_id":"`+item+`"}`); isToolErr(out) {
		t.Fatalf("claim: %s", out)
	}
	r.hq.ResetRequests()
	return item
}

func TestTheItemAskToolsAreOfferedByOfficerTypeOnly(t *testing.T) {
	r := newItemAskRig(t)
	ctx := context.Background()
	tr := r.addInstance(r.a, 1, 1001)
	wp := r.addInstance(r.b, 1, 1002)
	for _, c := range []struct {
		inst      agents.Agent
		has, lack string
	}{{tr, taskAskTool, floodTool}, {wp, floodTool, taskAskTool}} {
		listed := officerToolNameSet(r.s.MCPToolDefsFor(ctx, c.inst))
		sess, ok := r.s.OfficerClaudeCodeSession(c.inst)
		if !ok {
			t.Fatal("no officer session")
		}
		allowed := map[string]bool{}
		for _, m := range sess.AllowMCPTools {
			allowed[m] = true
		}
		if !listed[c.has] || !allowed[c.has] {
			t.Errorf("%s: %s listed %v, allowed %v; want both", c.inst.Name, c.has, listed[c.has], allowed[c.has])
		}
		if listed[c.lack] || allowed[c.lack] {
			t.Errorf("%s: %s listed %v, allowed %v; want neither", c.inst.Name, c.lack, listed[c.lack], allowed[c.lack])
		}
	}
	// Called anyway: refused at dispatch, and HQ never sees it.
	for _, c := range []struct {
		who  string
		a    agents.Agent
		tool string
	}{
		{"the web-performance officer", wp, taskAskTool},
		{"the task reconciler", tr, floodTool},
		{"an ordinary agent", r.agent(t, officerOrdinaryID), taskAskTool},
		{"an ordinary agent", r.agent(t, officerOrdinaryID), floodTool},
		{"the chief", r.agent(t, officerChiefID), taskAskTool},
		{"the chief", r.agent(t, officerChiefID), floodTool},
	} {
		r.hq.ResetRequests()
		out := r.s.dispatchAgentTool(ctx, c.a, c.tool, `{"item_id":"1","unit_ids":["prefix:a"],"ttl_hours":1}`)
		if !isToolErr(out) {
			t.Errorf("%s calling %s: %s, want a refusal", c.who, c.tool, out)
		}
		if n := len(r.hq.Requests()); n != 0 {
			t.Errorf("%s's refused %s reached HQ (%d requests)", c.who, c.tool, n)
		}
	}
}

// TestATaskAskSendsTheContractBodyWithTheOfficersToken: exactly one POST to
// …/items/{item}/task-asks, authorised with the OFFICER's token (muster's,
// never the instance's), its body exactly {instance, note} — instance the
// calling agent's own name — and HQ's answer passed back with the note that
// nothing is applied yet and the lease has ended.
func TestATaskAskSendsTheContractBodyWithTheOfficersToken(t *testing.T) {
	r := newItemAskRig(t)
	ctx := context.Background()
	inst := r.addInstance(r.a, 1, 1011)
	item := r.heldItemOf(t, inst, offTR, kindFinding)
	out := r.s.dispatchAgentTool(ctx, inst, taskAskTool, `{"item_id":"`+item+`","note":"closed PR #12 matches the done-when"}`)
	if isToolErr(out) {
		t.Fatalf("task ask: %s", out)
	}
	reqs := r.hq.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d HQ requests, want 1: %+v", len(reqs), reqs)
	}
	q := reqs[0]
	if q.Method != http.MethodPost || q.Path != "/api/v1/officers/task-reconciler:hq/items/"+item+"/task-asks" || q.Auth != "Bearer "+offTRTok {
		t.Fatalf("went %s %s with %q", q.Method, q.Path, q.Auth)
	}
	if got, want := normJSON(t, q.Body), normJSON(t, map[string]any{"instance": inst.Name, "note": "closed PR #12 matches the done-when"}); got != want {
		t.Fatalf("body:\n%s\nwant\n%s", got, want)
	}
	var res struct {
		Ask     map[string]any `json:"ask"`
		TaskAsk map[string]any `json:"task_ask"`
		Item    map[string]any `json:"item"`
		Note    string         `json:"note"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("not JSON: %s", out)
	}
	if res.TaskAsk["task_id"] != "fixture-task-1" || res.Ask["to"] != "fixture-owner" || res.Item["status"] != "waiting" {
		t.Errorf("result does not carry HQ's answer: %s", out)
	}
	for _, s := range []string{wantTaskNote, "ENDED", "do not heartbeat, complete or release"} {
		if !strings.Contains(res.Note, s) {
			t.Errorf("note does not say %q: %s", s, res.Note)
		}
	}
	// No note given: the body carries none (HQ's note is optional).
	inst2item := r.heldItemOf(t, inst, offTR, kindFinding)
	if out := r.s.dispatchAgentTool(ctx, inst, taskAskTool, `{"item_id":`+inst2item+`}`); isToolErr(out) {
		t.Fatalf("task ask with a numeric item id and no note: %s", out)
	}
	if got, want := normJSON(t, r.hq.Requests()[0].Body), normJSON(t, map[string]any{"instance": inst.Name}); got != want {
		t.Fatalf("body without a note:\n%s\nwant\n%s", got, want)
	}
}

// TestAFloodProposalSendsTheContractBodyWithTheOfficersToken: exactly one
// POST to …/flood-blocks with the officer's token, its body exactly
// {instance, unit_ids, action: managed_challenge, ttl_hours[, to]} — the
// action muster's, the ttl's number as given.
func TestAFloodProposalSendsTheContractBodyWithTheOfficersToken(t *testing.T) {
	r := newItemAskRig(t)
	ctx := context.Background()
	inst := r.addInstance(r.b, 1, 1021)
	item := r.heldItemOf(t, inst, offWP, kindBotSurge)
	out := r.s.dispatchAgentTool(ctx, inst, floodTool, `{"item_id":"`+item+`","unit_ids":["prefix:u2","prefix:u1"],"ttl_hours":1.5}`)
	if isToolErr(out) {
		t.Fatalf("proposal: %s", out)
	}
	reqs := r.hq.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d HQ requests, want 1: %+v", len(reqs), reqs)
	}
	q := reqs[0]
	if q.Method != http.MethodPost || q.Path != "/api/v1/officers/web-performance:alpha-web/items/"+item+"/flood-blocks" || q.Auth != "Bearer "+offWPTok {
		t.Fatalf("went %s %s with %q", q.Method, q.Path, q.Auth)
	}
	want := map[string]any{"instance": inst.Name, "unit_ids": []any{"prefix:u2", "prefix:u1"}, "action": "managed_challenge", "ttl_hours": 1.5}
	if got, w := normJSON(t, q.Body), normJSON(t, want); got != w {
		t.Fatalf("body:\n%s\nwant\n%s", got, w)
	}
	if !strings.Contains(string(q.Raw), `"ttl_hours":1.5`) {
		t.Errorf("ttl_hours not sent as the number given: %s", q.Raw)
	}
	var res struct {
		FloodBlock map[string]any `json:"flood_block"`
		Item       map[string]any `json:"item"`
		Note       string         `json:"note"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("not JSON: %s", out)
	}
	if res.FloodBlock["state"] != "pending" || res.Item["status"] != "waiting" {
		t.Errorf("result does not carry HQ's answer: %s", out)
	}
	for _, s := range []string{wantFloodNote, "ENDED", "never say traffic was challenged"} {
		if !strings.Contains(res.Note, s) {
			t.Errorf("note does not say %q: %s", s, res.Note)
		}
	}
	// A second proposal on the now-WAITING item, to a named approver: HQ
	// allows it, and muster does not pre-empt it with a lease check of its own.
	r.hq.ResetRequests()
	out = r.s.dispatchAgentTool(ctx, inst, floodTool, `{"item_id":"`+item+`","unit_ids":["prefix:u3"],"ttl_hours":24,"to":"perf-lead"}`)
	if isToolErr(out) {
		t.Fatalf("second proposal on a waiting item: %s", out)
	}
	want = map[string]any{"instance": inst.Name, "unit_ids": []any{"prefix:u3"}, "action": "managed_challenge", "ttl_hours": 24, "to": "perf-lead"}
	if got, w := normJSON(t, r.hq.Requests()[0].Body), normJSON(t, want); got != w {
		t.Fatalf("second body:\n%s\nwant\n%s", got, w)
	}
}

// TestTheItemAskToolsRefuseLocallyWhatMusterOwns: muster builds the body, so
// an argument it does not know (a token, an action, a task, another officer)
// or a wrongly typed one is refused before HQ is called — never dropped.
func TestTheItemAskToolsRefuseLocallyWhatMusterOwns(t *testing.T) {
	r := newItemAskRig(t)
	ctx := context.Background()
	tr := r.addInstance(r.a, 1, 1031)
	wp := r.addInstance(r.b, 1, 1032)
	for _, c := range []struct {
		a          agents.Agent
		tool, args string
	}{
		{tr, taskAskTool, `{}`},
		{tr, taskAskTool, `{"item_id":""}`},
		{tr, taskAskTool, `{"item_id":"1/../2"}`},
		{tr, taskAskTool, `{"item_id":"1","task_id":"868aaaa01"}`},
		{tr, taskAskTool, `{"item_id":"1","to":"someone"}`},
		{tr, taskAskTool, `{"item_id":"1","token":"x"}`},
		{tr, taskAskTool, `{"item_id":"1","note":7}`},
		{tr, taskAskTool, `{"item_id":"1","officer_id":"web-performance:alpha-web"}`},
		{wp, floodTool, `{"item_id":"1","ttl_hours":1}`},
		{wp, floodTool, `{"item_id":"1","unit_ids":[],"ttl_hours":1}`},
		{wp, floodTool, `{"item_id":"1","unit_ids":"prefix:a","ttl_hours":1}`},
		{wp, floodTool, `{"item_id":"1","unit_ids":["prefix:a"]}`},
		{wp, floodTool, `{"item_id":"1","unit_ids":["prefix:a"],"ttl_hours":"1"}`},
		{wp, floodTool, `{"item_id":"1","unit_ids":["prefix:a"],"ttl_hours":1,"action":"block"}`},
		{wp, floodTool, `{"item_id":"1","unit_ids":["prefix:a"],"ttl_hours":1,"prefixes":["1.2.3.0/24"]}`},
		{wp, floodTool, `{"unit_ids":["prefix:a"],"ttl_hours":1}`},
	} {
		r.hq.ResetRequests()
		out := r.s.dispatchAgentTool(ctx, c.a, c.tool, c.args)
		if !isToolErr(out) {
			t.Errorf("%s %s: %s, want a refusal", c.tool, c.args, out)
		}
		if n := len(r.hq.Requests()); n != 0 {
			t.Errorf("%s %s reached HQ (%d requests)", c.tool, c.args, n)
		}
	}
}

// refusedHQ is a tool result's refused.{status, code} and HQ's own body.
func refusedHQ(t *testing.T, out string) (int, string, map[string]any, string) {
	t.Helper()
	var got struct {
		Error   string `json:"error"`
		Refused struct {
			Status int            `json:"status"`
			Code   string         `json:"code"`
			HQ     map[string]any `json:"hq"`
		} `json:"refused"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %s", out)
	}
	return got.Refused.Status, got.Refused.Code, got.Refused.HQ, got.Error
}

// TestEveryHQRefusalReachesTheModelVerbatim: each documented refusal of each
// route comes back as a FINAL refusal (never "retry", never "unavailable")
// with HQ's status, its code, and HQ's body byte-for-byte under refused.hq —
// including the flood route's 503 flood_actuator_not_configured, a 5xx that is
// a decision — and the item is NOT ended (HQ wrote nothing).
func TestEveryHQRefusalReachesTheModelVerbatim(t *testing.T) {
	r := newItemAskRig(t)
	ctx := context.Background()
	tr := r.addInstance(r.a, 1, 1041)
	wp := r.addInstance(r.b, 1, 1042)
	trItem := r.heldItemOf(t, tr, offTR, kindFinding)
	wpItem := r.heldItemOf(t, wp, offWP, kindBotSurge)
	type ref struct {
		status int
		code   string
	}
	cases := map[string][]ref{
		"task-asks": {{400, "invalid"}, {403, "task_asks_off"}, {404, "not_found"}, {409, "officer_shadow"}, {409, "finding_void"},
			{409, "task_ask_open"}, {409, "task_cooldown"}, {409, "task_asks_policy_invalid"}, {409, "lease_lost"}, {409, "officer_disabled"},
			{422, "not_a_finding"}, {422, "no_recipient"}, {422, "no_options"}, {422, "unknown_recipient"}, {422, "recipient_muted"},
			{422, "recipient_unreachable"}, {429, "owner_daily_cap"}, {429, "rate_limited"}},
		"flood-blocks": {{400, "invalid"}, {403, "forbidden"}, {404, "not_found"}, {409, "lease_lost"}, {409, "flood_actuator_off"},
			{409, "flood_policy_invalid"}, {422, "too_many_prefixes"}, {422, "not_a_prefix_unit"}, {422, "not_a_bot_surge"},
			{422, "unit_not_surging"}, {422, "unit_unknown"}, {422, "prefix_refused"}, {422, "list_full"}, {422, "ttl_refused"},
			{422, "not_an_approver"}, {503, "flood_actuator_not_configured"}},
	}
	for verb, refs := range cases {
		inst, tool, item, args := tr, taskAskTool, trItem, `{"item_id":"`+trItem+`"}`
		if verb == "flood-blocks" {
			inst, tool, item, args = wp, floodTool, wpItem, `{"item_id":"`+wpItem+`","unit_ids":["prefix:a"],"ttl_hours":2}`
		}
		for _, c := range refs {
			msg := "fixture refusal " + c.code + " — exact words"
			r.hq.RefuseVerb(verb, c.status, c.code, msg)
			out := r.s.dispatchAgentTool(ctx, inst, tool, args)
			st, code, hq, text := refusedHQ(t, out)
			if !isToolErr(out) || st != c.status || code != c.code || strings.Contains(out, "retryable") || strings.Contains(out, "unavailable") {
				t.Errorf("%s %d %s: %s, want a final refusal %d %s", tool, c.status, c.code, out, c.status, c.code)
				continue
			}
			if hq["error"] != msg || hq["code"] != c.code {
				t.Errorf("%s %s: HQ's body not passed verbatim: %v", tool, c.code, hq)
			}
			if want := "HQ refused " + tool + " (" + strconv.Itoa(c.status) + " " + c.code + "): " + msg; text != want {
				t.Errorf("%s %s: error %q, want %q", tool, c.code, text, want)
			}
			if it := r.hq.Item(item); it.Status != "claimed" || it.ClaimedBy != inst.Name {
				t.Fatalf("%s %s: HQ's item changed on a refusal: %+v", tool, c.code, it)
			}
		}
		r.hq.RefuseVerb(verb, 0, "", "")
	}
}

// TestHQsOwnChecksComeBackAsRefusals: not forced — the fake's own lease and
// kind checks (HQ's) answer, and muster passes them on: an instance that does
// not hold the item gets 409 lease_lost, a finding tool on the wrong kind
// 422 not_a_finding / not_a_bot_surge.
func TestHQsOwnChecksComeBackAsRefusals(t *testing.T) {
	r := newItemAskRig(t)
	ctx := context.Background()
	tr := r.addInstance(r.a, 1, 1051)
	wp := r.addInstance(r.b, 1, 1052)
	unheld := r.hq.AddItem(offTR, kindFinding, "s:unheld")
	if st, code, _, _ := refusedHQ(t, r.s.dispatchAgentTool(ctx, tr, taskAskTool, `{"item_id":"`+unheld+`"}`)); st != 409 || code != "lease_lost" {
		t.Errorf("task ask on an unheld item: %d %s, want 409 lease_lost", st, code)
	}
	wrong := r.heldItemOf(t, tr, offTR, "task-reconciler.sweep")
	if st, code, _, _ := refusedHQ(t, r.s.dispatchAgentTool(ctx, tr, taskAskTool, `{"item_id":"`+wrong+`"}`)); st != 422 || code != "not_a_finding" {
		t.Errorf("task ask on a sweep item: %d %s, want 422 not_a_finding", st, code)
	}
	wpWrong := r.heldItemOf(t, wp, offWP, "perf.slow_route")
	if st, code, _, _ := refusedHQ(t, r.s.dispatchAgentTool(ctx, wp, floodTool, `{"item_id":"`+wpWrong+`","unit_ids":["prefix:a"],"ttl_hours":1}`)); st != 422 || code != "not_a_bot_surge" {
		t.Errorf("flood proposal on a slow-route item: %d %s, want 422 not_a_bot_surge", st, code)
	}
	if n := len(r.hq.DerivedAsks()); n != 0 {
		t.Errorf("HQ accepted %d asks, want 0", n)
	}
}

// TestAnOutageIsRetryNotARefusal: HQ down (a 503 with no decision code) is
// "unavailable, try again" — not passed off as HQ's decision.
func TestAnOutageIsRetryNotARefusal(t *testing.T) {
	r := newItemAskRig(t)
	ctx := context.Background()
	wp := r.addInstance(r.b, 1, 1061)
	item := r.heldItemOf(t, wp, offWP, kindBotSurge)
	r.hq.SetDown(true)
	out := r.s.dispatchAgentTool(ctx, wp, floodTool, `{"item_id":"`+item+`","unit_ids":["prefix:a"],"ttl_hours":1}`)
	if !isToolErr(out) || !strings.Contains(out, "unavailable") || strings.Contains(out, "refused") {
		t.Errorf("HQ down: %s, want unavailable", out)
	}
}
