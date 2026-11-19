package agentprovision

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// getDriver is provision.Noop whose Get answers err for the names in errs and
// counts every Get and List.
type getDriver struct {
	*provision.Noop
	errs        map[string]error
	gets, lists int
}

func (d *getDriver) Get(ctx context.Context, ref provision.Ref) (provision.Instance, error) {
	d.gets++
	if err, ok := d.errs[ref.Name]; ok {
		return provision.Instance{}, err
	}
	return d.Noop.Get(ctx, ref)
}

func (d *getDriver) List(ctx context.Context) ([]provision.Instance, error) {
	d.lists++
	return d.Noop.List(ctx)
}

// TestInstancesOfAnswersWhatListWouldForTheAgentsAsked: InstancesOf reads only
// the agents asked about (one Get each, no List), and answers about each what
// List would have: a live instance is reported; ErrNotFound — including a
// stranger's co-named object, which a by-name read reports as ErrNotFound AND
// ErrNotManaged — is absent; ErrNotManaged or ErrDivergentSpec alone (a
// RoleBinding muster refuses) is the instance in PhaseUnknown, naming the
// refusal; and any other failure (ErrBlind) fails the whole read rather than
// answering a partial list.
func TestInstancesOfAnswersWhatListWouldForTheAgentsAsked(t *testing.T) {
	ctx := context.Background()
	noop := provision.MustNewNoop()
	for _, name := range []string{"live-a", "other-b"} {
		if err := noop.Create(ctx, provision.Spec{Ref: provision.Ref{Name: name},
			Runtime: provision.Runtime{Image: "ghcr.io/muster-example/agent:1"}}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	d := &getDriver{Noop: noop, errs: map[string]error{
		"stranger-c": fmt.Errorf("%w: %w: deployment held by another app", provision.ErrNotFound, provision.ErrNotManaged),
		"refused-d":  fmt.Errorf("apply rolebinding: %w", provision.ErrNotManaged),
		"diverged-e": fmt.Errorf("%w: rolebinding binds another role", provision.ErrDivergentSpec),
	}}
	a, err := New(Config{Driver: d, Store: &recordingStore{}})
	if err != nil {
		t.Fatal(err)
	}
	ask := func(names ...string) []agents.Agent {
		var out []agents.Agent
		for i, n := range names {
			out = append(out, agents.Agent{ID: int64(i + 1), Name: n})
		}
		return out
	}

	got, err := a.InstancesOf(ctx, ask("live-a", "gone-f", "stranger-c", "refused-d", "diverged-e"))
	if err != nil {
		t.Fatalf("InstancesOf: %v", err)
	}
	var summary []string
	for _, in := range got {
		s := in.Ref.Name + "=" + string(in.Phase)
		if in.Phase == provision.PhaseUnknown && !strings.HasPrefix(in.Reason, "Refused: ") {
			t.Errorf("%s is unknown with reason %q; want the refusal named", in.Ref.Name, in.Reason)
		}
		summary = append(summary, s)
	}
	want := "live-a=" + string(provision.PhaseRunning) + " refused-d=" + string(provision.PhaseUnknown) +
		" diverged-e=" + string(provision.PhaseUnknown)
	if strings.Join(summary, " ") != want {
		t.Fatalf("InstancesOf answered %q; want %q (gone-f and the stranger's stranger-c absent; other-b, not asked, absent)",
			strings.Join(summary, " "), want)
	}
	if d.gets != 5 || d.lists != 0 {
		t.Fatalf("InstancesOf of 5 agents made %d Gets and %d Lists; want 5 and 0", d.gets, d.lists)
	}

	d.errs["blind-g"] = fmt.Errorf("%w: list pods: rate: Wait(n=1) would exceed context deadline", provision.ErrBlind)
	got, err = a.InstancesOf(ctx, ask("live-a", "blind-g"))
	if !errors.Is(err, provision.ErrBlind) || got != nil {
		t.Fatalf("InstancesOf over an unreadable agent answered %v, %v; want no instances and ErrBlind", got, err)
	}
}
