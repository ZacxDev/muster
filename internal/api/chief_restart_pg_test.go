package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/provision"
)

// chief_restart (chief_restart_tool.go) over a real Postgres: the operator's
// mirrored Claude Code session restarts the owner's OWN chief through the agent
// page's own Stop and Start (stopAgent, startAgent), waits for it to be running
// on a new pod, and reports the image; nobody else can call it, it takes no
// agent, another owner's chief and a non-chief agent are never reached (with
// row-level security rolled back too), and a mid-turn chief is refused unless
// force.
//
// ⚠ THE TOOL'S NAME, ITS ARGUMENTS AND ITS REFUSALS ARE STRING LITERALS here,
// so a change of any of them is a red test. The file does NOT build on the
// pre-feature tree (it drives toolChiefRestart and the wait knobs directly);
// the door-level red — the forwarded call answered as an unknown tool — was
// measured by removing chief_restart's case from handleMirrorMCP.

// restartProv records each Stop and Start, in order, and models the
// provisioner's effect the way agentprovision.Adapter does: Stop scales the
// instance to zero and stores `stopped`; Start brings up a NEW pod (a new
// InstanceID) on image and stores `running`. Instances reports every agent it
// has seen.
type restartProv struct {
	stubProvisioner
	pool  *pgxpool.Pool
	image string
	mu    sync.Mutex
	calls []string
	insts map[string]provision.Instance
	pods  int
	// startFails makes Start fail (the row is left stopped).
	startFails error
	// noReady makes a started instance never become ready.
	noReady bool
	// staleReads is how many Instances reads after a Start still answer the
	// instance as it was BEFORE the Stop (a lagging backend listing).
	staleReads int
	stale      map[string]provision.Instance
}

func (p *restartProv) name(id int64) string {
	var n string
	_ = p.pool.QueryRow(authz.System(context.Background()), `SELECT name FROM agents WHERE id = $1`, id).Scan(&n)
	return n
}

func (p *restartProv) setStatus(id int64, status string) error {
	_, err := p.pool.Exec(authz.System(context.Background()), `UPDATE agents SET status = $2 WHERE id = $1`, id, status)
	return err
}

// seed gives agent a a running, ready instance on pod.
func (p *restartProv) seed(a agents.Agent, pod, image string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.insts == nil {
		p.insts = map[string]provision.Instance{}
	}
	p.insts[a.Name] = provision.Instance{Ref: provision.Ref{Name: a.Name}, InstanceID: pod, Phase: provision.PhaseRunning, Ready: true, Replicas: 1, Image: image}
}

func (p *restartProv) Stop(_ context.Context, id int64) error {
	n := p.name(id)
	p.mu.Lock()
	p.calls = append(p.calls, "stop:"+strconv.FormatInt(id, 10))
	if p.insts == nil {
		p.insts = map[string]provision.Instance{}
	}
	p.stale = map[string]provision.Instance{}
	for k, v := range p.insts {
		p.stale[k] = v
	}
	p.insts[n] = provision.Instance{Ref: provision.Ref{Name: n}, Phase: provision.PhaseStopped, Reason: "scaled to zero"}
	p.mu.Unlock()
	return p.setStatus(id, agents.StatusStopped)
}

func (p *restartProv) Start(_ context.Context, id int64) error {
	n := p.name(id)
	p.mu.Lock()
	p.calls = append(p.calls, "start:"+strconv.FormatInt(id, 10))
	if p.startFails != nil {
		err := p.startFails
		p.mu.Unlock()
		return err
	}
	p.pods++
	p.insts[n] = provision.Instance{Ref: provision.Ref{Name: n}, InstanceID: n + "-pod-" + strconv.Itoa(p.pods),
		Phase: provision.PhaseRunning, Ready: !p.noReady, Replicas: 1, Image: p.image}
	p.mu.Unlock()
	return p.setStatus(id, agents.StatusRunning)
}

func (p *restartProv) Instances(context.Context) ([]provision.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	src := p.insts
	if p.staleReads > 0 && p.pods > 0 && p.stale != nil {
		p.staleReads--
		src = p.stale
	}
	out := make([]provision.Instance, 0, len(src))
	for _, i := range src {
		out = append(out, i)
	}
	return out, nil
}

func (p *restartProv) callList() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

// unavailableKinds is an AgentKinds whose Unavailable answers why; every other
// method panics (the nil embedded interface): the start refusal asks nothing
// else.
type unavailableKinds struct {
	AgentKinds
	why map[string]string
}

func (k unavailableKinds) Unavailable(context.Context, int64) map[string]string { return k.why }

const restartImage = "ghcr.io/example-org/cc@sha256:2222"

// restartFixture is the attention fixture (user 1's agents op and op2; a member
// with a labelled chief and a worker) plus a recording restartProv starting
// pods on restartImage, mirror tokens ws (user 1's host "ws", listing
// focusSID) and members (the member's token for a host also named "ws"), and
// user 1's chief as resolveChief over user 1's own agents finds it.
type restartFixture struct {
	*attnFixture
	prov        *restartProv
	ownChief    agents.Agent
	ws, members string
	log         *lockedBuf
}

func newRestartFixture(t *testing.T) *restartFixture {
	t.Helper()
	f := &restartFixture{prov: &restartProv{image: restartImage}, log: &lockedBuf{}}
	f.attnFixture = newAttnFixture(t, func(ext *Extensions, pool *pgxpool.Pool) {
		f.prov.pool = pool
		ext.Provisioner = f.prov
	})
	f.srv.logger = log.New(f.log, "", 0)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("server log:\n%s", f.log.String())
		}
	})
	oldWait, oldPoll := chiefRestartWait, chiefRestartPoll
	chiefRestartWait, chiefRestartPoll = 2*time.Second, 10*time.Millisecond
	t.Cleanup(func() { chiefRestartWait, chiefRestartPoll = oldWait, oldPoll })
	// User 1's chief: the one the tool's own rule resolves. A shared database
	// may already hold one; otherwise one is made here.
	list, err := agents.NewPG(f.pool).List(asUser(1))
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := resolveChief(ownedBy(list, 1)); ok {
		f.ownChief = c
	} else {
		f.ownChief = f.agentFor(t, 1, "rschief-"+strconv.FormatInt(time.Now().UnixNano(), 36), "chief", "rs-chief-tok-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	}
	if on, err := f.srv.chiefOnFor(asUser(1), 1); err != nil || !on {
		t.Fatalf("user 1's chief is off in this database (%v): the restart tests need it on", err)
	}
	f.prov.seed(f.ownChief, f.ownChief.Name+"-pod-old", "ghcr.io/example-org/cc@sha256:1111")
	f.ws, f.members = mirrorTok('a'), mirrorTok('c')
	f.tokens[f.ws] = mirror.Token{ID: 1, OwnerID: 1, Host: "ws"}
	f.tokens[f.members] = mirror.Token{ID: 3, OwnerID: f.member, Host: "ws"}
	// The chief's row is restored to running whatever a test did to it.
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()),
			`UPDATE agents SET status = $2, kickoff_claim_owner = NULL, kickoff_claim_expires_at = NULL WHERE id = $1`, f.ownChief.ID, f.ownChief.Status)
	})
	return f
}

// restart calls chief_restart through /mirror/mcp as token's host and session
// sid: the tool's text and whether it is a tool error.
func (f *restartFixture) restart(t *testing.T, token, sid string, args map[string]any) (string, bool) {
	t.Helper()
	code, r, body := f.mirrorRPC(t, mirrorAs(token, sid), mcpToolCall("chief_restart", args))
	if code != http.StatusOK || r == nil || r.Error != nil {
		t.Fatalf("chief_restart %v: HTTP %d %s", args, code, body)
	}
	return toolText(t, r)
}

// sessionCaller is a mirrored session of host ws, as handleMirrorMCP builds it.
func sessionCaller() toolCaller {
	return toolCaller{asker: attention.Asker{MirrorHost: "ws", MirrorSession: focusSID, Label: "main @ ws"}}
}

// labelledChief creates a claude-code agent of owner labelled chief.
func (f *restartFixture) labelledChief(t *testing.T, owner int64, name string) agents.Agent {
	t.Helper()
	a, err := agents.NewPG(f.pool).Create(asUser(owner), agents.Agent{Name: name + "-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Namespace: "devpod-" + name, DisplayName: "chief", Status: agents.StatusRunning, Kind: agents.KindClaudeCode, CCAccount: "rs-acct", KickedOff: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, a.ID)
	})
	return a
}

type restartResult struct {
	AgentID int64  `json:"agent_id"`
	Agent   string `json:"agent"`
	Before  struct {
		Status, Pod, Image string
		Ready              bool
	} `json:"before"`
	After struct {
		Status, Pod, Image string
		Ready              bool
	} `json:"after"`
	Restarted bool `json:"restarted"`
	Forced    bool `json:"forced"`
}

func decodeRestart(t *testing.T, text string) restartResult {
	t.Helper()
	var r restartResult
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		t.Fatalf("result %q: %v", text, err)
	}
	return r
}

// TestAMirroredSessionRestartsItsOwnersChief is the feature, through the real
// /mirror/mcp door: user 1's session restarts user 1's chief. Stop then Start
// run for exactly the chief and nothing else; the result names the chief, the
// pod before and the NEW pod after, running and ready; the server log names the
// host and session; and no other agent of user 1's was touched.
func TestAMirroredSessionRestartsItsOwnersChief(t *testing.T) {
	f := newRestartFixture(t)
	// Listed on the mirror door, with a provisioner wired (mirrorTools).
	_, lr, body := f.mirrorRPC(t, mirrorAs(f.ws, focusSID), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if lr == nil || !strings.Contains(string(lr.Result), `"name":"chief_restart"`) {
		t.Fatalf("tools/list on /mirror/mcp lacks chief_restart: %s", body)
	}
	text, isErr := f.restart(t, f.ws, focusSID, map[string]any{})
	if isErr {
		t.Fatalf("chief_restart: %s", text)
	}
	r := decodeRestart(t, text)
	id := strconv.FormatInt(f.ownChief.ID, 10)
	if got := strings.Join(f.prov.callList(), " "); got != "stop:"+id+" start:"+id {
		t.Fatalf("provisioner calls %q, want stop then start of the chief %s only", got, id)
	}
	if !r.Restarted || r.AgentID != f.ownChief.ID || r.Agent != f.ownChief.Name {
		t.Errorf("result: %s", text)
	}
	if r.Before.Pod != f.ownChief.Name+"-pod-old" || r.After.Pod == "" || r.After.Pod == r.Before.Pod ||
		r.After.Status != agents.StatusRunning || !r.After.Ready {
		t.Errorf("before/after: %s", text)
	}
	if !strings.Contains(f.log.String(), "restart of the chief "+id+" ("+f.ownChief.Name+") started by the operator via MCP from ws/"+focusSID) {
		t.Errorf("the server log does not name the restart's host and session:\n%s", f.log.String())
	}
	for _, a := range []agents.Agent{f.op, f.op2} {
		if strings.Contains(strings.Join(f.prov.callList(), " "), ":"+strconv.FormatInt(a.ID, 10)) {
			t.Errorf("agent %s was touched", a.Name)
		}
	}
	// The session claim and the turn gate are released after the restart.
	if won, err := f.srv.ext.Agents.ClaimKickoff(asUser(1), f.ownChief.ID, "probe", time.Minute); err != nil || !won {
		t.Errorf("the chief's session claim is still held after the restart (%v)", err)
	} else {
		_ = f.srv.ext.Agents.ReleaseKickoffClaim(asUser(1), f.ownChief.ID, "probe")
	}
	if !f.srv.turns.enter(f.ownChief.ID) {
		t.Errorf("the chief's turn gate is still closed after the restart")
	} else {
		f.srv.turns.leave(f.ownChief.ID)
	}
}

// TestChiefRestartReportsTheRunningImage: a restarted chief's result reports
// before.image and after.image as the provisioner reads them from the agent
// container, and nothing compares them with a configured image (a restart onto
// any image succeeds, and the result carries no expected_image/image_matches);
// one that never becomes ready is a tool error saying it was not seen running
// (after the stop and start, which did run). Driven through the handler as a
// fresh member whose claude-code chief is made here.
func TestChiefRestartReportsTheRunningImage(t *testing.T) {
	f := newRestartFixture(t)
	owner := f.newMember(t)
	chief := f.labelledChief(t, owner, "rsccchief")
	f.prov.seed(chief, "old-pod", "ghcr.io/example-org/cc@sha256:1111")

	text := f.srv.toolChiefRestart(asUser(owner), sessionCaller(), `{}`)
	r := decodeRestart(t, text)
	if !r.Restarted || r.After.Image != restartImage || r.Before.Image != "ghcr.io/example-org/cc@sha256:1111" {
		t.Fatalf("reported images: %s", text)
	}

	f.prov.mu.Lock()
	f.prov.image = "ghcr.io/example-org/cc@sha256:9999"
	f.prov.mu.Unlock()
	text = f.srv.toolChiefRestart(asUser(owner), sessionCaller(), `{}`)
	if isToolErr(text) {
		t.Fatalf("a restart onto another image is not the tool's to fail: %s", text)
	}
	if r := decodeRestart(t, text); !r.Restarted || r.Before.Image != restartImage || r.After.Image != "ghcr.io/example-org/cc@sha256:9999" {
		t.Fatalf("reported images after an image change: %s", text)
	}
	for _, k := range []string{"expected_image", "image_matches", "image_note"} {
		if strings.Contains(text, k) {
			t.Fatalf("the result still carries %s: %s", k, text)
		}
	}

	f.prov.mu.Lock()
	f.prov.noReady = true
	f.prov.mu.Unlock()
	text = f.srv.toolChiefRestart(asUser(owner), sessionCaller(), `{}`)
	if !isToolErr(text) || !strings.Contains(text, "was stopped and started, but was not seen running on a new pod") {
		t.Fatalf("never ready: %s", text)
	}
}

// TestChiefRestartTouchesOnlyTheCallersOwnChief: a member with a chief and a
// worker; user 1's session-shaped call as that member restarts the member's
// chief and nothing of user 1's; a member with NO chief is refused and nothing
// is stopped (a non-chief agent is never the fallback); arguments naming an
// agent are refused.
func TestChiefRestartTouchesOnlyTheCallersOwnChief(t *testing.T) {
	f := newRestartFixture(t)
	owner := f.newMember(t)
	chief := f.labelledChief(t, owner, "rsownchief")
	f.prov.seed(chief, "old-pod", "")
	if text := f.srv.toolChiefRestart(asUser(owner), sessionCaller(), `{}`); isToolErr(text) {
		t.Fatalf("control: %s", text)
	}
	cid := strconv.FormatInt(chief.ID, 10)
	if got := strings.Join(f.prov.callList(), " "); got != "stop:"+cid+" start:"+cid {
		t.Fatalf("calls %q, want only the member's chief %s", got, cid)
	}

	// A member with only a worker: refused, and the worker is not restarted.
	lone := f.newMember(t)
	worker := f.agentFor(t, lone, "rsworker-"+strconv.FormatInt(time.Now().UnixNano(), 36), "", "rs-worker-tok-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	before := len(f.prov.callList())
	if text := f.srv.toolChiefRestart(asUser(lone), sessionCaller(), `{}`); text != `{"error":"your owner has no chief (no agent labelled \"chief\"); nothing was restarted"}` {
		t.Errorf("an owner with no chief: %s", text)
	}
	if got := f.prov.callList(); len(got) != before {
		t.Errorf("an owner with no chief reached the provisioner: %v (worker %d)", got[before:], worker.ID)
	}

	for _, args := range []string{`{"id":` + strconv.FormatInt(f.ownChief.ID, 10) + `}`, `{"name":"` + f.ownChief.Name + `"}`, `{"force":"yes"}`} {
		if text := f.srv.toolChiefRestart(asUser(owner), sessionCaller(), args); text != `{"error":"arguments are {force?}; chief_restart takes no agent — it restarts your owner's own chief"}` {
			t.Errorf("%s: %s", args, text)
		}
	}
	if got := f.prov.callList(); len(got) != before {
		t.Errorf("refused arguments reached the provisioner: %v", got[before:])
	}
}

// TestChiefRestartKeepsToTheOwnersChiefWithRLSOff: with row-level security
// rolled back (db.RLSOff), the agent list holds every owner's agents, so the
// tool's own owner filter is the ONLY guard. A fresh member whose chief is
// made AFTER the shared database's other chiefs (so it is NOT the lowest-id
// "chief" overall) restarts its own chief, and no other owner's chief is
// stopped. POSITIVE CONTROL first: under RLS off the member's list really does
// hold another owner's lower-id chief, so the guard is what is tested.
func TestChiefRestartKeepsToTheOwnersChiefWithRLSOff(t *testing.T) {
	f := newRestartFixture(t)
	owner := f.newMember(t)
	chief := f.labelledChief(t, owner, "rsrlschief")
	f.prov.seed(chief, "old-pod", "")
	if err := db.RLSOff(context.Background(), f.pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.RLSOn(context.Background(), f.pool); err != nil {
			t.Errorf("RLSOn: %v", err)
		}
	})
	list, err := f.srv.ext.Agents.List(asUser(owner))
	if err != nil {
		t.Fatal(err)
	}
	if global, ok := resolveChief(list); !ok || global.OwnerID == owner {
		t.Fatalf("positive control: under RLS off the lowest-id chief is %+v (ok %v), not another owner's — the owner filter would not be what is tested", global, ok)
	}
	text := f.srv.toolChiefRestart(asUser(owner), sessionCaller(), `{}`)
	cid := strconv.FormatInt(chief.ID, 10)
	if got := strings.Join(f.prov.callList(), " "); got != "stop:"+cid+" start:"+cid {
		t.Fatalf("with RLS off the restart reached %q (result %s), want only the caller's chief %s", got, text, cid)
	}
}

// TestChiefRestartRefusesAMidTurnChiefUnlessForced: with the chief's session
// claim held (a delivery in flight) the call is refused and nothing is stopped;
// likewise with a chat turn running (the turn gate). force:true restarts it
// anyway, reports forced, and leaves the delivery's claim alone.
func TestChiefRestartRefusesAMidTurnChiefUnlessForced(t *testing.T) {
	f := newRestartFixture(t)
	if won, err := f.srv.ext.Agents.ClaimKickoff(asUser(1), f.ownChief.ID, "plugin-delivery/test", time.Minute); err != nil || !won {
		t.Fatalf("could not take the chief's claim for the test (%v)", err)
	}
	t.Cleanup(func() { _ = f.srv.ext.Agents.ReleaseKickoffClaim(asUser(1), f.ownChief.ID, "plugin-delivery/test") })
	want := `{"error":"the chief ` + f.ownChief.Name + ` is mid-turn (a plugin message, its kickoff or a memo is being delivered to it); nothing was restarted — retry when it finishes, or pass force:true to cut the turn off as Stop does"}`
	if text, isErr := f.restart(t, f.ws, focusSID, map[string]any{}); !isErr || text != want {
		t.Fatalf("mid-delivery: %s", text)
	}
	if got := f.prov.callList(); len(got) != 0 {
		t.Fatalf("a refused mid-turn restart reached the provisioner: %v", got)
	}
	_ = f.srv.ext.Agents.ReleaseKickoffClaim(asUser(1), f.ownChief.ID, "plugin-delivery/test")

	if !f.srv.turns.enter(f.ownChief.ID) {
		t.Fatal("could not start a chat turn for the test")
	}
	wantChat := `{"error":"a chat turn to the chief ` + f.ownChief.Name + ` is running; nothing was restarted — retry when it finishes, or pass force:true to cut the turn off as Stop does"}`
	if text, isErr := f.restart(t, f.ws, focusSID, map[string]any{}); !isErr || text != wantChat {
		t.Fatalf("mid-chat: %s", text)
	}
	if got := f.prov.callList(); len(got) != 0 {
		t.Fatalf("a refused mid-chat restart reached the provisioner: %v", got)
	}
	// The refusal released the claim it had taken: a delivery can still start.
	if won, err := f.srv.ext.Agents.ClaimKickoff(asUser(1), f.ownChief.ID, "plugin-delivery/test", time.Minute); err != nil || !won {
		t.Fatalf("the refused restart kept the chief's session claim (%v)", err)
	}

	// force: claim held AND a chat turn running — restarted anyway.
	text, isErr := f.restart(t, f.ws, focusSID, map[string]any{"force": true})
	if isErr {
		t.Fatalf("forced: %s", text)
	}
	if r := decodeRestart(t, text); !r.Restarted || !r.Forced {
		t.Fatalf("forced result: %s", text)
	}
	id := strconv.FormatInt(f.ownChief.ID, 10)
	if got := strings.Join(f.prov.callList(), " "); got != "stop:"+id+" start:"+id {
		t.Fatalf("forced calls %q", got)
	}
	// The delivery's claim is still the delivery's: force took nothing from it.
	if won, _ := f.srv.ext.Agents.ClaimKickoff(asUser(1), f.ownChief.ID, "another", time.Minute); won {
		t.Errorf("the forced restart released a claim it did not hold")
	}
	f.srv.turns.leave(f.ownChief.ID)
}

// TestChiefRestartIsRefusedBeforeTheStopWhenTheStartWouldBe: a kind the owner
// has no credential for refuses the start (agentStartRefusal, the browser
// Start's own rule) — so nothing is stopped. A start that fails after the stop
// says the chief is STOPPED.
func TestChiefRestartIsRefusedBeforeTheStopWhenTheStartWouldBe(t *testing.T) {
	f := newRestartFixture(t)
	owner := f.newMember(t)
	chief := f.labelledChief(t, owner, "rskindchief")
	f.prov.seed(chief, "old-pod", "")
	f.srv.ext.Kinds = unavailableKinds{why: map[string]string{agents.KindClaudeCode: "add a Claude account in Account"}}
	text := f.srv.toolChiefRestart(asUser(owner), sessionCaller(), `{}`)
	if !isToolErr(text) || !strings.Contains(text, "could not be started again (add a Claude account in Account), so it was not stopped") {
		t.Fatalf("an unstartable kind: %s", text)
	}
	if got := f.prov.callList(); len(got) != 0 {
		t.Fatalf("an unstartable chief was stopped: %v", got)
	}
	f.srv.ext.Kinds = nil
	f.prov.mu.Lock()
	f.prov.startFails = context.DeadlineExceeded
	f.prov.mu.Unlock()
	text = f.srv.toolChiefRestart(asUser(owner), sessionCaller(), `{}`)
	if !isToolErr(text) || !strings.Contains(text, "was STOPPED but could not be started again") {
		t.Fatalf("a failed start: %s", text)
	}
}

// TestOnlyAMirroredSessionCanRestartTheChief: chief_restart is not reachable by
// a dispatched agent — not through the gateway dispatcher (unknown there), not
// over POST /mcp (neither listed nor callable), not as a gateway tool def — nor
// by a non-mirror credential on /mirror/mcp (401), nor by the member's host
// token (its host lists no session); and the handler itself refuses a caller
// that is not a mirrored session. Nothing reaches the provisioner. CONTROL: the
// mirrored session restarts it.
func TestOnlyAMirroredSessionCanRestartTheChief(t *testing.T) {
	f := newRestartFixture(t)
	callers := map[string]struct {
		a     agents.Agent
		tok   string
		owner int64
	}{
		"a worker":      {f.op, f.opTok, 1},
		"another chief": {f.chief, f.chiefTok, f.member},
	}
	for name, c := range callers {
		if got := f.srv.dispatchAgentTool(asUser(c.owner), c.a, "chief_restart", `{}`); !strings.Contains(got, `"error"`) || strings.Contains(got, "restarted") {
			t.Errorf("%s through the gateway dispatcher: %s", name, got)
		}
		if _, _, rpc := f.tool(t, c.tok, "chief_restart", map[string]any{}); rpc == nil || rpc.Code != -32602 {
			t.Errorf("%s over POST /mcp: %+v, want unknown tool", name, rpc)
		}
		if f.listed(t, c.tok)["chief_restart"] {
			t.Errorf("%s is offered chief_restart on /mcp", name)
		}
		for _, d := range f.srv.AgentToolDefsFor(asUser(c.owner), c.a) {
			if d.Name == "chief_restart" {
				t.Errorf("%s is offered chief_restart as a gateway tool", name)
			}
		}
	}
	const refused = `{"error":"chief_restart is available only to your owner's own mirrored Claude Code sessions"}`
	for name, c := range map[string]toolCaller{
		"an agent":   {agent: &f.op, asker: attention.Asker{AgentID: f.op.ID, Label: f.op.Name}},
		"muster":     {asker: attention.Asker{System: true}},
		"no session": {asker: attention.Asker{MirrorHost: "ws"}},
		"no host":    {asker: attention.Asker{MirrorSession: focusSID}},
	} {
		if got := f.srv.toolChiefRestart(asUser(1), c, `{}`); got != refused {
			t.Errorf("%s calling the handler: %s", name, got)
		}
	}
	for name, tok := range map[string]string{"the hook token": connTestHookToken, "an agent's token": f.opTok} {
		if code, _, body := f.mirrorRPC(t, mirrorAs(tok, focusSID), mcpToolCall("chief_restart", map[string]any{})); code != http.StatusUnauthorized {
			t.Errorf("%s on /mirror/mcp: %d %s, want 401", name, code, body)
		}
	}
	if text, isErr := f.restart(t, f.members, focusSID, map[string]any{}); !isErr || !strings.Contains(text, "lists no window running Claude session") {
		t.Errorf("the member's mirror token: %s", text)
	}
	if got := f.prov.callList(); len(got) != 0 {
		t.Fatalf("a refused caller reached the provisioner: %v", got)
	}
	if text, isErr := f.restart(t, f.ws, focusSID, map[string]any{}); isErr {
		t.Fatalf("control: %s", text)
	}
}

// TestALaggingListingOfTheOldPodIsNotTakenAsRestarted: the backend's listing
// still shows the chief's OLD pod, running and ready, for a few reads after the
// start. The wait does not take that as the restart: it reports the NEW pod.
func TestALaggingListingOfTheOldPodIsNotTakenAsRestarted(t *testing.T) {
	f := newRestartFixture(t)
	owner := f.newMember(t)
	chief := f.labelledChief(t, owner, "rslagchief")
	f.prov.seed(chief, "old-pod", restartImage)
	f.prov.mu.Lock()
	f.prov.staleReads = 3
	f.prov.mu.Unlock()
	text := f.srv.toolChiefRestart(asUser(owner), sessionCaller(), `{}`)
	if r := decodeRestart(t, text); !r.Restarted || r.Before.Pod != "old-pod" || r.After.Pod == "" || r.After.Pod == "old-pod" {
		t.Fatalf("a lagging listing: %s", text)
	}
}

// TestChiefRestartRefusesAChiefTurnedOff: an owner who turned their chief off
// (chief_toggle.go) is refused and nothing is stopped or started. CONTROL: on
// again, the same call restarts it.
func TestChiefRestartRefusesAChiefTurnedOff(t *testing.T) {
	f := newRestartFixture(t)
	owner := f.newMember(t)
	chief := f.labelledChief(t, owner, "rsoffchief")
	f.prov.seed(chief, "old-pod", "")
	prefs := &memChiefPrefs{}
	f.srv.ext.ChiefPrefs = prefs
	_ = prefs.SetEnabled(context.Background(), owner, false)
	want := `{"error":"your owner's chief ` + chief.Name + ` is turned off; nothing was restarted — turn it on from muster first"}`
	if text := f.srv.toolChiefRestart(asUser(owner), sessionCaller(), `{}`); text != want {
		t.Fatalf("a chief turned off: %s", text)
	}
	if got := f.prov.callList(); len(got) != 0 {
		t.Fatalf("a chief turned off was restarted: %v", got)
	}
	_ = prefs.SetEnabled(context.Background(), owner, true)
	if text := f.srv.toolChiefRestart(asUser(owner), sessionCaller(), `{}`); isToolErr(text) {
		t.Fatalf("control: %s", text)
	}
}
