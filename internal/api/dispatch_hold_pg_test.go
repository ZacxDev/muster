package api

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
)

// dispatch_request + dispatch_approve (dispatch_hold_tools.go) over a real
// Postgres and the real POST /mirror/mcp door: the owner's mirrored session
// holds a start, then approves it; nothing else can do either.

// holdLaptopSID is a Claude session on the owner's SECOND host, "laptop".
const holdLaptopSID = "3e5f6071-8293-4a41-9dce-3f4a5b6c7d8e"

// ownersMirror is the fake hub serving its host "ws" (focusSID, secondSID)
// and a second host "laptop" (holdLaptopSID) to each user in users — so the
// fixture's owner and a second owner both have mirrored sessions.
type ownersMirror struct {
	*fakeMirror
	mu    sync.Mutex
	users map[int64]bool
}

func (m *ownersMirror) Hosts(ctx context.Context) []mirror.HostView {
	p, ok := authz.From(ctx)
	m.mu.Lock()
	allowed := ok && !p.All() && m.users[p.UserID]
	m.mu.Unlock()
	if !allowed {
		return nil
	}
	hosts := m.fakeMirror.Hosts(asUser(1))
	return append(hosts, mirror.HostView{Host: "laptop", Sessions: []string{"main"}, Online: true, Interactive: true,
		Windows: []mirror.Window{{Session: "main", ID: "@1", Index: 1, Active: true, Claude: true, ClaudeSession: holdLaptopSID,
			Status: mirror.StatusIdle, StatusSince: attnSince, Activity: 1_700_000_000}}})
}

// holdFixture is the task-routing fixture (dispatch on, the owner opted in,
// both pod kinds enabled) with the mirror door wired: the owner's tokens for
// hosts "ws" and "laptop", a second opted-in owner's token for "ws", and push.
type holdFixture struct {
	*taskRouteFixture
	hub                 *ownersMirror
	tokens              fakeMirrorTokens
	pusher              *payloadPusher
	ws, laptop, theirWS string
	other               int64
}

func newHoldFixture(t *testing.T, extra ...func(ext *Extensions)) *holdFixture {
	t.Helper()
	h := &holdFixture{tokens: fakeMirrorTokens{}, pusher: &payloadPusher{},
		hub: &ownersMirror{users: map[int64]bool{}, fakeMirror: &fakeMirror{online: true, interactive: true, statusReport: true, v2: &fakeMirrorV2{},
			windows: append(attnWindows(mirror.StatusIdle), mirror.Window{Session: "side", ID: "@3", Index: 0, Active: true, Claude: true,
				ClaudeSession: secondSID, Status: mirror.StatusBusy, StatusSince: attnSince, Activity: 1_700_000_000})}}}
	store := &memPush{}
	h.taskRouteFixture = newTaskRouteFixture(t, true, true, append([]func(ext *Extensions){withKinds("pool-acct-a"), func(ext *Extensions) {
		ext.Mirror, ext.MirrorTokens = h.hub, h.tokens
		ext.Push = push.NewService(store, h.pusher, push.VAPIDConfig{PublicKey: "k"}, nil)
	}}, extra...)...)
	claim(t, store, h.owner, ep("hold-owner"))
	h.other = h.newMember(t)
	h.tr.DispatchOwners[h.other] = true
	h.hub.users[h.owner], h.hub.users[h.other] = true, true
	h.ws, h.laptop, h.theirWS = mirrorTok('h'), mirrorTok('i'), mirrorTok('j')
	h.tokens[h.ws] = mirror.Token{ID: 11, OwnerID: h.owner, Host: "ws"}
	h.tokens[h.laptop] = mirror.Token{ID: 12, OwnerID: h.owner, Host: "laptop"}
	h.tokens[h.theirWS] = mirror.Token{ID: 13, OwnerID: h.other, Host: "ws"}
	return h
}

// tool calls name over POST /mirror/mcp as token's host and Claude session
// sid: the tool's text and whether it was a tool error, or the JSON-RPC error.
func (h *holdFixture) tool(t *testing.T, token, sid, name string, args any) (string, bool, *mcpError) {
	t.Helper()
	hdr := map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + token, mirrorSessionHeader: sid}
	w := h.connFixture.call(t, http.MethodPost, "/mirror/mcp", strings.NewReader(mcpToolCall(name, args)), hdr)
	if w.Code != http.StatusOK {
		t.Fatalf("/mirror/mcp %s: HTTP %d %s", name, w.Code, w.Body)
	}
	var r mcpReply
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("not JSON-RPC: %s", w.Body)
	}
	if r.Error != nil {
		return "", false, r.Error
	}
	text, isErr := toolText(t, &r)
	return text, isErr, nil
}

// mirrorListed is the mirror door's tools/list for token.
func (h *holdFixture) mirrorListed(t *testing.T, token string) map[string]bool {
	t.Helper()
	hdr := map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + token, mirrorSessionHeader: focusSID}
	w := h.connFixture.call(t, http.MethodPost, "/mirror/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), hdr)
	var r mcpReply
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("tools/list: %d %s", w.Code, w.Body)
	}
	return holdToolNames(t, r.Result)
}

func holdToolNames(t *testing.T, result json.RawMessage) map[string]bool {
	t.Helper()
	var res struct {
		Tools []struct{ Name string } `json:"tools"`
	}
	if err := json.Unmarshal(result, &res); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, tl := range res.Tools {
		out[tl.Name] = true
	}
	return out
}

// request is a dispatch_request by the owner's "ws" session focusSID that
// must be held: its answer.
func (h *holdFixture) request(t *testing.T, args map[string]any) holdAnswerOut {
	t.Helper()
	text, isErr, rpc := h.tool(t, h.ws, focusSID, dispatchRequestTool, args)
	if rpc != nil || isErr {
		t.Fatalf("dispatch_request %v: %v %s", args, rpc, text)
	}
	var out holdAnswerOut
	if err := json.Unmarshal([]byte(text), &out); err != nil || !plugins.ValidHoldID(out.HoldID) {
		t.Fatalf("dispatch_request answered %s", text)
	}
	return out
}

type holdAnswerOut struct {
	HoldID    string `json:"hold_id"`
	Summary   string `json:"summary"`
	ExpiresAt string `json:"expires_at"`
	Model     string `json:"model"`
	Next      string `json:"next"`
}

// heldRows / approvedRows count the owner's hold and approval audit rows.
func (h *holdFixture) auditRows(t *testing.T, action string) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(h.asOwner, `SELECT count(*) FROM audit_log WHERE action = $1 AND user_id = $2`, action, h.owner).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// auditDetail is the detail of the owner's audit row action on object.
func (h *holdFixture) auditDetail(t *testing.T, action, object string) (map[string]any, string) {
	t.Helper()
	var (
		raw []byte
		via string
	)
	if err := h.pool.QueryRow(h.asOwner, `SELECT detail, via FROM audit_log WHERE action = $1 AND object = $2 AND user_id = $3`,
		action, object, h.owner).Scan(&raw, &via); err != nil {
		t.Fatalf("audit row %s %s: %v", action, object, err)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, via
}

// nothingStarted asserts no reservation, no agent and no provisioner call.
func (h *holdFixture) nothingStarted(t *testing.T) {
	t.Helper()
	if n := h.allReservations(t); n != 0 {
		t.Errorf("%d reservation(s), want none", n)
	}
	if got := h.ownersAgents(t); len(got) != 0 {
		t.Errorf("agents %+v, want none", got)
	}
	if n := h.prov.calls(0); n != 0 {
		t.Errorf("%d provisioner dispatch(es), want none", n)
	}
}

func gatewayRequest() map[string]any {
	return map[string]any{"target": "pod/gateway", "model": "openrouter/vendor-a/coder-7", "prompt": "Profile the nightly export and open a draft PR."}
}

// --- the request ----------------------------------------------------------------

// TestDispatchRequestHoldsAndStartsNothing: a valid request files ONE hold —
// its audit row names the host, the session, the target, the model and the
// prompt's digest — answers {hold_id, summary, expires_at 15 minutes on}, and
// reserves, creates and provisions nothing. The same request again files a
// SECOND hold — there is no idempotency key: a duplicate hold starts nothing,
// lapses in 15 minutes, and each hold starts at most once.
func TestDispatchRequestHoldsAndStartsNothing(t *testing.T) {
	h := newHoldFixture(t)
	before := time.Now()
	out := h.request(t, gatewayRequest())
	if !strings.Contains(out.Summary, "pod/gateway") || !strings.Contains(out.Summary, "openrouter/vendor-a/coder-7") ||
		!strings.Contains(out.Summary, "Profile the nightly export") || out.Model != "openrouter/vendor-a/coder-7" {
		t.Errorf("answer %+v: want a summary naming the target, the model and the prompt", out)
	}
	exp, err := time.Parse(time.RFC3339, out.ExpiresAt)
	if err != nil || exp.Sub(before) < 14*time.Minute || exp.Sub(before) > 16*time.Minute {
		t.Errorf("expires_at %q: want 15 minutes on", out.ExpiresAt)
	}
	if !strings.Contains(out.Next, "dispatch_approve") || !strings.Contains(out.Next, out.HoldID) {
		t.Errorf("next %q: want it to name dispatch_approve and the hold", out.Next)
	}
	h.nothingStarted(t)
	if n := h.auditRows(t, plugins.ActionDispatchHeld); n != 1 {
		t.Fatalf("%d hold rows, want 1", n)
	}
	d, via := h.auditDetail(t, plugins.ActionDispatchHeld, out.HoldID)
	if via != string(authz.ViaMirrorHost) || d["host"] != "ws" || d["session"] != focusSID || d["target"] != "pod/gateway" ||
		d["model"] != "openrouter/vendor-a/coder-7" || d["prompt_sha256"] != promptDigest("Profile the nightly export and open a draft PR.") ||
		!strings.HasSuffix(d["session_label"].(string), "@ ws") {
		t.Errorf("hold row via=%s detail=%v: want the host, the session, the target, the model and the prompt digest", via, d)
	}

	again := h.request(t, gatewayRequest())
	if again.HoldID == out.HoldID {
		t.Errorf("the same request again answered the same hold %s, want a new one", again.HoldID)
	}
	if n := h.auditRows(t, plugins.ActionDispatchHeld); n != 2 {
		t.Errorf("%d hold rows after the second request, want 2", n)
	}
	h.nothingStarted(t)
}

// TestDispatchRequestValidatesLikeAgentDispatch: each guard refuses with its
// own sentence and holds nothing — the target (none, mirrored, a host, a
// malformed one), an argument outside the table, model on claude-code, a display name as
// model, a bare provider id, the prompt, a repository the owner did not
// connect, a gated task, another owner's task.
func TestDispatchRequestValidatesLikeAgentDispatch(t *testing.T) {
	h := newHoldFixture(t)
	gated := h.task(t, "gated work", "gate:needs-owner-ok")
	theirs := func() int64 {
		f2 := h.as(h.other)
		n, err := h.notes.Create(f2, notes.Note{Title: "their task", Body: "Theirs."})
		if err != nil {
			t.Fatal(err)
		}
		return n.ID
	}()
	base := func(over map[string]any) map[string]any {
		args := map[string]any{"target": "pod/claude-code", "prompt": "do the thing"}
		for k, v := range over {
			if v == nil {
				delete(args, k)
			} else {
				args[k] = v
			}
		}
		return args
	}
	for _, c := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"no target", base(map[string]any{"target": nil}), dispatchRequestNoTarget},
		{"mirrored", base(map[string]any{"target": "mirrored"}), dispatchRequestMirrored},
		{"a host target", base(map[string]any{"target": "host/box-a/claude"}), `host "box-a" cannot start agents now`},
		{"unknown target", base(map[string]any{"target": "pod/robot"}), `target "pod/robot" is not one of:`},
		{"a request_key (the request has none)", base(map[string]any{"request_key": "k-1"}), dispatchRequestArgs},
		{"model on claude-code", base(map[string]any{"model": "openrouter/vendor-a/coder-7"}), dispatchRequestModel},
		{"a display name as model", base(map[string]any{"target": "pod/gateway", "model": "Acme v4.1 flash"}),
			`model "Acme v4.1 flash" is not a model id (a display name is not one)`},
		{"a bare provider id", base(map[string]any{"target": "pod/gateway", "model": "vendor-a/coder-7"}), `Did you mean "openrouter/vendor-a/coder-7"?`},
		{"no prompt and no task", base(map[string]any{"prompt": nil}), dispatchRequestPrompt},
		{"a repo not connected", base(map[string]any{"repo": "example-org/example-repo"}), chiefDispatchNoGitHub},
		{"a gated task", base(map[string]any{"task_id": gated.ID}), "this task is gated (needs-owner-ok), so only you can release it"},
		{"another owner's task", base(map[string]any{"task_id": theirs}), "no such task"},
		{"an unknown argument", base(map[string]any{"owner_asked": true}), dispatchRequestArgs},
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
}

// TestDispatchRequestRefusesAnOwnerWhoDidNotOptIn: the opt-in's own sentence.
func TestDispatchRequestRefusesAnOwnerWhoDidNotOptIn(t *testing.T) {
	h := newHoldFixture(t)
	delete(h.tr.DispatchOwners, h.owner)
	text, isErr, _ := h.tool(t, h.ws, focusSID, dispatchRequestTool, gatewayRequest())
	if !isErr || errorText(t, text) != dispatchRequestOptOut {
		t.Fatalf("answer %s; want %q", text, dispatchRequestOptOut)
	}
	if n := h.auditRows(t, plugins.ActionDispatchHeld); n != 0 {
		t.Errorf("%d hold rows, want none", n)
	}
}

// --- the approval ---------------------------------------------------------------

// TestDispatchApproveStartsExactlyTheHeldRequestOnce: the approval starts one
// gateway agent with the held model and a first message that is the held
// prompt fenced as relayed from the owner's session, with muster's rules last;
// its reservation names the hold; an approval audit row names who approved and
// the agent; the owner is pushed once ("<session> dispatched <agent> (<target>,
// <model>)"). A second approval of the same hold is refused and starts and
// pushes nothing more.
func TestDispatchApproveStartsExactlyTheHeldRequestOnce(t *testing.T) {
	h := newHoldFixture(t)
	held := h.request(t, gatewayRequest())
	before := h.budget(t)
	text, isErr, rpc := h.tool(t, h.ws, focusSID, dispatchApproveTool, map[string]any{"hold_id": held.HoldID})
	if rpc != nil || isErr {
		t.Fatalf("dispatch_approve: %v %s", rpc, text)
	}
	var out struct {
		AgentID int64  `json:"agent_id"`
		Agent   string `json:"agent"`
		Model   string `json:"model"`
		Target  string `json:"target"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.AgentID <= 0 || out.Target != "pod/gateway" ||
		out.Model != "openrouter/vendor-a/coder-7" {
		t.Fatalf("approval answered %s", text)
	}
	list := h.ownersAgents(t)
	if len(list) != 1 || list[0].ID != out.AgentID {
		t.Fatalf("agents %+v, want exactly agent %d", list, out.AgentID)
	}
	a := list[0]
	if a.Kind != agents.KindGateway || a.Model != "openrouter/vendor-a/coder-7" || a.NoteID != nil {
		t.Errorf("agent %+v: want a gateway agent on the held model, no task", a)
	}
	msg := a.PendingNote
	if !strings.HasPrefix(msg, "muster: these instructions were relayed from one of your owner's own Claude Code sessions (a session on host ws)") ||
		!strings.Contains(msg, "<<<"+sessionInstructionsMarker+" untrusted nonce=") ||
		!strings.Contains(msg, "| Profile the nightly export and open a draft PR.\n") || !strings.HasSuffix(msg, sessionDispatchRulesNoTask) {
		t.Errorf("first message %q: want the session-relay preface, the fenced prompt, then muster's rules last", msg)
	}
	if n := h.prov.calls(1); n != 1 || !h.prov.kickoffs[0] {
		t.Errorf("provisioner dispatches %d (kickoffs %v), want one with a kickoff", n, h.prov.kickoffs)
	}
	objs := h.dispatchObjects(t)
	if len(objs) != 1 {
		t.Fatalf("reservations %v, want one", objs)
	}
	rd, _ := h.auditDetail(t, plugins.ActionChiefDispatch, objs[0])
	if rd[plugins.DetailHeld] != held.HoldID || rd["approved_from"] != "ws/"+focusSID || rd["target"] != "pod/gateway" {
		t.Errorf("reservation detail %v: want it to name the hold and the approving session", rd)
	}
	if after := h.budget(t); after.Today != before.Today+1 || after.Live != before.Live+1 {
		t.Errorf("budget %+v after the start, want today and live up by one from %+v", after, before)
	}
	ad, via := h.auditDetail(t, plugins.ActionDispatchApproved, held.HoldID)
	if via != string(authz.ViaMirrorHost) || ad["approved_from"] != "ws/"+focusSID || ad["requested_from"] != "ws/"+focusSID ||
		ad["agent"] != a.Name || ad["model"] != "openrouter/vendor-a/coder-7" || ad["target"] != "pod/gateway" ||
		ad["prompt_sha256"] != promptDigest("Profile the nightly export and open a draft PR.") {
		t.Errorf("approval row via=%s %v: want who approved, the agent, the target, the model and the prompt digest", via, ad)
	}
	h.srv.WaitForPushes()
	pushes := h.pusher.to(ep("hold-owner"))
	line := "main:1 · ws @ ws dispatched " + a.Name + " (pod/gateway, openrouter/vendor-a/coder-7)"
	if len(pushes) != 1 || pushes[0].Body != line || pushes[0].Tag != "dispatch-approved-"+a.Name || pushes[0].Data["url"] != "/agents/"+a.Name {
		t.Errorf("pushes %+v: want exactly one, %q", pushes, line)
	}

	// The replay: refused, and nothing more happens.
	text, isErr, _ = h.tool(t, h.ws, focusSID, dispatchApproveTool, map[string]any{"hold_id": held.HoldID})
	if want := "hold " + held.HoldID + " was already approved: a hold starts one agent, once. Nothing new was started"; !isErr || errorText(t, text) != want {
		t.Fatalf("the replay answered %s\nwant %q", text, want)
	}
	if got := h.ownersAgents(t); len(got) != 1 {
		t.Errorf("agents after the replay: %d, want 1", len(got))
	}
	if n := h.prov.calls(2); n != 1 {
		t.Errorf("provisioner dispatches after the replay: %d, want 1", n)
	}
	if n := h.allReservations(t); n != 1 {
		t.Errorf("reservations after the replay: %d, want 1", n)
	}
	h.srv.WaitForPushes()
	if n := len(h.pusher.to(ep("hold-owner"))); n != 1 {
		t.Errorf("pushes after the replay: %d, want 1", n)
	}
	// The chief's chat gets nothing: the push and the ledger reservation cover it.
	var notes int
	if err := h.pool.QueryRow(h.asOwner, `SELECT count(*) FROM chat_messages WHERE agent_id = $1`, h.chief.ID).Scan(&notes); err != nil || notes != 0 {
		t.Errorf("the chief's chat has %d message(s) (%v), want none", notes, err)
	}
}

// TestDispatchApproveOnATaskStartsItThere: a held start on an open task
// starts the agent on that task (claude-code: no model), the relayed prompt
// before the task's text and the task rules last, and moves it in_progress.
func TestDispatchApproveOnATaskStartsItThere(t *testing.T) {
	h := newHoldFixture(t)
	n := h.task(t, "nightly export")
	held := h.request(t, map[string]any{"target": "pod/claude-code", "task_id": n.ID, "prompt": "Start with the slow query."})
	text, isErr, _ := h.tool(t, h.ws, focusSID, dispatchApproveTool, map[string]any{"hold_id": held.HoldID})
	if isErr {
		t.Fatalf("approval: %s", text)
	}
	a := h.startedOne(t, text, n.ID)
	if a.Kind != agents.KindClaudeCode || a.Model != "" {
		t.Errorf("agent %+v: want a claude-code agent with no model", a)
	}
	msg := a.PendingNote
	if !strings.Contains(msg, "| Start with the slow query.") || !strings.HasSuffix(msg, sessionDispatchRules) ||
		strings.Index(msg, n.Body) < strings.Index(msg, "| Start with the slow query.") {
		t.Errorf("first message %q: want the relay, then the task, then the task rules", msg)
	}
	got, err := h.notes.Get(h.asOwner, n.ID)
	if err != nil || got.Status != "in_progress" {
		t.Errorf("task after the start: %+v %v; want in_progress", got.Status, err)
	}
}

// TestAnotherSessionOfTheOwnerMayApprove: a hold filed from the owner's "ws"
// session is approved from their session on another host ("laptop"), and the
// approval row names both.
func TestAnotherSessionOfTheOwnerMayApprove(t *testing.T) {
	h := newHoldFixture(t)
	held := h.request(t, gatewayRequest())
	text, isErr, _ := h.tool(t, h.laptop, holdLaptopSID, dispatchApproveTool, map[string]any{"hold_id": held.HoldID})
	if isErr {
		t.Fatalf("approval from the laptop: %s", text)
	}
	if got := h.ownersAgents(t); len(got) != 1 {
		t.Fatalf("agents %d, want 1", len(got))
	}
	ad, _ := h.auditDetail(t, plugins.ActionDispatchApproved, held.HoldID)
	if ad["approved_from"] != "laptop/"+holdLaptopSID || ad["requested_from"] != "ws/"+focusSID {
		t.Errorf("approval row %v: want approved from the laptop session, requested from ws", ad)
	}
}

// TestAnotherOwnersSessionCannotApprove: a session of another owner's reads
// the hold as missing (row-level security), consumes nothing, and the owner
// can still approve it.
func TestAnotherOwnersSessionCannotApprove(t *testing.T) {
	h := newHoldFixture(t)
	held := h.request(t, gatewayRequest())
	text, isErr, _ := h.tool(t, h.theirWS, focusSID, dispatchApproveTool, map[string]any{"hold_id": held.HoldID})
	if want := `no held dispatch "` + held.HoldID + `" of your owner's: dispatch_approve takes the hold_id dispatch_request returned`; !isErr ||
		errorText(t, text) != want {
		t.Fatalf("another owner's approval answered %s\nwant %q", text, want)
	}
	h.nothingStarted(t)
	var theirs int
	if err := h.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM agents WHERE owner_id = $1`, h.other).Scan(&theirs); err != nil || theirs != 0 {
		t.Errorf("the other owner has %d agents (%v), want none", theirs, err)
	}
	if _, isErr, _ := h.tool(t, h.ws, focusSID, dispatchApproveTool, map[string]any{"hold_id": held.HoldID}); isErr {
		t.Error("the owner's own approval after the refusal was refused")
	}
}

// TestAnExpiredHoldIsRefused: a hold 16 minutes old is refused with its own
// sentence and starts nothing.
func TestAnExpiredHoldIsRefused(t *testing.T) {
	h := newHoldFixture(t)
	held := h.request(t, gatewayRequest())
	if _, err := h.pool.Exec(authz.System(context.Background()), `UPDATE audit_log SET at = at - interval '16 minutes' WHERE object = $1`, held.HoldID); err != nil {
		t.Fatal(err)
	}
	text, isErr, _ := h.tool(t, h.ws, focusSID, dispatchApproveTool, map[string]any{"hold_id": held.HoldID})
	if !isErr || !strings.HasPrefix(errorText(t, text), "hold "+held.HoldID+" expired unapproved at ") {
		t.Fatalf("an expired hold answered %s", text)
	}
	h.nothingStarted(t)
}

// TestConcurrentApprovalsStartOneAgent: eight approvals of one hold at once
// start exactly one agent; the rest are refused as already approved.
func TestConcurrentApprovalsStartOneAgent(t *testing.T) {
	h := newHoldFixture(t)
	held := h.request(t, gatewayRequest())
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		refused int
	)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			args, _ := json.Marshal(map[string]any{"hold_id": held.HoldID})
			text := h.srv.toolDispatchApprove(h.mirrorCtx(), h.mirrorCaller(focusSID), string(args))
			if strings.Contains(text, "was already approved") {
				mu.Lock()
				refused++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if got := h.ownersAgents(t); len(got) != 1 || refused != 7 {
		t.Fatalf("%d agents and %d refusals from 8 concurrent approvals; want 1 and 7", len(got), refused)
	}
}

// TestAnApprovalThatLosesTheRaceIsRefusedUnderTheLock: a second approval of the
// same hold lands between this approval's early read (which saw the hold open)
// and its reservation. The check under the reservation's lock is what refuses
// this one — deterministically, with no timing — and one agent exists.
func TestAnApprovalThatLosesTheRaceIsRefusedUnderTheLock(t *testing.T) {
	h := newHoldFixture(t)
	held := h.request(t, gatewayRequest())
	args, _ := json.Marshal(map[string]any{"hold_id": held.HoldID})
	var inner string
	// The hook unsets itself before the inner approval runs, so the inner call
	// (which passes the same point) does not recurse into it.
	dispatchApproveBeforeReserve = func() {
		dispatchApproveBeforeReserve = nil
		inner = h.srv.toolDispatchApprove(h.mirrorCtx(), h.mirrorCaller(secondSID), string(args))
	}
	t.Cleanup(func() { dispatchApproveBeforeReserve = nil })
	outer := h.srv.toolDispatchApprove(h.mirrorCtx(), h.mirrorCaller(focusSID), string(args))
	if strings.Contains(inner, `"error"`) {
		t.Fatalf("the approval inside the window was refused: %s", inner)
	}
	if want := "hold " + held.HoldID + " was already approved: a hold starts one agent, once. Nothing new was started"; errorText(t, outer) != want {
		t.Fatalf("the approval that lost the race answered %s\nwant %q", outer, want)
	}
	if got := h.ownersAgents(t); len(got) != 1 {
		t.Errorf("%d agents, want 1", len(got))
	}
	if n := h.allReservations(t); n != 1 {
		t.Errorf("%d reservations, want 1", n)
	}
}

// mirrorCtx / mirrorCaller are the owner's "ws" session as handleMirrorMCP
// makes it, for a direct handler call.
func (h *holdFixture) mirrorCtx() context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: h.owner, Role: authz.RoleMember, Via: authz.ViaMirrorHost})
}

func (h *holdFixture) mirrorCaller(sid string) toolCaller {
	return toolCaller{asker: attention.Asker{MirrorHost: "ws", MirrorSession: sid, Label: "main:@1 @ ws"}}
}

// --- who may call -----------------------------------------------------------------

// TestOnlyTheOwnersMirroredSessionsMayRequestOrApprove: the chief and a
// dispatched agent are not offered either verb on /mcp, are refused by name
// there (unknown tool), and a gateway runtime naming them reaches no handler;
// called directly as an agent, or as a mirrored-looking caller whose request
// did not come through a mirror host's token, each handler refuses with its
// own sentence; an agent's token on /mirror/mcp is refused at the door. None
// of it holds or starts anything.
func TestOnlyTheOwnersMirroredSessionsMayRequestOrApprove(t *testing.T) {
	h := newHoldFixture(t)
	held := h.request(t, gatewayRequest())
	for _, tok := range []string{h.chiefTok, h.workerTok} {
		listed := h.listedFor(t, tok)
		if listed[dispatchRequestTool] || listed[dispatchApproveTool] {
			t.Errorf("/mcp offers the verbs to an agent: %v", listed)
		}
		for _, name := range []string{dispatchRequestTool, dispatchApproveTool} {
			_, _, rpc := h.mcpCall(t, tok, name, map[string]any{"hold_id": held.HoldID, "target": "pod/claude-code"})
			if rpc == nil || rpc.Code != -32602 {
				t.Errorf("%s over /mcp by an agent: %+v; want unknown tool (-32602)", name, rpc)
			}
		}
	}
	for _, name := range []string{dispatchRequestTool, dispatchApproveTool} {
		text := h.callRaw(h.chief, name, map[string]any{"hold_id": held.HoldID})
		if !strings.Contains(text, `unknown tool`) {
			t.Errorf("%s named by a gateway chief: %s; want unknown tool", name, text)
		}
	}
	reqArgs, _ := json.Marshal(gatewayRequest())
	appArgs, _ := json.Marshal(map[string]any{"hold_id": held.HoldID})
	sessionCtx := h.as(h.owner) // a signed-in browser principal, not a mirror host's token
	for _, c := range []struct {
		name   string
		ctx    context.Context
		caller toolCaller
	}{
		{"the chief", h.mirrorCtx(), callerOfAgent(h.chief)},
		{"a dispatched agent", h.mirrorCtx(), callerOfAgent(h.worker)},
		{"a mirrored-looking caller without a mirror host's token", sessionCtx, h.mirrorCaller(focusSID)},
		{"no principal", context.Background(), h.mirrorCaller(focusSID)},
	} {
		if got := errorText(t, h.srv.toolDispatchRequest(c.ctx, c.caller, string(reqArgs))); got != "dispatch_request is available only to your owner's own mirrored Claude Code sessions" {
			t.Errorf("dispatch_request by %s: %q", c.name, got)
		}
		if got := errorText(t, h.srv.toolDispatchApprove(c.ctx, c.caller, string(appArgs))); got != "dispatch_approve is available only to your owner's own mirrored Claude Code sessions" {
			t.Errorf("dispatch_approve by %s: %q", c.name, got)
		}
	}
	w := h.connFixture.call(t, http.MethodPost, "/mirror/mcp", strings.NewReader(mcpToolCall(dispatchApproveTool, map[string]any{"hold_id": held.HoldID})),
		map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + h.chiefTok, mirrorSessionHeader: focusSID})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("an agent's token on /mirror/mcp: HTTP %d, want 401", w.Code)
	}
	h.nothingStarted(t)
	if n := h.auditRows(t, plugins.ActionDispatchHeld); n != 1 {
		t.Errorf("%d hold rows, want only the one the owner's session filed", n)
	}
}

// listedFor is POST /mcp's tools/list for an agent's token.
func (h *holdFixture) listedFor(t *testing.T, token string) map[string]bool {
	t.Helper()
	r, code := h.mcp(t, token, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	if r == nil {
		t.Fatalf("/mcp tools/list: HTTP %d", code)
	}
	return holdToolNames(t, r.Result)
}

// TestTheVerbsAreListedOnTheMirrorDoorOnly is the per-door ledger: with the
// chief's dispatch on, the mirror door lists both verbs (with their feature,
// task routing); the agent door never does. With dispatch off the mirror door
// lists neither, and a call by name is refused as unknown.
func TestTheVerbsAreListedOnTheMirrorDoorOnly(t *testing.T) {
	h := newHoldFixture(t)
	mirrorTools := h.mirrorListed(t, h.ws)
	if !mirrorTools[dispatchRequestTool] || !mirrorTools[dispatchApproveTool] {
		t.Errorf("mirror door tools %v: want both verbs", mirrorTools)
	}
	for _, d := range h.srv.AgentToolDefsFor(h.asOwner, h.chief) {
		if d.Name == dispatchRequestTool || d.Name == dispatchApproveTool {
			t.Errorf("the chief's registry offers %s", d.Name)
		}
	}
	for _, d := range h.srv.AgentToolDefsFor(h.asOwner, h.worker) {
		if d.Name == dispatchRequestTool || d.Name == dispatchApproveTool {
			t.Errorf("a dispatched agent's registry offers %s", d.Name)
		}
	}
	tf := toolFeatures(mirrorDoor)
	if tf[dispatchRequestTool] != "task-routing" || tf[dispatchApproveTool] != "task-routing" {
		t.Errorf("mirror door features %v: want both verbs gated by task-routing", tf)
	}
	if af := toolFeatures(agentDoor); af[dispatchRequestTool] != "" || af[dispatchApproveTool] != "" {
		t.Errorf("agent door features list a verb: %v", af)
	}

	off := newHoldFixtureOff(t)
	listed := off.mirrorListed(t, off.ws)
	if listed[dispatchRequestTool] || listed[dispatchApproveTool] {
		t.Errorf("dispatch off: mirror door tools %v list a verb", listed)
	}
	if !listed[attentionAnswerTool] {
		t.Fatalf("dispatch off: mirror door tools %v — positive control: attention_answer must be listed", listed)
	}
	_, _, rpc := off.tool(t, off.ws, focusSID, dispatchRequestTool, gatewayRequest())
	if rpc == nil || rpc.Code != -32602 {
		t.Errorf("dispatch off: a call by name answered %+v; want unknown tool (-32602)", rpc)
	}
}

// newHoldFixtureOff is newHoldFixture with the chief's dispatch OFF (no
// owner list).
func newHoldFixtureOff(t *testing.T) *holdFixture {
	t.Helper()
	h := newHoldFixture(t)
	for k := range h.tr.DispatchOwners {
		delete(h.tr.DispatchOwners, k)
	}
	return h
}

// --- the budget -------------------------------------------------------------------

// TestTheBudgetRefusesARequestAndAnApproval: with the owner's live cap full a
// request is refused with the cap's sentence and the budget, holding nothing;
// a hold filed before the cap filled is refused at approval with the same
// sentence, and stays open (nothing names it).
func TestTheBudgetRefusesARequestAndAnApproval(t *testing.T) {
	h := newHoldFixture(t)
	held := h.request(t, gatewayRequest())
	for i := 0; i < plugins.MaxChiefLiveAgents; i++ {
		obj, _ := plugins.NewDispatchObject()
		if _, err := h.plugins.ReserveChiefDispatch(h.asOwner, obj, map[string]any{"target": "pod/claude-code"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	reserved := h.allReservations(t)
	text, isErr, _ := h.tool(t, h.ws, focusSID, dispatchRequestTool, gatewayRequest())
	var out struct {
		Error  string               `json:"error"`
		Budget *plugins.ChiefBudget `json:"budget"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || !isErr || out.Error != dispatchRequestLiveCap || out.Budget == nil ||
		out.Budget.Live != plugins.MaxChiefLiveAgents {
		t.Fatalf("a request at the live cap answered %s", text)
	}
	if n := h.auditRows(t, plugins.ActionDispatchHeld); n != 1 {
		t.Errorf("%d hold rows, want only the early one", n)
	}
	text, isErr, _ = h.tool(t, h.ws, focusSID, dispatchApproveTool, map[string]any{"hold_id": held.HoldID})
	if !isErr || errorText(t, text) != dispatchRequestLiveCap {
		t.Fatalf("an approval at the live cap answered %s", text)
	}
	if n := h.allReservations(t); n != reserved {
		t.Errorf("reservations %d after a capped approval, want %d", n, reserved)
	}
	if got := h.ownersAgents(t); len(got) != 0 {
		t.Errorf("agents %+v, want none", got)
	}
	hold, err := h.plugins.DispatchHold(h.asOwner, held.HoldID)
	if err != nil || !hold.Open() {
		t.Errorf("the hold after a capped approval: %+v %v; want still open", hold, err)
	}
}

// TestTheHoldIDShapeIsChecked: a hold id that is not one is "no held
// dispatch", and an approval with no hold_id too.
func TestTheHoldIDShapeIsChecked(t *testing.T) {
	h := newHoldFixture(t)
	for _, id := range []string{"", "held:nope", "dispatch:0123456789abcdef0123456789abcdef"} {
		text, isErr, _ := h.tool(t, h.ws, focusSID, dispatchApproveTool, map[string]any{"hold_id": id})
		if !isErr || !regexp.MustCompile(`^no held dispatch ".*" of your owner's`).MatchString(errorText(t, text)) {
			t.Errorf("hold_id %q answered %s", id, text)
		}
	}
	h.nothingStarted(t)
}
