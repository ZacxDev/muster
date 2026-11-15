package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
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
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/users"
)

// Officers declared in the database (officers redesign O-A, migration 0135),
// through the real server, the real built-in store and the real wake loop's
// pass, with the fleet's agents and provisioner played in memory
// (officerAgents, officerProv) and the mirror hub played by
// hostOfficerMirror.

// declID is the officer these tests declare.
const declID = "scout:main"

// declOther is a second person the tests create, so two owners can declare
// the same id; declTeam is a team.
const (
	declOther = int64(987301)
	declTeam  = int64(987302)
)

// declRig is a server running DB-declared officers.
type declRig struct {
	f     *connFixture
	s     *Server
	h     http.Handler
	store *builtin.Store
	ag    *officerAgents
	prov  *officerProv
	m     *hostOfficerMirror
	logs  *bytes.Buffer
}

// declRigOpts shapes a rig: pods (a provisioner, the claude-code kind, the
// session settings taken), a mirror hub with hosts (its hosts, nil for no
// hub), and env, a MUSTER_OFFICERS roster.
type declRigOpts struct {
	pods  bool
	hosts []mirror.HostView
	hub   bool
	env   string
}

func newDeclRig(t *testing.T, o declRigOpts) *declRig {
	t.Helper()
	f := newConnFixture(t)
	return newDeclRigOn(t, f, o)
}

func newDeclRigOn(t *testing.T, f *connFixture, o declRigOpts) *declRig {
	t.Helper()
	dsn := dbtest.DSN(t)
	wipe := func() {
		dbtest.OwnerExec(t, dsn, `TRUNCATE officer_contact_events, officer_contacts, officer_schedules, officer_items, officer_defs, officer_types RESTART IDENTITY`)
	}
	wipe()
	for _, u := range []struct {
		id   int64
		kind string
	}{{declOther, "person"}, {declTeam, "team"}} {
		dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, kind, role, display_name) OVERRIDING SYSTEM VALUE VALUES (`+
			strconv.FormatInt(u.id, 10)+`, '`+u.kind+`', 'member', 'decl-`+u.kind+`') ON CONFLICT DO NOTHING`)
	}
	t.Cleanup(func() {
		wipe()
		dbtest.OwnerExec(t, dsn, `DELETE FROM users WHERE id IN (987301, 987302)`)
	})
	r := &declRig{f: f, store: builtin.NewStore(f.pool), ag: &officerAgents{}, logs: &bytes.Buffer{}}
	ext := Extensions{Agents: r.ag, SessionLiveness: stubLiveness{}, Users: users.NewPG(f.pool), AgentNamespacePrefix: "muster-agent-"}
	ocfg := OfficersConfig{Builtin: r.store, HostSpawns: officers.NewMemHostSpawnStore()}
	if o.pods {
		r.prov = &officerProv{store: r.ag, phase: map[string]provision.Phase{}}
		ext.Provisioner, ext.Gateway, ext.Kinds = r.prov, &repromptChat{}, stubKinds{pick: "acct-one"}
		ocfg.InstallSession = discardSession
	}
	if o.hub || o.hosts != nil {
		r.m = newHostOfficerMirror()
		r.m.hosts = o.hosts
		ext.Mirror = r.m
	}
	list, err := officers.Parse(o.env, func(string) string { return "" }, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	ocfg.Officers = list
	s := New(nil, AuthConfig{HookToken: connTestHookToken, ServiceToken: connTestServiceToken, UIPassword: connTestPassword}, log.New(r.logs, "", 0))
	s.UseExtensions(ext)
	s.EnableOfficerStore(r.store)
	if _, err := s.EnableOfficers(ocfg); err != nil {
		t.Fatal(err)
	}
	r.s, r.h = s, s.Handler()
	return r
}

// hook is a request through the real handler on the shared hook token: it
// acts as the operator, user 1.
func (r *declRig) hook(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+connTestHookToken)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

// as runs handler f for user uid (the principal a personal token admits), so
// a second owner can be exercised without minting a token.
func (r *declRig) as(uid int64, f http.HandlerFunc, method, path, body string, vals map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range vals {
		req.SetPathValue(k, v)
	}
	req = req.WithContext(authz.WithPrincipal(req.Context(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaToken}))
	rec := httptest.NewRecorder()
	f(rec, req)
	return rec
}

func decodeView(t *testing.T, rec *httptest.ResponseRecorder) declaredOfficerView {
	t.Helper()
	var v declaredOfficerView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("not an officer: %v\n%s", err, rec.Body.String())
	}
	return v
}

// officerFor is owner's officer id in the LIVE registry.
func (r *declRig) officerFor(t *testing.T, owner int64, id string) officers.Officer {
	t.Helper()
	o, ok := r.s.officerRT.reg.ForOwner(owner, id)
	if !ok {
		t.Fatalf("owner %d's %s is not in the registry; logs:\n%s", owner, id, r.logs)
	}
	return o
}

// enqueue puts one open item on owner's officer.
func (r *declRig) enqueue(t *testing.T, owner int64, id, subject string) {
	t.Helper()
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: owner, Role: authz.RoleMember, Via: authz.ViaToken})
	if _, _, err := r.store.Enqueue(ctx, owner, builtin.NewItem{Officer: id, Kind: "scan", SubjectKey: subject}, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// pass runs one wake pass for o (Check, forced), as Run does.
func (r *declRig) pass(t *testing.T, o officers.Officer) officers.PassResult {
	t.Helper()
	res := r.s.officerRT.rec.Check(context.Background(), o.Key(), true)
	if res.Err != nil {
		t.Fatalf("pass for %s: %v\nlogs:\n%s", o.Key(), res.Err, r.logs)
	}
	return res
}

func (r *declRig) agentNamed(name string) (agents.Agent, bool) {
	a, err := r.ag.GetByName(context.Background(), name)
	return a, err == nil
}

const declPodBody = `{"id":"` + declID + `","name":"Scout","charter":"Scan the queue; never post.","runtime":"pod"}`

// TestADeclaredOfficerStartsWithNoRestart: an officer created through the API
// — the type, the def and the runtime declaration in one write — is in the
// live registry at once, and the next wake pass for it starts an instance
// when its queue holds an item (none before), with no restart.
func TestADeclaredOfficerStartsWithNoRestart(t *testing.T) {
	r := newDeclRig(t, declRigOpts{pods: true})
	rec := r.hook(t, http.MethodPost, "/api/officers/declared", declPodBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	v := decodeView(t, rec)
	if !v.Running || v.RunsOn != officers.RuntimePod || v.ManagedBy != officers.ManagedByDB || v.ReadOnly || v.InstanceSlug != "scout-main" || v.Backend != officers.BackendBuiltin {
		t.Fatalf("created view %+v, want running in pods, managed by db, slug scout-main, builtin", v)
	}
	ctx := dbtest.AsOperator(context.Background())
	ty, err := r.store.GetType(ctx, 1, "scout")
	if err != nil || ty.Charter != "Scan the queue; never post." || ty.Name != "Scout" {
		t.Fatalf("the type was not written with the officer: %+v, %v", ty, err)
	}
	o := r.officerFor(t, 1, declID)
	if !o.FromDB() || !o.IsBuiltin() || o.OnHost() || o.Tiers != nil {
		t.Fatalf("registry officer %+v, want a builtin, untiered pod officer from the db", o)
	}
	// Idle: nothing to do, nothing starts (CONTROL).
	if res := r.pass(t, o); len(res.Decision.Start) != 0 {
		t.Fatalf("an idle officer started %v", res.Decision.Start)
	}
	r.enqueue(t, 1, declID, "first")
	res := r.pass(t, o)
	if len(res.Decision.Start) != 1 || len(res.ActErrs) != 0 {
		t.Fatalf("with an item, the pass decided %+v (errors %v); want one start\n%s", res.Decision, res.ActErrs, r.logs)
	}
	if _, ok := r.agentNamed("officer-scout-main-1"); !ok {
		t.Fatalf("no instance agent officer-scout-main-1 was created")
	}
	// The dispatch runs in the background (createAndDispatchAgent).
	officerWaitFor(t, func() bool { d, _, _ := r.prov.counts(); return d == 1 })
}

// TestPauseStopsAndResumeRestartsADeclaredOfficer: pause is the officer's
// switch off — the next pass stops its up instance and starts none while its
// queue still holds work; resume switches it on, and the next pass starts it
// again. The officer stays in the registry throughout.
func TestPauseStopsAndResumeRestartsADeclaredOfficer(t *testing.T) {
	r := newDeclRig(t, declRigOpts{pods: true})
	if rec := r.hook(t, http.MethodPost, "/api/officers/declared", declPodBody); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	o := r.officerFor(t, 1, declID)
	r.enqueue(t, 1, declID, "work")
	if res := r.pass(t, o); len(res.Decision.Start) != 1 {
		t.Fatalf("control: the first pass started %v", res.Decision.Start)
	}
	rec := r.hook(t, http.MethodPost, "/api/officers/declared/"+declID+"/pause", "")
	if rec.Code != http.StatusOK || decodeView(t, rec).Paused == nil || !*decodeView(t, rec).Paused {
		t.Fatalf("pause: %d %s", rec.Code, rec.Body.String())
	}
	res := r.pass(t, o)
	if len(res.Decision.Stop) != 1 || len(res.Decision.Start) != 0 {
		t.Fatalf("a paused officer's pass decided %+v; want its instance stopped and none started", res.Decision)
	}
	if res := r.pass(t, o); len(res.Decision.Start) != 0 {
		t.Fatalf("a paused officer with work started %v", res.Decision.Start)
	}
	if _, ok := r.s.officerRT.reg.ForOwner(1, declID); !ok {
		t.Fatal("a paused officer left the registry; it must stay (its machine stays gated, its queue kept)")
	}
	rec = r.hook(t, http.MethodPost, "/api/officers/declared/"+declID+"/resume", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("resume: %d %s", rec.Code, rec.Body.String())
	}
	if res := r.pass(t, o); len(res.Decision.Start) != 1 {
		t.Fatalf("a resumed officer with work started %v, want its instance", res.Decision.Start)
	}
	_, starts, stops := r.prov.counts()
	if starts != 1 || stops < 1 {
		t.Fatalf("provisioner starts %d stops %d; want the instance stopped, then started again", starts, stops)
	}
}

// TestDeletingADeclaredOfficerCleansUp: delete removes the officer from the
// registry, its rows (def, type, items, schedules) from the database, and
// tears its instance agent down; a pass for it then finds no officer.
func TestDeletingADeclaredOfficerCleansUp(t *testing.T) {
	r := newDeclRig(t, declRigOpts{pods: true})
	if rec := r.hook(t, http.MethodPost, "/api/officers/declared", declPodBody); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	o := r.officerFor(t, 1, declID)
	r.enqueue(t, 1, declID, "work")
	r.pass(t, o)
	inst, ok := r.agentNamed("officer-scout-main-1")
	if !ok {
		t.Fatal("control: no instance was started")
	}
	ctx := dbtest.AsOperator(context.Background())
	if _, err := r.store.PutSchedule(ctx, 1, declID, builtin.Schedule{Name: "nightly", Cron: "0 3 * * *", Kind: "scan", SubjectKey: "nightly", SkipIfLive: true, Enabled: true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	rec := r.hook(t, http.MethodDelete, "/api/officers/declared/"+declID, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "officer-scout-main-1") {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if !waitDestroyed(r.prov, inst.ID) {
		t.Fatalf("the instance agent %d was not torn down", inst.ID)
	}
	if _, ok := r.s.officerRT.reg.ForOwner(1, declID); ok {
		t.Fatal("the deleted officer is still in the registry")
	}
	if res := r.s.officerRT.rec.Check(context.Background(), o.Key(), true); !errors.Is(res.Err, officers.ErrUnknownOfficer) {
		t.Fatalf("a pass for the deleted officer: %v, want ErrUnknownOfficer", res.Err)
	}
	for tbl, q := range map[string]string{
		"officer_defs":      `SELECT count(*) FROM officer_defs WHERE owner_id = 1`,
		"officer_types":     `SELECT count(*) FROM officer_types WHERE owner_id = 1`,
		"officer_items":     `SELECT count(*) FROM officer_items WHERE owner_id = 1`,
		"officer_schedules": `SELECT count(*) FROM officer_schedules WHERE owner_id = 1`,
	} {
		var n int
		if err := r.f.pool.QueryRow(ctx, q).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s keeps %d rows of the deleted officer (%v)", tbl, n, err)
		}
	}
	// A start for the deleted officer, from a pass that read the roster before
	// the delete, is refused: no orphan agent.
	if err := (officerFleet{s: r.s}).Start(context.Background(), o, 2, "kick"); err == nil {
		t.Fatal("a start for a deleted officer was let through")
	}
	if _, ok := r.agentNamed("officer-scout-main-2"); ok {
		t.Fatal("a start for a deleted officer created an agent")
	}
}

// TestTwoOwnersMayDeclareTheSameOfficerID (B5): two owners each create
// scout:main; they get distinct server-allocated slugs, distinct instance
// agents and their own backends — an item on the SECOND owner's queue starts
// the second owner's instance, never the first's.
func TestTwoOwnersMayDeclareTheSameOfficerID(t *testing.T) {
	r := newDeclRig(t, declRigOpts{pods: true})
	if rec := r.hook(t, http.MethodPost, "/api/officers/declared", declPodBody); rec.Code != http.StatusCreated {
		t.Fatalf("owner 1's create: %d %s", rec.Code, rec.Body.String())
	}
	o1 := r.officerFor(t, 1, declID)
	// Owner 1's backend is built (and cached) first.
	r.pass(t, o1)
	rec := r.as(declOther, r.s.personalDeclared(r.s.declaredCreate), http.MethodPost, "/api/officers/declared", declPodBody, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("the second owner's create of the same id: %d %s", rec.Code, rec.Body.String())
	}
	o2 := r.officerFor(t, declOther, declID)
	if o1.InstanceSlug == o2.InstanceSlug || o1.InstanceName(1) == o2.InstanceName(1) || o1.Key() == o2.Key() {
		t.Fatalf("two owners' %s share slug %q / %q", declID, o1.InstanceSlug, o2.InstanceSlug)
	}
	if p, _, ok := r.s.officerRT.reg.ForAgentName(o2.InstanceName(1)); !ok || p.Owner != declOther {
		t.Fatalf("the second owner's instance name binds to %+v, want the second owner's officer", p)
	}
	r.enqueue(t, declOther, declID, "theirs")
	if res := r.pass(t, o2); len(res.Decision.Start) != 1 {
		t.Fatalf("an item on the second owner's queue started %v for their officer; logs:\n%s", res.Decision.Start, r.logs)
	}
	if res := r.pass(t, o1); len(res.Decision.Start) != 0 {
		t.Fatalf("the second owner's item woke the FIRST owner's officer: %v", res.Decision.Start)
	}
	if _, ok := r.agentNamed(o2.InstanceName(1)); !ok {
		t.Fatalf("no agent %s for the second owner", o2.InstanceName(1))
	}
	if _, ok := r.agentNamed(o1.InstanceName(1)); ok {
		t.Fatalf("the first owner's instance %s was created for the second owner's item", o1.InstanceName(1))
	}
}

// TestADeletedSlugReusedByAnotherOwnerGetsItsOwnBackend: owner 1's scout:main
// (slug scout-main) runs and is deleted; another owner then creates
// scout:main and is given the freed slug. Its backend must be its own, not
// the cached one owner 1's officer was built with (Backends.Forget on the
// swap): its item starts its instance.
func TestADeletedSlugReusedByAnotherOwnerGetsItsOwnBackend(t *testing.T) {
	r := newDeclRig(t, declRigOpts{pods: true})
	if rec := r.hook(t, http.MethodPost, "/api/officers/declared", declPodBody); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	r.pass(t, r.officerFor(t, 1, declID)) // builds and caches owner 1's backend
	if rec := r.hook(t, http.MethodDelete, "/api/officers/declared/"+declID, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	rec := r.as(declOther, r.s.personalDeclared(r.s.declaredCreate), http.MethodPost, "/api/officers/declared", declPodBody, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("the other owner's create: %d %s", rec.Code, rec.Body.String())
	}
	o := r.officerFor(t, declOther, declID)
	if o.InstanceSlug != "scout-main" {
		t.Fatalf("the freed slug was not reused (got %q): this test needs the reuse", o.InstanceSlug)
	}
	r.enqueue(t, declOther, declID, "theirs")
	if res := r.pass(t, o); len(res.Decision.Start) != 1 {
		t.Fatalf("the reused slug's officer read another owner's queue: started %v\n%s", res.Decision.Start, r.logs)
	}
}

// TestAnEnvOfficerIsReadOnlyAndWinsAClash: MUSTER_OFFICERS declares
// scout:main for user 1. The API lists it read-only (managed_by env), and
// refuses to create, replace, pause or delete it (409); a DB row declaring the
// same (owner, id) does not run — the env officer does — and says why.
// Another owner's scout:main is not a clash, but cannot take the env
// officer's slug.
func TestAnEnvOfficerIsReadOnlyAndWinsAClash(t *testing.T) {
	r := newDeclRig(t, declRigOpts{pods: true, env: `[{"id":"` + declID + `","backend":"builtin"}]`})
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/officers/declared", declPodBody},
		{http.MethodPut, "/api/officers/declared/" + declID, declPodBody},
		{http.MethodPost, "/api/officers/declared/" + declID + "/pause", ""},
		{http.MethodDelete, "/api/officers/declared/" + declID, ""},
	} {
		rec := r.hook(t, c.method, c.path, c.body)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"managed_by":"env"`) {
			t.Errorf("%s %s on an env officer: %d %s; want 409 managed_by env", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	rec := r.hook(t, http.MethodGet, "/api/officers/declared", "")
	var list struct{ Officers []declaredOfficerView }
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Officers) != 1 || list.Officers[0].ManagedBy != officers.ManagedByEnv || !list.Officers[0].ReadOnly {
		t.Fatalf("list: %d %s; want the env officer, read-only", rec.Code, rec.Body.String())
	}
	// A DB row declaring the same (owner, id), written by hand.
	dbtest.OwnerExec(t, dbtest.DSN(t), `INSERT INTO officer_types (owner_id, key, name) VALUES (1, 'scout', 'Scout') ON CONFLICT DO NOTHING`)
	dbtest.OwnerExec(t, dbtest.DSN(t), `INSERT INTO officer_defs (owner_id, officer, type_key, runtime, instance_slug) VALUES (1, '`+declID+`', 'scout', 'pod', 'scout-main-db')`)
	if err := r.s.reloadDeclaredOfficers(context.Background()); err != nil {
		t.Fatal(err)
	}
	o := r.officerFor(t, 1, declID)
	if o.FromDB() || o.InstanceSlug != "" {
		t.Fatalf("owner 1's %s in the registry is %+v; want the env officer", declID, o)
	}
	for _, x := range r.s.officerRT.reg.All() {
		if x.InstanceSlug == "scout-main-db" {
			t.Fatal("the clashing DB row runs beside the env officer")
		}
	}
	if st, ok := r.s.officerRT.declaredView(1, declID); !ok || st.officer != nil || !strings.Contains(st.reason, "env officer wins") {
		t.Fatalf("the clashing row's state %+v; want not running, saying the env officer wins", st)
	}
	// Another owner: their own scout:main, never the env officer's slug.
	rec = r.as(declOther, r.s.personalDeclared(r.s.declaredCreate), http.MethodPost, "/api/officers/declared", declPodBody, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("another owner's scout:main: %d %s", rec.Code, rec.Body.String())
	}
	if v := decodeView(t, rec); v.InstanceSlug == officers.Slug(declID) || !v.Running {
		t.Fatalf("another owner's officer took slug %q (running %v); the env officer's is %q", v.InstanceSlug, v.Running, officers.Slug(declID))
	}
}

// TestADeclaredOfficersDefReadsDeclaredAtOnce: a DB-declared officer's def
// reads declared (managed_by db) in GET /api/officers/defs the moment it is
// created — the flag reads the live roster, not the boot one.
func TestADeclaredOfficersDefReadsDeclaredAtOnce(t *testing.T) {
	r := newDeclRig(t, declRigOpts{pods: true})
	body := `{"id":"other:one","charter":"c","runtime":"pod"}`
	if rec := r.as(declOther, r.s.personalDeclared(r.s.declaredCreate), http.MethodPost, "/api/officers/declared", body, nil); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	rec := r.as(declOther, r.s.handleOfficerDefs, http.MethodGet, "/api/officers/defs", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"officer":"other:one"`) ||
		!strings.Contains(rec.Body.String(), `"declared":true,"managed_by":"db"`) {
		t.Fatalf("a DB-declared officer's def: %d %s; want declared, managed_by db", rec.Code, rec.Body.String())
	}
	// Another owner's view of the same id: not theirs, not declared.
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaToken})
	if err := r.store.PutType(ctx, 1, builtin.Type{Key: "other", Name: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := r.store.PutDef(ctx, 1, builtin.Def{Officer: "other:one", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if rec := r.hook(t, http.MethodGet, "/api/officers/defs", ""); strings.Contains(rec.Body.String(), `"declared":true`) {
		t.Fatalf("user 1's def of the same id reads declared because ANOTHER owner declared it: %s", rec.Body.String())
	}
}

// TestCreateRefusesPerFieldWithTheDeclarationChecks: the API refuses what the
// boot would (the one declaration check), naming each field: a host officer
// with no repo (B6) and a model; a bad repo; a pod officer naming a host; a
// bad duration; and an id whose instance names cannot fit the namespace.
func TestCreateRefusesPerFieldWithTheDeclarationChecks(t *testing.T) {
	r := newDeclRig(t, declRigOpts{pods: true, hub: true})
	for _, c := range []struct {
		name, body string
		fields     []string
	}{
		{"host without repo, with model", `{"id":"` + declID + `","runtime":"host","host":"` + hostOffHost + `","model":"opus"}`, []string{"repo", "model"}},
		{"bad repo", `{"id":"` + declID + `","runtime":"pod","repo":"not a repo"}`, []string{"repo"}},
		{"pod naming a host", `{"id":"` + declID + `","runtime":"pod","host":"` + hostOffHost + `"}`, []string{"host"}},
		{"bad poll", `{"id":"` + declID + `","runtime":"pod","poll_interval":"1s"}`, []string{"poll_interval"}},
		{"unknown runtime", `{"id":"` + declID + `","runtime":"vm"}`, []string{"runtime"}},
		{"bad id", `{"id":"Scout","runtime":"pod"}`, []string{"id"}},
		{"too many instances", `{"id":"` + declID + `","runtime":"pod","max_instances":9}`, []string{"max_instances"}},
	} {
		rec := r.hook(t, http.MethodPost, "/api/officers/declared", c.body)
		var out struct {
			Fields []fieldRefusal `json:"fields"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		got := map[string]bool{}
		for _, f := range out.Fields {
			got[f.Field] = true
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s; want 400", c.name, rec.Code, rec.Body.String())
			continue
		}
		for _, f := range c.fields {
			if !got[f] {
				t.Errorf("%s: fields %v, want %q named (%s)", c.name, out.Fields, f, rec.Body.String())
			}
		}
	}
	if n := len(r.s.officerRT.reg.All()); n != 0 {
		t.Fatalf("refused creates left %d officers in the registry", n)
	}
	// A long id is not refused: its slug is CUT so its highest instance name
	// fits a namespace under the deployment's prefix (the server allocates
	// it, so the user's id need not).
	long := strings.Repeat("a", 30) + ":" + strings.Repeat("b", 30)
	rec := r.hook(t, http.MethodPost, "/api/officers/declared", `{"id":"`+long+`","runtime":"pod","max_instances":8}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("a long id: %d %s", rec.Code, rec.Body.String())
	}
	v := decodeView(t, rec)
	if top := v.Instances[len(v.Instances)-1]; len("muster-agent-"+top) > officers.MaxNamespaceLen || !strings.HasPrefix(top, "officer-aaaa") {
		t.Fatalf("a long id's highest instance %q does not fit a namespace (%d)", top, len("muster-agent-"+top))
	}
}

// TestTheStepUpGateFollowsDeclaredHostOfficers (F8): the mirror door's
// step-up gate is keyed on the host officers declared for a machine — read
// live, so a host officer created in the database gates its machine at once,
// a paused one still does (its window may be open), and a deleted one no
// longer does.
func TestTheStepUpGateFollowsDeclaredHostOfficers(t *testing.T) {
	h := spawnableHost(hostOffHost, true)
	h.AgentClose = true
	r := newDeclRig(t, declRigOpts{hosts: []mirror.HostView{h}})
	if got := r.s.hostOfficersOnMachine(hostOffOwner, hostOffHost); len(got) != 0 {
		t.Fatalf("control: the machine is gated before any officer: %v", got)
	}
	body := `{"id":"` + declID + `","charter":"c","runtime":"host","host":"` + hostOffHost + `","repo":"` + hostOffRepo + `"}`
	if rec := r.hook(t, http.MethodPost, "/api/officers/declared", body); rec.Code != http.StatusCreated || !decodeView(t, rec).Running {
		t.Fatalf("create a host officer: %d %s", rec.Code, rec.Body.String())
	}
	if got := r.s.hostOfficersOnMachine(hostOffOwner, hostOffHost); len(got) != 1 || got[0] != declID {
		t.Fatalf("after the create the machine's gate names %v, want %s", got, declID)
	}
	if !r.s.hostOfficersOn() {
		t.Fatal("hostOfficersOn does not see the declared host officer")
	}
	if rec := r.hook(t, http.MethodPost, "/api/officers/declared/"+declID+"/pause", ""); rec.Code != http.StatusOK {
		t.Fatalf("pause: %d %s", rec.Code, rec.Body.String())
	}
	if got := r.s.hostOfficersOnMachine(hostOffOwner, hostOffHost); len(got) != 1 {
		t.Fatalf("a paused host officer no longer gates its machine: %v", got)
	}
	if rec := r.hook(t, http.MethodDelete, "/api/officers/declared/"+declID, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if got := r.s.hostOfficersOnMachine(hostOffOwner, hostOffHost); len(got) != 0 {
		t.Fatalf("a deleted host officer still gates its machine: %v", got)
	}
}

// TestRuntimeAutoDetection is the matrix: a provisioner only, a qualifying
// host only, both, neither — runtime-options' answer and where an auto
// officer resolves, each with its reason.
func TestRuntimeAutoDetection(t *testing.T) {
	f := newConnFixture(t)
	good := spawnableHost(hostOffHost, true)
	good.AgentClose = true
	noClose := spawnableHost(hostOffOther, true) // spawns, never closes
	auto := `{"id":"` + declID + `","charter":"c","repo":"` + hostOffRepo + `"}`
	for _, c := range []struct {
		name                  string
		opts                  declRigOpts
		podOK                 bool
		autoRuntime, autoHost string
		reasons               []string
	}{
		{"provisioner only", declRigOpts{pods: true}, true, officers.RuntimePod, "", []string{"no mirror hub"}},
		{"a qualifying host only", declRigOpts{hosts: []mirror.HostView{noClose, good}}, false, officers.RuntimeHost, hostOffHost, []string{"no agent provisioner", "does not close windows"}},
		{"both", declRigOpts{pods: true, hosts: []mirror.HostView{good}}, true, officers.RuntimePod, "", nil},
		{"neither", declRigOpts{hosts: []mirror.HostView{noClose}}, false, "", "", []string{"no agent provisioner", "none of your hosts qualifies", "does not close windows"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newDeclRigOn(t, f, c.opts)
			rec := r.hook(t, http.MethodGet, "/api/officers/runtime-options?repo="+hostOffRepo, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("runtime-options: %d %s", rec.Code, rec.Body.String())
			}
			var ro runtimeOptions
			if err := json.Unmarshal(rec.Body.Bytes(), &ro); err != nil {
				t.Fatal(err)
			}
			if ro.Pod.Available != c.podOK || ro.Auto.Runtime != c.autoRuntime || ro.Auto.Host != c.autoHost {
				t.Fatalf("runtime-options %s; want pod %v, auto %q %q", rec.Body.String(), c.podOK, c.autoRuntime, c.autoHost)
			}
			for _, want := range c.reasons {
				if !strings.Contains(rec.Body.String(), want) {
					t.Errorf("runtime-options does not say %q: %s", want, rec.Body.String())
				}
			}
			// The wake loop's resolution of a stored "auto" agrees.
			crec := r.hook(t, http.MethodPost, "/api/officers/declared", auto)
			if crec.Code != http.StatusCreated {
				t.Fatalf("create auto: %d %s", crec.Code, crec.Body.String())
			}
			v := decodeView(t, crec)
			if v.Runtime != officers.RuntimeAuto || v.RunsOn != c.autoRuntime || v.RunsOnHost != c.autoHost || v.Running != (c.autoRuntime != "") {
				t.Fatalf("auto officer %+v; want runs on %q %q", v, c.autoRuntime, c.autoHost)
			}
			if c.autoRuntime == "" && !strings.Contains(v.Reason, "runtime auto found nowhere to run") {
				t.Fatalf("an unrunnable auto officer's reason %q does not say so", v.Reason)
			}
		})
	}
}

// TestTeamOfficerRules: a team's manager creates a TEAM pod officer through
// the team door (owned by the team); an operator of the team is refused; a
// team cannot own a host officer — at the door (400, naming the runtime) and
// at the roster load for a row written by hand (officers.CheckOwners, the one
// rule).
func TestTeamOfficerRules(t *testing.T) {
	w := newIsoWorld(t)
	team := w.aTeam
	w.setRole(t, team, w.b.uid, authz.TeamOperator)
	st := builtin.NewStore(w.pool)
	if _, err := w.srv.EnableOfficers(OfficersConfig{Builtin: st, HostSpawns: officers.NewMemHostSpawnStore()}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM officer_defs WHERE owner_id = $1`, `DELETE FROM officer_types WHERE owner_id = $1`} {
			_, _ = w.pool.Exec(authz.System(context.Background()), q, team)
		}
	})
	path := "/teams/" + strconv.FormatInt(team, 10) + "/officers/declared"
	pod := `{"id":"tm:pod","charter":"team charter","runtime":"pod"}`
	if rec := w.do(w.h, http.MethodPost, path, pod, cred{sessionUser: w.b.uid}); rec.Code != http.StatusForbidden {
		t.Fatalf("an operator's create of a team officer: %d %s; want 403", rec.Code, rec.Body.String())
	}
	rec := w.do(w.h, http.MethodPost, path, pod, cred{sessionUser: w.a.uid})
	if rec.Code != http.StatusCreated {
		t.Fatalf("the manager's create of a team pod officer: %d %s", rec.Code, rec.Body.String())
	}
	if v := decodeView(t, rec); v.Owner != team {
		t.Fatalf("the team officer is owned by %d, want the team %d", v.Owner, team)
	}
	if _, err := st.GetDeclared(w.sys, team, "tm:pod"); err != nil {
		t.Fatalf("the team's officer row: %v", err)
	}
	host := `{"id":"tm:host","charter":"c","runtime":"host","host":"` + hostOffHost + `","repo":"` + hostOffRepo + `"}`
	rec = w.do(w.h, http.MethodPost, path, host, cred{sessionUser: w.a.uid})
	var refused struct {
		Fields []fieldRefusal `json:"fields"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &refused)
	if rec.Code != http.StatusBadRequest || len(refused.Fields) != 1 || refused.Fields[0].Field != "runtime" || !strings.Contains(refused.Fields[0].Error, officers.ErrTeamHostOfficer.Error()) {
		t.Fatalf("a team host officer: %d %s; want 400 on runtime with %q", rec.Code, rec.Body.String(), officers.ErrTeamHostOfficer)
	}
	// By hand, past the door: the roster load refuses to run it.
	if _, err := w.pool.Exec(authz.System(context.Background()), `INSERT INTO officer_types (owner_id, key, name) VALUES ($1, 'tmh', 'x') ON CONFLICT DO NOTHING`, team); err != nil {
		t.Fatal(err)
	}
	if _, err := w.pool.Exec(authz.System(context.Background()), `INSERT INTO officer_defs (owner_id, officer, type_key, runtime, host, repo, instance_slug)
		VALUES ($1, 'tmh:x', 'tmh', 'host', $2, $3, 'tmh-x-iso')`, team, hostOffHost, hostOffRepo); err != nil {
		t.Fatal(err)
	}
	if err := w.srv.reloadDeclaredOfficers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stt, ok := w.srv.officerRT.declaredView(team, "tmh:x"); !ok || stt.officer != nil || !strings.Contains(stt.reason, officers.ErrTeamHostOfficer.Error()) {
		t.Fatalf("a hand-written team host officer: %+v; want not running", stt)
	}
}

// TestTheRegistrySwapIsRaceFreeUnderTheWakeLoop: the wake loop's passes and
// readers of the roster run while officers are created, paused and deleted
// and the roster reloaded. Run under -race (the scoped command in the PR).
func TestTheRegistrySwapIsRaceFreeUnderTheWakeLoop(t *testing.T) {
	r := newDeclRig(t, declRigOpts{pods: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			for _, o := range r.s.officerRT.reg.All() {
				r.s.officerRT.rec.Check(ctx, o.Key(), true)
				r.s.officerRT.reg.ForAgentName(o.InstanceName(1))
			}
			r.s.hostOfficersOn()
			r.s.hostOfficersOnMachine(1, hostOffHost)
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			r.s.refreshDeclaredOfficers(ctx)
			time.Sleep(2 * time.Millisecond)
		}
	}()
	for i := 0; i < 15; i++ {
		id := "race:o" + strconv.Itoa(i)
		if rec := r.hook(t, http.MethodPost, "/api/officers/declared", `{"id":"`+id+`","charter":"c","runtime":"pod"}`); rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", id, rec.Code, rec.Body.String())
		}
		r.enqueue(t, 1, id, "x")
		r.hook(t, http.MethodPost, "/api/officers/declared/"+id+"/pause", "")
		if i%2 == 0 {
			r.hook(t, http.MethodDelete, "/api/officers/declared/"+id, "")
		}
	}
	cancel()
	wg.Wait()
	if n := len(r.s.officerRT.reg.All()); n != 7 {
		t.Fatalf("after 15 creates and 8 deletes the registry holds %d officers, want 7", n)
	}
}
