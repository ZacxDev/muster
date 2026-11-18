package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// officer_obs_raw (POST /officers/{id}/obs/raw, passed through) and THE ONE
// PER-ITEM READ BUDGET the three read tools share (officers/reads.go). The
// tool name is a literal here on purpose: the charters name it.
// ---------------------------------------------------------------------------

const obsRawTool = "officer_obs_raw"

// rawArgs is a valid raw query's arguments (no item_id).
const rawArgs = `{"backend":"prometheus","query":"sum by (node) (rate(fixture_oom_total[5m]))","start":"2000-01-01T00:00:00Z","end":"2000-01-01T01:00:00Z","step":"300s"}`

// obsQueryArgs is a valid named query's arguments.
const obsQueryArgs = `{"query_id":"fixture_error_lines","start":"2000-01-01T00:00:00Z","end":"2000-01-01T01:00:00Z"}`

// TestObsRawPassesTheCallThroughForTheHeldItem: a judgement instance holding
// one item sends HQ exactly its arguments plus that item (HQ's integer) and
// its own instance name, with its officer's own token, and gets HQ's answer
// back byte for byte.
func TestObsRawPassesTheCallThroughForTheHeldItem(t *testing.T) {
	r := newKubeOfficerRig(t)
	ctx := context.Background()
	judge := r.addNamed(offKJudge+"1", 1971)
	if !officerToolNameSet(r.s.MCPToolDefsFor(ctx, judge))[obsRawTool] {
		t.Fatalf("the judgement instance is not offered %s", obsRawTool)
	}
	sess, _ := r.s.OfficerClaudeCodeSession(judge)
	if !strings.Contains(","+strings.Join(sess.AllowMCPTools, ",")+",", ","+obsRawTool+",") {
		t.Fatalf("the judgement instance's session does not pre-allow %s: %v", obsRawTool, sess.AllowMCPTools)
	}
	item := r.hq.AddTieredItem(offK, "anomaly.alert", "node-a", officers.TierJudgement)
	r.hq.SetClaim(item, judge.Name, time.Now().Add(10*time.Minute))
	r.hq.ResetRequests()

	out := r.s.dispatchAgentTool(ctx, judge, obsRawTool, rawArgs)
	if isToolErr(out) || len(r.hq.ObsRawAnswers) != 1 || out != string(r.hq.ObsRawAnswers[0]) {
		t.Fatalf("answer %s, want HQ's answer byte for byte (%d answers)", out, len(r.hq.ObsRawAnswers))
	}
	var raw []hqtest.Request
	for _, q := range r.hq.Requests() {
		if strings.HasSuffix(q.Path, "/obs/raw") {
			raw = append(raw, q)
		}
	}
	if len(raw) != 1 || raw[0].Method != "POST" || raw[0].Path != "/api/v1/officers/"+offK+"/obs/raw" || raw[0].Auth != "Bearer "+offKTok {
		t.Fatalf("HQ saw %+v, want one POST /officers/%s/obs/raw with the officer's own token", raw, offK)
	}
	var want map[string]any
	_ = json.Unmarshal([]byte(rawArgs), &want)
	want["item_id"] = float64(numItem(t, item))
	want["instance"] = judge.Name
	gb, _ := json.Marshal(raw[0].Body)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Fatalf("HQ received %s, want %s", gb, wb)
	}
}

// TestObsRawRefusals: a plain agent is neither offered nor served the tool
// (a routine instance is, by the operator's decision:
// TestObsRawIsOfferedToARoutineInstanceAndCounted); a judgement instance
// holding no item, naming an item it does not
// hold, or naming its own instance is refused without a raw query reaching
// HQ; HQ's own refusal (403 not_investigating) comes back with HQ's code.
func TestObsRawRefusals(t *testing.T) {
	r := newKubeOfficerRig(t)
	ctx := context.Background()
	judge := r.addNamed(offKJudge+"1", 1981)
	plain := r.addNamed("brave-otter", 1983)
	untiered := r.addNamed(officers.InstanceName(offA, 1), 1984)
	for _, a := range []agents.Agent{plain, untiered} {
		if officerToolNameSet(r.s.MCPToolDefsFor(ctx, a))[obsRawTool] {
			t.Errorf("%s is offered %s", a.Name, obsRawTool)
		}
		if out := r.s.dispatchAgentTool(ctx, a, obsRawTool, rawArgs); !isToolErr(out) {
			t.Errorf("%s calling %s: %s, want a refusal", a.Name, obsRawTool, out)
		}
	}
	if out := r.s.dispatchAgentTool(ctx, judge, obsRawTool, rawArgs); !isToolErr(out) || !strings.Contains(out, "no claimed item") {
		t.Errorf("no item held: %s, want a refusal naming the item", out)
	}
	item := r.hq.AddTieredItem(offK, "anomaly.alert", "node-a", officers.TierJudgement)
	r.hq.SetClaim(item, judge.Name, time.Now().Add(10*time.Minute))
	other := r.hq.AddTieredItem(offK, "anomaly.alert", "node-b", officers.TierJudgement)
	for name, args := range map[string]string{
		"an item it does not hold": `{"item_id":"` + other + `",` + rawArgs[1:],
		"its instance":             `{"instance":"` + offKJudge + `2",` + rawArgs[1:],
		"its instance, any case":   `{"Instance":"` + judge.Name + `",` + rawArgs[1:],
		"another officer":          `{"officer":"` + offA + `",` + rawArgs[1:],
	} {
		if out := r.s.dispatchAgentTool(ctx, judge, obsRawTool, args); !isToolErr(out) {
			t.Errorf("naming %s: %s, want a refusal", name, out)
		}
	}
	// splitItemID removes only the exact key item_id: ITEM_ID / Item_Id reach
	// ObsRawBody, whose case-insensitive check must refuse them with ITS
	// words and before any request to /obs/raw (HQ's decoder would also
	// refuse the unknown key, with a 400 — a different, later refusal).
	r.hq.ResetRequests()
	for _, key := range []string{"ITEM_ID", "Item_Id"} {
		out := r.s.dispatchAgentTool(ctx, judge, obsRawTool, `{"`+key+`":"`+item+`",`+rawArgs[1:])
		if !isToolErr(out) || !strings.Contains(out, "is muster's to send") {
			t.Errorf("naming the item as %s: %s, want muster's refusal of the key", key, out)
		}
	}
	for _, q := range r.hq.Requests() {
		if strings.HasSuffix(q.Path, "/obs/raw") {
			t.Fatalf("a refused item key reached HQ: %+v", q)
		}
	}
	if n := len(r.hq.ObsRawCalls); n != 0 {
		t.Fatalf("%d raw queries reached HQ from refused calls", n)
	}
	// the positive control: the same instance's valid call does reach HQ
	if out := r.s.dispatchAgentTool(ctx, judge, obsRawTool, rawArgs); isToolErr(out) || len(r.hq.ObsRawCalls) != 1 {
		t.Fatalf("the valid call: %s (%d raw calls), want it served", out, len(r.hq.ObsRawCalls))
	}
	r.hq.ObsRawNotInvestigating = true
	if out := r.s.dispatchAgentTool(ctx, judge, obsRawTool, rawArgs); !isToolErr(out) || !strings.Contains(out, `"not_investigating"`) || !strings.Contains(out, "403") {
		t.Fatalf("HQ's 403: %s, want HQ's refusal with its code", out)
	}
}

// TestCountedReadToolsAreExactlyTheThree: the ledger of which officer tools
// spend the item's read budget — for the judgement pool exactly
// officer_kube_read, officer_obs_raw and officer_obs_query; for the routine
// pool exactly officer_obs_raw; for an untiered officer none. It fails when
// any set grows or shrinks.
func TestCountedReadToolsAreExactlyTheThree(t *testing.T) {
	r := newKubeOfficerRig(t)
	judge, _, ok := r.s.officerOf(r.addNamed(offKJudge+"1", 1991))
	routine, _, ok2 := r.s.officerOf(r.addNamed(offKRout+"1", 1992))
	if !ok || !ok2 || judge.Tier != officers.TierJudgement || routine.Tier != officers.TierRoutine {
		t.Fatal("the rig's instances do not resolve to their pools")
	}
	untiered, _, ok3 := r.s.officerOf(r.addNamed(officers.InstanceName(offA, 1), 1993))
	if !ok3 || untiered.Tier != "" {
		t.Fatal("the rig's untiered instance does not resolve to an untiered officer")
	}
	var j, rt, un []string
	for _, d := range officerToolDefs() {
		if officers.CountsRead(judge, d.Name) {
			j = append(j, d.Name)
		}
		if officers.CountsRead(routine, d.Name) {
			rt = append(rt, d.Name)
		}
		if officers.CountsRead(untiered, d.Name) {
			un = append(un, d.Name)
		}
	}
	sort.Strings(j)
	sort.Strings(rt)
	if strings.Join(j, ",") != "officer_kube_read,officer_obs_query,officer_obs_raw" || strings.Join(rt, ",") != "officer_obs_raw" || len(un) != 0 {
		t.Fatalf("counted: judgement %v, routine %v, untiered %v; want the three read tools, officer_obs_raw, and none", j, rt, un)
	}
	for _, d := range officerToolDefs() {
		if d.Name == officers.ToolKubeRead || d.Name == officers.ToolObsRaw || d.Name == officers.ToolObsQuery {
			if !strings.Contains(d.Description, officers.ReadBudgetNote) {
				t.Errorf("%s's description does not state the read budget", d.Name)
			}
			if strings.Contains(d.Description, "do not count") {
				t.Errorf("%s's description still says other reads do not count", d.Name)
			}
		}
	}
}

// readSeam is a server with a tiered officer whose judgement instance is
// approved for kube-read against a TLS API server, for the budget tests.
type readSeam struct {
	s       *Server
	hq      *hqtest.Server
	api     *seamAPI
	judge   agents.Agent
	routine agents.Agent
}

func newReadSeam(t *testing.T) *readSeam {
	t.Helper()
	api := newSeamAPI(t, "budget-seam-canary-77b1")
	hq := hqtest.New(map[string]string{offKTok: offK})
	t.Cleanup(hq.Close)
	store := &officerAgents{}
	prov := &officerProv{store: store, phase: map[string]provision.Phase{}}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
	s.UseExtensions(Extensions{Agents: store, Provisioner: prov, Gateway: &repromptChat{}, SessionLiveness: stubLiveness{}, Kinds: stubKinds{pick: "acct-one"},
		Connectors: &kubeConnStub{status: connectors.StatusApproved}})
	raw := `[{"id":"` + offK + `","hq_url":"` + hq.BaseURL() + `","tiers":{"routine":{"model":"haiku"},"judgement":{"model":"opus"}},` +
		`"kube":{"clusters":[{"name":"prod-a","service_account":"officers/officer-anomaly-prod-a"}]}}]`
	list, err := officers.Parse(raw, func(k string) string {
		return map[string]string{
			officers.TokenEnvPrefix + officers.TokenEnvSuffix(offK): offKTok,
			officers.KubeEndpointEnvPrefix + "PROD_A":               api.srv.Listener.Addr().String(),
			officers.KubeCAEnvPrefix + "PROD_A":                     api.caPEM,
			officers.KubeMinterEnvPrefix + "PROD_A":                 "fixture-minter-token-b7e2",
			"MUSTER_KUBE_READ_ALLOWLIST_PROD_A":                     kubeRigAllowlist,
		}[k]
	}, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnableOfficers(OfficersConfig{Officers: list, InstallSession: discardSession, NoShipResume: true}); err != nil {
		t.Fatal(err)
	}
	judge := agents.Agent{ID: 2981, Name: offKJudge + "1", Kind: agents.KindClaudeCode, Status: agents.StatusRunning, OwnerID: 1, Namespace: "ns"}
	routine := agents.Agent{ID: 2982, Name: offKRout + "1", Kind: agents.KindClaudeCode, Status: agents.StatusRunning, OwnerID: 1, Namespace: "ns"}
	store.mu.Lock()
	store.rows = append(store.rows, judge, routine)
	store.mu.Unlock()
	return &readSeam{s: s, hq: hq, api: api, judge: judge, routine: routine}
}

const kubeArgs = `{"cluster":"prod-a","verb":"list","group":"","version":"v1","resource":"configmaps","namespace":"web"}`

// TestTheThreeReadToolsShareOneBudgetPerItem: 100 named queries, 99 raw
// queries and 1 cluster read of ONE item are its 200 reads, all served; the
// 201st is refused whichever tool makes it — each refusal naming the budget,
// reaching neither HQ's obs routes nor the cluster. Another item the
// instance holds still has its own 200, and item_id never reaches HQ's
// named-query route.
func TestTheThreeReadToolsShareOneBudgetPerItem(t *testing.T) {
	rs := newReadSeam(t)
	ctx := context.Background()
	item := rs.hq.AddTieredItem(offK, "anomaly.alert", "node-a", officers.TierJudgement)
	rs.hq.SetClaim(item, rs.judge.Name, time.Now().Add(time.Hour))
	call := func(tool, args string) string { return rs.s.dispatchAgentTool(ctx, rs.judge, tool, args) }
	for i := 0; i < 100; i++ {
		if out := call(obsQueryTool, obsQueryArgs); isToolErr(out) {
			t.Fatalf("named query %d: %s", i+1, out)
		}
	}
	for i := 0; i < 99; i++ {
		if out := call(obsRawTool, rawArgs); isToolErr(out) {
			t.Fatalf("raw query %d: %s", i+1, out)
		}
	}
	if out := call(officers.ToolKubeRead, kubeArgs); isToolErr(out) {
		t.Fatalf("the cluster read (the 200th read): %s", out)
	}
	_, reads := rs.api.counts()
	if len(rs.hq.ObsCalls) != 100 || len(rs.hq.ObsRawCalls) != 99 || reads != 1 {
		t.Fatalf("served %d named, %d raw, %d cluster reads; want 100, 99, 1", len(rs.hq.ObsCalls), len(rs.hq.ObsRawCalls), reads)
	}
	for _, c := range []struct{ tool, args string }{{officers.ToolKubeRead, kubeArgs}, {obsRawTool, rawArgs}, {obsQueryTool, obsQueryArgs}} {
		out := call(c.tool, c.args)
		if !isToolErr(out) || !strings.Contains(out, "all 200 reads of item "+item) {
			t.Errorf("the 201st read, by %s: %s, want the budget's refusal", c.tool, out)
		}
	}
	if _, reads := rs.api.counts(); len(rs.hq.ObsCalls) != 100 || len(rs.hq.ObsRawCalls) != 99 || reads != 1 {
		t.Fatalf("a refused read was served: %d named, %d raw, %d cluster", len(rs.hq.ObsCalls), len(rs.hq.ObsRawCalls), reads)
	}

	second := rs.hq.AddTieredItem(offK, "anomaly.alert", "node-b", officers.TierJudgement)
	rs.hq.SetClaim(second, rs.judge.Name, time.Now().Add(time.Hour))
	if out := call(obsRawTool, `{"item_id":"`+second+`",`+rawArgs[1:]); isToolErr(out) {
		t.Fatalf("a raw read of a second held item: %s", out)
	}
	if got := rs.hq.ObsRawCalls[len(rs.hq.ObsRawCalls)-1].Body["item_id"]; got != float64(numItem(t, second)) {
		t.Fatalf("the second item's raw read was sent for item %v", got)
	}
	if out := call(obsQueryTool, `{"item_id":"`+second+`",`+obsQueryArgs[1:]); isToolErr(out) {
		t.Fatalf("a named query of a second held item: %s", out)
	}
	if _, sent := rs.hq.ObsCalls[len(rs.hq.ObsCalls)-1].Body["item_id"]; sent {
		t.Fatal("item_id reached HQ's named-query route")
	}
	if out := call(obsQueryTool, obsQueryArgs); !isToolErr(out) || !strings.Contains(out, "item_id") {
		t.Fatalf("a named query holding two items, naming none: %s, want a refusal asking for item_id", out)
	}
}

// TestANamedQueryIsCountedOnlyForAJudgementItem: a routine instance's named
// queries, and a judgement instance's while it holds no item, spend no
// budget — 201 of each are all served.
func TestANamedQueryIsCountedOnlyForAJudgementItem(t *testing.T) {
	rs := newReadSeam(t)
	ctx := context.Background()
	routineItem := rs.hq.AddTieredItem(offK, "anomaly.alert", "node-r", officers.TierRoutine)
	rs.hq.SetClaim(routineItem, rs.routine.Name, time.Now().Add(time.Hour))
	for _, a := range []agents.Agent{rs.routine, rs.judge} {
		for i := 0; i < officers.ReadsPerItem+1; i++ {
			if out := rs.s.dispatchAgentTool(ctx, a, obsQueryTool, obsQueryArgs); isToolErr(out) {
				t.Fatalf("%s's named query %d: %s", a.Name, i+1, out)
			}
		}
	}
	if n := len(rs.hq.ObsCalls); n != 2*(officers.ReadsPerItem+1) {
		t.Fatalf("%d named queries served, want %d", n, 2*(officers.ReadsPerItem+1))
	}
}

// TestOnlyAnAnswerHQJudgedSpendsARead (audit Y3): each HQ read tool's call
// that HQ answers 429, 503 (an outage, and the route's own 503
// obs_unconfigured) leaves the item's budget where it was; one HQ answers 200
// or 403 with a code other than not_investigating spends exactly one (a 403
// not_investigating spends none: TestANotInvestigatingRefusalSpendsNoRead). A call muster refuses before HQ (a key it owns)
// spends nothing either — that case is an INVARIANT guard (it held before
// the fix too). Red before the fix: both tools spent before calling HQ and
// never gave the unit back, so the 429 and 503 cases read spent 1, want 0.
func TestOnlyAnAnswerHQJudgedSpendsARead(t *testing.T) {
	rs := newReadSeam(t)
	ctx := context.Background()
	item := rs.hq.AddTieredItem(offK, "anomaly.alert", "node-a", officers.TierJudgement)
	rs.hq.SetClaim(item, rs.judge.Name, time.Now().Add(time.Hour))
	spent := func() int { return rs.s.officerRT.reads.Spent(offK, officers.ItemID(item)) }
	cases := []struct {
		name  string
		set   func()
		spend int
	}{
		{"429", func() { rs.hq.ObsStatus = 429 }, 0},
		{"503", func() { rs.hq.ObsStatus = 503 }, 0},
		{"503 obs_unconfigured", func() { rs.hq.ObsOff = true }, 0},
		{"200", func() {}, 1},
		{"403 forced (a code other than not_investigating)", func() { rs.hq.ObsStatus = 403 }, 1},
	}
	for _, tool := range []struct{ name, args string }{{obsRawTool, rawArgs}, {obsQueryTool, obsQueryArgs}} {
		for _, c := range cases {
			rs.hq.ObsStatus, rs.hq.ObsOff = 0, false
			c.set()
			before := spent()
			out := rs.s.dispatchAgentTool(ctx, rs.judge, tool.name, tool.args)
			if served := !isToolErr(out); served != (c.name == "200") {
				t.Fatalf("%s answered %s: %s (the fake did not answer as set)", tool.name, c.name, out)
			}
			if got := spent() - before; got != c.spend {
				t.Errorf("%s answered %s: spent %d, want %d", tool.name, c.name, got, c.spend)
			}
		}
	}
	rs.hq.ObsStatus, rs.hq.ObsOff = 0, false
	before := spent()
	if out := rs.s.dispatchAgentTool(ctx, rs.judge, obsRawTool, `{"Instance":"x",`+rawArgs[1:]); !isToolErr(out) || !strings.Contains(out, "is muster's to send") {
		t.Fatalf("%s naming its instance: %s, want muster's refusal", obsRawTool, out)
	}
	if got := spent() - before; got != 0 {
		t.Errorf("a call muster refused before HQ spent %d", got)
	}
}

// TestTheObsRawToolSchemaIsOpen (audit Y1): officer_obs_raw's definition is
// not a second copy of HQ's request schema — an open object (as
// officer_obs_query's), no property carrying a bound, and neither the
// description nor a property stating one of HQ's figures.
func TestTheObsRawToolSchemaIsOpen(t *testing.T) {
	var def map[string]any
	var desc string
	for _, d := range officerToolDefs() {
		if d.Name == obsRawTool {
			b, _ := json.Marshal(d.Parameters)
			_ = json.Unmarshal(b, &def)
			desc = d.Description
		}
	}
	if def == nil {
		t.Fatal("no officer_obs_raw definition")
	}
	if open, _ := def["additionalProperties"].(bool); !open {
		t.Fatalf("officer_obs_raw's schema is closed: %v", def)
	}
	props, _ := def["properties"].(map[string]any)
	for _, k := range []string{"backend", "query", "start", "end", "step", "limit", "item_id"} {
		if props[k] == nil {
			t.Errorf("the schema no longer describes %s", k)
		}
	}
	b, _ := json.Marshal(def)
	for _, bound := range []string{"minimum", "maximum", "maxLength", "enum"} {
		if strings.Contains(string(b), `"`+bound+`"`) {
			t.Errorf("the schema carries %q, a limit HQ owns: %s", bound, b)
		}
	}
	for _, figure := range []string{"4096", "7 days", "24 hours", "1000", "default 100", "at least 1m"} {
		if strings.Contains(desc+string(b), figure) {
			t.Errorf("the definition states HQ's figure %q", figure)
		}
	}
	if !strings.Contains(desc, "refusal names the limit") {
		t.Error("the description does not say HQ's refusal names the limit")
	}
}

// TestObsRawIsOfferedToARoutineInstanceAndCounted (operator decision
// "Give routine obs_raw"): a routine instance is offered
// officer_obs_raw (its session pre-allows it) and served it for the routine
// item it holds, each call one read of that item's shared budget. Today's HQ
// refuses a routine item's raw read (HQ's raw-read holder check: 403
// not_investigating) and that refusal reaches the instance with HQ's code;
// an HQ that admits it gets the call with the item and instance added, and
// the 201st read of the item is refused by the budget before HQ.
func TestObsRawIsOfferedToARoutineInstanceAndCounted(t *testing.T) {
	rs := newReadSeam(t)
	ctx := context.Background()
	if !officerToolNameSet(rs.s.MCPToolDefsFor(ctx, rs.routine))[obsRawTool] {
		t.Fatalf("the routine instance is not offered %s", obsRawTool)
	}
	sess, _ := rs.s.OfficerClaudeCodeSession(rs.routine)
	if !strings.Contains(","+strings.Join(sess.AllowMCPTools, ",")+",", ","+obsRawTool+",") {
		t.Fatalf("the routine instance's session does not pre-allow %s: %v", obsRawTool, sess.AllowMCPTools)
	}
	item := rs.hq.AddTieredItem(offK, "anomaly.alert", "node-r", officers.TierRoutine)
	rs.hq.SetClaim(item, rs.routine.Name, time.Now().Add(time.Hour))
	spent := func() int { return rs.s.officerRT.reads.Spent(offK, officers.ItemID(item)) }
	call := func() string { return rs.s.dispatchAgentTool(ctx, rs.routine, obsRawTool, rawArgs) }

	// today's HQ: a routine item's raw read is refused, with HQ's code, and
	// NOT counted (audit F1, officers.ReadCounted: a 403 not_investigating
	// read nothing)
	if out := call(); !isToolErr(out) || !strings.Contains(out, `"not_investigating"`) || !strings.Contains(out, "403") {
		t.Fatalf("today's HQ: %s, want HQ's 403 not_investigating", out)
	}
	if got := spent(); got != 0 {
		t.Fatalf("HQ's 403 not_investigating spent %d reads, want 0", got)
	}

	rs.hq.ObsRawAdmitsRoutine = true
	rs.hq.ResetRequests()
	if out := call(); isToolErr(out) || len(rs.hq.ObsRawAnswers) != 1 || out != string(rs.hq.ObsRawAnswers[0]) {
		t.Fatalf("an HQ that admits it: %s, want HQ's answer byte for byte", out)
	}
	var want map[string]any
	_ = json.Unmarshal([]byte(rawArgs), &want)
	want["item_id"] = float64(numItem(t, item))
	want["instance"] = rs.routine.Name
	wb, _ := json.Marshal(want)
	gb, _ := json.Marshal(rs.hq.ObsRawCalls[0].Body)
	if string(gb) != string(wb) {
		t.Fatalf("HQ received %s, want %s", gb, wb)
	}
	for i := spent(); i < officers.ReadsPerItem; i++ {
		if out := call(); isToolErr(out) {
			t.Fatalf("routine raw read %d: %s", i+1, out)
		}
	}
	if got := spent(); got != 200 {
		t.Fatalf("spent %d, want 200", got)
	}
	served := len(rs.hq.ObsRawCalls)
	if out := call(); !isToolErr(out) || !strings.Contains(out, "all 200 reads of item "+item) {
		t.Fatalf("the 201st read: %s, want the budget's refusal", out)
	}
	if len(rs.hq.ObsRawCalls) != served {
		t.Fatal("the refused 201st read reached HQ")
	}
}

// TestANotInvestigatingRefusalSpendsNoRead (audit F1): officer_obs_raw
// answered 403 not_investigating leaves the item's budget where it was, for a
// routine instance and a judgement one alike; a 403 with another code
// (the fake's "forced") still spends one. Red before the fix: the
// not_investigating case spent 1.
func TestANotInvestigatingRefusalSpendsNoRead(t *testing.T) {
	rs := newReadSeam(t)
	ctx := context.Background()
	for _, c := range []struct {
		a    agents.Agent
		tier string
		node string
	}{{rs.routine, officers.TierRoutine, "node-r"}, {rs.judge, officers.TierJudgement, "node-j"}} {
		item := rs.hq.AddTieredItem(offK, "anomaly.alert", c.node, c.tier)
		rs.hq.SetClaim(item, c.a.Name, time.Now().Add(time.Hour))
		spent := func() int { return rs.s.officerRT.reads.Spent(offK, officers.ItemID(item)) }

		rs.hq.ObsStatus, rs.hq.ObsRawNotInvestigating = 0, true
		for i := 0; i < 3; i++ {
			if out := rs.s.dispatchAgentTool(ctx, c.a, obsRawTool, rawArgs); !isToolErr(out) || !strings.Contains(out, `"not_investigating"`) {
				t.Fatalf("%s: %s, want HQ's 403 not_investigating", c.a.Name, out)
			}
		}
		if got := spent(); got != 0 {
			t.Errorf("%s: three 403 not_investigating answers spent %d, want 0", c.a.Name, got)
		}

		rs.hq.ObsStatus, rs.hq.ObsRawNotInvestigating = 403, false
		for i := 0; i < 2; i++ {
			if out := rs.s.dispatchAgentTool(ctx, c.a, obsRawTool, rawArgs); !isToolErr(out) || !strings.Contains(out, `"forced"`) || !strings.Contains(out, "403") {
				t.Fatalf("%s: %s, want HQ's 403 forced", c.a.Name, out)
			}
		}
		if got := spent(); got != 2 {
			t.Errorf("%s: two 403 forced answers spent %d, want 2", c.a.Name, got)
		}
		rs.hq.ObsStatus = 0
	}
}

// TestRoutineReadsCarryOverToTheJudgementInvestigation (audit F1): the budget
// is the item's, so the reads a routine instance spent on an item count
// toward the same 200 after it hands the item on with needs_judgement (HQ
// promotes it in place: same id). A routine instance's 1 refused (403
// not_investigating) and 7 served raw reads, then needs_judgement: the
// judgement instance that takes the item is served exactly 193 reads and its
// 194th is refused by the budget. Red before the fix: the refused read was
// counted too, so only 192 were served.
func TestRoutineReadsCarryOverToTheJudgementInvestigation(t *testing.T) {
	rs := newReadSeam(t)
	ctx := context.Background()
	item := rs.hq.AddTieredItem(offK, "anomaly.alert", "node-c", officers.TierRoutine)
	rs.hq.SetClaim(item, rs.routine.Name, time.Now().Add(time.Hour))
	routineCall := func() string { return rs.s.dispatchAgentTool(ctx, rs.routine, obsRawTool, rawArgs) }

	if out := routineCall(); !isToolErr(out) || !strings.Contains(out, `"not_investigating"`) {
		t.Fatalf("today's HQ: %s, want HQ's 403 not_investigating", out)
	}
	rs.hq.ObsRawAdmitsRoutine = true
	for i := 0; i < 7; i++ {
		if out := routineCall(); isToolErr(out) {
			t.Fatalf("routine raw read %d: %s", i+1, out)
		}
	}
	if got := rs.s.officerRT.reads.Spent(offK, officers.ItemID(item)); got != 7 {
		t.Fatalf("the routine instance's reads spent %d, want 7", got)
	}
	out := rs.s.dispatchAgentTool(ctx, rs.routine, completeTool, `{"item_id":"`+item+`","outcome":"needs_judgement","summary":"needs a judgement pass"}`)
	if isToolErr(out) {
		t.Fatalf("needs_judgement: %s", out)
	}
	rs.hq.SetClaim(item, rs.judge.Name, time.Now().Add(time.Hour))

	before := len(rs.hq.ObsRawCalls)
	served := 0
	for i := 0; i < officers.ReadsPerItem; i++ {
		out := rs.s.dispatchAgentTool(ctx, rs.judge, obsRawTool, rawArgs)
		if isToolErr(out) {
			if !strings.Contains(out, "all 200 reads of item "+item) {
				t.Fatalf("judgement read %d: %s, want served or the budget's refusal", i+1, out)
			}
			break
		}
		served++
	}
	if served != 193 || len(rs.hq.ObsRawCalls)-before != 193 {
		t.Fatalf("the judgement instance was served %d reads (%d reached HQ), want 193 (200 - the routine instance's 7)", served, len(rs.hq.ObsRawCalls)-before)
	}
	if got := rs.s.officerRT.reads.Spent(offK, officers.ItemID(item)); got != 200 {
		t.Fatalf("spent %d, want 200", got)
	}
}

func numItem(t *testing.T, id string) int64 {
	t.Helper()
	var n int64
	if err := json.Unmarshal([]byte(id), &n); err != nil {
		t.Fatalf("item id %q is not numeric: %v", id, err)
	}
	return n
}
