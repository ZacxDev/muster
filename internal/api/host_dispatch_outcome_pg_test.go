package api

import (
	"context"
	"io"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentoutcome"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/ui"
)

// A HOST DISPATCH'S OUTCOME FROM ITS HOST'S LISTING (host_dispatch_outcome.go),
// over a real Postgres: an opencode dispatch's final reply is done ONLY on its
// last line's done grammar and waiting otherwise; each reply is told once (one
// push, one chief note); a later turn re-arms it (and, for Claude, only once its
// done turn has ended); and no other dispatch, session, host or owner can
// record one. Every session id, window, mark and summary is distinct, and none
// equals a constant an assertion names.

// The fixture's opencode sessions (the spawn reply's agent_session).
const (
	ocOutcomeSID   = "ses_0utc0meAgentSessionAAAA1"
	ocOutcomeOther = "ses_0utc0meSomeoneElseBBBB2"
)

type hostOutcomeFixture struct {
	*hostFixture
	pusher *payloadPusher
	device string
	seq    atomic.Uint64
}

func newHostOutcomeFixture(t *testing.T) *hostOutcomeFixture {
	t.Helper()
	f := &hostOutcomeFixture{hostFixture: newOCHostFixture(t), pusher: &payloadPusher{}, device: ep("host-outcome-owner")}
	f.srv.ext.Outcomes = agentoutcome.NewPG(f.pool)
	store := &memPush{}
	f.srv.ext.Push = push.NewService(store, f.pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))
	claim(t, store, f.owner, f.device)
	return f
}

// listOn is host's listing in the hub's view.
func (f *hostOutcomeFixture) listOn(host string, ws ...mirror.Window) {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	for i := range f.m.hosts {
		if f.m.hosts[i].Host == host {
			f.m.hosts[i].Windows = mirror.NormalizeWindows(ws, 0)
		}
	}
}

// startOC starts an opencode dispatch on host-echo whose spawn reply names sid,
// and lists its window idle (so it runs). It returns the dispatch and its
// window as listed.
func (f *hostOutcomeFixture) startOC(t *testing.T, key, sid string) (int64, mirror.Window) {
	t.Helper()
	f.m.reply = func(_ string, dispatch int64, _ mirror.AgentSpawnReq) (mirror.AgentSpawned, error) {
		return mirror.AgentSpawned{Session: "agents", Window: "@" + strconv.FormatInt(dispatch, 10), Worktree: "/w/oc", Branch: "muster/oc",
			AgentSession: sid}, nil
	}
	f.ownerTurn(t, f.chief, f.owner, "Do it mirrored on host-echo with qwen.")
	id := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, ocCall("host/host-echo/opencode", "qwen/qwen3-coder", key, true)))
	w := mirror.Window{Session: "agents", ID: "@" + strconv.FormatInt(id, 10), Runtime: mirror.RuntimeOpenCode, AgentSend: true,
		AgentSession: sid, Dispatch: id, Status: mirror.StatusBusy, StatusSince: 1}
	f.listOn("host-echo", w)
	f.srv.reconcileListing(context.Background(), f.owner, "host-echo", []mirror.Window{w}, time.Now())
	if r := f.row(t, id); r.Status != plugins.HostDispatchRunning || r.AgentSession != sid || r.Window != w.ID {
		t.Fatalf("not running as its session: %+v", r)
	}
	return id, w
}

// snap applies one outcomes snapshot of owner's host, numbered after every
// earlier one.
func (f *hostOutcomeFixture) snap(owner int64, host string, ws []mirror.Window, oc map[string]mirror.WindowOutcome) {
	f.srv.reconcileOutcomes(context.Background(), owner, host, f.seq.Add(1), ws, oc)
}

// idleWith is w idle, with its final reply mark and lines.
func (f *hostOutcomeFixture) idleWith(w mirror.Window, mark string, lines ...string) ([]mirror.Window, map[string]mirror.WindowOutcome) {
	w.Status = mirror.StatusIdle
	return []mirror.Window{w}, map[string]mirror.WindowOutcome{w.ID: {Mark: mark, Lines: lines}}
}

func busy(w mirror.Window) []mirror.Window {
	w.Status = mirror.StatusBusy
	return []mirror.Window{w}
}

// pushes is every outcome push the owner's device got for host dispatch id.
func (f *hostOutcomeFixture) pushes(id int64) []push.Payload {
	f.srv.WaitForPushes()
	var out []push.Payload
	for _, p := range f.pusher.to(f.device) {
		if p.Tag == "host-dispatch-"+strconv.FormatInt(id, 10)+"-outcome" {
			out = append(out, p)
		}
	}
	return out
}

// chiefNotes is muster's outcome notes about host dispatch id in the chief's chat.
func (f *hostOutcomeFixture) chiefNotes(t *testing.T, id int64) []string {
	t.Helper()
	rows, err := f.pool.Query(authz.System(context.Background()), `SELECT content FROM chat_messages WHERE agent_id = $1 AND role = 'user' ORDER BY id`, f.chief.ID)
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
		if strings.HasPrefix(c, "[muster] The ") && strings.Contains(c, "(host dispatch "+strconv.FormatInt(id, 10)+",") {
			out = append(out, c)
		}
	}
	return out
}

func (f *hostOutcomeFixture) card(t *testing.T, id int64) ui.DispatchCard {
	t.Helper()
	c, err := f.srv.hostDispatchCardFromStore(f.asOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestAnOpenCodeReplyEndingInTheDoneLineIsDone: the final reply's last line is
// `MUSTER-DONE: <summary>` → the row is done with that summary and STAYS
// running (done is not ended), the card reads Done, the owner gets one push
// and the chief one note. The same reply again (a later snapshot) tells
// nobody twice.
func TestAnOpenCodeReplyEndingInTheDoneLineIsDone(t *testing.T) {
	f := newHostOutcomeFixture(t)
	id, w := f.startOC(t, "oc-done-1", ocOutcomeSID)
	const summary = "opened draft PR acme/widgets#52 fixing the typo"
	ws, oc := f.idleWith(w, "msg_0utc0meReply01", "I fixed the README.", "MUSTER-DONE: "+summary)
	f.snap(f.owner, "host-echo", ws, oc)
	r := f.row(t, id)
	if r.DoneAt.IsZero() || r.DoneSummary != summary || !r.WaitingAt.IsZero() {
		t.Fatalf("not recorded done: %+v", r)
	}
	if r.Status != plugins.HostDispatchRunning {
		t.Fatalf("its done ENDED the row (the window is open): %+v", r)
	}
	if c := f.card(t, id); c.State != ui.DispatchDone || !strings.Contains(c.Detail, summary) {
		t.Fatalf("the card: %+v", c)
	}
	ps := f.pushes(id)
	if len(ps) != 1 || !strings.Contains(ps[0].Title, "finished: "+summary) {
		t.Fatalf("pushes %+v; want one \"… finished: <summary>\"", ps)
	}
	notes := f.chiefNotes(t, id)
	if len(notes) != 1 || !strings.Contains(notes[0], "finished: "+summary) || !strings.Contains(notes[0], "Tell your owner it is done.") {
		t.Fatalf("chief notes %q", notes)
	}
	f.snap(f.owner, "host-echo", ws, oc)
	if n, m := len(f.pushes(id)), len(f.chiefNotes(t, id)); n != 1 || m != 1 {
		t.Fatalf("the same reply again told the owner %d times and the chief %d times; want once each", n, m)
	}
}

// TestAnOpenCodeReplyWithoutTheDoneLineIsWaiting: every reply whose LAST line
// is not the done grammar is waiting on the owner — a question, the sentinel
// quoted MID-reply, an empty summary, the wrong case, the sentinel not at the
// start, in markdown — with that last line on the card and in the push, and
// the reply's last lines in the chief's note. Each new reply is told once.
// Then a reply that does end in the done line is done (the positive control).
func TestAnOpenCodeReplyWithoutTheDoneLineIsWaiting(t *testing.T) {
	f := newHostOutcomeFixture(t)
	id, w := f.startOC(t, "oc-wait-1", ocOutcomeSID)
	cases := []struct {
		why   string
		lines []string
	}{
		{"a question", []string{"I found two READMEs.", "Which one should I fix?"}},
		{"the done line quoted mid-reply", []string{"MUSTER-DONE: fixed it", "Wait — should I also update the docs?"}},
		{"an empty summary", []string{"All good.", "MUSTER-DONE:   "}},
		{"the wrong case", []string{"muster-done: lower-cased summary"}},
		{"not at the start", []string{"Status: MUSTER-DONE: prefixed summary"}},
		{"in markdown", []string{"**MUSTER-DONE:** bold summary"}},
	}
	for i, c := range cases {
		ws, oc := f.idleWith(w, "msg_0utc0meWait0"+strconv.Itoa(i), c.lines...)
		f.snap(f.owner, "host-echo", ws, oc)
		last := c.lines[len(c.lines)-1]
		r := f.row(t, id)
		if !r.DoneAt.IsZero() || r.WaitingAt.IsZero() || r.WaitingLine != strings.TrimSpace(last) {
			t.Fatalf("%s: not waiting on its last line: %+v", c.why, r)
		}
		if card := f.card(t, id); card.State != ui.DispatchWaiting || !strings.Contains(card.Detail, strings.TrimSpace(last)) {
			t.Fatalf("%s: the card %+v", c.why, card)
		}
		ps := f.pushes(id)
		// (The push title is rendered without markdown; its body quotes the line.)
		if len(ps) != i+1 || !strings.Contains(ps[i].Title, " is waiting for you: ") || !strings.HasPrefix(ps[i].Body, strings.TrimSpace(last)+" ") {
			t.Fatalf("%s: pushes %+v", c.why, ps)
		}
		notes := f.chiefNotes(t, id)
		if len(notes) != i+1 || !strings.Contains(notes[i], "WAITING FOR YOUR OWNER") || !strings.Contains(notes[i], c.lines[0]) {
			t.Fatalf("%s: chief notes %q", c.why, notes)
		}
	}
	ws, oc := f.idleWith(w, "msg_0utc0meDoneAfter", "MUSTER-DONE: settled on the root README")
	f.snap(f.owner, "host-echo", ws, oc)
	if r := f.row(t, id); r.DoneSummary != "settled on the root README" || !r.WaitingAt.IsZero() {
		t.Fatalf("control: a done line after waiting: %+v", r)
	}
}

// TestALaterTurnReArmsAnOpenCodeDispatch: done, then the window is listed busy
// (the owner typed): working again — the card reads Working, nothing is
// pushed. Its next reply is judged afresh. And a snapshot OLDER than one
// applied (the hook runs on its own goroutine) changes nothing.
func TestALaterTurnReArmsAnOpenCodeDispatch(t *testing.T) {
	f := newHostOutcomeFixture(t)
	id, w := f.startOC(t, "oc-rearm-1", ocOutcomeSID)
	ws, oc := f.idleWith(w, "msg_0utc0meFirstTurn", "MUSTER-DONE: first pass finished")
	f.snap(f.owner, "host-echo", ws, oc)
	if r := f.row(t, id); r.DoneAt.IsZero() {
		t.Fatalf("control: not done: %+v", r)
	}
	f.snap(f.owner, "host-echo", busy(w), nil)
	r := f.row(t, id)
	if !r.DoneAt.IsZero() || !r.WaitingAt.IsZero() || r.OutcomeMark != "" || r.Status != plugins.HostDispatchRunning {
		t.Fatalf("a later turn did not re-arm it: %+v", r)
	}
	if c := f.card(t, id); c.State != ui.DispatchWorking {
		t.Fatalf("the card after a later turn: %+v", c)
	}
	if n := len(f.pushes(id)); n != 1 {
		t.Fatalf("the re-arm pushed: %d pushes", n)
	}
	ws, oc = f.idleWith(w, "msg_0utc0meSecondTurn", "Do you want the docs updated too?")
	newer := f.seq.Add(1)
	f.srv.reconcileOutcomes(context.Background(), f.owner, "host-echo", newer, ws, oc)
	if r := f.row(t, id); r.WaitingLine != "Do you want the docs updated too?" {
		t.Fatalf("the second turn's reply: %+v", r)
	}
	// An older busy snapshot arriving late is dropped.
	f.srv.reconcileOutcomes(context.Background(), f.owner, "host-echo", newer-1, busy(w), nil)
	if r := f.row(t, id); r.WaitingAt.IsZero() {
		t.Fatalf("an older snapshot re-armed it: %+v", r)
	}
	if n := len(f.pushes(id)); n != 2 {
		t.Fatalf("%d pushes; want 2 (one per reply)", n)
	}
}

// TestALaterTurnReArmsAClaudeDispatchOnlyAfterItsDoneTurnEnded: a Claude
// dispatch reports done mid-turn (agent_report_done), so the busy listing
// right after is THAT turn, not a new one: still Done. Listed idle (the turn
// ended), still Done. Listed busy again: a later turn — Working. And it can
// report done again.
func TestALaterTurnReArmsAClaudeDispatchOnlyAfterItsDoneTurnEnded(t *testing.T) {
	f := newHostOutcomeFixture(t)
	id, win := f.startRunningHostDispatch(t)
	agent := mirror.Window{Session: "agents", ID: win, Claude: true, ClaudeSession: doneAgentSID, Dispatch: id, Status: mirror.StatusBusy}
	f.setBravoWindows(agent)
	if out := f.reportDone(t, f.owner, "host-bravo", doneAgentSID, "Claude first pass shipped"); !strings.Contains(out, "Recorded") {
		t.Fatalf("report: %s", out)
	}
	at := func(status string) []mirror.Window { a := agent; a.Status = status; return []mirror.Window{a} }
	f.snap(f.owner, "host-bravo", at(mirror.StatusBusy), nil)
	if c := f.card(t, id); c.State != ui.DispatchDone {
		t.Fatalf("the reporting turn still running re-armed it: %+v", c)
	}
	f.snap(f.owner, "host-bravo", at(mirror.StatusIdle), nil)
	if r := f.row(t, id); !r.DoneSettled || r.DoneAt.IsZero() {
		t.Fatalf("idle after done: %+v", r)
	}
	f.snap(f.owner, "host-bravo", at(mirror.StatusBusy), nil)
	if c := f.card(t, id); c.State != ui.DispatchWorking {
		t.Fatalf("a later turn: the card %+v", c)
	}
	if r := f.row(t, id); !r.DoneAt.IsZero() || r.DoneSettled {
		t.Fatalf("a later turn: %+v", r)
	}
	if out := f.reportDone(t, f.owner, "host-bravo", doneAgentSID, "Claude second pass shipped"); !strings.Contains(out, "Recorded") {
		t.Fatalf("a second report after the re-arm: %s", out)
	}
	if r := f.row(t, id); r.DoneSummary != "Claude second pass shipped" {
		t.Fatalf("the second report: %+v", r)
	}
}

// TestAClaudeHostDispatchsDoneIsNotedToTheChief: agent_report_done on the
// mirror door tells the owner's chief once, as a pod's done does.
func TestAClaudeHostDispatchsDoneIsNotedToTheChief(t *testing.T) {
	f := newHostOutcomeFixture(t)
	id, _ := f.startRunningHostDispatch(t)
	if out := f.reportDone(t, f.owner, "host-bravo", doneAgentSID, "Claude fixed the README"); !strings.Contains(out, "Recorded") {
		t.Fatalf("report: %s", out)
	}
	notes := f.chiefNotes(t, id)
	if len(notes) != 1 || !strings.Contains(notes[0], "finished: Claude fixed the README") || !strings.Contains(notes[0], "host-bravo") {
		t.Fatalf("chief notes %q", notes)
	}
}

// TestNoOtherWindowSessionHostOrOwnerRecordsAnOpenCodeOutcome: none of these
// records an outcome on the dispatch:
//   - its window running ANOTHER opencode session (its tag on someone else's
//     conversation);
//   - another window carrying its tag, running its own session (a forged
//     tag never moves a row's window);
//   - another host's snapshot naming its window and tag;
//   - another owner's snapshot of a host of the same name;
//   - a Claude dispatch's window carrying a done line (Claude reports done
//     only with agent_report_done).
//
// Then its own window's reply still is (the positive control).
func TestNoOtherWindowSessionHostOrOwnerRecordsAnOpenCodeOutcome(t *testing.T) {
	f := newHostOutcomeFixture(t)
	id, w := f.startOC(t, "oc-scope-1", ocOutcomeSID)
	done := []string{"MUSTER-DONE: a summary nobody should record"}
	assertNothing := func(why string) {
		t.Helper()
		if r := f.row(t, id); !r.DoneAt.IsZero() || !r.WaitingAt.IsZero() || r.OutcomeMark != "" {
			t.Fatalf("%s recorded an outcome: %+v", why, r)
		}
		if n := len(f.pushes(id)); n != 0 {
			t.Fatalf("%s pushed", why)
		}
	}
	other := w
	other.AgentSession = ocOutcomeOther
	ws, oc := f.idleWith(other, "msg_0utc0meScope01", done...)
	f.snap(f.owner, "host-echo", ws, oc)
	assertNothing("its window running another session")

	forged := w
	forged.ID, forged.Session = "@9001", "main"
	ws, oc = f.idleWith(forged, "msg_0utc0meScope02", done...)
	f.snap(f.owner, "host-echo", ws, oc)
	assertNothing("another window carrying its tag")

	ws, oc = f.idleWith(w, "msg_0utc0meScope03", done...)
	f.snap(f.owner, "host-foxtrot", ws, oc)
	assertNothing("another host")
	f.snap(f.owner+1, "host-echo", ws, oc)
	assertNothing("another owner")

	// A Claude dispatch's window with a done line.
	cid, cwin := f.startRunningHostDispatch(t)
	cw := mirror.Window{Session: "agents", ID: cwin, Claude: true, ClaudeSession: doneAgentSID, Dispatch: cid, Status: mirror.StatusIdle}
	f.snap(f.owner, "host-bravo", []mirror.Window{cw}, map[string]mirror.WindowOutcome{cwin: {Mark: "msg_0utc0meScope04", Lines: done}})
	if r := f.row(t, cid); !r.DoneAt.IsZero() || r.OutcomeMark != "" {
		t.Fatalf("a Claude dispatch's window recorded a reply outcome: %+v", r)
	}

	ws, oc = f.idleWith(w, "msg_0utc0meScope05", "MUSTER-DONE: its own window's summary")
	f.snap(f.owner, "host-echo", ws, oc)
	if r := f.row(t, id); r.DoneSummary != "its own window's summary" {
		t.Fatalf("control: its own window: %+v", r)
	}
}

// TestTheOutcomeWritersWriteOnlyTheirOwnSessionsRunningRow: the store's own
// WHERE is a second binding, below the reconciler's: a wrong session, a wrong
// host, no session or mark, the same reply twice, a Claude row and an ended row
// write nothing; a Claude row's done is not re-armed before it is settled.
func TestTheOutcomeWritersWriteOnlyTheirOwnSessionsRunningRow(t *testing.T) {
	f := newHostOutcomeFixture(t)
	id, _ := f.startOC(t, "oc-store-1", ocOutcomeSID)
	for _, c := range []struct{ why, host, sid, mark string }{
		{"another session", "host-echo", ocOutcomeOther, "msg_0utc0meStore01"},
		{"another host", "host-foxtrot", ocOutcomeSID, "msg_0utc0meStore01"},
		{"no session", "host-echo", "", "msg_0utc0meStore01"},
		{"no mark", "host-echo", ocOutcomeSID, ""},
	} {
		if wrote, err := f.plugins.RecordHostDispatchOutcome(f.asOwner, id, c.host, c.sid, plugins.HostOutcomeWaiting, "x", c.mark); err != nil || wrote {
			t.Fatalf("%s: wrote=%v err=%v", c.why, wrote, err)
		}
	}
	if wrote, err := f.plugins.RecordHostDispatchOutcome(f.asOwner, id, "host-echo", ocOutcomeSID, plugins.HostOutcomeWaiting, "a question?", "msg_0utc0meStore02"); err != nil || !wrote {
		t.Fatalf("control: its own: wrote=%v err=%v", wrote, err)
	}
	if wrote, _ := f.plugins.RecordHostDispatchOutcome(f.asOwner, id, "host-echo", ocOutcomeSID, plugins.HostOutcomeDone, "x", "msg_0utc0meStore02"); wrote {
		t.Fatal("the same reply was recorded twice")
	}
	cid, _ := f.startRunningHostDispatch(t)
	if wrote, _ := f.plugins.RecordHostDispatchOutcome(f.asOwner, cid, "host-bravo", doneAgentSID, plugins.HostOutcomeDone, "x", "msg_0utc0meStore03"); wrote {
		t.Fatal("a Claude row took a reply outcome")
	}
	if out := f.reportDone(t, f.owner, "host-bravo", doneAgentSID, "claude store check"); !strings.Contains(out, "Recorded") {
		t.Fatalf("report: %s", out)
	}
	if wrote, _ := f.plugins.RearmHostDispatch(f.asOwner, cid, "host-bravo", doneAgentSID); wrote {
		t.Fatal("a Claude row's done was re-armed before it settled")
	}
	if wrote, _ := f.plugins.SettleHostDispatchDone(f.asOwner, cid, "host-bravo", doneOtherSID); wrote {
		t.Fatal("another session settled it")
	}
	if _, err := f.srv.endHostDispatch(f.asOwner, f.owner, id, "ended by owner", false); err != nil {
		t.Fatal(err)
	}
	if wrote, _ := f.plugins.RecordHostDispatchOutcome(f.asOwner, id, "host-echo", ocOutcomeSID, plugins.HostOutcomeDone, "x", "msg_0utc0meStore04"); wrote {
		t.Fatal("an ended row took an outcome")
	}
	if wrote, _ := f.plugins.RearmHostDispatch(f.asOwner, id, "host-echo", ocOutcomeSID); wrote {
		t.Fatal("an ended row was re-armed")
	}
}
