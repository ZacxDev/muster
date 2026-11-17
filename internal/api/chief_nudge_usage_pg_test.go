package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/chiefusage"
	"github.com/ZacxDev/muster/internal/provision"
)

// A STALL CHECK IS A CHIEF CALL, AND ITS TOKENS ARE RECORDED (chief_nudge.go
// askChiefForNudge; docs/usage.md, trigger nudge_check). The check is a fresh,
// tool-less, throwaway call that never passes chatTurn, so before it recorded
// itself it was invisible to chief_turn_usage. These run the sweep through the
// REAL agentgateway against a fake runtime that reports usage, so the tokens
// travel the production path (agents.RunToollessTurn's sink) into the row.

// nudgeCheckRuntime is the chief's /v1/responses for stall checks: each call
// answers the next scripted status and body, and is counted.
type nudgeCheckRuntime struct {
	mu     sync.Mutex
	status int // 0: 200 with body
	body   string
	calls  int
}

func (r *nudgeCheckRuntime) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	_, _ = io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if req.URL.Path != "/v1/responses" || !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") {
		http.Error(w, "refused", http.StatusUnauthorized)
		return
	}
	if r.status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(r.status)
		_, _ = io.WriteString(w, r.body)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, r.body)
}

// nudgeCheckBody is a completed response whose text is reply, reporting usage
// (raw JSON).
func nudgeCheckBody(reply, usage string) string {
	text, _ := json.Marshal(reply)
	return "event: response.completed\ndata: " + `{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":` +
		string(text) + `}]}],"usage":` + usage + `}}` + "\n\n"
}

// nudgeCheckModel is the chief's model for these tests: a value no default
// produces, so the row's model can only have come from the chief.
const nudgeCheckModel = "claude-nudge-check-sentinel-7"

// withRealCheckGateway points the fixture's gateway at rt through the real
// agentgateway, gives chief nudgeCheckModel, and records chief turns.
func withRealCheckGateway(t *testing.T, f *nudgeFixture, rt *nudgeCheckRuntime, chief agents.Agent) {
	t.Helper()
	sys := authz.System(context.Background())
	if _, err := f.pool.Exec(sys, `UPDATE agents SET model = $2 WHERE id = $1`, chief.ID, nudgeCheckModel); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM chief_turn_usage WHERE chief_agent = $1`, chief.ID)
	})
	up := httptest.NewServer(rt)
	t.Cleanup(up.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(up.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	gw, err := agentgateway.New(agentgateway.Config{Driver: fixedEndpoint{provision.Endpoint{Host: host, Port: port}},
		Runtime: agentgateway.HooksSHA256(), Model: "runtime-sentinel", Secrets: agents.NewPG(f.pool), Client: up.Client()})
	if err != nil {
		t.Fatal(err)
	}
	f.srv.ext.Gateway = gw
	f.srv.ext.ChiefUsage = chiefusage.NewPG(f.pool)
}

type nudgeCheckRow struct {
	owner                  int64
	trigger, outcome       string
	known, noSession       bool
	in, out, read, create  int64
	model                  string
	durationNonNegative    bool
	pluginEmpty, senderNil bool
}

func nudgeCheckRows(t *testing.T, f *nudgeFixture, chief int64) []nudgeCheckRow {
	t.Helper()
	rows, err := f.pool.Query(authz.System(context.Background()), `SELECT owner_id, trigger, outcome, usage_known, session_id IS NULL,
		input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, model, duration_ms >= 0,
		plugin_name = '', plugin_message_id IS NULL
		FROM chief_turn_usage WHERE chief_agent = $1 ORDER BY id`, chief)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []nudgeCheckRow
	for rows.Next() {
		var r nudgeCheckRow
		if err := rows.Scan(&r.owner, &r.trigger, &r.outcome, &r.known, &r.noSession, &r.in, &r.out, &r.read, &r.create, &r.model,
			&r.durationNonNegative, &r.pluginEmpty, &r.senderNil); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestAStallCheckWritesExactlyOneUsageRowTaggedNudgeCheck: one sweep, one
// stalled session, one check — and exactly one chief_turn_usage row, trigger
// nudge_check, carrying the runtime's own tokens (pairwise distinct literals),
// the chief's model, no chat session (the check's is throwaway), under the
// SWEPT owner. That owner is the fixture's member, NOT the operator (user 1),
// so a row filed under the operator whatever the sweep's owner is cannot
// pass. A second pass that calls nothing (the streak is decided) adds none.
func TestAStallCheckWritesExactlyOneUsageRowTaggedNudgeCheck(t *testing.T) {
	f := newNudgeFixture(t, false)
	// The operator has nothing stalled: no mirrored window, no transcript.
	f.srv.nudgeReadHook = nil
	f.mirror.windows = nil
	if f.member == authz.OperatorUserID {
		t.Fatal("instrument check: the fixture's member is the operator")
	}
	_, quoteID := f.stallAgent(t, f.worker, f.member)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM chief_nudges WHERE owner_id = $1`, f.member)
	})
	rt := &nudgeCheckRuntime{body: nudgeCheckBody(nudgeReply(strconv.FormatInt(quoteID, 10), "keep going until merged", "merge the pull request"),
		`{"input_tokens":21,"output_tokens":43,"cache_read_input_tokens":6500,"cache_creation_input_tokens":870}`)}
	withRealCheckGateway(t, f, rt, f.chief)

	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d, want 1", n)
	}
	// The check's reply went through the real gateway and was parsed: the
	// member's decision is the scripted nudge, so the call the row counts ran.
	var decision string
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT decision FROM chief_nudges WHERE owner_id = $1 AND agent_id = $2`,
		f.member, f.worker.ID).Scan(&decision); err != nil || decision != chiefnudge.DecisionNudge {
		t.Fatalf("the member's decision %q (%v), want a nudge", decision, err)
	}
	if got := nudgeCheckRows(t, f, f.boss.ID); len(got) != 0 {
		t.Fatalf("the operator's chief has usage rows: %+v", got)
	}
	want := nudgeCheckRow{owner: f.member, trigger: "nudge_check", outcome: "other", known: true, noSession: true,
		in: 21, out: 43, read: 6500, create: 870, model: nudgeCheckModel, durationNonNegative: true, pluginEmpty: true, senderNil: true}
	got := nudgeCheckRows(t, f, f.chief.ID)
	if len(got) != 1 || got[0] != want {
		t.Fatalf("chief_turn_usage rows for the chief = %+v\nwant exactly [%+v]", got, want)
	}
	if n := f.pass(t); n != 0 {
		t.Fatalf("a second pass decided %d", n)
	}
	if got := nudgeCheckRows(t, f, f.chief.ID); len(got) != 1 {
		t.Fatalf("a pass that put nothing to the chief wrote usage rows: %+v", got)
	}
	rt.mu.Lock()
	calls := rt.calls
	rt.mu.Unlock()
	if calls != 1 {
		t.Fatalf("the runtime saw %d calls, want 1 (one row per call)", calls)
	}
}

// TestARefusedStallCheckWritesNoUsageRow: a check the runtime refused before
// anything ran (ccd has no lane for its CLI: a typed 503 capacity) cost
// nothing and is retried, so it records no row — the same rule as a busy
// turn (chiefTurnUsage.finish). The runtime WAS reached (its call count
// moves), so the zero is not an instrument that never ran.
func TestARefusedStallCheckWritesNoUsageRow(t *testing.T) {
	f := newNudgeFixture(t, false)
	rt := &nudgeCheckRuntime{status: http.StatusServiceUnavailable,
		body: `{"error":{"type":"` + agents.FailureCapacity + `","message":"no lane for the check"}}`}
	withRealCheckGateway(t, f, rt, f.boss)

	if n := f.pass(t); n != 0 {
		t.Fatalf("decided %d, want 0 (a refused check is retried)", n)
	}
	rt.mu.Lock()
	calls := rt.calls
	rt.mu.Unlock()
	if calls < 1 {
		t.Fatalf("the runtime saw %d calls: the refusal was never exercised", calls)
	}
	if got := nudgeCheckRows(t, f, f.boss.ID); len(got) != 0 {
		t.Fatalf("a refused check wrote usage rows: %+v", got)
	}
}
