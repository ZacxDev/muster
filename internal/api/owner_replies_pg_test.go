package api

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// THE OWNER ANSWERED IT THEMSELVES (owner_replies.go, migration 0062), through
// the real server against a real Postgres: a plugin's POST
// /plugin/v1/owner-replies {thread, at} ends the routes that reply answers
// owner_answered — a waiting one, and one that timed out at most
// OwnerReplyGrace before it — and no other: not one on another thread, of
// another plugin, of another owner, made after the reply, or timed out longer
// ago. And the deadline a person's conversation gets (human_conversation).
//
// Written against the HTTP door and the stored rows (state, owner_replied_at,
// handback read by SQL) so the same file compiles at the commit before the
// feature and shows each case red there.

// stateOwnerAnswered is plugins.RouteOwnerAnswered, spelled as the schema
// stores it.
const stateOwnerAnswered = plugins.RouteState("owner_answered")

// ownerReplyFixture is a route fixture with a plugin whose messages carry a
// thread, and its token.
type ownerReplyFixture struct {
	*routeFixture
	tok string
	p   plugins.Plugin
}

func newOwnerReplyFixture(t *testing.T) *ownerReplyFixture {
	t.Helper()
	f := &ownerReplyFixture{routeFixture: newRouteFixture(t)}
	f.tok, f.p = f.mintPlugin(t, f.owner, "example-dm")
	f.chiefSession(t, f.p)
	return f
}

// mintPlugin creates owner's plugin name and returns its token.
func (f *ownerReplyFixture) mintPlugin(t *testing.T, owner int64, name string) (string, plugins.Plugin) {
	t.Helper()
	tok, p, err := f.plugins.Create(f.as(owner), name, "Example "+name, name+"-data")
	if err != nil {
		t.Fatal(err)
	}
	return tok, p
}

// threadMessage POSTs one message on thread through plugin token tok, with
// extra fields merged in, and settles it as delivered to the chief: the
// message a route can take.
func (f *ownerReplyFixture) threadMessage(t *testing.T, tok, external, thread string, extra map[string]any) plugins.Message {
	t.Helper()
	in := map[string]any{"external_id": external, "sender": "Someone (Example DM 1)", "subject": "Example DM: 1 message",
		"body": "can you look at this?", "thread": thread}
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

// ownerReply is the plugin's report: the status and the settled route ids.
func (f *ownerReplyFixture) ownerReply(t *testing.T, tok, thread string, at time.Time) (int, []string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"thread": thread, "at": at.UTC().Format(time.RFC3339Nano)})
	w := f.call(t, "POST", "/plugin/v1/owner-replies", strings.NewReader(string(b)), map[string]string{"Authorization": "Bearer " + tok})
	var out struct {
		Settled []string `json:"settled"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out.Settled
}

// ownerRepliedAt is route id's owner_replied_at (nil for none), read by SQL.
func (f *ownerReplyFixture) ownerRepliedAt(t *testing.T, id string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT owner_replied_at FROM session_routes WHERE route_id = $1`, id).Scan(&at); err != nil {
		t.Fatalf("owner_replied_at: %v", err)
	}
	return at
}

// timeOut times route id out as the sweep's store call does, WITHOUT the
// sweep's delivery of the hand-back: a route timed out and still owing it.
// Its deadline is moved into the past first.
func (f *ownerReplyFixture) timeOut(t *testing.T, id string) {
	t.Helper()
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE session_routes SET created_at = created_at - interval '5 hours',
		timeout_at = now() - interval '1 second' WHERE route_id = '`+id+`'`)
	if _, timedOut, err := f.plugins.TimeOutRoute(f.asOwner, id); err != nil || !timedOut {
		t.Fatalf("time out %s: %v %v", id, timedOut, err)
	}
}

// endedAgo moves a timed-out route's finished_at d into the past: it timed
// out d ago.
func endedAgo(t *testing.T, id string, d time.Duration) {
	t.Helper()
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE session_routes SET finished_at = now() - make_interval(secs => `+
		strconv.Itoa(int(d/time.Second))+`) WHERE route_id = '`+id+`'`)
}

// chiefNotes is the text of the chief's conversation for the plugin.
func (f *ownerReplyFixture) chiefNotes(t *testing.T) string {
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

func TestAnOwnerReplySettlesAWaitingRoute(t *testing.T) {
	f := newOwnerReplyFixture(t)
	m := f.threadMessage(t, f.tok, "own-wait-1", "dm-111", nil)
	id := f.route(t, m)
	r := f.routeState(t, id)
	if r.SendQueueID == nil {
		t.Fatalf("the route has no paste: %+v", r)
	}
	// Routed a minute ago; the owner replied just now.
	ageRoute(t, id, time.Minute)
	at := time.Now().Truncate(time.Second)
	code, settled := f.ownerReply(t, f.tok, "dm-111", at)
	if code != 200 || len(settled) != 1 || settled[0] != id {
		t.Fatalf("the owner's reply: %d %v; want 200 settling %s", code, settled, id)
	}
	got := f.routeState(t, id)
	if got.State != stateOwnerAnswered || got.FinishedAt == nil || got.Error != "" {
		t.Fatalf("the route is %s (finished %v, error %q); want owner_answered, finished, no error", got.State, got.FinishedAt, got.Error)
	}
	if ra := f.ownerRepliedAt(t, id); ra == nil || !ra.Equal(at) {
		t.Fatalf("owner_replied_at %v; want %v", ra, at)
	}
	if hb := f.handBackOwed(t, id); hb != "" {
		t.Fatalf("an owner-answered route owes a hand-back: %q", hb)
	}
	if it := f.queueRow(t, *r.SendQueueID); it.State != sendqueue.Cancelled {
		t.Fatalf("its paste is %s; want cancelled", it.State)
	}
	if !strings.Contains(f.chiefNotes(t), "Route "+id) || !strings.Contains(f.chiefNotes(t), "answered the sender directly") {
		t.Fatalf("the chief was not told:\n%s", f.chiefNotes(t))
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
		t.Fatalf("the chief was handed back a message the owner answered:\n%s", f.chiefNotes(t))
	}
	// The session's late answer is refused: the route is closed.
	if code, _ := f.reply(t, f.token, id, "late answer"); code != 409 {
		t.Fatalf("a late route reply: %d; want 409", code)
	}
	// The same report again settles nothing more.
	if code, settled := f.ownerReply(t, f.tok, "dm-111", at); code != 200 || len(settled) != 0 {
		t.Fatalf("a repeated report: %d %v; want 200, nothing", code, settled)
	}
}

// 🔴 A route that timed out SHORTLY before the owner's reply — the hand-back
// still owed — is settled, and the hand-back withdrawn, so the chief never
// gets it. Measured at 37 minutes after the deadline (the case that was seen)
// and at 1h55m, inside OwnerReplyGrace's edge.
func TestAnOwnerReplySettlesARouteThatJustTimedOut(t *testing.T) {
	for _, ago := range []time.Duration{37 * time.Minute, 115 * time.Minute} {
		t.Run(ago.String(), func(t *testing.T) {
			f := newOwnerReplyFixture(t)
			thread := "dm-222-" + strconv.Itoa(int(ago/time.Minute))
			id := f.route(t, f.threadMessage(t, f.tok, "own-late-"+thread, thread, nil))
			f.timeOut(t, id)
			if hb := f.handBackOwed(t, id); hb == "" {
				t.Fatal("CONTROL: the timed-out route owes no hand-back")
			}
			endedAgo(t, id, ago)
			code, settled := f.ownerReply(t, f.tok, thread, time.Now())
			if code != 200 || len(settled) != 1 || settled[0] != id {
				t.Fatalf("%v after the deadline: %d %v; want 200 settling %s", ago, code, settled, id)
			}
			if got := f.routeState(t, id); got.State != stateOwnerAnswered {
				t.Fatalf("the route is %s; want owner_answered", got.State)
			}
			if hb := f.handBackOwed(t, id); hb != "" {
				t.Fatalf("the hand-back is still owed: %q", hb)
			}
			if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
				t.Fatal(err)
			}
			notes := f.chiefNotes(t)
			if strings.Contains(notes, "back with you") || !strings.Contains(notes, "answered the sender directly") {
				t.Fatalf("the chief's conversation:\n%s\nwant the owner's answer and no hand-back", notes)
			}
		})
	}
}

// A route whose hand-back was ALREADY delivered is still settled: the
// hand-back stands, the row says the owner answered, and the chief is told.
func TestAnOwnerReplySettlesARouteAlreadyHandedBack(t *testing.T) {
	f := newOwnerReplyFixture(t)
	id := f.route(t, f.threadMessage(t, f.tok, "own-handed-1", "dm-333", nil))
	ageRoute(t, id, 31*time.Minute)
	if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.routeState(t, id); got.State != plugins.RouteTimedOut || f.handBackOwed(t, id) != "" ||
		!strings.Contains(f.chiefNotes(t), "back with you") {
		t.Fatalf("CONTROL: the route is %s, hand-back owed %q; want timed out and handed back", got.State, f.handBackOwed(t, id))
	}
	if code, settled := f.ownerReply(t, f.tok, "dm-333", time.Now()); code != 200 || len(settled) != 1 {
		t.Fatalf("the owner's reply: %d %v; want 200 settling it", code, settled)
	}
	if got := f.routeState(t, id); got.State != stateOwnerAnswered {
		t.Fatalf("the route is %s; want owner_answered", got.State)
	}
	if !strings.Contains(f.chiefNotes(t), "after it came back to you") {
		t.Fatalf("the chief was not told the owner answered it after the hand-back:\n%s", f.chiefNotes(t))
	}
}

// A reply that answers NO route leaves each as it is — and the same fixture's
// matching report then settles it, so each refusal is the predicate's, not a
// fixture that could never settle anything.
func TestAnOwnerReplyLeavesRoutesItDoesNotAnswer(t *testing.T) {
	f := newOwnerReplyFixture(t)
	otherTok, _ := f.mintPlugin(t, f.owner, "example-other")
	stranger := f.newMember(t)
	strangerTok, _ := f.mintPlugin(t, stranger, "example-dm")

	waiting := f.route(t, f.threadMessage(t, f.tok, "own-no-1", "dm-444", nil))
	for _, c := range []struct {
		name, tok, thread string
		at                time.Time
	}{
		{"another thread", f.tok, "dm-445", time.Now()},
		{"another plugin of the same owner, same thread", otherTok, "dm-444", time.Now()},
		{"another owner's plugin, same name and thread", strangerTok, "dm-444", time.Now()},
		{"a reply from before the route was made", f.tok, "dm-444", time.Now().Add(-time.Hour)},
	} {
		code, settled := f.ownerReply(t, c.tok, c.thread, c.at)
		if code != 200 || len(settled) != 0 {
			t.Errorf("%s: %d %v; want 200 settling nothing", c.name, code, settled)
		}
		if got := f.routeState(t, waiting); got.State != plugins.RouteRouted {
			t.Fatalf("%s: the route is %s; want still routed", c.name, got.State)
		}
	}

	// Timed out PAST the grace: 2h05m before the reply.
	stale := f.route(t, f.threadMessage(t, f.tok, "own-no-2", "dm-446", nil))
	f.timeOut(t, stale)
	endedAgo(t, stale, 125*time.Minute)
	if code, settled := f.ownerReply(t, f.tok, "dm-446", time.Now()); code != 200 || len(settled) != 0 {
		t.Errorf("a route timed out 2h05m before the reply: %d %v; want 200 settling nothing", code, settled)
	}
	if got := f.routeState(t, stale); got.State != plugins.RouteTimedOut {
		t.Errorf("the stale route is %s; want still timed_out", got.State)
	}

	// The positive control: the right plugin, thread and time settle it.
	if code, settled := f.ownerReply(t, f.tok, "dm-444", time.Now()); code != 200 || len(settled) != 1 || settled[0] != waiting {
		t.Fatalf("CONTROL: the matching report: %d %v; want it to settle %s", code, settled, waiting)
	}
}

func TestAnOwnerReplyRefusesABadReport(t *testing.T) {
	f := newOwnerReplyFixture(t)
	id := f.route(t, f.threadMessage(t, f.tok, "own-bad-1", "dm-555", nil))
	for _, c := range []struct{ name, body string }{
		{"no thread", `{"at":"2000-01-01T00:00:00Z"}`},
		{"no time", `{"thread":"dm-555"}`},
		{"a thread with a space", `{"thread":"dm 555","at":"2000-01-01T00:00:00Z"}`},
		{"a time an hour ahead", `{"thread":"dm-555","at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`},
	} {
		w := f.call(t, "POST", "/plugin/v1/owner-replies", strings.NewReader(c.body), map[string]string{"Authorization": "Bearer " + f.tok})
		if w.Code != 400 {
			t.Errorf("%s: %d; want 400", c.name, w.Code)
		}
	}
	if got := f.routeState(t, id); got.State != plugins.RouteRouted {
		t.Fatalf("a refused report changed the route to %s", got.State)
	}
	// An agent's or the owner's credential is not a plugin's.
	if w := f.call(t, "POST", "/plugin/v1/owner-replies", strings.NewReader(`{"thread":"dm-555","at":"2000-01-01T00:00:00Z"}`),
		map[string]string{"Authorization": "Bearer " + f.token}); w.Code != 401 {
		t.Fatalf("the owner's personal token: %d; want 401", w.Code)
	}
}

// A PERSON'S CONVERSATION WAITS 4H (ConversationRouteTimeout): a message its
// plugin marks human_conversation gets the long deadline; the same plugin's
// unmarked message on a thread — as agent mail is — keeps 30m.
func TestAHumanConversationRouteHasTheFourHourDeadline(t *testing.T) {
	f := newOwnerReplyFixture(t)
	marked := f.threadMessage(t, f.tok, "own-deadline-1", "dm-666", map[string]any{"human_conversation": true})
	deadlineIs(t, "a human_conversation message's route", f.routeState(t, f.route(t, marked)), 4*time.Hour)
	unmarked := f.threadMessage(t, f.tok, "own-deadline-2", "dm-667", nil)
	deadlineIs(t, "an unmarked message's route", f.routeState(t, f.route(t, unmarked)), 30*time.Minute)
}

func TestAnAgentMailRouteKeepsTheThirtyMinuteDeadline(t *testing.T) {
	f := newOwnerReplyFixture(t)
	tok, _ := f.mintPlugin(t, f.owner, plugins.AgentMailPlugin)
	m := f.threadMessage(t, tok, "own-mail-1", "7777", nil)
	deadlineIs(t, "an agent-mail route", f.routeState(t, f.route(t, m)), 30*time.Minute)
}
