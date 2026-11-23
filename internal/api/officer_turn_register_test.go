package api

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// registryProbeChat is a runtime that looks at the server's per-turn record
// for the agent while the re-prompt's turn is being sent: before and (when
// accept) after it plays ccd's 103 acceptance, then answers busy (409, before
// anything was typed) or with a reply.
type registryProbeChat struct {
	stubGateway
	s             *Server
	accept, busy  bool
	mu            sync.Mutex
	before, after int64
}

func (c *registryProbeChat) Chat(ctx context.Context, a agents.Agent, _, _ string, _ func(string)) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.before = c.s.turnInputs.only(a.ID)
	if c.busy {
		return "", &agents.RuntimeError{Status: http.StatusConflict, Type: "busy", Message: "a turn holds the session"}
	}
	if c.accept {
		agents.MarkTurnAccepted(ctx, agents.TurnAcceptance{})
		c.after = c.s.turnInputs.only(a.ID)
	}
	return "done", nil
}

// TestARepromptIsRecordedOnlyOnceCCDAcceptsIt (round 1's Y3): a re-prompt's
// prompt row is in the per-turn record (what an item's moment is read from)
// only AFTER ccd accepts the turn — never while it is being sent — so a turn
// ccd refuses busy (409, nothing typed) is never recorded, and its row, taken
// back, can never be written as an item's origin_message_id. An accepted
// turn is recorded with ITS prompt row, and the record ends with the turn.
func TestARepromptIsRecordedOnlyOnceCCDAcceptsIt(t *testing.T) {
	r := newOfficerRig(t)
	inst := r.addInstance(r.b, 1, 893)
	r.store.mu.Lock()
	r.store.rows[len(r.store.rows)-1].KickedOff = true
	r.store.mu.Unlock()
	rec := &recordingAgents{officerAgents: r.store}
	r.s.ext.Agents = rec

	busy := &registryProbeChat{s: r.s, busy: true}
	r.s.ext.Gateway = busy
	if err := (officerFleet{s: r.s}).Reprompt(context.Background(), r.b, 1, "new work"); err != nil {
		t.Fatal(err)
	}
	r.s.WaitForOfficerReprompts()
	if busy.before != 0 || r.s.turnInputs.only(inst.ID) != 0 || rec.rows != 0 {
		t.Fatalf("a busy-refused re-prompt: recorded %d while sent, %d after, %d rows left; want 0, 0, 0", busy.before, r.s.turnInputs.only(inst.ID), rec.rows)
	}

	ok := &registryProbeChat{s: r.s, accept: true}
	r.s.ext.Gateway = ok
	if err := (officerFleet{s: r.s}).Reprompt(context.Background(), r.b, 1, "new work"); err != nil {
		t.Fatal(err)
	}
	r.s.WaitForOfficerReprompts()
	// The busy turn's row was 1 (taken back); this turn's prompt is row 2.
	if ok.before != 0 || ok.after != 2 {
		t.Fatalf("an accepted re-prompt: recorded %d before ccd's acceptance and %d after; want 0, then its prompt row 2", ok.before, ok.after)
	}
	waitNoTurn(t, r.s, inst.ID) // officer_origin_pg_test.go
}
