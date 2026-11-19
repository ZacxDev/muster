package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/util/flowcontrol"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agentkickoff"
	"github.com/ZacxDev/muster/internal/agentprovision"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentstatus"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
	"github.com/ZacxDev/muster/internal/provision"
	k8sdriver "github.com/ZacxDev/muster/internal/provision/k8s"
)

// THE OFFICER LOOP'S INSTANCE READ, OVER THE REAL KUBERNETES DRIVER.
//
// 🔴 THE FAKE HERE IS THE CLUSTER, NOT THE PROVISIONER. The officer rigs
// elsewhere in this package use officerProv, whose Instances costs nothing,
// so no test could see what one officer pass cost the cluster. A deployment
// with 18 officers on the least-privilege RBAC layout read EVERY agent
// namespace on every officer's pass (three requests each), ran client-go's
// default request budget (5/s, burst 10) dry, and every pass failed with
// "rate: Wait(n=1) would exceed context deadline". These tests build the
// production stack below the officer runtime — agentprovision.Adapter over
// the k8s driver in binding mode — over a fake clientset, and count or
// rate-limit the requests it actually sends.

const (
	readRigPrefix    = "muster-agent-"
	readRigOfficers  = 18
	readRigSlots     = 3
	readRigTokenBase = "fixture-officer-token-read-"
)

// readRigOfficerID is officer i's id.
func readRigOfficerID(i int) string { return fmt.Sprintf("release:fleet-%02d", i) }

type instanceReadRig struct {
	s       *Server
	cs      *fake.Clientset
	offs    []officers.Officer
	adapter *agentprovision.Adapter
	store   *officerAgents
}

// readRigOpts adjusts newInstanceReadRigWith's rig: extra agents (each with a
// live instance) besides the officers', each officer row through edit, and the
// agents store the server reads through wrap.
type readRigOpts struct {
	extra []agents.Agent
	edit  func(*agents.Agent)
	wrap  func(*officerAgents) agents.Store
}

// newInstanceReadRig declares readRigOfficers untiered officers of
// readRigSlots instances each, and gives instance n of officer i an agent row
// when row(i, n) and a live instance (namespace, RoleBinding, Deployment,
// written by the driver's own Create) when live(i, n).
func newInstanceReadRig(t *testing.T, row, live func(i, n int) bool) *instanceReadRig {
	t.Helper()
	return newInstanceReadRigWith(t, row, live, readRigOpts{})
}

func newInstanceReadRigWith(t *testing.T, row, live func(i, n int) bool, opts readRigOpts) *instanceReadRig {
	t.Helper()
	tokens := map[string]string{}
	env := map[string]string{}
	var raw []string
	for i := 0; i < readRigOfficers; i++ {
		id, tok := readRigOfficerID(i), fmt.Sprintf("%s%02d", readRigTokenBase, i)
		tokens[tok] = id
		env[officers.TokenEnvPrefix+officers.TokenEnvSuffix(id)] = tok
		raw = append(raw, `{"id":"`+id+`","hq_url":"HQ","max_instances":`+fmt.Sprint(readRigSlots)+`}`)
	}
	hq := hqtest.New(tokens)
	t.Cleanup(hq.Close)
	list, err := officers.Parse(strings.ReplaceAll("["+strings.Join(raw, ",")+"]", `"HQ"`, `"`+hq.BaseURL()+`"`),
		func(k string) string { return env[k] }, readRigPrefix)
	if err != nil {
		t.Fatalf("parse the officers: %v", err)
	}

	cs := fake.NewClientset()
	d, err := k8sdriver.New(k8sdriver.Config{Client: cs, NamespacePerInstance: true, NamespacePrefix: readRigPrefix,
		NamespaceRoleBinding: &k8sdriver.NamespaceRoleBindingConfig{ClusterRole: "muster-agent-namespace",
			ServiceAccountNamespace: "muster-system", ServiceAccountName: "muster"}})
	if err != nil {
		t.Fatalf("k8s driver: %v", err)
	}
	store := &officerAgents{}
	create := func(name string) {
		t.Helper()
		if err := d.Create(context.Background(), provision.Spec{Ref: provision.Ref{Name: name},
			Runtime: provision.Runtime{Image: "ghcr.io/muster-example/agent:1"},
			Ports:   []provision.Port{{Name: provision.DefaultPortName, Port: 8421}}}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	for _, a := range opts.extra {
		create(a.Name)
		store.rows = append(store.rows, a)
	}
	id := int64(5000)
	for i, o := range list {
		for n := 1; n <= o.Slots(); n++ {
			name := o.InstanceName(n)
			if live(i, n) {
				create(name)
			}
			if row(i, n) {
				id++
				a := agents.Agent{ID: id, Name: name, DisplayName: "officer",
					Kind: agents.KindClaudeCode, Status: agents.StatusRunning, OwnerID: o.Owner, Namespace: readRigPrefix + name}
				if opts.edit != nil {
					opts.edit(&a)
				}
				store.rows = append(store.rows, a)
			}
		}
	}
	adapter, err := agentprovision.New(agentprovision.Config{Driver: d, Store: store})
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	var as agents.Store = store
	if opts.wrap != nil {
		as = opts.wrap(store)
	}
	s.UseExtensions(Extensions{Agents: as, Provisioner: adapter, Gateway: &repromptChat{},
		SessionLiveness: stubLiveness{}, Kinds: stubKinds{pick: "acct-one"}})
	if _, err := s.EnableOfficers(OfficersConfig{Officers: list, Now: time.Now, InstallSession: discardSession}); err != nil {
		t.Fatalf("enable officers: %v", err)
	}
	cs.ClearActions()
	return &instanceReadRig{s: s, cs: cs, offs: list, adapter: adapter, store: store}
}

func everySlot(int, int) bool { return true }

// (a) TestAnOfficersInstanceReadCostsItsOwnInstancesNotTheFleet: one
// officer's instance read, in a cluster holding every other officer's 3
// instances (51 agent namespaces besides its own), sends requests about ITS
// OWN agents only. Its slot 1 has an agent and an instance, slot 2 an agent
// row whose instance is gone, slot 3 nothing at all.
//
// The cost pinned: one LIST (slot 1's pods) and at most five requests in all
// (slot 1: namespace, RoleBinding, Deployment, pods; slot 2: its namespace,
// which is absent). A slot with no agent row costs nothing. Before the fix the
// same read listed every agent namespace in the cluster: 157 requests, 105 of
// them LISTs.
func TestAnOfficersInstanceReadCostsItsOwnInstancesNotTheFleet(t *testing.T) {
	r := newInstanceReadRig(t,
		func(i, n int) bool { return i != 0 || n <= 2 },
		func(i, n int) bool { return i != 0 || n == 1 })
	o := r.offs[0]
	insts, err := (officerFleet{s: r.s}).Instances(context.Background(), o)
	if err != nil {
		t.Fatalf("officer %s's instance read: %v", o.ID, err)
	}
	if len(insts) != 3 || !insts[0].Exists || !insts[0].Up || !insts[1].Exists || insts[1].Up || insts[2].Exists {
		t.Fatalf("officer %s's instances %+v: want 1 existing and up, 2 existing and down (its instance is gone), 3 absent", o.ID, insts)
	}

	own := map[string]bool{readRigPrefix + o.InstanceName(1): true, readRigPrefix + o.InstanceName(2): true}
	var lists, total int
	var foreign []string
	for _, a := range r.cs.Actions() {
		total++
		if a.GetVerb() == "list" {
			lists++
		}
		ns := a.GetNamespace()
		if a.GetResource().Resource == "namespaces" {
			if g, ok := a.(k8stesting.GetAction); ok {
				ns = g.GetName()
			}
		}
		if !own[ns] {
			foreign = append(foreign, a.GetVerb()+" "+a.GetResource().Resource+" "+ns)
		}
	}
	if lists > 1 || total > 5 {
		t.Fatalf("one officer's instance read (1 live instance, 1 gone, 1 empty slot) sent %d requests, %d of them LISTs; "+
			"want at most 5 and at most 1 LIST: an officer's pass must cost its own instances, not every agent namespace in the cluster",
			total, lists)
	}
	if len(foreign) != 0 {
		t.Fatalf("one officer's instance read touched what is not its own agents': %v", foreign)
	}
}

// virtualClock is flowcontrol.Clock in virtual time: Sleep advances it at
// once, so a rate limiter over it costs the test no wall time.
type virtualClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *virtualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *virtualClock) Since(t time.Time) time.Duration { return c.Now().Sub(t) }

func (c *virtualClock) Sleep(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// (b) TestEighteenOfficersReadTheirInstancesWithinTheOldClientBudget is the
// seam: 18 officers of 3 instances each (54 agent namespaces), their instance
// reads run 4 at a time (the officer loop's concurrent passes), and every
// request the driver sends first takes a token from client-go's own token
// bucket at client-go's DEFAULT rate (rest.DefaultQPS 5, rest.DefaultBurst 10)
// — on a virtual clock, so the time a read waits for tokens is measured, not
// slept.
//
// Every officer's read must answer within the bound the officer runtime gives
// it (officerInstancesWithin), and all 18 within one poll interval, with the
// officer reads as the ONLY traffic on the old 5/10 budget. Before the fix
// one officer's read alone needed 163 requests (> 30s of tokens) and the 18
// needed ~10 minutes. What it does not show: that the rest of the process's
// readers fit beside them on that budget — see
// TestOfficerReadsAndTheTenSecondLoopsShareTheOldClientBudget for the timed
// loops, and the raised MUSTER_AGENT_KUBE_QPS default for everything else.
func TestEighteenOfficersReadTheirInstancesWithinTheOldClientBudget(t *testing.T) {
	r := newInstanceReadRig(t, everySlot, everySlot)
	clk := &virtualClock{now: time.Unix(1_700_000_000, 0)}
	lim := flowcontrol.NewTokenBucketRateLimiterWithClock(5, 10, clk)
	var limMu sync.Mutex
	requests := 0
	r.cs.PrependReactor("*", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		// Serialised: the bucket hands out tokens in one order, and the
		// virtual clock moves only here.
		limMu.Lock()
		lim.Accept()
		requests++
		limMu.Unlock()
		return false, nil, nil
	})

	type read struct {
		id    string
		took  time.Duration
		err   error
		up    int
		slots int
	}
	reads := make([]read, len(r.offs))
	start := clk.Now()
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, o := range r.offs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			t0 := clk.Now()
			insts, err := (officerFleet{s: r.s}).Instances(context.Background(), o)
			rd := read{id: o.ID, took: clk.Now().Sub(t0), err: err, slots: len(insts)}
			for _, in := range insts {
				if in.Up {
					rd.up++
				}
			}
			reads[i] = rd
		}()
	}
	wg.Wait()
	all := clk.Now().Sub(start)

	for _, rd := range reads {
		if rd.err != nil || rd.slots != readRigSlots || rd.up != readRigSlots {
			t.Errorf("officer %s: read %d slots, %d up, err %v; want all %d up", rd.id, rd.slots, rd.up, rd.err, readRigSlots)
		}
		if rd.took > officerInstancesWithin {
			t.Errorf("officer %s's instance read took %s of client-go's default request budget (5/s, burst 10), "+
				"past the %s the officer runtime waits for it: in production it fails as "+
				"\"rate: Wait(n=1) would exceed context deadline\"", rd.id, rd.took, officerInstancesWithin)
		}
	}
	if all > officers.DefaultPollInterval {
		t.Errorf("18 officers' instance reads, 4 at a time, took %s of client-go's default request budget (%d requests); "+
			"want every officer's read inside one poll interval (%s)", all, requests, officers.DefaultPollInterval)
	}
	t.Logf("%d requests, %s of token time for 18 officers' reads", requests, all)
}

// kickoffNoGateway is a kickoff gateway that is never expected to be reached:
// every method fails (no instance in these rigs becomes ready).
type kickoffNoGateway struct{}

var errKickoffNoGateway = errors.New("kickoffNoGateway: no first turn is deliverable in this rig")

func (kickoffNoGateway) Resolve(context.Context, agents.Agent) (agentgateway.Target, error) {
	return agentgateway.Target{}, errKickoffNoGateway
}
func (kickoffNoGateway) Send(context.Context, agentgateway.Target, string, string, func(string)) (string, error) {
	return "", errKickoffNoGateway
}
func (kickoffNoGateway) SendWithTools(context.Context, agentgateway.Target, string, string, string, []agents.ToolDef,
	agents.ToolDispatch, agents.StreamEmit) (string, error) {
	return "", errKickoffNoGateway
}
func (kickoffNoGateway) Checkout(context.Context, agentgateway.Target) (agents.CheckoutStatus, error) {
	return agents.CheckoutStatus{}, errKickoffNoGateway
}

// TestOfficerReadsAndTheTenSecondLoopsShareTheOldClientBudget: the seam the
// officer-only test leaves out. The officer reads share the Kubernetes
// client's ONE limiter with the 10-second background loops — the first-turn
// deliverer (agentkickoff) and the provisioning→running promoter
// (agentstatus) — which read instances on every tick while anything is owed
// or provisioning. Here one officer instance is starting (it owes its first
// turn and is provisioning), the 18 officers' reads run 4 at a time, and the
// two loops tick 6 times each (one poll interval of 10-second ticks, back to
// back) — all on client-go's token bucket at its OLD default (5/s, burst 10)
// on a virtual clock. The whole interval's work must fit in one poll interval
// of tokens.
//
// Before the loops read only the agents they act on, each tick read the
// whole fleet (163 requests here): 2172 requests, over 7 minutes of tokens.
// What it does NOT claim: with MANY instances starting at once (a cold start
// of every officer), the loops' per-instance reads still exceed 5/s; that
// case is carried by the raised MUSTER_AGENT_KUBE_QPS default, not by this.
func TestOfficerReadsAndTheTenSecondLoopsShareTheOldClientBudget(t *testing.T) {
	now := time.Now()
	starting := officers.InstanceName(readRigOfficerID(0), 1)
	r := newInstanceReadRigWith(t, everySlot, everySlot, readRigOpts{edit: func(a *agents.Agent) {
		if a.Name == starting {
			a.Status, a.PendingNote, a.KickedOff, a.UpdatedAt = agents.StatusProvisioning, "your first turn", false, now
		} else {
			a.KickedOff = true
		}
	}})
	deliverer, err := agentkickoff.New(agentkickoff.Config{Store: r.store, Instances: r.adapter, Gateway: kickoffNoGateway{},
		NamespacePrefix: readRigPrefix, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatalf("kickoff deliverer: %v", err)
	}
	promoter, err := agentstatus.New(agentstatus.Config{Store: r.store, Instances: r.adapter, NamespacePrefix: readRigPrefix})
	if err != nil {
		t.Fatalf("status promoter: %v", err)
	}

	clk := &virtualClock{now: time.Unix(1_700_000_000, 0)}
	lim := flowcontrol.NewTokenBucketRateLimiterWithClock(5, 10, clk)
	var limMu sync.Mutex
	requests := 0
	r.cs.PrependReactor("*", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		limMu.Lock()
		lim.Accept()
		requests++
		limMu.Unlock()
		return false, nil, nil
	})

	start := clk.Now()
	var wg sync.WaitGroup
	var loopErrs []error
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 6; i++ {
			if err := deliverer.Tick(context.Background()); err != nil {
				loopErrs = append(loopErrs, fmt.Errorf("deliverer tick %d: %w", i, err))
			}
			if _, err := promoter.Tick(context.Background()); err != nil {
				loopErrs = append(loopErrs, fmt.Errorf("promoter tick %d: %w", i, err))
			}
		}
		deliverer.Wait()
	}()
	readErrs := make([]error, len(r.offs))
	sem := make(chan struct{}, 4)
	for i, o := range r.offs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			insts, err := (officerFleet{s: r.s}).Instances(context.Background(), o)
			if err == nil && len(insts) != readRigSlots {
				err = fmt.Errorf("%d slots, want %d", len(insts), readRigSlots)
			}
			readErrs[i] = err
		}()
	}
	wg.Wait()
	all := clk.Now().Sub(start)

	for i, err := range readErrs {
		if err != nil {
			t.Errorf("officer %s's instance read: %v", r.offs[i].ID, err)
		}
	}
	for _, err := range loopErrs {
		t.Errorf("%v", err)
	}
	if all > officers.DefaultPollInterval {
		t.Errorf("one poll interval's work — 18 officers' instance reads (4 at a time) and 6 ticks each of the first-turn "+
			"deliverer and the status promoter, with 1 instance starting — took %s of client-go's default request budget "+
			"(5/s, burst 10; %d requests); want it inside one poll interval (%s)", all, requests, officers.DefaultPollInterval)
	}
	t.Logf("%d requests, %s of token time", requests, all)
}

// TestTheTimedReadersReadOnlyTheAgentsTheyActOn: the stopped-agent reaper
// (one agent due) and the consumer-usage collector (one eligible agent) each
// send requests about that one agent only, in a cluster of 55 agent
// namespaces — at most 4 requests (namespace, RoleBinding, Deployment, pods),
// where a whole-fleet read sends 166.
func TestTheTimedReadersReadOnlyTheAgentsTheyActOn(t *testing.T) {
	const worker = "brave-otter"
	var reap *reapOnlyStore
	r := newInstanceReadRigWith(t, everySlot, everySlot, readRigOpts{
		extra: []agents.Agent{{ID: 4001, Name: worker, DisplayName: "worker", Kind: agents.KindClaudeCode,
			Status: agents.StatusRunning, OwnerID: authz.OperatorUserID, CCAccount: "acct-one", Namespace: readRigPrefix + worker}},
		wrap: func(st *officerAgents) agents.Store {
			reap = &reapOnlyStore{officerAgents: st, due: []agents.StoppedAgent{{ID: 4001, Name: worker, OwnerID: authz.OperatorUserID}}}
			return reap
		}})
	count := func(what string) {
		t.Helper()
		own := readRigPrefix + worker
		var total int
		var foreign []string
		for _, a := range r.cs.Actions() {
			total++
			ns := a.GetNamespace()
			if g, ok := a.(k8stesting.GetAction); ok && a.GetResource().Resource == "namespaces" {
				ns = g.GetName()
			}
			if ns != own {
				foreign = append(foreign, a.GetVerb()+" "+a.GetResource().Resource+" "+ns)
			}
		}
		if total > 4 || len(foreign) != 0 {
			t.Errorf("%s sent %d requests (want at most 4), touching %d outside the one agent it acts on: %v",
				what, total, len(foreign), foreign)
		}
		r.cs.ClearActions()
	}

	r.s.SetAgentReapStoppedAfter(time.Hour)
	if n := r.s.reapStoppedAgents(context.Background()); n != 0 {
		t.Fatalf("the reaper started %d teardowns of an agent whose instance is up; want 0", n)
	}
	if reap.claims != 0 {
		t.Fatalf("the reaper claimed an agent whose instance is up")
	}
	count("the stopped-agent reaper's pass (1 agent due)")

	names, err := r.s.consumerUsageInstances(context.Background())
	if err != nil || fmt.Sprint(names) != "["+worker+"]" {
		t.Fatalf("consumer-usage instances %v, %v; want [%s]", names, err, worker)
	}
	count("the consumer-usage collector's read (1 eligible agent)")
}

// reapOnlyStore is the rig's agents store with the reaper's half: due is the
// due list, and every claim is refused (and counted), so nothing is destroyed.
type reapOnlyStore struct {
	*officerAgents
	due    []agents.StoppedAgent
	claims int
}

func (s *reapOnlyStore) ListReapableStopped(context.Context, time.Duration) ([]agents.StoppedAgent, error) {
	return s.due, nil
}

func (s *reapOnlyStore) ClaimStoppedReap(context.Context, int64, time.Duration) (bool, error) {
	s.claims++
	return false, nil
}
