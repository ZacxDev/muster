package api

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ccpool"
	"github.com/ZacxDev/muster/internal/plugins"
)

// dispatch_request's worker_pool and cc_account, and agents_list
// (dispatch_hold_tools.go, agents_list_tool.go), over a real Postgres and the
// real POST /mirror/mcp door.

// accountKinds is stubKinds with an account pool of names: a pin outside it is
// refused as ccpool.Pool.Select refuses one (ErrUnknownAccount, naming the
// caller's accounts); no pin is the pool's own choice, pick. names is read
// through a pointer so a test can take an account away mid-flight.
type accountKinds struct {
	stubKinds
	names *[]string
}

func (k accountKinds) SelectClaudeAccount(ctx context.Context, owner int64, pin string) (string, error) {
	if pin != "" && !slices.Contains(*k.names, pin) {
		return "", fmt.Errorf("%w %q (yours: %s)", ccpool.ErrUnknownAccount, pin, strings.Join(*k.names, ", "))
	}
	return k.stubKinds.SelectClaudeAccount(ctx, owner, pin)
}

// newPoolHoldFixture is newHoldFixture with the owner's Claude accounts
// acct-auto (the pool's own choice) and acct-pinned.
func newPoolHoldFixture(t *testing.T) (*holdFixture, *[]string) {
	t.Helper()
	names := &[]string{"acct-auto", "acct-pinned"}
	h := newHoldFixture(t, func(ext *Extensions) {
		ext.Kinds = accountKinds{stubKinds: stubKinds{pick: "acct-auto"}, names: names}
	})
	return h, names
}

// TestDispatchRequestHoldsAPoolWorkerAndApproveStartsItAsHeld: a claude-code
// request with worker_pool and cc_account and NO prompt is held — the answer
// and the hold's audit row name the pool and the account — and starts
// nothing. Its approval starts exactly that: a claude-code agent in the pool,
// PINNED to the held account (not the pool's own choice, acct-auto), with no
// first turn (Start, not a kickoff Dispatch), and the answer and the approval
// row name the pool and the account.
func TestDispatchRequestHoldsAPoolWorkerAndApproveStartsItAsHeld(t *testing.T) {
	h, _ := newPoolHoldFixture(t)
	held := h.request(t, map[string]any{"target": "pod/claude-code", "worker_pool": "render-pool", "cc_account": "acct-pinned"})
	if !strings.Contains(held.Summary, "as a machine-job worker in pool render-pool") ||
		!strings.Contains(held.Summary, "on Claude account acct-pinned") || strings.Contains(held.Summary, "told:") {
		t.Errorf("summary %q: want the pool and the account, and no prompt", held.Summary)
	}
	h.nothingStarted(t)
	d, _ := h.auditDetail(t, plugins.ActionDispatchHeld, held.HoldID)
	if d["worker_pool"] != "render-pool" || d["cc_account"] != "acct-pinned" || d["kind"] != agents.KindClaudeCode {
		t.Errorf("hold row %v: want worker_pool render-pool, cc_account acct-pinned, kind claude-code", d)
	}

	text, isErr, rpc := h.tool(t, h.ws, focusSID, dispatchApproveTool, map[string]any{"hold_id": held.HoldID})
	if rpc != nil || isErr {
		t.Fatalf("dispatch_approve: %v %s", rpc, text)
	}
	var out struct {
		AgentID    int64  `json:"agent_id"`
		WorkerPool string `json:"worker_pool"`
		CCAccount  string `json:"cc_account"`
		Next       string `json:"next"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.WorkerPool != "render-pool" || out.CCAccount != "acct-pinned" ||
		!strings.Contains(out.Next, "serves machine jobs in pool render-pool and opens no pull request") {
		t.Fatalf("approval answered %s", text)
	}
	list := h.ownersAgents(t)
	if len(list) != 1 || list[0].ID != out.AgentID {
		t.Fatalf("agents %+v, want exactly agent %d", list, out.AgentID)
	}
	a := list[0]
	if a.Kind != agents.KindClaudeCode || a.WorkerPool != "render-pool" || a.CCAccount != "acct-pinned" || !a.CCAccountPinned ||
		a.NoteID != nil || a.PendingNote != "" {
		t.Errorf("agent %+v: want a claude-code worker in render-pool pinned to acct-pinned, with no task and no first turn", a)
	}
	if got := h.prov.startedIDs(1); len(got) != 1 || got[0] != a.ID {
		t.Errorf("provisioner Starts %v, want exactly agent %d (a pool worker with no prompt claims no first turn)", got, a.ID)
	}
	if n := h.prov.calls(0); n != 0 {
		t.Errorf("%d kickoff dispatch(es), want none", n)
	}
	ad, _ := h.auditDetail(t, plugins.ActionDispatchApproved, held.HoldID)
	if ad["worker_pool"] != "render-pool" || ad["cc_account"] != "acct-pinned" {
		t.Errorf("approval row %v: want the pool and the account", ad)
	}
}

// TestAPoolWorkerWithAPromptIsKickedOffAndAutoChoosesItsAccount: with a
// prompt the pool worker gets it as its first turn (a kickoff Dispatch), and
// with cc_account omitted it runs on the pool's own choice, unpinned.
func TestAPoolWorkerWithAPromptIsKickedOffAndAutoChoosesItsAccount(t *testing.T) {
	h, _ := newPoolHoldFixture(t)
	held := h.request(t, map[string]any{"target": "pod/claude-code", "worker_pool": "render-pool", "prompt": "Warm the cache first."})
	d, _ := h.auditDetail(t, plugins.ActionDispatchHeld, held.HoldID)
	if _, named := d["cc_account"]; named || d["worker_pool"] != "render-pool" {
		t.Errorf("hold row %v: want the pool and no account (muster's choice)", d)
	}
	text, isErr, _ := h.tool(t, h.ws, focusSID, dispatchApproveTool, map[string]any{"hold_id": held.HoldID})
	if isErr {
		t.Fatalf("approval: %s", text)
	}
	list := h.ownersAgents(t)
	if len(list) != 1 {
		t.Fatalf("agents %+v, want one", list)
	}
	a := list[0]
	if a.WorkerPool != "render-pool" || a.CCAccount != "acct-auto" || a.CCAccountPinned ||
		!strings.Contains(a.PendingNote, "| Warm the cache first.") {
		t.Errorf("agent %+v: want a render-pool worker on the pool's choice acct-auto, unpinned, told the prompt", a)
	}
	if n := h.prov.calls(1); n != 1 || !h.prov.kickoffs[0] {
		t.Errorf("provisioner dispatches %d (kickoffs %v), want one with a kickoff", n, h.prov.kickoffs)
	}
}

// TestDispatchRequestRefusesABadPoolOrAccount: each is refused AT THE REQUEST
// with POST /api/agents' own sentence, and holds and starts nothing — a pool
// or an account on pod/gateway, a pool name the create refuses, an account the
// owner does not have, a pool worker on a task.
func TestDispatchRequestRefusesABadPoolOrAccount(t *testing.T) {
	h, _ := newPoolHoldFixture(t)
	task := h.task(t, "pool on a task")
	gw := func(over map[string]any) map[string]any {
		m := gatewayRequest()
		for k, v := range over {
			m[k] = v
		}
		return m
	}
	cc := func(over map[string]any) map[string]any {
		m := map[string]any{"target": "pod/claude-code", "prompt": "serve"}
		for k, v := range over {
			m[k] = v
		}
		return m
	}
	for _, c := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"worker_pool on pod/gateway", gw(map[string]any{"worker_pool": "render-pool"}),
			`worker_pool "render-pool": only a claude-code agent can be a machine-job worker`},
		{"worker_pool on pod/gateway, no prompt", gw(map[string]any{"worker_pool": "render-pool", "prompt": ""}),
			`worker_pool "render-pool": only a claude-code agent can be a machine-job worker`},
		{"cc_account on pod/gateway", gw(map[string]any{"cc_account": "acct-pinned"}),
			"a Claude account can only be pinned for a claude-code agent"},
		{"a pool name the create refuses", cc(map[string]any{"worker_pool": "Render_Pool"}),
			`worker_pool "Render_Pool" is not 1-63 of [a-z0-9-], not starting with '-'`},
		{"a pool name starting with '-'", cc(map[string]any{"worker_pool": "-render"}),
			`worker_pool "-render" is not 1-63 of [a-z0-9-], not starting with '-'`},
		{"an account not in the owner's pool", cc(map[string]any{"cc_account": "acct-elsewhere"}),
			`unknown Claude account "acct-elsewhere" (yours: acct-auto, acct-pinned)`},
		{"an account not in the pool, on a pool worker", cc(map[string]any{"worker_pool": "render-pool", "cc_account": "acct-elsewhere"}),
			`unknown Claude account "acct-elsewhere"`},
		{"a pool worker on a task", cc(map[string]any{"worker_pool": "render-pool", "task_id": task.ID}), agentDispatchPoolNoTask},
	} {
		t.Run(c.name, func(t *testing.T) {
			text, isErr, rpc := h.tool(t, h.ws, focusSID, dispatchRequestTool, c.args)
			if rpc != nil || !isErr || !strings.Contains(errorText(t, text), c.want) {
				t.Fatalf("answer %v %s\nwant a refusal carrying %q", rpc, text, c.want)
			}
		})
	}
	if n := h.auditRows(t, plugins.ActionDispatchHeld); n != 0 {
		t.Errorf("%d hold rows after refusals, want none", n)
	}
	h.nothingStarted(t)
	// Positive control: the same fixture holds a valid pool request, so the
	// refusals above are the pool and account rules, not a broken door.
	h.request(t, cc(map[string]any{"worker_pool": "render-pool", "cc_account": "acct-pinned"}))
}

// TestAnAccountGoneBeforeTheApprovalRefusesItAndKeepsTheHold: the account is
// checked again at the approval, by the same rule, BEFORE the reservation —
// refused with the hold's "can no longer start as it was held", nothing
// started or reserved, and the hold still open; with the account back, the
// same hold starts the agent on it.
func TestAnAccountGoneBeforeTheApprovalRefusesItAndKeepsTheHold(t *testing.T) {
	h, names := newPoolHoldFixture(t)
	held := h.request(t, map[string]any{"target": "pod/claude-code", "worker_pool": "render-pool", "cc_account": "acct-pinned"})
	*names = []string{"acct-auto"}
	text, isErr, _ := h.tool(t, h.ws, focusSID, dispatchApproveTool, map[string]any{"hold_id": held.HoldID})
	if want := "hold " + held.HoldID + " can no longer start as it was held"; !isErr || !strings.Contains(errorText(t, text), want) ||
		!strings.Contains(errorText(t, text), `unknown Claude account "acct-pinned"`) {
		t.Fatalf("approval with the account gone answered %s", text)
	}
	h.nothingStarted(t)
	if hold, err := h.plugins.DispatchHold(h.asOwner, held.HoldID); err != nil || !hold.Open() {
		t.Fatalf("the hold after the refusal: %+v %v; want still open", hold, err)
	}
	*names = []string{"acct-auto", "acct-pinned"}
	text, isErr, _ = h.tool(t, h.ws, focusSID, dispatchApproveTool, map[string]any{"hold_id": held.HoldID})
	if isErr {
		t.Fatalf("approval with the account back: %s", text)
	}
	if list := h.ownersAgents(t); len(list) != 1 || list[0].CCAccount != "acct-pinned" || list[0].WorkerPool != "render-pool" {
		t.Fatalf("agents %+v, want one render-pool worker on acct-pinned", list)
	}
}

// --- agents_list ---------------------------------------------------------------

type agentsListOut struct {
	Agents     []agentsListItem `json:"agents"`
	Count      int              `json:"count"`
	WorkerPool string           `json:"worker_pool"`
}

func (h *holdFixture) agentsList(t *testing.T, token, sid string, args map[string]any) agentsListOut {
	t.Helper()
	text, isErr, rpc := h.tool(t, token, sid, agentsListTool, args)
	if rpc != nil || isErr {
		t.Fatalf("agents_list %v: %v %s", args, rpc, text)
	}
	var out agentsListOut
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("agents_list answered %s", text)
	}
	return out
}

func listedNames(out agentsListOut) []string {
	var n []string
	for _, a := range out.Agents {
		n = append(n, a.Name)
	}
	slices.Sort(n)
	return n
}

// TestAgentsListIsTheOwnersAgentsOnly: agents_list answers the caller's
// owner's agents and no one else's — with each one's kind, worker pool and
// Claude account — and worker_pool filters to that pool. Another owner's
// session sees only theirs. It is listed on the mirror door, never offered to
// an agent on /mcp (unknown there), and refuses a non-mirrored caller.
func TestAgentsListIsTheOwnersAgentsOnly(t *testing.T) {
	h, _ := newPoolHoldFixture(t)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	mk := func(owner int64, name, pool, account string) agents.Agent {
		t.Helper()
		a, err := agents.NewPG(h.pool).Create(h.as(owner), agents.Agent{Name: name + "-" + suffix, Namespace: "devpod-" + name + "-" + suffix,
			Status: agents.StatusRunning, Kind: agents.KindClaudeCode, CCAccount: account, WorkerPool: pool})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	w1 := mk(h.owner, "rw1", "render-pool", "acct-pinned")
	w2 := mk(h.owner, "rw2", "render-pool", "acct-auto")
	solo := mk(h.owner, "solo", "", "acct-auto")
	theirs := mk(h.other, "theirs", "render-pool", "acct-auto")

	all := h.agentsList(t, h.ws, focusSID, nil)
	want := []string{h.chief.Name, h.worker.Name, solo.Name, w1.Name, w2.Name}
	slices.Sort(want)
	if got := listedNames(all); !slices.Equal(got, want) || all.Count != len(want) {
		t.Fatalf("agents_list names %v (count %d); want exactly the owner's %v", got, all.Count, want)
	}
	for _, a := range all.Agents {
		if a.Name == w1.Name && (a.ID != w1.ID || a.Kind != agents.KindClaudeCode || a.WorkerPool != "render-pool" ||
			a.CCAccount != "acct-pinned" || a.Status != agents.StatusRunning || a.CreatedAt.IsZero()) {
			t.Errorf("w1 listed as %+v; want its id, kind, pool, account, status and created time", a)
		}
	}
	pool := h.agentsList(t, h.ws, focusSID, map[string]any{"worker_pool": "render-pool"})
	wantPool := []string{w1.Name, w2.Name}
	slices.Sort(wantPool)
	if got := listedNames(pool); !slices.Equal(got, wantPool) || pool.WorkerPool != "render-pool" {
		t.Errorf("agents_list in render-pool: %v %q; want %v (not the other owner's %s)", got, pool.WorkerPool, wantPool, theirs.Name)
	}
	if none := h.agentsList(t, h.ws, focusSID, map[string]any{"worker_pool": "no-such-pool"}); none.Count != 0 || len(none.Agents) != 0 {
		t.Errorf("an empty pool listed %+v", none)
	}
	if got := listedNames(h.agentsList(t, h.theirWS, focusSID, nil)); !slices.Equal(got, []string{theirs.Name}) {
		t.Errorf("the other owner's session listed %v; want only %s", got, theirs.Name)
	}
	if text, isErr, _ := h.tool(t, h.ws, focusSID, agentsListTool, map[string]any{"pool": "x"}); !isErr || errorText(t, text) != agentsListArgs {
		t.Errorf("an unknown argument answered %s", text)
	}

	if !h.mirrorListed(t, h.ws)[agentsListTool] {
		t.Error("the mirror door does not list agents_list")
	}
	for _, tok := range []string{h.chiefTok, h.workerTok} {
		if h.listedFor(t, tok)[agentsListTool] {
			t.Errorf("/mcp offers agents_list to an agent")
		}
		if _, _, rpc := h.mcpCall(t, tok, agentsListTool, map[string]any{}); rpc == nil || rpc.Code != -32602 {
			t.Errorf("agents_list over /mcp by an agent: %+v; want unknown tool (-32602)", rpc)
		}
	}
	if got := errorText(t, h.srv.toolAgentsList(h.mirrorCtx(), callerOfAgent(h.worker), `{}`)); got != agentsListMirrorOnly {
		t.Errorf("agents_list by a dispatched agent: %q", got)
	}
}
