package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/podagent"
	"github.com/ZacxDev/muster/internal/provision"
)

// WHERE A STALL CHECK RUNS IN THE CHIEF'S POD (chief_nudge.go nudgeRoute).
//
// These run the sweep against the REAL agentgateway.Gateway — its real request
// builder, which is what puts a turn's route on the wire — with only the chief
// pod's endpoint faked. The fixture's nudgeGateway replaces Gateway.Chat whole,
// so a route set on the context would never reach a request there.

// nudgeCCD stands in for the chief pod's one endpoint (ccd's port, or a
// gateway runtime's): it records each turn's session key and lane headers, and
// answers the chief's reply — or, while full, a lane turn gets ccd's typed
// lanes-full refusal, written as ccd writes it (podagent.WriteFailure,
// cmd/ccd/lanes.go acquireIn).
type nudgeCCD struct {
	mu    sync.Mutex
	full  bool
	// notReady: a lane turn gets ccd's typed 503 not_ready instead (a ccd with
	// CCD_MAX_SESSIONS=0, server.laneFor).
	notReady bool
	reply    string
	turns    []routedTurn
}

func (c *nudgeCCD) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.turns = append(c.turns, routedTurn{key: r.Header.Get("X-Openclaw-Session-Key"),
		ccSession: r.Header.Get("X-Muster-CC-Session"), ccModel: r.Header.Get("X-Muster-CC-Model"),
		ccEphemeral: r.Header.Get("X-Muster-CC-Ephemeral"), ccToolless: r.Header.Get("X-Muster-CC-Toolless")})
	full, notReady, reply := c.full, c.notReady, c.reply
	c.mu.Unlock()
	if notReady && r.Header.Get("X-Muster-CC-Session") != "" {
		podagent.WriteFailure(w, &podagent.Failure{Status: http.StatusServiceUnavailable, Type: "not_ready",
			Message: "this ccd runs one Claude Code session only (it does not supervise the CLI, or CCD_MAX_SESSIONS is 0)"})
		return
	}
	// ccd refuses for capacity only a turn that needs a lane (laneFor); the
	// primary's turns are never refused for it.
	if full && r.Header.Get("X-Muster-CC-Session") != "" {
		podagent.WriteFailure(w, &podagent.Failure{Status: http.StatusServiceUnavailable, Type: "capacity",
			Message: "every Claude Code session ccd may run at once is mid-turn (3; CCD_MAX_SESSIONS) — retry when one finishes"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\","+
		"\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":%q}]}]}}\n\n", reply)
}

func (c *nudgeCCD) seen() []routedTurn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]routedTurn(nil), c.turns...)
}

func (c *nudgeCCD) setFull(full bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.full = full
}

func (c *nudgeCCD) setNotReady(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notReady = v
}

// newLaneNudgeFixture is newNudgeFixture (shadow) with the real gateway in
// front of a nudgeCCD, and the chief of kind kind.
func newLaneNudgeFixture(t *testing.T, kind string) (*nudgeFixture, *nudgeCCD) {
	t.Helper()
	f := newNudgeFixture(t, false)
	if kind == agents.KindClaudeCode {
		// An agent's kind is fixed at creation: the fixture's chief is replaced
		// by a claude-code one (which carries a Claude account).
		sys := authz.System(context.Background())
		if _, err := f.pool.Exec(sys, `DELETE FROM agents WHERE id = $1`, f.boss.ID); err != nil {
			t.Fatal(err)
		}
		name := "nudgecc-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		a, err := agents.NewPG(f.pool).Create(asUser(authz.OperatorUserID), agents.Agent{Name: name, Namespace: "devpod-" + name,
			DisplayName: "chief", Status: agents.StatusRunning, HooksToken: "nudge-cc-tok-" + name,
			Kind: agents.KindClaudeCode, CCAccount: "acct-nudge"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = f.pool.Exec(sys, `DELETE FROM agents WHERE id = $1`, a.ID) })
		f.boss = a
	}
	ccd := &nudgeCCD{reply: nudgeReply(nudgeOwnerID, "keep going until merged", "merge the pull request")}
	up := httptest.NewServer(ccd)
	t.Cleanup(up.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(up.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	gw, err := agentgateway.New(agentgateway.Config{Driver: fixedEndpoint{provision.Endpoint{Host: host, Port: port}},
		Runtime: agentgateway.HooksSHA256(), Model: "runtime-sentinel", Client: up.Client()})
	if err != nil {
		t.Fatal(err)
	}
	f.srv.ext.Gateway = gw
	return f, ccd
}

// TestChiefNudgeOnAClaudeCodeChiefRunsInALaneOfItsOwn: a claude-code chief's
// stall check carries its per-decision key as the explicit session ccd routes
// by (X-Muster-CC-Session), so ccd runs it in a CLI of its own with an empty
// conversation — never in the primary CLI, the operator's conversation with
// the chief, which is where a turn without that header goes. No model header:
// the lane starts on the pod's default model, as the primary does. And it is
// marked a throwaway check (X-Muster-CC-Ephemeral: 1), so ccd stops its CLI
// once its turn is over and never stops an operator's session to make room —
// and TOOL-LESS (X-Muster-CC-Toolless: 1), so ccd starts that CLI with no
// built-in tool and no MCP server: a transcript quoted in the check's prompt
// cannot steer the chief into calling one.
func TestChiefNudgeOnAClaudeCodeChiefRunsInALaneOfItsOwn(t *testing.T) {
	f, ccd := newLaneNudgeFixture(t, agents.KindClaudeCode)
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d, want 1", n)
	}
	if d := f.one(t); d.Outcome != chiefnudge.OutcomeWouldSend {
		t.Fatalf("outcome %q, want %q (the chief's answer came back through the lane)", d.Outcome, chiefnudge.OutcomeWouldSend)
	}
	turns := ccd.seen()
	if len(turns) != 1 {
		t.Fatalf("turns %v, want 1", turns)
	}
	got := turns[0]
	if !strings.HasPrefix(got.key, "agent:"+f.boss.Name+":webchat:nudge:") {
		t.Fatalf("session key %q is not the check's own", got.key)
	}
	if got.ccSession != got.key {
		t.Fatalf("X-Muster-CC-Session %q, want the check's key %q: without it ccd runs the check in the chief's primary CLI", got.ccSession, got.key)
	}
	if got.ccModel != "" {
		t.Fatalf("X-Muster-CC-Model %q, want none", got.ccModel)
	}
	if got.ccEphemeral != "1" {
		t.Fatalf("X-Muster-CC-Ephemeral %q, want \"1\": without it ccd keeps the check's CLI and may stop an operator's session for it", got.ccEphemeral)
	}
	if got.ccToolless != "1" {
		t.Fatalf("X-Muster-CC-Toolless %q, want \"1\": without it ccd starts the check's CLI with the chief's full tool set", got.ccToolless)
	}
}

// TestChiefNudgeLanesFullIsRetriedNotRecorded: ccd's typed 503 `capacity` —
// no slot for the check's CLI, answered before anything is pasted — records
// NO decision (a recorded chief_error would close the streak for good); the
// next pass asks again, under the same decision's key, and records it.
func TestChiefNudgeLanesFullIsRetriedNotRecorded(t *testing.T) {
	f, ccd := newLaneNudgeFixture(t, agents.KindClaudeCode)
	ccd.setFull(true)
	if n := f.pass(t); n != 0 {
		t.Fatalf("decided %d with ccd full, want 0", n)
	}
	if list := f.decisions(t); len(list) != 0 {
		t.Fatalf("decisions with ccd full: %+v, want none (the streak must be retried)", list)
	}
	ccd.setFull(false)
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d on the retry, want 1", n)
	}
	if d := f.one(t); d.Outcome != chiefnudge.OutcomeWouldSend {
		t.Fatalf("retry outcome %q, want %q", d.Outcome, chiefnudge.OutcomeWouldSend)
	}
	turns := ccd.seen()
	if len(turns) != 2 || turns[0].ccSession != turns[1].ccSession || turns[1].ccSession == "" {
		t.Fatalf("turns %v, want two under one decision's lane", turns)
	}
}

// TestChiefNudgeOnAGatewayChiefCarriesNoLaneHeader: a gateway chief has no
// lanes; its per-decision session key is a session of its own already, and its
// request carries no claude-code route header (exactly as before lanes). An
// INVARIANT guard: the bug never sent the header to a gateway chief; this pins
// that nudgeRoute keeps it so.
func TestChiefNudgeOnAGatewayChiefCarriesNoLaneHeader(t *testing.T) {
	f, ccd := newLaneNudgeFixture(t, agents.KindGateway)
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d, want 1", n)
	}
	turns := ccd.seen()
	if len(turns) != 1 {
		t.Fatalf("turns %v, want 1", turns)
	}
	if got := turns[0]; !strings.HasPrefix(got.key, "agent:"+f.boss.Name+":webchat:nudge:") || got.ccSession != "" || got.ccModel != "" || got.ccEphemeral != "" || got.ccToolless != "" {
		t.Fatalf("gateway chief's check: %v, want its own session key and no claude-code route", got)
	}
}
