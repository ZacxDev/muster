package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers/hqtest"
)

// ---------------------------------------------------------------------------
// THE OBS TOOLS (officer_obs_queries, officer_obs_query): an officer reads
// HQ's NAMED obs queries through HQ's officer obs route, with its own officer
// token. The tool names are spelled as literals here on purpose: they are the
// contract the charters name, so renaming one must fail a test.
// ---------------------------------------------------------------------------

const (
	obsListTool  = "officer_obs_queries"
	obsQueryTool = "officer_obs_query"
)

// TestTheObsToolsAreOfferedToEveryOfficerOnly: both officers' instances (a
// release officer and a triage one) are offered both obs tools over MCP and in
// their session allowlist; the ordinary agent and the chief are offered
// neither, and calling one anyway is refused without reaching HQ.
func TestTheObsToolsAreOfferedToEveryOfficerOnly(t *testing.T) {
	r := newOfficerRig(t)
	ctx := context.Background()
	for _, inst := range []struct {
		o  string
		id int64
	}{{offA, 861}, {offB, 862}} {
		o := r.a
		if inst.o == offB {
			o = r.b
		}
		a := r.addInstance(o, 1, inst.id)
		got := officerToolNameSet(r.s.MCPToolDefsFor(ctx, a))
		sess, ok := r.s.OfficerClaudeCodeSession(a)
		if !ok {
			t.Fatalf("%s: no officer session", inst.o)
		}
		for _, n := range []string{obsListTool, obsQueryTool} {
			if !got[n] {
				t.Errorf("officer %s's instance is not offered %s over MCP", inst.o, n)
			}
			allowed := false
			for _, m := range sess.AllowMCPTools {
				allowed = allowed || m == n
			}
			if !allowed {
				t.Errorf("officer %s's session does not allow %s", inst.o, n)
			}
		}
	}
	for _, id := range []int64{officerOrdinaryID, officerChiefID} {
		a := r.agent(t, id)
		if got := officerToolNameSet(r.s.MCPToolDefsFor(ctx, a)); got[obsListTool] || got[obsQueryTool] {
			t.Errorf("agent %d is offered an obs tool", id)
		}
		r.hq.ResetRequests()
		out := r.s.dispatchAgentTool(ctx, a, obsQueryTool, `{"query_id":"fixture_error_lines","start":"2000-01-01T00:00:00Z","end":"2000-01-01T01:00:00Z"}`)
		if !isToolErr(out) {
			t.Errorf("agent %d calling %s: %s, want a refusal", id, obsQueryTool, out)
		}
		if n := len(r.hq.Requests()); n != 0 {
			t.Errorf("agent %d's refused obs call reached HQ (%d requests)", id, n)
		}
	}
}

// TestObsQueriesListsHQsRegistryWithTheOfficersOwnToken: the list tool is
// GET /officers/{id}/obs/queries for the CALLER's officer, with that
// officer's token, and HQ's registry comes back as is.
func TestObsQueriesListsHQsRegistryWithTheOfficersOwnToken(t *testing.T) {
	r := newOfficerRig(t)
	ctx := context.Background()
	for _, c := range []struct {
		inst      int64
		officer   string
		tok       string
		isOfficer string
	}{{871, offA, offATok, "a"}, {872, offB, offBTok, "b"}} {
		o := r.a
		if c.isOfficer == "b" {
			o = r.b
		}
		a := r.addInstance(o, 1, c.inst)
		r.hq.ResetRequests()
		out := r.s.dispatchAgentTool(ctx, a, obsListTool, `{}`)
		if isToolErr(out) {
			t.Fatalf("%s listing obs queries: %s", c.officer, out)
		}
		var got struct {
			Queries []struct {
				ID   string   `json:"id"`
				Args []string `json:"args"`
			} `json:"queries"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil || len(got.Queries) != 2 ||
			got.Queries[0].ID != "fixture_calls_by_procedure" || got.Queries[1].ID != "fixture_error_lines" {
			t.Fatalf("%s: list answer = %s (err %v), want the fake's two-entry registry", c.officer, out, err)
		}
		reqs := r.hq.Requests()
		want := "/api/v1/officers/" + c.officer + "/obs/queries"
		if len(reqs) != 1 || reqs[0].Method != "GET" || reqs[0].Path != want || reqs[0].Auth != "Bearer "+c.tok {
			t.Fatalf("%s: HQ saw %+v, want one GET %s with its own token", c.officer, reqs, want)
		}
	}
}

// TestObsQuerySendsTheNamedQueryAndOnlyItsArguments: the query tool posts
// {query_id, start, end, <the entry's declared arguments>} to
// POST /officers/{id}/obs/query with the officer's own token — exactly the
// fields given, nothing added (no instance, no officer) — and HQ's answer
// (empty flag and hint included) comes back as is.
func TestObsQuerySendsTheNamedQueryAndOnlyItsArguments(t *testing.T) {
	r := newOfficerRig(t)
	ctx := context.Background()
	inst := r.addInstance(r.b, 1, 881)
	out := r.s.dispatchAgentTool(ctx, inst, obsQueryTool,
		`{"query_id":"fixture_calls_by_procedure","start":"2000-01-01T00:00:00Z","end":"2000-01-01T06:00:00Z","step":"300s","procedures":["a.list","b.get"]}`)
	if isToolErr(out) {
		t.Fatalf("obs query: %s", out)
	}
	if len(r.hq.ObsCalls) != 1 {
		t.Fatalf("HQ ran %d obs queries, want 1", len(r.hq.ObsCalls))
	}
	call := r.hq.ObsCalls[0]
	body, _ := json.Marshal(call.Body)
	want := `{"end":"2000-01-01T06:00:00Z","procedures":["a.list","b.get"],"query_id":"fixture_calls_by_procedure","start":"2000-01-01T00:00:00Z","step":"300s"}`
	if call.Officer != offB || string(body) != want {
		t.Fatalf("obs query body =\n  %s\nwant\n  %s (officer %s)", body, want, call.Officer)
	}
	reqs := r.hq.Requests()
	last := reqs[len(reqs)-1]
	if last.Method != "POST" || last.Path != "/api/v1/officers/"+offB+"/obs/query" || last.Auth != "Bearer "+offBTok {
		t.Fatalf("obs query went %s %s with %q", last.Method, last.Path, last.Auth)
	}
	var ans struct {
		AuditID int    `json:"audit_id"`
		QueryID string `json:"query_id"`
		Empty   bool   `json:"empty"`
		Series  []struct {
			Key   string  `json:"key"`
			Total float64 `json:"total"`
		} `json:"series"`
	}
	if err := json.Unmarshal([]byte(out), &ans); err != nil || ans.AuditID != 1 || ans.QueryID != "fixture_calls_by_procedure" ||
		ans.Empty || len(ans.Series) != 1 || ans.Series[0].Total != 42 {
		t.Fatalf("answer = %s (err %v), want HQ's answer passed through", out, err)
	}

	// A lines entry with its limit; and HQ's empty answer carries its hint.
	r.hq.ObsEmpty = true
	out = r.s.dispatchAgentTool(ctx, inst, obsQueryTool, `{"query_id":"fixture_error_lines","start":"2000-01-01T00:00:00Z","end":"2000-01-01T01:00:00Z","limit":20}`)
	if isToolErr(out) || !strings.Contains(out, `"empty":true`) || !strings.Contains(out, "positive control") {
		t.Fatalf("empty answer: %s, want HQ's empty answer with its hint", out)
	}
	if body, _ := json.Marshal(r.hq.ObsCalls[1].Body); string(body) != `{"end":"2000-01-01T01:00:00Z","limit":20,"query_id":"fixture_error_lines","start":"2000-01-01T00:00:00Z"}` {
		t.Fatalf("lines query body = %s", body)
	}
}

// obsRefusal is a tool answer's HQ status and code ("refused": {status, hq:
// {code}}), zero when it is not an HQ refusal.
func obsRefusal(out string) (int, string) {
	var got struct {
		Refused struct {
			Status int `json:"status"`
			HQ     struct {
				Code string `json:"code"`
			} `json:"hq"`
		} `json:"refused"`
	}
	_ = json.Unmarshal([]byte(out), &got)
	return got.Refused.Status, got.Refused.HQ.Code
}

// obsWin is a valid window, RFC 3339 in whole seconds.
const obsWin = `"start":"2000-01-01T00:00:00Z","end":"2000-01-01T01:00:00Z"`

// TestHQsDecoderIsTheOneSchema: muster holds no copy of HQ's request schema.
// The tool's arguments object goes to HQ unchanged — a raw PromQL/LogQL key
// included, not dropped — and HQ's refusal reaches the officer with HQ's
// status and code: an unknown field (400 invalid), an unknown or missing
// query id (400 unknown_query), a missing or malformed window (400 invalid),
// a field of the wrong type (400 invalid), an argument the query does not take
// or one it needs and lacks (400 invalid). Each reaches HQ exactly once, and
// HQ runs none of them.
func TestHQsDecoderIsTheOneSchema(t *testing.T) {
	r := newOfficerRig(t)
	ctx := context.Background()
	inst := r.addInstance(r.a, 1, 891)
	for _, c := range []struct {
		name, args, code string
		sentKey          string // a key the request HQ saw must still carry
	}{
		{"raw promql", `{"query_id":"fixture_calls_by_procedure","promql":"sum(rate(x[5m]))",` + obsWin + `,"step":"300s"}`, "invalid", "promql"},
		{"raw logql", `{"query_id":"fixture_error_lines","logql":"{app=\"x\"}",` + obsWin + `}`, "invalid", "logql"},
		{"a bare query", `{"query":"up",` + obsWin + `}`, "invalid", "query"},
		{"an instance", `{"query_id":"fixture_error_lines","instance":"spoofed",` + obsWin + `}`, "invalid", "instance"},
		{"limit of the wrong type", `{"query_id":"fixture_error_lines",` + obsWin + `,"limit":"twenty"}`, "invalid", "limit"},
		{"procedures not an array", `{"query_id":"fixture_calls_by_procedure",` + obsWin + `,"step":"300s","procedures":"a.list"}`, "invalid", "procedures"},
		{"no query_id", `{` + obsWin + `}`, "unknown_query", "start"},
		{"an unknown query_id", `{"query_id":"../brief",` + obsWin + `}`, "unknown_query", "query_id"},
		{"no start", `{"query_id":"fixture_error_lines","end":"2000-01-01T01:00:00Z"}`, "invalid", "end"},
		{"no end", `{"query_id":"fixture_error_lines","start":"2000-01-01T00:00:00Z"}`, "invalid", "start"},
		{"a fractional second", `{"query_id":"fixture_error_lines","start":"2000-01-01T00:00:00.5Z","end":"2000-01-01T01:00:00Z"}`, "invalid", "start"},
		{"an argument the query does not take", `{"query_id":"fixture_error_lines",` + obsWin + `,"step":"300s"}`, "invalid", "step"},
		{"an argument the query needs", `{"query_id":"fixture_calls_by_procedure",` + obsWin + `}`, "invalid", "query_id"},
	} {
		r.hq.ResetRequests()
		out := r.s.dispatchAgentTool(ctx, inst, obsQueryTool, c.args)
		if st, code := obsRefusal(out); !isToolErr(out) || st != 400 || code != c.code {
			t.Errorf("%s: %s, want HQ's refusal 400 %s", c.name, out, c.code)
		}
		reqs := r.hq.Requests()
		if len(reqs) != 1 || reqs[0].Path != "/api/v1/officers/"+offA+"/obs/query" || reqs[0].Auth != "Bearer "+offATok {
			t.Errorf("%s: HQ saw %d requests (%+v), want the one POST with the officer's own token", c.name, len(reqs), reqs)
			continue
		}
		if _, ok := reqs[0].Body[c.sentKey]; !ok {
			t.Errorf("%s: the request HQ saw lacks %q (muster changed the arguments): %v", c.name, c.sentKey, reqs[0].Body)
		}
	}
	if n := len(r.hq.ObsCalls); n != 0 {
		t.Fatalf("HQ ran %d of the refused queries", n)
	}
}

// TestObsQueryRefusesLocallyOnlyWhatMusterOwns: what muster itself owns is
// refused before HQ — arguments that are not a JSON object, a body over HQ's
// officer-route cap (256 KiB), and naming another officer. Positive control:
// a well-formed call reaches HQ once and runs.
func TestObsQueryRefusesLocallyOnlyWhatMusterOwns(t *testing.T) {
	r := newOfficerRig(t)
	ctx := context.Background()
	inst := r.addInstance(r.a, 1, 893)
	r.hq.ResetRequests()
	for name, args := range map[string]string{
		"an array":        `[{"query_id":"fixture_error_lines"}]`,
		"a string":        `"fixture_error_lines"`,
		"null":            `null`,
		"over the cap":    `{"query_id":"fixture_error_lines",` + obsWin + `,"path":"` + strings.Repeat("x", 256<<10) + `"}`,
		"another officer": `{"query_id":"fixture_error_lines","officer":"` + offB + `",` + obsWin + `}`,
	} {
		if out := r.s.dispatchAgentTool(ctx, inst, obsQueryTool, args); !isToolErr(out) {
			t.Errorf("%s: %s, want a local refusal", name, out)
		}
	}
	if n := len(r.hq.Requests()); n != 0 {
		t.Fatalf("calls muster must refuse reached HQ (%d requests)", n)
	}
	if out := r.s.dispatchAgentTool(ctx, inst, obsQueryTool, `{"query_id":"fixture_error_lines",`+obsWin+`}`); isToolErr(out) {
		t.Fatalf("well-formed control: %s", out)
	}
	if n := len(r.hq.ObsCalls); n != 1 {
		t.Fatalf("the well-formed control ran %d queries, want 1", n)
	}
}

// TestObsQueryPassesHQsRefusalsBack: HQ's 5xx DECISIONS reach the instance
// as final refusals with HQ's status and code — 503 obs_unconfigured (the
// route switched off; not an outage, which an instance would retry forever)
// and 502 wrong_shape (HQ's own defect) — while 502 upstream (the backend
// read failed) is RETRYABLE, as HQ's guide says: not a final refusal, and it
// names the code and says retry.
func TestObsQueryPassesHQsRefusalsBack(t *testing.T) {
	r := newOfficerRig(t)
	ctx := context.Background()
	inst := r.addInstance(r.a, 1, 895)
	ok := `{"query_id":"fixture_error_lines",` + obsWin + `}`
	for _, c := range []struct {
		name, tool, args string
		off              bool
		fail             hqtest.ObsFail
		status           int
		code             string
	}{
		{"route off (query)", obsQueryTool, ok, true, hqtest.ObsFail{}, 503, "obs_unconfigured"},
		{"route off (list)", obsListTool, `{}`, true, hqtest.ObsFail{}, 503, "obs_unconfigured"},
		{"wrong shape", obsQueryTool, ok, false, hqtest.ObsFail{Status: 502, Code: "wrong_shape"}, 502, "wrong_shape"},
	} {
		r.hq.ObsOff, r.hq.ObsFail = c.off, c.fail
		out := r.s.dispatchAgentTool(ctx, inst, c.tool, c.args)
		if st, code := obsRefusal(out); !isToolErr(out) || st != c.status || code != c.code {
			t.Errorf("%s: %s, want a final refusal with status %d and HQ code %q", c.name, out, c.status, c.code)
		}
		if strings.Contains(out, "unavailable") || strings.Contains(out, "retry") {
			t.Errorf("%s: %s reads as retryable", c.name, out)
		}
	}
	r.hq.ObsOff, r.hq.ObsFail = false, hqtest.ObsFail{Status: 502, Code: "upstream"}
	out := r.s.dispatchAgentTool(ctx, inst, obsQueryTool, ok)
	var got struct {
		Error     string `json:"error"`
		Retryable *struct {
			Status int    `json:"status"`
			Code   string `json:"code"`
		} `json:"retryable"`
	}
	_ = json.Unmarshal([]byte(out), &got)
	if !isToolErr(out) || got.Retryable == nil || got.Retryable.Status != 502 || got.Retryable.Code != "upstream" || !strings.Contains(got.Error, "retry") {
		t.Errorf("upstream: %s, want a retryable answer naming 502 upstream", out)
	}
	if st, _ := obsRefusal(out); st != 0 {
		t.Errorf("upstream: %s is a final refusal; HQ's guide says retry it", out)
	}
	if n := len(r.hq.ObsCalls); n != 0 {
		t.Fatalf("the fake ran %d failed queries", n)
	}
}
