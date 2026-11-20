package api

// chief_restart IS NOT STEP-UP GATED (operator decision; host_stepup.go,
// hostStepUpExempt): from a machine the gate covers — here one whose host
// token is marked as a spawning host's (mirror_tokens.spawned_at, read
// through the real token store, as spawn_authority_pg_test.go marks it) — the
// owner's session restarts the chief AT ONCE, force included, and no
// confirmation card is filed. Every other owner-authority tool on the same
// machine is still a card (the agent_destroy control).
//
// ⚠ These tests name only identifiers that exist before the change (the
// pending status and the card's asker label are string literals), so the file
// also compiles against the earlier tree, where the two restart tests are red
// for a behavioural reason (the call files a card and nothing is restarted).

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
)

// markedRestartHost swaps f's token store for the real one and mints user 1's
// host "ws" a token MARKED as one whose host has started agents for the chief
// — a machine the step-up gate covers (hostSpawnStepUp, spawnedTokenWhy).
func markedRestartHost(t *testing.T, f *restartFixture) string {
	t.Helper()
	tokens := mirror.NewPG(f.pool)
	f.srv.ext.MirrorTokens = tokens
	tok, rec, err := tokens.CreateToken(asUser(1), "ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := tokens.MarkSpawned(authz.System(context.Background()), rec.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		_, _ = f.pool.Exec(sys, `UPDATE mirror_tokens SET revoked_at = now() WHERE owner_id = 1 AND host = 'ws' AND revoked_at IS NULL`)
		_, _ = f.pool.Exec(sys, `DELETE FROM attention_asks WHERE owner_id = 1 AND system_asker AND asker_label = 'step-up'`)
	})
	return tok
}

// stepUpCards counts user 1's step-up confirmation cards.
func stepUpCards(t *testing.T, f *restartFixture) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(authz.System(context.Background()),
		`SELECT count(*) FROM attention_asks WHERE owner_id = 1 AND system_asker AND asker_label = 'step-up'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestChiefRestartFromACoveredHostRunsAtOnce: the owner's session on a marked
// host restarts the chief through the real /mirror/mcp door: Stop then Start
// of the chief, a restarted result, no pending status and no card.
func TestChiefRestartFromACoveredHostRunsAtOnce(t *testing.T) {
	f := newRestartFixture(t)
	tok := markedRestartHost(t, f)
	before := stepUpCards(t, f)
	text, isErr := f.restart(t, tok, focusSID, map[string]any{})
	if strings.Contains(text, "awaiting_owner_confirmation") {
		t.Fatalf("chief_restart from a covered host asked the owner's phone instead of running: %s", text)
	}
	if isErr {
		t.Fatalf("chief_restart from a covered host: %s", text)
	}
	if n := stepUpCards(t, f); n != before {
		t.Fatalf("chief_restart from a covered host filed %d confirmation card(s)", n-before)
	}
	id := strconv.FormatInt(f.ownChief.ID, 10)
	if got := strings.Join(f.prov.callList(), " "); got != "stop:"+id+" start:"+id {
		t.Fatalf("provisioner calls %q, want stop then start of the chief %s only", got, id)
	}
	if r := decodeRestart(t, text); !r.Restarted || r.Forced || r.AgentID != f.ownChief.ID {
		t.Fatalf("result: %s", text)
	}
}

// TestAForcedChiefRestartFromACoveredHostCutsTheTurnOff: the same machine, the
// chief mid-turn (a delivery's session claim held and a chat turn running):
// unforced it is still refused as mid-turn (the gate change touches nothing
// else), and force:true restarts it at once, with no card.
func TestAForcedChiefRestartFromACoveredHostCutsTheTurnOff(t *testing.T) {
	f := newRestartFixture(t)
	tok := markedRestartHost(t, f)
	if won, err := f.srv.ext.Agents.ClaimKickoff(asUser(1), f.ownChief.ID, "plugin-delivery/test", time.Minute); err != nil || !won {
		t.Fatalf("could not take the chief's claim for the test (%v)", err)
	}
	t.Cleanup(func() { _ = f.srv.ext.Agents.ReleaseKickoffClaim(asUser(1), f.ownChief.ID, "plugin-delivery/test") })
	if !f.srv.turns.enter(f.ownChief.ID) {
		t.Fatal("could not start a chat turn for the test")
	}
	defer f.srv.turns.leave(f.ownChief.ID)
	before := stepUpCards(t, f)

	text, isErr := f.restart(t, tok, focusSID, map[string]any{})
	// Errorf, not Fatalf: the forced call below is the assertion this test is
	// for, so it runs whatever the unforced one answered.
	if !isErr || !strings.Contains(text, "is mid-turn") || len(f.prov.callList()) != 0 {
		t.Errorf("unforced, mid-turn, from a covered host: %s (calls %v), want the mid-turn refusal and nothing stopped", text, f.prov.callList())
	}
	before = stepUpCards(t, f)

	text, isErr = f.restart(t, tok, focusSID, map[string]any{"force": true})
	if strings.Contains(text, "awaiting_owner_confirmation") {
		t.Fatalf("forced chief_restart from a covered host asked the owner's phone instead of running: %s", text)
	}
	if isErr {
		t.Fatalf("forced chief_restart from a covered host: %s", text)
	}
	if n := stepUpCards(t, f); n != before {
		t.Fatalf("forced chief_restart from a covered host filed %d confirmation card(s)", n-before)
	}
	id := strconv.FormatInt(f.ownChief.ID, 10)
	if got := strings.Join(f.prov.callList(), " "); got != "stop:"+id+" start:"+id {
		t.Fatalf("forced calls %q, want stop then start of the chief %s only", got, id)
	}
	if r := decodeRestart(t, text); !r.Restarted || !r.Forced {
		t.Fatalf("forced result: %s", text)
	}
}

// TestAGatedToolFromTheSameCoveredHostStillFilesAStepUpCard is the control: on the
// very machine chief_restart now runs from at once, agent_destroy is still a
// request to the owner's phone — the pending status, one card, and the agent
// untouched — so the exemption is chief_restart's alone. (Green before the
// change too: it proves the host IS covered, so the two tests above are red
// on the earlier tree for the gate, not for the fixture.)
func TestAGatedToolFromTheSameCoveredHostStillFilesAStepUpCard(t *testing.T) {
	f := newRestartFixture(t)
	tok := markedRestartHost(t, f)
	before := stepUpCards(t, f)
	code, r, body := f.mirrorRPC(t, mirrorAs(tok, focusSID), mcpToolCall(agentDestroyTool, map[string]any{"id": f.op.ID}))
	if code != 200 || r == nil || r.Error != nil {
		t.Fatalf("agent_destroy: HTTP %d %s", code, body)
	}
	text, isErr := toolText(t, r)
	if isErr || !strings.Contains(text, "awaiting_owner_confirmation") {
		t.Fatalf("agent_destroy from a covered host: %s, want a confirmation card", text)
	}
	if n := stepUpCards(t, f); n != before+1 {
		t.Fatalf("agent_destroy from a covered host filed %d card(s), want 1", n-before)
	}
	if a, err := agents.NewPG(f.pool).Get(asUser(1), f.op.ID); err != nil || a.ID != f.op.ID {
		t.Fatalf("agent_destroy ran before the owner confirmed: %v", err)
	}
	if got := f.prov.callList(); len(got) != 0 {
		t.Fatalf("agent_destroy from a covered host reached the provisioner: %v", got)
	}
}

// TestChiefRestartFromEveryOtherCoveredCallerRunsAtOnce: the gate's two other
// covered callers — a REMOTE caller (a host mirrored to another muster, which
// this one cannot see) and a machine with a HOST OFFICER declared on it — each
// restart the chief at once with no card, through the real /mirror/mcp door,
// while agent_destroy from the same caller still files one (the control, so
// each case's red on the earlier tree is the gate's).
func TestChiefRestartFromEveryOtherCoveredCallerRunsAtOnce(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, f *restartFixture) map[string]string
	}{
		{"remote caller", func(t *testing.T, f *restartFixture) map[string]string {
			remote := mirrorTok('r')
			f.tokens[remote] = mirror.Token{ID: 9, OwnerID: 1, Host: "far"}
			prev := mirrorBindWait
			mirrorBindWait = 0
			t.Cleanup(func() { mirrorBindWait = prev })
			return remoteAs(remote, remoteSID)
		}},
		{"host officer machine", func(t *testing.T, f *restartFixture) map[string]string {
			declareHostOfficerOn(t, f.srv, 1, "ws")
			return mirrorAs(f.ws, focusSID)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRestartFixture(t)
			t.Cleanup(func() {
				_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM attention_asks WHERE owner_id = 1 AND system_asker AND asker_label = 'step-up'`)
			})
			hdr := c.setup(t, f)
			before := stepUpCards(t, f)

			// The control first: this caller IS covered.
			_, r, body := f.mirrorRPC(t, hdr, mcpToolCall(agentDestroyTool, map[string]any{"id": f.op.ID}))
			if r == nil || r.Error != nil || !strings.Contains(body, "awaiting_owner_confirmation") {
				t.Fatalf("CONTROL: agent_destroy from the %s filed no card: %s", c.name, body)
			}
			if n := stepUpCards(t, f); n != before+1 {
				t.Fatalf("CONTROL: agent_destroy from the %s filed %d card(s), want 1", c.name, n-before)
			}
			before++

			code, r, body := f.mirrorRPC(t, hdr, mcpToolCall("chief_restart", map[string]any{"force": true}))
			if code != 200 || r == nil || r.Error != nil {
				t.Fatalf("chief_restart from the %s: HTTP %d %s", c.name, code, body)
			}
			text, isErr := toolText(t, r)
			if strings.Contains(text, "awaiting_owner_confirmation") {
				t.Fatalf("chief_restart from the %s asked the owner's phone instead of running: %s", c.name, text)
			}
			if isErr {
				t.Fatalf("chief_restart from the %s: %s", c.name, text)
			}
			if n := stepUpCards(t, f); n != before {
				t.Fatalf("chief_restart from the %s filed %d confirmation card(s)", c.name, n-before)
			}
			id := strconv.FormatInt(f.ownChief.ID, 10)
			if got := strings.Join(f.prov.callList(), " "); got != "stop:"+id+" start:"+id {
				t.Fatalf("provisioner calls %q, want stop then start of the chief %s only", got, id)
			}
			if res := decodeRestart(t, text); !res.Restarted || res.AgentID != f.ownChief.ID {
				t.Fatalf("result: %s", text)
			}
		})
	}
}
