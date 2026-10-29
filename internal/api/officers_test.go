package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// THE OFFICER RUNTIME, BOUND TO THE SERVER (officers.go).
//
// Two officers, A and B, each with its own token, against one fake HQ that
// knows which officer each token is (hqtest). The tokens are pairwise distinct
// and appear in no fixture name, so a token showing up anywhere is the token
// and nothing else.
// ---------------------------------------------------------------------------

const (
	offA    = "release:alpha-web"
	offB    = "triage:beta-api"
	offATok = "fixture-officer-token-alpha-7c1e"
	offBTok = "fixture-officer-token-beta-93d4"
)

// officerAgents is an agents.Store holding rows in memory, with the re-arm.
type officerAgents struct {
	agents.Store
	mu      sync.Mutex
	rows    []agents.Agent
	nextID  int64
	rearmed map[int64]string
	created []agents.Agent
	claims  map[int64]string
}

func (s *officerAgents) List(context.Context) ([]agents.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agents.Agent(nil), s.rows...), nil
}

func (s *officerAgents) Get(_ context.Context, id int64) (agents.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.rows {
		if a.ID == id {
			return a, nil
		}
	}
	return agents.Agent{}, errors.New("no such agent")
}

func (s *officerAgents) GetByName(_ context.Context, name string) (agents.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.rows {
		if a.Name == name {
			return a, nil
		}
	}
	return agents.Agent{}, errors.New("no such agent")
}

func (s *officerAgents) Create(_ context.Context, a agents.Agent) (agents.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rows {
		if r.Name == a.Name {
			return agents.Agent{}, errors.New("duplicate name")
		}
	}
	s.nextID++
	a.ID = 500 + s.nextID
	s.rows = append(s.rows, a)
	s.created = append(s.created, a)
	return a, nil
}

func (s *officerAgents) RearmKickoff(_ context.Context, id int64, note string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rearmed == nil {
		s.rearmed = map[int64]string{}
	}
	s.rearmed[id] = note
	for i := range s.rows {
		if s.rows[i].ID == id {
			s.rows[i].PendingNote, s.rows[i].KickedOff = note, false
		}
	}
	return nil
}

func (s *officerAgents) ClaimKickoff(_ context.Context, id int64, owner string, _ time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claims == nil {
		s.claims = map[int64]string{}
	}
	if h, ok := s.claims[id]; ok && h != owner {
		return false, nil
	}
	s.claims[id] = owner
	return true, nil
}

func (s *officerAgents) ReleaseKickoffClaim(_ context.Context, id int64, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claims[id] == owner {
		delete(s.claims, id)
	}
	return nil
}

func (s *officerAgents) LatestOrCreateSession(_ context.Context, id int64, name string) (agents.ChatSession, error) {
	return agents.ChatSession{ID: id, SessionKey: "agent:" + name + ":webchat"}, nil
}

func (s *officerAgents) AddChatMessage(_ context.Context, m agents.ChatMessage) (agents.ChatMessage, error) {
	return m, nil
}

func (s *officerAgents) AgentsByNoteIDs(context.Context, []int64) (map[int64]agents.Agent, error) {
	return map[int64]agents.Agent{}, nil
}

func (s *officerAgents) LatestSessionIDs(context.Context, []int64) (map[int64]int64, error) {
	return map[int64]int64{}, nil
}

func (s *officerAgents) LastMessageByAgentIDs(context.Context, []int64) (map[int64]time.Time, error) {
	return map[int64]time.Time{}, nil
}

// officerProv is a provisioner whose instances are a name -> phase map, and
// which records every lifecycle call.
type officerProv struct {
	stubProvisioner
	mu        sync.Mutex
	store     *officerAgents
	phase     map[string]provision.Phase
	starts    []int64
	stops     []int64
	dispatch  []int64
	destroyed []int64
}

func (p *officerProv) nameOf(id int64) string {
	a, _ := p.store.Get(context.Background(), id)
	return a.Name
}

func (p *officerProv) Dispatch(_ context.Context, id int64, _ bool) error {
	n := p.nameOf(id)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dispatch = append(p.dispatch, id)
	p.phase[n] = provision.PhaseRunning
	return nil
}

func (p *officerProv) Start(_ context.Context, id int64) error {
	n := p.nameOf(id)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.starts = append(p.starts, id)
	p.phase[n] = provision.PhaseRunning
	return nil
}

func (p *officerProv) Stop(_ context.Context, id int64) error {
	n := p.nameOf(id)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stops = append(p.stops, id)
	p.phase[n] = provision.PhaseStopped
	return nil
}

func (p *officerProv) Destroy(_ context.Context, id int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.destroyed = append(p.destroyed, id)
	return nil
}

func (p *officerProv) Instances(context.Context) ([]provision.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []provision.Instance
	for n, ph := range p.phase {
		out = append(out, provision.Instance{Ref: provision.Ref{Name: n}, Phase: ph, Ready: ph == provision.PhaseRunning})
	}
	return out, nil
}

func (p *officerProv) counts() (dispatch, starts, stops int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.dispatch), len(p.starts), len(p.stops)
}

type officerRig struct {
	s     *Server
	h     http.Handler
	store *officerAgents
	prov  *officerProv
	hq    *hqtest.Server
	rec   *officers.Reconciler
	logs  *bytes.Buffer
	a, b  officers.Officer
	chat  *repromptChat

	clockMu sync.Mutex
	clock   time.Time
	// installed is the session source EnableOfficers installed.
	installed func(agents.Agent) ([]string, string, bool)
}

func (r *officerRig) installSession(f func(agents.Agent) ([]string, string, bool)) { r.installed = f }

func discardSession(func(agents.Agent) ([]string, string, bool)) {}

func (r *officerRig) now() time.Time {
	r.clockMu.Lock()
	defer r.clockMu.Unlock()
	return r.clock
}

func (r *officerRig) advance(d time.Duration) {
	r.clockMu.Lock()
	r.clock = r.clock.Add(d)
	r.clockMu.Unlock()
}

// officerOrdinaryID and officerChiefID are non-officer rows the fixture
// carries, so "offered to officers only" is measured against agents that
// exist.
const (
	officerOrdinaryID int64 = 61
	officerChiefID    int64 = 73
)

func newOfficerRig(t *testing.T) *officerRig {
	t.Helper()
	hq := hqtest.New(map[string]string{offATok: offA, offBTok: offB})
	t.Cleanup(hq.Close)
	logs := &bytes.Buffer{}
	store := &officerAgents{rows: []agents.Agent{
		{ID: officerOrdinaryID, Name: "brave-otter", DisplayName: "worker", Status: agents.StatusRunning, OwnerID: 1},
		{ID: officerChiefID, Name: agents.ChiefName, DisplayName: ChiefDisplayName, Status: agents.StatusRunning, OwnerID: 1},
	}}
	prov := &officerProv{store: store, phase: map[string]provision.Phase{}}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(logs, "", 0))
	chat := &repromptChat{}
	s.UseExtensions(Extensions{Agents: store, Provisioner: prov, Gateway: chat, SessionLiveness: stubLiveness{}, Kinds: stubKinds{pick: "acct-one"}})
	a := officers.Officer{ID: offA, HQURL: hq.BaseURL(), MaxInstances: 2, IdleAfter: 30 * time.Minute, PollInterval: time.Minute, Owner: 1}.WithToken(offATok)
	b := officers.Officer{ID: offB, HQURL: hq.BaseURL(), MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: time.Minute, Owner: 1}.WithToken(offBTok)
	r := &officerRig{s: s, store: store, prov: prov, hq: hq, logs: logs, a: a, b: b, chat: chat, clock: time.Now()}
	hq.Now = r.now
	rec, err := s.EnableOfficers(OfficersConfig{Officers: []officers.Officer{a, b}, Now: r.now, InstallSession: r.installSession})
	if err != nil {
		t.Fatal(err)
	}
	r.rec, r.h = rec, s.Handler()
	return r
}

// addInstance stores instance n of o as an agent row.
func (r *officerRig) addInstance(o officers.Officer, n int, id int64) agents.Agent {
	a := agents.Agent{ID: id, Name: officers.InstanceName(o.ID, n), DisplayName: "officer", Kind: agents.KindClaudeCode, Status: agents.StatusRunning, OwnerID: 1, Namespace: "ns"}
	r.store.mu.Lock()
	r.store.rows = append(r.store.rows, a)
	r.store.mu.Unlock()
	r.prov.mu.Lock()
	r.prov.phase[a.Name] = provision.PhaseRunning
	r.prov.mu.Unlock()
	return a
}

func (r *officerRig) agent(t *testing.T, id int64) agents.Agent {
	t.Helper()
	a, err := r.store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func officerToolNameSet(defs []agents.ToolDef) map[string]bool {
	out := map[string]bool{}
	for _, d := range defs {
		out[d.Name] = true
	}
	return out
}

var allOfficerTools = []string{officers.ToolBrief, officers.ToolItems, officers.ToolClaim, officers.ToolHeartbeat, officers.ToolComplete, officers.ToolRelease, officers.ToolReleaseEvaluation}

// TestTheOfficerToolsAreOfferedToOfficerInstancesOnly: an officer's instance is
// offered all six officer tools over MCP; an ordinary agent and the chief are
// offered none, and calling one anyway is refused without reaching HQ.
func TestTheOfficerToolsAreOfferedToOfficerInstancesOnly(t *testing.T) {
	r := newOfficerRig(t)
	inst := r.addInstance(r.a, 1, 801)
	ctx := context.Background()
	got := officerToolNameSet(r.s.MCPToolDefsFor(ctx, inst))
	for _, n := range allOfficerTools {
		if !got[n] {
			t.Errorf("officer instance is not offered %s over MCP", n)
		}
	}
	for _, id := range []int64{officerOrdinaryID, officerChiefID} {
		a := r.agent(t, id)
		for _, defs := range [][]agents.ToolDef{r.s.MCPToolDefsFor(ctx, a), r.s.AgentToolDefsFor(ctx, a)} {
			for n := range officerToolNameSet(defs) {
				if isOfficerTool(n) {
					t.Errorf("agent %d (%s) is offered %s", id, a.Name, n)
				}
			}
		}
		r.hq.ResetRequests()
		out := r.s.dispatchAgentTool(ctx, a, officers.ToolClaim, `{"item_id":"1"}`)
		if !strings.Contains(out, "officer instances only") || !isToolErr(out) {
			t.Errorf("agent %d calling officer_claim: %s, want the officer-only refusal", id, out)
		}
		if n := len(r.hq.Requests()); n != 0 {
			t.Errorf("a refused officer tool call from agent %d still reached HQ (%d requests)", id, n)
		}
	}
}

// TestAnOfficerToolReachesHQWithItsOwnOfficersTokenOnly: officer A's instance
// claiming an item reaches HQ under A's path with A's token, as instance =
// its agent name; officer B's instance with B's. Naming the other officer is
// refused before HQ is reached.
func TestAnOfficerToolReachesHQWithItsOwnOfficersTokenOnly(t *testing.T) {
	r := newOfficerRig(t)
	instA := r.addInstance(r.a, 2, 802)
	instB := r.addInstance(r.b, 1, 803)
	itemA := r.hq.AddItem(offA, "release.evaluate", "svc:v1")
	itemB := r.hq.AddItem(offB, "triage.look", "svc:x")
	ctx := context.Background()

	for _, tc := range []struct {
		inst            agents.Agent
		officer, tok    string
		item, otherTok  string
		otherOfficerArg string
	}{
		{instA, offA, offATok, itemA, offBTok, offB},
		{instB, offB, offBTok, itemB, offATok, offA},
	} {
		r.hq.ResetRequests()
		out := r.s.dispatchAgentTool(ctx, tc.inst, officers.ToolClaim, `{"item_id":"`+tc.item+`","lease_seconds":120}`)
		// What reached HQ is checked BEFORE the result, so a call that went to
		// the wrong officer fails on the token and path it carried, not only on
		// HQ's answer to it.
		reqs := r.hq.Requests()
		if len(reqs) != 1 {
			t.Fatalf("%s claim: %d HQ requests, want 1", tc.officer, len(reqs))
		}
		q := reqs[0]
		if want := "/api/v1/officers/" + tc.officer + "/items/" + tc.item + "/claim"; q.Path != want {
			t.Errorf("path = %s, want %s", q.Path, want)
		}
		if q.Auth != "Bearer "+tc.tok {
			t.Errorf("%s's instance reached HQ with Authorization %q, want its own officer's token", tc.officer, q.Auth)
		}
		if strings.Contains(q.Auth, tc.otherTok) {
			t.Errorf("%s's instance carried the OTHER officer's token", tc.officer)
		}
		if q.Body["instance"] != tc.inst.Name || q.Body["lease_seconds"] != float64(120) {
			t.Errorf("claim body = %v, want instance %s, lease 120", q.Body, tc.inst.Name)
		}
		if isToolErr(out) {
			t.Fatalf("%s claim: %s", tc.officer, out)
		}
		if it := r.hq.Item(tc.item); it.ClaimedBy != tc.inst.Name {
			t.Errorf("item claimed by %q, want %q", it.ClaimedBy, tc.inst.Name)
		}

		// Naming the other officer, by either key, is refused before HQ.
		for _, key := range []string{"officer_id", "officer"} {
			r.hq.ResetRequests()
			out := r.s.dispatchAgentTool(ctx, tc.inst, officers.ToolItems, `{"`+key+`":"`+tc.otherOfficerArg+`"}`)
			if !isToolErr(out) || !strings.Contains(out, "you act for officer "+tc.officer+" only") {
				t.Errorf("%s naming %s via %s: %s, want the binding refusal", tc.officer, tc.otherOfficerArg, key, out)
			}
			if n := len(r.hq.Requests()); n != 0 {
				t.Errorf("%s naming another officer reached HQ (%d requests)", tc.officer, n)
			}
		}
		// Naming its OWN officer is fine.
		r.hq.ResetRequests()
		if out := r.s.dispatchAgentTool(ctx, tc.inst, officers.ToolItems, `{"officer_id":"`+tc.officer+`"}`); isToolErr(out) {
			t.Errorf("%s naming itself: %s", tc.officer, out)
		}
	}
	if bytes.Contains(r.logs.Bytes(), []byte(offATok)) || bytes.Contains(r.logs.Bytes(), []byte(offBTok)) {
		t.Fatal("an officer token reached the server log")
	}
}

// TestTheOfficerToolsWalkTheQueue drives the whole item life cycle through the
// tools: brief, items, claim, a second instance's conflicting claim (409 with
// the holder), heartbeat, complete; and the error shapes.
func TestTheOfficerToolsWalkTheQueue(t *testing.T) {
	r := newOfficerRig(t)
	one := r.addInstance(r.a, 1, 811)
	two := r.addInstance(r.a, 2, 812)
	r.hq.Charters[offA] = "# Release captain\nEvaluate each release."
	item := r.hq.AddItem(offA, "release.evaluate", "svc:v9")
	ctx := context.Background()
	call := func(a agents.Agent, tool, args string) string { return r.s.dispatchAgentTool(ctx, a, tool, args) }

	if out := call(one, officers.ToolBrief, ``); !strings.Contains(out, "Release captain") || !strings.Contains(out, `"svc:v9"`) {
		t.Fatalf("brief = %s", out)
	}
	if out := call(one, officers.ToolItems, `{}`); !strings.Contains(out, `"subject_key":"svc:v9"`) {
		t.Fatalf("items = %s", out)
	}
	if out := call(one, officers.ToolClaim, `{"item_id":`+item+`}`); isToolErr(out) {
		t.Fatalf("claim with a numeric id: %s", out)
	}
	out := call(two, officers.ToolClaim, `{"item_id":"`+item+`"}`)
	if !isToolErr(out) || !strings.Contains(out, `"status":409`) || !strings.Contains(out, one.Name) {
		t.Fatalf("a second instance's claim: %s, want a 409 refusal naming the holder", out)
	}
	if out := call(one, officers.ToolHeartbeat, `{"item_id":"`+item+`"}`); isToolErr(out) {
		t.Fatalf("heartbeat: %s", out)
	}
	if out := call(one, officers.ToolComplete, `{"item_id":"`+item+`","outcome":"maybe","summary":"x"}`); !strings.Contains(out, "outcome must be one of") {
		t.Fatalf("a bad outcome: %s", out)
	}
	if out := call(one, officers.ToolComplete, `{"item_id":"`+item+`","outcome":"done","summary":"`+strings.Repeat("x", officers.MaxSummaryChars+1)+`"}`); !strings.Contains(out, "at most") {
		t.Fatalf("an oversized summary: %s", out)
	}
	if out := call(one, officers.ToolComplete, `{"item_id":"`+item+`","outcome":"done","summary":"x","links":[`+strings.TrimSuffix(strings.Repeat(`"https://e.example/x",`, officers.MaxLinks+1), ",")+`]}`); !strings.Contains(out, "at most 10 links") {
		t.Fatalf("eleven links: %s", out)
	}
	if out := call(one, officers.ToolComplete, `{"item_id":"`+item+`","outcome":"done","summary":"low risk"}`); isToolErr(out) {
		t.Fatalf("complete: %s", out)
	}
	if it := r.hq.Item(item); it.Status != "done" || it.Summary != "low risk" {
		t.Fatalf("item after complete: %+v", it)
	}
	if out := call(one, officers.ToolClaim, `{"item_id":"../../x"}`); !strings.Contains(out, "item_id is required") {
		t.Fatalf("a path-shaped item id: %s", out)
	}
	r.hq.SetDown(true)
	if out := call(one, officers.ToolItems, `{}`); !strings.Contains(out, "HQ is unavailable") {
		t.Fatalf("HQ down: %s", out)
	}
}

// TestTheWakeLoopCreatesStartsAndStopsInstanceAgents: the loop, over the real
// fleet binding — an open item creates instance 1 as a claude-code agent whose
// first turn is the kickoff; idling scales it to zero through Stop; a new item
// re-arms its first turn and Starts it (the agent and its volume are kept).
func TestTheWakeLoopCreatesStartsAndStopsInstanceAgents(t *testing.T) {
	r := newOfficerRig(t)
	ctx := context.Background()
	r.hq.AddItem(offB, "triage.look", "svc:1")
	if res := r.rec.Check(ctx, offB, true); res.Err != nil || len(res.ActErrs) != 0 {
		t.Fatalf("pass: %+v", res)
	}
	if len(r.store.created) != 1 {
		t.Fatalf("created %d agents, want 1", len(r.store.created))
	}
	c := r.store.created[0]
	if c.Name != officers.InstanceName(offB, 1) || c.Kind != agents.KindClaudeCode || c.CCAccount != "acct-one" {
		t.Fatalf("created %+v", c)
	}
	if !strings.Contains(c.PendingNote, "You are officer "+offB) || strings.Contains(c.PendingNote, offBTok) {
		t.Fatalf("first turn: %q (must name the officer and never carry its token)", c.PendingNote)
	}
	officerWaitFor(t, func() bool { d, _, _ := r.prov.counts(); return d == 1 })

	// Idle: the item is done; after the window the instance is stopped.
	cl := officers.NewClient(r.b, nil)
	if _, err := cl.Claim(ctx, "1", c.Name, 60); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Complete(ctx, "1", c.Name, officers.Completion{Outcome: "done", Summary: "ok"}); err != nil {
		t.Fatal(err)
	}
	r.rec.Check(ctx, offB, true) // idle since now
	r.advance(29 * time.Minute)
	if res := r.rec.Check(ctx, offB, true); len(res.Decision.Stop) != 0 {
		t.Fatalf("29m into a 30m window: %+v", res.Decision)
	}
	r.advance(time.Minute)
	if res := r.rec.Check(ctx, offB, true); len(res.Decision.Stop) != 1 {
		t.Fatalf("idle past the window: %+v", res.Decision)
	}
	if _, _, stops := r.prov.counts(); stops != 1 {
		t.Fatalf("stops = %d, want 1", stops)
	}
	// A new item: the existing agent is re-armed and started, not re-created.
	r.hq.AddItem(offB, "triage.look", "svc:2")
	if res := r.rec.Check(ctx, offB, true); len(res.ActErrs) != 0 || len(res.Decision.Start) != 1 {
		t.Fatalf("re-wake: %+v", res)
	}
	if len(r.store.created) != 1 {
		t.Fatalf("re-wake re-created the agent (%d creates)", len(r.store.created))
	}
	if note := r.store.rearmed[c.ID]; !strings.Contains(note, "You are officer "+offB) || strings.Contains(note, offBTok) {
		t.Fatalf("re-armed first turn: %q", note)
	}
	if _, starts, _ := r.prov.counts(); starts != 1 {
		t.Fatalf("starts = %d, want 1", starts)
	}
}

// TestAnErrorOrInFlightInstanceHoldsItsSlot: an instance whose row is in
// error, or whose create is in flight (row provisioning, no backend instance
// yet), counts as up — the loop does not start it again every poll.
func TestAnErrorOrInFlightInstanceHoldsItsSlot(t *testing.T) {
	r := newOfficerRig(t)
	r.hq.AddItem(offB, "triage.look", "svc:1")
	for _, tc := range []struct {
		name  string
		row   string
		phase provision.Phase // "" = no backend instance
	}{
		{"error row", agents.StatusError, provision.PhaseRunning},
		{"crash loop", agents.StatusRunning, provision.PhaseFailed},
		{"create in flight", agents.StatusProvisioning, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r.store.mu.Lock()
			r.store.rows = r.store.rows[:2]
			r.store.rows = append(r.store.rows, agents.Agent{ID: 901, Name: officers.InstanceName(offB, 1), Status: tc.row, Namespace: "ns", OwnerID: 1})
			r.store.mu.Unlock()
			r.prov.mu.Lock()
			r.prov.phase = map[string]provision.Phase{}
			if tc.phase != "" {
				r.prov.phase[officers.InstanceName(offB, 1)] = tc.phase
			}
			r.prov.mu.Unlock()
			if res := r.rec.Check(context.Background(), offB, true); len(res.Decision.Start) != 0 {
				t.Fatalf("started an instance that holds its slot: %+v", res.Decision)
			}
		})
	}
	// Control: the same row STOPPED is started.
	r.store.mu.Lock()
	r.store.rows[2].Status = agents.StatusStopped
	r.store.mu.Unlock()
	r.prov.mu.Lock()
	r.prov.phase[officers.InstanceName(offB, 1)] = provision.PhaseStopped
	r.prov.mu.Unlock()
	if res := r.rec.Check(context.Background(), offB, true); len(res.Decision.Start) != 1 {
		t.Fatalf("a stopped instance with an open item was not started: %+v", res.Decision)
	}
}

func officerWaitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestADeclaredOfficersInstanceCannotBeDeleted: DELETE /agents/{id} refuses a
// declared officer's instance with 409 and tears nothing down; its card offers
// no Delete. An agent whose name LOOKS like an instance but is past the
// officer's max, or belongs to no declared officer, is an ordinary agent.
func TestADeclaredOfficersInstanceCannotBeDeleted(t *testing.T) {
	r := newOfficerRig(t)
	inst := r.addInstance(r.a, 1, 821)
	past := r.addInstance(officers.Officer{ID: offA, MaxInstances: 3}, 3, 822) // past A's max of 2
	stranger := r.addInstance(officers.Officer{ID: "gone:officer", MaxInstances: 1}, 1, 823)
	for _, tc := range []struct {
		a       agents.Agent
		protect bool
	}{{inst, true}, {past, false}, {stranger, false}} {
		rec := doReq(t, r.s, r.h, http.MethodDelete, "/agents/"+strconv.FormatInt(tc.a.ID, 10))
		got := waitDestroyed(r.prov, tc.a.ID)
		if tc.protect {
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "instance of officer "+offA) {
				t.Errorf("DELETE %s = %d %q, want 409 naming the officer", tc.a.Name, rec.Code, rec.Body.String())
			}
			if got {
				t.Errorf("%s was refused AND destroyed", tc.a.Name)
			}
			continue
		}
		if rec.Code != http.StatusOK || !got {
			t.Errorf("DELETE %s = %d (destroyed %v), want 200 and a Destroy", tc.a.Name, rec.Code, got)
		}
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(r.logs.String())
		}
	})
	body := getOwed(t, r.s, r.h, "/ui/agents").Body.String()
	if card := cardChunk(t, body, inst.ID); strings.Contains(card, `hx-delete="/agents/`+itoa(inst.ID)+`"`) {
		t.Error("the officer instance's card offers Delete")
	}
	if card := cardChunk(t, body, officerOrdinaryID); !strings.Contains(card, `hx-delete="/agents/`+itoa(officerOrdinaryID)+`"`) {
		t.Error("control: an ordinary agent's card lost Delete")
	}
}

func waitDestroyed(p *officerProv, id int64) bool {
	deadline := time.Now().Add(destroyWindow)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		for _, d := range p.destroyed {
			if d == id {
				p.mu.Unlock()
				return true
			}
		}
		p.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// TestTheChiefPredicateIgnoresOfficers: an officer instance is delete-protected
// WITHOUT becoming the chief — it is not offered a chief-only tool, and the
// chief predicate answers exactly as it did.
func TestTheChiefPredicateIgnoresOfficers(t *testing.T) {
	r := newOfficerRig(t)
	inst := r.addInstance(r.a, 1, 831)
	list, _ := r.store.List(context.Background())
	chief := chiefPredicate(list)
	for _, a := range list {
		want := a.ID == officerChiefID
		if got := chief(a); got != want {
			t.Errorf("chiefPredicate(%s) = %v, want %v", a.Name, got, want)
		}
	}
	if !r.s.deleteProtection(list)(inst) || !r.s.deleteProtection(list)(r.agent(t, officerChiefID)) || r.s.deleteProtection(list)(r.agent(t, officerOrdinaryID)) {
		t.Error("deleteProtection must cover the chief and the officer instance, not the ordinary agent")
	}
	if r.s.isChiefAgent(context.Background(), inst) {
		t.Error("an officer instance reads as the chief")
	}
}

// TestEnableOfficersRefusesWithoutClaudeCode: officers need the claude-code kind.
func TestEnableOfficersRefusesWithoutClaudeCode(t *testing.T) {
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
	s.UseExtensions(Extensions{Agents: &officerAgents{}, Provisioner: stubProvisioner{}, SessionLiveness: stubLiveness{}})
	o := officers.Officer{ID: offA, HQURL: "http://hq.invalid/api/v1", MaxInstances: 1}.WithToken("x")
	if _, err := s.EnableOfficers(OfficersConfig{Officers: []officers.Officer{o}, InstallSession: discardSession}); err == nil || !strings.Contains(err.Error(), "claude-code") {
		t.Fatalf("err = %v", err)
	}
}

// TestTheOfficerNamePrefixCannotBeGenerated: the name generator's pool cannot
// mint a name in the officer namespace, so no ordinary agent can come to be
// bound to an officer. Drawn many times: every draw is adjective-noun.
func TestTheOfficerNamePrefixCannotBeGenerated(t *testing.T) {
	store := &officerAgents{}
	for i := 0; i < 500; i++ {
		n, err := agents.BuildUniqueAgentName(context.Background(), nameExistsNever{store})
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(n, officers.InstanceNamePrefix) {
			t.Fatalf("the generator minted %q", n)
		}
	}
}

type nameExistsNever struct{ *officerAgents }

func (nameExistsNever) NameExists(context.Context, string) (bool, error) { return false, nil }

// TestTheReleaseEvaluationToolIsTheReleaseOfficersOnly: a release officer's
// instance posts to HQ's captain route with its own token; muster builds
// HQ's body {service, tag, evaluation?, report?, instance} from explicit tool
// arguments, setting instance itself. A non-release officer's instance is
// neither offered the tool nor able to call it.
func TestTheReleaseEvaluationToolIsTheReleaseOfficersOnly(t *testing.T) {
	r := newOfficerRig(t)
	rel := r.addInstance(r.a, 1, 841)    // release:alpha-web
	triage := r.addInstance(r.b, 1, 842) // triage:beta-api
	ctx := context.Background()
	if !officerToolNameSet(r.s.MCPToolDefsFor(ctx, rel))[officers.ToolReleaseEvaluation] {
		t.Fatal("the release officer is not offered officer_release_evaluation")
	}
	if officerToolNameSet(r.s.MCPToolDefsFor(ctx, triage))[officers.ToolReleaseEvaluation] {
		t.Fatal("a triage officer is offered officer_release_evaluation")
	}
	out := r.s.dispatchAgentTool(ctx, rel, officers.ToolReleaseEvaluation,
		`{"service":"svc-web","tag":"20000101-abc","evaluation":{"level":"high","summary":"touches billing","monitors":[{"name":"Billing 5xx","url":"https://grafana.example.com/d/b"}]}}`)
	if isToolErr(out) {
		t.Fatalf("release evaluation: %s", out)
	}
	if len(r.hq.Evaluations) != 1 {
		t.Fatalf("captain route got %d posts, want 1", len(r.hq.Evaluations))
	}
	ev := r.hq.Evaluations[0]
	got, _ := json.Marshal(ev.Body)
	want := `{"evaluation":{"level":"high","monitors":[{"name":"Billing 5xx","url":"https://grafana.example.com/d/b"}],"summary":"touches billing"},` +
		`"instance":"` + rel.Name + `","service":"svc-web","tag":"20000101-abc"}`
	if ev.Officer != offA || string(got) != want {
		t.Fatalf("captain route body =\n  %s\nwant\n  %s (officer %s)", got, want, ev.Officer)
	}
	reqs := r.hq.Requests()
	if last := reqs[len(reqs)-1]; last.Path != "/api/v1/releases/captain" || last.Auth != "Bearer "+offATok {
		t.Fatalf("captain post went to %s with %q", last.Path, last.Auth)
	}
	// A report alone is a valid body.
	if out := r.s.dispatchAgentTool(ctx, rel, officers.ToolReleaseEvaluation, `{"service":"svc-web","tag":"t1","report":"an hour in: quiet"}`); isToolErr(out) {
		t.Fatalf("report only: %s", out)
	}
	if got, _ := json.Marshal(r.hq.Evaluations[1].Body); string(got) != `{"instance":"`+rel.Name+`","report":"an hour in: quiet","service":"svc-web","tag":"t1"}` {
		t.Fatalf("report-only body = %s", got)
	}

	// Malformed calls are refused locally, before HQ.
	r.hq.ResetRequests()
	for name, args := range map[string]string{
		"release_id at top level":       `{"service":"s","tag":"t","release_id":42,"evaluation":{"level":"low","summary":"x"}}`,
		"unknown evaluation field":      `{"service":"s","tag":"t","evaluation":{"level":"low","summary":"x","release_id":42}}`,
		"instance from the agent":       `{"service":"s","tag":"t","instance":"spoofed","report":"x"}`,
		"inner-only body":               `{"level":"low","summary":"x"}`,
		"old wrapper shape":             `{"evaluation":{"release_id":42,"risk":"low"}}`,
		"no service":                    `{"tag":"t","report":"x"}`,
		"no tag":                        `{"service":"s","report":"x"}`,
		"neither evaluation nor report": `{"service":"s","tag":"t"}`,
		"bad level":                     `{"service":"s","tag":"t","evaluation":{"level":"meh","summary":"x"}}`,
		"empty summary":                 `{"service":"s","tag":"t","evaluation":{"level":"low","summary":""}}`,
		"summary too long":              `{"service":"s","tag":"t","evaluation":{"level":"low","summary":"` + strings.Repeat("x", 10001) + `"}}`,
		"report too long":               `{"service":"s","tag":"t","report":"` + strings.Repeat("x", 20001) + `"}`,
		"monitor url not http":          `{"service":"s","tag":"t","evaluation":{"level":"low","summary":"x","monitors":[{"name":"m","url":"ftp://h/x"}]}}`,
		"monitor url with userinfo":     `{"service":"s","tag":"t","evaluation":{"level":"low","summary":"x","monitors":[{"name":"m","url":"https://u:p@h/x"}]}}`,
		"monitor no name":               `{"service":"s","tag":"t","evaluation":{"level":"low","summary":"x","monitors":[{"name":"","url":"https://h/x"}]}}`,
		"report not a string":           `{"service":"s","tag":"t","report":{"text":"x"}}`,
	} {
		if out := r.s.dispatchAgentTool(ctx, rel, officers.ToolReleaseEvaluation, args); !isToolErr(out) {
			t.Errorf("%s: %s, want a local refusal", name, out)
		}
	}
	if n := len(r.hq.Requests()); n != 0 {
		t.Fatalf("malformed captain calls reached HQ (%d requests)", n)
	}

	out = r.s.dispatchAgentTool(ctx, triage, officers.ToolReleaseEvaluation, `{"service":"s","tag":"t","report":"x"}`)
	if !isToolErr(out) || !strings.Contains(out, "not one of officer "+offB+"'s tools") {
		t.Fatalf("triage officer calling the captain tool: %s", out)
	}
	if len(r.hq.Requests()) != 0 {
		t.Fatal("a refused captain post reached HQ")
	}
}

// TestClaimingAnItemYouAlreadyHoldRenewsItsLease: HQ answers a claim on an
// item the CALLING instance already holds under a live lease with 409
// held_by=<that same instance> ("use heartbeat"). Instance ids are agent names,
// so a restarted instance re-claiming its own item hits exactly that. The claim
// tool turns it into success — a heartbeat with the requested lease, the item
// returned as claimed, "already yours". A 409 naming ANOTHER instance stays a
// refusal.
func TestClaimingAnItemYouAlreadyHoldRenewsItsLease(t *testing.T) {
	r := newOfficerRig(t)
	one := r.addInstance(r.a, 1, 851)
	two := r.addInstance(r.a, 2, 852)
	item := r.hq.AddItem(offA, "release.evaluate", "svc:v1")
	ctx := context.Background()
	if out := r.s.dispatchAgentTool(ctx, one, officers.ToolClaim, `{"item_id":"`+item+`","lease_seconds":60}`); isToolErr(out) {
		t.Fatalf("first claim: %s", out)
	}
	first := *r.hq.Item(item).LeaseExpiresAt
	r.advance(30 * time.Second)

	// The restarted instance claims its own item again.
	r.hq.ResetRequests()
	out := r.s.dispatchAgentTool(ctx, one, officers.ToolClaim, `{"item_id":"`+item+`","lease_seconds":120}`)
	if isToolErr(out) {
		t.Fatalf("re-claiming its own live item: %s, want success", out)
	}
	if !strings.Contains(out, "already yours") || !strings.Contains(out, `"subject_key":"svc:v1"`) {
		t.Fatalf("re-claim result = %s, want the item and an \"already yours\" note", out)
	}
	var sawHeartbeat bool
	for _, q := range r.hq.Requests() {
		if strings.HasSuffix(q.Path, "/items/"+item+"/heartbeat") && q.Body["instance"] == one.Name && q.Body["lease_seconds"] == float64(120) {
			sawHeartbeat = true
		}
	}
	if !sawHeartbeat {
		t.Fatalf("no heartbeat with the requested lease reached HQ: %+v", r.hq.Requests())
	}
	if got := *r.hq.Item(item).LeaseExpiresAt; !got.After(first) {
		t.Fatalf("lease not renewed: %s, was %s", got, first)
	}

	// Another instance claiming it is still refused, naming the holder.
	r.hq.ResetRequests()
	out = r.s.dispatchAgentTool(ctx, two, officers.ToolClaim, `{"item_id":"`+item+`"}`)
	if !isToolErr(out) || !strings.Contains(out, `"status":409`) || !strings.Contains(out, one.Name) {
		t.Fatalf("another instance's claim: %s, want a 409 refusal naming %s", out, one.Name)
	}
	for _, q := range r.hq.Requests() {
		if strings.HasSuffix(q.Path, "/heartbeat") {
			t.Fatal("a claim refused for ANOTHER holder sent a heartbeat")
		}
	}
	if it := r.hq.Item(item); it.ClaimedBy != one.Name {
		t.Fatalf("the holder changed to %q", it.ClaimedBy)
	}
}

// TestAnIdleUpInstanceIsRepromptedAsItsOwnTurn: over the real fleet binding,
// an up instance holding nothing while its officer has an open item is sent a
// re-prompt as a turn of its own (the gateway's Chat, under its session claim).
// No kickoff state is touched (nothing re-armed), nothing is started or
// created, and the claim is released after. While another holder has the
// session claim (a turn running), it is busy: nothing sent.
func TestAnIdleUpInstanceIsRepromptedAsItsOwnTurn(t *testing.T) {
	r := newOfficerRig(t)
	inst := r.addInstance(r.b, 1, 861)
	r.store.mu.Lock()
	r.store.rows[len(r.store.rows)-1].KickedOff = true // its earlier turn was delivered
	r.store.mu.Unlock()
	r.hq.AddItem(offB, "triage.look", "svc:1")
	ctx := context.Background()

	// A turn holds the session: busy, nothing sent, not a failure.
	r.store.ClaimKickoff(ctx, inst.ID, "agentkickoff/turn", time.Hour)
	if res := r.rec.Check(ctx, offB, true); len(res.ActErrs) != 0 || r.chat.count() != 0 {
		t.Fatalf("re-prompt into a running turn: %+v, sent %d", res, r.chat.count())
	}
	r.store.ReleaseKickoffClaim(ctx, inst.ID, "agentkickoff/turn")

	res := r.rec.Check(ctx, offB, true)
	r.s.WaitForOfficerReprompts()
	if len(res.Decision.Reprompt) != 1 || len(res.ActErrs) != 0 || r.chat.count() != 1 {
		t.Fatalf("idle up instance with an open item: %+v, sent %d", res, r.chat.count())
	}
	if sent := r.chat.sent[0]; !strings.Contains(sent, "New work for officer "+offB) || strings.Contains(sent, offBTok) {
		t.Fatalf("re-prompt turn = %q", sent)
	}
	if len(r.store.rearmed) != 0 {
		t.Fatalf("a re-prompt re-armed the kickoff: %v", r.store.rearmed)
	}
	if d, st, _ := r.prov.counts(); d != 0 || st != 0 || len(r.store.created) != 0 {
		t.Fatalf("a re-prompt dispatched/started/created (dispatch %d starts %d creates %d)", d, st, len(r.store.created))
	}
	if won, _ := r.store.ClaimKickoff(ctx, inst.ID, "someone-else", time.Minute); !won {
		t.Fatal("the re-prompt's session claim was not released")
	}
}

// TestAnOfficerInstancePreAllowsExactlyItsOwnTools: the session settings the
// officer runtime hands the provisioner are, for each officer's instance,
// EXACTLY the officer tools it is offered (the release officer's seven, the
// triage officer's six — never another tool, never a built-in) and its
// configured model; a non-officer agent gets nothing.
func TestAnOfficerInstancePreAllowsExactlyItsOwnTools(t *testing.T) {
	r := newOfficerRig(t)
	rel := r.addInstance(r.a, 1, 871)
	tri := r.addInstance(r.b, 1, 872)
	allow, _, ok := r.s.OfficerClaudeCodeSession(rel)
	if want := "officer_brief,officer_items,officer_claim,officer_heartbeat,officer_complete,officer_release,officer_release_evaluation"; !ok || strings.Join(allow, ",") != want {
		t.Fatalf("release officer allow = %v (ok %v), want %s", allow, ok, want)
	}
	allow, _, ok = r.s.OfficerClaudeCodeSession(tri)
	if want := "officer_brief,officer_items,officer_claim,officer_heartbeat,officer_complete,officer_release"; !ok || strings.Join(allow, ",") != want {
		t.Fatalf("triage officer allow = %v (ok %v), want %s", allow, ok, want)
	}
	if allow, model, ok := r.s.OfficerClaudeCodeSession(r.agent(t, officerOrdinaryID)); ok || allow != nil || model != "" {
		t.Fatalf("an ordinary agent got session settings: %v %q %v", allow, model, ok)
	}
	if allow, _, ok := r.s.OfficerClaudeCodeSession(r.agent(t, officerChiefID)); ok || allow != nil {
		t.Fatalf("the chief got officer session settings: %v", allow)
	}
	// The model follows the officer's config.
	r.s.officerRT = nil
	o := r.a
	o.Model = "claude-opus-4-5"
	if _, err := r.s.EnableOfficers(OfficersConfig{Officers: []officers.Officer{o, r.b}, InstallSession: discardSession}); err != nil {
		t.Fatal(err)
	}
	if _, model, _ := r.s.OfficerClaudeCodeSession(rel); model != "claude-opus-4-5" {
		t.Fatalf("model = %q, want the configured claude-opus-4-5", model)
	}
	if _, model, _ := r.s.OfficerClaudeCodeSession(tri); model != "" {
		t.Fatalf("an officer with no model got %q", model)
	}
}

// TestEnableOfficersInstallsTheSessionSourceOrRefuses: the source the
// provisioner is handed IS the officer runtime's (it answers an instance with
// its tools), and EnableOfficers refuses to run officers with no installer —
// an instance would otherwise block on its first permission prompt, silently.
func TestEnableOfficersInstallsTheSessionSourceOrRefuses(t *testing.T) {
	r := newOfficerRig(t)
	inst := r.addInstance(r.a, 1, 881)
	if r.installed == nil {
		t.Fatal("EnableOfficers installed no session source")
	}
	if allow, _, ok := r.installed(inst); !ok || len(allow) != 7 {
		t.Fatalf("installed source answered %v %v for a release instance", allow, ok)
	}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
	s.UseExtensions(Extensions{Agents: &officerAgents{}, Provisioner: stubProvisioner{}, SessionLiveness: stubLiveness{}, Kinds: stubKinds{}})
	if _, err := s.EnableOfficers(OfficersConfig{Officers: []officers.Officer{r.a}}); err == nil || !strings.Contains(err.Error(), "session settings") {
		t.Fatalf("no installer: err = %v, want a refusal", err)
	}
}

// blockingChat is a Chat that blocks until its ctx ends (a turn in flight),
// then answers as an unsent refusal would, or with err when set.
type blockingChat struct {
	stubGateway
	started chan struct{}
	err     error
}

func (g *blockingChat) Chat(ctx context.Context, _ agents.Agent, _, _ string, _ func(string)) (string, error) {
	close(g.started)
	<-ctx.Done()
	return "", ctx.Err()
}

// recordingAgents wraps officerAgents to count transcript rows.
type recordingAgents struct {
	*officerAgents
	rows int
	mu   sync.Mutex
}

func (r *recordingAgents) AddChatMessage(_ context.Context, m agents.ChatMessage) (agents.ChatMessage, error) {
	r.mu.Lock()
	r.rows++
	r.mu.Unlock()
	return m, nil
}

// TestShutdownMidRepromptReleasesTheClaim: a re-prompt send in flight when
// the server shuts down (EndStreams) is cancelled, its session claim is
// released, and WaitForOfficerReprompts returns — rather than the claim staying
// held for the rest of the turn's budget (~39 min) after the process exited,
// which made a stop+start of that instance end in error.
func TestShutdownMidRepromptReleasesTheClaim(t *testing.T) {
	r := newOfficerRig(t)
	inst := r.addInstance(r.b, 1, 891)
	r.store.mu.Lock()
	r.store.rows[len(r.store.rows)-1].KickedOff = true
	r.store.mu.Unlock()
	gw := &blockingChat{started: make(chan struct{})}
	r.s.ext.Gateway = gw
	if err := (officerFleet{s: r.s}).Reprompt(context.Background(), r.b, 1, "new work"); err != nil {
		t.Fatal(err)
	}
	<-gw.started
	r.s.EndStreams()
	done := make(chan struct{})
	go func() { r.s.WaitForOfficerReprompts(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("WaitForOfficerReprompts did not return after shutdown: the send was not cancelled")
	}
	if won, _ := r.store.ClaimKickoff(context.Background(), inst.ID, "after-restart", time.Minute); !won {
		t.Fatal("the session claim is still held after shutdown")
	}
}

// TestAnUnsentRepromptRecordsNoTranscript: a re-prompt the runtime refused
// before typing anything (ccd's 409 busy / 503 not_ready) is not a turn, and
// leaves no "user" row on the agent's transcript; a delivered one leaves both.
func TestAnUnsentRepromptRecordsNoTranscript(t *testing.T) {
	r := newOfficerRig(t)
	r.addInstance(r.b, 1, 892)
	r.store.mu.Lock()
	r.store.rows[len(r.store.rows)-1].KickedOff = true
	r.store.mu.Unlock()
	rec := &recordingAgents{officerAgents: r.store}
	r.s.ext.Agents = rec
	r.s.ext.Gateway = refusingChat{}
	if err := (officerFleet{s: r.s}).Reprompt(context.Background(), r.b, 1, "new work"); err != nil {
		t.Fatal(err)
	}
	r.s.WaitForOfficerReprompts()
	if rec.rows != 0 {
		t.Fatalf("an unsent re-prompt recorded %d transcript rows", rec.rows)
	}
	r.s.ext.Gateway = &repromptChat{}
	if err := (officerFleet{s: r.s}).Reprompt(context.Background(), r.b, 1, "new work"); err != nil {
		t.Fatal(err)
	}
	r.s.WaitForOfficerReprompts()
	if rec.rows != 2 {
		t.Fatalf("a delivered re-prompt recorded %d rows, want 2 (user + reply)", rec.rows)
	}
}

// refusingChat answers like ccd refusing a turn before typing it (409 busy).
type refusingChat struct{ stubGateway }

func (refusingChat) Chat(context.Context, agents.Agent, string, string, func(string)) (string, error) {
	return "", &agents.RuntimeError{Status: http.StatusConflict, Type: "busy", Message: "a turn holds the session"}
}

// TestOfficerCompleteTakesHQsSummaryLimitInCharacters: HQ accepts a summary of
// up to 24,576 CHARACTERS (a shadow officer puts a whole evaluation or report
// there). A 20,000-character summary with multibyte runes — well over the old
// 4,096-byte bound — passes locally and reaches HQ byte for byte; 24,577
// characters is refused locally, before HQ.
func TestOfficerCompleteTakesHQsSummaryLimitInCharacters(t *testing.T) {
	r := newOfficerRig(t)
	one := r.addInstance(r.a, 1, 895)
	ctx := context.Background()
	item := r.hq.AddItem(offA, "release.evaluate", "svc:shadow")
	if out := r.s.dispatchAgentTool(ctx, one, officers.ToolClaim, `{"item_id":"`+item+`"}`); isToolErr(out) {
		t.Fatal(out)
	}
	long := strings.Repeat("é", 10000) + strings.Repeat("x", 10000) // 20,000 chars, 30,000 bytes
	body, _ := json.Marshal(map[string]any{"item_id": item, "outcome": "done", "summary": long})
	if out := r.s.dispatchAgentTool(ctx, one, officers.ToolComplete, string(body)); isToolErr(out) {
		t.Fatalf("a 20,000-character summary was refused: %.200s", out)
	}
	if got := r.hq.Item(item).Summary; got != long {
		t.Fatalf("HQ got a %d-byte summary, want the %d-byte original", len(got), len(long))
	}
	item2 := r.hq.AddItem(offA, "release.evaluate", "svc:shadow2")
	r.s.dispatchAgentTool(ctx, one, officers.ToolClaim, `{"item_id":"`+item2+`"}`)
	r.hq.ResetRequests()
	over, _ := json.Marshal(map[string]any{"item_id": item2, "outcome": "done", "summary": strings.Repeat("é", officers.MaxSummaryChars+1)})
	if out := r.s.dispatchAgentTool(ctx, one, officers.ToolComplete, string(over)); !isToolErr(out) || !strings.Contains(out, "24576") {
		t.Fatalf("a 24,577-character summary: %.200s, want a local refusal naming 24576", out)
	}
	if n := len(r.hq.Requests()); n != 0 {
		t.Fatalf("the oversized summary reached HQ (%d requests)", n)
	}
}

// TestARepromptStartedAfterShutdownSendsNothing: a re-prompt the loop starts
// after EndStreams (its context already cancelled) sends nothing, records no
// transcript row, and releases its claim.
func TestARepromptStartedAfterShutdownSendsNothing(t *testing.T) {
	r := newOfficerRig(t)
	inst := r.addInstance(r.b, 1, 896)
	r.store.mu.Lock()
	r.store.rows[len(r.store.rows)-1].KickedOff = true
	r.store.mu.Unlock()
	rec := &recordingAgents{officerAgents: r.store}
	r.s.ext.Agents = rec
	chat := &repromptChat{}
	r.s.ext.Gateway = chat
	r.s.EndStreams()
	if err := (officerFleet{s: r.s}).Reprompt(context.Background(), r.b, 1, "new work"); err != nil {
		t.Fatal(err)
	}
	r.s.WaitForOfficerReprompts()
	if chat.count() != 0 || rec.rows != 0 {
		t.Fatalf("after shutdown: %d sends, %d transcript rows; want none", chat.count(), rec.rows)
	}
	if won, _ := r.store.ClaimKickoff(context.Background(), inst.ID, "after", time.Minute); !won {
		t.Fatal("claim not released")
	}
}
