package api

import (
	"context"
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

	"github.com/ZacxDev/muster/internal/agentprovision"
	"github.com/ZacxDev/muster/internal/agents"
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
	s    *Server
	cs   *fake.Clientset
	offs []officers.Officer
}

// newInstanceReadRig declares readRigOfficers untiered officers of
// readRigSlots instances each, and gives instance n of officer i an agent row
// when row(i, n) and a live instance (namespace, RoleBinding, Deployment,
// written by the driver's own Create) when live(i, n).
func newInstanceReadRig(t *testing.T, row, live func(i, n int) bool) *instanceReadRig {
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
	id := int64(5000)
	for i, o := range list {
		for n := 1; n <= o.Slots(); n++ {
			name := o.InstanceName(n)
			if live(i, n) {
				if err := d.Create(context.Background(), provision.Spec{Ref: provision.Ref{Name: name},
					Runtime: provision.Runtime{Image: "ghcr.io/muster-example/agent:1"},
					Ports:   []provision.Port{{Name: provision.DefaultPortName, Port: 8421}}}); err != nil {
					t.Fatalf("create %s: %v", name, err)
				}
			}
			if row(i, n) {
				id++
				store.rows = append(store.rows, agents.Agent{ID: id, Name: name, DisplayName: "officer",
					Kind: agents.KindClaudeCode, Status: agents.StatusRunning, OwnerID: o.Owner, Namespace: readRigPrefix + name})
			}
		}
	}
	adapter, err := agentprovision.New(agentprovision.Config{Driver: d, Store: store})
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Agents: store, Provisioner: adapter, Gateway: &repromptChat{},
		SessionLiveness: stubLiveness{}, Kinds: stubKinds{pick: "acct-one"}})
	if _, err := s.EnableOfficers(OfficersConfig{Officers: list, Now: time.Now, InstallSession: discardSession}); err != nil {
		t.Fatalf("enable officers: %v", err)
	}
	cs.ClearActions()
	return &instanceReadRig{s: s, cs: cs, offs: list}
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
// it (officerInstancesWithin), and all 18 within one poll interval. That holds
// even at the OLD client budget, so the fix does not depend on the raised
// MUSTER_AGENT_KUBE_QPS default. Before it, one officer's read alone needed
// 163 requests (> 30s of tokens) and the 18 needed ~10 minutes.
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
