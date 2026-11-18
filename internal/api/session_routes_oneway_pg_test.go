package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
)

// ROUTE FIRST, through the real server against a real Postgres (migration
// 0087): a plugin message its plugin marked no_reply — feedback, a heads-up —
// can be handed to the session already on it ONE-WAY, its answer reaching the
// chief and never the sender; the owner is told, once, of every route the chief
// makes on its own; and an incoming task event's session hint is matched on
// the task's words, leaving out the agents already on it.
//
// The expectations are typed literals, not the implementation's constants:
// they are what the session, the chief and the owner read.

// oneWaySubject is the subject of the no_reply message the tests route: a
// bug report.
const oneWaySubject = "Bug: the export button does nothing"

// withPush gives the fixture a push service with one device of the owner's,
// and returns the fake that records what reached it and the device.
func (f *routeFixture) withPush(t *testing.T) (*payloadPusher, string) {
	t.Helper()
	pusher, store := &payloadPusher{}, &memPush{}
	dev := ep("route-owner-" + strconv.FormatInt(f.owner, 10))
	claim(t, store, f.owner, dev)
	f.srv.ext.Push = push.NewService(store, pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))
	return pusher, dev
}

// routePushes is every push the device got for a route: their tags begin
// "route-".
func routePushes(f *routeFixture, p *payloadPusher, dev string) []push.Payload {
	f.srv.WaitForPushes()
	var out []push.Payload
	for _, pl := range p.to(dev) {
		if strings.HasPrefix(pl.Tag, "route-") {
			out = append(out, pl)
		}
	}
	return out
}

// noReplyMessage stores one no_reply message through the owner's feedback
// plugin (created on first use), from sender — verified or not — and settles
// it as delivered to the chief.
func (f *routeFixture) noReplyMessage(t *testing.T, external, sender string, verified bool) (plugins.Plugin, plugins.Message) {
	t.Helper()
	ctx := f.asOwner
	ps, err := f.plugins.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var p plugins.Plugin
	for _, x := range ps {
		if x.Name == "example-feedback" {
			p = x
		}
	}
	if p.ID == 0 {
		if _, p, err = f.plugins.Create(ctx, "example-feedback", "Example feedback", "example-feedback-data"); err != nil {
			t.Fatal(err)
		}
	}
	m, _, err := f.plugins.Submit(ctx, p.ID, plugins.MessageIn{ExternalID: external, Sender: sender, Subject: oneWaySubject,
		Body: "Clicking export on the billing page does nothing.", NoReply: true, SenderVerified: verified})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.plugins.BeginDelivery(ctx, m.ID, f.chief.ID); err != nil || !ok {
		t.Fatalf("begin delivery: %v %v", ok, err)
	}
	if ok, err := f.plugins.RecordDelivery(ctx, m.ID, f.chief.ID, plugins.StatusDelivered); err != nil || !ok {
		t.Fatalf("record delivery: %v %v", ok, err)
	}
	return p, m
}

// draftsFor is how many reply drafts exist for message id, read as the system.
func (f *routeFixture) draftsFor(t *testing.T, id int64) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM plugin_reply_drafts WHERE message_id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 🔴 TestANoReplyMessageGoesOneWayAndItsAnswerReachesOnlyTheChief: the chief
// routes a no_reply message (it was refused before 0087: "nothing to route");
// the session's paste says NO REPLY POSSIBLE and to report to the chief; the
// session's answer is relayed to the chief's plugin session — never a reply
// draft, even for a VERIFIED sender the owner ALLOWLISTED (the one case a
// two-way route sends without asking) — and reply_draft still refuses the
// message. The answer is delivered once however often the sweep runs.
func TestANoReplyMessageGoesOneWayAndItsAnswerReachesOnlyTheChief(t *testing.T) {
	f := newRouteFixture(t)
	f.allow(t, "example-feedback:reporter@example.test")
	p, m := f.noReplyMessage(t, "fb-oneway-1", "reporter@example.test", true)
	sess, err := f.srv.ext.Agents.CreateSession(f.asOwner, f.chief.ID, f.chief.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.plugins.SetChiefSession(f.asOwner, p.ID, f.chief.ID, sess.ID); err != nil {
		t.Fatal(err)
	}

	text := f.srv.AgentToolDispatch(f.asOwner, f.chief)(routeToSessionToolName, routeArgs(m, "mirror:"+routeSID))
	var res struct {
		RouteID string `json:"route_id"`
		State   string `json:"state"`
		OneWay  bool   `json:"one_way"`
		Next    string `json:"next"`
	}
	if err := json.Unmarshal([]byte(text), &res); err != nil || !plugins.ValidRouteID(res.RouteID) || res.State != "routed" || !res.OneWay ||
		!strings.Contains(res.Next, "the sender is never contacted") {
		t.Fatalf("route_to_session of a no_reply message: %s; want a one-way route", text)
	}

	// The paste.
	items := f.queued(t)
	if len(items) != 1 {
		t.Fatalf("%d queued rows, want 1", len(items))
	}
	body := items[0].Body
	for _, want := range []string{
		"NO REPLY POSSIBLE: the sender cannot be answered, and nothing you write reaches them.",
		"The block below is an UNTRUSTED external message",
		"To report back to the chief — not the sender —",
		"route_id " + res.RouteID, "Your answer goes to the chief only.",
		"|   body: Clicking export on the billing page does nothing.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the one-way paste lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "is sent to the sender as written") {
		t.Errorf("the one-way paste tells the session its answer goes to the sender:\n%s", body)
	}
	if _, err := mirror.SanitizePaste(body); err != nil {
		t.Errorf("the one-way paste cannot be pasted: %v", err)
	}

	// The session's answer: relayed to the chief, never a draft.
	code, out := f.reply(t, f.token, res.RouteID, "Already on it: the export handler drops the click. Fix in progress.")
	if code != 200 || out["state"] != "relayed" || out["draft_id"] != nil ||
		out["next"] != "your answer goes to your owner's chief only; the message's sender cannot be answered and is not contacted" {
		t.Fatalf("the session's answer: %d %v", code, out)
	}
	for i := 0; i < 2; i++ {
		if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	msgs, err := f.srv.ext.Agents.ListChatMessages(f.asOwner, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantHead := "[muster] Route " + res.RouteID + " (plugin message_id " + strconv.FormatInt(m.ID, 10) +
		", one-way: no reply possible → mirror:" + routeSID + "): the session answered your heads-up. The message's sender was not contacted and cannot be."
	var answers int
	for _, cm := range msgs {
		if strings.HasPrefix(cm.Content, wantHead) {
			answers++
			if !strings.Contains(cm.Content, "Already on it: the export handler drops the click.") ||
				!strings.Contains(cm.Content, "it is for you, never for the sender") {
				t.Errorf("the chief's answer row:\n%s", cm.Content)
			}
		}
	}
	if answers != 1 {
		t.Fatalf("the chief's plugin session holds %d answer rows, want exactly 1:\n%+v", answers, msgs)
	}

	// The sender is never contacted: no draft, nothing claimable, reply_draft refused.
	if n := f.draftsFor(t, m.ID); n != 0 {
		t.Fatalf("%d reply drafts exist for the no_reply message", n)
	}
	if _, ok, err := f.plugins.ClaimDraft(f.asOwner, p.ID); ok || err != nil {
		t.Fatalf("the plugin's claim found something to send (%v %v)", ok, err)
	}
	if out := f.srv.AgentToolDispatch(f.asOwner, f.chief)("reply_draft", `{"message_id":`+strconv.FormatInt(m.ID, 10)+`,"body":"Thanks!"}`); !isToolErr(out) ||
		!strings.Contains(out, "cannot be replied to") {
		t.Fatalf("reply_draft for the no_reply message: %s", out)
	}
	if r, err := f.plugins.Route(f.asOwner, res.RouteID); err != nil || r.State != plugins.RouteRelayed || r.DraftID != nil {
		t.Fatalf("the route: %+v %v", r, err)
	}
}

// TestAOneWayAnswerFallsBackToTheChiefsDefaultSession: with no plugin chief
// session (the plugin's chief session was never set), a one-way route's
// answer — OWED to the chief — lands in the chief's default session instead
// of staying owed for ever.
func TestAOneWayAnswerFallsBackToTheChiefsDefaultSession(t *testing.T) {
	f := newRouteFixture(t)
	_, m := f.noReplyMessage(t, "fb-oneway-default", "reporter@example.test", false)
	id := f.route(t, m)
	if code, out := f.reply(t, f.token, id, "Noted; it is the same bug I am fixing."); code != 200 || out["state"] != "relayed" {
		t.Fatalf("answer: %d %v", code, out)
	}
	if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.chiefDefaultSessionText(t); !strings.Contains(got, "the session answered your heads-up") ||
		!strings.Contains(got, "Noted; it is the same bug I am fixing.") {
		t.Fatalf("the chief's default session does not hold the answer:\n%s", got)
	}
	var owed bool
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT handback IS NOT NULL FROM session_routes WHERE route_id = $1`, id).Scan(&owed); err != nil || owed {
		t.Fatalf("the answer is still owed after delivery (%v)", err)
	}
}

// TestATimedOutOneWayRouteComesBackWithoutAReplyChoice: a one-way route that
// times out is handed back to the chief offering what it can do with a
// message nobody can answer — never reply_draft.
func TestATimedOutOneWayRouteComesBackWithoutAReplyChoice(t *testing.T) {
	f := newRouteFixture(t)
	p, m := f.noReplyMessage(t, "fb-oneway-timeout", "reporter@example.test", false)
	sess, err := f.srv.ext.Agents.CreateSession(f.asOwner, f.chief.ID, f.chief.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.plugins.SetChiefSession(f.asOwner, p.ID, f.chief.ID, sess.ID); err != nil {
		t.Fatal(err)
	}
	id := f.route(t, m)
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE session_routes SET timeout_at = now() - interval '1 minute' WHERE route_id = '`+id+`'`)
	if n, err := f.srv.RouteTimeoutPass(context.Background()); err != nil || n < 1 {
		t.Fatalf("past the deadline: %d timed out (%v)", n, err)
	}
	msgs, err := f.srv.ext.Agents.ListChatMessages(f.asOwner, sess.ID)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("the chief's session: %+v %v", msgs, err)
	}
	got := msgs[0].Content
	if !strings.Contains(got, "so the message is back with you: it still cannot be answered, so route it to another session with "+
		"route_to_session, file the work with task_create_gated and task_route_attention, or settle it with plugin_message_settle.") ||
		strings.Contains(got, "reply_draft") {
		t.Fatalf("the hand-back:\n%s", got)
	}
}

// TestOnlyTheChiefsOwnRouteTakesATaskEventByItsTask: a TASK EVENT is the one
// no_reply message the chief cannot route one-way by message_id: refused,
// pointing at its task, writing nothing. (The owner's own door,
// plugin_message_route, still refuses every no_reply message:
// plugin_message_route_tools_pg_test.go.)
func TestOnlyTheChiefsOwnRouteTakesATaskEventByItsTask(t *testing.T) {
	f := newRouteFixture(t)
	n := f.newTask(t, f.owner, "Fix the export button")
	ps, _ := f.plugins.List(f.asOwner)
	var pid int64
	if len(ps) > 0 {
		pid = ps[0].ID
	} else {
		_, p, err := f.plugins.Create(f.asOwner, "example-tracker", "Example tracker", "example-tracker-data")
		if err != nil {
			t.Fatal(err)
		}
		pid = p.ID
	}
	in, err := plugins.NewTaskEventIn(plugins.TaskEvent{Event: "assigned", TaskID: n.ID, Key: "EX-1", Status: "todo", Title: n.Title}, "ev-1")
	if err != nil {
		t.Fatal(err)
	}
	ev, _, err := f.plugins.SubmitTaskEvent(f.asOwner, pid, n.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.plugins.BeginDelivery(f.asOwner, ev.ID, f.chief.ID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	before, rows := f.ownersRoutesAndRows(t)
	text := f.srv.AgentToolDispatch(f.asOwner, f.chief)(routeToSessionToolName, routeArgs(ev, "mirror:"+routeSID))
	want := routeRefusal(RefusedNoReply, "message_id "+strconv.FormatInt(ev.ID, 10)+" is a task event: route its task instead, with task_id "+
		strconv.FormatInt(n.ID, 10))
	if text != want {
		t.Fatalf("route_to_session of a task event by message_id:\n%s\nwant:\n%s", text, want)
	}
	if after, rows2 := f.ownersRoutesAndRows(t); after != before || rows2 != rows {
		t.Fatalf("a refused route wrote routes %d→%d, rows %d→%d", before, after, rows, rows2)
	}
}

// 🔴 TestTheOwnerIsToldOnceOfEachRouteTheChiefMakes: a message route (two-way
// and one-way) and a task route made by the chief each send the owner exactly
// ONE `plugin` notification naming the session (its tmux place) and what it
// got; a refused route sends none. (A request route, the owner's own ask, sends
// none either: TestTheOwnersOwnRequestRouteSendsNoRouteNotice.)
func TestTheOwnerIsToldOnceOfEachRouteTheChiefMakes(t *testing.T) {
	f := newRouteFixture(t)
	pusher, dev := f.withPush(t)
	gw := f.srv.AgentToolDispatch(f.asOwner, f.chief)
	where := "tmux work:1 @ tower"

	// A refusal (a window no host shows) sends nothing.
	two := f.deliveredMessage(t, f.owner, f.chief.ID, "notify-two-way")
	if text := gw(routeToSessionToolName, routeArgs(two, "mirror:nosuchsession")); !strings.Contains(text, RefusedWindowGone) {
		t.Fatalf("control: %s", text)
	}
	if got := routePushes(f, pusher, dev); len(got) != 0 {
		t.Fatalf("a refused route notified the owner: %+v", got)
	}

	cases := []struct {
		name, body string
		route      func() string
	}{
		{"two-way message", "Example inbox: (no subject)", func() string { return f.route(t, two) }},
		{"one-way message", "Example feedback: " + oneWaySubject + " (no reply possible: a heads-up)", func() string {
			_, m := f.noReplyMessage(t, "notify-one-way", "reporter@example.test", false)
			return f.route(t, m)
		}},
		{"task", "", func() string { return f.routeTask(t, f.newTask(t, f.owner, "Fix the export button")) }},
	}
	seen := 0
	for _, c := range cases {
		id := c.route()
		got := routePushes(f, pusher, dev)
		if len(got) != seen+1 {
			t.Fatalf("%s: %d route pushes after it, want %d: %+v", c.name, len(got), seen+1, got)
		}
		pl := got[seen]
		seen++
		if pl.Tag != "route-"+id || pl.Type != "notify" || pl.Title != "Chief routed it to "+where {
			t.Errorf("%s: push %+v; want tag route-%s, type notify, title naming %q", c.name, pl, id, where)
		}
		if c.name == "task" {
			if !strings.HasPrefix(pl.Body, "Task #") || !strings.HasSuffix(pl.Body, ": Fix the export button") ||
				!strings.HasPrefix(pl.Data["url"], "/tasks/") {
				t.Errorf("task: push %+v", pl)
			}
			continue
		}
		if pl.Body != c.body || !strings.HasPrefix(pl.Data["url"], "/mirror/view?") {
			t.Errorf("%s: push body %q url %q; want %q and the session's view", c.name, pl.Body, pl.Data["url"], c.body)
		}
	}
	// Exactly once: the sweep, the answer and its relay notify nothing more.
	if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := routePushes(f, pusher, dev); len(got) != seen {
		t.Fatalf("%d route pushes after the sweep, want %d", len(got), seen)
	}
}

// TestTheRouteNoticeIsMutedWithThePluginToggle: muting the `plugin` event on
// /account silences it — the one switch, and no other is needed.
func TestTheRouteNoticeIsMutedWithThePluginToggle(t *testing.T) {
	f := newRouteFixture(t)
	pusher, store := &payloadPusher{}, &memPush{muted: map[int64][]string{}}
	dev := ep("route-muted-" + strconv.FormatInt(f.owner, 10))
	claim(t, store, f.owner, dev)
	store.muted[f.owner] = []string{push.EventPlugin}
	f.srv.ext.Push = push.NewService(store, pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))
	f.route(t, f.deliveredMessage(t, f.owner, f.chief.ID, "notify-muted"))
	if got := routePushes(f, pusher, dev); len(got) != 0 {
		t.Fatalf("a muted owner got %+v", got)
	}
	store.mu.Lock()
	store.muted[f.owner] = nil
	store.mu.Unlock()
	f.route(t, f.deliveredMessage(t, f.owner, f.chief.ID, "notify-unmuted"))
	if got := routePushes(f, pusher, dev); len(got) != 1 {
		t.Fatalf("control, unmuted: %d route pushes, want 1", len(got))
	}
}

// TestTheOwnersOwnRequestRouteSendsNoRouteNotice: a request route (the owner
// typed it, owner_asked) is not the chief's own decision: no notice. CONTROL:
// in the same fixture, the chief's message route sends one.
func TestTheOwnersOwnRequestRouteSendsNoRouteNotice(t *testing.T) {
	f, _ := newRequestRouteFixture(t, true)
	pusher, dev := f.withPush(t)
	f.typed(t, f.owner, "Send this to whoever is on the changelog: "+ownersIdea)
	if text, isErr := f.chiefMCP(t, routeToSessionToolName, requestArgs(ownersIdea, "mirror:"+routeSID)); isErr || !strings.Contains(text, `"state":"routed"`) {
		t.Fatalf("the owner's request route: %s", text)
	}
	if got := routePushes(f, pusher, dev); len(got) != 0 {
		t.Fatalf("the owner's own request route notified them: %+v", got)
	}
	f.route(t, f.deliveredMessage(t, f.owner, f.chief.ID, "notify-control"))
	if got := routePushes(f, pusher, dev); len(got) != 1 {
		t.Fatalf("control: the chief's message route sent %d route pushes, want 1", len(got))
	}
}

// 🔴 TestATaskEventsHintIsTheTasksWordsWithoutItsAgents: an incoming task
// event's session hint is matched on the TASK as stored (its title and body),
// not the event's JSON, and leaves out the agents bound to the task — those the
// event recorded and those bound since. CONTROL: with nobody bound, the worker
// on the job is named.
func TestATaskEventsHintIsTheTasksWordsWithoutItsAgents(t *testing.T) {
	g, b, _, _, _ := overviewReplay(t)
	worker := "agent:" + strconv.FormatInt(g.worker.ID, 10)
	n, err := g.notes.Create(g.asOwner, notes.Note{Title: "node-7k upgrade", Body: overviewMessage})
	if err != nil {
		t.Fatal(err)
	}
	event := func(bound ...int64) plugins.Message {
		ev := plugins.TaskEvent{Event: "assigned", TaskID: n.ID, Key: "EX-7", Status: "todo", Title: "unrelated words only"}
		for _, id := range bound {
			ev.Agents = append(ev.Agents, plugins.TaskEventAgent{ID: id, Name: "x", Status: "running"})
		}
		body, _ := json.Marshal(ev)
		return plugins.Message{ID: 9, TaskID: &n.ID, Subject: "task assigned", Body: string(body), NoReply: true}
	}
	ids := func(h plugins.Hints) []string {
		var out []string
		for _, s := range h.Sessions {
			out = append(out, s.ID)
		}
		return out
	}
	// Control: nobody bound — the worker on node-7k is named, from the task's
	// own words (the event's title shares none).
	if h := g.srv.pluginSessionHints(g.asOwner, g.chief, event()); len(h.Sessions) != 1 || h.Sessions[0].ID != worker || h.Author != nil {
		t.Fatalf("control: the task event's hint %v; want exactly %s", ids(h), worker)
	}
	// Bound by the event's own record: left out.
	if h := g.srv.pluginSessionHints(g.asOwner, g.chief, event(g.worker.ID)); len(h.Sessions) != 0 {
		t.Fatalf("the agent the event names as on the task is hinted: %v", ids(h))
	}
	// Bound since (its task is this one), not in the event: left out too.
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE agents SET note_id = `+strconv.FormatInt(n.ID, 10)+` WHERE id = `+strconv.FormatInt(g.worker.ID, 10))
	if h := g.srv.pluginSessionHints(g.asOwner, g.chief, event(b.ID)); len(h.Sessions) != 0 {
		t.Fatalf("an agent bound to the task now is hinted: %v", ids(h))
	}
}
