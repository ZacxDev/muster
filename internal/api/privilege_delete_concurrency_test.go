package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/privilege"
)

// ---------------------------------------------------------------------------
// THE PROFILE DELETE, AGAINST THE THREE THINGS IT USED TO GET WRONG.
//
// 🔴 WHY THESE FAKES ARE STATEFUL WHERE privilege_writes_test.go's ARE NOT. That
// file pins the ORDER of two calls, and a call log is the right instrument for an
// ordering claim. The defects here are about a reachable END STATE — live RBAC that
// no record names — which a call log cannot express: the same sequence of calls is
// safe or catastrophic depending on what rows and what cluster objects exist when it
// finishes. So racingStore models the two tables INCLUDING the foreign key and the
// ON DELETE CASCADE that agent_privileges declares, and racingApplier models the set
// of live objects. The assertion is then over state, not over calls.
//
// ⚠ MODELLING THE SCHEMA IS NOT DERIVING THE EXPECTATION FROM THE IMPLEMENTATION.
// The cascade and the FK are facts about internal/db/migrations/0001_init.sql, which
// no code in internal/api can change; the expected end state is pinned literally,
// from the invariant, and would be the same against any correct handler.
// ---------------------------------------------------------------------------

// interleaveBudget is how long the delete's holder-snapshot waits for a concurrent
// grant to record before giving up.
//
// 🔴 THE ASYMMETRY IS THE POINT, AND IT IS WHY THIS IS NOT A FLAKY TIMING TEST.
// Against the UNFIXED handler the wait is not a timeout at all: the interleaving
// grant is not blocked by anything, so it records and signals, and the wait returns
// on the signal. The red leg is therefore DETERMINISTIC, not a race this test hopes
// to win. Against the FIXED handler the grant is parked on the profile lock and the
// signal can never arrive, so the wait always runs to the full budget — the cost is
// paid once, on the green leg, and buys a red leg with a ~6-order-of-magnitude
// margin (the unfixed path needs a handful of in-memory fake calls, microseconds).
const interleaveBudget = 2 * time.Second

// grantKey is (agent_id, profile_id) — agent_privileges' unique pair.
type grantKey struct{ agentID, profileID int64 }

// racingStore is privilege.Store over two in-memory maps that honour the schema's
// foreign key and its ON DELETE CASCADE, plus a rendezvous that lets a test place
// a concurrent grant exactly inside the delete's window.
type racingStore struct {
	privilege.Store

	mu       sync.Mutex
	profiles map[int64]privilege.Profile
	grants   map[grantKey]bool

	// listed is closed once the delete has taken its holder snapshot, which is the
	// moment the interleaving grant must be allowed to start.
	listed chan struct{}
	// recorded is closed once that grant has been written. nil disables the whole
	// rendezvous, for the tests that are not about concurrency.
	recorded     chan struct{}
	recordedOnce sync.Once
	listedOnce   sync.Once

	deleteErr error

	// deleteDelay holds DeleteProfile open for a while before it takes effect.
	//
	// 🔴 IT IS DETECTOR SENSITIVITY, NOT A TIMING ASSERTION, AND IT IS ONE-SIDED BY
	// CONSTRUCTION. The reapply is ASYNC (safeGo), so "did it run before the delete?"
	// is observed from a goroutine and cannot be pinned by ordering alone. Measured:
	// against a mutant that moves the reapply loop ABOVE DeleteProfile, the ordering
	// assertion fired in 1 run out of 20 — reachable, but a 5% detector. Holding the
	// delete open widens the mutant's window to the whole delay, taking that to 20/20.
	// It CANNOT produce a false positive: under correct code reapplyEnvAsync is not
	// even called until DeleteProfile has returned, so no goroutine exists during the
	// delay and there is nothing for the delay to perturb.
	deleteDelay time.Duration
}

func newRacingStore(prof privilege.Profile, holders []int64) *racingStore {
	s := &racingStore{
		profiles: map[int64]privilege.Profile{prof.ID: prof},
		grants:   map[grantKey]bool{},
		listed:   make(chan struct{}),
		recorded: make(chan struct{}),
	}
	for _, a := range holders {
		s.grants[grantKey{a, prof.ID}] = true
	}
	return s
}

func (s *racingStore) GetProfile(_ context.Context, id int64) (privilege.Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[id]
	if !ok {
		return privilege.Profile{}, pgx.ErrNoRows
	}
	return p, nil
}

func (s *racingStore) ListProfiles(context.Context) ([]privilege.Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]privilege.Profile, 0, len(s.profiles))
	for _, p := range s.profiles {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ListGrantsForProfile is the snapshot handleProfileDelete revokes from, and the
// rendezvous point: once it has read the rows, the other tab is released.
func (s *racingStore) ListGrantsForProfile(_ context.Context, profileID int64) ([]privilege.Grant, error) {
	s.mu.Lock()
	name := s.profiles[profileID].Name
	out := make([]privilege.Grant, 0, len(s.grants))
	for k := range s.grants {
		if k.profileID == profileID {
			out = append(out, privilege.Grant{AgentID: k.agentID, ProfileID: profileID, ProfileName: name})
		}
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })

	if s.listed != nil {
		s.listedOnce.Do(func() { close(s.listed) })
		select {
		case <-s.recorded:
		case <-time.After(interleaveBudget):
		}
	}
	return out, nil
}

func (s *racingStore) ListGrantsForAgent(_ context.Context, agentID int64) ([]privilege.Grant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]privilege.Grant, 0)
	for k := range s.grants {
		if k.agentID == agentID {
			out = append(out, privilege.Grant{
				AgentID: k.agentID, ProfileID: k.profileID,
				ProfileName: s.profiles[k.profileID].Name,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProfileID < out[j].ProfileID })
	return out, nil
}

// Grant honours the foreign key: agent_privileges.profile_id REFERENCES
// privilege_profiles (id), so recording a grant for a deleted profile FAILS.
func (s *racingStore) Grant(_ context.Context, agentID, profileID int64, _ string) (privilege.Grant, error) {
	s.mu.Lock()
	_, ok := s.profiles[profileID]
	if ok {
		s.grants[grantKey{agentID, profileID}] = true
	}
	s.mu.Unlock()
	if !ok {
		return privilege.Grant{}, errors.New(`insert or update on table "agent_privileges" ` +
			`violates foreign key constraint "agent_privileges_profile_id_fkey"`)
	}
	if s.recorded != nil {
		s.recordedOnce.Do(func() { close(s.recorded) })
	}
	return privilege.Grant{AgentID: agentID, ProfileID: profileID}, nil
}

func (s *racingStore) Revoke(_ context.Context, agentID, profileID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.grants, grantKey{agentID, profileID})
	return nil
}

// DeleteProfile drops the profile and, as ON DELETE CASCADE does, its grant rows.
func (s *racingStore) DeleteProfile(_ context.Context, id int64) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	if s.deleteDelay > 0 {
		time.Sleep(s.deleteDelay)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.profiles, id)
	for k := range s.grants {
		if k.profileID == id {
			delete(s.grants, k)
		}
	}
	return nil
}

func (s *racingStore) liveGrants() map[grantKey]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[grantKey]bool, len(s.grants))
	for k := range s.grants {
		out[k] = true
	}
	return out
}

// racingApplier models the cluster: the set of (agent, profile) pairs that have
// RBAC objects live right now.
type racingApplier struct {
	mu   sync.Mutex
	live map[string]bool
}

func newRacingApplier() *racingApplier {
	return &racingApplier{live: map[string]bool{}}
}

func (a *racingApplier) ApplyGrant(_ context.Context, agentName, _ string, prof privilege.Profile) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.live[agentName+"/"+prof.Name] = true
	return nil
}

func (a *racingApplier) RemoveGrant(_ context.Context, agentName, _, profileName string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.live, agentName+"/"+profileName)
	return nil
}

func (a *racingApplier) liveSet() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.live))
	for k := range a.live {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// racingAgents is a fixed roster; names are distinct so an assertion cannot be
// satisfied by the wrong agent.
type racingAgents struct {
	agents.Store
	byID map[int64]agents.Agent
}

func (f *racingAgents) Get(_ context.Context, id int64) (agents.Agent, error) {
	a, ok := f.byID[id]
	if !ok {
		return agents.Agent{}, errors.New("no such agent")
	}
	return a, nil
}

func racingRoster(status string) map[int64]agents.Agent {
	mk := func(id int64, name string) agents.Agent {
		return agents.Agent{ID: id, Name: name, Namespace: "devpod-" + name, Status: status}
	}
	return map[int64]agents.Agent{
		11: mk(11, "alpha"),
		22: mk(22, "bravo"),
		33: mk(33, "charlie"),
	}
}

func grantRequest(agentID, profileID string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/agents/"+agentID+"/grants",
		strings.NewReader("profile_id="+profileID))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetPathValue("id", agentID)
	return r
}

// TestAGrantInterleavedWithAProfileDeleteNeverLeavesRBACNoRecordNames is the
// regression test for issue #12.
//
// 🔴 RED AGAINST origin/main, AND THE MECHANISM IS THE ONE THE ISSUE NAMES. The
// unfixed handler lists the holders, revokes them, and then deletes — with nothing
// serialising it against handleAgentGrant. This test releases a grant for a THIRD
// agent at the instant the snapshot is taken, so that grant is not in the snapshot,
// its RBAC is applied, and the delete's cascade then takes the row that was the only
// thing able to name it. End state on origin/main: `charlie/ops-readonly` live in the
// cluster with zero grant rows.
//
// 🔴 THE ASSERTION IS THE INVARIANT, NOT AN EXPECTED CALL SEQUENCE — "every live RBAC
// object is named by a grant row that still exists". Both acceptable outcomes pass it:
// the grant may win (row + RBAC) or lose (no row, no RBAC). Only the forbidden
// direction fails. That matters because a handler could legitimately be fixed either
// by making the grant wait or by making it fail, and a test that pinned one of those
// would be a guard against an implementation rather than against the hazard.
func TestAGrantInterleavedWithAProfileDeleteNeverLeavesRBACNoRecordNames(t *testing.T) {
	prof := privilege.Profile{
		ID:   7,
		Name: "ops-readonly",
		Spec: privilege.Spec{ClusterRules: []privilege.PolicyRule{{
			APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"},
		}}},
	}
	store := newRacingStore(prof, []int64{11, 22})
	applier := newRacingApplier()
	roster := racingRoster(agents.StatusStopped)

	// The two holders' RBAC is live before the delete starts, as it would be.
	for _, id := range []int64{11, 22} {
		if err := applier.ApplyGrant(context.Background(), roster[id].Name, roster[id].Namespace, prof); err != nil {
			t.Fatalf("seeding live RBAC for agent %d: %v", id, err)
		}
	}

	srv := New(nil, AuthConfig{}, log.New(&strings.Builder{}, "", 0))
	srv.UseExtensions(Extensions{
		Agents:         &racingAgents{byID: roster},
		Privilege:      store,
		PrivilegeApply: applier,
	})

	var wg sync.WaitGroup
	wg.Add(2)

	// Tab 1: delete the profile.
	go func() {
		defer wg.Done()
		srv.handleProfileDelete(httptest.NewRecorder(), deleteProfileRequest("7"))
	}()

	// Tab 2: grant the same profile to a THIRD agent, released the moment the
	// delete has taken its holder snapshot.
	go func() {
		defer wg.Done()
		<-store.listed
		srv.handleAgentGrant(httptest.NewRecorder(), grantRequest("33", "7"))
	}()

	wg.Wait()

	// THE INVARIANT, pinned literally: nothing live may be unnamed by a row.
	rows := store.liveGrants()
	named := map[string]bool{}
	for k := range rows {
		named[roster[k.agentID].Name+"/"+prof.Name] = true
	}
	for _, obj := range applier.liveSet() {
		if !named[obj] {
			t.Errorf("ORPHANED RBAC: %q is live in the cluster and NO grant row names it.\n"+
				"live objects: %v\ngrant rows:   %d\n"+
				"A grant recorded between handleProfileDelete's holder snapshot and its "+
				"DeleteProfile was never revoked, and the delete's ON DELETE CASCADE then "+
				"removed the only record that could ever resolve those objects again. This is "+
				"the one direction the RECORD ⊇ LIVE invariant forbids, and it is not "+
				"recoverable: RemoveGrant derives the object names from the (agent, profile) "+
				"pair the row carried.", obj, applier.liveSet(), len(rows))
		}
	}
}

// TestAGrantAndAProfileDeleteOfTheSameProfileDoNotOverlap pins the mechanism rather
// than the outcome, because the outcome test above passes for two different reasons
// and a reader should be able to tell which one is in force.
//
// It records, for the same interleaving, whether the grant's RBAC apply landed while
// the delete was still inside its own critical section. On origin/main it does.
func TestAGrantAndAProfileDeleteOfTheSameProfileDoNotOverlap(t *testing.T) {
	prof := privilege.Profile{
		ID:   7,
		Name: "ops-readonly",
		Spec: privilege.Spec{ClusterRules: []privilege.PolicyRule{{
			APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"},
		}}},
	}
	store := newRacingStore(prof, []int64{11, 22})
	roster := racingRoster(agents.StatusStopped)

	var mu sync.Mutex
	overlapped := false

	// 🔴 THE WINDOW IS READ OFF THE STORE, NOT OFF A FLAG THE TEST SETS AROUND THE
	// HANDLER CALL, AND THE FIRST VERSION OF THIS TEST GOT THAT WRONG. It bracketed
	// handleProfileDelete with `deleteInFlight = true/false`, which FALSELY REPORTED
	// AN OVERLAP once in ~25 runs against the FIXED code: the handler releases the
	// profile lock in a defer, so the parked grant can acquire it and apply before the
	// delete's goroutine gets back to clearing the flag. The instrument had a
	// false-positive window of its own, which is the failure mode that makes a red
	// leg unreadable.
	//
	// The hazard's real boundaries are both observable in the store: the window opens
	// when the holder snapshot has been taken (`listed` closed) and closes when the
	// profile row is gone (DeleteProfile has cascaded the grants). An apply inside it
	// is a grant whose row the cascade will take; an apply after it is safely ordered.
	inWindow := func() bool {
		select {
		case <-store.listed:
		default:
			return false // snapshot not taken yet — not the window
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		_, stillThere := store.profiles[prof.ID]
		return stillThere // still deletable ⇒ a grant now gets cascaded away
	}

	applier := &overlapApplier{
		onApply: func() {
			if inWindow() {
				mu.Lock()
				overlapped = true
				mu.Unlock()
			}
		},
	}

	srv := New(nil, AuthConfig{}, log.New(&strings.Builder{}, "", 0))
	srv.UseExtensions(Extensions{
		Agents:         &racingAgents{byID: roster},
		Privilege:      store,
		PrivilegeApply: applier,
	})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		srv.handleProfileDelete(httptest.NewRecorder(), deleteProfileRequest("7"))
	}()
	go func() {
		defer wg.Done()
		<-store.listed
		srv.handleAgentGrant(httptest.NewRecorder(), grantRequest("33", "7"))
	}()
	wg.Wait()

	mu.Lock()
	got := overlapped
	mu.Unlock()
	if got {
		t.Error("a grant APPLIED RBAC for profile 7 while a delete of profile 7 was still " +
			"in flight. The two write paths are not serialised, so the delete's holder " +
			"snapshot can be stale by the time it deletes — see lockProfile.")
	}
}

// overlapApplier calls onApply before recording, so a test can observe WHEN the
// apply happened relative to another request.
type overlapApplier struct {
	onApply func()
}

func (a *overlapApplier) ApplyGrant(_ context.Context, _, _ string, _ privilege.Profile) error {
	a.onApply()
	return nil
}

func (a *overlapApplier) RemoveGrant(_ context.Context, _, _, _ string) error { return nil }

// --- issue #13: the env/kubeconfig half ---

// reapplyRecorder is a Provisioner that also satisfies ProfileReapplier, recording
// which agents were reapplied and in what order relative to the store's delete.
type reapplyRecorder struct {
	Provisioner

	mu    sync.Mutex
	calls []int64
	done  chan struct{}
	want  int

	// seenDelete reports whether DeleteProfile had already run when each reapply
	// landed. The ORDER is load-bearing: ReapplyProfiles recomputes from the grants
	// that exist when it runs, so a reapply before the delete recomputes WITH the
	// profile still granted and changes nothing.
	deletedFirst func() bool
	beforeDelete []int64
}

func (p *reapplyRecorder) ReapplyProfiles(_ context.Context, agentID int64) error {
	p.mu.Lock()
	p.calls = append(p.calls, agentID)
	if p.deletedFirst != nil && !p.deletedFirst() {
		p.beforeDelete = append(p.beforeDelete, agentID)
	}
	n := len(p.calls)
	p.mu.Unlock()
	if n == p.want {
		close(p.done)
	}
	return nil
}

func (p *reapplyRecorder) wait(t *testing.T) ([]int64, []int64) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	calls := append([]int64(nil), p.calls...)
	sort.Slice(calls, func(i, j int) bool { return calls[i] < calls[j] })
	return calls, append([]int64(nil), p.beforeDelete...)
}

// TestDeletingAProfileRecomputesEveryHolderSEnvAndKubeconfig is the regression test
// for issue #13.
//
// 🔴 RED AGAINST origin/main, WHICH CALLED reapplyEnvAsync FOR NO HOLDER AT ALL. The
// unfixed handler removed the RBAC and dropped the profile, and every running holder
// kept the profile's env vars AND its MOUNTED KUBECONFIG SECRET until something else
// reprovisioned it. The credential is the half worth the test: removing a role is
// visible in the cluster, but a kubeconfig already written into a pod's filesystem is
// not undone by removing a role.
//
// 🔴 IT ALSO PINS THE ORDER, because a reapply in the wrong place is INERT rather than
// wrong-looking. ReapplyProfiles recomputes from the grants that exist when it runs,
// so a reapply issued before DeleteProfile recomputes with this profile's grant still
// present and writes the same env back. A test that only counted calls would pass
// against that.
func TestDeletingAProfileRecomputesEveryHolderSEnvAndKubeconfig(t *testing.T) {
	prof := privilege.Profile{
		ID:   7,
		Name: "ops-readonly",
		Spec: privilege.Spec{
			ClusterRules: []privilege.PolicyRule{{
				APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"},
			}},
			Env:              []privilege.EnvVar{{Name: "OPS_TOKEN", Value: "s3cret"}},
			KubeconfigSecret: "ops-readonly-kubeconfig",
		},
	}
	store := newRacingStore(prof, []int64{11, 22})
	store.listed, store.recorded = nil, nil // no interleaving in this test
	store.deleteDelay = 100 * time.Millisecond
	roster := racingRoster(agents.StatusRunning)

	prov := &reapplyRecorder{done: make(chan struct{}), want: 2}
	prov.deletedFirst = func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		_, still := store.profiles[7]
		return !still
	}

	srv := New(nil, AuthConfig{}, log.New(&strings.Builder{}, "", 0))
	srv.UseExtensions(Extensions{
		Agents:         &racingAgents{byID: roster},
		Privilege:      store,
		PrivilegeApply: newRacingApplier(),
		Provisioner:    prov,
	})

	rec := httptest.NewRecorder()
	srv.handleProfileDelete(rec, deleteProfileRequest("7"))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete returned %d, want 200: %s", rec.Code, rec.Body.String())
	}

	got, early := prov.wait(t)
	want := []int64{11, 22}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("ReapplyProfiles was called for agents %v, want one call per holder %v.\n"+
			"Deleting a profile revoked its RBAC but never recomputed the holders' env or "+
			"their MOUNTED KUBECONFIG, so each running holder kept the profile's env vars "+
			"and its kubeconfig CREDENTIAL until its next provision — while the operator "+
			"had been told the profile was gone.", got, want)
	}
	if len(early) != 0 {
		t.Errorf("ReapplyProfiles ran for agents %v BEFORE the profile was deleted. "+
			"ReapplyProfiles recomputes from the grants that exist when it runs, so a "+
			"reapply before the delete still sees this profile granted and is INERT.", early)
	}
}

// TestAProfileDeleteWithNoApplierStillRecomputesEnvAndKubeconfig is the seam the
// RBAC-shaped reading of this code gets wrong.
//
// A nil PrivilegeApply means no RBAC of this server's making exists to remove. It does
// NOT mean the profile contributed no env and no mounted kubeconfig — so an early
// `return nil` on that branch would silently reinstate the whole of issue #13 for
// every deployment without an in-cluster client.
func TestAProfileDeleteWithNoApplierStillRecomputesEnvAndKubeconfig(t *testing.T) {
	prof := privilege.Profile{
		ID:   7,
		Name: "ops-readonly",
		Spec: privilege.Spec{
			Env:              []privilege.EnvVar{{Name: "OPS_TOKEN", Value: "s3cret"}},
			KubeconfigSecret: "ops-readonly-kubeconfig",
		},
	}
	store := newRacingStore(prof, []int64{11, 22})
	store.listed, store.recorded = nil, nil
	roster := racingRoster(agents.StatusRunning)

	prov := &reapplyRecorder{done: make(chan struct{}), want: 2}

	srv := New(nil, AuthConfig{}, log.New(&strings.Builder{}, "", 0))
	srv.UseExtensions(Extensions{
		Agents:      &racingAgents{byID: roster},
		Privilege:   store,
		Provisioner: prov,
		// PrivilegeApply deliberately nil.
	})

	rec := httptest.NewRecorder()
	srv.handleProfileDelete(rec, deleteProfileRequest("7"))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete returned %d, want 200: %s", rec.Code, rec.Body.String())
	}

	got, _ := prov.wait(t)
	if len(got) != 2 {
		t.Errorf("with no applier wired, ReapplyProfiles was called for %v, want both "+
			"holders [11 22]. A nil applier means there is no RBAC to remove; the env and "+
			"the mounted kubeconfig still have to be recomputed.", got)
	}
}

// --- issue #14: the two failure branches must be distinguishable ---

// TestADeleteThatFailsAfterTheRevokeSaysTheClusterAlreadyChanged is the regression
// test for issue #14.
//
// 🔴 THE WHOLE NORMALISED STRING IS PINNED, NOT KEYWORDS. The artifact under test is
// PROSE, and a guard on words ("mentions RBAC", "mentions retry") is walkable by
// rewording — including by rewording back into something as uninformative as the
// original `could not delete profile`. Pinning the string costs a test edit whenever
// the copy is deliberately changed, which is the price of the claim being machine
// readable at all.
func TestADeleteThatFailsAfterTheRevokeSaysTheClusterAlreadyChanged(t *testing.T) {
	prof := privilege.Profile{
		ID:   7,
		Name: "ops-readonly",
		Spec: privilege.Spec{ClusterRules: []privilege.PolicyRule{{
			APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"},
		}}},
	}
	store := newRacingStore(prof, []int64{11, 22})
	store.listed, store.recorded = nil, nil
	store.deleteErr = errors.New("connection reset by peer")

	srv := New(nil, AuthConfig{}, log.New(&strings.Builder{}, "", 0))
	srv.UseExtensions(Extensions{
		Agents:         &racingAgents{byID: racingRoster(agents.StatusStopped)},
		Privilege:      store,
		PrivilegeApply: newRacingApplier(),
	})

	rec := httptest.NewRecorder()
	srv.handleProfileDelete(rec, deleteProfileRequest("7"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	const want = "could not delete profile, and THE CLUSTER HAS ALREADY CHANGED: this profile's " +
		"RBAC was removed from every agent holding it BEFORE the delete failed, so the access " +
		"is already withdrawn while the profile and its grant records were KEPT. The grant " +
		"chips still shown for it now claim access the ServiceAccount no longer has. Nothing " +
		"is unrevocable — every record survives and removal is idempotent — so retrying this " +
		"delete is safe and is the way to converge. Cause: connection reset by peer"
	got := strings.TrimSpace(rec.Body.String())
	if got != want {
		t.Errorf("the post-revoke delete failure does not describe the state it leaves.\n"+
			" got: %q\nwant: %q\n"+
			"Every holder's RBAC is already gone from the cluster while every grant row "+
			"survives, so each grant chip claims access the ServiceAccount no longer has. "+
			"That is a different state from the revoke-failure branch, where nothing has "+
			"changed, and the two call for different next moves.", got, want)
	}
}

// TestTheTwoProfileDeleteFailuresAreNotTheSameMessage is an INVARIANT GUARD, not
// regression coverage, and the distinction is worth stating because it is red against
// origin/main for a weaker reason than it looks.
//
// ⚠ ITS PRIMARY ASSERTION — that the two messages DIFFER — ALREADY PASSED ON THE
// UNFIXED CODE: there the revoke branch had its long message and the delete branch
// had the bare "could not delete profile", which are certainly different strings.
// What went red on origin/main was the secondary assertion, and that one is about a
// phrase THIS change introduced, so it is close to self-referential. The genuine
// regression coverage for #14 is the full-string pin in the test above; this test's
// value is forward-looking: it fails if a later edit collapses the two branches onto
// one string, or lets the riskier branch claim the operation was inert.
//
// Counted honestly, #14 has ONE regression test and one guard.
func TestTheTwoProfileDeleteFailuresAreNotTheSameMessage(t *testing.T) {
	prof := privilege.Profile{
		ID:   7,
		Name: "ops-readonly",
		Spec: privilege.Spec{ClusterRules: []privilege.PolicyRule{{
			APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"},
		}}},
	}

	body := func(deleteErr error, removeErr error) string {
		store := newRacingStore(prof, []int64{11, 22})
		store.listed, store.recorded = nil, nil
		store.deleteErr = deleteErr
		srv := New(nil, AuthConfig{}, log.New(&strings.Builder{}, "", 0))
		srv.UseExtensions(Extensions{
			Agents:         &racingAgents{byID: racingRoster(agents.StatusStopped)},
			Privilege:      store,
			PrivilegeApply: &failingApplier{removeErr: removeErr},
		})
		rec := httptest.NewRecorder()
		srv.handleProfileDelete(rec, deleteProfileRequest("7"))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status %d, want 500", rec.Code)
		}
		return strings.TrimSpace(rec.Body.String())
	}

	revokeFailed := body(nil, errors.New("forbidden"))
	deleteFailed := body(errors.New("connection reset by peer"), nil)

	if revokeFailed == deleteFailed {
		t.Fatalf("both profile-delete failures return the same message, so the operator "+
			"cannot tell 'nothing has changed' from 'the cluster has already changed':\n%q",
			revokeFailed)
	}
	// The safer branch must say nothing changed; the riskier one must not claim that.
	if !strings.Contains(revokeFailed, "NOTHING HAS CHANGED") {
		t.Errorf("the revoke-failure branch no longer says the operation was inert: %q", revokeFailed)
	}
	if strings.Contains(deleteFailed, "NOTHING HAS CHANGED") {
		t.Errorf("the post-revoke delete failure claims nothing changed, but every holder's "+
			"RBAC was already removed: %q", deleteFailed)
	}
}

// failingApplier applies fine and fails to remove, which is the revoke-failure branch.
type failingApplier struct {
	removeErr error
}

func (a *failingApplier) ApplyGrant(context.Context, string, string, privilege.Profile) error {
	return nil
}

func (a *failingApplier) RemoveGrant(context.Context, string, string, string) error {
	return a.removeErr
}
