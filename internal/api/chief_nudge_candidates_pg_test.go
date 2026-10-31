package api

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers"
)

// 🔴 CHARACTERIZATION OF THE NUDGE SWEEP'S CANDIDATE SET (nudgeCandidates),
// written and run green against the pre-refactor code (origin/main d55f5bd4,
// before the stall facts were extracted into stallInputs) and committed BEFORE
// the extraction. Each case starts from one baseline — a stalled mirrored
// window and a stalled dispatched-agent conversation, both candidates, plus an
// officer, the chief and another user's agent, each just as stalled and never
// candidates — changes ONE fact, and pins the EXACT candidate set that
// results, every field of every candidate included (idle times rounded to the
// minute). The baseline is its own positive control: it yields both.

type nudgeCandFixture struct {
	*nudgeFixture
	worker            agents.Agent
	workerSess        agents.ChatSession
	officer           agents.Agent
	ids               map[string]string // "a"/"s" + an agent/session id -> its placeholder (agent and session ids can coincide)
	baseScriptWindows []mirror.Window
}

func newNudgeCandFixture(t *testing.T) *nudgeCandFixture {
	t.Helper()
	f := &nudgeCandFixture{nudgeFixture: newNudgeFixture(t, false), ids: map[string]string{}}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	f.worker = f.agentFor(t, authz.OperatorUserID, "nudgecand-"+suffix, "cand worker", "nudge-cand-tok-"+suffix)
	f.officer = f.agentFor(t, authz.OperatorUserID, officers.InstanceName("ci-watch", 1), "", "nudge-cand-off-tok-"+suffix)
	f.srv.officerRT = &officerRuntime{reg: officers.NewRegistry([]officers.Officer{{ID: "ci-watch", MaxInstances: 1}})}
	f.workerSess, _ = f.stallAgent(t, f.worker, authz.OperatorUserID)
	f.stallAgent(t, f.officer, authz.OperatorUserID)
	f.stallAgent(t, f.boss, authz.OperatorUserID)
	f.stallAgent(t, f.nudgeFixture.worker, f.member) // another user's agent
	f.ids["a"+strconv.FormatInt(f.worker.ID, 10)] = "<WORKER>"
	f.ids["s"+strconv.FormatInt(f.workerSess.ID, 10)] = "<WSESS>"
	f.baseScriptWindows = append([]mirror.Window(nil), f.mirror.windows...)
	return f
}

// describe is one candidate, every field, database ids replaced.
func (f *nudgeCandFixture) describe(c nudgeCandidate) string {
	s := c.Session
	ph := func(ns string, v int64) string {
		k := strconv.FormatInt(v, 10)
		if p, ok := f.ids[ns+k]; ok {
			return p
		}
		return k
	}
	aid := func(v int64) string { return ph("a", v) }
	sid := func(v int64) string { return ph("s", v) }
	key := s.Key
	if s.Kind == chiefnudge.KindAgent {
		key = "agent/" + aid(s.AgentID) + "/" + sid(s.ChatSessionID)
	}
	streak := s.StreakKey
	if strings.HasPrefix(streak, "msg:") {
		streak = "msg:<ID>"
	}
	return fmt.Sprintf("%s|%s|host=%s|tmux=%s|win=%s|label=%s|status=%s|streak=%s|idle=%s|shell=%t|turnerr=%t|ask=%t|"+
		"agent=%s|chat=%s|other=%t|chief=%t|officer=%t|decided=%t|title=%q|ca=%s|cs=%s",
		s.Kind, key, s.Host, s.TmuxSession, s.Window, s.Label, s.Status, streak, s.IdleFor.Round(time.Minute), s.Shell, s.TurnError,
		s.OpenAsk, aid(s.AgentID), sid(s.ChatSessionID), s.OtherUser, s.Chief, s.Officer, s.Decided, s.Title,
		aid(c.agent.ID), sid(c.session.ID))
}

func (f *nudgeCandFixture) candidates(t *testing.T, inFlight map[string]bool) []string {
	t.Helper()
	octx := f.srv.attentionPrincipal(context.Background(), authz.OperatorUserID)
	var out []string
	for _, c := range f.srv.nudgeCandidates(octx, inFlight) {
		out = append(out, f.describe(c))
	}
	return out
}

const (
	candMirror = "mirror|" + nudgeSID + "|host=ws|tmux=main|win=@1|label=Gold:1|status=idle|streak=since:1760000500000|idle=20m0s|" +
		"shell=false|turnerr=false|ask=false|agent=0|chat=0|other=false|chief=false|officer=false|decided=false|title=\"\"|ca=0|cs=0"
	candWorker = "agent|agent/<WORKER>/<WSESS>|host=|tmux=|win=|label=cand worker|status=idle|streak=msg:<ID>|idle=20m0s|" +
		"shell=false|turnerr=false|ask=false|agent=<WORKER>|chat=<WSESS>|other=false|chief=false|officer=false|decided=false|title=\"\"|" +
		"ca=<WORKER>|cs=<WSESS>"
)

func TestNudgeCandidatesCharacterization(t *testing.T) {
	f := newNudgeCandFixture(t)
	sys := authz.System(context.Background())
	octx := asUser(authz.OperatorUserID)
	reset := func() {
		f.idle(20 * time.Minute)
		f.mirror.mu.Lock()
		f.mirror.work, f.mirror.online, f.mirror.statusReport = nil, true, true
		f.mirror.windows = append([]mirror.Window(nil), f.baseScriptWindows...)
		f.mirror.mu.Unlock()
		for _, q := range []string{
			`UPDATE attention_asks SET status = 'cancelled', resolved_at = now() WHERE owner_id = $1 AND status = 'open'`,
			`DELETE FROM chief_nudges WHERE owner_id = $1`,
		} {
			if _, err := f.pool.Exec(sys, q, authz.OperatorUserID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.pool.Exec(sys, `UPDATE agents SET status = 'running' WHERE id = $1`, f.worker.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(sys, `DELETE FROM chat_sessions WHERE agent_id = $1 AND id <> $2`, f.worker.ID, f.workerSess.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(sys, `DELETE FROM chat_messages WHERE session_id = $1 AND content LIKE $2`, f.workerSess.ID, AgentTurnFailedPrefix+"%"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(sys, `UPDATE chat_sessions SET updated_at = now() WHERE id = $1`, f.workerSess.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(sys, `UPDATE chat_messages SET created_at = now() - interval '20 minutes' WHERE session_id = $1`, f.workerSess.ID); err != nil {
			t.Fatal(err)
		}
	}
	decided := func(kind, key, streak string) {
		if _, ok, err := f.nudges.Insert(octx, chiefnudge.Decision{Kind: kind, Key: key, StreakKey: streak, Decision: chiefnudge.DecisionNudge,
			Mode: chiefnudge.ModeShadow, Outcome: chiefnudge.OutcomeWouldSend}, ""); err != nil || !ok {
			t.Fatalf("seed decision: %v %v", ok, err)
		}
	}
	lastMsgID := func() int64 {
		var id int64
		if err := f.pool.QueryRow(sys, `SELECT max(id) FROM chat_messages WHERE session_id = $1`, f.workerSess.ID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}

	both := []string{candMirror, candWorker}
	cases := []struct {
		name     string
		setup    func()
		inFlight map[string]bool
		want     []string
	}{
		{"baseline", func() {}, nil, both},
		// The mirrored window, one fact at a time.
		{"mirror idle 5 minutes (too soon)", func() { f.idle(5 * time.Minute) }, nil, []string{candWorker}},
		{"mirror busy", func() { f.state(func(s *mirror.AgentState) { s.Status = mirror.StatusBusy }) }, nil, []string{candWorker}},
		{"mirror waiting", func() { f.state(func(s *mirror.AgentState) { s.Status = mirror.StatusWaiting }) }, nil, []string{candWorker}},
		{"mirror turn error", func() {
			f.state(func(s *mirror.AgentState) { s.TurnError = &mirror.TurnError{Kind: mirror.TurnErrorLimit} })
		}, nil, []string{candWorker}},
		{"mirror host limit", func() { f.state(func(s *mirror.AgentState) { s.HostLimit = &mirror.HostLimit{} }) }, nil, []string{candWorker}},
		{"mirror guard held", func() { f.state(func(s *mirror.AgentState) { s.GuardHeld = "approval" }) }, nil, []string{candWorker}},
		{"mirror status unknown", func() { f.state(func(s *mirror.AgentState) { s.Unknown = mirror.ErrNoStatusReport }) }, nil, []string{candWorker}},
		{"mirror no streak", func() { f.state(func(s *mirror.AgentState) { s.Since = 0 }) }, nil, []string{candWorker}},
		{"mirror background shell", func() { f.mirror.work = map[string]mirror.WindowWork{"@1": {Shells: 1}} }, nil, []string{candWorker}},
		{"mirror open ask", func() {
			if _, err := f.attn.Create(octx, attention.New{Asker: attention.Asker{MirrorHost: "ws", MirrorSession: nudgeSID, Label: "Gold:1"},
				Question: "Which branch, characterised?"}); err != nil {
				t.Fatal(err)
			}
		}, nil, []string{candWorker}},
		{"mirror in flight", func() {}, map[string]bool{chiefnudge.KindMirror + ":" + nudgeSID: true}, []string{candWorker}},
		{"mirror already decided", func() { decided(chiefnudge.KindMirror, nudgeSID, "since:"+strconv.FormatInt(nudgeSince, 10)) }, nil, []string{candWorker}},
		{"mirror decided in another streak", func() { decided(chiefnudge.KindMirror, nudgeSID, "since:1") }, nil, both},
		{"host reports no status", func() { f.mirror.statusReport = false }, nil, []string{candWorker}},
		{"host offline", func() { f.mirror.online = false }, nil, []string{candWorker}},
		{"window has a runtime", func() { f.mirror.windows[0].Runtime = "hermes" }, nil, []string{candWorker}},
		{"window is not claude", func() { f.mirror.windows[0].Claude = false }, nil, []string{candWorker}},
		{"window has no claude session", func() { f.mirror.windows[0].ClaudeSession = "" }, nil, []string{candWorker}},
		{"two windows, one conversation", func() {
			w := f.mirror.windows[0]
			w.ID, w.Index, w.Active, w.Label = "@2", 2, false, "Gold:2"
			f.mirror.windows = append(f.mirror.windows, w)
		}, nil, both},
		// The dispatched agent, one fact at a time.
		{"agent stopped", func() {
			if _, err := f.pool.Exec(sys, `UPDATE agents SET status = 'stopped' WHERE id = $1`, f.worker.ID); err != nil {
				t.Fatal(err)
			}
		}, nil, []string{candMirror}},
		{"agent's last message is a failed turn", func() {
			if _, err := agents.NewPG(f.pool).AddChatMessage(octx, agents.ChatMessage{AgentID: f.worker.ID, SessionID: f.workerSess.ID,
				Role: "assistant", Content: AgentTurnFailedPrefix + "usage limit"}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.pool.Exec(sys, `UPDATE chat_messages SET created_at = now() - interval '20 minutes' WHERE session_id = $1`, f.workerSess.ID); err != nil {
				t.Fatal(err)
			}
		}, nil, []string{candMirror}},
		{"agent open ask", func() {
			if _, err := f.attn.Create(octx, attention.New{Asker: attention.Asker{AgentID: f.worker.ID}, Question: "Agent asks, characterised?"}); err != nil {
				t.Fatal(err)
			}
		}, nil, []string{candMirror}},
		{"agent conversation in flight", func() {}, map[string]bool{chiefnudge.KindAgent + ":" + chiefnudge.AgentKey(f.worker.ID, f.workerSess.ID): true},
			[]string{candMirror}},
		{"agent base key in flight (not the conversation's)", func() {}, map[string]bool{chiefnudge.KindAgent + ":" + chiefnudge.AgentKey(f.worker.ID, 0): true},
			both},
		{"agent too soon", func() {
			if _, err := f.pool.Exec(sys, `UPDATE chat_messages SET created_at = now() - interval '2 minutes' WHERE session_id = $1`, f.workerSess.ID); err != nil {
				t.Fatal(err)
			}
		}, nil, []string{candMirror}},
		{"agent conversation older than its bound", func() {
			if _, err := f.pool.Exec(sys, `UPDATE chat_sessions SET updated_at = now() - interval '25 hours' WHERE id = $1`, f.workerSess.ID); err != nil {
				t.Fatal(err)
			}
		}, nil, []string{candMirror}},
		{"agent already decided", func() {
			decided(chiefnudge.KindAgent, chiefnudge.AgentKey(f.worker.ID, f.workerSess.ID), "msg:"+strconv.FormatInt(lastMsgID(), 10))
		}, nil, []string{candMirror}},
	}
	for _, c := range cases {
		reset()
		c.setup()
		got := f.candidates(t, c.inFlight)
		if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
			t.Errorf("%s:\n got  %v\n want %v", c.name, got, c.want)
		}
	}

	// 🔴 THE ORDER IS PINNED, NOT SORTED AWAY. Two stalled conversations of one
	// agent are two candidates, each on its own, the most recently active
	// first: set the recency explicitly, read the order, flip it, read again.
	reset()
	second, _ := f.stallAgent(t, f.worker, authz.OperatorUserID)
	f.ids["s"+strconv.FormatInt(second.ID, 10)] = "<WSESS2>"
	candWorker2 := strings.ReplaceAll(candWorker, "<WSESS>", "<WSESS2>")
	recency := func(newer, older int64) {
		for q, id := range map[string]int64{
			`UPDATE chat_sessions SET updated_at = now() WHERE id = $1`:                       newer,
			`UPDATE chat_sessions SET updated_at = now() - interval '1 minute' WHERE id = $1`: older,
		} {
			if _, err := f.pool.Exec(sys, q, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, o := range []struct {
		name         string
		newer, older int64
		want         []string
	}{
		{"two conversations, the first newer", f.workerSess.ID, second.ID, []string{candMirror, candWorker, candWorker2}},
		{"two conversations, the second newer", second.ID, f.workerSess.ID, []string{candMirror, candWorker2, candWorker}},
	} {
		recency(o.newer, o.older)
		if got := f.candidates(t, nil); strings.Join(got, "\n") != strings.Join(o.want, "\n") {
			t.Errorf("%s:\n got  %v\n want %v", o.name, got, o.want)
		}
	}

	// A mix: three mirrored windows (three conversations; appended out of index
	// order on purpose, read in the hub's window order, which is by index) and
	// two agents (the newer agent first), the worker with both conversations,
	// the second newer. Mirrored windows all come before every agent.
	const sidB, sidC = "1c6e8b63-7a2f-4d4a-8b4f-3e5d9c2a1f88", "2d7f9c74-8b3a-4e5b-9c5a-4f6eab3b2a99"
	other := f.agentFor(t, authz.OperatorUserID, "nudgecand2-"+strconv.FormatInt(time.Now().UnixNano(), 36), "cand worker two",
		"nudge-cand2-tok-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	otherSess, _ := f.stallAgent(t, other, authz.OperatorUserID)
	f.ids["a"+strconv.FormatInt(other.ID, 10)] = "<OTHER>"
	f.ids["s"+strconv.FormatInt(otherSess.ID, 10)] = "<OSESS>"
	f.mirror.mu.Lock()
	w := f.mirror.windows[0]
	wC, wB := w, w
	wC.ID, wC.Index, wC.Active, wC.Label, wC.ClaudeSession = "@3", 3, false, "Gold:3", sidC
	wB.ID, wB.Index, wB.Active, wB.Label, wB.ClaudeSession = "@2", 2, false, "Gold:2", sidB
	f.mirror.windows = append(f.mirror.windows, wC, wB)
	f.mirror.mu.Unlock()
	recency(second.ID, f.workerSess.ID)
	mirrorAs := func(sid, win, label string) string {
		return strings.Replace(strings.Replace(strings.Replace(candMirror, nudgeSID, sid, 1), "win=@1", "win="+win, 1), "Gold:1", label, 1)
	}
	candOther := strings.NewReplacer("<WORKER>", "<OTHER>", "<WSESS>", "<OSESS>", "label=cand worker", "label=cand worker two").Replace(candWorker)
	want := []string{candMirror, mirrorAs(sidB, "@2", "Gold:2"), mirrorAs(sidC, "@3", "Gold:3"), candOther, candWorker2, candWorker}
	if got := f.candidates(t, nil); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("a mix of windows and agents:\n got  %v\n want %v", got, want)
	}
}
