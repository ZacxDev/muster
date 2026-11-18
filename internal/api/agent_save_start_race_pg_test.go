package api

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentprovision"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

// gatedSaveProvisioner is the REAL agentprovision.Adapter (noop driver, real
// Postgres store), except that a Dispatch with no kickoff — the background step a
// save used to start — waits for release before it runs. That pins the
// interleaving the race needs: the save's write lands AFTER the Start, every run,
// instead of whenever the scheduler happens to put it there.
type gatedSaveProvisioner struct {
	*agentprovision.Adapter
	release chan struct{}
	done    chan struct{} // closed when a kickoff=false Dispatch has returned
}

func (p *gatedSaveProvisioner) Dispatch(ctx context.Context, id int64, kickoff bool) error {
	if kickoff {
		return p.Adapter.Dispatch(ctx, id, kickoff)
	}
	<-p.release
	defer close(p.done)
	return p.Adapter.Dispatch(ctx, id, kickoff)
}

// TestAStartRightAfterASaveIsNotOverwrittenBackToStopped: a save ("save for
// later": no kickoff, no Start) is inserted `stopped` already, so a background
// Dispatch(kickoff=false) after it only rewrote `stopped` — unconditionally
// (the adapter's setStatus). A Start issued between the 201 and that goroutine
// running was then overwritten back to `stopped` over an instance the Start had
// just brought up. The create no longer starts that goroutine for a save.
//
// The real adapter writes both statuses, so what is asserted is what production
// stores. The race is forced: the save's Dispatch is held until after the Start.
//
// ⚠ THE GREEN CASE IS AN ABSENCE, SO IT IS BOUNDED BY A WAIT. With the fix there
// is no Dispatch to wait for; the test gives a stray one saveDispatchGrace to
// show up (and land, since it is released) before reading the row.
func TestAStartRightAfterASaveIsNotOverwrittenBackToStopped(t *testing.T) {
	const saveDispatchGrace = 2 * time.Second
	prov := &gatedSaveProvisioner{release: make(chan struct{}), done: make(chan struct{})}
	f := newTaskRouteFixture(t, false, false, func(ext *Extensions) { ext.Kinds, ext.Provisioner = stubKinds{}, prov })
	store := agents.NewPG(f.pool)
	a, err := agentprovision.New(agentprovision.Config{Driver: provision.MustNewNoop(), Store: store,
		Spec:               agentspec.Config{ImageRepo: "registry.example.test/muster/agent-runtime", APIBaseURL: "http://muster.example.test:8105"},
		KickoffDeliverable: true})
	if err != nil {
		t.Fatal(err)
	}
	prov.Adapter = a

	rec, err := f.srv.createAndDispatchAgent(f.asOwner, dispatchParams{
		Kind: agents.KindGateway, NoteText: "saved for later", Kickoff: false,
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if rec.Status != agents.StatusStopped {
		t.Fatalf("fixture: the save was inserted %q; want %q", rec.Status, agents.StatusStopped)
	}

	// The operator starts it before the save's background step has run.
	if err := a.Start(f.asOwner, rec.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Positive control: the Start really did move the row out of `stopped`, or
	// the assertion below could not tell an overwrite from a Start that wrote
	// nothing.
	got, err := store.Get(f.asOwner, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agents.StatusProvisioning {
		t.Fatalf("control: right after Start the row reads %q; want %q", got.Status, agents.StatusProvisioning)
	}

	close(prov.release)
	select {
	case <-prov.done:
	case <-time.After(saveDispatchGrace):
	}
	got, err = store.Get(f.asOwner, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agents.StatusProvisioning {
		t.Errorf("after the save's background step the started agent reads %q; want %q — "+
			"the save's Dispatch(kickoff=false) overwrote the Start", got.Status, agents.StatusProvisioning)
	}
}
