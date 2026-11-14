package agentprovision

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

// updateSpy is the noop driver recording every spec Update applies.
type updateSpy struct {
	*provision.Noop
	specs []provision.Spec
}

func (d *updateSpy) Update(ctx context.Context, spec provision.Spec) error {
	d.specs = append(d.specs, spec)
	return d.Noop.Update(ctx, spec)
}

// specCarries reports whether spec places value as one of its secrets.
func specCarries(spec provision.Spec, value string) bool {
	for _, e := range spec.Secrets {
		if e.Value == value {
			return true
		}
	}
	return false
}

// TEAM SHARING PHASE 4, SLICE 4.5 at the adapter: a team's chief is told its
// owner is a team, and a manager's credential rotation mints new credentials
// and re-applies them to a live instance.

// TestATeamsChiefIsToldItsOwnerIsATeam: a claude-code chief whose owner the
// lookup names a team carries agentspec.TeamChiefSection(name) after the
// chief prose; a person's chief, and a team's WORKER, do not; and a lookup
// that fails refuses the spec rather than guess.
func TestATeamsChiefIsToldItsOwnerIsATeam(t *testing.T) {
	const team, person = int64(612), int64(41)
	lookup := func(_ context.Context, owner int64) (string, bool, error) {
		switch owner {
		case team:
			return "platform-crew", true, nil
		case person:
			return "", false, nil
		}
		return "", false, errors.New("users unreachable")
	}
	prose := func(t *testing.T, row agents.Agent) (string, error) {
		t.Helper()
		cfg := ccSpecConfig()
		a := adapterWith(t, cfg, row)
		a.UseTeamOwner(lookup)
		spec, err := a.buildSpec(context.Background(), row)
		if err != nil {
			return "", err
		}
		if len(spec.Files) == 0 {
			return "", nil
		}
		return string(spec.Files[0].Content), nil
	}
	chief := ccRow(agents.ChiefNameFor(team))
	chief.OwnerID = team
	got, err := prose(t, chief)
	if err != nil {
		t.Fatal(err)
	}
	if want := agentspec.ChiefInstructionsClaudeCode(false, false) + agentspec.TeamChiefSection("platform-crew"); got != want {
		t.Errorf("the team's chief prose is not the chief prose plus the team section")
	}
	if !strings.Contains(got, "your owner is the team `platform-crew`") || !strings.Contains(got, "[muster] typed by <name> (<role>) in team platform-crew") {
		t.Errorf("the team section does not name the team and the typed-by line it will be handed")
	}

	mine := ccRow(agents.ChiefNameFor(person))
	mine.OwnerID = person
	if got, err := prose(t, mine); err != nil || got != agentspec.ChiefInstructionsClaudeCode(false, false) {
		t.Errorf("a person's chief: err %v, prose is the plain chief prose %v; want it, with no team section", err, got == agentspec.ChiefInstructionsClaudeCode(false, false))
	}
	worker := ccRow("team-worker")
	worker.OwnerID = team
	if got, err := prose(t, worker); err != nil || got != "" {
		t.Errorf("a team's worker: err %v, %d bytes of prose; want none (a worker gets no instructions)", err, len(got))
	}
	lost := ccRow(agents.ChiefNameFor(99))
	lost.OwnerID = 99
	if _, err := prose(t, lost); err == nil || !strings.Contains(err.Error(), "could not tell whether its owner 99 is a team") {
		t.Errorf("a failed lookup: %v; want the spec refused", err)
	}
}

// TestRotatingCredentialsMintsNewOnesAndReappliesALiveInstance: on a running
// claude-code agent RotateCredentials writes a NEW hooks token and a NEW turn
// credential (64 hex characters each, distinct from each other and from the
// old ones) through the store, then re-applies the spec, which carries the new
// token. On a stopped agent it writes both and applies nothing (its next Start
// re-applies). A gateway agent gets a new hooks token and no turn credential.
func TestRotatingCredentialsMintsNewOnesAndReappliesALiveInstance(t *testing.T) {
	for _, c := range []struct {
		name, kind, status string
		turn, apply        bool
	}{
		{"running claude-code", agents.KindClaudeCode, agents.StatusRunning, true, true},
		{"stopped claude-code", agents.KindClaudeCode, agents.StatusStopped, true, false},
		{"stopped gateway", agents.KindGateway, agents.StatusStopped, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			row := ccRow("rot-worker")
			row.Kind, row.Status = c.kind, c.status
			if c.kind != agents.KindClaudeCode {
				row.CCAccount, row.GatewaySecret = "", ""
			}
			oldTok, oldTurn := row.HooksToken, row.GatewaySecret
			store := &recordingStore{agent: row}
			driver := &updateSpy{Noop: provision.MustNewNoop()}
			a, err := New(Config{Driver: driver, Store: store, Spec: ccSpecConfig()})
			if err != nil {
				t.Fatal(err)
			}
			if rotated, err := a.RotateCredentials(context.Background(), row.ID); err != nil || !rotated {
				t.Fatalf("rotated %v, %v; want rotated", rotated, err)
			}
			got := store.agent
			if len(got.HooksToken) != 64 || got.HooksToken == oldTok {
				t.Errorf("hooks token: %d chars, changed %v; want a new 64-char token", len(got.HooksToken), got.HooksToken != oldTok)
			}
			if c.turn && (len(got.GatewaySecret) != 64 || got.GatewaySecret == oldTurn || got.GatewaySecret == got.HooksToken) {
				t.Errorf("turn credential: %d chars, changed %v; want a new 64-char credential distinct from the token",
					len(got.GatewaySecret), got.GatewaySecret != oldTurn)
			}
			if !c.turn && got.GatewaySecret != oldTurn {
				t.Errorf("a %s agent was given a turn credential", c.kind)
			}
			if applied := len(driver.specs) > 0; applied != c.apply {
				t.Errorf("the spec was re-applied = %v; want %v (%s)", applied, c.apply, store.transcript())
			}
			if c.apply && !specCarries(driver.specs[len(driver.specs)-1], got.HooksToken) {
				t.Errorf("the re-applied spec does not carry the NEW hooks token")
			}
		})
	}
}

// TestARotationWhoseWriteFailsChangesNeitherCredential: the rotation's store
// write fails, and the agent keeps BOTH old credentials — RotateCredentials
// reports not rotated, writes neither credential on its own (no SetHooksToken,
// no SetGatewaySecret: the two go through the store's one transaction,
// agents.CredentialsWriter) and re-applies nothing. The Postgres half — the
// transaction itself rolling back — is internal/agents
// TestARotationThatFailsHalfWayLeavesTheOldCredentials.
//
// 🔴 RED at 23eb2f32: the hooks token was written (SetHooksToken) before the
// turn credential, so a failure of the second left the agent on a new token.
func TestARotationWhoseWriteFailsChangesNeitherCredential(t *testing.T) {
	row := ccRow("rot-fail")
	row.Status = agents.StatusRunning
	oldTok, oldTurn := row.HooksToken, row.GatewaySecret
	store := &recordingStore{agent: row, credsErr: errors.New("injected: the turn credential write failed")}
	driver := &updateSpy{Noop: provision.MustNewNoop()}
	a, err := New(Config{Driver: driver, Store: store, Spec: ccSpecConfig()})
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := a.RotateCredentials(context.Background(), row.ID)
	if err == nil || rotated {
		t.Fatalf("rotated %v, %v; want not rotated, with the error", rotated, err)
	}
	if store.agent.HooksToken != oldTok || store.agent.GatewaySecret != oldTurn {
		t.Errorf("after a failed rotation the hooks token changed %v and the turn credential %v; want neither (%s)",
			store.agent.HooksToken != oldTok, store.agent.GatewaySecret != oldTurn, store.transcript())
	}
	if len(driver.specs) != 0 {
		t.Errorf("a failed rotation re-applied the instance %d time(s)", len(driver.specs))
	}
}
