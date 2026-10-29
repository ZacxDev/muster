package api

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// A REPLY SENT DIRECTLY, STAMPED WITH ITS MESSAGE (owner_replies.go, migration
// 0062), through the real server against a real Postgres: a plugin's POST
// /plugin/v1/messages/{id}/owner-answered ends THAT message's route
// owner_answered when it is still routed, and changes nothing else — not a
// route that already ended any other way, not a message never routed, and not
// another plugin's or another owner's message (404). And the deadline a
// person's conversation gets (human_conversation).
//
// Written against the HTTP door and the stored rows (state, owner_replied_at
// read by SQL) so the same file compiles at the commit before the feature and
// shows each case red there.

// stateOwnerAnswered is plugins.RouteOwnerAnswered, spelled as the schema
// stores it.
const stateOwnerAnswered = plugins.RouteState("owner_answered")

// ownerAnsweredFixture is a route fixture with a plugin and its token.
type ownerAnsweredFixture struct {
	*routeFixture
	tok string
	p   plugins.Plugin
}

func newOwnerAnsweredFixture(t *testing.T) *ownerAnsweredFixture {
	t.Helper()
	f := &ownerAnsweredFixture{routeFixture: newRouteFixture(t)}
	f.tok, f.p = f.mintPlugin(t, f.owner, "example-dm")
	f.chiefSession(t, f.p)
	return f
}

// mintPlugin creates owner's plugin name and returns its token.
func (f *ownerAnsweredFixture) mintPlugin(t *testing.T, owner int64, name string) (string, plugins.Plugin) {
	t.Helper()
	tok, p, err := f.plugins.Create(f.as(owner), name, "Example "+name, name+"-data")
	if err != nil {
		t.Fatal(err)
	}
	return tok, p
}

// message POSTs one message through plugin token tok, with extra fields
// merged in, and settles it as delivered to the chief: a message a route can
// take.
func (f *ownerAnsweredFixture) message(t *testing.T, tok, external string, extra map[string]any) plugins.Message {
	t.Helper()
	in := map[string]any{"external_id": external, "sender": "Someone (Example DM 1)", "subject": "Example DM: 1 message",
		"body": "can you look at this?"}
	for k, v := range extra {
		in[k] = v
	}
	b, _ := json.Marshal(in)
	w := f.call(t, "POST", "/plugin/v1/messages", strings.NewReader(string(b)), map[string]string{"Authorization": "Bearer " + tok})
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 202 || out.ID == 0 {
		t.Fatalf("post %s: %d %s", external, w.Code, w.Body.String())
	}
	if ok, err := f.plugins.BeginDelivery(f.asOwner, out.ID, f.chief.ID); err != nil || !ok {
		t.Fatalf("begin delivery: %v %v", ok, err)
	}
	if ok, err := f.plugins.RecordDelivery(f.asOwner, out.ID, f.chief.ID, plugins.StatusDelivered); err != nil || !ok {
		t.Fatalf("record delivery: %v %v", ok, err)
	}
	m, err := f.plugins.Message(f.asOwner, out.ID)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// stampAnswer is the plugin's report: the status and the answer's fields.
type stampAnswer struct {
	Code    int
	Settled bool   `json:"settled"`
	State   string `json:"state"`
	RouteID string `json:"route_id"`
	Error   string `json:"error"`
}

func (f *ownerAnsweredFixture) stamp(t *testing.T, tok string, messageID int64) stampAnswer {
	t.Helper()
	w := f.call(t, "POST", "/plugin/v1/messages/"+strconv.FormatInt(messageID, 10)+"/owner-answered", strings.NewReader(""),
		map[string]string{"Authorization": "Bearer " + tok})
	var a stampAnswer
	_ = json.Unmarshal(w.Body.Bytes(), &a)
	a.Code = w.Code
	return a
}

// ownerRepliedAt is route id's owner_replied_at (nil for none), read by SQL.
func (f *ownerAnsweredFixture) ownerRepliedAt(t *testing.T, id string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT owner_replied_at FROM session_routes WHERE route_id = $1`, id).Scan(&at); err != nil {
		t.Fatalf("owner_replied_at: %v", err)
	}
	return at
}

// chiefNotes is the text of the chief's conversation for the plugin.
func (f *ownerAnsweredFixture) chiefNotes(t *testing.T) string {
	t.Helper()
	p, err := f.plugins.Plugin(f.asOwner, f.p.ID)
	if err != nil || p.ChiefSessionID == nil {
		t.Fatalf("the plugin's chief session: %+v %v", p, err)
	}
	msgs, err := f.srv.ext.Agents.ListChatMessages(f.asOwner, *p.ChiefSessionID)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Content + "\n")
	}
	return b.String()
}

func TestAStampedReplySettlesAWaitingRoute(t *testing.T) {
	f := newOwnerAnsweredFixture(t)
	m := f.message(t, f.tok, "stamp-wait-1", nil)
	id := f.route(t, m)
	r := f.routeState(t, id)
	if r.SendQueueID == nil {
		t.Fatalf("the route has no paste: %+v", r)
	}
	before := time.Now().Add(-time.Second)
	if a := f.stamp(t, f.tok, m.ID); a.Code != 200 || !a.Settled || a.State != "owner_answered" || a.RouteID != id {
		t.Fatalf("the stamp: %+v; want 200 settling %s", a, id)
	}
	got := f.routeState(t, id)
	if got.State != stateOwnerAnswered || got.FinishedAt == nil {
		t.Fatalf("the route is %s (finished %v); want owner_answered, finished", got.State, got.FinishedAt)
	}
	if ra := f.ownerRepliedAt(t, id); ra == nil || ra.Before(before) || ra.After(time.Now().Add(time.Second)) {
		t.Fatalf("owner_replied_at %v; want muster's now", ra)
	}
	if hb := f.handBackOwed(t, id); hb != "" {
		t.Fatalf("a settled route owes a hand-back: %q", hb)
	}
	if it := f.queueRow(t, *r.SendQueueID); it.State != sendqueue.Cancelled {
		t.Fatalf("its paste is %s; want cancelled", it.State)
	}
	if notes := f.chiefNotes(t); !strings.Contains(notes, "Route "+id) || !strings.Contains(notes, "sent to the sender directly") {
		t.Fatalf("the chief was not told:\n%s", notes)
	}
	// The deadline passes: the sweep has nothing to time out or hand back.
	ageRoute(t, id, 31*time.Minute)
	if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.routeState(t, id); got.State != stateOwnerAnswered {
		t.Fatalf("after the sweep the route is %s; want owner_answered", got.State)
	}
	if strings.Contains(f.chiefNotes(t), "back with you") {
		t.Fatalf("the chief was handed back an answered message:\n%s", f.chiefNotes(t))
	}
	// The session's late answer is refused: the route is closed.
	if code, _ := f.reply(t, f.token, id, "late answer"); code != 409 {
		t.Fatalf("a late route reply: %d; want 409", code)
	}
	// The same stamp again: 200, nothing more settled.
	if a := f.stamp(t, f.tok, m.ID); a.Code != 200 || a.Settled || a.State != "owner_answered" {
		t.Fatalf("a repeated stamp: %+v; want 200, settled false, owner_answered", a)
	}
}

// A stamp on a message whose route already ended — timed out (and handed
// back), answered through route_reply, failed — or that was never routed
// changes nothing: 409 naming the state. The control at the end: the same
// fixture's waiting route IS settled, so each 409 is the state's, not a door
// that settles nothing.
func TestAStampOnARouteThatIsNotWaitingChangesNothing(t *testing.T) {
	f := newOwnerAnsweredFixture(t)

	timedOut := f.message(t, f.tok, "stamp-not-1", nil)
	tid := f.route(t, timedOut)
	ageRoute(t, tid, 31*time.Minute)
	if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.routeState(t, tid); got.State != plugins.RouteTimedOut || !strings.Contains(f.chiefNotes(t), "back with you") {
		t.Fatalf("CONTROL: the route is %s; want timed out and handed back", got.State)
	}

	answered := f.message(t, f.tok, "stamp-not-2", nil)
	aid := f.route(t, answered)
	if code, _ := f.reply(t, f.token, aid, "the session's answer"); code != 200 {
		t.Fatalf("CONTROL: the session's route reply: %d", code)
	}
	answeredState := f.routeState(t, aid).State

	failed := f.message(t, f.tok, "stamp-not-3", nil)
	fid := f.route(t, failed)
	if _, err := f.plugins.FailRoute(f.asOwner, fid, "the window moved on", ""); err != nil {
		t.Fatal(err)
	}

	never := f.message(t, f.tok, "stamp-not-4", nil)

	for _, c := range []struct {
		name    string
		m       plugins.Message
		routeID string
		want    plugins.RouteState
	}{
		{"timed out", timedOut, tid, plugins.RouteTimedOut},
		{"answered through route_reply", answered, aid, answeredState},
		{"failed", failed, fid, plugins.RouteFailed},
		{"never routed", never, "", "none"},
	} {
		a := f.stamp(t, f.tok, c.m.ID)
		if a.Code != 409 || a.Settled || a.State != string(c.want) || a.Error == "" {
			t.Errorf("%s: %+v; want 409, settled false, state %s", c.name, a, c.want)
		}
		if c.routeID != "" {
			if got := f.routeState(t, c.routeID); got.State != c.want || f.ownerRepliedAt(t, c.routeID) != nil {
				t.Errorf("%s: the route is now %s (owner_replied_at %v); want unchanged %s", c.name, got.State,
					f.ownerRepliedAt(t, c.routeID), c.want)
			}
		}
	}

	waiting := f.message(t, f.tok, "stamp-not-5", nil)
	wid := f.route(t, waiting)
	if a := f.stamp(t, f.tok, waiting.ID); a.Code != 200 || !a.Settled || a.RouteID != wid {
		t.Fatalf("CONTROL: the waiting route's stamp: %+v; want 200 settling %s", a, wid)
	}
}

// A message routed again after its first route timed out: the stamp settles
// its LATEST route — the one now waiting — and leaves the earlier one timed out.
func TestAStampSettlesTheMessagesLatestRoute(t *testing.T) {
	f := newOwnerAnsweredFixture(t)
	m := f.message(t, f.tok, "stamp-latest-1", nil)
	first := f.route(t, m)
	ageRoute(t, first, 31*time.Minute)
	if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := f.route(t, m)
	if a := f.stamp(t, f.tok, m.ID); a.Code != 200 || !a.Settled || a.RouteID != second {
		t.Fatalf("the stamp: %+v; want 200 settling the latest route %s", a, second)
	}
	if got := f.routeState(t, first); got.State != plugins.RouteTimedOut {
		t.Fatalf("the earlier route is %s; want still timed_out", got.State)
	}
}

// 🔴 Only the plugin that delivered the message settles it: another plugin of
// the same owner, another owner's plugin and a missing id are all 404, the
// route untouched. The control: the message's own plugin then settles it.
func TestAStampForAnotherPluginsOrOwnersMessageIsNotFound(t *testing.T) {
	f := newOwnerAnsweredFixture(t)
	otherTok, _ := f.mintPlugin(t, f.owner, "example-other")
	stranger := f.newMember(t)
	strangerTok, _ := f.mintPlugin(t, stranger, "example-dm")

	m := f.message(t, f.tok, "stamp-who-1", nil)
	id := f.route(t, m)
	for _, c := range []struct {
		name, tok string
		id        int64
	}{
		{"another plugin of the same owner", otherTok, m.ID},
		{"another owner's plugin, same name", strangerTok, m.ID},
		{"a message that does not exist", f.tok, m.ID + 1_000_000},
	} {
		if a := f.stamp(t, c.tok, c.id); a.Code != 404 || a.Settled {
			t.Errorf("%s: %+v; want 404", c.name, a)
		}
		if got := f.routeState(t, id); got.State != plugins.RouteRouted {
			t.Fatalf("%s: the route is %s; want still routed", c.name, got.State)
		}
	}
	// A personal token is not a plugin's.
	if a := f.stamp(t, f.token, m.ID); a.Code != 401 {
		t.Fatalf("the owner's personal token: %+v; want 401", a)
	}
	if a := f.stamp(t, f.tok, m.ID); a.Code != 200 || !a.Settled || a.RouteID != id {
		t.Fatalf("CONTROL: the message's own plugin: %+v; want 200 settling %s", a, id)
	}
}

// A PERSON'S CONVERSATION WAITS 4H (ConversationRouteTimeout): a message its
// plugin marks human_conversation gets the long deadline; the same plugin's
// unmarked message keeps 30m.
func TestAHumanConversationRouteHasTheFourHourDeadline(t *testing.T) {
	f := newOwnerAnsweredFixture(t)
	marked := f.message(t, f.tok, "stamp-deadline-1", map[string]any{"human_conversation": true})
	deadlineIs(t, "a human_conversation message's route", f.routeState(t, f.route(t, marked)), 4*time.Hour)
	unmarked := f.message(t, f.tok, "stamp-deadline-2", nil)
	deadlineIs(t, "an unmarked message's route", f.routeState(t, f.route(t, unmarked)), 30*time.Minute)
}

func TestAnAgentMailRouteKeepsTheThirtyMinuteDeadline(t *testing.T) {
	f := newOwnerAnsweredFixture(t)
	tok, _ := f.mintPlugin(t, f.owner, plugins.AgentMailPlugin)
	m := f.message(t, tok, "stamp-mail-1", map[string]any{"thread": "7777"})
	deadlineIs(t, "an agent-mail route", f.routeState(t, f.route(t, m)), 30*time.Minute)
}
