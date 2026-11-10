package api

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/prroute"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// THE TASK TRIAGE through the real plugin route and stores: an assignment
// muster can place deterministically is placed with NO chief turn — forwarded,
// routed or put on the board by the chief's own tools — and anything doubtful
// still reaches the chief. Every title, repository and id is invented.

const (
	triRepo = "acme-labs/gizmo-api"
	triPR   = 3141
	triSID2 = "5e55a1d0-0000-4000-8000-00000000b0b2"
)

type triageFixture struct {
	*taskRouteFixture
	mirror *routeMirror
	turns  *threadTurns
	tok    string
	p      plugins.Plugin

	mu       sync.Mutex
	forwards []string
}

func newTriageFixture(t *testing.T, dispatch bool) *triageFixture {
	t.Helper()
	f := &triageFixture{taskRouteFixture: newTaskRouteFixture(t, dispatch, dispatch), turns: &threadTurns{},
		mirror: &routeMirror{interactive: true, statusReport: true}}
	// What a deployment with task and session routing, pull-request
	// authorship and a chief gateway wires.
	f.srv.ext.Mirror, f.srv.ext.SendQueue = f.mirror, sendqueue.NewPG(f.pool)
	f.srv.ext.Routes, f.srv.ext.Routing = f.plugins, &Routing{}
	f.srv.ext.PRSessions = f.plugins
	d, err := plugins.NewDeliverer(plugins.DelivererConfig{Store: f.plugins, Agents: f.srv.ext.Agents, Gateway: f.turns,
		Turn: f.turns.turn, Routing: true})
	if err != nil {
		t.Fatal(err)
	}
	f.srv.pluginDeliverer = d
	f.srv.taskForwardTurn = func(_ context.Context, x agents.Agent, _ agents.ChatSession, text string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.forwards = append(f.forwards, text)
		f.srv.turns.leave(x.ID)
	}
	f.tok, f.p = f.plugin(t)
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM session_prs WHERE owner_id = $1`, `DELETE FROM send_queue WHERE owner_id = $1`,
			`DELETE FROM plugin_message_handling WHERE owner_id = $1`} {
			_, _ = f.pool.Exec(sys, q, f.owner)
		}
	})
	return f
}

// assign files a new task through the plugin route (not routed), gates it
// when gated, binds bind (an agent id, 0 for none) to it, then posts its
// "assigned" notice routed to the chief; the task and its event.
func (f *triageFixture) assign(t *testing.T, title, body string, gated bool, bind int64) (int64, plugins.Message) {
	t.Helper()
	key := "tracker:tri" + strconv.FormatInt(time.Now().UnixNano(), 36)
	id := f.fileTask(t, f.tok, key, map[string]any{"create": map[string]any{"title": title, "body": body}})
	if gated {
		if _, err := f.pool.Exec(f.asOwner, `UPDATE notes SET tags = array_append(tags, 'gate:review') WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	if bind > 0 {
		if _, err := f.pool.Exec(f.asOwner, `UPDATE agents SET note_id = $1, status = 'running', kicked_off = true WHERE id = $2`, id, bind); err != nil {
			t.Fatal(err)
		}
	}
	if out := f.notice(t, f.tok, key, noticeAssigned, "upstream: assigned to you", false); out["routed"] != routedChief {
		t.Fatalf("the assignment: %v", out)
	}
	f.srv.WaitForPluginDeliveries()
	m, err := f.eventOf(id)
	if err != nil {
		t.Fatal(err)
	}
	return id, m
}

func (f *triageFixture) eventOf(taskID int64) (plugins.Message, error) {
	var id int64
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT id FROM plugin_messages WHERE task_id = $1 ORDER BY id DESC LIMIT 1`,
		taskID).Scan(&id); err != nil {
		return plugins.Message{}, err
	}
	return f.plugins.Message(f.asOwner, id)
}

// handled is the event's handling row.
func (f *triageFixture) handled(t *testing.T, m plugins.Message) string {
	t.Helper()
	var h string
	_ = f.pool.QueryRow(authz.System(context.Background()), `SELECT handler FROM plugin_message_handling WHERE owner_id = $1 AND plugin_message_id = $2`,
		f.owner, m.ID).Scan(&h)
	return h
}

// asksAbout is the open asks naming task id.
func (f *triageFixture) asksAbout(t *testing.T, id int64) []attention.Ask {
	t.Helper()
	out, err := f.srv.ext.Attention.OpenForTask(f.asOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *triageFixture) authoredBy(t *testing.T, ctx context.Context, sid string) {
	t.Helper()
	if _, err := f.plugins.RecordPRSession(ctx, sid, "tower", prroute.Signal{Kind: prroute.KindPRCreate, Repo: triRepo, PR: triPR}); err != nil {
		t.Fatal(err)
	}
}

// TestAnAssignmentNobodyIsOnGoesToTheBoardWithNoChiefTurn: no agent, no
// session hinted — muster raises the chief's own board ask (asker: the chief,
// the task linked), settles the event with no turn, logs it in the chief's
// chat and records task_board.
func TestAnAssignmentNobodyIsOnGoesToTheBoardWithNoChiefTurn(t *testing.T) {
	f := newTriageFixture(t, false)
	id, m := f.assign(t, "Rotate the build key", "Rotate it before Friday.", false, 0)
	if n := f.turns.count(); n != 0 {
		t.Fatalf("the chief took %d turn(s); the board needs none", n)
	}
	asks := f.asksAbout(t, id)
	if len(asks) != 1 || asks[0].Asker.AgentID != f.chief.ID || !strings.HasPrefix(asks[0].Question, "Task #"+strconv.FormatInt(id, 10)+" needs you: Rotate the build key") {
		t.Fatalf("the board ask: %+v", asks)
	}
	if m.Status != plugins.StatusDelivered || m.SettleReason == nil || !strings.HasPrefix(*m.SettleReason, "muster: task triage — put it in front of your owner") {
		t.Fatalf("the event: %s %v", m.Status, m.SettleReason)
	}
	if h := f.handled(t, m); h != "task_board" {
		t.Fatalf("handled by %q, want task_board", h)
	}
	// Routed: it is no longer a "Task to route" on the queue.
	items, err := f.srv.attentionItems(f.asOwner)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Kind == attentionKindTask {
			t.Fatalf("the task is still on the queue as a task to route: %+v", it)
		}
	}
	var notes int
	if err := f.pool.QueryRow(f.asOwner, `SELECT count(*) FROM chat_messages WHERE agent_id = $1 AND plugin_message_id = $2
		AND content LIKE '%muster put it in front of your owner%No turn was spent%'`, f.chief.ID, m.ID).Scan(&notes); err != nil || notes != 1 {
		t.Fatalf("the chief's chat note: %d (%v)", notes, err)
	}
}

// TestTheBoardAsksOptionsAreTheChiefsOwn: muster's board ask is the chief's
// task_route_attention ask for the same task — the same question and the same
// options — on an open task and on a gated one, with dispatch on (the options
// the chief's call would offer are whatever it offers then: the SAME call).
func TestTheBoardAsksOptionsAreTheChiefsOwn(t *testing.T) {
	for _, gated := range []bool{false, true} {
		f := newTriageFixture(t, true)
		id, _ := f.assign(t, "Ship the gizmo", "Ship it.", gated, 0)
		mine := f.asksAbout(t, id)
		if len(mine) != 1 || f.turns.count() != 0 {
			t.Fatalf("gated %v: muster's board ask: %+v, %d turns", gated, mine, f.turns.count())
		}
		// The chief's own call on the same task, for comparison.
		out := f.call(f.chief, taskRouteAttentionTool, map[string]any{"task_id": id, "note": ""})
		if toolRefused(out) {
			t.Fatalf("gated %v: the chief's task_route_attention: %s", gated, out)
		}
		asks := f.asksAbout(t, id)
		if len(asks) != 2 {
			t.Fatalf("gated %v: %d asks after the chief's call, want 2", gated, len(asks))
		}
		var chiefs attention.Ask
		for _, a := range asks {
			if a.ID != mine[0].ID {
				chiefs = a
			}
		}
		if !slices.Equal(mine[0].Options, chiefs.Options) || mine[0].Question != chiefs.Question || mine[0].Asker != chiefs.Asker {
			t.Fatalf("gated %v: muster's ask %q %v %+v\nthe chief's %q %v %+v\nwant the same", gated, mine[0].Question, mine[0].Options,
				mine[0].Asker, chiefs.Question, chiefs.Options, chiefs.Asker)
		}
	}
}

// TestAnAssignmentOneSessionAuthoredIsRoutedThere: the task names a pull
// request exactly one online session was recorded creating — the chief's own
// task route to that session, no turn, task_route.
func TestAnAssignmentOneSessionAuthoredIsRoutedThere(t *testing.T) {
	f := newTriageFixture(t, false)
	f.authoredBy(t, f.asOwner, routeSID)
	id, m := f.assign(t, "Fix the flaky check", "See "+triRepo+"#"+strconv.Itoa(triPR)+" for the failing run.", false, 0)
	if n := f.turns.count(); n != 0 {
		t.Fatalf("the chief took %d turn(s); a single strong match needs none", n)
	}
	var target string
	var taskRoute int64
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT target_key, task_id FROM session_routes WHERE owner_id = $1 AND task_id = $2`,
		f.owner, id).Scan(&target, &taskRoute); err != nil || target != routeSID {
		t.Fatalf("the task's route: %q (%v); want %s", target, err, routeSID)
	}
	if h := f.handled(t, m); h != "task_route" {
		t.Fatalf("handled by %q, want task_route", h)
	}
	if len(f.asksAbout(t, id)) != 0 {
		t.Fatal("a routed task also raised a board ask")
	}
}

// TestAnAssignmentTheAgentOnItIsForwarded: one running agent bound to the
// task and no session hinted — task_forward's own forward, no chief turn.
func TestAnAssignmentTheAgentOnItIsForwarded(t *testing.T) {
	f := newTriageFixture(t, false)
	_, m := f.assign(t, "Rename the gizmo flag", "Rename it.", false, f.worker.ID)
	f.mu.Lock()
	fw := append([]string(nil), f.forwards...)
	f.mu.Unlock()
	if f.turns.count() != 0 || len(fw) != 1 {
		t.Fatalf("%d chief turns, %d forwards; want 0 and 1", f.turns.count(), len(fw))
	}
	if h := f.handled(t, m); h != "task_forward" {
		t.Fatalf("handled by %q, want task_forward", h)
	}
}

// TestAnAmbiguousAssignmentStillReachesTheChief (REGRESSION): two online
// sessions authored the pull request the task names, or one did and another
// matches by focus only — the chief's turn, exactly as before, and nothing
// routed or asked by muster.
func TestAnAmbiguousAssignmentStillReachesTheChief(t *testing.T) {
	t.Run("two strong sessions", func(t *testing.T) {
		f := newTriageFixture(t, false)
		f.mirror.set(func(m *routeMirror) { m.otherSession = triSID2 })
		f.authoredBy(t, f.asOwner, routeSID)
		f.authoredBy(t, f.asOwner, triSID2)
		id, m := f.assign(t, "Fix the flaky check", "See "+triRepo+"#"+strconv.Itoa(triPR)+".", false, 0)
		f.mustBeTheChiefs(t, id, m)
	})
	t.Run("a strong session and a weak one", func(t *testing.T) {
		f := newTriageFixture(t, false)
		f.mirror.set(func(m *routeMirror) { m.otherSession = triSID2 })
		f.authoredBy(t, f.asOwner, triSID2)
		// Another live session whose focus shares the task's words: a weak match.
		if _, err := f.focus.Upsert(f.asOwner, focus.MirrorSubject(routeSID), focus.Write{Title: "fixing the flaky gizmo-9x check in probe-42",
			Source: focus.SourceSummarizer}); err != nil {
			t.Fatal(err)
		}
		id, m := f.assign(t, "Fix the flaky gizmo-9x check in probe-42", "See "+triRepo+"#"+strconv.Itoa(triPR)+".", false, 0)
		f.mustBeTheChiefs(t, id, m)
	})
}

func (f *triageFixture) mustBeTheChiefs(t *testing.T, id int64, m plugins.Message) {
	t.Helper()
	if n := f.turns.count(); n != 1 {
		t.Fatalf("the chief took %d turn(s), want 1", n)
	}
	var routes int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM session_routes WHERE owner_id = $1 AND task_id = $2`,
		f.owner, id).Scan(&routes); err != nil || routes != 0 {
		t.Fatalf("muster routed it: %d (%v)", routes, err)
	}
	if len(f.asksAbout(t, id)) != 0 {
		t.Fatal("muster raised a board ask for an ambiguous task")
	}
	if h := f.handled(t, m); h != "chief" {
		t.Fatalf("handled by %q, want chief", h)
	}
}

// TestTheTriageReadsOnlyTheOwnersEvidence: another owner's session recorded
// authoring the pull request is no candidate — the owner's assignment goes to
// the board, never to that session.
func TestTheTriageReadsOnlyTheOwnersEvidence(t *testing.T) {
	f := newTriageFixture(t, false)
	stranger := f.newMember(t)
	f.authoredBy(t, f.as(stranger), routeSID)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM session_prs WHERE owner_id = $1`, stranger)
	})
	id, m := f.assign(t, "Fix the flaky check", "See "+triRepo+"#"+strconv.Itoa(triPR)+".", false, 0)
	var routes int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM session_routes WHERE task_id = $1`, id).Scan(&routes); err != nil || routes != 0 {
		t.Fatalf("routed by another owner's authorship: %d (%v)", routes, err)
	}
	if h := f.handled(t, m); h != "task_board" || f.turns.count() != 0 {
		t.Fatalf("handled by %q with %d turns; want the board", h, f.turns.count())
	}
}

// TestTheTriageNeedsTheChiefTurnItReplaces: with no deliverer (no chief turn
// would run) the assignment is left as it always was — queued, for the chief.
func TestTheTriageNeedsTheChiefTurnItReplaces(t *testing.T) {
	f := newTriageFixture(t, false)
	f.srv.pluginDeliverer = nil
	id, m := f.assign(t, "Rotate the build key", "Rotate it.", false, 0)
	if m.Status != plugins.StatusQueued || len(f.asksAbout(t, id)) != 0 {
		t.Fatalf("with no deliverer: %s, %d asks; want queued and none", m.Status, len(f.asksAbout(t, id)))
	}
}
