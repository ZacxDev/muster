package api

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sessionstate"
)

// Session state's audit round 1 (#426): the classifier's input, a host that
// has not been read, a failed agent list, one bill per streak, the cost
// bounds, an agent's turn, and the per-pass budget.

// stateSecretLast is a last message carrying a secret-shaped string.
const stateSecretLast = "Deployed with key sk-live-abcdef1234567890abcdef. Shall I merge it?"

func (f *stateFixture) setLast(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script = append(f.script[:1:1], chiefnudge.Msg{Role: "assistant", Text: text})
}

func (j *jevFake) seenText() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.text
}

// TestTheStatePassSendsTheClassifierRedactedText (🔴 F1): what the classifier
// receives from the state pass is the session's last message redacted and cut
// — byte for byte what the nudge sweep sends (stallClassifierText) — never the
// raw secret.
func TestTheStatePassSendsTheClassifierRedactedText(t *testing.T) {
	f := newStateFixture(t, true)
	j := withJev(t, f.nudgeFixture)
	j.set(200, 0.03, 0.02, 0.95)
	f.setLast(stateSecretLast)
	f.statePass(t)
	got := j.seenText()
	if strings.Contains(got, "sk-live-abcdef1234567890abcdef") || got != stallClassifierText(stateSecretLast) || got == stateSecretLast {
		t.Fatalf("the classifier received %q", got)
	}
}

// TestTheStatePassCutsALongLastMessage (F1): a last message past
// chiefnudge.MaxLastRunes reaches the classifier cut, as the nudge sweep's
// does, so both classify the same, backtested input.
func TestTheStatePassCutsALongLastMessage(t *testing.T) {
	f := newStateFixture(t, true)
	j := withJev(t, f.nudgeFixture)
	j.set(200, 0.03, 0.02, 0.95)
	long := strings.Repeat("work log line. ", chiefnudge.MaxLastRunes/10) + "Shall I merge it?"
	f.setLast(long)
	f.statePass(t)
	if got := j.seenText(); got != stallClassifierText(long) || len([]rune(got)) > chiefnudge.MaxLastRunes+20 {
		t.Fatalf("the classifier received %d runes", len([]rune(got)))
	}
}

// TestTheNudgeSweepAloneIsUnchangedWithSessionStateOff (F5) — an INVARIANT
// GUARD: with session state off, the nudge sweep classifies the redacted text
// once, as it did before session state existed.
func TestTheNudgeSweepAloneIsUnchangedWithSessionStateOff(t *testing.T) {
	f := newNudgeFixture(t, false)
	j := withJev(t, f)
	j.set(200, 0.81, 0.10, 0.09)
	f.mu.Lock()
	f.script = append(f.script[:1:1], chiefnudge.Msg{Role: "assistant", Text: stateSecretLast})
	f.mu.Unlock()
	f.pass(t)
	j.mu.Lock()
	calls, got := j.calls, j.text
	j.mu.Unlock()
	if calls != 1 || got != stallClassifierText(stateSecretLast) {
		t.Fatalf("calls %d, text %q", calls, got)
	}
	if d := f.one(t); d.Decision != chiefnudge.DecisionAsk || d.JevLabel != chiefnudge.LabelNeedsInput {
		t.Fatalf("decision: %+v", d)
	}
}

// TestAHostThatHasSentNoWindowListEndsNothing (F2): a host online and
// reporting status with no window list read is unknown — none of its episodes
// is gone.
func TestAHostThatHasSentNoWindowListEndsNothing(t *testing.T) {
	f := newStateFixture(t, false)
	f.statePass(t)
	first := f.oneEpisode(t)
	f.mirror.mu.Lock()
	f.mirror.windows = []mirror.Window{}
	f.mirror.mu.Unlock()
	f.statePass(t)
	if e := f.oneEpisode(t); e.ID != first.ID || !e.Open() {
		t.Fatalf("an unread host ended it: %+v", e)
	}
}

// TestAHostThatStoppedReportingStatusEndsNothing (F2): a host online with its
// windows listed but no status report cannot say a window is gone — its
// windows are simply not judged.
func TestAHostThatStoppedReportingStatusEndsNothing(t *testing.T) {
	f := newStateFixture(t, false)
	f.statePass(t)
	first := f.oneEpisode(t)
	f.mirror.mu.Lock()
	f.mirror.statusReport = false
	f.mirror.mu.Unlock()
	f.statePass(t)
	if e := f.oneEpisode(t); e.ID != first.ID || !e.Open() {
		t.Fatalf("a host not reporting status ended it: %+v", e)
	}
}

// listFailing is an agent store whose List fails while fail is set.
type listFailing struct {
	agents.Store
	mu   sync.Mutex
	fail bool
}

func (l *listFailing) List(ctx context.Context) ([]agents.Agent, error) {
	l.mu.Lock()
	fail := l.fail
	l.mu.Unlock()
	if fail {
		return nil, errors.New("listFailing: the database is down")
	}
	return l.Store.List(ctx)
}

// TestAFailedAgentListEndsNoAgentEpisode (F3): one failed read of the
// owner's agents lists none of them — it ends nothing.
func TestAFailedAgentListEndsNoAgentEpisode(t *testing.T) {
	f := newStateFixture(t, false)
	f.mirror.mu.Lock()
	f.mirror.windows = nil
	f.mirror.mu.Unlock()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	worker := f.agentFor(t, authz.OperatorUserID, "statelist-"+suffix, "worker", "state-list-tok-"+suffix)
	f.srv.nudgeReadHook = nil
	f.stallAgent(t, worker, authz.OperatorUserID)
	lf := &listFailing{Store: f.srv.ext.Agents}
	f.srv.ext.Agents = lf
	f.statePass(t)
	mine := func() []sessionstate.Episode {
		var out []sessionstate.Episode
		for _, e := range f.episodes(t) {
			if e.AgentID == worker.ID {
				out = append(out, e)
			}
		}
		return out
	}
	if got := mine(); len(got) != 1 || !got[0].Open() {
		t.Fatalf("the agent's stop: %+v", got)
	}
	lf.mu.Lock()
	lf.fail = true
	lf.mu.Unlock()
	f.statePass(t)
	if got := mine(); len(got) != 1 || !got[0].Open() {
		t.Fatalf("a failed agent list ended it: %+v", got)
	}
}

// TestTheClassifierIsBilledOncePerStreakWhicheverPassRunsFirst (F4): the
// nudge sweep classifies first and forgets its in-memory copy; the state pass
// then reads the decision row. And after a restart (the in-memory ledger
// gone), the nudge sweep reads the episode's stored answer. One call, in both
// orders.
func TestTheClassifierIsBilledOncePerStreakWhicheverPassRunsFirst(t *testing.T) {
	t.Run("nudge sweep first", func(t *testing.T) {
		f := newStateFixture(t, true)
		j := withJev(t, f.nudgeFixture)
		j.set(200, 0.03, 0.02, 0.95)
		f.pass(t)
		f.statePass(t)
		if n := f.jevCalls(j); n != 1 {
			t.Fatalf("classifier calls: %d", n)
		}
		if e := f.oneEpisode(t); e.Jev.Label != chiefnudge.LabelNeither || e.ClassifiedStreak == "" {
			t.Fatalf("episode: %+v", e)
		}
	})
	t.Run("state pass first, then a restart", func(t *testing.T) {
		f := newStateFixture(t, true)
		j := withJev(t, f.nudgeFixture)
		j.set(200, 0.03, 0.02, 0.95)
		f.statePass(t)
		f.srv.nudgeRetry = nudgeRetries{} // a restart: the in-memory ledger is gone
		f.pass(t)
		f.statePass(t)
		if n := f.jevCalls(j); n != 1 {
			t.Fatalf("classifier calls: %d", n)
		}
	})
}

// TestAFailingTranscriptReadBacksOff (F6): a read that keeps failing is not
// retried every pass: after the first failure, not for stateReadBackoff;
// after the second, not for twice that.
func TestAFailingTranscriptReadBacksOff(t *testing.T) {
	f := newStateFixture(t, true)
	withJev(t, f.nudgeFixture)
	f.mu.Lock()
	f.readErr = errors.New("the host did not answer")
	f.mu.Unlock()
	f.statePass(t) // read 1, fails: next at +30s
	f.advance(10 * time.Second)
	f.statePass(t) // not due
	if n := f.readCount(); n != 1 {
		t.Fatalf("reads after the first failure: %d", n)
	}
	f.advance(25 * time.Second)
	f.statePass(t) // read 2 (+35s), fails: next at +60s after it
	f.advance(40 * time.Second)
	f.statePass(t) // not due
	if n := f.readCount(); n != 2 {
		t.Fatalf("reads after the second failure: %d", n)
	}
	f.advance(25 * time.Second)
	f.statePass(t) // due
	if n := f.readCount(); n != 3 {
		t.Fatalf("reads after the backoff: %d", n)
	}
	// A failed read never quiets the session: its stop is open, unclassified.
	if e := f.oneEpisode(t); !e.Open() || !e.Asked || e.ClassifiedStreak != "" {
		t.Fatalf("episode: %+v", e)
	}
}

// TestAnAgentsQuickExchangeStaysOneStop (F7): the agent's turn is timed by
// its messages — a 10 s exchange does not end the stop, a reply turn of more
// than EndAfter does. The exchange's messages are dated BEFORE the stop
// opened, so the owner-acted end (an owner message after the opening, which
// ends a stop at once: TestTheOwnerAnsweringAnAgentEndsItsStop) does not
// apply here, and the turn timing is what is measured.
func TestAnAgentsQuickExchangeStaysOneStop(t *testing.T) {
	f := newStateFixture(t, false)
	f.srv.nudgeReadHook = nil
	f.mirror.mu.Lock()
	f.mirror.windows = nil
	f.mirror.mu.Unlock()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	worker := f.agentFor(t, authz.OperatorUserID, "statequick-"+suffix, "worker", "state-quick-tok-"+suffix)
	sess, _ := f.stallAgent(t, worker, authz.OperatorUserID)
	store := agents.NewPG(f.pool)
	ctx := asUser(authz.OperatorUserID)
	add := func(role, text string, ago time.Duration) {
		t.Helper()
		m, err := store.AddChatMessage(ctx, agents.ChatMessage{AgentID: worker.ID, SessionID: sess.ID, Role: role, Content: text})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE chat_messages SET created_at = $2 WHERE id = $1`,
			m.ID, time.Now().Add(-ago)); err != nil {
			t.Fatal(err)
		}
	}
	mine := func() []sessionstate.Episode {
		var out []sessionstate.Episode
		for _, e := range f.episodes(t) {
			if e.AgentID == worker.ID {
				out = append(out, e)
			}
		}
		return out
	}
	f.statePass(t)
	first := mine()
	if len(first) != 1 || !first[0].Open() {
		t.Fatalf("the agent's stop: %+v", first)
	}
	add("user", "main", 15*time.Minute)
	add("assistant", "Merged into main. Delete the branch too?", 15*time.Minute-10*time.Second)
	f.statePass(t)
	if got := mine(); len(got) != 1 || got[0].ID != first[0].ID || !got[0].Open() {
		t.Fatalf("a 10 s exchange split the stop: %+v", got)
	}
	add("user", "yes, and run the release", 13*time.Minute)
	add("assistant", "Released 1.2.3.", 13*time.Minute-sessionstate.EndAfter-17*time.Second)
	f.statePass(t)
	if got := mine(); len(got) < 1 || got[len(got)-1].ID != first[0].ID || got[len(got)-1].Open() ||
		got[len(got)-1].EndReason != sessionstate.EndWorked {
		t.Fatalf("a long reply turn did not end the stop: %+v", got)
	}
}

// TestThePassClassifiesAtMostItsBudget (F9): seven stopped sessions; one
// pass reads and classifies five, opening the other two unclassified (counted
// as asked); the next pass classifies those two.
func TestThePassClassifiesAtMostItsBudget(t *testing.T) {
	f := newStateFixture(t, true)
	j := withJev(t, f.nudgeFixture)
	j.set(200, 0.03, 0.02, 0.95)
	var ws []mirror.Window
	for i := 1; i <= 7; i++ {
		ws = append(ws, mirror.Window{Session: "main", ID: "@" + strconv.Itoa(i), Index: i, Label: "w", Claude: true,
			ClaudeSession: "0b5d7a52-6f1e-4c39-9a3e-2d4c8b1f0e0" + strconv.Itoa(i), Status: mirror.StatusIdle, StatusSince: nudgeSince})
	}
	f.mirror.mu.Lock()
	f.mirror.windows = ws
	f.mirror.mu.Unlock()
	f.statePass(t)
	if n := f.jevCalls(j); n != sessionStateClassifyPerPass {
		t.Fatalf("calls in one pass: %d", n)
	}
	unclassified := 0
	for _, e := range f.episodes(t) {
		if e.ClassifiedStreak == "" {
			unclassified++
			if !e.Asked {
				t.Fatalf("an unclassified episode is not counted as asked: %+v", e)
			}
		}
	}
	if unclassified != 2 {
		t.Fatalf("unclassified after one pass: %d", unclassified)
	}
	f.statePass(t)
	if n := f.jevCalls(j); n != 7 {
		t.Fatalf("calls after two passes: %d", n)
	}
}

// staleEpisodes is an episode store whose OpenEpisodes reads nothing: a pass
// that read before another replica opened the stop.
type staleEpisodes struct{ *sessionstate.PG }

func (staleEpisodes) OpenEpisodes(context.Context) ([]sessionstate.Episode, error) { return nil, nil }

// TestAPassWithAStaleReadCannotOpenASecondEpisode (F9): the stop is already
// open (another replica), and this pass's read missed it. Its Open computes
// the NEXT seq, so only the one-open index refuses it.
func TestAPassWithAStaleReadCannotOpenASecondEpisode(t *testing.T) {
	f := newStateFixture(t, false)
	f.statePass(t)
	f.oneEpisode(t)
	f.srv.ext.Episodes = staleEpisodes{f.eps}
	f.statePass(t)
	open := 0
	for _, e := range f.episodes(t) {
		if e.Open() {
			open++
		}
	}
	if open != 1 {
		t.Fatalf("open episodes: %d", open)
	}
}
