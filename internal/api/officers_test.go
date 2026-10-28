package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
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
	key   []byte

	clockMu sync.Mutex
	clock   time.Time
}

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
	s.UseExtensions(Extensions{Agents: store, Provisioner: prov, SessionLiveness: stubLiveness{}, Kinds: stubKinds{pick: "acct-one"}})
	a := officers.Officer{ID: offA, HQURL: hq.BaseURL(), MaxInstances: 2, IdleAfter: 30 * time.Minute, PollInterval: time.Minute, Owner: 1}.WithToken(offATok)
	b := officers.Officer{ID: offB, HQURL: hq.BaseURL(), MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: time.Minute, Owner: 1}.WithToken(offBTok)
	key := []byte("fixture-webhook-key-0123456789ab")
	r := &officerRig{s: s, store: store, prov: prov, hq: hq, logs: logs, a: a, b: b, key: key, clock: time.Now()}
	hq.Now = r.now
	rec, err := s.EnableOfficers(OfficersConfig{Officers: []officers.Officer{a, b}, WebhookKey: key, Now: r.now})
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
	if out := call(one, officers.ToolComplete, `{"item_id":"`+item+`","outcome":"done","summary":"`+strings.Repeat("x", officers.MaxSummaryBytes+1)+`"}`); !strings.Contains(out, "at most") {
		t.Fatalf("an oversized summary: %s", out)
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

func signedWebhook(t *testing.T, key []byte, id string, body string, sig string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/officers/webhook", strings.NewReader(body))
	ts := time.Now().Unix()
	req.Header.Set("webhook-id", id)
	req.Header.Set("webhook-timestamp", strconv.FormatInt(ts, 10))
	if sig == "" {
		sig = officers.Sign(key, id, ts, []byte(body))
	}
	if sig != "none" {
		req.Header.Set("webhook-signature", sig)
	}
	return req
}

// TestTheWebhookWakesAnOfficer: a validly signed officer.item_created for
// officer B wakes it (the running loop starts its instance) well before any
// poll would; a bad or missing signature is 401 and wakes nothing; a replay
// of the same webhook-id is 204 and wakes nothing more.
func TestTheWebhookWakesAnOfficer(t *testing.T) {
	saved := officers.RunTick
	officers.RunTick = time.Hour // no poll: only a nudge can cause a pass
	t.Cleanup(func() { officers.RunTick = saved })
	r := newOfficerRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.rec.Run(ctx)

	r.hq.AddItem(offB, "triage.look", "svc:1")
	body := fmt.Sprintf(`{"type":"officer.item_created","timestamp":"2000-01-01T00:00:00Z","data":{"officer_id":%q,"item_id":1,"kind":"triage.look"}}`, offB)

	for name, req := range map[string]*http.Request{
		"bad signature":     signedWebhook(t, r.key, "msg_bad", body, "v1,AAAA"),
		"missing signature": signedWebhook(t, r.key, "msg_none", body, "none"),
		"wrong key":         signedWebhook(t, []byte("another-key-another-key-another!"), "msg_wk", body, ""),
	} {
		rec := httptest.NewRecorder()
		r.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, rec.Code)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if len(r.store.created) != 0 {
		t.Fatalf("an unverified webhook woke the officer (%d agents created)", len(r.store.created))
	}

	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, signedWebhook(t, r.key, "msg_1", body, ""))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("valid webhook: %d %s", rec.Code, rec.Body.String())
	}
	officerWaitFor(t, func() bool { r.store.mu.Lock(); defer r.store.mu.Unlock(); return len(r.store.created) == 1 })

	// Replay: accepted, no second pass. Count HQ reads before and after.
	time.Sleep(50 * time.Millisecond)
	before := len(r.hq.Requests())
	rec = httptest.NewRecorder()
	r.h.ServeHTTP(rec, signedWebhook(t, r.key, "msg_1", body, ""))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("replay: %d, want 204", rec.Code)
	}
	time.Sleep(100 * time.Millisecond)
	if after := len(r.hq.Requests()); after != before {
		t.Fatalf("a replayed webhook-id caused another pass (%d -> %d HQ requests)", before, after)
	}
	// Control for that measurement: a NEW id does cause a pass.
	r.h.ServeHTTP(httptest.NewRecorder(), signedWebhook(t, r.key, "msg_2", body, ""))
	officerWaitFor(t, func() bool { return len(r.hq.Requests()) > before })
}

// TestTheWebhookIs404WithoutASecret: no secret, no webhook door.
func TestTheWebhookIs404WithoutASecret(t *testing.T) {
	r := newOfficerRig(t)
	r.s.officerRT.webhookKey = nil
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, signedWebhook(t, r.key, "m", `{}`, ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("%d, want 404", rec.Code)
	}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, signedWebhook(t, r.key, "m", `{}`, ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("officers off: %d, want 404", rec.Code)
	}
}

// TestEnableOfficersRefusesWithoutClaudeCode: officers need the claude-code kind.
func TestEnableOfficersRefusesWithoutClaudeCode(t *testing.T) {
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
	s.UseExtensions(Extensions{Agents: &officerAgents{}, Provisioner: stubProvisioner{}, SessionLiveness: stubLiveness{}})
	o := officers.Officer{ID: offA, HQURL: "http://hq.invalid/api/v1", MaxInstances: 1}.WithToken("x")
	if _, err := s.EnableOfficers(OfficersConfig{Officers: []officers.Officer{o}}); err == nil || !strings.Contains(err.Error(), "claude-code") {
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
// instance posts its evaluation to HQ's captain route with its own token, the
// body passed through and its instance set by muster (an instance field the
// agent sends is overwritten); a non-release officer's instance is neither
// offered the tool nor able to call it.
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
	out := r.s.dispatchAgentTool(ctx, rel, officers.ToolReleaseEvaluation, `{"evaluation":{"release_id":42,"risk":"low","summary":"two PRs, both reviewed","instance":"spoofed"}}`)
	if isToolErr(out) {
		t.Fatalf("release evaluation: %s", out)
	}
	if len(r.hq.Evaluations) != 1 {
		t.Fatalf("captain route got %d posts, want 1", len(r.hq.Evaluations))
	}
	ev := r.hq.Evaluations[0]
	if ev.Officer != offA || ev.Body["release_id"] != float64(42) || ev.Body["risk"] != "low" || ev.Body["instance"] != rel.Name {
		t.Fatalf("captain route recorded %+v, want officer %s, the body passed through, instance %s", ev, offA, rel.Name)
	}
	reqs := r.hq.Requests()
	if last := reqs[len(reqs)-1]; last.Path != "/api/v1/releases/captain" || last.Auth != "Bearer "+offATok {
		t.Fatalf("captain post went to %s with %q", last.Path, last.Auth)
	}
	r.hq.ResetRequests()
	out = r.s.dispatchAgentTool(ctx, triage, officers.ToolReleaseEvaluation, `{"evaluation":{"release_id":1}}`)
	if !isToolErr(out) || !strings.Contains(out, "not one of officer "+offB+"'s tools") {
		t.Fatalf("triage officer calling the captain tool: %s", out)
	}
	if len(r.hq.Requests()) != 0 {
		t.Fatal("a refused captain post reached HQ")
	}
	if out := r.s.dispatchAgentTool(ctx, rel, officers.ToolReleaseEvaluation, `{"evaluation":{}}`); !strings.Contains(out, "evaluation is required") {
		t.Fatalf("empty evaluation: %s", out)
	}
}
