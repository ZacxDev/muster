package api

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/sse"
)

// MIRRORED DISPATCH, SERVER HALF (host_dispatch.go, host_dispatch_reconcile.go)
// over a real Postgres, with a fake mirror whose hosts are pairwise distinct:
// host-bravo and host-charlie can spawn claude; host-delta is offline. So a
// mutant that picks "the first host" or "the only host" is seen.

// spawnMirror is the fixture's mirror: fakeMirror's sessions, plus spawnable
// hosts for the fixture's owner and a recording SpawnAgent.
type spawnMirror struct {
	*fakeMirror
	owner int64
	mu    sync.Mutex
	hosts []mirror.HostView
	calls []spawnCall
	reply func(host string, dispatch int64, req mirror.AgentSpawnReq) (mirror.AgentSpawned, error)
	// pastes is every PasteWindow, with the dispatch it carried (ForDispatch).
	pastes []spawnPaste
}

type spawnPaste struct {
	host, session, window, text string
	dispatch                    int64
}

func (m *spawnMirror) PasteWindow(ctx context.Context, host, session, window, text string, target mirror.PasteTarget, opts ...mirror.PasteOption) error {
	o := mirror.PasteOptionsOf(opts...)
	m.mu.Lock()
	m.pastes = append(m.pastes, spawnPaste{host, session, window, text, o.Dispatch})
	m.mu.Unlock()
	return nil
}

type spawnCall struct {
	host     string
	dispatch int64
	req      mirror.AgentSpawnReq
}

func (m *spawnMirror) Hosts(ctx context.Context) []mirror.HostView {
	if p, ok := authz.From(ctx); !ok || p.UserID != m.owner {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mirror.HostView(nil), m.hosts...)
}

func (m *spawnMirror) SpawnAgent(ctx context.Context, host string, dispatch int64, req mirror.AgentSpawnReq) (mirror.AgentSpawned, error) {
	m.mu.Lock()
	m.calls = append(m.calls, spawnCall{host, dispatch, req})
	reply := m.reply
	m.mu.Unlock()
	if reply != nil {
		return reply(host, dispatch, req)
	}
	return mirror.AgentSpawned{Session: "agents", Window: "@" + strconv.FormatInt(dispatch, 10), Worktree: "/w/" + strconv.FormatInt(dispatch, 10),
		Branch: "muster/" + strconv.FormatInt(dispatch, 10), AgentSession: "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b"}, nil
}

func (m *spawnMirror) spawnCalls() []spawnCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]spawnCall(nil), m.calls...)
}

func spawnableHost(name string, online bool) mirror.HostView {
	return mirror.HostView{Host: name, Online: online, Interactive: true, Rendered: online, AgentSpawn: []string{mirror.SpawnRuntimeClaude}}
}

type hostFixture struct {
	*releaseFixture
	m *spawnMirror
}

const hostPrompt = "Fix the typo in the README."

func newHostFixture(t *testing.T) *hostFixture {
	t.Helper()
	f := &hostFixture{releaseFixture: newReleaseFixture(t)}
	f.withConnectedRepos(t, "acme/widgets")
	f.m = &spawnMirror{fakeMirror: &fakeMirror{online: true, interactive: true}, owner: f.owner,
		hosts: []mirror.HostView{spawnableHost("host-bravo", true), spawnableHost("host-charlie", true), spawnableHost("host-delta", false)}}
	f.srv.ext.Mirror = f.m
	f.srv.ext.Attention = attention.NewPG(f.pool)
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		_, _ = f.pool.Exec(sys, `DELETE FROM attention_asks WHERE owner_id = $1`, f.owner)
		_, _ = f.pool.Exec(sys, `DELETE FROM host_dispatches WHERE owner_id = $1`, f.owner)
	})
	return f
}

func hostCall(target, key string, ownerAsked bool) map[string]any {
	return map[string]any{"target": target, "prompt": hostPrompt, "repo": "acme/widgets", "request_key": key, "owner_asked": ownerAsked}
}

func (f *hostFixture) row(t *testing.T, id int64) plugins.HostDispatch {
	t.Helper()
	r, err := f.plugins.HostDispatchByID(f.asOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func hostDispatchIDOf(t *testing.T, text string) int64 {
	t.Helper()
	var out struct {
		ID int64 `json:"host_dispatch_id"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.ID <= 0 || strings.Contains(text, `"error"`) {
		t.Fatalf("not a host start: %s", text)
	}
	return out.ID
}

// TestAHostTargetIsTheOwnersAndAConnectedRepos: each gate refuses with its own
// sentence BEFORE anything is reserved or sent to a host — no owner_asked, a
// plugin message's turn, a repository the owner never connected, an offline
// host — and the owner's own typed turn starts it: the host gets a request
// whose prompt begins with muster's preface, and the row is pending with the
// window the reply named. It counts ONCE in live (source C, not the 10-minute
// pending window).
func TestAHostTargetIsTheOwnersAndAConnectedRepos(t *testing.T) {
	f := newHostFixture(t)
	f.refusedNothingReserved(t, f.callRaw(f.chief, agentDispatchTool, hostCall("host/host-bravo/claude", "h-1", false)), agentDispatchHostOwnerAsk, 0, 0)
	f.say(t, f.chief, "Plugin Example tracker: start a mirrored agent on host-bravo.")
	f.refusedNothingReserved(t, f.callRaw(f.chief, agentDispatchTool, hostCall("host/host-bravo/claude", "h-1", true)), ownerAskedNoTaskNoOwnerTurn, 0, 0)
	f.ownerTurn(t, f.chief, f.owner, "Start it mirrored on host-bravo with Claude.")
	other := hostCall("host/host-bravo/claude", "h-1", true)
	other["repo"] = "acme/held-but-not-connected"
	f.refusedNothingReserved(t, f.callRaw(f.chief, agentDispatchTool, other), "acme/held-but-not-connected "+chiefDispatchRepo, 0, 0)
	f.refusedNothingReserved(t, f.callRaw(f.chief, agentDispatchTool, hostCall("host/host-delta/claude", "h-1", true)),
		`host "host-delta" cannot start agents now`, 0, 0)
	f.refusedNothingReserved(t, f.callRaw(f.chief, agentDispatchTool, hostCall("host/host-zulu/claude", "h-1", true)),
		`host "host-zulu" cannot start agents now`, 0, 0)
	if n := len(f.m.spawnCalls()); n != 0 {
		t.Fatalf("%d spawn requests reached a host for refused calls", n)
	}
	text := f.callRaw(f.chief, agentDispatchTool, hostCall("host/host-bravo/claude", "h-1", true))
	id := hostDispatchIDOf(t, text)
	calls := f.m.spawnCalls()
	if len(calls) != 1 || calls[0].host != "host-bravo" || calls[0].dispatch != id || calls[0].req.Runtime != "claude" ||
		calls[0].req.Repo != "acme/widgets" || !strings.HasPrefix(calls[0].req.Prompt, mirror.SpawnPreface) ||
		!strings.Contains(calls[0].req.Prompt, hostPrompt) || len(calls[0].req.Prompt) > mirror.MaxSpawnPrompt {
		t.Fatalf("the spawn request: %+v", calls)
	}
	r := f.row(t, id)
	if r.Status != plugins.HostDispatchPending || r.Window != "@"+strconv.FormatInt(id, 10) || r.Host != "host-bravo" || r.ReservationID <= 0 {
		t.Fatalf("the row: %+v", r)
	}
	b := f.budget(t)
	if b.Live != 1 || b.Today != 1 || len(b.LiveAgents) != 1 || b.LiveAgents[0].HostDispatch != id || b.LiveAgents[0].Target != "host/host-bravo/claude" {
		t.Fatalf("budget %+v: want the host dispatch counted once, from its row", b)
	}
}

// TestARefusedHostSpawnEndsReleasesAndFreesItsKey: the host refuses (no_repo);
// the chief reads the code's sentence; the row ends, holds no slot, and the
// request_key is FREE: the same call starts a new dispatch. A dispatch that ran
// and then ended keeps its key: the same call replays it.
func TestARefusedHostSpawnEndsReleasesAndFreesItsKey(t *testing.T) {
	f := newHostFixture(t)
	f.ownerTurn(t, f.chief, f.owner, "Start it mirrored on host-bravo with Claude.")
	f.m.reply = func(string, int64, mirror.AgentSpawnReq) (mirror.AgentSpawned, error) {
		return mirror.AgentSpawned{}, &mirror.SpawnRefusedError{Code: mirror.SpawnNoRepo, Detail: "repo acme/widgets is not on this host's allow-list"}
	}
	text := f.callRaw(f.chief, agentDispatchTool, hostCall("host/host-bravo/claude", "h-2", true))
	if got := errorText(t, text); !strings.HasPrefix(got, `repo acme/widgets has no single clone on host "host-bravo": repo acme/widgets is not on this host's allow-list`) {
		t.Fatalf("refusal %q", got)
	}
	ids := f.hostRows(t)
	if len(ids) != 1 || ids[0].Status != plugins.HostDispatchEnded || ids[0].EndedWhy != "refused" || !strings.HasPrefix(ids[0].Refusal, "no_repo:") {
		t.Fatalf("rows %+v", ids)
	}
	if b := f.budget(t); b.Live != 0 {
		t.Fatalf("a refused spawn holds a slot: %+v", b)
	}
	f.m.reply = nil
	again := f.callRaw(f.chief, agentDispatchTool, hostCall("host/host-bravo/claude", "h-2", true))
	second := hostDispatchIDOf(t, again)
	if second == ids[0].ID || strings.Contains(again, `"replayed"`) {
		t.Fatalf("the freed key did not start a new dispatch: %s", again)
	}
	replay := f.callRaw(f.chief, agentDispatchTool, hostCall("host/host-bravo/claude", "h-2", true))
	if !strings.Contains(replay, `"replayed":true`) || hostDispatchIDOf(t, replay) != second || len(f.m.spawnCalls()) != 2 {
		t.Fatalf("the same key after a start did not replay it: %s", replay)
	}
}

func (f *hostFixture) hostRows(t *testing.T) []plugins.HostDispatch {
	t.Helper()
	rows, err := f.plugins.HostDispatchesForHost(f.asOwner, "host-bravo")
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestTheReconcilerMovesARowByListings: a listing carrying the window's
// dispatch makes it running; ONE missing listing does not end it; the second
// does ("window gone"), and its task's start is released (a new start on the
// task then succeeds). A pending row younger than 180 s is not counted down;
// one older than that is, after two listings.
func TestTheReconcilerMovesARowByListings(t *testing.T) {
	f := newHostFixture(t)
	task := f.task(t, "Fix the README")
	f.ownerTurn(t, f.chief, f.owner, "Do task "+strconv.FormatInt(task.ID, 10)+" mirrored on host-bravo.")
	args := hostCall("host/host-bravo/claude", "", true)
	args["task_id"] = task.ID
	delete(args, "request_key")
	id := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, args))
	win := mirror.Window{Session: "agents", ID: "@" + strconv.FormatInt(id, 10), Claude: true, Dispatch: id}
	now := time.Now()
	f.srv.reconcileListing(context.Background(), f.owner, "host-bravo", []mirror.Window{win}, now)
	if r := f.row(t, id); r.Status != plugins.HostDispatchRunning || r.MissingListings != 0 {
		t.Fatalf("after its listing: %+v", r)
	}
	f.srv.reconcileListing(context.Background(), f.owner, "host-bravo", nil, now)
	if r := f.row(t, id); r.Status != plugins.HostDispatchRunning || r.MissingListings != 1 {
		t.Fatalf("one missing listing ended it: %+v", r)
	}
	f.srv.reconcileListing(context.Background(), f.owner, "host-bravo", nil, now)
	r := f.row(t, id)
	if r.Status != plugins.HostDispatchEnded || r.EndedWhy != "window gone" {
		t.Fatalf("two missing listings: %+v", r)
	}
	// Released: the task takes a new start.
	f.ownerTurn(t, f.chief, f.owner, "Start it again mirrored on host-charlie.")
	args["target"] = "host/host-charlie/claude"
	if again := f.callRaw(f.chief, agentDispatchTool, args); strings.Contains(again, "already_dispatched") || strings.Contains(again, `"error"`) {
		t.Fatalf("the ended dispatch did not release its task's start: %s", again)
	}

	// A pending row (no window ever listed): not counted down while young.
	f.m.reply = func(string, int64, mirror.AgentSpawnReq) (mirror.AgentSpawned, error) { return mirror.AgentSpawned{}, mirror.ErrSpawnUnconfirmed }
	f.ownerTurn(t, f.chief, f.owner, "And one more on host-bravo.")
	pend := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, hostCall("host/host-bravo/claude", "h-3", true)))
	for i := 0; i < 3; i++ {
		f.srv.reconcileListing(context.Background(), f.owner, "host-bravo", nil, time.Now())
	}
	if r := f.row(t, pend); r.Status != plugins.HostDispatchPending || r.MissingListings != 0 {
		t.Fatalf("a young pending row was counted down: %+v", r)
	}
	later := time.Now().Add(hostPendingCountdown + time.Minute)
	f.srv.reconcileListing(context.Background(), f.owner, "host-bravo", nil, later)
	f.srv.reconcileListing(context.Background(), f.owner, "host-bravo", nil, later)
	if r := f.row(t, pend); r.Status != plugins.HostDispatchEnded || r.EndedWhy != "window gone" {
		t.Fatalf("an old pending row with no window: %+v", r)
	}
}

// TestALateReplyIsAppliedOnlyFromItsOwnHostAndOwner: the waiter timed out (the
// row stays pending); the late frame, through the hub's hook, records the
// window. A frame for that dispatch arriving on ANOTHER host's connection, or
// another owner's, changes nothing; a late refusal ends a row; a duplicate is
// one event.
func TestALateReplyIsAppliedOnlyFromItsOwnHostAndOwner(t *testing.T) {
	f := newHostFixture(t)
	f.m.reply = func(string, int64, mirror.AgentSpawnReq) (mirror.AgentSpawned, error) { return mirror.AgentSpawned{}, mirror.ErrSpawnUnconfirmed }
	f.ownerTurn(t, f.chief, f.owner, "Start it mirrored on host-bravo.")
	text := f.callRaw(f.chief, agentDispatchTool, hostCall("host/host-bravo/claude", "h-4", true))
	id := hostDispatchIDOf(t, text)
	if !strings.Contains(text, `"status":"starting"`) {
		t.Fatalf("an unconfirmed start: %s", text)
	}
	// Unconfirmed: no agent row, so ONLY its host_dispatches row may count it
	// (source C), never also the reservation's 10-minute window (source B).
	if b := f.budget(t); b.Live != 1 || len(b.LiveAgents) != 1 || b.LiveAgents[0].HostDispatch != id {
		t.Fatalf("an unconfirmed host start is counted %d times: %+v", b.Live, b)
	}
	frame := mirror.Msg{Type: mirror.TypeAgentSpawned, Dispatch: id, Spawned: &mirror.AgentSpawned{Session: "agents", Window: "@9", Worktree: "/w", Branch: "b", AgentSession: "s"}}
	f.srv.HostDispatchSpawnReply(f.owner, "host-charlie", frame)
	f.srv.HostDispatchSpawnReply(f.owner+1, "host-bravo", frame)
	if r := f.row(t, id); r.Window != "" {
		t.Fatalf("a frame from the wrong host or owner was applied: %+v", r)
	}
	f.srv.HostDispatchSpawnReply(f.owner, "host-bravo", frame)
	f.srv.HostDispatchSpawnReply(f.owner, "host-bravo", frame)
	if r := f.row(t, id); r.Window != "@9" || r.Status != plugins.HostDispatchPending {
		t.Fatalf("the late reply: %+v", r)
	}
	if n := f.agentRowsFor(t, id); n != 1 {
		t.Fatalf("%d agent rows for one dispatch (a duplicate reply is one event)", n)
	}
	f.srv.HostDispatchSpawnReply(f.owner, "host-bravo", mirror.Msg{Type: mirror.TypeAgentSpawned, Dispatch: id, Refusal: mirror.SpawnFailed, Error: "deadline passed"})
	if r := f.row(t, id); r.Status != plugins.HostDispatchEnded {
		t.Fatalf("a late refusal: %+v", r)
	}
	// A reply after the end revives nothing (UpdateHostDispatch writes no ended row).
	f.srv.HostDispatchSpawnReply(f.owner, "host-bravo", mirror.Msg{Type: mirror.TypeAgentSpawned, Dispatch: id,
		Spawned: &mirror.AgentSpawned{Session: "agents", Window: "@10", Worktree: "/w2", Branch: "b2", AgentSession: "s2"}})
	if r := f.row(t, id); r.Status != plugins.HostDispatchEnded || r.Window != "@9" {
		t.Fatalf("a reply after the end changed the row: %+v", r)
	}
}

func (f *hostFixture) agentRowsFor(t *testing.T, id int64) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(f.asOwner, `SELECT count(*) FROM audit_log WHERE action = $1 AND user_id = $2 AND (detail->>'host_dispatch_id')::bigint = $3`,
		plugins.ActionChiefDispatchAgent, f.owner, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestAnEndFromTheReconcilerReachesTheOwnersStream: the reconciler ends a row
// from a context with no principal ("window gone"); the owner's stream must
// still get agent.changed naming it, or their page keeps showing it running.
func TestAnEndFromTheReconcilerReachesTheOwnersStream(t *testing.T) {
	f := newHostFixture(t)
	f.ownerTurn(t, f.chief, f.owner, "Start it mirrored on host-bravo.")
	id := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, hostCall("host/host-bravo/claude", "h-9", true)))
	bus := sse.New(64)
	f.srv.bus = bus
	_, stream := bus.Subscribe(f.owner)
	if _, err := f.srv.endHostDispatch(context.Background(), f.owner, id, "window gone", false); err != nil {
		t.Fatal(err)
	}
	want := `{"id":"host-dispatch-` + strconv.FormatInt(id, 10) + `"}`
	for {
		select {
		case ev := <-stream:
			if ev.Name == EventAgentChanged && ev.Data == want {
				return
			}
		default:
			t.Fatalf("the owner's stream got no %s %s", EventAgentChanged, want)
		}
	}
}

// TestAnOrphanWindowRaisesOneNoticeAndIsNeverAdopted: an ENDED dispatch whose
// window is still listed raises exactly one notice (its id on the row), stays
// ended, raises no second notice on the next listing, and the notice is
// withdrawn once the window is gone. A tag naming no row raises nothing.
func TestAnOrphanWindowRaisesOneNoticeAndIsNeverAdopted(t *testing.T) {
	f := newHostFixture(t)
	f.ownerTurn(t, f.chief, f.owner, "Start it mirrored on host-bravo.")
	id := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, hostCall("host/host-bravo/claude", "h-5", true)))
	if _, err := f.srv.endHostDispatch(context.Background(), f.owner, id, "ended by owner", false); err != nil {
		t.Fatal(err)
	}
	win := mirror.Window{Session: "agents", ID: "@" + strconv.FormatInt(id, 10), Claude: true, Dispatch: id}
	stray := mirror.Window{Session: "agents", ID: "@77", Claude: true, Dispatch: 987654321}
	notices := func() []attention.Ask {
		all, _ := f.srv.ext.Attention.Open(f.asOwner)
		var out []attention.Ask
		for _, a := range all {
			if a.Kind == attention.KindNotice {
				out = append(out, a)
			}
		}
		return out
	}
	f.srv.reconcileListing(context.Background(), f.owner, "host-bravo", []mirror.Window{win, stray}, time.Now())
	r := f.row(t, id)
	ns := notices()
	if r.Status != plugins.HostDispatchEnded || r.OrphanNoticeID == 0 || len(ns) != 1 || ns[0].ID != r.OrphanNoticeID ||
		!strings.Contains(ns[0].Question, "dispatch "+strconv.FormatInt(id, 10)) {
		t.Fatalf("row %+v, notices %+v", r, ns)
	}
	f.srv.reconcileListing(context.Background(), f.owner, "host-bravo", []mirror.Window{win}, time.Now())
	// Counted over EVERY notice row, withdrawn ones included: a second notice
	// raised and then withdrawn is still a second notice raised.
	var raised int
	if err := f.pool.QueryRow(f.asOwner, `SELECT count(*) FROM attention_asks WHERE owner_id = $1 AND kind = 'notice'`, f.owner).Scan(&raised); err != nil {
		t.Fatal(err)
	}
	if len(notices()) != 1 || raised != 1 {
		t.Fatalf("a second listing raised a second notice (%d open, %d raised)", len(notices()), raised)
	}
	f.srv.reconcileListing(context.Background(), f.owner, "host-bravo", nil, time.Now())
	if len(notices()) != 0 {
		t.Fatal("the notice was not withdrawn once the window was gone")
	}
}

// TestTheSweepEndsAHostNotSeenForADay: a row whose host_seen_at is 25 hours old
// ends "host not seen"; one seen an hour ago does not. It reads only the table.
func TestTheSweepEndsAHostNotSeenForADay(t *testing.T) {
	f := newHostFixture(t)
	f.ownerTurn(t, f.chief, f.owner, "Start two mirrored.")
	old := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, hostCall("host/host-bravo/claude", "h-6", true)))
	f.ownerTurn(t, f.chief, f.owner, "And another.")
	fresh := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, hostCall("host/host-charlie/claude", "h-7", true)))
	sys := authz.System(context.Background())
	if _, err := f.pool.Exec(sys, `UPDATE host_dispatches SET host_seen_at = now() - interval '25 hours' WHERE id = $1`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(sys, `UPDATE host_dispatches SET host_seen_at = now() - interval '1 hour' WHERE id = $1`, fresh); err != nil {
		t.Fatal(err)
	}
	f.srv.sweepHostDispatches(context.Background())
	if r := f.row(t, old); r.Status != plugins.HostDispatchEnded || r.EndedWhy != "host not seen" {
		t.Fatalf("the stale row: %+v", r)
	}
	if r := f.row(t, fresh); r.Status == plugins.HostDispatchEnded {
		t.Fatalf("the fresh row was swept: %+v", r)
	}
}

// --- the held card and its binding ----------------------------------------------

// cards is the owner's open attention asks raised by the chief (the cards).
func (f *hostFixture) cards(t *testing.T) []attention.Ask {
	t.Helper()
	all, err := f.srv.ext.Attention.Open(f.asOwner)
	if err != nil {
		t.Fatal(err)
	}
	var out []attention.Ask
	for _, a := range all {
		if a.Asker.AgentID == f.chief.ID {
			out = append(out, a)
		}
	}
	return out
}

func (f *hostFixture) holds(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(f.asOwner, `SELECT count(*) FROM audit_log WHERE action = $1 AND user_id = $2 AND detail->>'door' = 'card'`,
		plugins.ActionDispatchHeld, f.owner).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// tap answers card id as the owner through door via (session = their
// browser, device = their phone).
func (f *hostFixture) tap(t *testing.T, id int64, label string, via authz.Via) {
	t.Helper()
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.owner, Role: authz.RoleAdmin, Via: via})
	if _, err := f.srv.answerAsk(ctx, id, label); err != nil {
		t.Fatalf("tap %q: %v", label, err)
	}
}

// TestAHeldHostTargetRaisesOneCardOfTheSpawnableHosts: host/?/claude in the
// owner's turn files ONE hold and ONE card whose options are exactly the two
// spawnable hosts (the offline one absent), reserves nothing and starts
// nothing; the same call again replays (no second card). With NO spawnable
// host it is refused with no card and no hold — counted, against the one-host
// positive control.
func TestAHeldHostTargetRaisesOneCardOfTheSpawnableHosts(t *testing.T) {
	f := newHostFixture(t)
	f.ownerTurn(t, f.chief, f.owner, "Start a mirrored agent with Claude to fix the README.")
	text := f.callRaw(f.chief, agentDispatchTool, hostCall("host/?/claude", "held-1", true))
	if !strings.Contains(text, `"held":true`) {
		t.Fatalf("not held: %s", text)
	}
	cs := f.cards(t)
	if len(cs) != 1 || strings.Join(cs[0].Options, "|") != "host-bravo|host-charlie" || f.holds(t) != 1 {
		t.Fatalf("cards %+v, holds %d", cs, f.holds(t))
	}
	if f.allReservations(t) != 0 || len(f.m.spawnCalls()) != 0 {
		t.Fatal("a held call reserved or spawned")
	}
	if again := f.callRaw(f.chief, agentDispatchTool, hostCall("host/?/claude", "held-1", true)); !strings.Contains(again, `"replayed":true`) ||
		len(f.cards(t)) != 1 || f.holds(t) != 1 {
		t.Fatalf("the same held call raised again: %s", again)
	}
	// No spawnable host: refused, nothing raised. Control first: one host.
	f.m.hosts = []mirror.HostView{spawnableHost("host-bravo", true)}
	f.ownerTurn(t, f.chief, f.owner, "Another mirrored one.")
	if ctl := f.callRaw(f.chief, agentDispatchTool, hostCall("host/?/claude", "held-2", true)); !strings.Contains(ctl, `"held":true`) ||
		len(f.cards(t)) != 2 || f.holds(t) != 2 {
		t.Fatalf("control (one host): %s", ctl)
	}
	f.m.hosts = []mirror.HostView{spawnableHost("host-delta", false)}
	f.ownerTurn(t, f.chief, f.owner, "And another mirrored one.")
	if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, hostCall("host/?/claude", "held-3", true))); got != agentDispatchNoHost {
		t.Fatalf("no spawnable host: %q", got)
	}
	if len(f.cards(t)) != 2 || f.holds(t) != 2 {
		t.Fatal("a call with no spawnable host raised a card or filed a hold")
	}
}

// TestAHeldCallOnAPluginTurnRaisesNothing: the same held call in a turn the
// owner did not type is refused by ownerTurnFor: no card, no hold.
func TestAHeldCallOnAPluginTurnRaisesNothing(t *testing.T) {
	f := newHostFixture(t)
	f.say(t, f.chief, "Plugin Example tracker: run it mirrored.")
	if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, hostCall("host/?/claude", "held-4", true))); got != ownerAskedNoTaskNoOwnerTurn {
		t.Fatalf("plugin turn: %q", got)
	}
	if len(f.cards(t)) != 0 || f.holds(t) != 0 {
		t.Fatal("a plugin turn raised a card")
	}
}

// heldAndTapped files host/?/claude under key in the owner's turn and returns
// its card; the chief's NEXT turn is not the owner's (a plugin row).
func (f *hostFixture) heldCard(t *testing.T, key string) attention.Ask {
	t.Helper()
	f.ownerTurn(t, f.chief, f.owner, "Start a mirrored agent with Claude.")
	if text := f.callRaw(f.chief, agentDispatchTool, hostCall("host/?/claude", key, true)); !strings.Contains(text, `"held":true`) {
		t.Fatalf("not held: %s", text)
	}
	var card attention.Ask
	for _, c := range f.cards(t) {
		if card.ID < c.ID {
			card = c
		}
	}
	f.say(t, f.chief, "muster: your owner answered attention #"+strconv.FormatInt(card.ID, 10))
	return card
}

// TestTheOwnersTapCompletesTheHeldDispatchExactly: through each owner door
// (browser, phone) the tap on host-charlie lets the completing call — same
// key, same arguments, host/host-charlie/claude, in a turn the owner did NOT
// type — start the dispatch, with held_id set and one unit spent. Each refusal
// is its own sentence: a different host than tapped, a changed prompt, repo or
// base, an unanswered card, an answer through attention_answer (a mirrored
// session), and a second completion (the hold is consumed: one tap, one agent).
func TestTheOwnersTapCompletesTheHeldDispatchExactly(t *testing.T) {
	for _, via := range []authz.Via{authz.ViaSession, authz.ViaDevice} {
		t.Run(string(via), func(t *testing.T) {
			f := newHostFixture(t)
			card := f.heldCard(t, "tap-1")
			complete := hostCall("host/host-charlie/claude", "tap-1", false)
			if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, complete)); got != agentDispatchHeldWaiting {
				t.Fatalf("before the tap: %q", got)
			}
			f.tap(t, card.ID, "host-charlie", via)
			wrong := hostCall("host/host-bravo/claude", "tap-1", false)
			if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, wrong)); got != "your owner chose host-charlie for this dispatch: call agent_dispatch with exactly that" {
				t.Fatalf("another host: %q", got)
			}
			for field, v := range map[string]any{"prompt": "Something else entirely.", "repo": "acme/other", "base": "dev"} {
				changed := hostCall("host/host-charlie/claude", "tap-1", false)
				changed[field] = v
				if field == "repo" {
					f.withConnectedRepos(t, "acme/widgets", "acme/other")
				}
				if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, changed)); got != `request_key "tap-1" was held with different arguments: only the blank your owner answered may change` {
					t.Fatalf("changed %s: %q", field, got)
				}
			}
			before := f.budget(t).Today
			id := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, complete))
			r := f.row(t, id)
			if r.Host != "host-charlie" || r.HeldID == "" {
				t.Fatalf("the completed row: %+v", r)
			}
			if after := f.budget(t).Today; after != before+1 {
				t.Fatalf("today %d → %d; want exactly one unit", before, after)
			}
			// The hold is consumed by the start it made: one tap, one agent.
			if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, complete)); got != `request_key "tap-1" holds no dispatch waiting for your owner's answer (or it is more than 15 minutes old): ask them again in the chat` {
				t.Fatalf("a second completion: %q", got)
			}
			if n := len(f.m.spawnCalls()); n != 1 {
				t.Fatalf("%d spawns for one tap", n)
			}
		})
	}
}

// TestATapThroughAMirroredSessionDoesNotBind: the card answered through
// attention_answer (a mirrored session's door, recorded mcp) is not the
// owner's tap: refused with the door sentence, nothing starts.
func TestATapThroughAMirroredSessionDoesNotBind(t *testing.T) {
	f := newHostFixture(t)
	card := f.heldCard(t, "tap-2")
	if _, err := f.srv.answerAskFrom(f.asOwner, card.ID, "host-charlie", attention.AnswerSource{Via: attention.ViaMCP, Host: "laptop", Session: "5a6b7c8d-9e0f-4a1b-8c2d-3e4f5a6b7c8d"}); err != nil {
		t.Fatal(err)
	}
	if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, hostCall("host/host-charlie/claude", "tap-2", false))); got != agentDispatchHeldDoor {
		t.Fatalf("an MCP answer: %q", got)
	}
	if len(f.m.spawnCalls()) != 0 {
		t.Fatal("an MCP answer started a dispatch")
	}
}

// TestAHoldOlderThanFifteenMinutesBindsNothing: the same tap, on a hold 16
// minutes old, is refused as no hold.
func TestAHoldOlderThanFifteenMinutesBindsNothing(t *testing.T) {
	f := newHostFixture(t)
	card := f.heldCard(t, "tap-3")
	f.tap(t, card.ID, "host-bravo", authz.ViaSession)
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE audit_log SET at = now() - interval '16 minutes' WHERE action = $1 AND user_id = $2`,
		plugins.ActionDispatchHeld, f.owner); err != nil {
		t.Fatal(err)
	}
	if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, hostCall("host/host-bravo/claude", "tap-3", false))); got != `request_key "tap-3" holds no dispatch waiting for your owner's answer (or it is more than 15 minutes old): ask them again in the chat` {
		t.Fatalf("an old hold: %q", got)
	}
}

// TestMirroredWithNothingElseAsksTheHostThenTheModel: "mirrored" (host/?/?)
// raises the host card; the tap re-files the hold with the host filled and
// raises the MODEL card, whose only option in phase 3 is Claude (Claude Code);
// its tap completes the start as host/<tapped>/claude.
func TestMirroredWithNothingElseAsksTheHostThenTheModel(t *testing.T) {
	f := newHostFixture(t)
	f.ownerTurn(t, f.chief, f.owner, "Start a mirrored agent.")
	if text := f.callRaw(f.chief, agentDispatchTool, hostCall("mirrored", "m-1", true)); !strings.Contains(text, `"held":true`) {
		t.Fatalf("mirrored: %s", text)
	}
	host := f.cards(t)[0]
	f.say(t, f.chief, "muster: your owner answered")
	f.tap(t, host.ID, "host-charlie", authz.ViaSession)
	text := f.callRaw(f.chief, agentDispatchTool, hostCall("host/host-charlie/?", "m-1", false))
	if !strings.Contains(text, `"held":true`) {
		t.Fatalf("after the host tap: %s", text)
	}
	cs := f.cards(t)
	if len(cs) != 1 || strings.Join(cs[0].Options, "|") != "Claude (Claude Code)" {
		t.Fatalf("the model card: %+v", cs)
	}
	f.tap(t, cs[0].ID, "Claude (Claude Code)", authz.ViaDevice)
	id := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, hostCall("host/host-charlie/claude", "m-1", false)))
	if r := f.row(t, id); r.Host != "host-charlie" || r.Runtime != "claude" {
		t.Fatalf("the started row: %+v", r)
	}
}

// TestTheOwnerTypingInsteadOfTappingStartsThroughTheirTurn: the full target
// with owner_asked in the owner's typed turn starts the dispatch through
// ownerTurnFor; the hold is left unconsumed.
func TestTheOwnerTypingInsteadOfTappingStartsThroughTheirTurn(t *testing.T) {
	f := newHostFixture(t)
	f.heldCard(t, "type-1")
	f.ownerTurn(t, f.chief, f.owner, "On host-bravo.")
	id := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, hostCall("host/host-bravo/claude", "type-2", true)))
	if r := f.row(t, id); r.HeldID != "" {
		t.Fatalf("a typed start consumed the hold: %+v", r)
	}
}

// TestAHostStartReadsAsAHostStartEverywhere: on a task whose latest start is a
// host dispatch, the prior-start answer names it, destroying some pod on the
// task releases nothing, and a completed task stops no host window.
func TestAHostStartReadsAsAHostStartEverywhere(t *testing.T) {
	f := newHostFixture(t)
	task := f.task(t, "Host task")
	f.ownerTurn(t, f.chief, f.owner, "Do it mirrored on host-bravo.")
	args := hostCall("host/host-bravo/claude", "", true)
	delete(args, "request_key")
	args["task_id"] = task.ID
	id := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, args))
	if wrote, err := f.plugins.ReleaseChiefDispatch(f.asOwner, task.ID, 123456); err != nil || wrote {
		t.Fatalf("a pod destroy released a host start: %v %v", wrote, err)
	}
	if _, ok, err := f.plugins.ChiefStartedAgent(f.asOwner, task.ID); err != nil || ok {
		t.Fatalf("ChiefStartedAgent on a host start: %v %v", ok, err)
	}
	f.ownerTurn(t, f.chief, f.owner, "Again.")
	again := f.callRaw(f.chief, agentDispatchTool, args)
	if !strings.Contains(again, "already_dispatched") {
		t.Fatalf("the prior start: %s", again)
	}
	_ = id
}
