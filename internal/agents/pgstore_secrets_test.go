package agents

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// secretsFixture is a migrated store, its pool, and a fresh agent.
func secretsFixture(t *testing.T, ctx context.Context, tag, tok string) (*PGStore, *pgxpool.Pool, Agent) {
	t.Helper()
	store := requirePGStore(t, ctx)
	pool, ok := store.pool.(*pgxpool.Pool)
	if !ok {
		t.Fatalf("instrument check FAILED: the store is not on a pool (%T)", store.pool)
	}
	name := tag + "-" + time.Now().Format("150405.000000")
	a, err := store.Create(ctx, Agent{Name: name, Namespace: "muster-agent-" + name,
		Status: StatusProvisioning, HooksToken: tok, Kind: KindClaudeCode, CCAccount: "acct"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupAgent(context.Background(), pool, a.ID) })
	return store, pool, a
}

// secretsRow is agent id's agent_secrets row, read outside the store
// (scope all); ok is false when there is none.
func secretsRow(t *testing.T, pool *pgxpool.Pool, id int64) (s Secrets, owner int64, ok bool) {
	t.Helper()
	err := pool.QueryRow(authz.System(context.Background()),
		`SELECT hooks_token, gateway_secret, owner_id FROM agent_secrets WHERE agent_id=$1`, id).
		Scan(&s.HooksToken, &s.GatewaySecret, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return Secrets{}, 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return s, owner, true
}

// TestTheCredentialsAreStoredApartAndDualWritten: Create and the two mints
// write agent_secrets (owned by the agent's owner) AND the old agents columns
// an older image reads; Secrets returns them; GetByHooksToken resolves the
// token; and a second agent's credentials are its own.
func TestTheCredentialsAreStoredApartAndDualWritten(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	tok := "fixture-hooks-split-" + time.Now().Format("150405.000000")
	store, pool, a := secretsFixture(t, ctx, "cred-split", tok)
	_, _, b := secretsFixture(t, ctx, "cred-split-b", "")

	if s, owner, ok := secretsRow(t, pool, a.ID); !ok || s.HooksToken != tok || s.GatewaySecret != "" || owner != a.OwnerID {
		t.Fatalf("after Create, agent_secrets holds (%v, owner %d, row %v); want the token, owned by %d",
			s.HooksToken == tok, owner, ok, a.OwnerID)
	}
	if _, _, ok := secretsRow(t, pool, b.ID); ok {
		t.Fatalf("Create with no token wrote an agent_secrets row")
	}

	const secret = "fixture-turn-credential-split-3d17"
	if err := store.SetGatewaySecret(ctx, a.ID, secret); err != nil {
		t.Fatal(err)
	}
	const btok = "fixture-hooks-split-b-minted"
	if err := store.SetHooksToken(ctx, b.ID, btok); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		ag   Agent
		want Secrets
	}{{a, Secrets{tok, secret}}, {b, Secrets{btok, ""}}} {
		if got, _, ok := secretsRow(t, pool, c.ag.ID); !ok || got != c.want {
			t.Errorf("agent %d: agent_secrets holds the expected credentials: %v (row %v)", c.ag.ID, got == c.want, ok)
		}
		var oldTok, oldSecret string
		if err := pool.QueryRow(ctx, `SELECT hooks_token, gateway_secret FROM agents WHERE id=$1`, c.ag.ID).
			Scan(&oldTok, &oldSecret); err != nil {
			t.Fatal(err)
		}
		if (Secrets{oldTok, oldSecret}) != c.want {
			t.Errorf("agent %d: the old agents columns were not dual-written (an older image reads them)", c.ag.ID)
		}
		got, err := store.Secrets(ctx, c.ag.ID)
		if err != nil || got != c.want {
			t.Errorf("agent %d: Secrets returned the stored credentials: %v (err %v)", c.ag.ID, got == c.want, err)
		}
		byTok, err := store.GetByHooksToken(ctx, c.want.HooksToken)
		if err != nil || byTok.ID != c.ag.ID {
			t.Errorf("GetByHooksToken resolved agent %d's token to %d (err %v)", c.ag.ID, byTok.ID, err)
		}
		withSecrets, err := WithSecrets(ctx, store, byTok)
		if err != nil || withSecrets.HooksToken != c.want.HooksToken || withSecrets.GatewaySecret != c.want.GatewaySecret {
			t.Errorf("agent %d: WithSecrets did not fill the stored credentials (err %v)", c.ag.ID, err)
		}
	}
}

// TestATokenHeldOnlyInAgentSecretsResolves is the shape a team agent will
// have (its agents.hooks_token stays ”): the token lives only in
// agent_secrets, and both the token lookup and Secrets find it.
func TestATokenHeldOnlyInAgentSecretsResolves(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	store, pool, a := secretsFixture(t, ctx, "cred-only-secrets", "")
	tok := "fixture-hooks-only-in-secrets-" + time.Now().Format("150405.000000")
	if _, err := pool.Exec(ctx, `INSERT INTO agent_secrets (agent_id, hooks_token) VALUES ($1, $2)`, a.ID, tok); err != nil {
		t.Fatal(err)
	}
	var old string
	if err := pool.QueryRow(ctx, `SELECT hooks_token FROM agents WHERE id=$1`, a.ID).Scan(&old); err != nil || old != "" {
		t.Fatalf("instrument check FAILED: the agents row holds a token (%v, err %v)", old != "", err)
	}
	got, err := store.GetByHooksToken(ctx, tok)
	if err != nil || got.ID != a.ID {
		t.Fatalf("a token held only in agent_secrets resolved to agent %d (err %v), want %d", got.ID, err, a.ID)
	}
	if s, err := store.Secrets(ctx, a.ID); err != nil || s.HooksToken != tok {
		t.Fatalf("Secrets did not return the token held in agent_secrets (err %v)", err)
	}
}

// TestAnOlderImagesMintIsStillFound: an older image (rolled back to after this
// migration ran) writes only the old agents column. Secrets and the token
// lookup fall back to it, so the agent is neither locked out nor re-minted a
// credential its pod does not hold.
func TestAnOlderImagesMintIsStillFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	store, pool, a := secretsFixture(t, ctx, "cred-old-image", "")
	tok := "fixture-hooks-old-image-" + time.Now().Format("150405.000000")
	const secret = "fixture-turn-credential-old-image-90ab"
	if _, err := pool.Exec(ctx, `UPDATE agents SET hooks_token=$2, gateway_secret=$3 WHERE id=$1`, a.ID, tok, secret); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := secretsRow(t, pool, a.ID); ok {
		t.Fatalf("instrument check FAILED: the older-image write produced an agent_secrets row")
	}
	if s, err := store.Secrets(ctx, a.ID); err != nil || s != (Secrets{tok, secret}) {
		t.Fatalf("Secrets did not fall back to the old columns (err %v)", err)
	}
	if got, err := store.GetByHooksToken(ctx, tok); err != nil || got.ID != a.ID {
		t.Fatalf("GetByHooksToken did not fall back to the old column: agent %d (err %v)", got.ID, err)
	}
}

// TestTheEmptyTokenResolvesNoAgent: many rows hold no token; presenting none
// must identify none of them.
func TestTheEmptyTokenResolvesNoAgent(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	store, _, _ := secretsFixture(t, ctx, "cred-empty", "")
	if got, err := store.GetByHooksToken(ctx, ""); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("the empty token resolved agent %d (err %v), want no row", got.ID, err)
	}
}

// TestAnotherOwnersPrincipalReadsNoCredentials: agent_secrets is under
// owner_isolation. A member principal reads none of another owner's
// credentials — not through Secrets, not by a raw SELECT — while the owner
// does (the positive control that the row is there to be hidden).
func TestAnotherOwnersPrincipalReadsNoCredentials(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	tok := "fixture-hooks-other-owner-" + time.Now().Format("150405.000000")
	store, pool, a := secretsFixture(t, ctx, "cred-other-owner", tok)

	sys := authz.System(context.Background())
	var two int64
	if err := pool.QueryRow(sys, `INSERT INTO users (role) VALUES ('member') RETURNING id`).Scan(&two); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(authz.System(context.Background()), `DELETE FROM users WHERE id=$1`, two) })
	asTwo := authz.WithPrincipal(ctx, authz.Principal{UserID: two, Role: authz.RoleMember, Via: authz.ViaSession})

	count := func(c context.Context) int {
		var n int
		if err := pool.QueryRow(c, `SELECT count(*) FROM agent_secrets WHERE agent_id=$1`, a.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(ctx); n != 1 {
		t.Fatalf("instrument check FAILED: the owner sees %d agent_secrets rows for its agent, want 1", n)
	}
	if n := count(asTwo); n != 0 {
		t.Errorf("another owner's principal sees %d agent_secrets rows of an agent it does not own", n)
	}
	if s, err := store.Secrets(asTwo, a.ID); err == nil || s.HooksToken != "" {
		t.Errorf("another owner's principal read the agent's credentials through Secrets (token set: %v, err %v)",
			s.HooksToken != "", err)
	}
	if _, err := pool.Exec(asTwo, `INSERT INTO agent_secrets (agent_id, owner_id, hooks_token) VALUES ($1, $2, 'planted')`,
		a.ID, a.OwnerID); err == nil {
		t.Errorf("another owner's principal wrote an agent_secrets row naming the agent's owner")
	}
}

// TestDeletingAnAgentDeletesItsCredentials: the composite foreign key
// cascades, so no credential outlives its agent.
func TestDeletingAnAgentDeletesItsCredentials(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	tok := "fixture-hooks-cascade-" + time.Now().Format("150405.000000")
	store, pool, a := secretsFixture(t, ctx, "cred-cascade", tok)
	if _, _, ok := secretsRow(t, pool, a.ID); !ok {
		t.Fatalf("instrument check FAILED: no agent_secrets row to cascade")
	}
	if err := store.Delete(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := secretsRow(t, pool, a.ID); ok {
		t.Fatalf("the agent's credentials outlived it")
	}
}
