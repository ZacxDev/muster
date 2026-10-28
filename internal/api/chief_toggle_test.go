package api

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE CHIEF IS OPTIONAL AND OFF UNLESS YOU HAVE ONE (chief_toggle.go). These
// drive the real handler chain over an in-memory agent list (homeAgents, the
// home tests' store) with the switch's store faked; the Postgres store and the
// create-on-turn-on path are chief_toggle_pg_test.go's.

// memChiefPrefs is chiefpref.Store in memory: one choice per owner, or err on
// every read.
type memChiefPrefs struct {
	mu     sync.Mutex
	choice map[int64]bool
	err    error
}

func (p *memChiefPrefs) Choice(_ context.Context, owner int64) (bool, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return false, false, p.err
	}
	on, chose := p.choice[owner]
	return chose, on, nil
}

func (p *memChiefPrefs) SetEnabled(_ context.Context, owner int64, on bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.choice == nil {
		p.choice = map[int64]bool{}
	}
	p.choice[owner] = on
	return nil
}

func (p *memChiefPrefs) get(owner int64) (chose, on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	on, chose = p.choice[owner]
	return chose, on
}

// callProvisioner is stubProvisioner recording Start and Stop by agent id, and
// the instance's resulting state ("running"/"stopped") as a cluster would hold
// it. startDelay / stopDelay hold a call before it lands; startErr fails Start.
type callProvisioner struct {
	stubProvisioner
	mu                    sync.Mutex
	calls                 []string
	state                 string
	startDelay, stopDelay time.Duration
	startErr              error
}

func (p *callProvisioner) Start(_ context.Context, id int64) error {
	time.Sleep(p.startDelay)
	if p.startErr != nil {
		p.record("start-failed", id, "")
		return p.startErr
	}
	p.record("start", id, "running")
	return nil
}
func (p *callProvisioner) Stop(_ context.Context, id int64) error {
	time.Sleep(p.stopDelay)
	p.record("stop", id, "stopped")
	return nil
}
func (p *callProvisioner) record(op string, id int64, state string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, op+" "+itoa(id))
	if state != "" {
		p.state = state
	}
}

func (p *callProvisioner) instance() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

// waitFor waits up to two seconds for the background call want.
func (p *callProvisioner) waitFor(t *testing.T, want string) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		p.mu.Lock()
		for _, c := range p.calls {
			if c == want {
				p.mu.Unlock()
				return
			}
		}
		p.mu.Unlock()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	t.Fatalf("the provisioner was not called %q; calls %v", want, p.calls)
}

func (p *callProvisioner) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

// GetByName finds a row by slug (POST /chief/provision's lookup).
func (s *homeAgents) GetByName(_ context.Context, name string) (agents.Agent, error) {
	for _, a := range s.rows {
		if a.Name == name {
			return a, nil
		}
	}
	return agents.Agent{}, errors.New("not found")
}

// chiefWorld is a server over rows (each owned by the operator) with the
// switch in prefs and, when prov is set, an agent provisioner.
type chiefWorld struct {
	s     *Server
	h     http.Handler
	store *homeAgents
	prefs *memChiefPrefs
	prov  *callProvisioner
}

func newChiefWorld(t *testing.T, withChief, withProvisioner bool) *chiefWorld {
	t.Helper()
	store := &homeAgents{rows: homeRows(withChief), latest: map[int64]int64{homeChiefID: homeChiefSess, homeWorkerID: 55}}
	w := &chiefWorld{store: store, prefs: &memChiefPrefs{}}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	ext := Extensions{Agents: store, SessionLiveness: stubLiveness{}, ChiefPrefs: w.prefs}
	if withProvisioner {
		w.prov = &callProvisioner{}
		ext.Provisioner = w.prov
	}
	s.UseExtensions(ext)
	w.s, w.h = s, s.Handler()
	return w
}

func (w *chiefWorld) mirror() []agents.ToolDef                  { m, _ := w.tools(agents.Agent{}); return m }
func (w *chiefWorld) chiefTools(a agents.Agent) map[string]bool { _, c := w.tools(a); return c }

// tools reads the two tool lists that need the attention store (the mirror
// door's and the chief's), with a stub store wired only for the read — the
// pages' attention badges would call into it.
func (w *chiefWorld) tools(chief agents.Agent) (mirror []agents.ToolDef, chiefs map[string]bool) {
	w.s.ext.Attention = stubAttention{}
	defer func() { w.s.ext.Attention = nil }()
	return w.s.mirrorTools(), offered(w.s.toolsFor(opCtx, chief))
}

// askChief is a mirrored session's chief_ask, as the operator's session.
func (w *chiefWorld) askChief() string {
	return w.s.toolChiefAsk(opCtx, toolCaller{}, `{"question":"which branch?"}`)
}

func (w *chiefWorld) get(path string, hdr map[string]string) *httptest.ResponseRecorder {
	return homeGet(w.s, w.h, path, hdr)
}

// post is a same-origin form POST as the signed-in operator.
func (w *chiefWorld) post(path string, form url.Values) *httptest.ResponseRecorder {
	r := operatorRequest(w.s, http.MethodPost, path)
	r.Body = io.NopCloser(strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://"+r.Host)
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, r)
	return rec
}

// landing is where `/` sends the operator.
func (w *chiefWorld) landing(t *testing.T) string {
	t.Helper()
	rec := w.get("/", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("GET / = %d", rec.Code)
	}
	return rec.Header().Get("Location")
}

// sidebar is the sidebar's rows.
func (w *chiefWorld) sidebar() string {
	return w.get(ui.NavAgentsPath, map[string]string{"HX-Request": "true"}).Body.String()
}

// chiefSection is the Account page's Chief section, and its state.
func (w *chiefWorld) chiefSection(t *testing.T) (section, state string) {
	t.Helper()
	rec := w.get("/account", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /account = %d", rec.Code)
	}
	body := rec.Body.String()
	i := strings.Index(body, `data-testid="chief-settings"`)
	if i < 0 {
		t.Fatalf("the Account page has no Chief section:\n%.600s", body)
	}
	section = body[i:]
	section = section[:strings.Index(section, "</section>")]
	const attr = `id="chief-state" data-state="`
	j := strings.Index(section, attr)
	if j < 0 {
		t.Fatalf("the Chief section states nothing:\n%s", section)
	}
	state = section[j+len(attr):]
	return section, state[:strings.Index(state, `"`)]
}

var opCtx = authz.Operator(context.Background())

// TestAFreshInstallHasNoChiefAndSaysSo: the public install — no agent
// provisioner, no chief agent. Home is the Agents list, the sidebar has no
// chief row, nothing offers chief_ask, and the Account page says the chief is
// not available here instead of drawing a switch; a POST to the switch is
// refused and stores nothing.
//
// Red on the base tree: the Account page had no Chief section, chief_ask was
// offered to every mirrored session with the attention store, and there was
// no switch to refuse.
func TestAFreshInstallHasNoChiefAndSaysSo(t *testing.T) {
	w := newChiefWorld(t, false, false)
	if got := w.landing(t); got != "/agents" {
		t.Errorf("GET / → %q, want /agents", got)
	}
	if sb := w.sidebar(); strings.Contains(sb, "data-nav-chief") {
		t.Errorf("the sidebar has a chief row:\n%s", sb)
	}
	section, state := w.chiefSection(t)
	if state != "unavailable" || !strings.Contains(section, "Not available on this install") {
		t.Errorf("Chief section state %q, want unavailable:\n%s", state, section)
	}
	if strings.Contains(section, `id="chief-toggle"`) || strings.Contains(section, "<form") {
		t.Errorf("an unavailable chief draws a switch:\n%s", section)
	}
	// chief_ask stays listed (a session's list is read once, at connect); the
	// call says there is no chief and where to ask instead.
	if !hasTool(w.mirror(), chiefAskTool) {
		t.Errorf("a session is not offered %s", chiefAskTool)
	}
	if got := w.askChief(); !strings.Contains(got, "has no chief agent") || !strings.Contains(got, "attention_ask") {
		t.Errorf("chief_ask with no chief = %s, want the no-chief refusal naming attention_ask", got)
	}
	if rec := w.post(accountChiefPath, url.Values{"on": {"1"}}); rec.Code != http.StatusConflict {
		t.Errorf("POST %s on=1 = %d, want 409", accountChiefPath, rec.Code)
	}
	if chose, _ := w.prefs.get(1); chose {
		t.Error("a refused switch stored a choice")
	}
}

// TestANewUserWithAProvisionerHasTheChiefOff: with Kubernetes, a user with no
// chief agent sees the switch OFF, with the control to turn it on, and home
// is the Agents list.
func TestANewUserWithAProvisionerHasTheChiefOff(t *testing.T) {
	w := newChiefWorld(t, false, true)
	section, state := w.chiefSection(t)
	if state != "off" || !strings.Contains(section, `name="on" value="1"`) || !strings.Contains(section, "Turn the chief on") {
		t.Errorf("Chief section state %q, want off with a turn-on control:\n%s", state, section)
	}
	if got := w.landing(t); got != "/agents" {
		t.Errorf("GET / → %q, want /agents", got)
	}
}

// TestAnExistingChiefStaysOnWithNoChoice: a deployment that already runs a
// chief and never touched the switch keeps it, unchanged — home opens it, the
// sidebar pins it, everything that talks to it resolves it, sessions are
// offered chief_ask — and the Account page reads ON, with the control to turn
// it off. Nothing was stored to make it so.
//
// Red on the base tree: no Chief section. (The home, sidebar, chiefAgent and
// chief_ask halves are INVARIANT guards: the base tree already did them; they
// pin that the new default does not take the chief away.)
func TestAnExistingChiefStaysOnWithNoChoice(t *testing.T) {
	w := newChiefWorld(t, true, true)
	if got := w.landing(t); got != chiefHref {
		t.Errorf("GET / → %q, want the chief %q", got, chiefHref)
	}
	if sb := w.sidebar(); !strings.Contains(sb, "data-nav-chief") {
		t.Errorf("the sidebar lost the chief row:\n%s", sb)
	}
	if c, err := w.s.chiefAgent(opCtx); err != nil || c.ID != homeChiefID {
		t.Errorf("chiefAgent = %d %v, want %d", c.ID, err, homeChiefID)
	}
	if !hasTool(w.mirror(), chiefAskTool) {
		t.Errorf("a session is not offered %s with the chief on", chiefAskTool)
	}
	section, state := w.chiefSection(t)
	if state != "on" || !strings.Contains(section, `name="on" value="0"`) || !strings.Contains(section, "Turn the chief off") {
		t.Errorf("Chief section state %q, want on with a turn-off control:\n%s", state, section)
	}
	if chose, _ := w.prefs.get(1); chose {
		t.Error("reading the page stored a choice")
	}
}

func hasTool(defs []agents.ToolDef, name string) bool {
	for _, d := range defs {
		if d.Name == name {
			return true
		}
	}
	return false
}

func offered(ts []agentTool) map[string]bool {
	out := map[string]bool{}
	for _, t := range ts {
		out[t.def.Name] = true
	}
	return out
}

// TestTurningTheChiefOffAndOnAgain: OFF stores the choice, stops the chief's
// instance, sends home to the Agents list, takes the chief out of the sidebar,
// makes chiefAgent answer ErrChiefOff (an ErrNoChief), withdraws chief_ask and
// the chief's chief-only tools (a call to one is refused), and a mention's ask
// is raised by muster. It deletes nothing. ON brings every one of them back
// and starts the stopped instance.
//
// Red on the base tree: POST /account/chief did not exist.
func TestTurningTheChiefOffAndOnAgain(t *testing.T) {
	w := newChiefWorld(t, true, true)
	chief := w.store.rows[len(w.store.rows)-1]
	if chief.ID != homeChiefID {
		t.Fatalf("fixture: the last row is %d, want the chief", chief.ID)
	}
	if !w.chiefTools(chief)[attentionListTool] {
		t.Fatalf("control: the chief on is not offered %s", attentionListTool)
	}
	if a := w.s.mentionAsker(opCtx, 1); a.System {
		t.Fatalf("control: with the chief on, a mention's ask is muster's: %+v", a)
	}

	rec := w.post(accountChiefPath, url.Values{"on": {"0"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/account?chief=saved#chief" {
		t.Fatalf("off: %d → %q", rec.Code, rec.Header().Get("Location"))
	}
	if chose, on := w.prefs.get(1); !chose || on {
		t.Fatalf("off stored chose=%v on=%v", chose, on)
	}
	w.prov.waitFor(t, "stop "+itoa(homeChiefID))
	if got := w.landing(t); got != "/agents" {
		t.Errorf("off: GET / → %q, want /agents", got)
	}
	if sb := w.sidebar(); strings.Contains(sb, "data-nav-chief") || strings.Contains(sb, `/agents/zesty-stoat`) {
		t.Errorf("off: the sidebar still lists the chief:\n%s", sb)
	}
	if _, err := w.s.chiefAgent(opCtx); !errors.Is(err, ErrChiefOff) || !errors.Is(err, ErrNoChief) {
		t.Errorf("off: chiefAgent err = %v, want ErrChiefOff (an ErrNoChief)", err)
	}
	// chief_ask stays listed — a session that connected before Off holds the
	// list it read then — and its call is refused with the way out.
	if !hasTool(w.mirror(), chiefAskTool) {
		t.Errorf("off: %s left the list (a connected session would still hold it)", chiefAskTool)
	}
	if got := w.askChief(); !strings.Contains(got, "turned their chief off") || !strings.Contains(got, "attention_ask") {
		t.Errorf("off: chief_ask = %s, want the turned-off refusal naming attention_ask", got)
	}
	if w.chiefTools(chief)[attentionListTool] {
		t.Errorf("off: the chief is still offered %s", attentionListTool)
	}
	w.s.ext.Attention = stubAttention{}
	text := w.s.dispatchAgentTool(opCtx, chief, attentionListTool, `{}`)
	w.s.ext.Attention = nil
	if !strings.Contains(text, "turned the chief off") {
		t.Errorf("off: the chief's %s call = %s, want refused", attentionListTool, text)
	}
	if a := w.s.mentionAsker(opCtx, 1); !a.System {
		t.Errorf("off: a mention's ask is raised by %+v, want muster", a)
	}
	if _, state := w.chiefSection(t); state != "off" {
		t.Errorf("off: Account reads %q", state)
	}
	// Off deletes nothing: the agent is still in the store, still protected.
	if _, chief, err := w.s.agentIsChief(opCtx, homeChiefID); err != nil || !chief {
		t.Errorf("off: the chief row is gone or unprotected (chief=%v err=%v)", chief, err)
	}

	// The provisioner stopped it; on starts it again.
	w.store.rows[len(w.store.rows)-1].Status = agents.StatusStopped
	if rec := w.post(accountChiefPath, url.Values{"on": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("on: %d", rec.Code)
	}
	if chose, on := w.prefs.get(1); !chose || !on {
		t.Fatalf("on stored chose=%v on=%v", chose, on)
	}
	w.prov.waitFor(t, "start "+itoa(homeChiefID))
	if got := w.landing(t); got != chiefHref {
		t.Errorf("on: GET / → %q, want the chief", got)
	}
	if sb := w.sidebar(); !strings.Contains(sb, "data-nav-chief") {
		t.Errorf("on: the sidebar has no chief row:\n%s", sb)
	}
	if c, err := w.s.chiefAgent(opCtx); err != nil || c.ID != homeChiefID {
		t.Errorf("on: chiefAgent = %d %v", c.ID, err)
	}
	if !hasTool(w.mirror(), chiefAskTool) || !w.chiefTools(chief)[attentionListTool] {
		t.Error("on: chief_ask or the chief's tools did not come back")
	}
	if _, state := w.chiefSection(t); state != "on" {
		t.Errorf("on: Account reads %q", state)
	}
	if rec := w.post(accountChiefPath, url.Values{"on": {"maybe"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("on=maybe: %d, want 400", rec.Code)
	}
}

// TestAQuickOffThenOnLeavesTheChiefRunning (audit round 1, #1): Off then On
// in quick succession ends with the instance started and the switch on —
// On starts the chief whatever its row last said, and after Off's Stop, not
// before it. The fixture's row keeps reading `running` (as it would until
// Stop's write lands), which is what used to make On start nothing.
func TestAQuickOffThenOnLeavesTheChiefRunning(t *testing.T) {
	w := newChiefWorld(t, true, true)
	w.prov.stopDelay = 50 * time.Millisecond
	for _, on := range []string{"0", "1"} {
		if rec := w.post(accountChiefPath, url.Values{"on": {on}}); rec.Code != http.StatusSeeOther {
			t.Fatalf("on=%s: %d", on, rec.Code)
		}
	}
	time.Sleep(150 * time.Millisecond) // any background call would have landed
	if got := strings.Join(w.prov.seen(), ","); got != "stop 23,start 23" {
		t.Errorf("provisioner calls %q, want stop then start", got)
	}
	if st := w.prov.instance(); st != "running" {
		t.Errorf("the chief is on and its instance is %q", st)
	}
	if chose, on := w.prefs.get(1); !chose || !on {
		t.Errorf("stored chose=%v on=%v, want on", chose, on)
	}
}

// TestTheLastChoiceWinsUnderConcurrentClicks (audit round 1, #3): On and Off
// racing — a slow Start, a fast Stop — end with the instance in the state the
// stored choice says, whichever request ran last. Each request holds the
// owner's switch lock through its call, so the two cannot interleave.
func TestTheLastChoiceWinsUnderConcurrentClicks(t *testing.T) {
	for _, c := range []struct {
		name   string
		clicks [2]string
		slow   string // which call is held: start or stop
	}{
		{"on then off, a slow Start", [2]string{"1", "0"}, "start"},
		{"off then on, a slow Stop", [2]string{"0", "1"}, "stop"},
	} {
		t.Run(c.name, func(t *testing.T) {
			for i := 0; i < 5; i++ {
				w := newChiefWorld(t, true, true)
				if c.clicks[0] == "1" {
					// Stopped before, so On has an instance to bring up.
					w.store.rows[len(w.store.rows)-1].Status = agents.StatusStopped
					w.prov.state = "stopped"
				} else {
					w.prov.state = "running"
				}
				if c.slow == "start" {
					w.prov.startDelay = 40 * time.Millisecond
				} else {
					w.prov.stopDelay = 40 * time.Millisecond
				}
				var wg sync.WaitGroup
				for _, on := range c.clicks {
					wg.Add(1)
					go func(on string) {
						defer wg.Done()
						w.post(accountChiefPath, url.Values{"on": {on}})
					}(on)
					time.Sleep(5 * time.Millisecond) // the second click while the first's call is in flight
				}
				wg.Wait()
				time.Sleep(100 * time.Millisecond) // any background call would have landed
				_, on := w.prefs.get(1)
				want := map[bool]string{true: "running", false: "stopped"}[on]
				if st := w.prov.instance(); st != want {
					t.Fatalf("round %d: the switch is on=%v and the instance is %q (calls %v)", i, on, st, w.prov.seen())
				}
			}
		})
	}
}

// TestAFailedStartIsSaidAndLeavesTheChiefOff (audit round 1, #4): On whose
// Start fails does not say "Saved." — it says the chief could not be started,
// stores nothing, and the chief stays off.
func TestAFailedStartIsSaidAndLeavesTheChiefOff(t *testing.T) {
	w := newChiefWorld(t, true, true)
	_ = w.prefs.SetEnabled(opCtx, 1, false)
	w.prov.startErr = errors.New("cluster unreachable")
	rec := w.post(accountChiefPath, url.Values{"on": {"1"}})
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || loc != "/account?chief=startfailed#chief" {
		t.Fatalf("on with a failing Start: %d → %q", rec.Code, loc)
	}
	if _, on := w.prefs.get(1); on {
		t.Error("a failed Start stored on")
	}
	page := w.get("/account?chief=startfailed", nil).Body.String()
	if strings.Contains(page, "Saved.") || !strings.Contains(page, "could not be started") {
		t.Errorf("the Account page after a failed Start:\n%.800s", page[strings.Index(page, `data-testid="chief-settings"`):])
	}
	if got := w.landing(t); got != "/agents" {
		t.Errorf("GET / → %q, want /agents (still off)", got)
	}
}

// TestAChiefWithNoProvisionerCanStillBeTurnedOff: an install with no
// Kubernetes that nevertheless holds a chief agent (one made before) gets the
// switch, and off gates it with nothing to stop.
func TestAChiefWithNoProvisionerCanStillBeTurnedOff(t *testing.T) {
	w := newChiefWorld(t, true, false)
	if _, state := w.chiefSection(t); state != "on" {
		t.Fatalf("Account reads %q, want on", state)
	}
	if rec := w.post(accountChiefPath, url.Values{"on": {"0"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("off: %d %s", rec.Code, rec.Body)
	}
	if got := w.landing(t); got != "/agents" {
		t.Errorf("off: GET / → %q", got)
	}
}

// TestProvisionRefusesAChiefTurnedOff: POST /chief/provision does not create
// or start a chief its owner turned off (409, saying where to turn it on); with
// no choice it starts the existing chief as before.
func TestProvisionRefusesAChiefTurnedOff(t *testing.T) {
	w := newChiefWorld(t, true, true)
	w.store.rows[len(w.store.rows)-1].Name = agents.ChiefName // the reserved slug provision looks up
	_ = w.prefs.SetEnabled(opCtx, 1, false)
	rec := w.post("/chief/provision", url.Values{})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), ChiefOffProvisionRefusal) {
		t.Fatalf("provision with the chief off: %d %s", rec.Code, rec.Body)
	}
	time.Sleep(100 * time.Millisecond)
	if calls := w.prov.seen(); len(calls) != 0 {
		t.Errorf("a refused provision called the provisioner: %v", calls)
	}
	w.prefs.mu.Lock()
	delete(w.prefs.choice, 1)
	w.prefs.mu.Unlock()
	if rec := w.post("/chief/provision", url.Values{}); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"created":false`) {
		t.Fatalf("provision with no choice: %d %s", rec.Code, rec.Body)
	}
	w.prov.waitFor(t, "start "+itoa(homeChiefID))
}

// TestAnUnreadableSwitchIsOff: a switch that cannot be read never lets a
// chief act — home is the Agents list, chiefAgent fails (not as "no chief",
// which would mislabel an outage), the chief's tools are withheld — and the
// Account page says it could not read it rather than drawing a guess.
func TestAnUnreadableSwitchIsOff(t *testing.T) {
	w := newChiefWorld(t, true, true)
	w.prefs.err = errors.New("db down")
	if got := w.landing(t); got != "/agents" {
		t.Errorf("GET / → %q, want /agents", got)
	}
	if _, err := w.s.chiefAgent(opCtx); err == nil || errors.Is(err, ErrNoChief) {
		t.Errorf("chiefAgent err = %v, want the read error", err)
	}
	chief := w.store.rows[len(w.store.rows)-1]
	if w.chiefTools(chief)[attentionListTool] {
		t.Error("an unreadable switch offered the chief its tools")
	}
	if _, state := w.chiefSection(t); state != "unknown" {
		t.Errorf("Account reads %q, want unknown", state)
	}
}
