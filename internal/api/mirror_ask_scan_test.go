package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// A PENDING QUESTION SURVIVES A MUSTER RESTART AND A HOST RECONNECT
// (mirror_ask_scan.go). The question store is in memory and the hook
// registers a call ONCE, so a restart (every deploy) used to forget every open
// question: the card was gone and the window fell back to "answer it in the
// terminal" though the picker was still there. muster now reads each WAITING
// Claude window's transcript, through today's host, on the hub's nudges, and
// registers the AskUserQuestion it shows open.
//
// These tests drive ONLY what today's hosts and hub already do: a window
// listed waiting (mirror.Window.Status), its transcript stream
// (Mirror.WatchWindow), and the hub's nudge (AttentionMirrorChanged) — no
// host change.
// ---------------------------------------------------------------------------

// scanMirror is askMirror whose transcript stream starts, as a real host's
// does, with a Reset backfill of the transcript so far (then the prompt index,
// which marks the backfill complete), and then whatever feed sends.
type scanMirror struct {
	*askMirror
	tmu        sync.Mutex
	transcript []cctranscript.Event
	watches    int
}

func (m *scanMirror) setTranscript(evs ...cctranscript.Event) {
	m.tmu.Lock()
	m.transcript = evs
	m.tmu.Unlock()
}

func (m *scanMirror) watchCount() int {
	m.tmu.Lock()
	defer m.tmu.Unlock()
	return m.watches
}

func (m *scanMirror) WatchWindow(ctx context.Context, host, session, window, gen string, offset int64, out func(mirror.Msg) error) error {
	if p, ok := authz.From(ctx); !ok || p.UserID != 1 || host != "ws" || session != "main" || window != "@1" {
		return mirror.ErrHostOffline
	}
	// Every read ASKED for counts, an offline host's included (the hub
	// refuses it, as here).
	m.tmu.Lock()
	m.watches++
	backfill := append([]cctranscript.Event(nil), m.transcript...)
	m.tmu.Unlock()
	m.fakeMirror.mu.Lock()
	online := m.fakeMirror.online
	m.fakeMirror.mu.Unlock()
	if !online {
		return mirror.ErrHostOffline
	}
	if err := out(mirror.Msg{Type: mirror.TypeEvents, Gen: "sess-1", Reset: true, Events: backfill}); err != nil {
		return err
	}
	if err := out(mirror.Msg{Type: mirror.TypeEvents, Gen: "sess-1", PromptIndex: true}); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg := <-m.feedCh:
			if err := out(msg); err != nil {
				return err
			}
		}
	}
}

// setWindow lists main@1 running sess-1 with status (and pending, if any).
func (m *scanMirror) setWindow(status string, pending *mirror.Pending) {
	m.fakeMirror.mu.Lock()
	m.fakeMirror.windows = []mirror.Window{{Session: "main", ID: "@1", Index: 0, Active: true, Claude: true, ClaudeSession: "sess-1", Status: status, Pending: pending}}
	m.fakeMirror.mu.Unlock()
}

func (m *scanMirror) setOnline(on bool) {
	m.fakeMirror.mu.Lock()
	m.fakeMirror.online = on
	if !on {
		m.fakeMirror.offlineSince = time.Now()
	}
	m.fakeMirror.mu.Unlock()
}

// scanServer is a FRESH muster (a restart: an empty question store) whose
// host lists main@1 waiting, its transcript evs.
func scanServer(t *testing.T, evs ...cctranscript.Event) (*scanMirror, *Server, *httptest.Server) {
	t.Helper()
	sm := &scanMirror{askMirror: newAskMirror(true)}
	sm.setWindow(mirror.StatusWaiting, nil)
	sm.setTranscript(evs...)
	s, ts := mirrorServer(t, sm, newFakeTerminal())
	s.askWatch.mu.Lock()
	s.askWatch.retry = 20 * time.Millisecond
	s.askWatch.mu.Unlock()
	t.Cleanup(s.EndStreams)
	return sm, s, ts
}

var (
	scanPrompt = cctranscript.Event{Kind: cctranscript.KindPrompt, Text: "go"}
	gateKey    = askq.Key{Owner: 1, Source: askSourceMirror, Scope: "ws", Session: "sess-1", ToolUseID: gateTool}
)

// gateCall is the captured AskUserQuestion call as the stream carries it.
func gateCall(t *testing.T) cctranscript.Event {
	return cctranscript.Event{Kind: cctranscript.KindToolCall, ToolID: gateTool, Tool: askq.ToolName, Ask: &cctranscript.Ask{Questions: gateQs(t)}}
}

// nudge is the hub's change / waiting-set nudge for user 1.
func nudge(s *Server) { s.AttentionMirrorChanged(1) }

// cardCount is the live region's number of cards.
func cardCount(t *testing.T, s *Server, ts *httptest.Server) (int, string) {
	t.Helper()
	_, body := mirrorGet(t, s, ts, asksPath)
	return strings.Count(body, "data-ask-live"), body
}

// waitWatches waits until the host has served at least n transcript reads.
func waitWatches(t *testing.T, sm *scanMirror, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for sm.watchCount() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d transcript read(s), want %d", sm.watchCount(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestARestartedMusterRebuildsAWaitingWindowsQuestion: a question registered
// by the hook, then a RESTARTED muster (a fresh Server and store) whose host
// lists the window waiting with the call open in its transcript: once the
// hub's nudge comes (the host's connect), it is the same question, pending,
// answerable — and the answer is typed into the terminal's picker as before.
func TestARestartedMusterRebuildsAWaitingWindowsQuestion(t *testing.T) {
	_, s1, ts1 := scanServer(t, scanPrompt, gateCall(t))
	register(t, ts1)
	region(t, s1, ts1, `data-ask-state="pending"`)

	sm, s, ts := scanServer(t, scanPrompt, gateCall(t)) // the restarted muster
	if n, body := cardCount(t, s, ts); n != 0 {
		t.Fatalf("a fresh store already has a card:\n%s", body)
	}
	nudge(s)
	region(t, s, ts, `data-ask-state="pending"`)
	if got := s.Questions().Pending(1); len(got) != 1 || got[0].Key != gateKey || !reflect.DeepEqual(got[0].Questions, gateQs(t)) {
		t.Fatalf("the restarted muster's open questions: %+v", got)
	}
	resp, body := answerReq(t, s, ts, answerForm(gateTool, "a0", "Red", "a1", "Fish", "a1", "Dog"), "", "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Ask-State") != ui.AskStateTyping {
		t.Fatalf("answer after the restart: %d %s\n%s", resp.StatusCode, resp.Header.Get("X-Ask-State"), body)
	}
	if got := sm.typedSoFar(); !reflect.DeepEqual(got, []string{`ws main @1 [{"picks":[0]},{"picks":[1,2]}]`}) {
		t.Fatalf("typed %q", got)
	}
	sm.feed(gateTool, gateQs(t), map[string]string{"Which color do you prefer?": "Red", "Which pets do you have?": "Dog, Fish"})
	region(t, s, ts, `data-ask-state="answered"`)
}

// TestAHostThatReconnectsIsReadOnConnect: a restarted muster whose host is
// still away reads nothing (and offers no card); when the host connects, its
// nudge reads the waiting window and the question is back.
func TestAHostThatReconnectsIsReadOnConnect(t *testing.T) {
	sm, s, ts := scanServer(t, scanPrompt, gateCall(t))
	sm.setOnline(false)
	nudge(s)
	time.Sleep(100 * time.Millisecond)
	if n := sm.watchCount(); n != 0 {
		t.Fatalf("%d transcript read(s) of an offline host", n)
	}
	if got := s.Questions().Pending(1); len(got) != 0 {
		t.Fatalf("open questions with the host away: %+v", got)
	}
	sm.setOnline(true)
	nudge(s)
	region(t, s, ts, `data-ask-state="pending"`)
	// It drops and comes back: still one card, the same question.
	before, _ := s.Questions().Find(gateKey)
	sm.setOnline(false)
	nudge(s)
	sm.setOnline(true)
	nudge(s)
	time.Sleep(100 * time.Millisecond)
	if n, body := cardCount(t, s, ts); n != 1 {
		t.Fatalf("%d cards after a reconnect:\n%s", n, body)
	}
	if after, _ := s.Questions().Find(gateKey); after.ID != before.ID || after.State != askq.Pending {
		t.Fatalf("after the reconnect: %+v (was %s)", after, before.ID)
	}
}

// TestRepeatedNudgesAreOneReadAndOneCard: the hook's registration and the
// scan are ONE card under the hook's id, and a session with an open question
// is never read by the scan; a waiting window with no question is read once
// per waiting spell however many nudges come — and again once it waits anew.
func TestRepeatedNudgesAreOneReadAndOneCard(t *testing.T) {
	t.Run("the hook's question", func(t *testing.T) {
		sm, s, ts := scanServer(t, scanPrompt, gateCall(t))
		register(t, ts)
		region(t, s, ts, `data-ask-state="pending"`)
		hook, _ := s.Questions().Find(gateKey)
		waitWatches(t, sm, 1) // the question's own watch
		for range 5 {
			nudge(s)
		}
		time.Sleep(150 * time.Millisecond)
		if n := sm.watchCount(); n != 1 {
			t.Fatalf("%d transcript reads: the scan read a session its watch covers", n)
		}
		if got := s.Questions().Pending(1); len(got) != 1 || got[0].ID != hook.ID {
			t.Fatalf("open questions %+v (the hook's was %s)", got, hook.ID)
		}
	})
	t.Run("no question", func(t *testing.T) {
		bash := cctranscript.Event{Kind: cctranscript.KindToolCall, ToolID: "toolu_b", Tool: "Bash"}
		sm, s, _ := scanServer(t, scanPrompt, bash)
		// One after another, each after the last read ended: a nudge that
		// came while a read was running would be absorbed by that read.
		for range 5 {
			nudge(s)
			time.Sleep(60 * time.Millisecond)
		}
		waitWatches(t, sm, 1)
		time.Sleep(150 * time.Millisecond)
		if n := sm.watchCount(); n != 1 {
			t.Fatalf("%d reads of one waiting spell", n)
		}
		if got := s.Questions().Pending(1); len(got) != 0 {
			t.Fatalf("a question from a Bash call: %+v", got)
		}
		// It stops waiting, then waits again: one more read.
		sm.setWindow(mirror.StatusBusy, nil)
		nudge(s)
		sm.setWindow(mirror.StatusWaiting, nil)
		nudge(s)
		time.Sleep(60 * time.Millisecond)
		nudge(s)
		waitWatches(t, sm, 2)
		time.Sleep(150 * time.Millisecond)
		if n := sm.watchCount(); n != 2 {
			t.Fatalf("%d reads over two waiting spells", n)
		}
	})
}

// TestTheScanReadsOnlyWhatTheHostSaysMayBeAQuestion: today's hosts name what
// a waiting window waits on (Window.Pending); one naming another tool is not
// read, one naming AskUserQuestion is; a window that is not waiting, or not
// Claude's, is not.
func TestTheScanReadsOnlyWhatTheHostSaysMayBeAQuestion(t *testing.T) {
	sm, s, ts := scanServer(t, scanPrompt, gateCall(t))
	for _, c := range []struct {
		name    string
		status  string
		pending *mirror.Pending
	}{
		{"waiting on Bash", mirror.StatusWaiting, &mirror.Pending{Summary: "ls", Tool: "Bash"}},
		{"busy", mirror.StatusBusy, nil},
		{"idle", mirror.StatusIdle, nil},
	} {
		sm.setWindow(c.status, c.pending)
		nudge(s)
		time.Sleep(60 * time.Millisecond)
		if n := sm.watchCount(); n != 0 {
			t.Fatalf("%s: %d transcript read(s)", c.name, n)
		}
	}
	sm.setWindow(mirror.StatusWaiting, &mirror.Pending{Summary: "Which color do you prefer?", Tool: askq.ToolName})
	nudge(s)
	region(t, s, ts, `data-ask-state="pending"`)
}

// TestAnAnsweredQuestionIsNotReopenedByTheScan: the CLI recorded the
// terminal's answer; a read that still shows the call open (a transcript tail
// read before the result landed) never reopens it.
func TestAnAnsweredQuestionIsNotReopenedByTheScan(t *testing.T) {
	sm, s, ts := scanServer(t, scanPrompt, gateCall(t))
	sm.setWindow(mirror.StatusBusy, nil)
	register(t, ts)
	region(t, s, ts, `data-ask-state="pending"`)
	sm.feed(gateTool, gateQs(t), map[string]string{"Which color do you prefer?": "Green", "Which pets do you have?": "Dog"})
	region(t, s, ts, `data-ask-state="answered-terminal"`)
	reads := sm.watchCount()
	sm.setWindow(mirror.StatusWaiting, nil) // the stale tail still shows the call open
	nudge(s)
	waitWatches(t, sm, reads+1)
	time.Sleep(150 * time.Millisecond)
	if got := s.Questions().Pending(1); len(got) != 0 {
		t.Fatalf("an answered question was reopened: %+v", got)
	}
	region(t, s, ts, `data-ask-state="answered-terminal"`)
}

// TestAStaleCallWithAResultIsIgnored: a restarted muster reads a waiting
// window whose transcript holds the AskUserQuestion AND its result (answered
// before the window began waiting on something else): nothing is registered.
func TestAStaleCallWithAResultIsIgnored(t *testing.T) {
	result := cctranscript.Event{Kind: cctranscript.KindToolResult, ToolID: gateTool, OK: true,
		Ask: &cctranscript.Ask{Questions: gateQs(t), Answers: map[string]string{"Which color do you prefer?": "Blue", "Which pets do you have?": "Cat"}}}
	sm, s, ts := scanServer(t, scanPrompt, gateCall(t), result)
	nudge(s)
	waitWatches(t, sm, 1)
	time.Sleep(150 * time.Millisecond)
	if _, found := s.Questions().Find(gateKey); found {
		t.Fatal("a call with a result was registered")
	}
	if n, body := cardCount(t, s, ts); n != 0 {
		t.Fatalf("a card for an answered call:\n%s", body)
	}
	if got := sm.typedSoFar(); len(got) != 0 {
		t.Fatalf("typed %q", got)
	}
}

// TestAFalselyGoneQuestionIsReopened: the listing drops the session past the
// gone grace (expired, saying so) though it has not ended — then lists it
// waiting again with the call still open: the same card is pending and
// answerable again.
func TestAFalselyGoneQuestionIsReopened(t *testing.T) {
	sm, s, ts := scanServer(t, scanPrompt, gateCall(t))
	s.askWatch.mu.Lock()
	s.askWatch.grace = 150 * time.Millisecond
	s.askWatch.mu.Unlock()
	nudge(s)
	region(t, s, ts, `data-ask-state="pending"`)
	first, _ := s.Questions().Find(gateKey)
	sm.fakeMirror.mu.Lock()
	sm.fakeMirror.windows = []mirror.Window{{Session: "main", ID: "@1", Index: 0, Active: true}}
	sm.fakeMirror.mu.Unlock()
	nudge(s)
	deadline := time.Now().Add(5 * time.Second)
	for e, _ := s.Questions().Find(gateKey); e.State != askq.Expired; e, _ = s.Questions().Find(gateKey) {
		if time.Now().After(deadline) {
			t.Fatalf("the unlisted session's question never expired: %+v", e)
		}
		time.Sleep(10 * time.Millisecond)
	}
	sm.setWindow(mirror.StatusWaiting, nil)
	nudge(s)
	region(t, s, ts, `data-ask-state="pending"`)
	if e, _ := s.Questions().Find(gateKey); e.ID != first.ID || e.Note != "" {
		t.Fatalf("the reopened question: %+v (was %s)", e, first.ID)
	}
	if resp, body := answerReq(t, s, ts, answerForm(gateTool, "a0", "Red", "a1", "Cat"), "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("answer to the reopened card: %d\n%s", resp.StatusCode, body)
	}
	if got := sm.typedSoFar(); len(got) != 1 {
		t.Fatalf("typed %q", got)
	}
}

// TestAListedSessionsQuestionOutlivesMaxOpen: while its session is listed the
// watcher marks it seen, so it is still open long past MaxOpen. The control:
// the same question while its host is away (nothing seen) expires past it.
func TestAListedSessionsQuestionOutlivesMaxOpen(t *testing.T) {
	am, s, ts := askServer(t, true)
	var mu sync.Mutex
	now := time.Now()
	s.Questions().SetNow(func() time.Time { mu.Lock(); defer mu.Unlock(); return now })
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	register(t, ts)
	region(t, s, ts, `data-ask-state="pending"`)
	for range 3 { // 3 × (MaxOpen - 1 min) in all, the watcher looking between
		advance(askq.MaxOpen - time.Minute)
		time.Sleep(150 * time.Millisecond)
	}
	s.Questions().Sweep()
	if got := s.Questions().Pending(1); len(got) != 1 {
		t.Fatalf("a listed session's question expired past MaxOpen: %+v", got)
	}
	am.fakeMirror.mu.Lock()
	am.fakeMirror.online = false
	am.fakeMirror.mu.Unlock()
	time.Sleep(150 * time.Millisecond) // the watcher sees the host away
	advance(askq.MaxOpen + time.Minute)
	time.Sleep(150 * time.Millisecond)
	s.Questions().Sweep()
	if got := s.Questions().Pending(1); len(got) != 0 {
		t.Fatalf("the unseen control is still open past MaxOpen: %+v", got)
	}
}
