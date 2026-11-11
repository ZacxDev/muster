package api

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ccfailover"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/consumerusage"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// THE CONSUMER USAGE COLLECTOR, BOUND TO THE SERVER (consumer_usage.go): which
// instances it reads (the operator's up non-officer claude-code agents), each
// read by its own row and attributed by its failover history, the officer
// instances never pushed here (their own collector pushes them), and the
// pre-stop push installed on the lifecycle adapter. The collector's own rules
// are tested in internal/consumerusage.
// ---------------------------------------------------------------------------

// consumerGateway answers each agent's usage by name and records the reads.
type consumerGateway struct {
	repromptChat
	pages map[string]cctranscript.UsagePage
	reads []string
}

func (g *consumerGateway) Usage(_ context.Context, a agents.Agent, _ time.Time) (cctranscript.UsagePage, error) {
	g.reads = append(g.reads, a.Name)
	return g.pages[a.Name], nil
}

// eventsFailover is an AccountFailover whose history is fixed per agent id.
type eventsFailover struct {
	AccountFailover
	events map[int64][]ccfailover.Event
}

func (f eventsFailover) Events(_ context.Context, id int64, _ int) ([]ccfailover.Event, error) {
	return f.events[id], nil
}
func (f eventsFailover) UseHooks(ccfailover.Policy, ccfailover.Prepare) {}

func ch(h int) time.Time { return time.Date(2026, 11, 10, h, 0, 0, 0, time.UTC) }

type consumerRig struct {
	s       *Server
	gw      *consumerGateway
	prov    *officerProv
	hq      *hqtest.Server
	c       *consumerusage.Collector
	logs    *bytes.Buffer
	hook    func(context.Context, agents.Agent)
	officer string
}

// consumerServer is a muster with officer offA declared (instance 1 up) and
// consumer usage on, and these rows:
//
//	review-w1  operator, claude-code, acct-b, up; failed over acct-a -> acct-b at 11:30
//	review-w2  operator, claude-code, acct-c, up
//	review-w3  operator, claude-code, acct-c, STOPPED
//	member-w4  owner 2,  claude-code, acct-d, up (a member's own account)
//	gw-w5      operator, gateway kind (so no account), up
//	<officer>  operator, claude-code, acct-e, up — an officer instance
func consumerServer(t *testing.T) *consumerRig {
	t.Helper()
	hq := hqtest.New(map[string]string{offATok: offA})
	t.Cleanup(hq.Close)
	hq.AddPoolToken("pool-tok-SENTINEL-31")
	hq.Now = func() time.Time { return time.Date(2026, 11, 10, 12, 40, 0, 0, time.UTC) } // HQ refuses hours past 8 days old
	store := &officerAgents{}
	prov := &officerProv{store: store, phase: map[string]provision.Phase{}}
	gw := &consumerGateway{}
	logs := &bytes.Buffer{}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(logs, "", 0))
	s.UseExtensions(Extensions{Agents: store, Provisioner: prov, Gateway: gw, SessionLiveness: stubLiveness{}, Kinds: stubKinds{pick: "acct-one"},
		AccountFailover: eventsFailover{events: map[int64][]ccfailover.Event{
			801: {{AgentID: 801, From: "acct-a", To: "acct-b", At: ch(11).Add(30 * time.Minute)}}}}})
	o := officers.Officer{ID: offA, HQURL: hq.BaseURL(), MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: time.Minute, Owner: 1}.WithToken(offATok)
	now := time.Date(2026, 11, 10, 12, 40, 0, 0, time.UTC)
	if _, err := s.EnableOfficers(OfficersConfig{Officers: []officers.Officer{o}, InstallSession: discardSession, NoUsageRun: true,
		NoTranscriptRun: true, Now: func() time.Time { return now }}); err != nil {
		t.Fatal(err)
	}
	officer := officers.InstanceName(o.ID, 1)
	cc := agents.KindClaudeCode
	for _, a := range []agents.Agent{
		{ID: 801, Name: "review-w1", Kind: cc, OwnerID: 1, CCAccount: "acct-b", Status: agents.StatusRunning},
		{ID: 802, Name: "review-w2", Kind: cc, OwnerID: 1, CCAccount: "acct-c", Status: agents.StatusRunning},
		{ID: 803, Name: "review-w3", Kind: cc, OwnerID: 1, CCAccount: "acct-c", Status: agents.StatusStopped},
		{ID: 804, Name: "member-w4", Kind: cc, OwnerID: 2, CCAccount: "acct-d", Status: agents.StatusRunning},
		{ID: 805, Name: "gw-w5", Kind: agents.KindGateway, OwnerID: 1, Status: agents.StatusRunning},
		{ID: 806, Name: officer, Kind: cc, OwnerID: 1, CCAccount: "acct-e", Status: agents.StatusRunning},
	} {
		a.Namespace = "ns"
		store.rows = append(store.rows, a)
		if a.Status == agents.StatusRunning {
			prov.phase[a.Name] = provision.PhaseRunning
		} else {
			prov.phase[a.Name] = provision.PhaseStopped
		}
	}
	page := func(sess string, h, base int64) cctranscript.UsagePage {
		return cctranscript.UsagePage{Buckets: []cctranscript.UsageBucket{{Session: sess, Hour: ch(int(h)), Turns: base + 1, APICalls: base + 2,
			InputTokens: base + 3, OutputTokens: base + 4, CacheReadTokens: base + 5, CacheCreationTokens: base + 6}}}
	}
	w1 := page("s-alpha", 10, 100)
	w1.Buckets = append(w1.Buckets, page("s-beta", 12, 200).Buckets...)
	gw.pages = map[string]cctranscript.UsagePage{"review-w1": w1, "review-w2": page("s-gamma", 11, 300),
		"review-w3": page("s-delta", 9, 400), "member-w4": page("s-eps", 9, 500), officer: page("s-off", 9, 600)}
	r := &consumerRig{s: s, gw: gw, prov: prov, hq: hq, logs: logs, officer: officer}
	c, err := s.EnableConsumerUsage(ConsumerUsageConfig{Consumer: consumerusage.ConsumerPRReview, Instance: "review-muster",
		URL: hq.ConsumerUsageURL(), Token: "pool-tok-SENTINEL-31", InstallBeforeStop: func(f func(context.Context, agents.Agent)) { r.hook = f },
		Backoff: []time.Duration{}, NoRun: true, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	r.c = c
	return r
}

const (
	consumerW1Body = `{"instance":"review-muster","consumer":"pr-review","hours":[` +
		`{"hour":"2026-11-10T10:00:00Z","account":"acct-a","session":"s-alpha","turns":101,"api_calls":102,"input_tokens":103,"output_tokens":104,"cache_read_tokens":105,"cache_creation_tokens":106},` +
		`{"hour":"2026-11-10T12:00:00Z","account":"acct-b","session":"s-beta","turns":201,"api_calls":202,"input_tokens":203,"output_tokens":204,"cache_read_tokens":205,"cache_creation_tokens":206}]}`
	consumerW2Body = `{"instance":"review-muster","consumer":"pr-review","hours":[` +
		`{"hour":"2026-11-10T11:00:00Z","account":"acct-c","session":"s-gamma","turns":301,"api_calls":302,"input_tokens":303,"output_tokens":304,"cache_read_tokens":305,"cache_creation_tokens":306}]}`
)

// TestATickPushesTheOperatorsUpNonOfficerInstancesOnly: one tick reads and
// pushes review-w1 and review-w2 — not the stopped one, the member's, the
// gateway agent, nor the officer instance — w1's two sessions on the two
// accounts its failover history says they ran on.
func TestATickPushesTheOperatorsUpNonOfficerInstancesOnly(t *testing.T) {
	r := consumerServer(t)
	r.c.Tick(context.Background())
	if got := strings.Join(r.gw.reads, ","); got != "review-w1,review-w2" {
		t.Fatalf("reads %s, want review-w1,review-w2", got)
	}
	puts := r.hq.ConsumerPuts()
	if len(puts) != 2 || puts[0] != consumerW1Body || puts[1] != consumerW2Body {
		t.Fatalf("PUTs:\n%s\nwant:\n%s\n%s", strings.Join(puts, "\n"), consumerW1Body, consumerW2Body)
	}
	if n := len(r.hq.UsagePuts()); n != 0 {
		t.Fatalf("%d officer usage PUTs from the consumer collector", n)
	}
}

// TestTheOfficerInstanceIsPushedOnlyByItsOwnCollector: the officer's own
// collector pushes it to its officer route; the consumer collector's tick and
// its pre-stop hook never push it — so HQ sees it once.
func TestTheOfficerInstanceIsPushedOnlyByItsOwnCollector(t *testing.T) {
	r := consumerServer(t)
	r.s.officerRT.usage.Tick(context.Background())
	if n := len(r.hq.UsagePuts()); n != 1 {
		t.Fatalf("%d officer usage PUTs, want 1 (positive control)", n)
	}
	r.c.Tick(context.Background())
	a, _ := r.s.ext.Agents.GetByName(context.Background(), r.officer)
	r.hook(context.Background(), a)
	for _, p := range r.hq.ConsumerPuts() {
		if strings.Contains(p, "s-off") {
			t.Fatalf("the officer instance was pushed as consumer usage: %s", p)
		}
	}
	if strings.Count(strings.Join(r.gw.reads, ","), r.officer) != 1 {
		t.Fatalf("reads %v: the officer instance was read more than once (by its own collector)", r.gw.reads)
	}
}

// TestThePreStopHookPushesAnEligibleInstanceOnce: the hook EnableConsumerUsage
// installs pushes review-w2 before it stops; handed a member's agent it does
// nothing.
func TestThePreStopHookPushesAnEligibleInstanceOnce(t *testing.T) {
	r := consumerServer(t)
	if r.hook == nil {
		t.Fatal("EnableConsumerUsage installed no pre-stop hook")
	}
	w2, _ := r.s.ext.Agents.GetByName(context.Background(), "review-w2")
	r.hook(context.Background(), w2)
	if puts := r.hq.ConsumerPuts(); len(puts) != 1 || puts[0] != consumerW2Body {
		t.Fatalf("pre-stop push %v", puts)
	}
	m, _ := r.s.ext.Agents.GetByName(context.Background(), "member-w4")
	r.hook(context.Background(), m)
	if n := len(r.hq.ConsumerPuts()); n != 1 || strings.Contains(strings.Join(r.gw.reads, ","), "member-w4") {
		t.Fatalf("a member's agent was read or pushed: %v", r.gw.reads)
	}
}

// TestEnableConsumerUsageRefusesWithoutThePreStopInstaller: no installer, no
// collector.
func TestEnableConsumerUsageRefusesWithoutThePreStopInstaller(t *testing.T) {
	r := consumerServer(t)
	if _, err := r.s.EnableConsumerUsage(ConsumerUsageConfig{Consumer: consumerusage.ConsumerPRReview, Instance: "x", URL: r.hq.ConsumerUsageURL(),
		Token: "t", NoRun: true}); err == nil || !strings.Contains(err.Error(), "before it is stopped") {
		t.Fatalf("err %v", err)
	}
}
