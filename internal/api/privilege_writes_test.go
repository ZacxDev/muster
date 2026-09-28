package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/privilege"
)

// ---------------------------------------------------------------------------
// THE THREE PRIVILEGE WRITE PATHS, AGAINST THE ONE INVARIANT THEY SHARE.
//
// 🔴 WHY THIS FILE DID NOT EXIST AND HAD TO. internal/api had NO handler test for
// the privilege routes at all — only a seam check that the real applier satisfies
// the interface and a readiness table over the nil-ness predicate. Both are about
// WIRING. So every one of the three write paths was unguarded at the level where
// their disagreement lived: grantProfile applied before recording and failed
// CLOSED, the revoke path removed before dropping and failed OPEN, and the profile
// delete did not remove at all while its cascade destroyed the only record that
// could ever find the objects. Three directions, one table, nothing red.
//
// 🔴 EVERY TEST HERE DRIVES A HANDLER, NOT A HELPER. The defects were in the ORDER
// of two calls and in which one's failure won, and a helper-level test can be
// written to pass against either order. The fakes therefore record a SEQUENCE, and
// two of the assertions are about that sequence rather than about a count.
//
// ⚠ THE APPLIER IS A FAKE, SO NOTHING HERE IS A CLAIM ABOUT A CLUSTER. What it
// pins is which calls this package makes, in which order, and what it does when one
// fails — which is exactly where the findings were. internal/agentprivilege and
// internal/provision/k8s own the behaviour against a (fake) apiserver.
// ---------------------------------------------------------------------------

// fakePrivilegeStore implements only the methods these paths touch. The embedded
// interface covers the rest and panics if anything else is called, which is the
// property registration_test.go's stubs are built on.
type fakePrivilegeStore struct {
	privilege.Store

	profile  privilege.Profile
	profErr  error
	holders  []privilege.Grant
	listErr  error
	grantErr error

	// calls is the shared sequence log; every fake in a test writes to the same one.
	calls *[]string
}

func (f *fakePrivilegeStore) note(s string) { *f.calls = append(*f.calls, s) }

func (f *fakePrivilegeStore) GetProfile(context.Context, int64) (privilege.Profile, error) {
	return f.profile, f.profErr
}

func (f *fakePrivilegeStore) ListProfiles(context.Context) ([]privilege.Profile, error) {
	return []privilege.Profile{f.profile}, nil
}

func (f *fakePrivilegeStore) ListGrantsForProfile(context.Context, int64) ([]privilege.Grant, error) {
	f.note("store.ListGrantsForProfile")
	return f.holders, f.listErr
}

func (f *fakePrivilegeStore) ListGrantsForAgent(context.Context, int64) ([]privilege.Grant, error) {
	return nil, nil
}

func (f *fakePrivilegeStore) DeleteProfile(context.Context, int64) error {
	f.note("store.DeleteProfile")
	return nil
}

func (f *fakePrivilegeStore) Grant(context.Context, int64, int64, string) (privilege.Grant, error) {
	f.note("store.Grant")
	return privilege.Grant{}, f.grantErr
}

func (f *fakePrivilegeStore) Revoke(context.Context, int64, int64) error {
	f.note("store.Revoke")
	return nil
}

func (f *fakePrivilegeStore) CreateProfile(_ context.Context, p privilege.Profile) (privilege.Profile, error) {
	f.note("store.CreateProfile:" + p.Name)
	return p, nil
}

// fakeAgentsStore answers Get from a map keyed by id.
type fakeAgentsStore struct {
	agents.Store
	byID map[int64]agents.Agent
}

func (f *fakeAgentsStore) Get(_ context.Context, id int64) (agents.Agent, error) {
	a, ok := f.byID[id]
	if !ok {
		return agents.Agent{}, errors.New("no such agent")
	}
	return a, nil
}

// fakeApplier records what it was asked to do and can be made to fail.
type fakeApplier struct {
	applyErr  error
	removeErr error
	calls     *[]string
}

func (f *fakeApplier) ApplyGrant(_ context.Context, agentName, _ string, profile privilege.Profile) error {
	*f.calls = append(*f.calls, "applier.ApplyGrant:"+agentName+"/"+profile.Name)
	return f.applyErr
}

func (f *fakeApplier) RemoveGrant(_ context.Context, agentName, _, profileName string) error {
	*f.calls = append(*f.calls, "applier.RemoveGrant:"+agentName+"/"+profileName)
	return f.removeErr
}

// privilegeFixture builds a server over the three fakes, sharing one call log.
type privilegeFixture struct {
	srv     *Server
	store   *fakePrivilegeStore
	applier *fakeApplier
	calls   *[]string
}

func newPrivilegeFixture(t *testing.T, prof privilege.Profile, holders []privilege.Grant) *privilegeFixture {
	t.Helper()
	calls := new([]string)
	store := &fakePrivilegeStore{profile: prof, holders: holders, calls: calls}
	applier := &fakeApplier{calls: calls}
	byID := map[int64]agents.Agent{}
	for _, g := range holders {
		byID[g.AgentID] = agents.Agent{
			ID:        g.AgentID,
			Name:      g.ProfileName + "-holder", // distinct per fixture, DNS-label shaped
			Namespace: "devpod-holder",
			Status:    agents.StatusStopped,
		}
	}
	// Distinct names, so an assertion cannot be satisfied by the wrong agent.
	i := 0
	for id, a := range byID {
		a.Name = []string{"alpha", "bravo", "charlie", "delta"}[i%4]
		a.Namespace = "devpod-" + a.Name
		byID[id] = a
		i++
	}
	s := New(nil, AuthConfig{}, log.New(&strings.Builder{}, "", 0))
	s.UseExtensions(Extensions{
		Agents:         &fakeAgentsStore{byID: byID},
		Privilege:      store,
		PrivilegeApply: applier,
	})
	return &privilegeFixture{srv: s, store: store, applier: applier, calls: calls}
}

func (f *privilegeFixture) log() []string { return *f.calls }

func (f *privilegeFixture) called(prefix string) bool {
	for _, c := range f.log() {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (f *privilegeFixture) indexOf(prefix string) int {
	for i, c := range f.log() {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

func deleteProfileRequest(id string) *http.Request {
	r := httptest.NewRequest(http.MethodDelete, "/privileges/"+id, nil)
	r.SetPathValue("id", id)
	return r
}

// twoHolders is the fixture shape both delete tests use: one profile, two agents.
func twoHolders(profileName string) []privilege.Grant {
	return []privilege.Grant{
		{AgentID: 11, ProfileID: 7, ProfileName: profileName},
		{AgentID: 22, ProfileID: 7, ProfileName: profileName},
	}
}

func rbacProfile() privilege.Profile {
	return privilege.Profile{
		ID:   7,
		Name: "ops-readonly",
		Spec: privilege.Spec{
			ClusterRules: []privilege.PolicyRule{{
				APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"},
			}},
		},
	}
}

// TestDeletingAProfileRevokesItsLiveRBACFromEveryHolderFirst is the regression test
// for the delete that left escalated RBAC bound with nothing recording it.
//
// 🔴 RED AGAINST THE PRE-FIX HANDLER, WHICH CALLED DeleteProfile AND NOTHING ELSE.
// The pre-fix path made ZERO applier calls, so both the per-holder assertions and
// the ordering assertion fail — and the ordering one is the part that matters: the
// store's delete cascades the grant rows, so a removal attempted afterwards has no
// (agent, profile) pair left to resolve the object names from.
func TestDeletingAProfileRevokesItsLiveRBACFromEveryHolderFirst(t *testing.T) {
	prof := rbacProfile()
	f := newPrivilegeFixture(t, prof, twoHolders(prof.Name))

	rec := httptest.NewRecorder()
	f.srv.handleProfileDelete(rec, deleteProfileRequest("7"))

	if rec.Code != http.StatusOK {
		t.Fatalf("handleProfileDelete answered %d, want 200. body: %s", rec.Code, rec.Body.String())
	}

	// Positive control on the instrument: the fakes must have been reached at all,
	// or every assertion below is about an empty log.
	if len(f.log()) == 0 {
		t.Fatalf("positive control FAILED: the handler completed and called none of the fakes, " +
			"so this test observes nothing")
	}
	t.Logf("call sequence: %v", f.log())

	// Every holder's RBAC, by NAME — a count would pass for two calls naming one
	// agent twice, which is the shape a loop-variable bug produces.
	for _, want := range []string{
		"applier.RemoveGrant:alpha/" + prof.Name,
		"applier.RemoveGrant:bravo/" + prof.Name,
	} {
		if !f.called(want) {
			t.Errorf("deleting the profile did not remove its RBAC (%s).\n"+
				"    The store's delete CASCADES the grant rows, so after it there is no record "+
				"that any agent held the profile — while its ClusterRole and ClusterRoleBinding "+
				"are still bound to that agent's ServiceAccount, and RemoveGrant resolves them "+
				"from the (agent, profile) name pair that just disappeared. With the rbac "+
				"`escalate` verb granted, the orphan can hold arbitrary rules.\n  calls: %v",
				want, f.log())
		}
	}

	if !f.called("store.DeleteProfile") {
		t.Fatalf("the profile was never deleted, so this test is not exercising the delete path")
	}
	// ORDER, not just presence.
	if rm, del := f.indexOf("applier.RemoveGrant"), f.indexOf("store.DeleteProfile"); rm > del {
		t.Errorf("the profile was deleted BEFORE its RBAC was removed (RemoveGrant at %d, "+
			"DeleteProfile at %d).\n    The delete cascades the grant rows away, so a removal "+
			"after it has no name to resolve the objects from.\n  calls: %v", rm, del, f.log())
	}
}

// TestAFailedRBACRemovalAbortsTheProfileDelete is the fail-closed half.
//
// 🔴 RED AGAINST THE PRE-FIX HANDLER, WHICH ANSWERED 200 AND DELETED. It is also
// the case that distinguishes this fix from a best-effort one: removing first and
// deleting anyway would satisfy the test above and still destroy the record while
// the access stayed live.
func TestAFailedRBACRemovalAbortsTheProfileDelete(t *testing.T) {
	prof := rbacProfile()
	f := newPrivilegeFixture(t, prof, twoHolders(prof.Name))
	f.applier.removeErr = errors.New("clusterroles.rbac is forbidden: no delete verb")

	rec := httptest.NewRecorder()
	f.srv.handleProfileDelete(rec, deleteProfileRequest("7"))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("handleProfileDelete answered %d with the RBAC removal failing, want 500.\n"+
			"    Deleting the profile anyway drops the only record of live escalated access and "+
			"makes it unrevocable.\n  body: %s", rec.Code, rec.Body.String())
	}
	if f.called("store.DeleteProfile") {
		t.Errorf("the profile was DELETED even though its RBAC removal failed.\n  calls: %v", f.log())
	}
	// The refusal has to be actionable: it is the only place an operator learns the
	// profile is still there on purpose.
	if body := rec.Body.String(); !strings.Contains(body, "KEPT") {
		t.Errorf("the refusal does not say the profile was kept, so an operator reading a 500 "+
			"cannot tell whether the delete half-happened.\n  body: %s", body)
	}
}

// TestAFailedRBACRemovalKeepsTheGrantRecord is F4's revoke half.
//
// 🔴 RED AGAINST THE PRE-FIX HANDLER, WHICH LOGGED THE ERROR AND CARRIED ON: it
// answered 200, dropped the row, and the UI rendered "revoked" over RBAC that was
// still bound. A transient apiserver error or a missing `delete` verb was enough.
// That made the pair of paths fail in OPPOSITE directions — grant applies before
// recording and fails closed, revoke removed before dropping and failed open.
func TestAFailedRBACRemovalKeepsTheGrantRecord(t *testing.T) {
	prof := rbacProfile()
	f := newPrivilegeFixture(t, prof, twoHolders(prof.Name))
	f.applier.removeErr = errors.New("etcdserver: request timed out")

	r := httptest.NewRequest(http.MethodDelete, "/agents/11/grants/7", nil)
	r.SetPathValue("id", "11")
	r.SetPathValue("profileId", "7")
	rec := httptest.NewRecorder()
	f.srv.handleAgentRevoke(rec, r)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("handleAgentRevoke answered %d with the RBAC removal failing, want 500.\n"+
			"    Reporting a revoke that did not happen leaves escalated RBAC live with the "+
			"record — the only thing that can find it — deleted.\n  body: %s",
			rec.Code, rec.Body.String())
	}
	if f.called("store.Revoke") {
		t.Errorf("the grant record was dropped even though the RBAC removal failed.\n"+
			"    The record is what RemoveGrant's object names are derived from, so dropping it "+
			"here is what makes the live access unreachable.\n  calls: %v", f.log())
	}
	// Control: the removal was actually attempted, so the 500 is attributable to it
	// rather than to the handler refusing earlier for some other reason.
	if !f.called("applier.RemoveGrant:alpha/" + prof.Name) {
		t.Errorf("control FAILED: RemoveGrant was never called, so the 500 above is not "+
			"attributable to its failure.\n  calls: %v", f.log())
	}
}

// TestARevokeThatRemovesTheRBACStillDropsTheRecord is the positive control for the
// test above: the fail-closed direction must not have made the happy path refuse.
//
// ⚠ IT IS AN INVARIANT GUARD, LABELLED AS ONE. No defect ever violated it; it is
// here because the fix it controls is a new early return, and an early return is one
// mis-placed brace away from refusing every revoke.
func TestARevokeThatRemovesTheRBACStillDropsTheRecord(t *testing.T) {
	prof := rbacProfile()
	f := newPrivilegeFixture(t, prof, twoHolders(prof.Name))

	r := httptest.NewRequest(http.MethodDelete, "/agents/11/grants/7", nil)
	r.SetPathValue("id", "11")
	r.SetPathValue("profileId", "7")
	rec := httptest.NewRecorder()
	f.srv.handleAgentRevoke(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("handleAgentRevoke answered %d on the happy path, want 200. body: %s",
			rec.Code, rec.Body.String())
	}
	if !f.called("store.Revoke") {
		t.Errorf("the happy path did not drop the grant record.\n  calls: %v", f.log())
	}
	if rm, rv := f.indexOf("applier.RemoveGrant"), f.indexOf("store.Revoke"); rm > rv {
		t.Errorf("the record was dropped before the RBAC was removed (RemoveGrant at %d, "+
			"Revoke at %d), which is the order the invariant forbids.\n  calls: %v", rm, rv, f.log())
	}
}

// TestAFailedGrantRecordRollsBackTheAppliedRBAC is F4's grant half.
//
// 🔴 RED AGAINST THE PRE-FIX grantProfile, WHICH RETURNED THE RECORD ERROR AND LEFT
// THE RBAC APPLIED. The caller then rendered a 500, the operator saw a failed grant,
// and the cluster held permissions no row named — the same unreachable state a
// deleted profile produced, arrived at from the other end.
func TestAFailedGrantRecordRollsBackTheAppliedRBAC(t *testing.T) {
	prof := rbacProfile()
	f := newPrivilegeFixture(t, prof, twoHolders(prof.Name))
	f.store.grantErr = errors.New("connection reset by peer")

	agent := agents.Agent{ID: 11, Name: "alpha", Namespace: "devpod-alpha", Status: agents.StatusStopped}
	err := f.srv.grantProfile(context.Background(), agent, prof, "user")
	if err == nil {
		t.Fatal("grantProfile returned nil while recording the grant failed")
	}
	t.Logf("call sequence: %v", f.log())

	// Control: the apply happened, so the rollback below is a rollback of something.
	if !f.called("applier.ApplyGrant:alpha/" + prof.Name) {
		t.Fatalf("control FAILED: the RBAC was never applied, so there is nothing for this test "+
			"to observe a rollback of.\n  calls: %v", f.log())
	}
	if !f.called("applier.RemoveGrant:alpha/" + prof.Name) {
		t.Errorf("the applied RBAC was NOT rolled back after the record write failed.\n"+
			"    The cluster then holds permissions for (alpha, %s) that no grant row names, so "+
			"nothing in muster can revoke them — while the caller saw a failed grant.\n  calls: %v",
			prof.Name, f.log())
	}
	if !strings.Contains(err.Error(), "rolled back") {
		t.Errorf("the error does not say the RBAC was rolled back, so a caller cannot tell this "+
			"failure (nothing granted) from the one below (an orphan needing hand removal): %v", err)
	}
}

// TestAFailedRollbackNamesTheOrphanItCouldNotRemove is the second failure of the
// same pair, and it is the one a human has to act on.
func TestAFailedRollbackNamesTheOrphanItCouldNotRemove(t *testing.T) {
	prof := rbacProfile()
	f := newPrivilegeFixture(t, prof, twoHolders(prof.Name))
	f.store.grantErr = errors.New("connection reset by peer")
	f.applier.removeErr = errors.New("etcdserver: request timed out")

	agent := agents.Agent{ID: 11, Name: "alpha", Namespace: "devpod-alpha", Status: agents.StatusStopped}
	err := f.srv.grantProfile(context.Background(), agent, prof, "user")
	if err == nil {
		t.Fatal("grantProfile returned nil while both the record write and the rollback failed")
	}
	// The label is what a human greps for; the profile name alone is not enough to
	// find the objects.
	for _, want := range []string{"muster.dev/policy=" + prof.Name, "alpha"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %q. Both halves failed, so live RBAC exists that no "+
				"record names and nothing automatic will ever remove — the error is the only "+
				"place the operator learns which objects to delete: %v", want, err)
		}
	}
}

// TestAGrantWithNoApplierWiredRecordsAndDoesNotPretendToApply is the control on the
// nil-applier arm, which the rollback branch must not have changed.
//
// ⚠ INVARIANT GUARD, LABELLED. No finding touched it; it exists because the new
// `applied` bookkeeping is exactly the kind of flag that ends up true for a path
// that applied nothing, which would make the rollback branch nil-deref.
func TestAGrantWithNoApplierWiredRecordsAndDoesNotPretendToApply(t *testing.T) {
	prof := rbacProfile()
	calls := new([]string)
	store := &fakePrivilegeStore{profile: prof, calls: calls, grantErr: errors.New("boom")}
	s := New(nil, AuthConfig{}, log.New(&strings.Builder{}, "", 0))
	s.UseExtensions(Extensions{Agents: &fakeAgentsStore{byID: map[int64]agents.Agent{}}, Privilege: store})

	agent := agents.Agent{ID: 11, Name: "alpha", Namespace: "devpod-alpha", Status: agents.StatusStopped}
	err := s.grantProfile(context.Background(), agent, prof, "user")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("with no applier wired, a failed record write must surface unchanged; got %v", err)
	}
	if strings.Contains(err.Error(), "rolled back") {
		t.Errorf("the error claims a rollback with no applier wired — nothing was applied: %v", err)
	}
}

// TestAProfileNameThatCannotBecomeAClusterObjectIsRefusedAtCreate is F5.
//
// 🔴 RED AGAINST THE PRE-FIX HANDLER, WHICH CHECKED ONLY FOR THE EMPTY STRING. The
// name is spliced into `muster-<agent>-<profile>-<digest>` AND written verbatim into
// the muster.dev/policy label, so `Cluster Triage` was accepted here and failed at
// the FIRST GRANT as `500 could not apply grant: …` — three packages from the form
// that accepted it. k8s.io/client-go/kubernetes/fake validates neither names nor
// labels, which is why the driver's own mutation-clean suite could not see this.
func TestAProfileNameThatCannotBecomeAClusterObjectIsRefusedAtCreate(t *testing.T) {
	create := func(t *testing.T, name string) (int, []string) {
		t.Helper()
		calls := new([]string)
		store := &fakePrivilegeStore{profile: rbacProfile(), calls: calls}
		s := New(nil, AuthConfig{}, log.New(&strings.Builder{}, "", 0))
		s.UseExtensions(Extensions{Privilege: store})
		form := url.Values{"name": {name}}
		r := httptest.NewRequest(http.MethodPost, "/privileges", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		s.handleProfileCreate(rec, r)
		return rec.Code, *calls
	}

	for _, tc := range []struct {
		name   string
		value  string
		reason string
	}{
		{"a space", "Cluster Triage", "a space cannot appear in a label value or an object name"},
		{"upper case with a space", "Ops Read Only", "same, and it is the shape an operator types first"},
		{"a slash", "ops/readonly", "a slash would split the object name's path segment"},
		{"a leading dash", "-ops", "a label value must begin alphanumeric"},
		{"a trailing dot", "ops.", "a label value must end alphanumeric"},
		{"64 characters", strings.Repeat("a", 64), "one past the 63-character label-value cap"},
		{"empty", "", "there is no object name to derive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, calls := create(t, tc.value)
			if code != http.StatusBadRequest {
				t.Errorf("creating a profile named %q answered %d, want 400 (%s).\n"+
					"    Accepted here, it fails at the first grant with a 500 whose cause is "+
					"three packages away.", tc.value, code, tc.reason)
			}
			for _, c := range calls {
				if strings.HasPrefix(c, "store.CreateProfile") {
					t.Errorf("the profile was CREATED despite the name being unusable: %v", calls)
				}
			}
		})
	}

	// 🔴 THE CONTROLS. Without them a handler that rejected EVERY name would pass
	// every case above, which is the same green for the opposite defect. 63 is the
	// boundary the cap is ON, so it is a control and not a nicety.
	for _, tc := range []struct{ name, value string }{
		{"a plain slug", "ops-readonly"},
		{"dots and underscores", "ops.read_only1"},
		{"mixed case, no space", "OpsReadOnly"},
		{"63 characters", strings.Repeat("a", 63)},
	} {
		t.Run("control/"+tc.name, func(t *testing.T) {
			code, calls := create(t, tc.value)
			if code != http.StatusOK {
				t.Errorf("control FAILED: a usable name %q was refused with %d, so the refusals "+
					"above are not attributable to the names being unusable", tc.value, code)
			}
			found := false
			for _, c := range calls {
				if c == "store.CreateProfile:"+tc.value {
					found = true
				}
			}
			if !found {
				t.Errorf("control FAILED: %q was accepted but not created: %v", tc.value, calls)
			}
		})
	}
}
