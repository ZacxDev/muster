package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
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

// TestObsQueryRefusesARawQueryLocally: the tool takes a registry id and typed
// arguments only. A raw PromQL/LogQL field under any name, an instance or
// officer muster did not set, a missing query_id or window, or a malformed id
// is refused BEFORE HQ — no request leaves muster.
func TestObsQueryRefusesARawQueryLocally(t *testing.T) {
	r := newOfficerRig(t)
	ctx := context.Background()
	inst := r.addInstance(r.a, 1, 891)
	const win = `"start":"2000-01-01T00:00:00Z","end":"2000-01-01T01:00:00Z"`
	r.hq.ResetRequests()
	for name, args := range map[string]string{
		"raw query":            `{"query":"up",` + win + `}`,
		"raw promql":           `{"query_id":"fixture_calls_by_procedure","promql":"sum(rate(x[5m]))",` + win + `,"step":"300s"}`,
		"raw logql":            `{"query_id":"fixture_error_lines","logql":"{app=\"x\"}",` + win + `}`,
		"expr":                 `{"query_id":"fixture_error_lines","expr":"up",` + win + `}`,
		"instance from agent":  `{"query_id":"fixture_error_lines","instance":"spoofed",` + win + `}`,
		"another officer":      `{"query_id":"fixture_error_lines","officer":"` + offB + `",` + win + `}`,
		"no query_id":          `{` + win + `}`,
		"query_id with a path": `{"query_id":"../brief",` + win + `}`,
		"query_id with spaces": `{"query_id":"sum by (x) (up)",` + win + `}`,
		"no start":             `{"query_id":"fixture_error_lines","end":"2000-01-01T01:00:00Z"}`,
		"no end":               `{"query_id":"fixture_error_lines","start":"2000-01-01T00:00:00Z"}`,
		"procedures not array": `{"query_id":"fixture_calls_by_procedure",` + win + `,"step":"300s","procedures":"a.list"}`,
	} {
		if out := r.s.dispatchAgentTool(ctx, inst, obsQueryTool, args); !isToolErr(out) {
			t.Errorf("%s: %s, want a local refusal", name, out)
		}
	}
	if n := len(r.hq.Requests()); n != 0 {
		t.Fatalf("malformed obs calls reached HQ (%d requests)", n)
	}
	// Positive control: the same rig, a well-formed call, reaches HQ once — so
	// the zero above is the refusals, not a tool that never sends anything.
	if out := r.s.dispatchAgentTool(ctx, inst, obsQueryTool, `{"query_id":"fixture_error_lines",`+win+`}`); isToolErr(out) {
		t.Fatalf("well-formed control: %s", out)
	}
	if n := len(r.hq.ObsCalls); n != 1 {
		t.Fatalf("the well-formed control ran %d queries, want 1", n)
	}
}

// TestObsQueryPassesHQsRefusalsBack: HQ's decisions reach the instance as a
// refusal carrying HQ's status and code — an unknown query id, an argument the
// entry does not take, and the route switched off. 🔴 The switched-off route
// is HQ's 503 obs_unconfigured: a DECISION, so it must not read as "HQ is
// unavailable, try again", which an instance would retry forever.
func TestObsQueryPassesHQsRefusalsBack(t *testing.T) {
	r := newOfficerRig(t)
	ctx := context.Background()
	inst := r.addInstance(r.a, 1, 895)
	const win = `"start":"2000-01-01T00:00:00Z","end":"2000-01-01T01:00:00Z"`
	refusal := func(out string) (int, string) {
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
	for _, c := range []struct {
		name, tool, args string
		off              bool
		status           int
		code             string
	}{
		{"unknown query", obsQueryTool, `{"query_id":"no_such_query",` + win + `}`, false, 400, "unknown_query"},
		{"undeclared argument", obsQueryTool, `{"query_id":"fixture_error_lines",` + win + `,"step":"300s"}`, false, 400, "invalid"},
		{"route off (query)", obsQueryTool, `{"query_id":"fixture_error_lines",` + win + `}`, true, 503, "obs_unconfigured"},
		{"route off (list)", obsListTool, `{}`, true, 503, "obs_unconfigured"},
	} {
		r.hq.ObsOff = c.off
		out := r.s.dispatchAgentTool(ctx, inst, c.tool, c.args)
		if st, code := refusal(out); !isToolErr(out) || st != c.status || code != c.code {
			t.Errorf("%s: %s, want a refusal with status %d and HQ code %q", c.name, out, c.status, c.code)
		}
		if strings.Contains(out, "unavailable") {
			t.Errorf("%s: %s reads as an outage", c.name, out)
		}
	}
	if n := len(r.hq.ObsCalls); n != 0 {
		t.Fatalf("the fake ran %d refused queries", n)
	}
}
