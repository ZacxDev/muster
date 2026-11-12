package api

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
)

// WHICH CLONE A HOST DISPATCH STARTS IN (clone_choice.go) over a real Postgres
// and the approval fixture (dispatch_approval_pg_test.go). host-bravo lists
// its clones (AgentSpawnClones); host-charlie is an OLDER host that does not.
// The clone paths are fictional and pairwise distinct — none is a substring
// of another, none equals a constant the code names.
const (
	ccLive  = "/srv/fixture-a/real-name-live"
	ccOld   = "/srv/fixture-b/real-name-standalone"
	ccThird = "/opt/fixture-c/real-name-mirror"
)

// cloneAsk is one agent-clones the fake mirror answered.
type cloneAsk struct{ host, repo string }

// cloneMirror answers AgentClones from list (by host), recording each ask.
type cloneMirror struct {
	mu   sync.Mutex
	list map[string][]mirror.AgentClone
	asks []cloneAsk
}

var cloneLists sync.Map // *spawnMirror → *cloneMirror

func (m *spawnMirror) AgentClones(ctx context.Context, host, repo string) ([]mirror.AgentClone, error) {
	v, ok := cloneLists.Load(m)
	if !ok {
		return nil, mirror.ErrClonesUnsupported
	}
	c := v.(*cloneMirror)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asks = append(c.asks, cloneAsk{host, repo})
	return append([]mirror.AgentClone(nil), c.list[host]...), nil
}

// withClones makes host-bravo list clones (and only host-bravo advertise it).
func (f *hostFixture) withClones(t *testing.T, clones ...mirror.AgentClone) *cloneMirror {
	t.Helper()
	c := &cloneMirror{list: map[string][]mirror.AgentClone{"host-bravo": clones}}
	cloneLists.Store(f.m, c)
	t.Cleanup(func() { cloneLists.Delete(f.m) })
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	for i := range f.m.hosts {
		f.m.hosts[i].AgentSpawnClones = f.m.hosts[i].Host == "host-bravo"
	}
	return c
}

func (c *cloneMirror) set(host string, clones ...mirror.AgentClone) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.list[host] = clones
}

func (c *cloneMirror) asked() []cloneAsk {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]cloneAsk(nil), c.asks...)
}

func live(p string) mirror.AgentClone { return mirror.AgentClone{Path: p, Live: true} }
func dead(p string) mirror.AgentClone { return mirror.AgentClone{Path: p} }

// TestSeveralClonesTheLiveSessionsCloneIsTheSpawns: host-bravo has three clones
// of the repository, ONE of them live and listed second. In the owner's typed
// turn the start goes straight to the host with THAT clone as the hint, after
// asking host-bravo for its clones of the CANONICAL repository name. Outside
// it, the approval card names the clone and the tap starts in it. An older
// host (host-charlie) is asked nothing and sent no hint.
func TestSeveralClonesTheLiveSessionsCloneIsTheSpawns(t *testing.T) {
	f := newApprovalFixture(t)
	cm := f.withClones(t, dead(ccOld), live(ccLive), dead(ccThird))
	f.ownerTurn(t, f.chief, f.owner, "Start it mirrored on host-bravo with Claude.")
	typed := apCall("host/host-bravo/claude", strings.ToLower(apRealRepo), "live-1")
	typed["owner_asked"] = true
	hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, typed))
	calls := f.m.spawnCalls()
	if len(calls) != 1 || calls[0].req.Clone != ccLive {
		t.Fatalf("the typed start's spawn: %+v; want clone %s", calls, ccLive)
	}
	if a := cm.asked(); len(a) != 1 || a[0] != (cloneAsk{"host-bravo", apRealRepo}) {
		t.Fatalf("clone lists asked for: %+v", a)
	}
	// Outside the typed turn: the approval card names the clone; its tap starts there.
	f.untypedTurn(t)
	call := apCall("host/host-bravo/claude", apRealRepo, "live-2")
	if held := f.callRaw(f.chief, agentDispatchTool, call); !strings.Contains(held, `"blanks":["approve"]`) {
		t.Fatalf("not an approval card: %s", held)
	}
	card := f.newestCard(t)
	if !strings.Contains(card.Question, "\nClone: "+ccLive+"\n") {
		t.Fatalf("the approval card does not name the clone:\n%s", card.Question)
	}
	f.tap(t, card.ID, approveLabel, authz.ViaSession)
	f.untypedTurn(t)
	hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, call))
	if calls := f.m.spawnCalls(); len(calls) != 2 || calls[1].req.Clone != ccLive {
		t.Fatalf("the approved start's spawn: %+v", calls)
	}
	// The older host: no list asked for, no hint sent.
	f.withConnectedRepos(t, apTooling)
	f.ownerTurn(t, f.chief, f.owner, "Start it mirrored on host-charlie with Claude.")
	old := apCall("host/host-charlie/claude", apTooling, "live-3")
	old["owner_asked"] = true
	hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, old))
	if calls := f.m.spawnCalls(); len(calls) != 3 || calls[2].req.Clone != "" || len(cm.asked()) != 3 {
		t.Fatalf("an older host: spawn %+v, asks %+v; want no hint and no list asked for", calls, cm.asked())
	}
}

// TestSeveralClonesNoneOrBothLiveTheCardOffersThem: with no clone live, the
// call raises the CLONE card in the approval card's place — the whole request,
// "Clone: choose it below", the host's clones in its order, then "Don't
// start" — and nothing reaches a host. Before the tap the completing call
// waits; the tap binds the clone: the completing call names it and the spawn
// carries it. With TWO live the live ones come first, marked; in the owner's
// TYPED turn the clone card is raised too, and its tap starts the request as
// the owner's relayed message (not a brief they "approved on a card").
func TestSeveralClonesNoneOrBothLiveTheCardOffersThem(t *testing.T) {
	f := newApprovalFixture(t)
	cm := f.withClones(t, dead(ccOld), dead(ccThird))
	f.untypedTurn(t)
	call := apCall("host/host-bravo/claude", apRealRepo, "pick-1")
	if held := f.callRaw(f.chief, agentDispatchTool, call); !strings.Contains(held, `"blanks":["clone"]`) {
		t.Fatalf("not a clone card: %s", held)
	}
	card := f.newestCard(t)
	if want := ccOld + "|" + ccThird + "|" + declineLabel; strings.Join(card.Options, "|") != want {
		t.Fatalf("options %q, want %q", card.Options, want)
	}
	for _, part := range []string{"Clone: choose it below", "Repository: " + apRealRepo, apBriefLine1 + "\n" + apBriefLine2, "did not type"} {
		if !strings.Contains(card.Question, part) {
			t.Errorf("the clone card lacks %q:\n%s", part, card.Question)
		}
	}
	complete := apCall("host/host-bravo/claude", apRealRepo, "pick-1")
	complete["clone"] = ccThird
	if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, complete)); got != agentDispatchHeldWaiting {
		t.Fatalf("before the tap: %q", got)
	}
	if len(f.m.spawnCalls()) != 0 || f.allReservations(t) != 0 {
		t.Fatal("a clone card reserved or spawned")
	}
	f.tap(t, card.ID, ccThird, authz.ViaSession)
	f.untypedTurn(t)
	hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, complete))
	calls := f.m.spawnCalls()
	if len(calls) != 1 || calls[0].req.Clone != ccThird || !strings.Contains(calls[0].req.Prompt, apApprovedPre) {
		t.Fatalf("the tapped clone's spawn: %+v", calls)
	}
	// Two live, in the owner's typed turn.
	cm.set("host-bravo", dead(ccOld), live(ccThird), live(ccLive))
	f.ownerTurn(t, f.chief, f.owner, "Start it mirrored on host-bravo with Claude.")
	typed := apCall("host/host-bravo/claude", apRealRepo, "pick-2")
	typed["owner_asked"] = true
	if held := f.callRaw(f.chief, agentDispatchTool, typed); !strings.Contains(held, `"blanks":["clone"]`) {
		t.Fatalf("typed, two live: not a clone card: %s", held)
	}
	card = f.newestCard(t)
	if want := ccThird + cloneLiveSuffix + "|" + ccLive + cloneLiveSuffix + "|" + ccOld + "|" + declineLabel; strings.Join(card.Options, "|") != want {
		t.Fatalf("typed options %q, want %q", card.Options, want)
	}
	if !strings.HasPrefix(card.Question, "Which clone should this agent start in? You asked your chief for it in its chat") {
		t.Fatalf("the typed clone card's question:\n%s", card.Question)
	}
	f.tap(t, card.ID, ccLive+cloneLiveSuffix, authz.ViaSession)
	f.untypedTurn(t)
	// The chief passes the tapped label as the card offered it: the path is taken.
	done := apCall("host/host-bravo/claude", apRealRepo, "pick-2")
	done["clone"] = ccLive + cloneLiveSuffix
	hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, done))
	calls = f.m.spawnCalls()
	if len(calls) != 2 || calls[1].req.Clone != ccLive || strings.Contains(calls[1].req.Prompt, apApprovedPre) {
		t.Fatalf("the typed request's spawn: clone %q, approved wording %v", calls[1].req.Clone, strings.Contains(calls[1].req.Prompt, apApprovedPre))
	}
}

// TestTheHeldHashBindsTheClone: after the owner tapped one clone, a completing
// call naming ANOTHER of the host's clones is refused naming the one they
// chose; a clone the host did not list is refused before anything else; a
// clone on a pod target is refused; and an approval card filed with the one
// live clone is refused as changed when the host's live clone moved before
// the completing call (the hash binds the clone the card showed). Only the
// exact call starts, in the tapped clone.
func TestTheHeldHashBindsTheClone(t *testing.T) {
	f := newApprovalFixture(t)
	cm := f.withClones(t, dead(ccOld), dead(ccThird))
	f.untypedTurn(t)
	if held := f.callRaw(f.chief, agentDispatchTool, apCall("host/host-bravo/claude", apRealRepo, "bind-1")); !strings.Contains(held, `"blanks":["clone"]`) {
		t.Fatalf("not a clone card: %s", held)
	}
	f.tap(t, f.newestCard(t).ID, ccThird, authz.ViaSession)
	other := apCall("host/host-bravo/claude", apRealRepo, "bind-1")
	other["clone"] = ccOld
	if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, other)); got != fmt.Sprintf(agentDispatchHeldChoseClone, ccThird) {
		t.Fatalf("another listed clone: %q", got)
	}
	other["clone"] = ccLive
	if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, other)); got != fmt.Sprintf(agentDispatchCloneNotListed, ccLive, "host-bravo", apRealRepo) {
		t.Fatalf("a clone the host did not list: %q", got)
	}
	pod := map[string]any{"target": "pod/claude-code", "prompt": apBrief, "request_key": "bind-pod", "clone": ccOld}
	if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, pod)); got != agentDispatchCloneHostOnly {
		t.Fatalf("a clone on a pod target: %q", got)
	}
	if len(f.m.spawnCalls()) != 0 || f.allReservations(t) != 0 {
		t.Fatal("a refused clone started")
	}
	exact := apCall("host/host-bravo/claude", apRealRepo, "bind-1")
	exact["clone"] = ccThird
	hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, exact))
	if calls := f.m.spawnCalls(); len(calls) != 1 || calls[0].req.Clone != ccThird {
		t.Fatalf("the exact call's spawn: %+v", calls)
	}
	// The approval card's clone, settled by the host's one live clone, is bound.
	cm.set("host-bravo", live(ccOld), dead(ccThird))
	f.untypedTurn(t)
	call := apCall("host/host-bravo/claude", apRealRepo, "bind-2")
	if held := f.callRaw(f.chief, agentDispatchTool, call); !strings.Contains(held, `"blanks":["approve"]`) {
		t.Fatalf("not an approval card: %s", held)
	}
	f.tap(t, f.newestCard(t).ID, approveLabel, authz.ViaSession)
	cm.set("host-bravo", dead(ccOld), live(ccThird))
	if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, call)); got != fmt.Sprintf(agentDispatchHeldChanged, "bind-2") {
		t.Fatalf("the live clone moved after the card: %q", got)
	}
	cm.set("host-bravo", live(ccOld), dead(ccThird))
	hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, call))
	if calls := f.m.spawnCalls(); len(calls) != 2 || calls[1].req.Clone != ccOld {
		t.Fatalf("the approved card's spawn: %+v", calls)
	}
}

// TestAHostsBadCloneRefusalReachesTheChief: the host refuses the hinted clone
// (bad_clone, its own code): the chief reads its sentence, naming the host,
// the repository and the host's detail, and what to do.
func TestAHostsBadCloneRefusalReachesTheChief(t *testing.T) {
	f := newApprovalFixture(t)
	f.withClones(t, live(ccLive), dead(ccOld))
	detail := "the clone muster named is not one of this host's 2 clone(s) of repo " + apRealRepo + " under its roots"
	f.m.reply = func(string, int64, mirror.AgentSpawnReq) (mirror.AgentSpawned, error) {
		return mirror.AgentSpawned{}, &mirror.SpawnRefusedError{Code: mirror.SpawnBadClone, Detail: detail}
	}
	f.ownerTurn(t, f.chief, f.owner, "Start it mirrored on host-bravo with Claude.")
	call := apCall("host/host-bravo/claude", apRealRepo, "bad-1")
	call["owner_asked"] = true
	got := errorText(t, f.callRaw(f.chief, agentDispatchTool, call))
	if want := `host "host-bravo" refused the clone of ` + apRealRepo + ` muster named: ` + detail + `. Call again without clone, and muster asks your owner which`; got != want {
		t.Fatalf("the refusal: %q\nwant %q", got, want)
	}
}
