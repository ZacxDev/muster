package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agentkickoff"
	"github.com/ZacxDev/muster/internal/agentoutcome"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/ui"
)

// EVERY DISPATCHED AGENT ENDS IN A VISIBLE OUTCOME (agent_outcome.go,
// agent_watchdog.go, model_validate.go), against the real database: the chief's
// agent_dispatch, the real kickoff deliverer over the real gateway transport to
// a fake RUNTIME (an httptest server speaking /v1/responses), the real push
// pipeline to a recorded device, and the chief's real chat.

// incidentRuntimeFailure is the runtime's sentence in the incident's shape: a
// model it does not know, the bare name read as a model of its default
// provider. The vendor is synthetic.
const incidentRuntimeFailure = "FailoverError: Unknown model: openai/Acme v9 ultra"

type outcomeFixture struct {
	*taskRouteFixture
	pusher *payloadPusher
	pushes *memPush
	prefix string
	clock  *outcomeClock
}

// outcomeClock is the server's clock (Server.now), moved by the test.
type outcomeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *outcomeClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *outcomeClock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }
func (c *outcomeClock) Set(t time.Time)     { c.mu.Lock(); c.t = t; c.mu.Unlock() }
func newOutcomeClock() *outcomeClock {
	return &outcomeClock{t: time.Now().UTC().Truncate(time.Microsecond)}
}
func (o *outcomeFixture) ownerDevice() string { return ep("outcome-owner") }

func newOutcomeFixture(t *testing.T, cat ModelCatalog) *outcomeFixture {
	t.Helper()
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	o := &outcomeFixture{taskRouteFixture: f, pusher: &payloadPusher{}, pushes: &memPush{},
		prefix: "outc" + strconv.FormatInt(time.Now().UnixNano()%1e9, 36) + "-", clock: newOutcomeClock()}
	f.srv.ext.ModelCatalog = cat
	f.srv.ext.Outcomes = agentoutcome.NewPG(f.pool)
	// The fixture's own namespace prefix, so the kickoff deliverer below owns
	// exactly the agents this test dispatched and no other test's rows.
	f.srv.ext.AgentNamespacePrefix = o.prefix
	f.srv.ext.Push = push.NewService(o.pushes, o.pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))
	f.srv.now = o.clock.Now
	claim(t, o.pushes, f.owner, o.ownerDevice())
	return o
}

// outcomePushes is every outcome push the owner's device received for agent id.
func (o *outcomeFixture) outcomePushes(id int64) []push.Payload {
	o.srv.WaitForPushes()
	var out []push.Payload
	for _, p := range o.pusher.to(o.ownerDevice()) {
		if p.Tag == "agent-"+strconv.FormatInt(id, 10)+"-outcome" {
			out = append(out, p)
		}
	}
	return out
}

func (o *outcomeFixture) agent(t *testing.T, id int64) agents.Agent {
	t.Helper()
	a, err := agents.NewPG(o.pool).Get(o.asOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// chiefNotes is muster's notes about agent name in the chief's chat.
func (o *outcomeFixture) chiefNotes(t *testing.T, name string) []string {
	t.Helper()
	rows, err := o.pool.Query(o.asOwner, `SELECT content FROM chat_messages WHERE agent_id = $1 AND role = 'user' ORDER BY id`, o.chief.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(c, "[muster] Agent "+name+" ") {
			out = append(out, c)
		}
	}
	return out
}

// dispatchGateway starts a gateway agent on a fresh task through the chief's
// agent_dispatch, with model, and returns it.
func (o *outcomeFixture) dispatchGateway(t *testing.T, title, model string) agents.Agent {
	t.Helper()
	n := o.task(t, title)
	return o.startedOne(t, o.callRaw(o.chief, agentDispatchTool, map[string]any{"target": "pod/gateway", "task_id": n.ID,
		"model": model}), n.ID)
}

// outcomeEndpoint resolves every agent to the fake runtime.
type outcomeEndpoint struct{ ep provision.Endpoint }

func (e outcomeEndpoint) Endpoint(context.Context, provision.Ref) (provision.Endpoint, error) {
	return e.ep, nil
}

type readyInstances struct{ names []string }

func (r readyInstances) Instances(context.Context) ([]provision.Instance, error) {
	var out []provision.Instance
	for _, n := range r.names {
		out = append(out, provision.Instance{Ref: provision.Ref{Name: n}, InstanceID: n + "-pod", Phase: provision.PhaseRunning,
			Ready: true, Replicas: 1})
	}
	return out, nil
}

// sse is one /v1/responses stream event.
func sseEvent(typ string, data map[string]any) string {
	data["type"] = typ
	b, _ := json.Marshal(data)
	return "event: " + typ + "\ndata: " + string(b) + "\n\n"
}

// fakeRuntime is an agent runtime answering every /v1/responses turn with
// stream (the events, written in order).
func fakeRuntime(t *testing.T, stream func() string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/responses") {
			http.NotFound(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(stream()))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// kickoff runs ONE tick of the real kickoff deliverer for agents, over the real
// gateway to runtime, reporting to the server's outcome tracker.
func (o *outcomeFixture) kickoff(t *testing.T, runtime *httptest.Server, list ...agents.Agent) {
	t.Helper()
	o.kickoffWithin(t, runtime, 0, list...)
}

// kickoffWithin is kickoff with a first-turn budget (0: the deliverer's default).
func (o *outcomeFixture) kickoffWithin(t *testing.T, runtime *httptest.Server, budget time.Duration, list ...agents.Agent) {
	t.Helper()
	u, _ := url.Parse(runtime.URL)
	host, port, _ := net.SplitHostPort(u.Host)
	p, _ := strconv.Atoi(port)
	gw, err := agentgateway.New(agentgateway.Config{Driver: outcomeEndpoint{provision.Endpoint{Scheme: "http", Host: host, Port: p}},
		Runtime: agentgateway.HooksSHA256(), Model: "runtime-sentinel"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	store := agents.NewPG(o.pool)
	for _, a := range list {
		names = append(names, a.Name)
		// The token the provisioner mints at creation (recordingProvisioner does not).
		if err := store.SetHooksToken(authz.System(context.Background()), a.ID, "outcome-token-"+a.Name); err != nil {
			t.Fatal(err)
		}
	}
	rep, ok := o.srv.KickoffOutcomes()
	if !ok {
		t.Fatal("instrument check FAILED: the server tracks no outcomes")
	}
	d, err := agentkickoff.New(agentkickoff.Config{Store: store, Instances: readyInstances{names}, Gateway: gw,
		NamespacePrefix: o.prefix, Outcome: rep, TurnTimeout: budget})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Tick(authz.System(context.Background())); err != nil {
		t.Fatal(err)
	}
	d.Wait()
}

// --- 1. the model is checked at dispatch -----------------------------------------

// TestAgentDispatchRefusesAnUnknownModelBeforeAnyPod: a model the catalogue does
// not have is refused with the closest ids, and nothing is reserved, created or
// provisioned; the incident's shape — a DISPLAY NAME — is resolved to its id (the
// agent row stores the id), and the reservation's audit detail records the model
// and the value as given.
//
// 🔴 RED BEFORE THIS CHANGE: the unknown model started an agent, the display
// name was stored as typed, and the audit detail had no model.
func TestAgentDispatchRefusesAnUnknownModelBeforeAnyPod(t *testing.T) {
	o := newOutcomeFixture(t, stubModelCatalog{})
	n := o.task(t, "an unknown model")
	text := o.callRaw(o.chief, agentDispatchTool, map[string]any{"target": "pod/gateway", "task_id": n.ID, "model": "Acme v9 ultra"})
	o.mustRefuse(t, text, `model \"Acme v9 ultra\" is not in the model catalogue`, n.ID)
	var refusal struct {
		Suggestions []string `json:"model_suggestions"`
		Budget      any      `json:"budget"`
	}
	if err := json.Unmarshal([]byte(text), &refusal); err != nil || len(refusal.Suggestions) == 0 ||
		len(refusal.Suggestions) > modelSuggestionsMax || refusal.Budget == nil {
		t.Fatalf("refusal %s: want 1-%d model_suggestions and the budget", text, modelSuggestionsMax)
	}

	got := o.dispatchGateway(t, "the incident's display name", "Acme v4.1 flash")
	if got.Model != "openrouter/acme/acme-v4.1-flash" {
		t.Fatalf("a display name was stored as %q; want the catalogue id", got.Model)
	}
	d := o.reservationDetail(t, *got.NoteID)
	if d["model"] != "openrouter/acme/acme-v4.1-flash" || d["model_input"] != "Acme v4.1 flash" {
		t.Fatalf("audit detail %v: want model and model_input", d)
	}
	// An exact id records the model and no model_input.
	exact := o.dispatchGateway(t, "an exact id", "openrouter/acme/acme-v4.1-pro")
	if d := o.reservationDetail(t, *exact.NoteID); d["model"] != "openrouter/acme/acme-v4.1-pro" || d["model_input"] != nil {
		t.Fatalf("audit detail %v: want the model and no model_input", d)
	}
	// The started agent is tracked, naming the chief that started it.
	if got.Outcome != agentoutcome.Running || got.DispatchedBy == nil || *got.DispatchedBy != o.chief.ID {
		t.Errorf("started agent: outcome %q dispatched_by %v; want running by the chief %d", got.Outcome, got.DispatchedBy, o.chief.ID)
	}
}

// TestATasksOwnModelIsCheckedAtDispatch: a gateway start that names no model
// takes the TASK's; an unknown one is refused under the reservation's lock
// (nothing created), and a known one is recorded in the audit detail.
func TestATasksOwnModelIsCheckedAtDispatch(t *testing.T) {
	o := newOutcomeFixture(t, stubModelCatalog{})
	bad, err := o.notes.Create(o.asOwner, notes.Note{Title: "bad model", Body: "Do it.", Model: "Acme v9 ultra"})
	if err != nil {
		t.Fatal(err)
	}
	o.mustRefuse(t, o.callRaw(o.chief, agentDispatchTool, map[string]any{"target": "pod/gateway", "task_id": bad.ID}),
		"the task's model: model \\\"Acme v9 ultra\\\" is not in the model catalogue", bad.ID)
	good, err := o.notes.Create(o.asOwner, notes.Note{Title: "good model", Body: "Do it.", Model: "Acme v4.1 pro"})
	if err != nil {
		t.Fatal(err)
	}
	a := o.startedOne(t, o.callRaw(o.chief, agentDispatchTool, map[string]any{"target": "pod/gateway", "task_id": good.ID}), good.ID)
	if a.Model != "openrouter/acme/acme-v4.1-pro" {
		t.Errorf("the task's model was stored as %q", a.Model)
	}
	if d := o.reservationDetail(t, good.ID); d["model"] != "openrouter/acme/acme-v4.1-pro" {
		t.Errorf("audit detail %v: want the task's model", d)
	}
}

// TestTheAgentsPageModelChangeIsChecked: POST /agents/{id}/model refuses an
// unknown model (400, the closest ids in the text) and stores a display name as
// its id.
func TestTheAgentsPageModelChangeIsChecked(t *testing.T) {
	o := newOutcomeFixture(t, stubModelCatalog{})
	a := o.dispatchGateway(t, "model change", "openrouter/acme/acme-v4.1-pro")
	post := func(model string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/agents/"+strconv.FormatInt(a.ID, 10)+"/model",
			strings.NewReader(url.Values{"model": {model}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.SetPathValue("id", strconv.FormatInt(a.ID, 10))
		r = r.WithContext(o.asOwner)
		w := httptest.NewRecorder()
		o.srv.handleAgentModel(w, r)
		return w
	}
	if w := post("Acme v9 ultra"); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "Closest ids:") {
		t.Fatalf("unknown model: %d %s; want 400 with the closest ids", w.Code, w.Body)
	}
	if got := o.agent(t, a.ID).Model; got != "openrouter/acme/acme-v4.1-pro" {
		t.Fatalf("a refused change stored %q", got)
	}
	if w := post("Acme v4.1 flash"); w.Code != http.StatusOK {
		t.Fatalf("display name: %d %s", w.Code, w.Body)
	}
	if got := o.agent(t, a.ID).Model; got != "openrouter/acme/acme-v4.1-flash" {
		t.Fatalf("a display name was stored as %q; want its id", got)
	}
}

// --- 2. a failed turn is an error, a push and a note to the chief -----------------

// TestAFailedKickoffTurnIsAnErrorAPushAndANoteToTheChief REPRODUCES THE INCIDENT:
// the chief dispatches a gateway agent on a model the runtime does not know (the
// catalogue is unreachable, so an id-shaped value passes unchecked, as decided),
// and the runtime's first turn fails mid-stream with "Unknown model". The agent
// must end `error` with the runtime's sentence, the owner's device must get ONE
// "<agent> failed: …" push deep-linking to the agent, and the chief's chat must
// get one note it can act on. A later failure of the same agent pushes nothing.
//
// 🔴 RED BEFORE THIS CHANGE: status stayed `running`, error_message empty, no
// push, no note — exactly what the owner found hours later.
func TestAFailedKickoffTurnIsAnErrorAPushAndANoteToTheChief(t *testing.T) {
	o := newOutcomeFixture(t, stubModelCatalog{err: errCatalogUnavailable})
	a := o.dispatchGateway(t, "the incident", "openrouter/acme/acme-v9-ultra")
	runtime := fakeRuntime(t, func() string {
		return sseEvent("response.created", map[string]any{"response": map[string]any{"status": "in_progress"}}) +
			sseEvent("response.failed", map[string]any{"response": map[string]any{"status": "failed",
				"error": map[string]any{"code": "server_error", "message": incidentRuntimeFailure}}})
	})
	o.kickoff(t, runtime, a)

	got := o.agent(t, a.ID)
	if got.Status != agents.StatusError || !strings.Contains(got.ErrorMessage, incidentRuntimeFailure) {
		t.Fatalf("after the failed first turn: status %q error_message %q; want error with the runtime's sentence",
			got.Status, got.ErrorMessage)
	}
	if got.Outcome != agentoutcome.Failed || !strings.Contains(got.OutcomeReason, incidentRuntimeFailure) {
		t.Errorf("outcome %q reason %q; want failed with the runtime's sentence", got.Outcome, got.OutcomeReason)
	}
	ps := o.outcomePushes(a.ID)
	if len(ps) != 1 {
		t.Fatalf("%d outcome pushes; want exactly 1", len(ps))
	}
	if !strings.HasPrefix(ps[0].Title, agentLabel(a)+" failed: ") || !strings.Contains(ps[0].Title, "Unknown model") ||
		ps[0].Data["url"] != "/agents/"+a.Name {
		t.Errorf("push %+v; want \"<agent> failed: <reason>\" linking to /agents/%s", ps[0], a.Name)
	}
	notes := o.chiefNotes(t, a.Name)
	if len(notes) != 1 || !strings.Contains(notes[0], "FAILED") || !strings.Contains(notes[0], incidentRuntimeFailure) ||
		!strings.Contains(notes[0], "agent_dispatch") {
		t.Fatalf("the chief's notes about %s: %q; want one FAILED note with the reason and the way to re-dispatch", a.Name, notes)
	}

	// The same agent failing again (a later turn) records the newer reason and
	// pushes nothing more.
	o.srv.outcomeFailed(context.Background(), o.agent(t, a.ID), "a turn failed: "+incidentRuntimeFailure+" (again)")
	if n := len(o.outcomePushes(a.ID)); n != 1 {
		t.Errorf("a repeated failure pushed again: %d pushes", n)
	}
	if n := len(o.chiefNotes(t, a.Name)); n != 1 {
		t.Errorf("a repeated failure noted again: %d notes", n)
	}
	// The outcome API says it.
	out := o.srv.AgentOutcomeOf(o.agent(t, a.ID), time.Time{})
	if out.Status != agents.StatusError || out.Outcome != agentoutcome.Failed || !strings.Contains(out.Reason, "Unknown model") ||
		out.DispatchedBy == nil || *out.DispatchedBy != o.chief.ID {
		t.Errorf("AgentOutcomeOf = %+v", out)
	}
}

// failingGateway answers every chat turn with err.
type failingGateway struct {
	stubGateway
	err error
}

func (g failingGateway) ChatWithTools(ctx context.Context, _ agents.Agent, _, _, _ string, _ []agents.ToolDef, _ agents.ToolDispatch, _ agents.StreamEmit) (string, error) {
	agents.MarkTurnActivity(ctx)
	return "", g.err
}

// TestAFailedChatTurnIsAnErrorAndAPush: a later turn (the chat door's chatTurn)
// failing the same way sets `error` and pushes once; a typed busy refusal, a
// connection that could not be opened and the caller hanging up do not.
func TestAFailedChatTurnIsAnErrorAndAPush(t *testing.T) {
	o := newOutcomeFixture(t, stubModelCatalog{})
	a := o.dispatchGateway(t, "chat failure", "openrouter/acme/acme-v4.1-pro")
	sess, err := agents.NewPG(o.pool).LatestOrCreateSession(o.asOwner, a.ID, a.Name)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		err  error
	}{
		{"busy", &agents.RuntimeError{Status: http.StatusConflict, Type: "busy"}},
		{"not ready", &agents.RuntimeError{Status: http.StatusServiceUnavailable, Type: "not_ready"}},
		{"rate limited", &agents.RuntimeError{Status: http.StatusConflict, Type: "rate_limited"}},
		{"dial", &net.OpError{Op: "dial", Err: errors.New("connection refused")}},
		// The turn outlived its budget: the runtime may still be running it.
		{"budget exceeded", fmt.Errorf("post responses: %w", context.DeadlineExceeded)},
	} {
		o.srv.ext.Gateway = failingGateway{err: c.err}
		_, _ = o.srv.chatTurn(o.asOwner, o.agent(t, a.ID), sess.SessionKey, "hello", nil)
		if got := o.agent(t, a.ID); got.Status == agents.StatusError || got.Outcome == agentoutcome.Failed {
			t.Errorf("%s: status %q outcome %q; want not failed", c.name, got.Status, got.Outcome)
		}
	}
	cancelled, cancel := context.WithCancel(o.asOwner)
	cancel()
	o.srv.ext.Gateway = failingGateway{err: context.Canceled}
	_, _ = o.srv.chatTurn(cancelled, o.agent(t, a.ID), sess.SessionKey, "hello", nil)
	if got := o.agent(t, a.ID); got.Outcome == agentoutcome.Failed {
		t.Errorf("the caller hanging up failed the agent")
	}
	if n := len(o.outcomePushes(a.ID)); n != 0 {
		t.Fatalf("instrument check: %d pushes before the real failure", n)
	}
	o.srv.ext.Gateway = failingGateway{err: &agents.StreamError{Message: incidentRuntimeFailure}}
	for i := 0; i < 2; i++ {
		if _, err := o.srv.chatTurn(o.asOwner, o.agent(t, a.ID), sess.SessionKey, "hello", nil); err == nil {
			t.Fatal("the failing turn returned no error")
		}
	}
	got := o.agent(t, a.ID)
	if got.Status != agents.StatusError || !strings.Contains(got.ErrorMessage, incidentRuntimeFailure) {
		t.Fatalf("after a failed chat turn: status %q error %q", got.Status, got.ErrorMessage)
	}
	if n := len(o.outcomePushes(a.ID)); n != 1 {
		t.Fatalf("two failures pushed %d times; want 1", n)
	}
}

// TestTheChiefIsNotSetToErrorByAFailedTurn: the chief's failure is pushed and
// shown as its outcome, but its status — which gates every delivery to it — is
// left alone; its next good turn clears the failure.
func TestTheChiefIsNotSetToErrorByAFailedTurn(t *testing.T) {
	o := newOutcomeFixture(t, stubModelCatalog{})
	o.srv.outcomeFailed(o.asOwner, o.chief, "a turn failed: boom")
	got := o.agent(t, o.chief.ID)
	if got.Status != agents.StatusRunning || got.Outcome != agentoutcome.Failed {
		t.Fatalf("chief after a failure: status %q outcome %q; want running / failed", got.Status, got.Outcome)
	}
	if n := len(o.outcomePushes(o.chief.ID)); n != 1 {
		t.Errorf("chief failure pushed %d times; want 1", n)
	}
	o.srv.outcomeAnswered(o.asOwner, got, "all good")
	if got := o.agent(t, o.chief.ID); got.Outcome != "" {
		t.Errorf("chief after a good turn: outcome %q; want cleared", got.Outcome)
	}
	if n := len(o.outcomePushes(o.chief.ID)); n != 1 {
		t.Errorf("the chief's recovery pushed: %d", n)
	}
}

// --- 4. a finished agent reports back -------------------------------------------

// TestAnIdleReplyIsWaitingNeverDone (round 1's 🔴, reproduced): a first turn
// that ends with a QUESTION — the real kickoff over the real transport — leaves
// the agent WAITING on its owner: one push "<agent> is waiting for you: <the
// reply's LAST line>" and one chief note quoting its last lines; never done,
// never "finished". A turn that ends on attention_ask with no reply text is
// waiting too. A reply while its pod still reports work is neither (mid-work),
// and its work report clearing makes it waiting.
//
// 🔴 RED AT THE PREVIOUS HEAD: the question read as done, pushed "finished:
// <first line>", told the chief "Tell your owner it is done", and the agent was
// never watched again.
func TestAnIdleReplyIsWaitingNeverDone(t *testing.T) {
	o := newOutcomeFixture(t, stubModelCatalog{})
	a := o.dispatchGateway(t, "asks a question", "openrouter/acme/acme-v4.1-pro")
	const reply = "I looked at the task.\nThere are two repositories that could hold it.\nWhich repository should I work in?"
	runtime := fakeRuntime(t, func() string {
		return sseEvent("response.completed", map[string]any{"response": map[string]any{"status": "completed",
			"output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": reply}}}}}})
	})
	o.kickoff(t, runtime, a)
	got := o.agent(t, a.ID)
	if got.Outcome != agentoutcome.Waiting || got.OutcomeReason != "Which repository should I work in?" {
		t.Fatalf("a question: outcome %q reason %q; want waiting with its LAST line", got.Outcome, got.OutcomeReason)
	}
	o.tellWaits(t) // told once nobody has replied or looked through the hold
	ps := o.outcomePushes(a.ID)
	if len(ps) != 1 || ps[0].Title != agentLabel(a)+" is waiting for you: Which repository should I work in?" {
		t.Fatalf("pushes %+v; want one \"<agent> is waiting for you: <last line>\"", ps)
	}
	notes := o.chiefNotes(t, a.Name)
	if len(notes) != 1 || !strings.Contains(notes[0], "WAITING FOR YOUR OWNER") || !strings.Contains(notes[0], "Which repository should I work in?") ||
		!strings.Contains(notes[0], "There are two repositories") || strings.Contains(notes[0], "it is done") {
		t.Fatalf("chief notes %q; want one waiting note quoting the last lines, never 'done'", notes)
	}
	// The card says "waiting for you", from the same outcome.
	if state, _, ok := o.srv.outcomeCardState(got); !ok || state != ui.DispatchWaiting {
		t.Errorf("card state %q (%v); want waiting", state, ok)
	}

	// attention_ask, then the turn ends with no text: waiting, not done.
	b := o.dispatchGateway(t, "asks on attention", "openrouter/acme/acme-v4.1-pro")
	sess, err := agents.NewPG(o.pool).LatestOrCreateSession(o.asOwner, b.ID, b.Name)
	if err != nil {
		t.Fatal(err)
	}
	o.srv.ext.Gateway = askingGateway{}
	if _, err := o.srv.chatTurn(o.asOwner, o.agent(t, b.ID), sess.SessionKey, "go", nil); err != nil {
		t.Fatal(err)
	}
	if got := o.agent(t, b.ID); got.Outcome != agentoutcome.Waiting {
		t.Fatalf("after attention_ask and an empty end: outcome %q; want waiting", got.Outcome)
	}

	// Still working when it replies: running, until the work report clears.
	c := o.dispatchGateway(t, "keeps working", "openrouter/acme/acme-v4.1-pro")
	o.srv.agentWork.mu.Lock()
	if o.srv.agentWork.m == nil {
		o.srv.agentWork.m = map[int64]agentWorkReport{}
	}
	o.srv.agentWork.m[c.ID] = agentWorkReport{work: mirror.WindowWork{Subagents: 2}, seen: o.clock.Now()}
	o.srv.agentWork.mu.Unlock()
	o.srv.outcomeAnswered(o.asOwner, o.agent(t, c.ID), "Started two subagents.")
	if got := o.agent(t, c.ID); got.Outcome != agentoutcome.Running {
		t.Fatalf("answered while working: outcome %q; want still running", got.Outcome)
	}
	o.srv.agentWork.mu.Lock()
	delete(o.srv.agentWork.m, c.ID)
	o.srv.agentWork.mu.Unlock()
	o.srv.outcomeWorkReported(o.asOwner, o.agent(t, c.ID), true)
	if got := o.agent(t, c.ID); got.Outcome != agentoutcome.Waiting {
		t.Fatalf("after its work cleared: outcome %q; want waiting", got.Outcome)
	}
}

// askingGateway plays a turn that calls attention_ask through the real tool
// dispatch and then ends with no reply text.
type askingGateway struct{ stubGateway }

func (askingGateway) ChatWithTools(ctx context.Context, _ agents.Agent, _, _, _ string, _ []agents.ToolDef, dispatch agents.ToolDispatch, _ agents.StreamEmit) (string, error) {
	_ = dispatch("attention_ask", `{"question":"Which repository should I work in?"}`)
	return "", nil
}

// TestDoneIsOnlyAnExplicitSignal: an agent's work is done only when it says so —
// agent_report_done (one push "<agent> finished: <summary>", a chief note), or
// moving its task to review (the review push already says it: no second push)
// or complete (pushed). The owner moving the card finishes nothing. A new turn
// on a done (or waiting) agent puts it back to running, so it is watched again.
func TestDoneIsOnlyAnExplicitSignal(t *testing.T) {
	o := newOutcomeFixture(t, stubModelCatalog{})
	a := o.dispatchGateway(t, "reports done", "openrouter/acme/acme-v4.1-pro")
	out := o.srv.toolAgentReportDone(o.asOwner, o.agent(t, a.ID), `{"summary":"Opened draft PR #12 with the fix.\nmore"}`)
	if !strings.Contains(out, `"outcome":"done"`) {
		t.Fatalf("agent_report_done answered %s", out)
	}
	if got := o.agent(t, a.ID); got.Outcome != agentoutcome.Done {
		t.Fatalf("after agent_report_done: outcome %q", got.Outcome)
	}
	if ps := o.outcomePushes(a.ID); len(ps) != 1 || ps[0].Title != agentLabel(a)+" finished: Opened draft PR #12 with the fix." {
		t.Fatalf("pushes %+v; want one finished push with the summary", ps)
	}
	if n := o.chiefNotes(t, a.Name); len(n) != 1 || !strings.Contains(n[0], "finished: Opened draft PR #12") {
		t.Fatalf("chief notes %q", n)
	}
	if out := o.srv.toolAgentReportDone(o.asOwner, o.agent(t, a.ID), `{"summary":"again"}`); !strings.Contains(out, "Already") ||
		len(o.outcomePushes(a.ID)) != 1 {
		t.Errorf("a second report pushed again or said otherwise: %s", out)
	}
	if out := o.srv.toolAgentReportDone(o.asOwner, o.agent(t, a.ID), `{}`); !strings.Contains(out, `"error"`) {
		t.Errorf("no summary: %s", out)
	}

	// Done → a new turn → running (watched again).
	o.srv.outcomeTurnStarted(o.asOwner, o.agent(t, a.ID))
	if got := o.agent(t, a.ID); got.Outcome != agentoutcome.Running {
		t.Fatalf("a new turn on a done agent: outcome %q; want running", got.Outcome)
	}
	o.srv.agentOutcomes.open(a.ID, -1)

	// Its task to review: done, and the review push is the only one.
	r := o.dispatchGateway(t, "to review", "openrouter/acme/acme-v4.1-pro")
	if _, err := o.srv.setNoteStatus(withRefOrigin(o.asOwner, refOrigin{Agent: r.Name}), writerAgentTool, *r.NoteID, notes.StatusReadyForReview); err != nil {
		t.Fatal(err)
	}
	if got := o.agent(t, r.ID); got.Outcome != agentoutcome.Done {
		t.Fatalf("after the agent moved its task to review: outcome %q", got.Outcome)
	}
	if n := len(o.outcomePushes(r.ID)); n != 0 {
		t.Errorf("review: %d agent_outcome pushes beside the review push; want 0", n)
	}
	// To complete (no review push): done, pushed.
	c := o.dispatchGateway(t, "to complete", "openrouter/acme/acme-v4.1-pro")
	if _, err := o.srv.setNoteStatus(withRefOrigin(o.asOwner, refOrigin{Agent: c.Name}), writerTaskTool, *c.NoteID, notes.StatusComplete); err != nil {
		t.Fatal(err)
	}
	if ps := o.outcomePushes(c.ID); o.agent(t, c.ID).Outcome != agentoutcome.Done || len(ps) != 1 ||
		!strings.Contains(ps[0].Title, "moved its task to complete") {
		t.Errorf("complete: outcome %q pushes %+v", o.agent(t, c.ID).Outcome, ps)
	}
	// Control: the OWNER moving a card finishes nothing.
	d := o.dispatchGateway(t, "owner moves it", "openrouter/acme/acme-v4.1-pro")
	if _, err := o.srv.setNoteStatus(o.asOwner, writerHumanUI, *d.NoteID, notes.StatusReadyForReview); err != nil {
		t.Fatal(err)
	}
	if got := o.agent(t, d.ID); got.Outcome != agentoutcome.Running {
		t.Errorf("the owner moving the card finished the agent: %q", got.Outcome)
	}
}

// TestOnlyADispatchedAgentIsSetToError (round 1, 🟡2): a failed turn of an
// agent nobody dispatched (a saved agent Started by hand, an officer's
// instance) changes nothing; and an agent list that cannot be read makes the
// failure "maybe the chief" — pushed, never an error status (fail safe).
func TestOnlyADispatchedAgentIsSetToError(t *testing.T) {
	o := newOutcomeFixture(t, stubModelCatalog{})
	saved, err := agents.NewPG(o.pool).Create(o.asOwner, agents.Agent{Name: "saved-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Namespace: "ns-saved", Status: agents.StatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	o.srv.outcomeFailed(o.asOwner, saved, "a turn failed: boom")
	if got := o.agent(t, saved.ID); got.Status != agents.StatusRunning || got.Outcome != "" || len(o.outcomePushes(saved.ID)) != 0 {
		t.Fatalf("an untracked agent's failure: status %q outcome %q pushes %d; want nothing", got.Status, got.Outcome, len(o.outcomePushes(saved.ID)))
	}
	a := o.dispatchGateway(t, "list unreadable", "openrouter/acme/acme-v4.1-pro")
	real := o.srv.ext.Agents
	o.srv.ext.Agents = listFails{real}
	o.srv.outcomeFailed(o.asOwner, o.agent(t, a.ID), "a turn failed: boom")
	o.srv.ext.Agents = real
	if got := o.agent(t, a.ID); got.Status == agents.StatusError || got.Outcome != agentoutcome.Failed {
		t.Fatalf("unreadable list: status %q outcome %q; want no error status, failure recorded", got.Status, got.Outcome)
	}
}

// listFails is an agents store whose List fails.
type listFails struct{ agents.Store }

func (listFails) List(context.Context) ([]agents.Agent, error) { return nil, errors.New("list down") }

// --- 3. the silence watchdog ------------------------------------------------------

// kickedOff marks a's first turn delivered at the clock's now, as the deliverer
// does (the stamp, then TurnStarted).
func (o *outcomeFixture) kickedOff(t *testing.T, a agents.Agent, inFlight bool) agents.Agent {
	t.Helper()
	if err := agents.NewPG(o.pool).SetKickedOff(authz.System(context.Background()), a.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := o.pool.Exec(authz.System(context.Background()), `UPDATE agents SET status = 'running' WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	fresh := o.agent(t, a.ID)
	rep, _ := o.srv.KickoffOutcomes()
	rep.TurnStarted(o.asOwner, fresh)
	if !inFlight {
		// The first turn is no longer open in this process — it ended without a
		// reply, or muster restarted under it: only then can the agent go quiet.
		rep.TurnEnded(o.asOwner, fresh)
	}
	return o.agent(t, a.ID)
}

// tellWaits moves the clock past the waiting hold and sweeps: an untold wait
// is told (pushed, noted) then — never at the moment it began.
func (o *outcomeFixture) tellWaits(t *testing.T) {
	t.Helper()
	o.clock.Add(EpisodeWaitingGrace + time.Second)
	o.sweep(t)
}

func (o *outcomeFixture) sweep(t *testing.T) int {
	t.Helper()
	n, err := o.srv.AgentWatchdogPass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestTheWatchdogCallsAQuietAgentStuckOncePerChange, with a fake clock: no first
// reply 5 min after the kickoff is stuck ("no reply since kickoff") — one push,
// one note — and a second sweep says nothing more; activity recovers it; a
// working pod is never flipped; once it replied, 30 min of silence is stuck
// ("silent for 30 min"), again once. Each threshold is measured a second short
// and a second past.
//
// 🔴 RED BEFORE THIS CHANGE: nothing ever called a quiet agent anything.
func TestTheWatchdogCallsAQuietAgentStuckOncePerChange(t *testing.T) {
	o := newOutcomeFixture(t, stubModelCatalog{})
	th := agentoutcome.DefaultThresholds()
	a := o.kickedOff(t, o.dispatchGateway(t, "goes quiet", "openrouter/acme/acme-v4.1-pro"), false)
	start := o.clock.Now()

	o.clock.Set(start.Add(th.NoReply - time.Second))
	if n := o.sweep(t); n != 0 {
		t.Fatalf("a second short of %s: %d stuck", th.NoReply, n)
	}
	o.clock.Set(start.Add(th.NoReply + time.Second))
	if n := o.sweep(t); n != 1 {
		t.Fatalf("a second past %s with no reply: %d stuck; want 1", th.NoReply, n)
	}
	got := o.agent(t, a.ID)
	if got.Outcome != agentoutcome.Stuck || got.OutcomeReason != "no reply since kickoff" || got.Status != agents.StatusRunning {
		t.Fatalf("after the verdict: outcome %q reason %q status %q", got.Outcome, got.OutcomeReason, got.Status)
	}
	ps := o.outcomePushes(a.ID)
	if len(ps) != 1 || ps[0].Title != agentLabel(a)+" is stuck: no reply since kickoff" {
		t.Fatalf("pushes %+v; want one stuck push", ps)
	}
	if n := o.chiefNotes(t, a.Name); len(n) != 1 || !strings.Contains(n[0], "STUCK: no reply since kickoff") {
		t.Fatalf("chief notes %q", n)
	}
	o.clock.Add(10 * time.Minute)
	if n := o.sweep(t); n != 0 || len(o.outcomePushes(a.ID)) != 1 {
		t.Fatalf("a second sweep of a stuck agent: %d stuck, %d pushes; want 0 and still 1", n, len(o.outcomePushes(a.ID)))
	}

	// Recovery: new activity clears it back to running (and says nothing).
	o.srv.outcomeActivity(o.asOwner, o.agent(t, a.ID))
	if got := o.agent(t, a.ID); got.Outcome != agentoutcome.Running {
		t.Fatalf("after activity: outcome %q; want running", got.Outcome)
	}

	// It replies but keeps working (its pod reports subagents): the silence clock.
	o.srv.agentWork.mu.Lock()
	if o.srv.agentWork.m == nil {
		o.srv.agentWork.m = map[int64]agentWorkReport{}
	}
	o.srv.agentWork.m[a.ID] = agentWorkReport{work: mirror.WindowWork{Subagents: 1}, seen: o.clock.Now()}
	o.srv.agentWork.mu.Unlock()
	o.srv.outcomeAnswered(o.asOwner, o.agent(t, a.ID), "Working on it.")
	replied := o.clock.Now()
	// A working pod, long past every threshold, is never flipped.
	o.clock.Set(replied.Add(th.Silent + time.Hour))
	o.srv.agentWork.mu.Lock()
	o.srv.agentWork.m[a.ID] = agentWorkReport{work: mirror.WindowWork{Subagents: 1}, seen: o.clock.Now()}
	o.srv.agentWork.mu.Unlock()
	if n := o.sweep(t); n != 0 {
		t.Fatalf("a pod reporting work was called stuck")
	}
	// Its reports stop (the TTL lapses with no clear): silence from the last activity.
	o.srv.agentWork.mu.Lock()
	delete(o.srv.agentWork.m, a.ID)
	o.srv.agentWork.mu.Unlock()
	o.srv.outcomeActivity(o.asOwner, o.agent(t, a.ID)) // the last thing it did, now
	quietFrom := o.clock.Now()
	o.clock.Set(quietFrom.Add(th.Silent - time.Second))
	if n := o.sweep(t); n != 0 {
		t.Fatalf("a second short of %s silent: %d stuck", th.Silent, n)
	}
	o.clock.Set(quietFrom.Add(th.Silent + time.Second))
	if n := o.sweep(t); n != 1 {
		t.Fatalf("a second past %s silent: %d stuck; want 1", th.Silent, n)
	}
	if got := o.agent(t, a.ID); got.OutcomeReason != "silent for 30 min" {
		t.Fatalf("silence reason %q", got.OutcomeReason)
	}
	if ps := o.outcomePushes(a.ID); len(ps) != 2 || ps[1].Title != agentLabel(a)+" is stuck: silent for 30 min" {
		t.Fatalf("pushes %+v; want the second stuck push", ps)
	}
}

// TestAnInFlightTurnIsNeverNoReplySinceKickoff (round 1, 🟡1): a claude-code
// first turn streams nothing until it ends (ccd answers at turn end), so a turn
// OPEN in this process is activity — ten minutes in, no "no reply since
// kickoff". Once it is no longer open, the threshold applies.
func TestAnInFlightTurnIsNeverNoReplySinceKickoff(t *testing.T) {
	o := newOutcomeFixture(t, stubModelCatalog{})
	a := o.kickedOff(t, o.dispatchGateway(t, "long first turn", "openrouter/acme/acme-v4.1-pro"), true)
	start := o.clock.Now()
	o.clock.Set(start.Add(10 * time.Minute))
	if n := o.sweep(t); n != 0 {
		t.Fatalf("a first turn in flight for 10 min was called stuck")
	}
	o.srv.agentOutcomes.open(a.ID, -1) // the turn is no longer open
	o.clock.Set(o.clock.Now().Add(agentoutcome.DefaultNoReplyAfter + time.Second))
	if n := o.sweep(t); n != 1 {
		t.Fatalf("control: with no turn open, %d stuck; want 1", n)
	}
}
