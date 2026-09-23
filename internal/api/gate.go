package api

import (
	"context"
	"errors"
	"time"
)

// ---------------------------------------------------------------------------
// THE APPROVAL GATE, AS SEEN FROM THIS SIDE OF THE CARVE.
//
// An agent that reaches a checkpoint has to ask a human. muster has no approval
// queue — the permission router owns it, and owning it is what that service IS
// — so asking is a network call now, where it used to be a store write in the
// same process.
//
// 🔴 EVERY EXIT FROM THIS FILE THAT IS NOT AN EXPLICIT APPROVAL IS A REFUSAL,
// AND THE DEFAULT SURVIVES EVERY NEW FAILURE MODE THE HOP INTRODUCES. That
// sentence was already true in-process, where the only ways to fail were "no
// store" and "timed out". Across a network it is also true for: no router
// configured, a DNS failure, a refused connection, a 401 because the credential
// is wrong, a 503 because the router has no queue, an unparseable body, and a
// state name this client does not recognise. All of them land on
// `approved:false` with a status the agent can read, because "I could not ask"
// and "they said yes" must never be the same value. A gate that fails open is
// not a gate; it is a delay.
//
// ⚠ WHAT THE HOP GENUINELY CHANGES, STATED RATHER THAN GLOSSED: in-process, a
// checkpoint could only be lost by the operator not answering. Now a network
// partition refuses checkpoints that a human would have approved, and an agent
// sees `status:"unavailable"` rather than waiting. That is the correct
// direction to fail, and it is a real behaviour change — a partition now stops
// agent work rather than delaying it.
// ---------------------------------------------------------------------------

// GateSpec describes the approval card an agent is asking a human to judge.
type GateSpec struct {
	Type    string
	Tool    string
	Command string
	Host    string
	Project string
	Cwd     string
	Session string
	Context []string
}

// GateDecision is the answer to one gate read.
type GateDecision struct {
	// State is one of GateStateDecided, GateStatePending, GateStateGone.
	State string
	// Response is the human's answer when State is decided ("approve" / "reject").
	Response string
	// Comment is the human's note, when they left one.
	Comment string
}

// The three states a gate read can report. They are the router's wire
// vocabulary, spelled here so a handler does not have to import the client.
const (
	// GateStateDecided: a human answered.
	GateStateDecided = "decided"
	// GateStatePending: the card is on the operator's screen, undecided.
	GateStatePending = "pending"
	// GateStateGone: no decision will ever arrive. A caller MUST stop polling.
	GateStateGone = "gone"
)

// GatePort is the approval queue muster asks through.
//
// 🔴 A nil GatePort MEANS "NOBODY CAN BE ASKED", WHICH MEANS "NOT APPROVED".
// It does not mean "proceed"; see this file's header.
type GatePort interface {
	// MintGate files an approval request and returns its id.
	MintGate(ctx context.Context, spec GateSpec) (string, error)
	// ReadGate reports the current state of a request this caller minted.
	ReadGate(ctx context.Context, id string) (GateDecision, error)
	// ClearGate removes a pending card. Best-effort.
	ClearGate(ctx context.Context, id string) error
}

// gateTypeCheckpoint is the request type an agent checkpoint is filed under.
//
// 🔴 IT IS A LITERAL HERE AND AN ALLOWLIST ENTRY ON THE ROUTER, AND THE
// DUPLICATION IS DELIBERATE. The router's gate mints only allowlisted types and
// refuses anything else; this side cannot import that allowlist without
// importing the service. A typo here is therefore a 400 at the door naming the
// type — loud — rather than a silently mis-filed card.
const gateTypeCheckpoint = "checkpoint"

// errNoGate is the refusal when no approval queue is reachable.
var errNoGate = errors.New("this server has no approval queue, so no approval could be sought")

// ⚠ THE POLL TUNING IS NOT DECLARED HERE. checkpointPollInterval,
// checkpointMaxWait and checkpointMissesBeforeExpired live beside the checkpoint
// tool in agent.go and are UNCHANGED from the in-process implementation — one
// second, one hour, five consecutive misses. Re-declaring them here with fresh
// numbers is the shape of change that reads as a port and lands as a behaviour
// change: a shorter backstop silently turns approvals a human WOULD have given
// into timeouts, and nothing in a diff of this file would say so.
//
// ⚠ WHAT THE MISS COUNTER NOW COUNTS IS GENUINELY DIFFERENT, AND THAT IS NOT
// HIDDEN. In-process it counted "the request row was not found", a
// database-level blip. Here it counts a failed HTTP read — DNS, connection,
// status, decode. The tolerance value is kept; the population it tolerates is
// wider, because there is more between this process and the answer than there
// used to be.

// gate returns the approval queue, or nil when none is configured.
func (s *Server) gate() GatePort {
	if s.gatePort == nil {
		return nil
	}
	return s.gatePort
}

// mintGateRequest files a checkpoint card and returns its id.
//
// 🔴 THE TYPE ALLOWLIST IS THE ROUTER'S, NOT REPRODUCED HERE, AND THAT IS
// DELIBERATE. Upstream the check lives inside the chokepoint so no caller can
// perform half of it; re-implementing it on this side would create a second
// spelling that can drift in the direction that OPENS the gate. This side sends
// the type and lets the door refuse.
func (s *Server) mintGateRequest(ctx context.Context, spec GateSpec) (string, error) {
	g := s.gate()
	if g == nil {
		return "", errNoGate
	}
	return g.MintGate(ctx, spec)
}

// clearGateRequest removes a pending card. Best-effort by contract: a recorded
// decision already cleared it, so this covers the timeout and abort paths.
func (s *Server) clearGateRequest(ctx context.Context, id string) {
	g := s.gate()
	if g == nil {
		return
	}
	if err := g.ClearGate(ctx, id); err != nil {
		s.logger.Printf("gate: clearing card %s: %v", id, err)
	}
}

// awaitCheckpointDecision blocks until the request is decided, reported gone,
// the backstop elapses, or the context is cancelled, returning the decision and
// a status string.
//
// The status distinguishes a real decision ("resolved") from every non-decision
// exit, so the agent knows it was NOT approved and why.
//
// 🔴 A TRANSPORT ERROR IS TOLERATED FOR A FEW TICKS AND THEN BECOMES "expired",
// AND THE TOLERANCE IS WHY THIS IS NOT A ONE-SHOT READ. A single failed poll is
// indistinguishable from a router restart, a rolling deploy, or one dropped
// packet — abandoning a live card on the first error would strand a checkpoint
// a human is about to answer. Several CONSECUTIVE failures are a different
// claim, and that is the one acted on. A successful read resets the counter,
// which is what stops an intermittent link from accumulating its way to a false
// expiry over half an hour.
func (s *Server) awaitCheckpointDecision(ctx context.Context, id string) (GateDecision, string) {
	g := s.gate()
	if g == nil {
		return GateDecision{}, "unavailable"
	}
	deadline := s.now().Add(checkpointMaxWait)
	t := time.NewTicker(checkpointPollInterval)
	defer t.Stop()
	misses := 0
	for {
		dec, err := g.ReadGate(ctx, id)
		switch {
		case err != nil:
			// Could be a real outage OR a transient blip. Only conclude after
			// several consecutive misses; see this function's header.
			s.logger.Printf("gate: reading card %s: %v", id, err)
			if misses++; misses >= checkpointMissesBeforeExpired {
				return GateDecision{}, "expired"
			}
		case dec.State == GateStateDecided:
			return dec, "resolved"
		case dec.State == GateStateGone:
			// 🔴 `gone` IS CONCLUSIVE AND IS NOT A MISS. The router answers it for a
			// card that was evicted, swept or deleted — no decision will ever arrive
			// — so retrying would poll a dead id until the deadline and report
			// "timeout", which names the wrong cause to whoever reads the log.
			return GateDecision{}, "expired"
		default:
			misses = 0
		}
		if s.now().After(deadline) {
			return GateDecision{}, "timeout"
		}
		select {
		case <-ctx.Done():
			return GateDecision{}, "aborted"
		case <-t.C:
		}
	}
}
