package api

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// OFFICERS WORK ON COMPOSE (officers redesign O-B), muster-server's half:
// the host's readiness read by auto placement, the start and doctor; a
// scratch spawn; every start's reason; the per-officer spawn budget; the
// unclaimed-instance notice; and no builtin officer text saying HQ. Over the
// host officer rig (officer_host_test.go): hosts host-bravo and host-charlie
// of owner 1, officer B on host-bravo.
// ---------------------------------------------------------------------------

// readyHost is a host's readiness: claude found and logged in, repos on its
// allow-list, scratch spawns as given.
func readyHost(scratch bool, repos ...string) *mirror.SpawnReadiness {
	if repos == nil {
		repos = []string{}
	}
	return &mirror.SpawnReadiness{Repos: repos, Scratch: scratch, Claude: mirror.ClaudeFound, Login: mirror.LoginYes}
}

// owned is a context acting as the rig's owner.
func owned(s *Server) context.Context { return s.officerPrincipal(context.Background(), hostOffOwner) }

// TestAutoPlacementReadsTheHostsReadiness: a "runtime": "auto" officer is
// placed on a host whose readiness says it can start it — never one whose
// Claude Code is not logged in, whose claude is not found, or whose
// allow-list lacks the repo (repo_allowed says so per host); with no repo,
// only a host offering scratch spawns; and a host that sent no readiness (an
// older muster-mirror) qualifies as before. REGRESSION: at origin/main auto
// took the first spawnable host, logged in or not.
func TestAutoPlacementReadsTheHostsReadiness(t *testing.T) {
	r := newHostOfficerRig(t, false)
	set := func(host string, rd *mirror.SpawnReadiness) { r.m.setHost(host, func(v *mirror.HostView) { v.SpawnReady = rd }) }
	opt := func(o runtimeOptions, host string) hostOption {
		for _, h := range o.Hosts {
			if h.Host == host {
				return h
			}
		}
		t.Fatalf("no option for %s in %+v", host, o.Hosts)
		return hostOption{}
	}
	ctx := owned(r.s)

	// bravo sorts first, but its Claude Code is not logged in: charlie.
	notIn := readyHost(false, hostOffRepo)
	notIn.Login = mirror.LoginNo
	set(hostOffHost, notIn)
	set(hostOffOther, readyHost(false, hostOffRepo))
	o := r.s.runtimeOptionsFor(ctx, hostOffOwner, hostOffRepo)
	if o.Auto.Runtime != officers.RuntimeHost || o.Auto.Host != hostOffOther {
		t.Fatalf("auto = %+v, want host %s (bravo is not logged in)", o.Auto, hostOffOther)
	}
	if b := opt(o, hostOffHost); b.Qualifies || !strings.Contains(b.Reason, "not logged in") || b.Login != mirror.LoginNo ||
		b.RepoAllowed == nil || !*b.RepoAllowed {
		t.Fatalf("bravo's option %+v, want it refused for its login, repo allowed", b)
	}

	// charlie's claude is not found now: nowhere qualifies.
	gone := readyHost(false, hostOffRepo)
	gone.Claude, gone.ClaudeDetail = mirror.ClaudeMissing, "claude is not on the tmux server's PATH"
	set(hostOffOther, gone)
	o = r.s.runtimeOptionsFor(ctx, hostOffOwner, hostOffRepo)
	if o.Auto.Host != "" || !strings.Contains(opt(o, hostOffOther).Reason, "claude is not found there") {
		t.Fatalf("auto with no logged-in claude anywhere = %+v (charlie: %+v)", o.Auto, opt(o, hostOffOther))
	}

	// bravo logs in, but the repo is off its allow-list: refused, by name.
	set(hostOffHost, readyHost(false, "example-org/other"))
	o = r.s.runtimeOptionsFor(ctx, hostOffOwner, hostOffRepo)
	if b := opt(o, hostOffHost); b.Qualifies || b.RepoAllowed == nil || *b.RepoAllowed || !strings.Contains(b.Reason, "is not on its -agent-spawn-repos") {
		t.Fatalf("bravo with the repo off its allow-list: %+v", b)
	}

	// No repo: only a host that offers scratch spawns.
	set(hostOffHost, readyHost(true))
	set(hostOffOther, readyHost(false, hostOffRepo))
	o = r.s.runtimeOptionsFor(ctx, hostOffOwner, "")
	if o.Auto.Host != hostOffHost || opt(o, hostOffOther).Qualifies || opt(o, hostOffHost).RepoAllowed != nil {
		t.Fatalf("a scratch officer: auto %+v, charlie %+v", o.Auto, opt(o, hostOffOther))
	}

	// Control: a host that sent no readiness (an older mirror) qualifies for
	// an officer with a repo, as it did before.
	set(hostOffHost, nil)
	set(hostOffOther, nil)
	o = r.s.runtimeOptionsFor(ctx, hostOffOwner, hostOffRepo)
	if o.Auto.Host != hostOffHost || opt(o, hostOffHost).RepoAllowed != nil {
		t.Fatalf("control: hosts with no readiness: auto %+v", o.Auto)
	}
	// ...but not for one with no repo: an older mirror cannot start a scratch spawn.
	if o = r.s.runtimeOptionsFor(ctx, hostOffOwner, ""); o.Auto.Host != "" || !strings.Contains(opt(o, hostOffHost).Reason, "predates officers with no repository") {
		t.Fatalf("a scratch officer on hosts with no readiness: auto %+v, bravo %+v", o.Auto, opt(o, hostOffHost))
	}
}

// TestAHostStartIsRefusedBeforeTheHostWouldBe: a start the host's readiness
// says it would refuse (the repo off its allow-list) and a start past the
// officer's own spawn budget are refused HERE, with the reason, and send the
// host nothing — so neither spends the host's rate. The control: the same
// start with the repo allowed and the budget unspent is sent.
func TestAHostStartIsRefusedBeforeTheHostWouldBe(t *testing.T) {
	r := newHostOfficerRig(t, false)
	f := officerFleet{s: r.s}
	r.m.setHost(hostOffHost, func(v *mirror.HostView) { v.SpawnReady = readyHost(false, "example-org/other") })
	err := f.hostStart(context.Background(), r.host, 1, "kickoff")
	if err == nil || !strings.Contains(err.Error(), "repo "+hostOffRepo+" is not on its -agent-spawn-repos") {
		t.Fatalf("a start off the allow-list: %v", err)
	}
	if n, _, _ := r.m.counts(); n != 0 || len(r.spawns.All()) != 0 {
		t.Fatalf("a refused start sent %d spawn(s) and recorded %d row(s)", n, len(r.spawns.All()))
	}

	// The repo allowed, but the officer's budget spent: 3 starts in the last
	// 10 minutes is its default budget (officers.SpawnBudgetFor).
	r.m.setHost(hostOffHost, func(v *mirror.HostView) { v.SpawnReady = readyHost(false, hostOffRepo) })
	for i := 0; i < 3; i++ {
		if _, err := r.spawns.InsertHostSpawn(context.Background(), offB, officers.InstanceName(offB, 1), hostOffHost); err != nil {
			t.Fatal(err)
		}
	}
	err = f.hostStart(context.Background(), r.host, 1, "kickoff")
	if err == nil || !strings.Contains(err.Error(), "its spawn budget is spent: 3 starts in the last 10 minutes") {
		t.Fatalf("a start past the budget: %v", err)
	}
	if n, _, _ := r.m.counts(); n != 0 {
		t.Fatalf("a start past the budget sent %d spawn(s)", n)
	}

	// Control: 11 minutes on, the short window has room again: it is sent.
	r.advance(11 * time.Minute)
	if err := f.hostStart(context.Background(), r.host, 1, "kickoff"); err != nil {
		t.Fatalf("control: an allowed start within budget: %v", err)
	}
	r.waitSpawned(t)
}

// TestANoRepoHostOfficerAsksForAScratchSpawn (B6): a host officer with no
// repo is started as a SCRATCH spawn — no repo on the wire, Scratch set — on
// a host that offers it, and its first message says where it runs.
func TestANoRepoHostOfficerAsksForAScratchSpawn(t *testing.T) {
	r := newHostOfficerRig(t, false)
	o := r.host
	o.Repo = ""
	r.s.officerRT.reg.Swap([]officers.Officer{o})
	r.m.setHost(hostOffHost, func(v *mirror.HostView) { v.SpawnReady = readyHost(true) })
	r.hq.AddItem(offB, "triage.look", "svc:scratch-1")
	if res := r.rec.Check(context.Background(), offB, true); res.Err != nil || len(res.Decision.Start) != 1 {
		t.Fatalf("pass = %+v, want Start [1]", res)
	}
	r.waitSpawned(t)
	r.m.mu.Lock()
	call := r.m.spawns[0]
	r.m.mu.Unlock()
	if !call.req.Scratch || call.req.Repo != "" || !strings.Contains(call.req.Prompt, "private scratch directory") {
		t.Fatalf("the spawn request %+v (prompt %.200q), want a scratch spawn with no repo", call.req, call.req.Prompt)
	}
	// A host that does not offer scratch spawns (an older muster-mirror: every
	// current one with -agent-spawn does) is refused before the spawn, and
	// told to upgrade — there is no flag to set (O-B round 0, Q1).
	r.m.setHost(hostOffHost, func(v *mirror.HostView) { v.SpawnReady = readyHost(false) })
	if err := (officerFleet{s: r.s}).hostStart(context.Background(), o, 1, "kickoff"); err == nil || !strings.Contains(err.Error(), "does not start an officer with no repository (an older build)") {
		t.Fatalf("a scratch start on a host without scratch spawns: %v", err)
	}
}

// TestTheStartLineSaysWhyAHostInstanceWasDown: a host instance whose recorded
// window is gone is restarted, and the start line says so, naming the window
// and the host. hostDownReason's other readings are pinned beside it.
func TestTheStartLineSaysWhyAHostInstanceWasDown(t *testing.T) {
	r := newHostOfficerRig(t, false)
	ctx := context.Background()
	r.hq.AddItem(offB, "triage.look", "svc:restart-1")
	r.rec.Check(ctx, offB, true)
	h := r.waitSpawned(t)
	if !strings.Contains(r.logs.String(), "started instance 1 — first start (no window was started for it before)") {
		t.Fatalf("the first start does not say so:\n%s", r.logs.String())
	}
	// Its window is never listed: past the countdown it reads gone, and the
	// next start names it.
	r.advance(hostPendingCountdown + time.Second)
	r.logs.Reset()
	r.rec.Check(ctx, offB, true)
	want := "started instance 1 — restart: its window " + h.Window + " on " + hostOffHost + " is gone"
	if !strings.Contains(r.logs.String(), want) {
		t.Fatalf("the restart line %q is missing:\n%s", want, r.logs.String())
	}
	r.waitSpawned(t)

	w := []mirror.Window{{ID: "@9", Session: mirror.AgentSpawnSession, Dispatch: 1001, Claude: true}}
	for _, c := range []struct {
		name string
		r    officers.HostSpawn
		ok   bool
		want string
	}{
		{"no row", officers.HostSpawn{}, false, "first start (no window was started for it before)"},
		{"refused", officers.HostSpawn{ID: 1001, Refusal: "no_repo: repo x is not on this host's allow-list"}, true,
			"restart: its last start (spawn 1001) was refused by host-bravo: no_repo: repo x is not on this host's allow-list"},
		{"never answered", officers.HostSpawn{ID: 1002}, true, "restart: its last start (spawn 1002) got no answer from host-bravo"},
		{"window gone", officers.HostSpawn{ID: 1003, Window: "@7"}, true, "restart: its window @7 on host-bravo is gone (it exited, or was closed or killed outside muster)"},
		{"window listed: not down", officers.HostSpawn{ID: 1001, Window: "@9"}, true, ""},
	} {
		if got := hostDownReason(c.r, c.ok, hostOffHost, w); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// TestDoctorReportsEachOfficerRuntimeProblem: the doctor's officer entry
// names each thing in a host officer's way, with its fix — its host offline,
// starting no agent, not closing windows, claude not found, Claude Code not
// logged in, the repo off its allow-list — and none when it is ready; a host
// that sent no readiness is a note, not a problem; a pod officer on a server
// with no agent plane is a problem of its own.
func TestDoctorReportsEachOfficerRuntimeProblem(t *testing.T) {
	r := newHostOfficerRig(t, false)
	ctx := owned(r.s)
	report := func() DoctorOfficer {
		t.Helper()
		list := r.s.doctorOfficers(ctx, hostOffOwner)
		if len(list) != 1 || list[0].ID != offB || list[0].RunsOn != officers.RuntimeHost || list[0].Host != hostOffHost {
			t.Fatalf("the doctor's officers %+v, want officer B on %s", list, hostOffHost)
		}
		return list[0]
	}
	reset := func() {
		r.m.setHost(hostOffHost, func(v *mirror.HostView) {
			*v = spawnableHost(hostOffHost, true)
			v.AgentClose, v.SpawnReady = true, readyHost(false, hostOffRepo)
		})
	}
	reset()
	if d := report(); len(d.Problems) != 0 || len(d.Notes) != 0 {
		t.Fatalf("a ready host officer: %+v, want no problem and no note", d)
	}
	for _, c := range []struct {
		name, problem, fix string
		breakIt            func(v *mirror.HostView)
	}{
		{"host offline", "host host-bravo is not connected", "start muster-mirror", func(v *mirror.HostView) { v.Online = false }},
		{"spawn off", "it starts no agent (agent spawn is off)", "-agent-spawn", func(v *mirror.HostView) {
			v.AgentSpawn, v.SpawnReady.Unavailable = nil, "agent spawn is off"
		}},
		{"no claude", "claude is not found there", "MUSTER_MIRROR_CLAUDE_PATH", func(v *mirror.HostView) {
			v.AgentSpawn, v.SpawnReady.Claude, v.SpawnReady.ClaudeDetail = nil, mirror.ClaudeMissing, "claude is not on the tmux server's PATH"
		}},
		{"not logged in", "Claude Code is not logged in there", "run `claude` once", func(v *mirror.HostView) { v.SpawnReady.Login = mirror.LoginNo }},
		{"repo not allowed", "repo " + hostOffRepo + " is not on its -agent-spawn-repos", "MUSTER_MIRROR_AGENT_SPAWN_REPOS", func(v *mirror.HostView) {
			v.SpawnReady.Repos = []string{"example-org/other"}
		}},
		{"no close", "does not close windows", "upgrade muster-mirror", func(v *mirror.HostView) { v.AgentClose = false }},
	} {
		reset()
		r.m.setHost(hostOffHost, c.breakIt)
		d := report()
		if len(d.Problems) != 1 || !strings.Contains(d.Problems[0].Problem, c.problem) || !strings.Contains(d.Problems[0].Fix, c.fix) {
			t.Errorf("%s: problems %+v, want one saying %q with a fix naming %q", c.name, d.Problems, c.problem, c.fix)
		}
	}
	reset()
	r.m.setHost(hostOffHost, func(v *mirror.HostView) { v.SpawnReady = nil })
	if d := report(); len(d.Problems) != 0 || len(d.Notes) != 1 || !strings.Contains(d.Notes[0], "reports no spawn readiness") {
		t.Errorf("a host with no readiness: %+v, want a note and no problem", d)
	}
	// A pod officer on this server (no agent plane): its own problem.
	pod := officers.Officer{ID: offA, Owner: hostOffOwner, MaxInstances: 1}
	d := DoctorOfficer{ID: offA}
	r.s.doctorRuntime(ctx, &d, pod)
	if len(d.Problems) != 1 || !strings.HasPrefix(d.Problems[0].Problem, "pods: ") || !strings.Contains(d.Problems[0].Fix, `"runtime": "host"`) {
		t.Errorf("a pod officer with no agent plane: %+v", d.Problems)
	}
}

// memAttention is an Attention store in memory: Create, Get, Cancel and Open,
// the calls the unclaimed notice makes.
type memAttention struct {
	attention.Store
	mu   sync.Mutex
	next int64
	asks map[int64]attention.Ask
}

func newMemAttention() *memAttention { return &memAttention{next: 500, asks: map[int64]attention.Ask{}} }

func (m *memAttention) Create(ctx context.Context, n attention.New) (attention.Ask, error) {
	n, err := attention.Clean(n)
	if err != nil {
		return attention.Ask{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	kind := attention.KindAsk
	if n.Notice {
		kind = attention.KindNotice
	}
	a := attention.Ask{ID: m.next, Kind: kind, Owner: authz.MustFrom(ctx).UserID, Asker: n.Asker, Question: n.Question, Status: attention.Open}
	m.asks[a.ID] = a
	return a, nil
}

func (m *memAttention) Get(_ context.Context, id int64) (attention.Ask, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.asks[id]
	if !ok {
		return attention.Ask{}, attention.ErrNotFound
	}
	return a, nil
}

func (m *memAttention) Cancel(_ context.Context, id int64) (attention.Ask, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.asks[id]
	switch {
	case !ok:
		return attention.Ask{}, attention.ErrNotFound
	case a.Status != attention.Open:
		return a, attention.ErrNotOpen
	}
	a.Status = attention.Cancelled
	m.asks[id] = a
	return a, nil
}

// created is how many asks were ever filed.
func (m *memAttention) created() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.asks)
}

// open is the open asks, by id.
func (m *memAttention) open() []attention.Ask {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []attention.Ask
	for _, a := range m.asks {
		if a.Status == attention.Open {
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(a, b attention.Ask) int { return int(a.ID - b.ID) })
	return out
}

// TestAnUnclaimedHostInstanceRaisesOneNoticeThatClearsOnTheClaim: a host
// instance that has not claimed its item 10 minutes after it started, with
// its window listed and work open, raises ONE Attention notice naming the
// likely cause its host reported (Claude Code not logged in) and the window;
// the next sweeps raise no second one, and a replica's racing copy is
// withdrawn; the instance's first claim withdraws it at once, and no later
// sweep raises another for that spawn. Nothing is raised before the 10
// minutes (the control), nor for an instance that already claimed.
// REGRESSION: the blind probe's "Asks says Nothing is waiting" 12 minutes
// into a window stuck at Claude Code's onboarding.
func TestAnUnclaimedHostInstanceRaisesOneNoticeThatClearsOnTheClaim(t *testing.T) {
	r := newHostOfficerRig(t, false)
	att := newMemAttention()
	r.s.ext.Attention = att
	ctx := context.Background()
	notIn := readyHost(false, hostOffRepo)
	notIn.Login = mirror.LoginNo
	r.m.setHost(hostOffHost, func(v *mirror.HostView) { v.SpawnReady = notIn })
	r.hq.AddItem(offB, "triage.look", "svc:stuck-1")
	r.rec.Check(ctx, offB, true)
	h := r.waitSpawned(t)
	stuck := listed(h)
	stuck.ClaudeSession = "" // Claude Code has started no conversation in it
	r.m.list(hostOffHost, stuck)

	r.advance(DefaultOfficerUnclaimedAfter - time.Second)
	r.s.sweepUnclaimedInstances(ctx)
	if n := len(att.open()); n != 0 {
		t.Fatalf("control: %d notice(s) raised before the 10 minutes", n)
	}
	r.advance(2 * time.Second)
	r.s.sweepUnclaimedInstances(ctx)
	open := att.open()
	if len(open) != 1 || open[0].Kind != attention.KindNotice || open[0].Owner != hostOffOwner {
		t.Fatalf("after 10 minutes unclaimed: %+v, want one notice to the owner", open)
	}
	q := open[0].Question
	for _, want := range []string{"Officer " + offB, "has not claimed its item", "Claude Code is not logged in on " + hostOffHost, "window agents:" + h.Window} {
		if !strings.Contains(q, want) {
			t.Errorf("the notice %q does not say %q", q, want)
		}
	}
	row, _ := r.spawns.HostSpawnByID(ctx, h.ID)
	if row.UnclaimedNotice != open[0].ID {
		t.Fatalf("the notice %d is not recorded on spawn %d (%+v)", open[0].ID, h.ID, row)
	}
	// De-duplicated: the next sweeps file nothing more, and a replica that
	// raced from the row as it was before withdraws its own copy.
	r.s.sweepUnclaimedInstances(ctx)
	if n := att.created(); n != 1 {
		t.Fatalf("a second sweep filed another notice: %d filed, want 1", n)
	}
	before := h
	before.Window = row.Window
	r.s.raiseUnclaimedNotice(ctx, r.spawns, r.host, before, "a racing replica")
	if n := len(att.open()); n != 1 {
		t.Fatalf("%d open notices after a second sweep and a raced copy, want 1", n)
	}

	// The instance claims: its notice is withdrawn at once and settled.
	r.s.hostInstanceClaimed(ctx, r.host, h.Instance)
	if a, _ := att.Get(ctx, open[0].ID); a.Status != attention.Cancelled {
		t.Fatalf("the notice after the claim is %s, want cancelled", a.Status)
	}
	row, _ = r.spawns.HostSpawnByID(ctx, h.ID)
	if row.ClaimedAt == nil || !row.NoticeSettled {
		t.Fatalf("the spawn after the claim: %+v, want claimed and its notice settled", row)
	}
	r.advance(time.Hour)
	r.s.sweepUnclaimedInstances(ctx)
	if n := len(att.open()); n != 0 {
		t.Fatalf("a claimed instance raised %d notice(s) later", n)
	}
	// An instance that claimed BEFORE the 10 minutes (no notice raised yet)
	// raises none after them.
	r.m.list(hostOffHost)
	r.advance(hostPendingCountdown + time.Second)
	r.rec.Check(ctx, offB, true)
	h2 := r.waitSpawned(t)
	r.m.list(hostOffHost, listed(h2))
	r.s.hostInstanceClaimed(ctx, r.host, h2.Instance)
	r.advance(DefaultOfficerUnclaimedAfter + time.Second)
	filed := att.created()
	r.s.sweepUnclaimedInstances(ctx)
	if att.created() != filed {
		t.Fatalf("an instance that claimed early raised a notice")
	}
}

// TestAnUnclaimedNoticeIsWithdrawnWhenItsWindowGoes: the sweep withdraws a
// notice whose window is gone, and raises none for an officer with no open
// work (an instance with nothing to claim is not stuck).
func TestAnUnclaimedNoticeIsWithdrawnWhenItsWindowGoes(t *testing.T) {
	r := newHostOfficerRig(t, false)
	att := newMemAttention()
	r.s.ext.Attention = att
	ctx := context.Background()
	item := r.hq.AddItem(offB, "triage.look", "svc:stuck-2")
	r.rec.Check(ctx, offB, true)
	h := r.waitSpawned(t)
	r.m.list(hostOffHost, listed(h))
	r.advance(DefaultOfficerUnclaimedAfter + time.Second)
	r.s.sweepUnclaimedInstances(ctx)
	open := att.open()
	if len(open) != 1 || !strings.Contains(open[0].Question, "may be stuck") {
		t.Fatalf("a listed, conversing, unclaimed instance: %+v, want one notice (cause unknown)", open)
	}
	r.m.list(hostOffHost)
	r.s.sweepUnclaimedInstances(ctx)
	if n := len(att.open()); n != 0 {
		t.Fatalf("%d notice(s) open after the window went", n)
	}
	// No open work: nothing raised for a fresh unclaimed spawn either.
	r.hq.Complete(item)
	row, err := r.spawns.InsertHostSpawn(ctx, offB, officers.InstanceName(offB, 1), hostOffHost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.spawns.RecordHostSpawnWindow(ctx, row.ID, hostOffHost, "@77", hostOffSess); err != nil {
		t.Fatal(err)
	}
	r.m.list(hostOffHost, mirror.Window{ID: "@77", Session: mirror.AgentSpawnSession, Dispatch: row.ID, Claude: true, ClaudeSession: hostOffSess})
	r.advance(DefaultOfficerUnclaimedAfter + time.Second)
	r.s.sweepUnclaimedInstances(ctx)
	if n := len(att.open()); n != 0 {
		t.Fatalf("an officer with no open work raised %d notice(s)", n)
	}
}

// TestNoBuiltinOfficerTextSaysHQ (F4): every officer tool a builtin officer's
// instance is offered — on a host (the mirror door's list) and in a pod (the
// agent door's per-officer definition) — and its kickoff say HQ nowhere, in
// any description or parameter; and a refusal of a builtin officer's tool
// call is worded for muster. An HQ officer's definitions are untouched (the
// control: they still say HQ).
func TestNoBuiltinOfficerTextSaysHQ(t *testing.T) {
	walk := func(v any) string {
		var b strings.Builder
		var rec func(any)
		rec = func(v any) {
			switch x := v.(type) {
			case string:
				b.WriteString(x + "\n")
			case map[string]any:
				for _, e := range x {
					rec(e)
				}
			case []any:
				for _, e := range x {
					rec(e)
				}
			case []string:
				for _, e := range x {
					rec(e)
				}
			}
		}
		rec(v)
		return b.String()
	}
	text := func(d agents.ToolDef) string { return d.Description + "\n" + walk(d.Parameters) }
	for _, d := range hostOfficerToolDefs() {
		if s := text(d); strings.Contains(s, "HQ") {
			t.Errorf("host officer tool %s says HQ:\n%s", d.Name, s)
		}
	}
	pod := officers.Officer{ID: "marketing:main", Backend: officers.BackendBuiltin, MaxInstances: 1}
	hq := officers.Officer{ID: "marketing:main", Backend: officers.BackendHQ, MaxInstances: 1}
	sawHQ := false
	for _, d := range officerToolDefs() {
		if officers.ToolOffered(pod, d.Name) && officers.ToolServed(pod, officers.DeclaredCapabilities(pod), d.Name) {
			if s := text(officerToolDefFor(pod, d)); strings.Contains(s, "HQ") {
				t.Errorf("builtin pod officer tool %s says HQ:\n%s", d.Name, s)
			}
		}
		if officers.ToolOffered(hq, d.Name) && strings.Contains(text(officerToolDefFor(hq, d)), "HQ") {
			sawHQ = true
		}
	}
	if !sawHQ {
		t.Fatal("control: no HQ officer tool says HQ, so the walk above could not see it either")
	}
	if k := officers.KickoffText(pod, 1); strings.Contains(k, "HQ") {
		t.Errorf("the builtin pod officer's kickoff says HQ:\n%s", k)
	}
	refused := &officers.HQError{Status: 409, Body: []byte(`{"code":"held"}`), Builtin: true}
	for _, got := range []string{officerToolResultFor(pod, officers.ToolClaim, nil, refused), officerToolResultFor(pod, officers.ToolClaim, nil, officers.ErrHQUnavailable),
		askToolResult(pod, []byte(`{"ok":true}`), nil)} {
		if strings.Contains(got, "HQ") && !strings.Contains(got, `"hq"`) {
			t.Errorf("a builtin officer's tool result says HQ: %s", got)
		}
		if strings.Contains(strings.ReplaceAll(got, `"hq"`, ""), "HQ") {
			t.Errorf("a builtin officer's tool result says HQ: %s", got)
		}
	}
	if got := officerToolResultFor(hq, officers.ToolClaim, nil, errors.Join(officers.ErrHQUnavailable)); !strings.Contains(got, "HQ is unavailable") {
		t.Errorf("control: an HQ officer's outage reads %q", got)
	}
}
