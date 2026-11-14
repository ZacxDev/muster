package agents

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestARotationThatFailsHalfWayLeavesTheOldCredentials: SetCredentials (the
// rotation's one write, agentprovision.Adapter.RotateCredentials) stores a
// team agent's new hooks token AND turn credential in one transaction. With
// the SECOND write made to fail in Postgres (a trigger refusing any change of
// this agent's turn credential), the call fails and the agent keeps BOTH old
// credentials: the new hooks token is not in agent_secrets either. CONTROL:
// with the fault removed the same call stores both.
//
// 🔴 RED at 23eb2f32 in its adapter form: RotateCredentials wrote the token
// (SetHooksToken) and committed it before the turn credential's write failed.
func TestARotationThatFailsHalfWayLeavesTheOldCredentials(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 60*time.Second)
	defer cancel()
	w := newTeamSecretsWorld(t, ctx)
	sfx := time.Now().Format("150405.000000")
	oldTok, oldGW := "rot-old-hooks-"+sfx, "rot-old-turn-"+sfx
	ta, err := w.store.Create(w.teamCtx, Agent{Name: "team-rot-" + sfx, Namespace: "ns-team-rot-" + sfx,
		Status: StatusStopped, HooksToken: oldTok, Kind: KindClaudeCode, CCAccount: "acct"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.store.SetGatewaySecret(w.teamCtx, ta.ID, oldGW); err != nil {
		t.Fatal(err)
	}

	sys := authz.System(context.Background())
	// The fault is installed as the database's LOGIN role (dbtest.OwnerExec):
	// the application role the store runs as may not create a trigger.
	dsn := dbtest.DSN(t)
	fn := fmt.Sprintf("rot_fault_%d", ta.ID)
	for _, q := range []string{
		`CREATE FUNCTION ` + fn + `() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN
		   IF NEW.agent_id = ` + fmt.Sprint(ta.ID) + ` AND NEW.gateway_secret IS DISTINCT FROM OLD.gateway_secret THEN
		     RAISE EXCEPTION 'injected fault: the turn credential write fails';
		   END IF;
		   RETURN NEW;
		 END $$`,
		`CREATE TRIGGER ` + fn + ` BEFORE UPDATE ON agent_secrets FOR EACH ROW EXECUTE FUNCTION ` + fn + `()`,
	} {
		dbtest.OwnerExec(t, dsn, q)
	}
	dropped := false
	drop := func() {
		if dropped {
			return
		}
		dropped = true
		dbtest.OwnerExec(t, dsn, `DROP TRIGGER IF EXISTS `+fn+` ON agent_secrets`)
		dbtest.OwnerExec(t, dsn, `DROP FUNCTION IF EXISTS `+fn+`()`)
	}
	t.Cleanup(drop)

	newTok, newGW := "rot-new-hooks-"+sfx, "rot-new-turn-"+sfx
	if err := w.store.SetCredentials(w.teamCtx, ta.ID, newTok, newGW); err == nil {
		t.Fatal("SetCredentials with the turn credential's write failing answered nil")
	}
	if s, _, ok := secretsRow(t, w.pool, ta.ID); !ok || s.HooksToken != oldTok || s.GatewaySecret != oldGW {
		t.Fatalf("after a failed rotation: hooks token old=%v new=%v, turn credential old=%v; want BOTH old credentials kept",
			s.HooksToken == oldTok, s.HooksToken == newTok, s.GatewaySecret == oldGW)
	}

	drop()
	if err := w.store.SetCredentials(w.teamCtx, ta.ID, newTok, newGW); err != nil {
		t.Fatalf("control: SetCredentials with no fault: %v", err)
	}
	if s, _, ok := secretsRow(t, w.pool, ta.ID); !ok || s.HooksToken != newTok || s.GatewaySecret != newGW {
		t.Fatalf("control: after a rotation: hooks token new=%v, turn credential new=%v; want both new",
			s.HooksToken == newTok, s.GatewaySecret == newGW)
	}
	if h, g, _ := rowSecrets(t, sys, w.pool, ta.ID); h != "" || g != "" {
		t.Errorf("LEAK: the team agent's row carries a credential after SetCredentials (hooks %v, turn %v)", h != "", g != "")
	}
}
