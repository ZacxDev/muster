package api

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/sse"
)

// A ROUTE CARRYING A TASK (migration 0053), through the real server against a
// real Postgres: route_to_session{task_id}, the paste after the send queue's
// idle wait, route_reply → a comment on the task, the deadline's hand-back, and
// the refusals (exactly one of message_id | task_id, another owner's task, a
// second open route).

// newTask files one task for owner.
func (f *routeFixture) newTask(t *testing.T, owner int64, title string) notes.Note {
	t.Helper()
	n, err := f.srv.ext.Notes.Create(f.as(owner), notes.Note{Title: title, Body: "please look at the failing build", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func taskRouteArgs(n notes.Note, sub string) string {
	return `{"task_id":` + strconv.FormatInt(n.ID, 10) + `,"subject":"` + sub + `"}`
}

// routeTask routes task n through the chief's tool and returns the route id.
func (f *routeFixture) routeTask(t *testing.T, n notes.Note) string {
	t.Helper()
	text := f.srv.AgentToolDispatch(f.asOwner, f.chief)("route_to_session", taskRouteArgs(n, "mirror:"+routeSID))
	var res struct {
		RouteID string `json:"route_id"`
		TaskID  int64  `json:"task_id"`
		State   string `json:"state"`
	}
	if err := json.Unmarshal([]byte(text), &res); err != nil || !plugins.ValidRouteID(res.RouteID) || res.TaskID != n.ID || res.State != "routed" {
		t.Fatalf("route task %d: %s", n.ID, text)
	}
	return res.RouteID
}

// refusedAs is the refusal code of a route_to_session answer ("" for none).
func refusedAs(text string) string {
	var res struct{ Refused string }
	_ = json.Unmarshal([]byte(text), &res)
	return res.Refused
}

// chiefDefaultSessionText is every message in the chief's default session.
func (f *routeFixture) chiefDefaultSessionText(t *testing.T) string {
	t.Helper()
	sess, err := f.srv.defaultSession(f.asOwner, f.chief)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := f.srv.ext.Agents.ListChatMessages(f.asOwner, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Content + "\n")
	}
	return b.String()
}

// TestATaskRoutedToASessionIsPastedAfterTheIdleWaitAndAnsweredAsAComment: the
// chief routes a task; muster queues the task block (not the chief's words)
// into the window, which the send queue pastes only once Claude Code has been
// idle for the idle wait (an injected clock, no sleep); the session's
// route_reply becomes ONE comment on the task, the route ends relayed, the
// task leaves "Task to route", and the chief is told in its default session.
func TestATaskRoutedToASessionIsPastedAfterTheIdleWaitAndAnsweredAsAComment(t *testing.T) {
	f := newRouteFixture(t)
	f.srv.ext.ChiefDispatch = f.plugins
	tok := f.withRouteReply(t)
	n := f.newTask(t, f.owner, "Fix the nightly build")

	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	f.srv.sendQueueClock = func() time.Time { return now }
	f.mirror.set(func(m *routeMirror) {
		m.state = &mirror.AgentState{Status: mirror.StatusIdle, Since: 1000, Observed: now.Add(-10 * time.Second)}
	})

	bus := sse.New(512)
	f.srv.bus = bus
	_, events := bus.Subscribe(f.owner)

	id := f.routeTask(t, n)
	r := f.routeState(t, id)
	if r.TaskID != n.ID || r.MessageID != 0 || !r.IsTask() || r.State != plugins.RouteRouted {
		t.Fatalf("the route row: %+v", r)
	}
	items := f.queued(t)
	if len(items) != 1 || r.SendQueueID == nil || *r.SendQueueID != items[0].ID {
		t.Fatalf("queued %+v for route %+v", items, r)
	}
	for _, want := range []string{"route_id " + id, "task #" + strconv.FormatInt(n.ID, 10), "|   title: Fix the nightly build",
		"|   body: please look at the failing build", "added to task #" + strconv.FormatInt(n.ID, 10) + " as a comment",
		"<<<routed-message untrusted nonce="} {
		if !strings.Contains(items[0].Body, want) {
			t.Errorf("the queued block lacks %q:\n%s", want, items[0].Body)
		}
	}
	if _, err := mirror.SanitizePaste(items[0].Body); err != nil {
		t.Fatalf("the task block cannot be pasted: %v", err)
	}

	d := f.srv.newSendQueueDispatcher()
	tick := func() {
		t.Helper()
		if _, err := d.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	tick()
	if n := f.mirror.pastes(); n != 0 {
		t.Fatalf("pasted %d time(s) after 10 s idle; the idle wait is %s", n, f.srv.sendQueueIdle())
	}
	now = now.Add(f.srv.sendQueueIdle())
	tick()
	if n := f.mirror.pastes(); n != 1 {
		t.Fatalf("pasted %d time(s) once idle past the wait, want 1", n)
	}

	text, isErr := f.routeReplyCall(t, tok, routeSID, map[string]any{"route_id": id, "body": "The build fails on the lint step; fixed in my branch."})
	if isErr {
		t.Fatalf("route_reply: %s", text)
	}
	var out struct {
		State   string `json:"state"`
		TaskID  int64  `json:"task_id"`
		DraftID int64  `json:"draft_id"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.State != "relayed" || out.TaskID != n.ID || out.DraftID != 0 {
		t.Fatalf("route_reply result: %s", text)
	}
	got, err := f.srv.ext.Notes.Get(f.asOwner, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Comments) != 1 || got.Comments[0].Body != "The build fails on the lint step; fixed in my branch." ||
		got.Comments[0].Author != "mirror:"+routeSID+" via "+id {
		t.Fatalf("the task's comments: %+v", got.Comments)
	}
	// The comment reached the owner's open views: task-changed for this task.
	changed := 0
	for drained := false; !drained; {
		select {
		case ev := <-events:
			if ev.Name == EventTaskChanged && ev.Data == `{"id":"`+strconv.FormatInt(n.ID, 10)+`"}` {
				changed++
			}
		default:
			drained = true
		}
	}
	if changed != 1 {
		t.Fatalf("%d %s event(s) for task %d after the comment, want 1", changed, EventTaskChanged, n.ID)
	}
	if r := f.routeState(t, id); r.State != plugins.RouteRelayed || r.FinishedAt == nil || r.DraftID != nil {
		t.Fatalf("the answered task route: %+v", r)
	}
	// A relay retried as if the first one had died between the comment and
	// the route's end (the sweep retries an `answered` route) writes no second
	// comment.
	retried := f.routeState(t, id)
	retried.State = plugins.RouteAnswered
	if _, _, err := f.srv.relayRoute(f.asOwner, retried); err != nil {
		t.Fatalf("a second relay: %v", err)
	}
	if got, _ := f.srv.ext.Notes.Get(f.asOwner, n.ID); len(got.Comments) != 1 {
		t.Fatalf("a second relay left %d comments", len(got.Comments))
	}
	if text, isErr := f.routeReplyCall(t, tok, routeSID, map[string]any{"route_id": id, "body": "again"}); !isErr || !strings.Contains(text, "already been answered") {
		t.Fatalf("a second answer: %s", text)
	}
	var routed int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM audit_log WHERE user_id = $1 AND action = $2
		AND object = $3 AND detail->>'tool' = 'route_to_session'`, f.owner, plugins.ActionTaskRouted, "note:"+strconv.FormatInt(n.ID, 10)).Scan(&routed); err != nil || routed != 1 {
		t.Fatalf("the task's routed record: %d (%v)", routed, err)
	}
	if note := f.chiefDefaultSessionText(t); !strings.Contains(note, "Route "+id+" (task #"+strconv.FormatInt(n.ID, 10)) ||
		!strings.Contains(note, "added to the task as a comment") {
		t.Fatalf("the chief's default session: %q", note)
	}
}

// TestATaskRouteTimesOutBackToTheChief: no answer by the deadline (moved, not
// slept past) — the route times out, its queued paste is cancelled, and the
// task is handed back in the chief's default session; a late answer is refused.
func TestATaskRouteTimesOutBackToTheChief(t *testing.T) {
	f := newRouteFixture(t)
	n := f.newTask(t, f.owner, "Triage the flaky test")
	id := f.routeTask(t, n)
	if k, err := f.srv.RouteTimeoutPass(context.Background()); err != nil || k != 0 {
		t.Fatalf("before the deadline: %d timed out (%v)", k, err)
	}
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE session_routes SET timeout_at = now() - interval '1 minute' WHERE route_id = '`+id+`'`)
	if k, err := f.srv.RouteTimeoutPass(context.Background()); err != nil || k < 1 {
		t.Fatalf("past the deadline: %d timed out (%v)", k, err)
	}
	if r := f.routeState(t, id); r.State != plugins.RouteTimedOut {
		t.Fatalf("state %s, want timed_out", r.State)
	}
	if items := f.queued(t); len(items) != 1 || items[0].State != sendqueue.Cancelled {
		t.Fatalf("the paste was not cancelled: %+v", items)
	}
	note := f.chiefDefaultSessionText(t)
	if !strings.Contains(note, "Route "+id+" (task #"+strconv.FormatInt(n.ID, 10)) || !strings.Contains(note, "the task is back with you") ||
		!strings.Contains(note, "task_route_attention") {
		t.Fatalf("the chief was not handed the task back: %q", note)
	}
	if code, _ := f.reply(t, f.token, id, "too late"); code != 409 {
		t.Fatalf("an answer after the timeout: %d, want 409", code)
	}
	if got, _ := f.srv.ext.Notes.Get(f.asOwner, n.ID); len(got.Comments) != 0 {
		t.Fatalf("a timed-out route commented: %+v", got.Comments)
	}
}

// TestRouteToSessionTakesExactlyOneOfMessageAndTask: both, or neither, is
// bad_arguments and writes nothing; either one alone routes (the message route
// unchanged: no task on it).
func TestRouteToSessionTakesExactlyOneOfMessageAndTask(t *testing.T) {
	f := newRouteFixture(t)
	n := f.newTask(t, f.owner, "One of the two")
	m := f.deliveredMessage(t, f.owner, f.chief.ID, "route-one-of")
	gw := f.srv.AgentToolDispatch(f.asOwner, f.chief)
	sub := "mirror:" + routeSID
	for name, args := range map[string]string{
		"both":    `{"message_id":` + strconv.FormatInt(m.ID, 10) + `,"task_id":` + strconv.FormatInt(n.ID, 10) + `,"subject":"` + sub + `"}`,
		"neither": `{"subject":"` + sub + `"}`,
	} {
		if text := gw("route_to_session", args); refusedAs(text) != RefusedBadArguments || !isToolErr(text) {
			t.Errorf("%s: %s", name, text)
		}
	}
	var routes int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM session_routes WHERE owner_id = $1`, f.owner).Scan(&routes); err != nil || routes != 0 {
		t.Fatalf("a refused call wrote %d route(s) (%v)", routes, err)
	}
	// The schema holds it too: a route row naming both is refused.
	_, err := f.pool.Exec(authz.System(context.Background()), `INSERT INTO session_routes (owner_id, route_id, plugin_message_id, task_id,
		agent_id, target_kind, target_key, timeout_at) VALUES ($1, $2, $3, $4, $5, 'mirror', $6, now() + interval '1 hour')`,
		f.owner, plugins.NewRouteID(), m.ID, n.ID, f.chief.ID, routeSID)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.ConstraintName != "session_routes_one_item_check" {
		t.Fatalf("a route row naming a message and a task: %v, want session_routes_one_item_check", err)
	}
	// Each alone routes.
	mid := f.route(t, m)
	if r := f.routeState(t, mid); r.MessageID != m.ID || r.TaskID != 0 || r.IsTask() {
		t.Fatalf("the message route: %+v", r)
	}
	f.routeTask(t, n)
}

// TestATaskRouteRefusesAnotherOwnersTask: through the chief's tool another
// owner's task is task_not_found; and the store refuses it on its own (0053's
// trigger), so a caller that skipped the tool's check still cannot route it.
func TestATaskRouteRefusesAnotherOwnersTask(t *testing.T) {
	f := newRouteFixture(t)
	stranger := f.newMember(t)
	theirs := f.newTask(t, stranger, "Not yours")
	if text := f.srv.AgentToolDispatch(f.asOwner, f.chief)("route_to_session", taskRouteArgs(theirs, "mirror:"+routeSID)); refusedAs(text) != RefusedTaskNotFound {
		t.Fatalf("another owner's task through the tool: %s", text)
	}
	target := plugins.RouteTarget{Kind: string(focus.KindMirror), Key: routeSID}
	if _, err := f.plugins.CreateTaskRouteFrom(f.asOwner, f.chief.ID, f.owner, theirs.ID, target, time.Hour, plugins.RouteOrigin{}); !errors.Is(err, plugins.ErrNotFound) {
		t.Fatalf("another owner's task through the store: %v, want ErrNotFound", err)
	}
	// Positive control: the owner's own task, the same call.
	mine := f.newTask(t, f.owner, "Yours")
	if _, err := f.plugins.CreateTaskRouteFrom(f.asOwner, f.chief.ID, f.owner, mine.ID, target, time.Hour, plugins.RouteOrigin{}); err != nil {
		t.Fatalf("the owner's own task through the store: %v", err)
	}
}

// TestASecondOpenRouteForATaskIsRefused: a task already routed and not yet
// answered is already_routed; once that route has ended it may be routed again.
func TestASecondOpenRouteForATaskIsRefused(t *testing.T) {
	f := newRouteFixture(t)
	n := f.newTask(t, f.owner, "Route me once")
	id := f.routeTask(t, n)
	if text := f.srv.AgentToolDispatch(f.asOwner, f.chief)("route_to_session", taskRouteArgs(n, "mirror:"+routeSID)); refusedAs(text) != RefusedAlreadyRouted {
		t.Fatalf("a second route: %s", text)
	}
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE session_routes SET timeout_at = now() - interval '1 minute' WHERE route_id = '`+id+`'`)
	if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.routeTask(t, n)
}

// TestATaskRouteToAClosedTaskGoesBackToTheChief: a task completed while it was
// routed takes no comment; the route fails, owing the chief a hand-back.
func TestATaskRouteToAClosedTaskGoesBackToTheChief(t *testing.T) {
	f := newRouteFixture(t)
	n := f.newTask(t, f.owner, "Closed meanwhile")
	id := f.routeTask(t, n)
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE notes SET status = 'complete' WHERE id = `+strconv.FormatInt(n.ID, 10))
	if code, out := f.reply(t, f.token, id, "Done already."); code != 409 || out["state"] != string(plugins.RouteFailed) {
		t.Fatalf("an answer to a closed task: %d %v", code, out)
	}
	if got, _ := f.srv.ext.Notes.Get(f.asOwner, n.ID); len(got.Comments) != 0 {
		t.Fatalf("a closed task took a comment: %+v", got.Comments)
	}
	if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if note := f.chiefDefaultSessionText(t); !strings.Contains(note, "is no longer open") || !strings.Contains(note, "the task is back with you") {
		t.Fatalf("the chief's default session: %q", note)
	}
}
