package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// Round 0's findings on the DB-declared officers (PR #708), each as the
// regression test that was red before its fix. The F1 probe is the cache's
// own (internal/officers TestABackendCachedForOneOwnerIsNeverServedToAnother).

// declClock is a settable clock for the officer runtime.
type declClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *declClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *declClock) advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// TestAnAgentBindsToAnOfficerOnlyAsItsOwners (F2, the auditor's probe): an
// agent whose NAME parses as owner 1's DB-declared officer's instance, but
// which is another owner's row, is no instance of it — the binding is the
// name AND the owner (officerOf), as the host door's always was. CONTROL:
// owner 1's own row of that name binds.
//
// Red at 3b4296b7: the other owner's row bound to owner 1's officer.
func TestAnAgentBindsToAnOfficerOnlyAsItsOwners(t *testing.T) {
	r := newDeclRig(t, declRigOpts{pods: true})
	if rec := r.hook(t, http.MethodPost, "/api/officers/declared", declPodBody); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	o := r.officerFor(t, 1, declID)
	if _, _, ok := r.s.officerOf(agents.Agent{Name: o.InstanceName(1), OwnerID: declOther}); ok {
		t.Fatalf("owner %d's agent %s bound to owner 1's officer %s", declOther, o.InstanceName(1), declID)
	}
	if got, n, ok := r.s.officerOf(agents.Agent{Name: o.InstanceName(1), OwnerID: 1}); !ok || got.Owner != 1 || n != 1 {
		t.Fatalf("control: owner 1's own instance row binds to %+v #%d (%v)", got, n, ok)
	}
}

// TestAStartRacedByADeleteLeavesNoOrphan (F2, the auditor's probe, made
// deterministic): a wake pass's Start for owner 1's officer passes its
// "still declared" check, and the officer is deleted — swap and teardown —
// before the instance's agent is created (the agents store's create hook runs
// the delete). The start must not leave that agent behind: it asks again once
// the agent exists and removes it.
//
// Red at 3b4296b7: the agent was created after the teardown and stayed, an
// orphan no officer owns.
func TestAStartRacedByADeleteLeavesNoOrphan(t *testing.T) {
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
			if rec := r.hook(t, http.MethodDelete, "/api/officers/declared/"+declID, ""); rec.Code != http.StatusOK {
				t.Errorf("the racing delete: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
	err := (officerFleet{s: r.s}).Start(context.Background(), o, 1, "kick")
	a, created := r.agentNamed(o.InstanceName(1))
	if !created {
		t.Fatalf("control: the interleaving never reached the create (err %v)", err)
	}
	if !waitDestroyed(r.prov, a.ID) {
		t.Fatalf("the instance %s created after its officer's delete was left behind (start: %v)\n%s", a.Name, err, r.logs)
	}
	if err == nil || !strings.Contains(err.Error(), "was deleted while") {
		t.Fatalf("the start answered %v, want it to say the officer was deleted while it started", err)
	}
}

// TestASlugWithLeftoverAgentsIsNeverAllocated (F2): an agent named for a
// slug's instance already exists (an orphan, here another owner's), so a
// create passes that slug over and takes its next candidate — a new officer
// never adopts an agent it did not start.
func TestASlugWithLeftoverAgentsIsNeverAllocated(t *testing.T) {
	r := newDeclRig(t, declRigOpts{pods: true})
	cands := officers.SlugCandidates(1, declID, officers.SlugMaxFor("muster-agent-", 0))
	orphan := officers.Officer{InstanceSlug: cands[0]}.InstanceName(1)
	if _, err := r.ag.Create(context.Background(), agents.Agent{Name: orphan, OwnerID: declOther}); err != nil {
		t.Fatal(err)
	}
	rec := r.hook(t, http.MethodPost, "/api/officers/declared", declPodBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if v := decodeView(t, rec); v.InstanceSlug != cands[1] {
		t.Fatalf("the create took slug %q while agent %s exists; want the next candidate %q", v.InstanceSlug, orphan, cands[1])
	}
}

// TestAnAutoOfficerSticksToItsHostThroughABlip (F3, the auditor's probe; D3):
// a "runtime": "auto" officer runs on host-bravo (its owner's first
// qualifying host) and has started an instance there. host-bravo drops off;
// host-charlie still qualifies. The officer STAYS on host-bravo through the
// grace (autoHostGrace) — it does not move to a host whose start the old
// instance row would refuse. Past the grace it moves, the old host's instance
// row is torn down, and the next pass starts it on host-charlie. A restart
// (no memory) keeps it where its rows say it runs, though host-bravo is back
// and sorts first.
//
// Red at 3b4296b7: the first load after the blip moved it to host-charlie.
func TestAnAutoOfficerSticksToItsHostThroughABlip(t *testing.T) {
	clk := &declClock{t: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
	bravo, charlie := spawnableHost(hostOffHost, true), spawnableHost(hostOffOther, true)
	bravo.AgentClose, charlie.AgentClose = true, true
	r := newDeclRig(t, declRigOpts{hosts: []mirror.HostView{bravo, charlie}, now: clk.now})
	body := `{"id":"` + declID + `","charter":"c","repo":"` + hostOffRepo + `"}`
	rec := r.hook(t, http.MethodPost, "/api/officers/declared", body)
	if rec.Code != http.StatusCreated || decodeView(t, rec).RunsOnHost != hostOffHost {
		t.Fatalf("control: the auto officer: %d %s; want it on %s", rec.Code, rec.Body.String(), hostOffHost)
	}
	o := r.officerFor(t, 1, declID)
	r.enqueue(t, 1, declID, "work")
	waitSpawn := func(host string) {
		t.Helper()
		select {
		case <-r.m.spawned:
		case <-time.After(10 * time.Second):
			t.Fatalf("no spawn was sent to %s\n%s", host, r.logs)
		}
		r.m.mu.Lock()
		last := r.m.spawns[len(r.m.spawns)-1].host
		r.m.mu.Unlock()
		if last != host {
			t.Fatalf("the spawn went to %s, want %s", last, host)
		}
	}
	if res := r.pass(t, o); len(res.Decision.Start) != 1 {
		t.Fatalf("control: the first pass started %v", res.Decision.Start)
	}
	waitSpawn(hostOffHost)
	if a, ok := r.agentNamed(o.InstanceName(1)); !ok || a.RunsOnHost != hostOffHost {
		t.Fatalf("control: the instance row %+v (%v), want one on %s", a, ok, hostOffHost)
	}
	// The blip.
	r.m.setHost(hostOffHost, func(v *mirror.HostView) { v.Online = false })
	ctx := context.Background()
	reloadOn := func(want string) {
		t.Helper()
		if err := r.s.reloadDeclaredOfficers(ctx); err != nil {
			t.Fatal(err)
		}
		st, ok := r.s.officerRT.declaredView(1, declID)
		if !ok || st.officer == nil || st.officer.Host != want {
			got := "nowhere"
			if st.officer != nil {
				got = st.officer.Host
			}
			t.Fatalf("the auto officer runs on %s (%s); want %s\n%s", got, st.reason, want, r.logs)
		}
	}
	reloadOn(hostOffHost)
	clk.advance(autoHostGrace - time.Minute)
	reloadOn(hostOffHost)
	if a, ok := r.agentNamed(o.InstanceName(1)); !ok || a.RunsOnHost != hostOffHost {
		t.Fatalf("inside the grace the instance row was touched: %+v (%v)", a, ok)
	}
	// Past the grace: it moves, and the old host's row goes.
	clk.advance(2 * time.Minute)
	reloadOn(hostOffOther)
	if a, ok := r.agentNamed(o.InstanceName(1)); ok && a.RunsOnHost == hostOffHost {
		t.Fatalf("the move left host %s's instance row behind: %+v", hostOffHost, a)
	}
	moved := r.officerFor(t, 1, declID)
	if res := r.pass(t, moved); len(res.Decision.Start) != 1 || len(res.ActErrs) != 0 {
		t.Fatalf("after the move the pass decided %+v (errors %v); want a start on %s\n%s", res.Decision, res.ActErrs, hostOffOther, r.logs)
	}
	waitSpawn(hostOffOther)
	if a, ok := r.agentNamed(o.InstanceName(1)); !ok || a.RunsOnHost != hostOffOther {
		t.Fatalf("after the move the instance row is %+v (%v), want one on %s", a, ok, hostOffOther)
	}
	// A restart: no memory; host-bravo is back and sorts first. The rows say
	// host-charlie, so it stays.
	r.s.officerRT.declMu.Lock()
	r.s.officerRT.autoHost = nil
	r.s.officerRT.declMu.Unlock()
	r.m.setHost(hostOffHost, func(v *mirror.HostView) { v.Online = true })
	reloadOn(hostOffOther)
}

// TestADeclaredOfficerNeverRewritesAnotherOfficersType (F5): owner 1's env
// officer scout:ops runs on type scout. Creating and replacing DB officer
// scout:main writes ITS OWN type (keyed by its slug) and leaves type scout's
// charter alone; the officer's capability grant reads its own type's policy.
//
// Red at 3b4296b7: the create rewrote type scout's charter.
func TestADeclaredOfficerNeverRewritesAnotherOfficersType(t *testing.T) {
	r := newDeclRig(t, declRigOpts{pods: true, env: `[{"id":"scout:ops","backend":"builtin"}]`})
	ctx := dbtest.AsOperator(context.Background())
	const envCharter = "The env officer's charter: triage only."
	if err := r.store.PutType(ctx, 1, builtin.Type{Key: "scout", Name: "Scout", Charter: envCharter}); err != nil {
		t.Fatal(err)
	}
	body := `{"id":"` + declID + `","name":"Scout DB","charter":"the db officer's charter","policy":{"capabilities":["contacts"]},"runtime":"pod"}`
	rec := r.hook(t, http.MethodPost, "/api/officers/declared", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	slug := decodeView(t, rec).InstanceSlug
	envType := func() string {
		t.Helper()
		ty, err := r.store.GetType(ctx, 1, "scout")
		if err != nil {
			t.Fatal(err)
		}
		return ty.Charter
	}
	if got := envType(); got != envCharter {
		t.Fatalf("creating DB officer %s rewrote type scout's charter to %q", declID, got)
	}
	own, err := r.store.GetType(ctx, 1, slug)
	if err != nil || own.Charter != "the db officer's charter" {
		t.Fatalf("the DB officer's own type %s: %+v %v", slug, own, err)
	}
	if ok, err := r.store.TypeGrants(ctx, 1, declID, builtin.CapContacts); err != nil || !ok {
		t.Fatalf("the DB officer's grant does not read its own type's policy: %v %v", ok, err)
	}
	if ok, _ := r.store.TypeGrants(ctx, 1, "scout:ops", builtin.CapContacts); ok {
		t.Fatal("the env officer was granted the DB officer's capability")
	}
	put := `{"charter":"the db officer's charter, v2","runtime":"pod","mode":"live","paused":false}`
	if rec := r.hook(t, http.MethodPut, "/api/officers/declared/"+declID, put); rec.Code != http.StatusOK {
		t.Fatalf("replace: %d %s", rec.Code, rec.Body.String())
	}
	if got := envType(); got != envCharter {
		t.Fatalf("replacing DB officer %s rewrote type scout's charter to %q", declID, got)
	}
	if own, _ := r.store.GetType(ctx, 1, slug); own.Charter != "the db officer's charter, v2" {
		t.Fatalf("the replace did not write the officer's own type: %+v", own)
	}
	// Its delete takes its own type with it, and type scout stays.
	if rec := r.hook(t, http.MethodDelete, "/api/officers/declared/"+declID, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := r.store.GetType(ctx, 1, slug); err == nil {
		t.Fatal("the deleted officer's own type is still there")
	}
	if got := envType(); got != envCharter {
		t.Fatalf("the delete changed type scout: %q", got)
	}
}

// TestAReplaceMustStateModeAndPaused (F6): a PUT that leaves out mode or
// paused is refused per field, and changes nothing — a shadow officer is never
// flipped live, nor a paused one resumed, by omission. With both stated it is
// a whole replace. A replace of an officer the caller has none of is a 404,
// whatever its body.
func TestAReplaceMustStateModeAndPaused(t *testing.T) {
	r := newDeclRig(t, declRigOpts{pods: true})
	body := `{"id":"` + declID + `","charter":"c","runtime":"pod","mode":"shadow","paused":true}`
	if rec := r.hook(t, http.MethodPost, "/api/officers/declared", body); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range []struct {
		name, body string
		fields     []string
	}{
		{"neither", `{"charter":"c2","runtime":"pod"}`, []string{"mode", "paused"}},
		{"no mode", `{"charter":"c2","runtime":"pod","paused":true}`, []string{"mode"}},
		{"no paused", `{"charter":"c2","runtime":"pod","mode":"shadow"}`, []string{"paused"}},
	} {
		rec := r.hook(t, http.MethodPut, "/api/officers/declared/"+declID, c.body)
		var out struct {
			Fields []fieldRefusal `json:"fields"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		got := []string{}
		for _, f := range out.Fields {
			got = append(got, f.Field)
		}
		if rec.Code != http.StatusBadRequest || strings.Join(got, ",") != strings.Join(c.fields, ",") {
			t.Errorf("%s: %d %s; want 400 naming %v", c.name, rec.Code, rec.Body.String(), c.fields)
		}
	}
	row, err := r.store.GetDeclared(dbtest.AsOperator(context.Background()), 1, declID)
	if err != nil || row.Mode != "shadow" || row.Enabled {
		t.Fatalf("a refused replace changed the officer: %+v %v", row, err)
	}
	rec := r.hook(t, http.MethodPut, "/api/officers/declared/"+declID, `{"charter":"c2","runtime":"pod","mode":"shadow","paused":false}`)
	if v := decodeView(t, rec); rec.Code != http.StatusOK || v.Mode != "shadow" || v.Paused == nil || *v.Paused {
		t.Fatalf("a whole replace: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.hook(t, http.MethodPut, "/api/officers/declared/ghost:none", `{"runtime":"pod"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("a replace of no officer: %d %s; want 404", rec.Code, rec.Body.String())
	}
}

// TestAnExplicitHostMustBeOneOfTheOwners (F7, D2): a "runtime": "host"
// declaration naming a host its owner has no mirror token for is refused on
// the host field — at the create and the replace; the owner's own host is
// taken. runtime-options no longer answers repo_allowed.
func TestAnExplicitHostMustBeOneOfTheOwners(t *testing.T) {
	h := spawnableHost(hostOffHost, true)
	h.AgentClose = true
	r := newDeclRig(t, declRigOpts{hosts: []mirror.HostView{h}})
	host := func(name string) string {
		return `{"id":"` + declID + `","charter":"c","runtime":"host","host":"` + name + `","repo":"` + hostOffRepo + `","mode":"live","paused":false}`
	}
	fields := func(rec interface{ Bytes() []byte }) []string {
		var out struct {
			Fields []fieldRefusal `json:"fields"`
		}
		_ = json.Unmarshal(rec.Bytes(), &out)
		var got []string
		for _, f := range out.Fields {
			got = append(got, f.Field)
		}
		return got
	}
	rec := r.hook(t, http.MethodPost, "/api/officers/declared", host("host-nowhere"))
	if rec.Code != http.StatusBadRequest || strings.Join(fields(rec.Body), ",") != "host" {
		t.Fatalf("a host the owner has none of: %d %s; want 400 on host", rec.Code, rec.Body.String())
	}
	// Another owner's create naming owner 1's host is refused the same way.
	rec = r.as(declOther, r.s.personalDeclared(r.s.declaredCreate), http.MethodPost, "/api/officers/declared", host(hostOffHost), nil)
	if rec.Code != http.StatusBadRequest || strings.Join(fields(rec.Body), ",") != "host" {
		t.Fatalf("another owner's host: %d %s; want 400 on host", rec.Code, rec.Body.String())
	}
	if rec := r.hook(t, http.MethodPost, "/api/officers/declared", host(hostOffHost)); rec.Code != http.StatusCreated {
		t.Fatalf("control: the owner's own host: %d %s", rec.Code, rec.Body.String())
	}
	rec = r.hook(t, http.MethodPut, "/api/officers/declared/"+declID, host("host-nowhere"))
	if rec.Code != http.StatusBadRequest || strings.Join(fields(rec.Body), ",") != "host" {
		t.Fatalf("a replace naming a host the owner has none of: %d %s; want 400 on host", rec.Code, rec.Body.String())
	}
	ro := r.hook(t, http.MethodGet, "/api/officers/runtime-options?repo="+hostOffRepo, "")
	if ro.Code != http.StatusOK || strings.Contains(ro.Body.String(), "repo_allowed") {
		t.Fatalf("runtime-options: %d %s; want no repo_allowed (D2)", ro.Code, ro.Body.String())
	}
}

// TestAnIdleRefreshStillSeesTheFirstDeclaration (F9): a muster with nothing
// declared skips the list on its refresh (one AnyDeclared probe instead); the
// first row declared — here by hand, as another replica's write would land —
// is still read by the next refresh.
func TestAnIdleRefreshStillSeesTheFirstDeclaration(t *testing.T) {
	r := newDeclRig(t, declRigOpts{pods: true})
	ctx := context.Background()
	if err := r.s.reloadDeclaredOfficers(ctx); err != nil {
		t.Fatal(err)
	}
	r.s.officerRT.declMu.Lock()
	idle := r.s.officerRT.loaded && len(r.s.officerRT.declared) == 0
	r.s.officerRT.declMu.Unlock()
	if !idle {
		t.Fatal("control: the empty load did not leave the runtime idle")
	}
	dsn := dbtest.DSN(t)
	dbtest.OwnerExec(t, dsn, `INSERT INTO officer_types (owner_id, key, name) VALUES (1, 'idle-x-tag', 'x')`)
	dbtest.OwnerExec(t, dsn, `INSERT INTO officer_defs (owner_id, officer, type_key, runtime, instance_slug) VALUES (1, 'idle:x', 'idle-x-tag', 'pod', 'idle-x-tag')`)
	r.s.refreshDeclaredOfficers(ctx)
	if _, ok := r.s.officerRT.reg.ForOwner(1, "idle:x"); !ok {
		t.Fatalf("the refresh after an idle one did not read the first declaration\n%s", r.logs)
	}
}
