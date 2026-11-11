package api

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// THE USAGE COLLECTOR, BOUND TO THE SERVER (officer_usage.go): EnableOfficers
// builds it for every officer with no configuration, it reads an instance
// through the gateway BY ITS OWN AGENT ROW and reports the row's account, and
// officerFleet.Stop pushes once BEFORE the instance is scaled to zero —
// without ever failing the stop. The collector's own rules are tested in
// internal/officers/usage.
// ---------------------------------------------------------------------------

// usageGateway is the reprompt chat fake plus the gateway's usage read. It
// records, per read, the agent asked about and the instance's provisioner
// phase at that moment (so a read after the scale-down shows).
type usageGateway struct {
	repromptChat
	prov  *officerProv
	umu   sync.Mutex
	reads []string
	err   error
}

func (g *usageGateway) Usage(_ context.Context, a agents.Agent, _ time.Time) (cctranscript.UsagePage, error) {
	g.prov.mu.Lock()
	phase := g.prov.phase[a.Name]
	g.prov.mu.Unlock()
	g.umu.Lock()
	g.reads = append(g.reads, a.Name+"@"+string(phase))
	err := g.err
	g.umu.Unlock()
	if err != nil {
		return cctranscript.UsagePage{}, err
	}
	h := time.Date(2000, 1, 3, 11, 0, 0, 0, time.UTC)
	return cctranscript.UsagePage{Since: h, Buckets: []cctranscript.UsageBucket{{Session: "s-1", Hour: h, Turns: 1, APICalls: 2,
		InputTokens: 3, OutputTokens: 4, CacheReadTokens: 5, CacheCreationTokens: 6}}}, nil
}

func (g *usageGateway) readList() string {
	g.umu.Lock()
	defer g.umu.Unlock()
	return strings.Join(g.reads, ",")
}

func usageServer(t *testing.T) (*Server, *usageGateway, *officerProv, *hqtest.Server, officers.Officer, *bytes.Buffer) {
	t.Helper()
	hq := hqtest.New(map[string]string{offATok: offA})
	t.Cleanup(hq.Close)
	store := &officerAgents{}
	prov := &officerProv{store: store, phase: map[string]provision.Phase{}}
	gw := &usageGateway{prov: prov}
	logs := &bytes.Buffer{}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(logs, "", 0))
	s.UseExtensions(Extensions{Agents: store, Provisioner: prov, Gateway: gw, SessionLiveness: stubLiveness{}, Kinds: stubKinds{pick: "acct-one"}})
	o := officers.Officer{ID: offA, HQURL: hq.BaseURL(), MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: time.Minute, Owner: 1}.WithToken(offATok)
	now := time.Date(2000, 1, 3, 12, 20, 0, 0, time.UTC)
	if _, err := s.EnableOfficers(OfficersConfig{Officers: []officers.Officer{o}, InstallSession: discardSession, NoUsageRun: true,
		Now: func() time.Time { return now }}); err != nil {
		t.Fatal(err)
	}
	a := agents.Agent{ID: 901, Name: officers.InstanceName(o.ID, 1), Kind: agents.KindClaudeCode, Status: agents.StatusRunning, OwnerID: 1,
		Namespace: "ns", CCAccount: "acct-two"}
	store.rows = append(store.rows, a)
	prov.phase[a.Name] = provision.PhaseRunning
	return s, gw, prov, hq, o, logs
}

// TestEveryOfficerGetsAUsageCollectorThatReadsTheInstanceByItsOwnRow: no
// configuration, a collector; a tick reads the up instance through the
// gateway and pushes the contract body carrying the account its ROW is on.
func TestEveryOfficerGetsAUsageCollectorThatReadsTheInstanceByItsOwnRow(t *testing.T) {
	s, gw, _, hq, o, _ := usageServer(t)
	uc := s.officerRT.usage
	if uc == nil {
		t.Fatal("no usage collector for a declared officer")
	}
	uc.Tick(context.Background())
	name := officers.InstanceName(o.ID, 1)
	if got := gw.readList(); got != name+"@"+string(provision.PhaseRunning) {
		t.Fatalf("gateway reads %q, want the instance's own row once", got)
	}
	puts := hq.UsagePuts()
	want := `{"instance":"` + name + `","pool":"","account":"acct-two","generated_at":"2000-01-03T12:20:00Z","buckets":[` +
		`{"session":"s-1","hour":"2000-01-03T11:00:00Z","turns":1,"api_calls":2,"input_tokens":3,"output_tokens":4,"cache_read_tokens":5,"cache_creation_tokens":6}]}`
	if len(puts) != 1 || puts[0].Officer != offA || string(puts[0].Raw) != want {
		t.Fatalf("PUTs %+v\nwant one: %s", puts, want)
	}
}

// TestStopPushesTheInstancesUsageBeforeScalingItToZero: the flush reads the
// instance while it is still running, then the stop happens.
func TestStopPushesTheInstancesUsageBeforeScalingItToZero(t *testing.T) {
	s, gw, prov, hq, o, _ := usageServer(t)
	if err := (officerFleet{s: s}).Stop(context.Background(), o, 1); err != nil {
		t.Fatal(err)
	}
	name := officers.InstanceName(o.ID, 1)
	if got := gw.readList(); got != name+"@"+string(provision.PhaseRunning) {
		t.Fatalf("reads %q, want one read while the instance was still running", got)
	}
	if n := len(hq.UsagePuts()); n != 1 {
		t.Fatalf("%d usage pushes on Stop, want 1", n)
	}
	prov.mu.Lock()
	stops, phase := len(prov.stops), prov.phase[name]
	prov.mu.Unlock()
	if stops != 1 || phase != provision.PhaseStopped {
		t.Fatalf("stops %d, phase %s: the instance was not scaled to zero", stops, phase)
	}
}

// TestAFailingFlushDoesNotFailTheStop: a read that fails, and an HQ that is
// down — the stop still happens and returns nil.
func TestAFailingFlushDoesNotFailTheStop(t *testing.T) {
	for _, c := range []struct {
		name   string
		readOK bool
	}{{"read fails", false}, {"HQ down", true}} {
		t.Run(c.name, func(t *testing.T) {
			s, gw, prov, hq, o, _ := usageServer(t)
			if !c.readOK {
				gw.err = errors.New("agentgateway: its runtime answered 503")
			} else {
				hq.SetDown(true)
			}
			if err := (officerFleet{s: s}).Stop(context.Background(), o, 1); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			prov.mu.Lock()
			stops := len(prov.stops)
			prov.mu.Unlock()
			if stops != 1 || len(hq.UsagePuts()) != 0 {
				t.Fatalf("stops %d, accepted pushes %d: want the stop and no push", stops, len(hq.UsagePuts()))
			}
		})
	}
}

// TestAnOlderCcdIsUnsupportedNotAFailure: the gateway's
// ErrUsageUnsupported is the collector's quiet skip.
func TestAnOlderCcdIsUnsupportedNotAFailure(t *testing.T) {
	s, gw, _, hq, _, logs := usageServer(t)
	gw.err = agentgateway.ErrUsageUnsupported
	s.officerRT.usage.Tick(context.Background())
	if n := len(hq.Requests()); n != 0 {
		t.Fatalf("%d HQ requests for an unsupported instance", n)
	}
	if !strings.Contains(logs.String(), "does not serve its Claude usage") || strings.Contains(logs.String(), "could not be read") {
		t.Fatalf("log:\n%s", logs)
	}
}

// TestAnInstanceOnTheLLMProxyReportsNoAccount: a pool on the proxy runs on
// no Claude account whatever its row carries; every other instance reports
// its row's.
func TestAnInstanceOnTheLLMProxyReportsNoAccount(t *testing.T) {
	o := officers.Officer{ID: offA, Tiers: &officers.Tiers{Routine: officers.TierPool{MaxInstances: 1}, Judgement: officers.TierPool{MaxInstances: 1}},
		LLM: &officers.LLM{DailyBudgetUSD: 1}}
	routine, judgement := o.Pool(officers.TierRoutine), o.Pool(officers.TierJudgement)
	if got := instanceUsageAccount(routine, agents.Agent{Name: routine.InstanceName(1), CCAccount: "acct-two"}); got != "" {
		t.Fatalf("a routine instance on the proxy reports account %q, want none", got)
	}
	if got := instanceUsageAccount(judgement, agents.Agent{Name: judgement.InstanceName(1), CCAccount: "acct-two"}); got != "acct-two" {
		t.Fatalf("a judgement instance on Claude reports account %q, want its row's", got)
	}
}
