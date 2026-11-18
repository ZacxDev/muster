package agentprovision

import (
	"context"
	"errors"
	"fmt"

	"github.com/ZacxDev/muster/internal/agents"
)

// AccountRebinder is the agents store's conditional account move
// (agents.PGStore.RebindCCAccount), asserted rather than added to
// agents.Store: the account failover is its only caller.
type AccountRebinder interface {
	RebindCCAccount(ctx context.Context, id int64, from, to string) (bool, error)
}

var _ AccountRebinder = (*agents.PGStore)(nil)

// ErrNoAccountRebinder is Rebind's refusal when the agents store cannot move
// an agent's account (a store without RebindCCAccount): fail closed, nothing
// is re-applied.
var ErrNoAccountRebinder = errors.New("agentprovision: the agents store cannot move an agent to another Claude account")

// RebindResult is what [Adapter.Rebind] did.
type RebindResult struct {
	// Rebound: this call moved the agent from its `from` account to `to` and
	// re-applied its spec (Err may still report the apply failing).
	Rebound bool
	// Current is the account the agent is on after the call.
	Current string
	// PrevInstance is the backend's id of the instance that was running before
	// the re-apply ("" when none was seen), so a caller can wait for its
	// replacement (ccfailover.Service.AwaitRebound).
	PrevInstance string
	// Skipped says why nothing was moved: the agent is not on `from` any more
	// (another failover moved it), or it is not live (stopped, in error, gone).
	Skipped string
}

// Rebind is an ACCOUNT FAILOVER's lifecycle half (internal/ccfailover): move
// claude-code agent agentID from Claude account from to account to, then
// re-apply its spec, so its pod restarts with to's setup-token as
// CLAUDE_CODE_OAUTH_TOKEN. The agent's name, namespace, volume and identity are
// kept: only the per-agent Secret's token changes, and since the spec
// fingerprint covers the Secret's values the Deployment rolls (a persistent
// workspace rolls with Recreate, so the running CLI session restarts).
//
// prepare, when non-nil, runs after the move and before the re-apply (the
// officer runtime re-arms an instance's first turn there); its error is logged
// and does not stop the rebind.
//
// 🔴 IT RUNS UNDER THE SAME PER-AGENT LANE AS Start, Stop AND Destroy
// (lockAgent), AND RE-READS THE ROW INSIDE IT. So a Stop or Destroy that wins
// the lane first leaves a row that is not live (or none), and the rebind does
// nothing; one that arrives after waits for the re-apply and then stops or
// deletes the restarted pod. A rebind therefore never brings back a stopped
// agent nor re-creates a deleted one. The lane is this process's; across
// processes the conditional move (RebindCCAccount: WHERE cc_account = from)
// still makes the move itself happen once.
func (a *Adapter) Rebind(ctx context.Context, agentID int64, from, to string, prepare func(context.Context, agents.Agent) error) (RebindResult, error) {
	ctx, cancel := a.opCtx(ctx)
	defer cancel()
	defer a.lockAgent(agentID)()

	ag, err := a.store.Get(ctx, agentID)
	if err != nil {
		return RebindResult{Skipped: "the agent could not be loaded"}, fmt.Errorf("agentprovision: rebind agent %d: load row: %w", agentID, err)
	}
	if ag.CCAccount != from {
		return RebindResult{Current: ag.CCAccount, Skipped: fmt.Sprintf("the agent is on account %q, not %q", ag.CCAccount, from)}, nil
	}
	switch ag.Status {
	case agents.StatusRunning, agents.StatusProvisioning, agents.StatusPending:
	default:
		return RebindResult{Current: from, Skipped: fmt.Sprintf("the agent is %s, not live", ag.Status)}, nil
	}
	rb, ok := a.store.(AccountRebinder)
	if !ok {
		return RebindResult{Current: from, Skipped: "no account rebinder"}, ErrNoAccountRebinder
	}
	prev := a.instanceID(ctx, ag)
	moved, err := rb.RebindCCAccount(ctx, ag.ID, from, to)
	if err != nil {
		return RebindResult{Current: from}, fmt.Errorf("agentprovision: rebind agent %d (%s) from %q to %q: %w", ag.ID, ag.Name, from, to, err)
	}
	if !moved {
		cur := from
		if again, gerr := a.store.Get(ctx, agentID); gerr == nil {
			cur = again.CCAccount
		}
		return RebindResult{Current: cur, Skipped: "another failover moved it first"}, nil
	}
	res := RebindResult{Rebound: true, Current: to, PrevInstance: prev}
	ag.CCAccount = to
	if prepare != nil {
		if err := prepare(ctx, ag); err != nil {
			a.log.Printf("agentprovision: rebind %s: prepare: %v", ag.Name, err)
		}
	}
	// Re-read: prepare may have changed the kickoff state the status below
	// depends on.
	if again, err := a.store.Get(ctx, agentID); err == nil {
		ag = again
	}
	if ag, err = a.ensureHooksToken(ctx, ag); err != nil {
		return res, err
	}
	spec, err := a.buildSpec(ctx, ag)
	if err != nil {
		return res, a.fail(ctx, ag, "build spec after an account failover", err)
	}
	if err := a.driver.Update(ctx, spec); err != nil {
		return res, a.fail(ctx, ag, "apply instance after an account failover", err)
	}
	if err := a.setStatus(ctx, ag.ID, agents.StatusProvisioning); err != nil {
		return res, err
	}
	if !ag.KickedOff {
		return res, nil
	}
	return res, a.setStatus(ctx, ag.ID, agents.StatusRunning)
}

// instanceID is the backend's id of ag's running instance, "" when none is
// seen or the backend cannot be read (the caller then cannot tell the old pod
// from its replacement, and waits for readiness alone).
func (a *Adapter) instanceID(ctx context.Context, ag agents.Agent) string {
	list, err := a.driver.List(ctx)
	if err != nil {
		return ""
	}
	for _, in := range list {
		if in.Ref.Name == ag.Name {
			return in.InstanceID
		}
	}
	return ""
}
