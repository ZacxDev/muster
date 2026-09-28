package privilege_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/privilege"
)

// seededAgent is the minimum an agent row needs to be for a privilege row to FK
// to it: an id and a name.
type seededAgent struct {
	ID   int64
	Name string
}

// seedAgent INSERTs an agent row directly, rather than going through the agent
// store.
//
// 🔴 THAT IS DELIBERATE AND IT IS A SEAM, NOT A SHORTCUT. What these tests need
// from an agent is a row the `agent_privileges`/`privilege_requests` foreign
// keys can point at — nothing about how agents are created, named, provisioned
// or reconciled. Reaching for the agent store to get one would make every test
// in this file fail whenever THAT package broke, and would report the failure as
// a privilege-store defect. An INSERT of the two NOT NULL columns is the whole
// dependency, spelled out.
//
// ⚠ The row is deliberately minimal. If a future privilege query starts reading
// a column of `agents`, this helper is where that becomes visible — a test that
// needs `status` or `namespace` to mean something should say so here rather than
// inherit a default from somewhere else.
func seedAgent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) seededAgent {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO agents (name, namespace) VALUES ($1, $2) RETURNING id`,
		name, "workspace-"+name,
	).Scan(&id); err != nil {
		t.Fatalf("seed agent %q: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM agents WHERE id=$1`, id)
	})
	return seededAgent{ID: id, Name: name}
}

// TestPGStoreRequests verifies a privilege request persists and reads back, and
// that ListPendingRequests returns it. It needs a real Postgres (the store is
// Postgres-backed + the row FKs to agents). Database-gated; see internal/dbtest.
func TestPGStoreRequests(t *testing.T) {
	dsn := dbtest.DSN(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// A privilege request FKs to an agent, so seed one.
	ag := seedAgent(t, ctx, pool, "priv-test-"+time.Now().Format("150405.000000"))

	store := privilege.NewPG(pool)
	r, err := store.CreateRequest(ctx, privilege.Request{AgentID: ag.ID, AgentName: ag.Name, Profile: "k8s-read", Reason: "need pod logs"})
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if r.ID == 0 || r.Status != privilege.StatusPending {
		t.Fatalf("created request = %+v, want id + pending status", r)
	}

	pending, err := store.ListPendingRequests(ctx)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	found := false
	for _, p := range pending {
		if p.ID == r.ID {
			found = true
			if p.Profile != "k8s-read" || p.AgentName != ag.Name || p.Reason != "need pod logs" {
				t.Fatalf("listed request = %+v, want k8s-read/%s", p, ag.Name)
			}
		}
	}
	if !found {
		t.Fatalf("created request %d not in pending list (%d entries)", r.ID, len(pending))
	}

	// Decide the request → it leaves the pending list.
	if err := store.DecideRequest(ctx, r.ID, privilege.StatusApproved, "user"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	got, err := store.GetRequest(ctx, r.ID)
	if err != nil {
		t.Fatalf("get request: %v", err)
	}
	if got.Status != privilege.StatusApproved || got.DecidedBy != "user" || got.DecidedAt == nil {
		t.Fatalf("decided request = %+v, want approved/user/decidedAt", got)
	}

	// Retention: a future cutoff sweeps the resolved request but never a pending one.
	pendingReq, err := store.CreateRequest(ctx, privilege.Request{AgentID: ag.ID, AgentName: ag.Name, Profile: "k8s-write", Reason: "still open"})
	if err != nil {
		t.Fatalf("create pending request: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM privilege_requests WHERE id=$1`, pendingReq.ID)
	})

	n, err := store.DeleteResolvedRequestsOlderThan(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("delete resolved: %v", err)
	}
	if n < 1 {
		t.Fatalf("delete resolved swept %d, want >= 1 (the approved request)", n)
	}
	if _, err := store.GetRequest(ctx, r.ID); err == nil {
		t.Fatalf("resolved request %d should have been swept", r.ID)
	}
	if _, err := store.GetRequest(ctx, pendingReq.ID); err != nil {
		t.Fatalf("pending request %d must survive retention: %v", pendingReq.ID, err)
	}
}

// TestPGStoreProfilesAndGrants verifies the profiles registry and per-agent
// grants round-trip, including the JSONB spec. DB-gated like the test above.
func TestPGStoreProfilesAndGrants(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	name := "grant-test-" + time.Now().Format("150405.000000")
	ag := seedAgent(t, ctx, pool, name)

	store := privilege.NewPG(pool)
	prof, err := store.CreateProfile(ctx, privilege.Profile{
		Name:        "k8s-read-" + name,
		DisplayName: "K8s read",
		Description: "read-only cluster access",
		Spec: privilege.Spec{
			ClusterRules: []privilege.PolicyRule{
				{APIGroups: []string{""}, Resources: []string{"pods", "pods/log"}, Verbs: []string{"get", "list", "watch"}},
			},
			Env:              []privilege.EnvVar{{Name: "KUBECTL_NAMESPACE", Value: "ops"}},
			KubeconfigSecret: "ops-kubeconfig",
		},
	})
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM privilege_profiles WHERE id=$1`, prof.ID) })

	// Spec JSONB round-trips.
	rt, err := store.GetProfileByName(ctx, prof.Name)
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	if len(rt.Spec.ClusterRules) != 1 || rt.Spec.ClusterRules[0].Resources[1] != "pods/log" {
		t.Fatalf("spec did not round-trip: %+v", rt.Spec)
	}

	// Grant → appears for the agent; idempotent.
	if _, err := store.Grant(ctx, ag.ID, prof.ID, "user"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := store.Grant(ctx, ag.ID, prof.ID, "user"); err != nil {
		t.Fatalf("regrant (idempotent): %v", err)
	}
	grants, err := store.ListGrantsForAgent(ctx, ag.ID)
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants) != 1 || grants[0].ProfileName != prof.Name {
		t.Fatalf("grants = %+v, want 1 with profile name", grants)
	}
	// The grant carries the joined profile spec (env + kubeconfig + RBAC) so
	// AgentProfileEnv resolves without re-fetching each profile.
	gspec := grants[0].Spec
	if len(gspec.Env) != 1 || gspec.Env[0].Name != "KUBECTL_NAMESPACE" || gspec.Env[0].Value != "ops" {
		t.Fatalf("grant spec env not joined: %+v", gspec.Env)
	}
	if gspec.KubeconfigSecret != "ops-kubeconfig" {
		t.Fatalf("grant spec kubeconfig not joined: %q", gspec.KubeconfigSecret)
	}
	if len(gspec.ClusterRules) != 1 {
		t.Fatalf("grant spec RBAC not joined: %+v", gspec.ClusterRules)
	}

	// 🔴 THE OTHER DIRECTION, WHICH IS A DELETE'S PRE-CONDITION RATHER THAN A
	// SYMMETRY. internal/api's profile delete has to revoke the live RBAC from every
	// holder BEFORE dropping the profile, because the drop cascades these rows away
	// and the RBAC object names are derived from the (agent, profile) pair they
	// carry. So this list is the last chance to learn who holds the profile, and a
	// row whose ProfileName came back EMPTY would be a live ClusterRole nothing can
	// name — which is why the name is asserted here and not just the count.
	byProfile, err := store.ListGrantsForProfile(ctx, prof.ID)
	if err != nil {
		t.Fatalf("list grants for profile: %v", err)
	}
	if len(byProfile) == 0 {
		t.Fatalf("ListGrantsForProfile = 0 row(s), want 1 (the agent granted above) — the delete " +
			"path would then find no holders and delete the profile with its RBAC still bound")
	}
	// ⚠ Errorf, NOT Fatalf, AND THE DIFFERENCE IS MEASURED. A too-WIDE result (a
	// filter that matches everything) must reach the ungranted-profile control below;
	// with a Fatalf here that control was unreachable, so a mutant returning every
	// row died only on this count and the control scored nothing.
	if len(byProfile) != 1 {
		t.Errorf("ListGrantsForProfile = %d row(s), want 1 (the agent granted above)", len(byProfile))
	}
	if byProfile[0].AgentID != ag.ID {
		t.Errorf("ListGrantsForProfile returned agent %d, want %d — the delete path resolves the "+
			"agent's name and namespace from this id", byProfile[0].AgentID, ag.ID)
	}
	if byProfile[0].ProfileName != prof.Name {
		t.Errorf("ListGrantsForProfile returned ProfileName %q, want %q. The RBAC object name is "+
			"derived from it, so an empty or wrong one leaves objects nothing can delete",
			byProfile[0].ProfileName, prof.Name)
	}
	// Control: a profile nobody holds comes back empty rather than returning
	// everything, which is the failure mode of a WHERE clause on the wrong column.
	other, err := store.CreateProfile(ctx, privilege.Profile{Name: "ungranted-" + name})
	if err != nil {
		t.Fatalf("create control profile: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM privilege_profiles WHERE id=$1`, other.ID) })
	if rows, err := store.ListGrantsForProfile(ctx, other.ID); err != nil || len(rows) != 0 {
		t.Errorf("control FAILED: ListGrantsForProfile for an ungranted profile returned %d "+
			"row(s) (err %v), want 0 — the filter is not on profile_id", len(rows), err)
	}

	// Revoke → gone.
	if err := store.Revoke(ctx, ag.ID, prof.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	grants, _ = store.ListGrantsForAgent(ctx, ag.ID)
	if len(grants) != 0 {
		t.Fatalf("after revoke grants = %d, want 0", len(grants))
	}
	if rows, err := store.ListGrantsForProfile(ctx, prof.ID); err != nil || len(rows) != 0 {
		t.Errorf("after revoke ListGrantsForProfile = %d row(s) (err %v), want 0", len(rows), err)
	}
}
