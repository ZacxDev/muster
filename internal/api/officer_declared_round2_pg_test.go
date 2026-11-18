package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers"
)

// Round 2's findings on the DB-declared officers (PR #708), each as the
// regression test that was red before its fix.

// TestAStartRacedByADeleteOnAnotherReplicaLeavesNoOrphan (round 2's first
// finding; the auditor's probe TestAuditR2CrossReplicaDeleteDuringStartLeavesOrphan):
// a wake pass's Start for owner 1's officer passes its "still declared" check,
// and the officer is deleted on ANOTHER replica — its database row goes, and
// this replica's roster still holds it — before the instance's agent is
// created. The start's second check sees the delete in the database; its
// teardown must remove the agent although this replica's roster still
// protects it, and say so truthfully.
//
// Red at 312d5dbb: the teardown was refused by the roster-based delete
// protection, the agent stayed — and the start answered "removed it".
func TestAStartRacedByADeleteOnAnotherReplicaLeavesNoOrphan(t *testing.T) {
	r := newDeclRig(t, declRigOpts{pods: true})
	if rec := r.hook(t, http.MethodPost, "/api/officers/declared", declPodBody); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	o := r.officerFor(t, 1, declID)
	var once sync.Once
	r.ag.beforeCreate = func(a agents.Agent) {
		if a.Name != o.InstanceName(1) {
			return
		}
		once.Do(func() {
			// The other replica's delete: the row only; this roster is untouched.
			if _, err := r.store.DeleteDeclared(dbtest.AsOperator(context.Background()), 1, declID); err != nil {
				t.Errorf("the other replica's delete: %v", err)
			}
		})
	}
	err := (officerFleet{s: r.s}).Start(context.Background(), o, 1, "kick")
	a, created := r.agentNamed(o.InstanceName(1))
	if !created {
		t.Fatalf("control: the interleaving never reached the create (err %v)", err)
	}
	if _, ok := r.s.officerRT.reg.ForOwner(1, declID); !ok {
		t.Fatal("control: this replica's roster dropped the officer: the delete was not another replica's")
	}
	if !waitDestroyed(r.prov, a.ID) {
		t.Fatalf("ORPHAN: the instance %s created after its officer's delete on another replica was left behind (start: %v)\n%s", a.Name, err, r.logs)
	}
	if err == nil || !strings.Contains(err.Error(), "was deleted while") || !strings.Contains(err.Error(), "removed it") {
		t.Fatalf("the start answered %v, want it to say the officer was deleted while it started and the instance removed", err)
	}
}

// TestAStartThatCannotRemoveItsInstanceSaysSo (round 2's first finding, the
// message): the officer's database row is deleted while its instance is
// created, and then the database cannot be read at all. The start cannot
// tell the delete apart from the outage, so the delete protection stands
// (fail closed) and the instance stays — and the start says it could NOT
// remove it, never "removed it".
func TestAStartThatCannotRemoveItsInstanceSaysSo(t *testing.T) {
	r := newDeclRig(t, declRigOpts{pods: true})
	if rec := r.hook(t, http.MethodPost, "/api/officers/declared", declPodBody); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	o := r.officerFor(t, 1, declID)
	var once sync.Once
	r.ag.beforeCreate = func(a agents.Agent) {
		if a.Name == o.InstanceName(1) {
			once.Do(func() {
				if _, err := r.store.DeleteDeclared(dbtest.AsOperator(context.Background()), 1, declID); err != nil {
					t.Errorf("the other replica's delete: %v", err)
				}
				r.f.pool.Close() // the database is gone too
			})
		}
	}
	err := (officerFleet{s: r.s}).Start(context.Background(), o, 1, "kick")
	a, created := r.agentNamed(o.InstanceName(1))
	if !created {
		t.Fatalf("control: the interleaving never reached the create (err %v)", err)
	}
	time.Sleep(100 * time.Millisecond)
	r.prov.mu.Lock()
	for _, d := range r.prov.destroyed {
		if d == a.ID {
			r.prov.mu.Unlock()
			t.Fatal("with the database unreadable the protection was lifted: the instance was torn down on no word from the database")
		}
	}
	r.prov.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "could not remove it") || strings.Contains(err.Error(), "removed it;") {
		t.Fatalf("the start answered %v, want it to say it could not remove the instance", err)
	}
}

// TestTheGoneOfficerTeardownNeedsTheDatabasesWord (round 2's first finding,
// the guard's other side): the teardown that may pass the roster's delete
// protection does so ONLY for the gone officer's own instance row, and only
// once the database says that officer is gone. While the database still
// declares it, or for another officer's row, the protection stands and the
// start says it could not remove it.
func TestTheGoneOfficerTeardownNeedsTheDatabasesWord(t *testing.T) {
	r := newDeclRig(t, declRigOpts{pods: true})
	ctx := context.Background()
	if rec := r.hook(t, http.MethodPost, "/api/officers/declared", declPodBody); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	other := `{"id":"scout:side","name":"Side","charter":"c","runtime":"pod"}`
	if rec := r.hook(t, http.MethodPost, "/api/officers/declared", other); rec.Code != http.StatusCreated {
		t.Fatalf("create scout:side: %d %s", rec.Code, rec.Body.String())
	}
	o, side := r.officerFor(t, 1, declID), r.officerFor(t, 1, "scout:side")
	if err := (officerFleet{s: r.s}).Start(ctx, o, 1, "kick"); err != nil {
		t.Fatalf("control: the start: %v", err)
	}
	a, ok := r.agentNamed(o.InstanceName(1))
	if !ok {
		t.Fatal("control: no instance was created")
	}
	notDestroyed := func(why string) {
		t.Helper()
		time.Sleep(100 * time.Millisecond)
		r.prov.mu.Lock()
		defer r.prov.mu.Unlock()
		for _, d := range r.prov.destroyed {
			if d == a.ID {
				t.Fatalf("%s: the live officer's instance %s was torn down", why, a.Name)
			}
		}
	}
	// 1. The database still declares it: the roster's word is not enough.
	if got := r.s.teardownGoneInstance(ctx, o, 1); got != teardownFailed {
		t.Fatalf("a gone-teardown of an officer the database still declares: outcome %v, want refused (%v)", got, teardownFailed)
	}
	notDestroyed("the database still declares it")
	// 2. ANOTHER officer is gone (on another replica): its word does not reach
	// this officer's row.
	if _, err := r.store.DeleteDeclared(dbtest.AsOperator(ctx), 1, "scout:side"); err != nil {
		t.Fatal(err)
	}
	destroy := func(ctx context.Context, id int64) error { return r.s.ext.Provisioner.Destroy(ctx, id) }
	octx := r.s.officerPrincipal(ctx, 1)
	if _, err := r.s.destroyAgent(octx, a.ID, destroy, destroyHooks{goneOfficer: &side}); !errors.As(err, new(errDestroyChief)) {
		t.Fatalf("a gone OTHER officer's teardown of %s: %v, want the protection's refusal", a.Name, err)
	}
	notDestroyed("another officer is gone")
	// 3. CONTROL: once the database says it is gone, the same call removes it.
	if _, err := r.store.DeleteDeclared(dbtest.AsOperator(ctx), 1, declID); err != nil {
		t.Fatal(err)
	}
	if got := r.s.teardownGoneInstance(ctx, o, 1); got != teardownRemoved {
		t.Fatalf("control: the gone officer's teardown: outcome %v, want removed\n%s", got, r.logs)
	}
	if !waitDestroyed(r.prov, a.ID) {
		t.Fatal("control: the gone officer's instance was not torn down")
	}
}

// TestAReissuedWindowIDNeverReGatesTheMachine (round 2's second finding; the
// auditor's probe TestAuditR2ReusedWindowIDReGates): a deleted host officer's
// mid-turn window keeps its machine gated (F4); the window ends, and the host
// lists it no more. Later the host lists ANOTHER window under the same tmux
// id (a restarted tmux server numbers its windows from the start again). That
// window is not the officer's, and the machine is not gated for it.
//
// One missing listing frees nothing (a transient miss); two in a row close the
// recorded window for good.
//
// Red at 312d5dbb: the reissued id re-gated the machine for the gone officer.
func TestAReissuedWindowIDNeverReGatesTheMachine(t *testing.T) {
	clk := &declClock{t: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
	h := spawnableHost(hostOffHost, true)
	h.AgentClose = true
	r := newDeclRig(t, declRigOpts{hosts: []mirror.HostView{h}, now: clk.now})
	ctx := context.Background()
	body := `{"id":"` + declID + `","charter":"c","runtime":"host","host":"` + hostOffHost + `","repo":"` + hostOffRepo + `"}`
	if rec := r.hook(t, http.MethodPost, "/api/officers/declared", body); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	o := r.officerFor(t, 1, declID)
	r.enqueue(t, 1, declID, "work")
	if res := r.pass(t, o); len(res.Decision.Start) != 1 {
		t.Fatalf("control: the pass started %v\n%s", res.Decision.Start, r.logs)
	}
	select {
	case <-r.m.spawned:
	case <-time.After(10 * time.Second):
		t.Fatal("the spawn was never sent")
	}
	var window string
	officerWaitFor(t, func() bool {
		rows, _ := r.s.officerHostSpawns().LatestHostSpawns(authz.WithPrincipal(ctx, authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaToken}), declID)
		window = rows[o.InstanceName(1)].Window
		return window != ""
	})
	sp := r.s.officerHostSpawns().(*officers.MemHostSpawnStore).All()
	live := mirror.Window{ID: window, Dispatch: sp[len(sp)-1].ID, Claude: true, ClaudeSession: hostOffSess}
	r.m.list(hostOffHost, live)
	r.m.mu.Lock()
	r.m.state[hostOffHost+"/"+window] = mirror.AgentState{Status: mirror.StatusBusy, Session: hostOffSess}
	r.m.mu.Unlock()
	if rec := r.hook(t, http.MethodDelete, "/api/officers/declared/"+declID, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	clk.advance(hostPendingCountdown + time.Minute) // the row is settled
	gate := func() []string { return r.s.hostOfficersOnMachine(ctx, hostOffOwner, hostOffHost) }
	listing := func(ws ...mirror.Window) {
		r.m.list(hostOffHost, ws...)
		r.s.HostDispatchListing(hostOffOwner, hostOffHost, ws)
	}
	// While the window lives, listings keep it (CONTROL: F4 still holds).
	listing(live)
	listing(live)
	if got := gate(); len(got) != 1 || got[0] != declID {
		t.Fatalf("control: the deleted officer's live window gates %v; want %s", got, declID)
	}
	// One transient miss frees nothing.
	listing()
	listing(live)
	if got := gate(); len(got) != 1 || got[0] != declID {
		t.Fatalf("one transient missing listing dropped the gate: %v; want %s", got, declID)
	}
	// The window ends: two listings without it.
	listing()
	listing()
	// Another window, under the reissued id: a plain shell, no tag.
	reissued := mirror.Window{ID: window, Command: "zsh"}
	listing(reissued)
	if got := gate(); len(got) != 0 {
		t.Fatalf("the reissued window id %s re-gated the machine for the gone officer: %v", window, got)
	}
}

// TestAReplaceOffAutoClearsItsPlacement (round 2's third finding): an auto
// officer placed on a host is replaced by one naming its runtime; its stored
// placement goes with it (a placement is an auto officer's alone).
func TestAReplaceOffAutoClearsItsPlacement(t *testing.T) {
	h := spawnableHost(hostOffHost, true)
	h.AgentClose = true
	r := newDeclRig(t, declRigOpts{hosts: []mirror.HostView{h}})
	ctx := dbtest.AsOperator(context.Background())
	if rec := r.hook(t, http.MethodPost, "/api/officers/declared", `{"id":"`+declID+`","charter":"c","repo":"`+hostOffRepo+`"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if row, err := r.store.GetDeclared(ctx, 1, declID); err != nil || row.AutoHost != hostOffHost || row.AutoHostAt.IsZero() {
		t.Fatalf("control: the auto officer's stored placement is %q at %v (%v); want %s", row.AutoHost, row.AutoHostAt, err, hostOffHost)
	}
	put := `{"charter":"c","runtime":"host","host":"` + hostOffHost + `","repo":"` + hostOffRepo + `","mode":"live","paused":false}`
	if rec := r.hook(t, http.MethodPut, "/api/officers/declared/"+declID, put); rec.Code != http.StatusOK {
		t.Fatalf("a replace naming the host runtime: %d %s", rec.Code, rec.Body.String())
	}
	if row, err := r.store.GetDeclared(ctx, 1, declID); err != nil || row.AutoHost != "" || !row.AutoHostAt.IsZero() {
		t.Fatalf("after the replace the stored placement is %q at %v (%v); want none", row.AutoHost, row.AutoHostAt, err)
	}
}

// failingSpawns is a spawn store whose window read fails.
type failingSpawns struct{ officers.HostSpawnStore }

func (failingSpawns) HostSpawnsInWindows(context.Context, string, []string) ([]officers.HostSpawn, error) {
	return nil, errors.New("the spawn store is down")
}

// TestTheStepUpGateHoldsWhenItsWindowsCannotBeRead (round 2's fourth
// finding: F4's read-error path had no test): with no officer in the roster,
// a machine whose host lists a window, and a spawn store that cannot be read,
// is GATED — the gate cannot tell whether that window is an officer's, so it
// holds. CONTROL: the same machine with a readable store is not gated.
func TestTheStepUpGateHoldsWhenItsWindowsCannotBeRead(t *testing.T) {
	h := spawnableHost(hostOffHost, true)
	h.AgentClose = true
	r := newDeclRig(t, declRigOpts{hosts: []mirror.HostView{h}})
	ctx := context.Background()
	r.m.list(hostOffHost, mirror.Window{ID: "@41", Claude: true})
	if got := r.s.hostOfficersOnMachine(ctx, hostOffOwner, hostOffHost); len(got) != 0 {
		t.Fatalf("control: a readable store and no officer window gate the machine: %v", got)
	}
	r.s.officerRT.hostSpawns = failingSpawns{r.s.officerRT.hostSpawns}
	got := r.s.hostOfficersOnMachine(ctx, hostOffOwner, hostOffHost)
	if len(got) != 1 || !strings.HasPrefix(got[0], "unknown") {
		t.Fatalf("an unreadable spawn store: the gate names %v; want it held (\"unknown…\")", got)
	}
}

// TestTwoReplicasAgreeOnAnAutoOfficersHost (round 2's third finding): two
// replicas, each with its own hub — replica A sees host-bravo connected,
// replica B sees host-charlie — load the same "runtime": "auto" officer. The
// placement is the database's, so both run it where it was placed (bravo),
// and keep agreeing while A sees bravo qualify, however long; once bravo is
// gone for both past the grace, B moves it (it alone sees a host that
// qualifies) and A reads the move.
//
// Red at 312d5dbb: each replica kept its own pick — A bravo, B charlie.
func TestTwoReplicasAgreeOnAnAutoOfficersHost(t *testing.T) {
	clk := &declClock{t: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
	bravo, charlie := spawnableHost(hostOffHost, true), spawnableHost(hostOffOther, true)
	bravo.AgentClose, charlie.AgentClose = true, true
	off := func(v mirror.HostView) mirror.HostView { v.Online, v.Rendered = false, false; return v }
	f := newConnFixture(t)
	a := newDeclRigOn(t, f, declRigOpts{hosts: []mirror.HostView{bravo, off(charlie)}, now: clk.now})
	b := newDeclRigOn(t, f, declRigOpts{hosts: []mirror.HostView{off(bravo), charlie}, now: clk.now})
	b.s.ext.Agents, b.ag = a.ag, a.ag // one agents table
	ctx := context.Background()
	body := `{"id":"` + declID + `","charter":"c","repo":"` + hostOffRepo + `"}`
	if rec := a.hook(t, http.MethodPost, "/api/officers/declared", body); rec.Code != http.StatusCreated || decodeView(t, rec).RunsOnHost != hostOffHost {
		t.Fatalf("control: the auto officer on replica A: %d %s; want it on %s", rec.Code, rec.Body.String(), hostOffHost)
	}
	on := func(r *declRig, name string) string {
		t.Helper()
		if err := r.s.reloadDeclaredOfficers(ctx); err != nil {
			t.Fatal(err)
		}
		st, ok := r.s.officerRT.declaredView(1, declID)
		if !ok || st.officer == nil {
			t.Fatalf("replica %s does not run it: %s\n%s", name, st.reason, r.logs)
		}
		return st.officer.Host
	}
	agree := func(want, when string) {
		t.Helper()
		ga, gb := on(a, "A"), on(b, "B")
		if ga != want || gb != want {
			t.Fatalf("%s: replica A runs it on %s, replica B on %s; want both on %s", when, ga, gb, want)
		}
	}
	agree(hostOffHost, "at once")
	for i := 0; i < 3; i++ {
		clk.advance(autoHostGrace / 2)
		agree(hostOffHost, "while A sees host-bravo qualify")
	}
	// host-bravo goes for A too. Inside the grace both keep it.
	a.m.setHost(hostOffHost, func(v *mirror.HostView) { v.Online = false })
	clk.advance(autoHostGrace / 2)
	agree(hostOffHost, "inside the grace")
	// Past it, B moves it (it alone sees host-charlie qualify), and A reads it.
	clk.advance(autoHostGrace)
	if gb := on(b, "B"); gb != hostOffOther {
		t.Fatalf("past the grace replica B runs it on %s; want the move to %s", gb, hostOffOther)
	}
	if ga := on(a, "A"); ga != hostOffOther {
		t.Fatalf("after B's move replica A runs it on %s; want %s (the stored placement)", ga, hostOffOther)
	}
}

// TestALostPlacementSwapTakesTheWinners (round 2's third finding, the
// compare-and-swap): a replica that decided a move from a placement another
// replica has changed since does not write over it — it runs the officer
// where the database now says.
func TestALostPlacementSwapTakesTheWinners(t *testing.T) {
	clk := &declClock{t: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
	bravo, charlie := spawnableHost(hostOffHost, true), spawnableHost(hostOffOther, true)
	bravo.AgentClose, charlie.AgentClose = true, true
	bravo.Online = false
	r := newDeclRig(t, declRigOpts{hosts: []mirror.HostView{bravo, charlie}, now: clk.now})
	ctx := context.Background()
	body := `{"id":"` + declID + `","charter":"c","repo":"` + hostOffRepo + `"}`
	if rec := r.hook(t, http.MethodPost, "/api/officers/declared", body); rec.Code != http.StatusCreated || decodeView(t, rec).RunsOnHost != hostOffOther {
		t.Fatalf("control: the auto officer: %d %s; want it on %s", rec.Code, rec.Body.String(), hostOffOther)
	}
	row, err := r.store.GetDeclared(dbtest.AsOperator(ctx), 1, declID)
	if err != nil || row.AutoHost != hostOffOther {
		t.Fatalf("control: the stored placement is %q (%v); want %s", row.AutoHost, err, hostOffOther)
	}
	// What this replica read: placed on host-bravo, long gone. Meanwhile the
	// database says host-charlie (another replica's move). Its own move, to
	// host-charlie as well, would be a swap from bravo — which loses.
	clk.advance(2 * autoHostGrace)
	stale := autoPlacement{host: hostOffHost, ok: clk.now().Add(-2 * autoHostGrace)}
	// A host-delta placement in the database, as another replica wrote it.
	if ok, err := r.store.SetAutoHost(r.s.officerPrincipal(ctx, 1), 1, declID, row.Fields.InstanceSlug, hostOffOther, "host-delta", clk.now()); err != nil || !ok {
		t.Fatalf("control: another replica's move: %v %v", ok, err)
	}
	got, why := r.s.autoHostFor(ctx, row.Fields, stale)
	if got != "host-delta" {
		t.Fatalf("a lost swap ran it on %q (%s); want the winner's host-delta", got, why)
	}
	if cur, _ := r.store.GetDeclared(dbtest.AsOperator(ctx), 1, declID); cur.AutoHost != "host-delta" {
		t.Fatalf("a lost swap overwrote the winner's placement: %q", cur.AutoHost)
	}
}

// TestTheRoundTwoSeamsAreALedger: the teardown that may pass the roster's
// delete protection is reached only from officerFleet.Start, and is the one
// place that names a gone officer to destroyAgent; the stored auto placement
// has one writer, the roster load's autoHostFor.
func TestTheRoundTwoSeamsAreALedger(t *testing.T) {
	assertCallers(t, ".", "teardownGoneInstance", []string{"officers.go:Start"})
	assertCallers(t, ".", "goneOfficerRow", []string{"agents.go:destroyAgent"})
	assertCallers(t, ".", "SetAutoHost", []string{"officer_declared.go:autoHostFor"})
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var setters []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(string(b), "goneOfficer:"); n > 0 {
			setters = append(setters, f+":"+strconv.Itoa(n))
		}
	}
	if len(setters) != 1 || setters[0] != "officer_declared.go:1" {
		t.Fatalf("destroyHooks.goneOfficer is set in %v; want exactly once, in officer_declared.go (teardownGoneInstance)", setters)
	}
}
