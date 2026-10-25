package api

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// A REPLY ON A THREAD A SESSION STARTED (migration 0054), through the real
// server against a real Postgres: the plugin's `thread`, the hook's
// POST /api/plugin-threads, and the route muster makes itself (routeByThread)
// — or the chief's ordinary delivery when it must not.

// threadTurns is a plugin deliverer's chief turn that records each delivered
// message instead of reaching a runtime: a delivery to the chief is a call.
type threadTurns struct {
	mu   sync.Mutex
	sent []string
}

func (g *threadTurns) Resolve(context.Context, agents.Agent) (agentgateway.Target, error) {
	return agentgateway.Target{}, nil
}

func (g *threadTurns) turn(_ context.Context, _ agents.Agent, _, message string, _ agents.StreamEmit) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sent = append(g.sent, message)
	return "noted", nil
}

func (g *threadTurns) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.sent)
}

type threadFixture struct {
	*routeFixture
	turns *threadTurns
	// agentMail is the owner's agent-mail plugin and its token.
	agentMail plugins.Plugin
	mailTok   string
}

func newThreadFixture(t *testing.T) *threadFixture {
	t.Helper()
	f := &threadFixture{routeFixture: newRouteFixture(t), turns: &threadTurns{}}
	d, err := plugins.NewDeliverer(plugins.DelivererConfig{Store: f.plugins, Agents: f.srv.ext.Agents, Gateway: f.turns,
		Turn: f.turns.turn, Routing: true})
	if err != nil {
		t.Fatal(err)
	}
	f.srv.pluginDeliverer = d
	f.mailTok, f.agentMail = f.newPlugin(t, plugins.AgentMailPlugin)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM plugin_threads WHERE owner_id = $1`, f.owner)
	})
	return f
}

func (f *threadFixture) newPlugin(t *testing.T, name string) (string, plugins.Plugin) {
	t.Helper()
	tok, p, err := f.plugins.Create(f.asOwner, name, "Example "+name, name+"-data")
	if err != nil {
		t.Fatal(err)
	}
	return tok, p
}

// post sends one inbound message with plugin token tok; thread "" sends none.
func (f *threadFixture) post(t *testing.T, tok, external, thread string) (int, plugins.Message) {
	t.Helper()
	in := map[string]any{"external_id": external, "sender": "someone/agent", "subject": "re: the build", "body": "is it green yet?"}
	if thread != "" {
		in["thread"] = thread
	}
	b, _ := json.Marshal(in)
	w := f.call(t, "POST", "/plugin/v1/messages", strings.NewReader(string(b)), map[string]string{"Authorization": "Bearer " + tok})
	f.srv.WaitForPluginDeliveries()
	var out struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out.ID == 0 {
		return w.Code, plugins.Message{}
	}
	m, err := f.plugins.Message(f.asOwner, out.ID)
	if err != nil {
		t.Fatal(err)
	}
	return w.Code, m
}

// bind is the hook's report, with the owner's personal token unless tok is set.
func (f *threadFixture) bind(t *testing.T, tok, thread, sid string) (int, map[string]any) {
	t.Helper()
	if tok == "" {
		tok = f.token
	}
	b, _ := json.Marshal(map[string]string{"thread": thread, "claude_session": sid})
	w := f.call(t, "POST", pluginThreadsPath, strings.NewReader(string(b)), map[string]string{"Authorization": "Bearer " + tok})
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// routesOf is every route of message m.
func (f *threadFixture) routesOf(t *testing.T, m plugins.Message) []plugins.Route {
	t.Helper()
	rows, err := f.pool.Query(authz.System(context.Background()), `SELECT route_id FROM session_routes WHERE plugin_message_id = $1 ORDER BY id`, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []plugins.Route
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, f.routeState(t, id))
	}
	return out
}

func (f *threadFixture) bindings(t *testing.T, thread string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM plugin_threads WHERE thread = $1`, thread).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAPluginMessageStoresItsThread(t *testing.T) {
	f := newThreadFixture(t)
	if code, m := f.post(t, f.mailTok, "thr-store-1", "4242"); code != 202 || m.Thread == nil || *m.Thread != "4242" {
		t.Fatalf("a message with a thread: %d %+v", code, m)
	}
	if code, m := f.post(t, f.mailTok, "thr-store-2", ""); code != 202 || m.ID == 0 || m.Thread != nil {
		t.Fatalf("a message with no thread: %d %+v", code, m)
	}
	if code, _ := f.post(t, f.mailTok, "thr-store-3", "42 42"); code != 400 {
		t.Fatalf("a thread with a space: %d, want 400", code)
	}
}

func TestAHookReportBindsTheThreadOnceAndNeverRebinds(t *testing.T) {
	f := newThreadFixture(t)
	code, out := f.bind(t, "", "4242", routeSID)
	if code != 201 || out["bound"] != true || out["host"] != "tower" || out["claude_session"] != routeSID {
		t.Fatalf("first report: %d %v", code, out)
	}
	const other = "5e55a1d0-0000-4000-8000-00000000beef"
	f.mirror.set(func(m *routeMirror) { m.otherSession = other })
	code, out = f.bind(t, "", "4242", other)
	if code != 200 || out["bound"] != false || out["claude_session"] != routeSID {
		t.Fatalf("a second session's report: %d %v; want 200 and the first binding unchanged", code, out)
	}
	if n := f.bindings(t, "4242"); n != 1 {
		t.Fatalf("%d bindings for the thread, want 1", n)
	}
	b, err := f.plugins.BoundThread(f.asOwner, "4242")
	if err != nil || b.ClaudeSession != routeSID || b.Host != "tower" {
		t.Fatalf("the stored binding: %+v %v", b, err)
	}
	// A session no online window shows binds nothing.
	if code, _ := f.bind(t, "", "5151", "5e55a1d0-0000-4000-8000-0000000dead0"); code != 409 || f.bindings(t, "5151") != 0 {
		t.Fatalf("an offline session's report: %d, %d bindings; want 409 and none", code, f.bindings(t, "5151"))
	}
}

func TestThePluginThreadsRouteRefusesABadToken(t *testing.T) {
	f := newThreadFixture(t)
	if code, _ := f.bind(t, "mst_not-a-real-token-at-all-000000000000", "4242", routeSID); code != 401 {
		t.Fatalf("a bad token: %d, want 401", code)
	}
	if code, _ := f.bind(t, f.mailTok, "4242", routeSID); code != 401 {
		t.Fatalf("a plugin token: %d, want 401", code)
	}
	if n := f.bindings(t, "4242"); n != 0 {
		t.Fatalf("%d bindings after refused reports", n)
	}
	// Positive control: the same report with the owner's token binds.
	if code, _ := f.bind(t, "", "4242", routeSID); code != 201 {
		t.Fatalf("control: %d", code)
	}
}

func TestAReplyOnABoundThreadIsRoutedToItsSession(t *testing.T) {
	f := newThreadFixture(t)
	if code, _ := f.bind(t, "", "4242", routeSID); code != 201 {
		t.Fatal("bind")
	}
	code, m := f.post(t, f.mailTok, "thr-route-1", "4242")
	if code != 202 {
		t.Fatalf("post: %d", code)
	}
	rs := f.routesOf(t, m)
	if len(rs) != 1 || rs[0].State != plugins.RouteRouted || rs[0].TargetKind != "mirror" || rs[0].TargetKey != routeSID ||
		rs[0].AgentID == nil || *rs[0].AgentID != f.chief.ID {
		t.Fatalf("routes: %+v", rs)
	}
	items := f.queued(t)
	if len(items) != 1 || rs[0].SendQueueID == nil || *rs[0].SendQueueID != items[0].ID ||
		!strings.Contains(items[0].Body, "Routed to you by "+routedByThread+": route_id "+rs[0].RouteID) ||
		!strings.Contains(items[0].Body, "is it green yet?") {
		t.Fatalf("queued: %+v", items)
	}
	if m, _ = f.plugins.Message(f.asOwner, m.ID); m.Status != plugins.StatusDelivered || m.AgentID == nil || *m.AgentID != f.chief.ID {
		t.Fatalf("the message: %+v; want delivered to the chief", m)
	}
	if n := f.turns.count(); n != 0 {
		t.Fatalf("the chief took %d turn(s); a route by thread takes none", n)
	}
	if note := f.chiefDefaultSessionText(t); !strings.Contains(note, "is it green yet?") ||
		!strings.Contains(note, "Route "+rs[0].RouteID) || !strings.Contains(note, "started thread 4242") {
		t.Fatalf("the chief's conversation does not hold the message and its route: %q", note)
	}
	var via string
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT detail->>'routed_via' FROM audit_log
		WHERE action = 'session_route.routed' AND object = $1`, plugins.RouteObject(rs[0].RouteID)).Scan(&via); err != nil ||
		via != "thread 4242 started by tower/"+routeSID {
		t.Fatalf("routed_via %q (%v)", via, err)
	}
	// The same message again (a repeat POST, and a second attempt) routes nothing twice.
	if code, _ := f.post(t, f.mailTok, "thr-route-1", "4242"); code != 200 {
		t.Fatalf("a repeat: %d", code)
	}
	p, _ := f.plugins.Plugin(f.asOwner, m.PluginID)
	if !f.srv.routeByThread(f.asOwner, p, m) {
		t.Fatal("a second attempt on a routed message must leave it with its route")
	}
	if rs := f.routesOf(t, m); len(rs) != 1 || len(f.queued(t)) != 1 || f.turns.count() != 0 {
		t.Fatalf("after a repeat: %d routes, %d queued, %d turns; want 1, 1, 0", len(rs), len(f.queued(t)), f.turns.count())
	}
}

func TestABoundSessionThatIsGoneFallsBackToTheChief(t *testing.T) {
	f := newThreadFixture(t)
	if code, _ := f.bind(t, "", "4242", routeSID); code != 201 {
		t.Fatal("bind")
	}
	// The window now holds another conversation: no online window shows the session.
	f.mirror.set(func(m *routeMirror) { m.claudeSession = "5e55a1d0-0000-4000-8000-0000000c0ffe" })
	_, m := f.post(t, f.mailTok, "thr-gone-1", "4242")
	if rs := f.routesOf(t, m); len(rs) != 0 || f.turns.count() != 1 || len(f.queued(t)) != 0 {
		t.Fatalf("%d routes, %d chief turns, %d queued; want 0, 1, 0", len(rs), f.turns.count(), len(f.queued(t)))
	}
	// The session online, but on another host than the one it was bound from.
	f.mirror.set(func(m *routeMirror) { m.claudeSession = "" })
	if _, _, err := f.plugins.BindThread(f.asOwner, "7373", "elsewhere", routeSID); err != nil {
		t.Fatal(err)
	}
	_, m = f.post(t, f.mailTok, "thr-gone-2", "7373")
	if rs := f.routesOf(t, m); len(rs) != 0 || f.turns.count() != 2 {
		t.Fatalf("another host: %d routes, %d chief turns; want 0, 2", len(rs), f.turns.count())
	}
}

func TestAnUnboundThreadGoesToTheChief(t *testing.T) {
	f := newThreadFixture(t)
	_, m := f.post(t, f.mailTok, "thr-unbound-1", "9999")
	if rs := f.routesOf(t, m); len(rs) != 0 || f.turns.count() != 1 {
		t.Fatalf("%d routes, %d chief turns; want 0 and 1", len(rs), f.turns.count())
	}
}

func TestADiscordMessageIsNeverRoutedByThread(t *testing.T) {
	f := newThreadFixture(t)
	tok, _ := f.newPlugin(t, "discord-dm")
	if code, _ := f.bind(t, "", "4242", routeSID); code != 201 {
		t.Fatal("bind")
	}
	_, m := f.post(t, tok, "thr-discord-1", "4242")
	if rs := f.routesOf(t, m); len(rs) != 0 || f.turns.count() != 1 {
		t.Fatalf("%d routes, %d chief turns; want 0 and 1", len(rs), f.turns.count())
	}
	// Control: the same thread through agent mail is routed.
	_, m = f.post(t, f.mailTok, "thr-discord-2", "4242")
	if rs := f.routesOf(t, m); len(rs) != 1 {
		t.Fatalf("control: %d routes", len(rs))
	}
}

func TestAnotherOwnersThreadIsInvisible(t *testing.T) {
	f := newThreadFixture(t)
	other := f.newMember(t)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM plugin_threads WHERE owner_id = $1`, other)
	})
	if _, created, err := f.plugins.BindThread(f.as(other), "4242", "tower", routeSID); err != nil || !created {
		t.Fatalf("the other owner's bind: %v %v", created, err)
	}
	if _, err := f.plugins.BoundThread(f.asOwner, "4242"); err != plugins.ErrNotFound {
		t.Fatalf("the owner reads another's binding: %v", err)
	}
	_, m := f.post(t, f.mailTok, "thr-rls-1", "4242")
	if rs := f.routesOf(t, m); len(rs) != 0 || f.turns.count() != 1 {
		t.Fatalf("%d routes, %d chief turns; want 0 and 1", len(rs), f.turns.count())
	}
	// The owner's own binding of the same thread is its own row.
	if code, _ := f.bind(t, "", "4242", routeSID); code != 201 || f.bindings(t, "4242") != 2 {
		t.Fatalf("the owner's own bind: %d, %d rows", code, f.bindings(t, "4242"))
	}
}

func TestAThreadRoutedMessageRaisesNoUnroutedAsk(t *testing.T) {
	f := newThreadFixture(t)
	if code, _ := f.bind(t, "", "4242", routeSID); code != 201 {
		t.Fatal("bind")
	}
	_, routed := f.post(t, f.mailTok, "thr-ask-1", "4242")
	_, control := f.post(t, f.mailTok, "thr-ask-2", "") // delivered to the chief, which does nothing
	if rs := f.routesOf(t, routed); len(rs) != 1 {
		t.Fatalf("routed: %d routes", len(rs))
	}
	for _, m := range []plugins.Message{routed, control} {
		if got, _ := f.plugins.Message(f.asOwner, m.ID); got.Status != plugins.StatusDelivered {
			t.Fatalf("message %d is %s, want delivered", m.ID, got.Status)
		}
	}
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE plugin_messages SET delivered_at = now() - interval '1 hour' WHERE id IN (`+
		strconv.FormatInt(routed.ID, 10)+`, `+strconv.FormatInt(control.ID, 10)+`)`)
	due, err := f.plugins.UnroutedDue(context.Background(), time.Now(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	var sawRouted, sawControl bool
	for _, m := range due {
		sawRouted = sawRouted || m.ID == routed.ID
		sawControl = sawControl || m.ID == control.ID
	}
	if sawRouted || !sawControl {
		t.Fatalf("due: routed %v (want false), control %v (want true)", sawRouted, sawControl)
	}
}

func TestAThreadRouteTimesOutBackToTheChiefThatHoldsTheMessage(t *testing.T) {
	f := newThreadFixture(t)
	if code, _ := f.bind(t, "", "4242", routeSID); code != 201 {
		t.Fatal("bind")
	}
	_, m := f.post(t, f.mailTok, "thr-timeout-1", "4242")
	rs := f.routesOf(t, m)
	if len(rs) != 1 {
		t.Fatalf("%d routes", len(rs))
	}
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE session_routes SET timeout_at = now() - interval '1 minute' WHERE route_id = '`+rs[0].RouteID+`'`)
	if k, err := f.srv.RouteTimeoutPass(context.Background()); err != nil || k < 1 {
		t.Fatalf("past the deadline: %d timed out (%v)", k, err)
	}
	if r := f.routeState(t, rs[0].RouteID); r.State != plugins.RouteTimedOut {
		t.Fatalf("the route: %+v", r)
	}
	note := f.chiefDefaultSessionText(t)
	if !strings.Contains(note, "is it green yet?") || !strings.Contains(note, "so the message is back with you") {
		t.Fatalf("the chief's conversation: %q", note)
	}
}

// refusingQueue refuses every enqueue: a window whose queue cannot take the
// routed message.
type refusingQueue struct{ SendQueue }

func (refusingQueue) Enqueue(context.Context, sendqueue.Item) (sendqueue.Item, error) {
	return sendqueue.Item{}, errors.New("refusingQueue: no")
}

func TestAThreadRouteThatCannotBeQueuedGoesBackToTheChief(t *testing.T) {
	f := newThreadFixture(t)
	if code, _ := f.bind(t, "", "4242", routeSID); code != 201 {
		t.Fatal("bind")
	}
	f.srv.ext.SendQueue = refusingQueue{f.srv.ext.SendQueue}
	_, m := f.post(t, f.mailTok, "thr-noqueue-1", "4242")
	rs := f.routesOf(t, m)
	if len(rs) != 1 || rs[0].State != plugins.RouteFailed || f.turns.count() != 0 {
		t.Fatalf("routes %+v, %d chief turns; want one failed route and no turn", rs, f.turns.count())
	}
	if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	note := f.chiefDefaultSessionText(t)
	if !strings.Contains(note, "is it green yet?") || !strings.Contains(note, "could not be put into the session") ||
		!strings.Contains(note, "so the message is back with you") {
		t.Fatalf("the chief's conversation: %q", note)
	}
}

func TestANoReplyMessageOnABoundThreadGoesToTheChief(t *testing.T) {
	f := newThreadFixture(t)
	if code, _ := f.bind(t, "", "4242", routeSID); code != 201 {
		t.Fatal("bind")
	}
	b, _ := json.Marshal(map[string]any{"external_id": "thr-noreply-1", "body": "status: done", "thread": "4242", "no_reply": true})
	w := f.call(t, "POST", "/plugin/v1/messages", strings.NewReader(string(b)), map[string]string{"Authorization": "Bearer " + f.mailTok})
	f.srv.WaitForPluginDeliveries()
	var out struct{ ID int64 }
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if rs := f.routesOf(t, plugins.Message{ID: out.ID}); w.Code != 202 || len(rs) != 0 || f.turns.count() != 1 {
		t.Fatalf("%d, %d routes, %d chief turns; want 202, 0, 1", w.Code, len(rs), f.turns.count())
	}
}

func TestAMessageTheChiefAlreadyHasIsNotRoutedByThread(t *testing.T) {
	f := newThreadFixture(t)
	_, m := f.post(t, f.mailTok, "thr-had-1", "4242") // unbound: delivered to the chief
	if f.turns.count() != 1 {
		t.Fatalf("%d chief turns", f.turns.count())
	}
	if code, _ := f.bind(t, "", "4242", routeSID); code != 201 {
		t.Fatal("bind")
	}
	m, _ = f.plugins.Message(f.asOwner, m.ID)
	p, _ := f.plugins.Plugin(f.asOwner, m.PluginID)
	if !f.srv.routeByThread(f.asOwner, p, m) {
		t.Fatal("a message the chief holds must stay the chief's, not go out again")
	}
	if rs := f.routesOf(t, m); len(rs) != 0 || len(f.queued(t)) != 0 {
		t.Fatalf("%d routes, %d queued; want none", len(rs), len(f.queued(t)))
	}
}

func TestThePluginThreadsRouteIsOffWithRoutingOff(t *testing.T) {
	f := newThreadFixture(t)
	f.srv.ext.Routing = nil
	if code, _ := f.bind(t, "", "4242", routeSID); code != 404 || f.bindings(t, "4242") != 0 {
		t.Fatalf("routing off: %d, %d bindings; want 404 and none", code, f.bindings(t, "4242"))
	}
}

func TestWithNoDelivererNothingIsRoutedByThread(t *testing.T) {
	f := newThreadFixture(t)
	if code, _ := f.bind(t, "", "4242", routeSID); code != 201 {
		t.Fatal("bind")
	}
	f.srv.pluginDeliverer = nil // no gateway: nothing could record it for the chief, nor hand it back
	_, m := f.post(t, f.mailTok, "thr-nodeliverer-1", "4242")
	if rs := f.routesOf(t, m); len(rs) != 0 || m.Status != plugins.StatusQueued {
		t.Fatalf("%d routes, status %s; want none and queued", len(rs), m.Status)
	}
}
