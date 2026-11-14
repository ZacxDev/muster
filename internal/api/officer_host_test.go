package api

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// OFFICERS ON HOSTS (officer_host.go; docs/design/officers-on-hosts.md).
//
// Two officers against one fake HQ: officer A runs in PODS (the existing rig's
// provisioner), officer B on HOST host-bravo. The fixture's other host,
// host-charlie, is spawnable too, so a mutant that sends B's spawn to "the
// first host" or "any spawnable host" is seen. Ids, windows and sessions are
// pairwise distinct and distinct from every constant the code names.
// ---------------------------------------------------------------------------

const (
	hostOffHost  = "host-bravo"
	hostOffOther = "host-charlie"
	hostOffRepo  = "example-org/example-site"
	hostOffSess  = "7d1e2f30-4a5b-4c6d-8e9f-a0b1c2d3e4f5"
	hostOffOwner = int64(1)
)

// hostOfficerMirror is the hub, played: hosts for owner 1, a recording
// SpawnAgent, CloseAgent and PasteWindow, and an AgentState per window.
type hostOfficerMirror struct {
	Mirror
	// owner is the user whose hosts these are.
	owner  int64
	mu     sync.Mutex
	hosts  []mirror.HostView
	spawns []spawnCall
	closes []mirror.AgentCloseReq
	pastes []spawnPaste
	// state is AgentState's answer per window id; missing = unknown.
	state map[string]mirror.AgentState
	// spawnErr, when set, is SpawnAgent's answer instead of a window.
	spawnErr error
	// spawned is signalled after every SpawnAgent returns.
	spawned chan struct{}
}

func newHostOfficerMirror() *hostOfficerMirror { return newHostOfficerMirrorFor(hostOffOwner) }

// newHostOfficerMirrorFor is the fake with its hosts owned by owner.
func newHostOfficerMirrorFor(owner int64) *hostOfficerMirror {
	v := func(name string) mirror.HostView {
		h := spawnableHost(name, true)
		h.AgentClose = true
		return h
	}
	return &hostOfficerMirror{owner: owner, hosts: []mirror.HostView{v(hostOffOther), v(hostOffHost)}, state: map[string]mirror.AgentState{},
		spawned: make(chan struct{}, 16)}
}

func (m *hostOfficerMirror) Hosts(ctx context.Context) []mirror.HostView {
	if p, ok := authz.From(ctx); !ok || p.UserID != m.owner {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]mirror.HostView, len(m.hosts))
	copy(out, m.hosts)
	return out
}

func (m *hostOfficerMirror) setHost(name string, f func(*mirror.HostView)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.hosts {
		if m.hosts[i].Host == name {
			f(&m.hosts[i])
		}
	}
}

func (m *hostOfficerMirror) SpawnAgent(_ context.Context, host string, dispatch int64, req mirror.AgentSpawnReq) (mirror.AgentSpawned, error) {
	defer func() { m.spawned <- struct{}{} }()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spawns = append(m.spawns, spawnCall{host, dispatch, req})
	if m.spawnErr != nil {
		return mirror.AgentSpawned{}, m.spawnErr
	}
	return mirror.AgentSpawned{Session: mirror.AgentSpawnSession, Window: "@" + strconv.FormatInt(dispatch+7, 10), AgentSession: hostOffSess}, nil
}

func (m *hostOfficerMirror) CloseAgent(_ context.Context, host string, req mirror.AgentCloseReq) (mirror.AgentCloseResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closes = append(m.closes, req)
	return mirror.AgentCloseResult{Graceful: true}, nil
}

func (m *hostOfficerMirror) PasteWindow(_ context.Context, host, session, window, text string, _ mirror.PasteTarget, opts ...mirror.PasteOption) error {
	o := mirror.PasteOptionsOf(opts...)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pastes = append(m.pastes, spawnPaste{host, session, window, text, o.Dispatch})
	return nil
}

func (m *hostOfficerMirror) AgentState(_ context.Context, host, session, window string) mirror.AgentState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st, ok := m.state[host+"/"+window]; ok {
		return st
	}
	return mirror.AgentState{Unknown: errors.New("unknown")}
}

// list makes host list exactly windows.
func (m *hostOfficerMirror) list(host string, windows ...mirror.Window) {
	m.setHost(host, func(v *mirror.HostView) { v.Windows = windows })
}

func (m *hostOfficerMirror) counts() (spawns, closes, pastes int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.spawns), len(m.closes), len(m.pastes)
}

type hostOfficerRig struct {
	s      *Server
	m      *hostOfficerMirror
	hq     *hqtest.Server
	spawns *officers.MemHostSpawnStore
	rec    *officers.Reconciler
	store  *officerAgents
	prov   *officerProv
	logs   *bytes.Buffer
	pod    officers.Officer
	host   officers.Officer

	clockMu sync.Mutex
	clock   time.Time
}

func (r *hostOfficerRig) now() time.Time {
	r.clockMu.Lock()
	defer r.clockMu.Unlock()
	return r.clock
}

func (r *hostOfficerRig) advance(d time.Duration) {
	r.clockMu.Lock()
	r.clock = r.clock.Add(d)
	r.clockMu.Unlock()
}

// hostOfficer is officer B as a host officer on hostOffHost.
func hostOfficer(hqURL string) officers.Officer {
	return officers.Officer{ID: offB, HQURL: hqURL, MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: time.Minute, Owner: hostOffOwner,
		Runtime: officers.RuntimeHost, Host: hostOffHost, Repo: hostOffRepo}.WithToken(offBTok)
}

// newHostOfficerRig is the server with officer A in pods (withPod) and officer
// B on a host. Without withPod the roster is B alone and the server has NO
// provisioner, gateway or kinds — only the agents store: the Compose stack's
// shape.
func newHostOfficerRig(t *testing.T, withPod bool) *hostOfficerRig {
	t.Helper()
	hq := hqtest.New(map[string]string{offATok: offA, offBTok: offB})
	t.Cleanup(hq.Close)
	logs := &bytes.Buffer{}
	r := &hostOfficerRig{m: newHostOfficerMirror(), hq: hq, spawns: officers.NewMemHostSpawnStore(), logs: logs, clock: time.Now()}
	r.spawns.Now = r.now
	hq.Now = r.now
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(logs, "", 0))
	r.store = &officerAgents{}
	ext := Extensions{Mirror: r.m, SessionLiveness: stubLiveness{}, Agents: r.store}
	r.host = hostOfficer(hq.BaseURL())
	list := []officers.Officer{r.host}
	if withPod {
		r.prov = &officerProv{store: r.store, phase: map[string]provision.Phase{}}
		ext.Provisioner, ext.Gateway, ext.Kinds = r.prov, &repromptChat{}, stubKinds{pick: "acct-one"}
		r.pod = officers.Officer{ID: offA, HQURL: hq.BaseURL(), MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: time.Minute, Owner: 1}.WithToken(offATok)
		list = append([]officers.Officer{r.pod}, list...)
	}
	s.UseExtensions(ext)
	ocfg := OfficersConfig{Officers: list, Now: r.now, HostSpawns: r.spawns}
	if withPod {
		ocfg.InstallSession = discardSession
	}
	rec, err := s.EnableOfficers(ocfg)
	if err != nil {
		t.Fatal(err)
	}
	r.s, r.rec = s, rec
	return r
}

// waitSpawned waits for the background spawn to have returned and its reply
// to have been applied (the row names its window or its refusal).
func (r *hostOfficerRig) waitSpawned(t *testing.T) officers.HostSpawn {
	t.Helper()
	select {
	case <-r.m.spawned:
	case <-time.After(5 * time.Second):
		t.Fatal("no spawn reached the host")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		all := r.spawns.All()
		if len(all) > 0 && (all[len(all)-1].Window != "" || all[len(all)-1].Refusal != "") {
			return all[len(all)-1]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the spawn's reply was never recorded")
	return officers.HostSpawn{}
}

func (r *hostOfficerRig) waitCloses(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, c, _ := r.m.counts(); c >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("want %d close(s) of an officer window", n)
}

// listed is the window the host lists for spawn h: its recorded id, its tag,
// Claude Code running the recorded session.
func listed(h officers.HostSpawn) mirror.Window {
	return mirror.Window{ID: h.Window, Session: mirror.AgentSpawnSession, Dispatch: h.ID, Claude: true, ClaudeSession: h.AgentSession}
}

// TestAHostOfficerWakesOnAQueueItemAndScalesToZero: the whole life cycle over a
// host-spawn fake — an item starts instance 1 on ITS host (one spawn, the
// officer preface, its repo, the spawn row's id on the wire); the in-flight
// row counts up so a second pass starts nothing; the recorded window keeps it
// up; idle past its window closes exactly that window in that conversation.
func TestAHostOfficerWakesOnAQueueItemAndScalesToZero(t *testing.T) {
	r := newHostOfficerRig(t, false)
	ctx := context.Background()
	item := r.hq.AddItem(offB, "triage.look", "svc:host-1")

	if res := r.rec.Check(ctx, offB, true); res.Err != nil || len(res.Decision.Start) != 1 || res.Decision.Start[0] != 1 {
		t.Fatalf("first pass = %+v, want Start [1]", res)
	}
	h := r.waitSpawned(t)
	r.m.mu.Lock()
	call := r.m.spawns[0]
	r.m.mu.Unlock()
	if call.host != hostOffHost {
		t.Errorf("spawn went to host %q, want %q", call.host, hostOffHost)
	}
	if call.dispatch != h.ID || h.Instance != officers.InstanceName(offB, 1) || h.Officer != offB {
		t.Errorf("spawn dispatch %d, row %+v: want the row's id, instance 1 of %s", call.dispatch, h, offB)
	}
	if call.req.Runtime != mirror.SpawnRuntimeClaude || call.req.Repo != hostOffRepo || call.req.Model != "" {
		t.Errorf("spawn request = %+v, want claude in %s with no model", call.req, hostOffRepo)
	}
	if !strings.HasPrefix(call.req.Prompt, mirror.OfficerSpawnPreface) || !mirror.HasSpawnPreface(call.req.Prompt) {
		t.Errorf("prompt does not begin with the officer preface: %.80q", call.req.Prompt)
	}
	if !strings.Contains(call.req.Prompt, "You are officer "+offB) || !strings.Contains(call.req.Prompt, "worktree of "+hostOffRepo) {
		t.Errorf("prompt is not the host kickoff: %.300q", call.req.Prompt)
	}
	if strings.Contains(call.req.Prompt, offBTok) {
		t.Fatal("the officer's token reached the spawn prompt")
	}
	if h.Window != "@"+strconv.FormatInt(h.ID+7, 10) || h.AgentSession != hostOffSess {
		t.Errorf("recorded window %q session %q, want the host's reply", h.Window, h.AgentSession)
	}

	// Listed: up, nothing more started, nothing stopped.
	r.m.list(hostOffHost, listed(h))
	if res := r.rec.Check(ctx, offB, true); res.Err != nil || len(res.Decision.Start) != 0 || len(res.Decision.Stop) != 0 {
		t.Fatalf("pass with the window listed = %+v, want nothing started or stopped", res)
	}
	if n, _, _ := r.m.counts(); n != 1 {
		t.Fatalf("%d spawns, want 1", n)
	}

	// The work ends; idle; past the idle window the instance is closed.
	r.hq.Complete(item)
	r.rec.Check(ctx, offB, true) // first idle pass: idleSince
	r.advance(31 * time.Minute)
	if res := r.rec.Check(ctx, offB, true); len(res.Decision.Stop) != 1 || res.Decision.Stop[0] != 1 {
		t.Fatalf("idle pass = %+v, want Stop [1]", res)
	}
	r.waitCloses(t, 1)
	r.m.mu.Lock()
	cl := r.m.closes[0]
	r.m.mu.Unlock()
	if cl != (mirror.AgentCloseReq{Session: mirror.AgentSpawnSession, Window: h.Window, Key: hostOffSess}) {
		t.Errorf("close = %+v, want the recorded window and conversation", cl)
	}
}

// TestAnInFlightHostSpawnCountsUpThenDown: a spawn whose reply never came is
// up inside the pending countdown (no second window), and down after it (the
// loop may start again).
func TestAnInFlightHostSpawnCountsUpThenDown(t *testing.T) {
	r := newHostOfficerRig(t, false)
	ctx := context.Background()
	r.hq.AddItem(offB, "triage.look", "svc:host-2")
	row, err := r.spawns.InsertHostSpawn(ctx, offB, officers.InstanceName(offB, 1), hostOffHost)
	if err != nil {
		t.Fatal(err)
	}
	r.advance(hostPendingCountdown - time.Second)
	if res := r.rec.Check(ctx, offB, true); res.Err != nil || len(res.Decision.Start) != 0 {
		t.Fatalf("pass inside the countdown = %+v, want no start (spawn %d is in flight)", res, row.ID)
	}
	r.advance(2 * time.Second)
	if res := r.rec.Check(ctx, offB, true); len(res.Decision.Start) != 1 {
		t.Fatalf("pass past the countdown = %+v, want Start [1]", res)
	}
	r.waitSpawned(t)
}

// TestAHostThatCannotBeSeenChangesNothing: host offline is a fleet that cannot
// be seen — the pass errors, starts nothing; a host that does not close
// windows is never started on; a refused spawn is recorded and reads down.
func TestAHostThatCannotBeSeenChangesNothing(t *testing.T) {
	r := newHostOfficerRig(t, false)
	ctx := context.Background()
	r.hq.AddItem(offB, "triage.look", "svc:host-3")

	r.m.setHost(hostOffHost, func(v *mirror.HostView) { v.Online = false })
	if res := r.rec.Check(ctx, offB, true); res.Err == nil || !strings.Contains(res.Err.Error(), "is not connected") {
		t.Fatalf("pass with the host offline = %+v, want the cannot-see error", res)
	}
	r.m.setHost(hostOffHost, func(v *mirror.HostView) { v.Online, v.AgentClose = true, false })
	res := r.rec.Check(ctx, offB, true)
	if len(res.ActErrs) != 1 || !strings.Contains(res.ActErrs[0].Error(), "does not close windows") {
		t.Fatalf("pass with a host that cannot close = %+v, want the refusal to start", res)
	}
	if n, _, _ := r.m.counts(); n != 0 || len(r.spawns.All()) != 0 {
		t.Fatalf("%d spawns, %d rows, want none", n, len(r.spawns.All()))
	}

	r.m.setHost(hostOffHost, func(v *mirror.HostView) { v.AgentClose = true })
	r.m.spawnErr = &mirror.SpawnRefusedError{Code: mirror.SpawnNoRepo, Detail: "not on this host's allow-list"}
	r.rec.Check(ctx, offB, true)
	h := r.waitSpawned(t)
	if h.Refusal != "no_repo: not on this host's allow-list" || h.Window != "" {
		t.Fatalf("refused spawn recorded as %+v", h)
	}
	insts, err := officerFleet{s: r.s}.Instances(ctx, r.host)
	if err != nil || insts[0].Up {
		t.Fatalf("a refused spawn reads %+v (%v), want down", insts, err)
	}
}

// TestAHostInstanceIsRepromptedOnlyWhenIdle: a new turn is a paste stamped
// with the spawn's id into the recorded window, only while the host reports
// that window idle in the recorded conversation.
func TestAHostInstanceIsRepromptedOnlyWhenIdle(t *testing.T) {
	r := newHostOfficerRig(t, false)
	ctx := context.Background()
	h, _ := r.spawns.InsertHostSpawn(ctx, offB, officers.InstanceName(offB, 1), hostOffHost)
	if _, err := r.spawns.RecordHostSpawnWindow(ctx, h.ID, hostOffHost, "@91", hostOffSess); err != nil {
		t.Fatal(err)
	}
	f := officerFleet{s: r.s}
	for _, st := range []mirror.AgentState{
		{Status: mirror.StatusBusy, Session: hostOffSess},
		{Status: mirror.StatusIdle, Session: "11111111-2222-4333-8444-555555555555"},
		{Unknown: errors.New("no status")},
	} {
		r.m.state[hostOffHost+"/@91"] = st
		if err := f.Reprompt(ctx, r.host, 1, "next item"); !errors.Is(err, officers.ErrInstanceBusy) {
			t.Errorf("state %+v: Reprompt = %v, want busy", st, err)
		}
	}
	r.m.state[hostOffHost+"/@91"] = mirror.AgentState{Status: mirror.StatusIdle, Session: hostOffSess}
	if err := f.Reprompt(ctx, r.host, 1, "next item"); err != nil {
		t.Fatal(err)
	}
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if len(r.m.pastes) != 1 || r.m.pastes[0] != (spawnPaste{hostOffHost, mirror.AgentSpawnSession, "@91", "next item", h.ID}) {
		t.Fatalf("pastes = %+v, want one into @91 stamped with spawn %d", r.m.pastes, h.ID)
	}
}

// TestTheHostOfficerToolsAreBoundToTheRecordedWindow: the mirror door serves an
// officer tool to the session muster recorded for the spawn, in its recorded
// window, AS THE INSTANCE'S AGENT ROW — and on every call refuses the same
// session in another window, another session, a row on a host the officer is
// not declared on, and an opencode caller; the agent door refuses a
// provisioned row named like the host instance (an officer that moved off
// pods).
func TestTheHostOfficerToolsAreBoundToTheRecordedWindow(t *testing.T) {
	r := newHostOfficerRig(t, false)
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: hostOffOwner, Role: authz.RoleAdmin, Via: authz.ViaMirrorHost})
	row, err := r.s.ensureHostInstanceRow(ctx, r.host, 1)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := r.spawns.InsertHostSpawn(ctx, offB, officers.InstanceName(offB, 1), hostOffHost)
	_, _ = r.spawns.RecordHostSpawnWindow(ctx, h.ID, hostOffHost, "@91", hostOffSess)
	r.hq.Charters[offB] = "# Triage\nLook at each one."
	caller := func(sid, window string) toolCaller {
		return toolCaller{asker: attention.Asker{MirrorHost: hostOffHost, MirrorSession: sid}, mirrorWindow: window}
	}
	call := func(host string, c toolCaller, tool, args string) string {
		out, ok := r.s.hostOfficerMirrorTool(ctx, host, c, tool, args)
		if !ok {
			t.Fatalf("%s was not routed by the host officer rule", tool)
		}
		return out
	}

	if out := call(hostOffHost, caller(hostOffSess, "@91"), officers.ToolBrief, ``); isToolErr(out) || !strings.Contains(out, "Triage") {
		t.Fatalf("the instance's own call = %s, want its brief", out)
	}
	if a, ok := r.s.officerHostCaller(ctx, hostOffHost, hostOffSess, "@91"); !ok || a.ID != row.ID || a.RunsOnHost != hostOffHost {
		t.Fatalf("the binding = %+v %v, want the instance's row %d", a, ok, row.ID)
	}
	for name, c := range map[string]toolCaller{
		"another window":  caller(hostOffSess, "@92"),
		"another session": caller("22222222-3333-4444-8555-666666666666", "@91"),
		"no window":       caller(hostOffSess, ""),
	} {
		if out := call(hostOffHost, c, officers.ToolBrief, ``); !isToolErr(out) || !strings.Contains(out, "is an officer tool") {
			t.Errorf("%s: %s, want the binding refusal", name, out)
		}
	}
	// The same session and window, reported on ANOTHER host: not bound.
	if out := call(hostOffOther, caller(hostOffSess, "@91"), officers.ToolBrief, ``); !isToolErr(out) {
		t.Errorf("another host: %s, want refused", out)
	}
	// A row on ANOTHER host, called from that host in its own recorded window:
	// the officer is declared on hostOffHost, so the row binds nothing (an
	// officer moved between hosts leaves such rows).
	other, _ := r.spawns.InsertHostSpawn(ctx, offB, officers.InstanceName(offB, 1), hostOffOther)
	_, _ = r.spawns.RecordHostSpawnWindow(ctx, other.ID, hostOffOther, "@93", "33333333-4444-4555-8666-777777777777")
	if out := call(hostOffOther, caller("33333333-4444-4555-8666-777777777777", "@93"), officers.ToolBrief, ``); !isToolErr(out) {
		t.Errorf("a row on a host the officer is not declared on: %s, want refused", out)
	}
	oc := caller("opencode:ses_x", "@91")
	if out := call(hostOffHost, oc, officers.ToolBrief, ``); !isToolErr(out) || !strings.Contains(out, "opencode") {
		t.Errorf("opencode caller: %s, want refused", out)
	}
	// A tool a host officer is not offered, from the instance itself.
	if out := call(hostOffHost, caller(hostOffSess, "@91"), officers.ToolObsQuery, `{}`); !isToolErr(out) || !strings.Contains(out, "is not one of officer") {
		t.Errorf("obs from a host instance: %s, want not offered", out)
	}
	// The row the binding resolves must be this host's, never a provisioned
	// one: the same recorded session and window over a row that reads as a
	// pod's is refused.
	r.store.mu.Lock()
	for i := range r.store.rows {
		if r.store.rows[i].ID == row.ID {
			r.store.rows[i].RunsOnHost = ""
		}
	}
	r.store.mu.Unlock()
	if out := call(hostOffHost, caller(hostOffSess, "@91"), officers.ToolBrief, ``); !isToolErr(out) || !strings.Contains(out, "is an officer tool") {
		t.Errorf("the recorded window over a provisioned row: %s, want the binding refusal", out)
	}
	// The agent door: a PROVISIONED row with the instance's name reaches nothing.
	pod := agents.Agent{ID: 931, Name: officers.InstanceName(offB, 1), OwnerID: hostOffOwner}
	if out, _ := r.s.dispatchOfficerTool(ctx, pod, officers.ToolBrief, ``); !isToolErr(out) || !strings.Contains(out, "served to its host instances only") {
		t.Errorf("agent door, a provisioned row named like the host instance: %s, want the runtime refusal", out)
	}
}

// TestAHostInstanceIsNotItsOwnersSession: a call bound to a host officer
// instance runs only the instance's tools — the operator's tools on the mirror
// door (deciding a draft or a connector request, answering an ask, destroying
// an agent) are refused, so an instance cannot approve its own outbound draft
// or connector request through muster. An UNBOUND session's call to an
// instance-only tool is refused too, and its own tools are left to the door.
func TestAHostInstanceIsNotItsOwnersSession(t *testing.T) {
	r := newHostOfficerRig(t, false)
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: hostOffOwner, Role: authz.RoleAdmin, Via: authz.ViaMirrorHost})
	if _, err := r.s.ensureHostInstanceRow(ctx, r.host, 1); err != nil {
		t.Fatal(err)
	}
	h, _ := r.spawns.InsertHostSpawn(ctx, offB, officers.InstanceName(offB, 1), hostOffHost)
	_, _ = r.spawns.RecordHostSpawnWindow(ctx, h.ID, hostOffHost, "@91", hostOffSess)
	inst := toolCaller{asker: attention.Asker{MirrorHost: hostOffHost, MirrorSession: hostOffSess}, mirrorWindow: "@91"}
	for _, tool := range []string{"plugin_draft_decide", "connector_request_decide", attentionAnswerTool, agentDestroyTool, "dispatch_approve", chiefAskTool} {
		out, ok := r.s.hostOfficerMirrorTool(ctx, hostOffHost, inst, tool, `{}`)
		if !ok || !isToolErr(out) || !strings.Contains(out, "not one of your owner's sessions") {
			t.Errorf("%s from the instance: %v %s, want refused", tool, ok, out)
		}
	}
	user := toolCaller{asker: attention.Asker{MirrorHost: hostOffHost, MirrorSession: "44444444-5555-4666-8777-888888888888"}, mirrorWindow: "@7"}
	if out, ok := r.s.hostOfficerMirrorTool(ctx, hostOffHost, user, "connector_request", `{"name":"pg-example"}`); !ok || !isToolErr(out) {
		t.Errorf("an unbound session's connector_request: %v %s, want refused", ok, out)
	}
	if _, ok := r.s.hostOfficerMirrorTool(ctx, hostOffHost, user, "plugin_draft_decide", `{}`); ok {
		t.Error("an unbound session's own operator tool was taken by the host officer rule")
	}
}

// TestAHostInstancesAgentRowIsMadeOnceAndKept: the first start creates the
// instance's row — stopped, never provisioned (RunsOnHost its host, no
// namespace or account), the officer's owner's; scale-to-zero keeps it; the
// next start reuses it. A provisioned row of that name (left from pods) is
// refused, never adopted.
func TestAHostInstancesAgentRowIsMadeOnceAndKept(t *testing.T) {
	r := newHostOfficerRig(t, false)
	ctx := context.Background()
	first := r.hq.AddItem(offB, "triage.look", "svc:row-1")
	r.rec.Check(ctx, offB, true)
	h := r.waitSpawned(t)
	r.store.mu.Lock()
	created := append([]agents.Agent(nil), r.store.created...)
	r.store.mu.Unlock()
	if len(created) != 1 {
		t.Fatalf("%d agent rows created, want 1", len(created))
	}
	a := created[0]
	if a.Name != officers.InstanceName(offB, 1) || a.RunsOnHost != hostOffHost || a.Provisioned() || a.Status != agents.StatusStopped ||
		a.Namespace != "" || a.CCAccount != "" || agents.ResolveKind(a.Kind) != agents.KindGateway || a.OwnerID != hostOffOwner {
		t.Fatalf("the instance's row = %+v", a)
	}
	// Scale to zero, then wake again: the same row.
	r.m.list(hostOffHost, listed(h))
	r.hq.Complete(first)
	r.rec.Check(ctx, offB, true)
	r.advance(31 * time.Minute)
	r.rec.Check(ctx, offB, true)
	r.waitCloses(t, 1)
	r.m.list(hostOffHost)
	if _, err := r.store.GetByName(ctx, a.Name); err != nil {
		t.Fatalf("scale-to-zero removed the row: %v", err)
	}
	r.hq.AddItem(offB, "triage.look", "svc:row-2")
	r.rec.Check(ctx, offB, true)
	r.waitSpawned(t)
	r.store.mu.Lock()
	n := len(r.store.created)
	r.store.mu.Unlock()
	if n != 1 {
		t.Fatalf("a second start created another row (%d)", n)
	}

	// A provisioned row of the instance's name is refused, naming it.
	r2 := newHostOfficerRig(t, false)
	r2.store.rows = append(r2.store.rows, agents.Agent{ID: 77, Name: officers.InstanceName(offB, 1), Namespace: "ns-x", OwnerID: hostOffOwner})
	r2.hq.AddItem(offB, "triage.look", "svc:row-3")
	res := r2.rec.Check(ctx, offB, true)
	if len(res.ActErrs) != 1 || !strings.Contains(res.ActErrs[0].Error(), "exists as a pod instance's") {
		t.Fatalf("start over a pod row = %+v, want the refusal naming it", res)
	}
	if n, _, _ := r2.m.counts(); n != 0 {
		t.Fatal("a spawn was sent over a pod row")
	}
}

// TestALateSpawnReplyIsRecordedOnItsOwnHostOnly: a reply that arrives after
// the 150 s wait reaches the hub's late-reply hook (HostDispatchSpawnReply),
// which records an officer spawn's window — and not from another host's
// connection.
func TestALateSpawnReplyIsRecordedOnItsOwnHostOnly(t *testing.T) {
	r := newHostOfficerRig(t, false)
	ctx := context.Background()
	h, _ := r.spawns.InsertHostSpawn(ctx, offB, officers.InstanceName(offB, 1), hostOffHost)
	reply := mirror.Msg{Type: mirror.TypeAgentSpawned, Dispatch: h.ID,
		Spawned: &mirror.AgentSpawned{Session: mirror.AgentSpawnSession, Window: "@57", AgentSession: hostOffSess}}
	r.s.HostDispatchSpawnReply(hostOffOwner, hostOffOther, reply)
	if got, _ := r.spawns.HostSpawnByID(ctx, h.ID); got.Window != "" {
		t.Fatalf("a reply on host %s recorded spawn %d's window %q", hostOffOther, h.ID, got.Window)
	}
	r.s.HostDispatchSpawnReply(hostOffOwner, hostOffHost, reply)
	if got, _ := r.spawns.HostSpawnByID(ctx, h.ID); got.Window != "@57" || got.AgentSession != hostOffSess {
		t.Fatalf("the late reply recorded %+v, want window @57", got)
	}
}

// TestTheMirrorDoorListsTheHostOfficerToolsOnlyWithAHostOfficer.
func TestTheMirrorDoorListsTheHostOfficerToolsOnlyWithAHostOfficer(t *testing.T) {
	names := func(s *Server) map[string]bool {
		out := map[string]bool{}
		for _, d := range s.mirrorTools() {
			out[d.Name] = true
		}
		return out
	}
	r := newHostOfficerRig(t, false)
	got := names(r.s)
	for _, want := range []string{officers.ToolBrief, officers.ToolItems, officers.ToolClaim, officers.ToolHeartbeat, officers.ToolComplete, officers.ToolRelease, officers.ToolAskPerson, officers.ToolPostDraft} {
		if !got[want] {
			t.Errorf("mirror door lacks %s with a host officer declared", want)
		}
	}
	for _, not := range []string{officers.ToolObsQuery, officers.ToolKubeRead, officers.ToolMergePR} {
		if got[not] {
			t.Errorf("mirror door lists %s, which a host officer is not offered", not)
		}
	}
	// A pod-only roster lists none.
	pod := newOfficerRig(t)
	pod.s.ext.Mirror = newHostOfficerMirror()
	for n := range names(pod.s) {
		if isOfficerTool(n) {
			t.Errorf("a pod-only roster lists %s on the mirror door", n)
		}
	}
}

// TestEnableOfficersRefusesByRuntime: an all-host roster boots with no agents
// store, provisioner or kinds (the Compose stack); a pod officer beside it
// still needs them, named; a host officer needs a mirror hub and the spawn
// store and the agents store (its instances' rows), each named.
func TestEnableOfficersRefusesByRuntime(t *testing.T) {
	hq := hqtest.New(map[string]string{offATok: offA, offBTok: offB})
	t.Cleanup(hq.Close)
	host := hostOfficer(hq.BaseURL())
	pod := officers.Officer{ID: offA, HQURL: hq.BaseURL(), MaxInstances: 1, IdleAfter: time.Hour, PollInterval: time.Minute, Owner: 1}.WithToken(offATok)
	srv := func(m Mirror) *Server {
		s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
		s.UseExtensions(Extensions{Mirror: m, SessionLiveness: stubLiveness{}, Agents: &officerAgents{}})
		return s
	}
	noAgents := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
	noAgents.UseExtensions(Extensions{Mirror: newHostOfficerMirror(), SessionLiveness: stubLiveness{}})
	if _, err := srv(newHostOfficerMirror()).EnableOfficers(OfficersConfig{Officers: []officers.Officer{host}, HostSpawns: officers.NewMemHostSpawnStore()}); err != nil {
		t.Fatalf("an all-host roster with no provisioner: %v, want it to boot", err)
	}
	for _, c := range []struct {
		name string
		s    *Server
		cfg  OfficersConfig
		want string
	}{
		{"pod beside host, no provisioner", srv(newHostOfficerMirror()), OfficersConfig{Officers: []officers.Officer{pod, host}, HostSpawns: officers.NewMemHostSpawnStore()},
			offA + " run in pods, which need the agents store and an agent provisioner"},
		{"host, no mirror", srv(nil), OfficersConfig{Officers: []officers.Officer{host}, HostSpawns: officers.NewMemHostSpawnStore()},
			offB + ` run on hosts ("runtime": "host"), and this server has no mirror hub`},
		{"host, no spawn store", srv(newHostOfficerMirror()), OfficersConfig{Officers: []officers.Officer{host}},
			offB + " runs on a host, and this server keeps no host spawns"},
		{"host, no agents store", noAgents, OfficersConfig{Officers: []officers.Officer{host}, HostSpawns: officers.NewMemHostSpawnStore()},
			offB + " run on hosts, and each host instance has an agent row"},
	} {
		if _, err := c.s.EnableOfficers(c.cfg); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

// TestBothRuntimesShareTheOfficerPolicy is the SEAM: one Reconciler, one
// officerFleet, both runtimes. The same queue state makes the same decision
// for a pod officer and a host officer (Decide is the loop's alone), and each
// is MOVED by its own runtime — the pod by the provisioner, the host by a
// spawn — never the other's.
func TestBothRuntimesShareTheOfficerPolicy(t *testing.T) {
	r := newHostOfficerRig(t, true)
	ctx := context.Background()
	r.hq.AddItem(offA, "release.evaluate", "svc:pod-1")
	r.hq.AddItem(offB, "triage.look", "svc:host-4")
	pa := r.rec.Check(ctx, offA, true)
	pb := r.rec.Check(ctx, offB, true)
	if pa.Err != nil || pb.Err != nil {
		t.Fatalf("passes: %v / %v", pa.Err, pb.Err)
	}
	if len(pa.Decision.Start) != 1 || len(pb.Decision.Start) != 1 || pa.Decision.Claimable != pb.Decision.Claimable {
		t.Fatalf("decisions differ for the same queue: pod %+v, host %+v", pa.Decision, pb.Decision)
	}
	r.waitSpawned(t)
	d, starts, _ := r.prov.counts()
	n, _, _ := r.m.counts()
	if d+starts != 1 || n != 1 {
		t.Fatalf("pod moves %d, host spawns %d: want one each", d+starts, n)
	}
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if r.m.spawns[0].req.Name != officers.InstanceName(offB, 1) {
		t.Errorf("the spawn was for %q, want the host officer's instance", r.m.spawns[0].req.Name)
	}
	// Each instance has a row; only the pod's is provisioned, and the
	// provisioner was asked about the pod's row alone.
	rows := map[string]agents.Agent{}
	for _, a := range r.store.created {
		rows[a.Name] = a
	}
	podRow, hostRow := rows[officers.InstanceName(offA, 1)], rows[officers.InstanceName(offB, 1)]
	if !podRow.Provisioned() || hostRow.Provisioned() || hostRow.RunsOnHost != hostOffHost || len(rows) != 2 {
		t.Fatalf("rows = %+v, want a provisioned pod row and a host row on %s", rows, hostOffHost)
	}
	r.prov.mu.Lock()
	moved := append(append([]int64(nil), r.prov.dispatch...), r.prov.starts...)
	r.prov.mu.Unlock()
	for _, id := range moved {
		if id != podRow.ID {
			t.Errorf("the provisioner was asked to move agent %d, want only the pod row %d", id, podRow.ID)
		}
	}
}

// TestTheHostOfficerSeamsAreALedger: the host fleet is reached only through
// officerFleet's four methods; the mirror door's officer binding only from
// handleMirrorMCP; dispatchOfficerTool from exactly the agent door and that
// binding; the spawn store's writers each from their one caller; the one
// Reconciler built only by EnableOfficers.
func TestTheHostOfficerSeamsAreALedger(t *testing.T) {
	assertCallers(t, ".", "hostInstances", []string{"officers.go:Instances"})
	assertCallers(t, ".", "hostStart", []string{"officers.go:Start"})
	assertCallers(t, ".", "hostReprompt", []string{"officers.go:Reprompt"})
	assertCallers(t, ".", "hostStop", []string{"officers.go:Stop"})
	assertCallers(t, ".", "hostOfficerMirrorTool", []string{"mirror_mcp.go:handleMirrorMCP"})
	assertCallers(t, ".", "officerHostCaller", []string{"officer_host.go:hostOfficerMirrorTool"})
	assertCallers(t, ".", "toolHostOfficerInstance", []string{"officer_host.go:hostOfficerMirrorTool"})
	assertCallers(t, ".", "dispatchOfficerTool", []string{"agent.go:dispatchAgentTool", "officer_host.go:toolHostOfficerInstance"})
	assertCallers(t, ".", "ensureHostInstanceRow", []string{"officer_host.go:hostStart"})
	assertCallers(t, ".", "InsertHostSpawn", []string{"officer_host.go:hostStart"})
	assertCallers(t, ".", "RecordHostSpawnWindow", []string{"officer_host.go:applyOfficerSpawnReply"})
	assertCallers(t, ".", "RecordHostSpawnRefusal", []string{"officer_host.go:applyOfficerSpawnReply"})
	assertCallers(t, ".", "applyOfficerSpawnReply", []string{"host_dispatch_reconcile.go:HostDispatchSpawnReply", "officer_host.go:hostStart"})
	assertCallers(t, ".", "NewReconciler", []string{"officers.go:EnableOfficers"})
	assertCallers(t, ".", "hostSpawnUp", []string{"officer_host.go:hostInstances"})
}
