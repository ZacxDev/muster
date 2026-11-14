package agents

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestSetGatewaySecretRoundTripsAndStaysOutOfTheJSON: on real Postgres, a new
// row has no turn credential (""), SetGatewaySecret stores one that Secrets
// returns (and WithSecrets fills), only for THAT row; no ordinary read carries
// it (TestNoOrdinaryReadOfAnAgentCarriesItsCredentials pins every one), and the
// filled row's JSON never does.
func TestSetGatewaySecretRoundTripsAndStaysOutOfTheJSON(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	store := requirePGStore(t, ctx)

	stamp := time.Now().Format("150405.000000")
	mk := func(name, tok string) Agent {
		a, err := store.Create(ctx, Agent{Name: name, Namespace: "muster-agent-" + name,
			Status: StatusProvisioning, HooksToken: tok, Kind: KindClaudeCode, CCAccount: "acct"})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		t.Cleanup(func() { cleanupAgent(dbtest.AsOperator(context.Background()), store.pool, a.ID) })
		return a
	}
	a := mk("turn-cred-a-"+stamp, "fixture-hooks-a-"+stamp)
	b := mk("turn-cred-b-"+stamp, "fixture-hooks-b-"+stamp)
	for _, x := range []Agent{a, b} {
		if s, err := store.Secrets(ctx, x.ID); err != nil || s.GatewaySecret != "" {
			t.Fatalf("a new row carries a turn credential (err %v)", err)
		}
	}

	const secret = "fixture-turn-credential-pg-5e21"
	if err := store.SetGatewaySecret(ctx, a.ID, secret); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, err = WithSecrets(ctx, store, got); err != nil {
		t.Fatal(err)
	}
	if got.GatewaySecret != secret {
		t.Errorf("WithSecrets returned turn credential %q, want the stored one", got.GatewaySecret)
	}
	if blob, _ := json.Marshal(got); strings.Contains(string(blob), secret) {
		t.Errorf("the filled row's JSON carries the turn credential")
	}
	if other, err := store.Secrets(ctx, b.ID); err != nil || other.GatewaySecret != "" {
		t.Errorf("SetGatewaySecret on one row set another's: %q (err %v)", other.GatewaySecret, err)
	}
}
