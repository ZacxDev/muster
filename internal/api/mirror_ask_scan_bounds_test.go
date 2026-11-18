package api

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
)

// ---------------------------------------------------------------------------
// THE SCAN'S READS ARE BOUNDED, RETRIED, AND COUNT ONLY WHEN COMPLETE AND
// CURRENT (mirror_ask_scan.go): a stale read never replaces the question on
// screen, a host's reads take at most askScanPerHost of its watch slots, a
// refused or partial read is retried on a timer (bounded, cancelled with its
// waiting spell), and a read whose window stopped waiting meanwhile counts for
// nothing.
// ---------------------------------------------------------------------------

// heldMirror is scanMirror whose FIRST transcript read captures the
// transcript as it is when the read starts and delivers it (complete: the
// backfill, then the prompt index) only once release is closed — a slow read.
type heldMirror struct {
	*scanMirror
	started, release chan struct{}
	hmu              sync.Mutex
	calls            int
}

func newHeldMirror(sm *scanMirror) *heldMirror {
	return &heldMirror{scanMirror: sm, started: make(chan struct{}), release: make(chan struct{})}
}

func (m *heldMirror) callCount() int {
	m.hmu.Lock()
	defer m.hmu.Unlock()
	return m.calls
}

func (m *heldMirror) WatchWindow(ctx context.Context, host, session, window, gen string, offset int64, out func(mirror.Msg) error) error {
	m.hmu.Lock()
	m.calls++
	n := m.calls
	m.hmu.Unlock()
	if n != 1 {
		return m.scanMirror.WatchWindow(ctx, host, session, window, gen, offset, out)
	}
	m.tmu.Lock()
	stale := append([]cctranscript.Event(nil), m.transcript...)
	m.tmu.Unlock()
	close(m.started)
	<-m.release
	if err := out(mirror.Msg{Type: mirror.TypeEvents, Gen: "sess-1", Reset: true, Events: stale}); err != nil {
		return err
	}
	return out(mirror.Msg{Type: mirror.TypeEvents, Gen: "sess-1", PromptIndex: true})
}

// heldServer is scanServer on a heldMirror.
func heldServer(t *testing.T, evs ...cctranscript.Event) (*heldMirror, *Server, *httptest.Server) {
	t.Helper()
	sm := &scanMirror{askMirror: newAskMirror(true)}
	sm.setWindow(mirror.StatusWaiting, nil)
	sm.setTranscript(evs...)
	hm := newHeldMirror(sm)
	s, ts := mirrorServer(t, hm, newFakeTerminal())
	s.askWatch.mu.Lock()
	s.askWatch.retry = 20 * time.Millisecond
	s.askWatch.mu.Unlock()
	fastScanRetry(s)
	t.Cleanup(s.EndStreams)
	return hm, s, ts
}

// scanSettled waits until no scan read is running, then a little longer for
// the last one's restore.
func scanSettled(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.askScan.mu.Lock()
		n := len(s.askScan.busy)
		s.askScan.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d scan read(s) still running", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
}

// newerTool is a second AskUserQuestion call in sess-1.
const newerTool = "toolu_01ZZZZZZZZZZZZZZZZZZZZZZZZ"

// TestAStaleScanReadNeverSupersedesTheOnScreenQuestion: a restarted muster's
// scan read starts while Q1 is open; before it lands, Q1 is answered in the
// terminal and Claude asks Q2 — the same text — which the hook registers. The
// stale read must not register Q1: Q2 stays pending and the only card, and
// answering Q1's card cannot type into Q2's picker.
func TestAStaleScanReadNeverSupersedesTheOnScreenQuestion(t *testing.T) {
	q1 := gateCall(t)
	hm, s, ts := heldServer(t, scanPrompt, q1)
	nudge(s) // the host connects: the scan's (slow) read starts
	<-hm.started
	res := cctranscript.Event{Kind: cctranscript.KindToolResult, ToolID: gateTool, OK: true,
		Ask: &cctranscript.Ask{Questions: gateQs(t), Answers: map[string]string{"Which color do you prefer?": "Red", "Which pets do you have?": "Dog"}}}
	q2 := q1
	q2.ToolID = newerTool
	hm.setTranscript(scanPrompt, q1, res, q2)
	if code := openAsk(t, ts, "mmt_ws", "sess-1", newerTool, gateInput(t)); code < 200 || code > 299 {
		t.Fatalf("the hook's registration of Q2: %d", code)
	}
	k2 := gateKey
	k2.ToolUseID = newerTool
	waitWatches(t, hm.scanMirror, 1) // Q2's own watch, past Q1's result
	close(hm.release)                // the stale read lands
	scanSettled(t, s)

	if e, _ := s.Questions().Find(k2); e.State != askq.Pending {
		t.Fatalf("Q2 (on screen) after the stale read: %+v", e)
	}
	if got := s.Questions().Pending(1); len(got) != 1 || got[0].Key != k2 {
		t.Fatalf("open questions %+v, want Q2 alone", got)
	}
	if e, found := s.Questions().Find(gateKey); found && e.State.Open() {
		t.Fatalf("the stale read reopened Q1: %+v", e)
	}
	_, _ = answerReq(t, s, ts, answerForm(gateTool, "a0", "Red", "a1", "Dog"), "", "")
	if got := hm.typedSoFar(); len(got) != 0 {
		t.Fatalf("answering Q1's card typed into the terminal: %q", got)
	}
}

// TestAReadWhoseWindowStoppedWaitingCountsForNothing: the window stops
// waiting while its read is in flight; the read lands showing the call open.
// It registers nothing, and it does not count as the window's read: when the
// window waits again — after the read landed, or already before — it is read
// again, and what that read shows is what counts.
func TestAReadWhoseWindowStoppedWaitingCountsForNothing(t *testing.T) {
	bash := cctranscript.Event{Kind: cctranscript.KindToolCall, ToolID: "toolu_b", Tool: "Bash"}
	waitCalls := func(t *testing.T, hm *heldMirror, n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for hm.callCount() < n {
			if time.Now().After(deadline) {
				t.Fatalf("%d read(s): the window waiting anew was not read", hm.callCount())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	for _, anewFirst := range []bool{false, true} {
		name := "waits anew after the read lands"
		if anewFirst {
			name = "waits anew before the read lands"
		}
		t.Run(name, func(t *testing.T) {
			hm, s, _ := heldServer(t, scanPrompt, gateCall(t))
			nudge(s)
			<-hm.started
			hm.setWindow(mirror.StatusBusy, nil) // answered in the terminal: a turn runs
			nudge(s)
			anew := func() {
				hm.setTranscript(scanPrompt, bash) // now waiting on a Bash approval
				hm.setWindow(mirror.StatusWaiting, nil)
				nudge(s)
			}
			if anewFirst {
				anew()
			}
			close(hm.release)
			scanSettled(t, s)
			if !anewFirst {
				if e, found := s.Questions().Find(gateKey); found {
					t.Fatalf("a read of an ended waiting spell registered %+v", e)
				}
				anew()
			}
			waitCalls(t, hm, 2)
			scanSettled(t, s)
			if e, found := s.Questions().Find(gateKey); found {
				t.Fatalf("a read of an ended waiting spell registered %+v", e)
			}
		})
	}
}

// refusingMirror is scanMirror whose host refuses the first refuse reads
// (refuse < 0: every read) with err.
type refusingMirror struct {
	*scanMirror
	rmu    sync.Mutex
	refuse int
	err    error
	calls  int
}

func (m *refusingMirror) callCount() int {
	m.rmu.Lock()
	defer m.rmu.Unlock()
	return m.calls
}

func (m *refusingMirror) WatchWindow(ctx context.Context, host, session, window, gen string, offset int64, out func(mirror.Msg) error) error {
	m.rmu.Lock()
	m.calls++
	refused := m.refuse < 0 || m.calls <= m.refuse
	m.rmu.Unlock()
	if refused {
		return m.err
	}
	return m.scanMirror.WatchWindow(ctx, host, session, window, gen, offset, out)
}

func refusingServer(t *testing.T, refuse int, evs ...cctranscript.Event) (*refusingMirror, *Server, *httptest.Server) {
	t.Helper()
	sm := &scanMirror{askMirror: newAskMirror(true)}
	sm.setWindow(mirror.StatusWaiting, nil)
	sm.setTranscript(evs...)
	rm := &refusingMirror{scanMirror: sm, refuse: refuse, err: fmt.Errorf("%w (host %q)", mirror.ErrTooManyWatches, "ws")}
	s, ts := mirrorServer(t, rm, newFakeTerminal())
	s.askWatch.mu.Lock()
	s.askWatch.retry = 20 * time.Millisecond
	s.askWatch.mu.Unlock()
	fastScanRetry(s)
	t.Cleanup(s.EndStreams)
	return rm, s, ts
}

// TestARefusedReadIsRetried: the host refuses the first two reads (its watch
// slots are full, ErrTooManyWatches); with NO further nudge, the read is
// retried and the question's card appears.
func TestARefusedReadIsRetried(t *testing.T) {
	rm, s, ts := refusingServer(t, 2, scanPrompt, gateCall(t))
	nudge(s)
	region(t, s, ts, `data-ask-state="pending"`)
	if n := rm.callCount(); n < 3 {
		t.Fatalf("%d read(s), want the two refused and a third", n)
	}
}

// TestARetryIsBoundedAndEndsWithItsSpell: a host that refuses every read is
// read at most askScanAttempts times in one waiting spell, nudged or not; and
// the retries stop when the window stops waiting, or the host goes.
func TestARetryIsBoundedAndEndsWithItsSpell(t *testing.T) {
	waitCalls := func(t *testing.T, rm *refusingMirror, n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for rm.callCount() < n {
			if time.Now().After(deadline) {
				t.Fatalf("%d read(s), want %d", rm.callCount(), n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Run("bounded", func(t *testing.T) {
		rm, s, _ := refusingServer(t, -1, scanPrompt, gateCall(t))
		nudge(s)
		waitCalls(t, rm, askScanAttempts)
		time.Sleep(1500 * time.Millisecond) // past the next retry, had there been one
		nudge(s)
		time.Sleep(150 * time.Millisecond)
		if n := rm.callCount(); n != askScanAttempts {
			t.Fatalf("%d reads of one waiting spell, want %d", n, askScanAttempts)
		}
	})
	for _, c := range []struct {
		name string
		end  func(rm *refusingMirror)
	}{
		{"the window stops waiting", func(rm *refusingMirror) { rm.setWindow(mirror.StatusBusy, nil) }},
		{"the host goes", func(rm *refusingMirror) { rm.setOnline(false) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			rm, s, _ := refusingServer(t, -1, scanPrompt, gateCall(t))
			setScanRetry(s, 100*time.Millisecond)
			nudge(s)
			waitCalls(t, rm, 1)
			c.end(rm)
			nudge(s)
			n := rm.callCount()
			time.Sleep(700 * time.Millisecond) // three retries' worth, had they run
			if got := rm.callCount(); got != n {
				t.Fatalf("%d read(s) after the spell ended (was %d)", got, n)
			}
		})
	}
}

// partialMirror is scanMirror whose FIRST read delivers the transcript
// captured then WITHOUT the prompt index and never completes — a read the
// quiet timer cuts.
type partialMirror struct {
	*scanMirror
	pmu   sync.Mutex
	calls int
	first []cctranscript.Event
}

func (m *partialMirror) callCount() int {
	m.pmu.Lock()
	defer m.pmu.Unlock()
	return m.calls
}

func (m *partialMirror) WatchWindow(ctx context.Context, host, session, window, gen string, offset int64, out func(mirror.Msg) error) error {
	m.pmu.Lock()
	m.calls++
	n := m.calls
	m.pmu.Unlock()
	if n != 1 {
		return m.scanMirror.WatchWindow(ctx, host, session, window, gen, offset, out)
	}
	if err := out(mirror.Msg{Type: mirror.TypeEvents, Gen: "sess-1", Reset: true, Events: m.first}); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

// TestAPartialReadIsNotTheWindowsRead: the first read is cut before the
// prompt index — its tail shows the call open, the result that follows it
// missing. Nothing is restored from it, and it is retried: the complete read
// shows the call answered, and no card ever appears.
func TestAPartialReadIsNotTheWindowsRead(t *testing.T) {
	result := cctranscript.Event{Kind: cctranscript.KindToolResult, ToolID: gateTool, OK: true,
		Ask: &cctranscript.Ask{Questions: gateQs(t), Answers: map[string]string{"Which color do you prefer?": "Blue", "Which pets do you have?": "Cat"}}}
	sm := &scanMirror{askMirror: newAskMirror(true)}
	sm.setWindow(mirror.StatusWaiting, nil)
	sm.setTranscript(scanPrompt, gateCall(t), result)
	pm := &partialMirror{scanMirror: sm, first: []cctranscript.Event{scanPrompt, gateCall(t)}}
	s, _ := mirrorServer(t, pm, newFakeTerminal())
	fastScanRetry(s)
	t.Cleanup(s.EndStreams)
	nudge(s)
	deadline := time.Now().Add(10 * time.Second)
	for pm.callCount() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("%d read(s): the partial read was not retried", pm.callCount())
		}
		if _, found := s.Questions().Find(gateKey); found {
			t.Fatal("a question restored from a partial read")
		}
		time.Sleep(5 * time.Millisecond)
	}
	scanSettled(t, s)
	if e, found := s.Questions().Find(gateKey); found {
		t.Fatalf("a question registered for an answered call: %+v", e)
	}
}

// manyMirror is a host listing manyWindows waiting Claude windows, each with
// its own AskUserQuestion open. Its watch slots are shared with what else it
// serves: at most manyFree reads run at once before a window's first complete
// read (a scan's); past that it refuses with ErrTooManyWatches. Each read takes
// a moment. Reads after a window's first complete one (its question's watch)
// are served and not counted.
type manyMirror struct {
	*askMirror
	mmu          sync.Mutex
	inflight     int
	maxInflight  int
	served       map[string]bool
	refused      int
	transcriptOf func(window string) []cctranscript.Event
}

const (
	manyWindows = 12
	manyFree    = 3
)

func manyTool(i int) string { return fmt.Sprintf("toolu_01RY27FfKTitvDGupDC74z%02d", i) }

func (m *manyMirror) WatchWindow(ctx context.Context, host, session, window, gen string, offset int64, out func(mirror.Msg) error) error {
	var i int
	if _, err := fmt.Sscanf(window, "@%d", &i); err != nil || host != "ws" || session != "main" {
		return mirror.ErrHostOffline
	}
	sid := fmt.Sprintf("sess-%d", i)
	m.mmu.Lock()
	scan := !m.served[window]
	if scan {
		if m.inflight >= manyFree {
			m.refused++
			m.mmu.Unlock()
			return fmt.Errorf("%w (host %q)", mirror.ErrTooManyWatches, host)
		}
		m.inflight++
		m.maxInflight = max(m.maxInflight, m.inflight)
	}
	m.mmu.Unlock()
	if scan {
		defer func() {
			m.mmu.Lock()
			m.inflight--
			m.served[window] = true
			m.mmu.Unlock()
		}()
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(30 * time.Millisecond):
		}
	}
	if err := out(mirror.Msg{Type: mirror.TypeEvents, Gen: sid, Reset: true, Events: m.transcriptOf(window)}); err != nil {
		return err
	}
	if err := out(mirror.Msg{Type: mirror.TypeEvents, Gen: sid, PromptIndex: true}); err != nil {
		return err
	}
	if scan {
		return nil
	}
	<-ctx.Done()
	return nil
}

// TestMoreWaitingWindowsThanSlotsAllGetTheirCards: a host connects with more
// waiting windows than it has free watch slots. The scan never runs more than
// askScanPerHost of its reads at once, and every window's card appears.
func TestMoreWaitingWindowsThanSlotsAllGetTheirCards(t *testing.T) {
	am := newAskMirror(true)
	var ws []mirror.Window
	for i := 1; i <= manyWindows; i++ {
		ws = append(ws, mirror.Window{Session: "main", ID: fmt.Sprintf("@%d", i), Index: i - 1, Claude: true,
			ClaudeSession: fmt.Sprintf("sess-%d", i), Status: mirror.StatusWaiting})
	}
	am.fakeMirror.windows = ws
	mm := &manyMirror{askMirror: am, served: map[string]bool{}}
	mm.transcriptOf = func(window string) []cctranscript.Event {
		var i int
		_, _ = fmt.Sscanf(window, "@%d", &i)
		call := gateCall(t)
		call.ToolID = manyTool(i)
		return []cctranscript.Event{scanPrompt, call}
	}
	s, _ := mirrorServer(t, mm, newFakeTerminal())
	s.askWatch.mu.Lock()
	s.askWatch.retry = 20 * time.Millisecond
	s.askWatch.mu.Unlock()
	fastScanRetry(s)
	t.Cleanup(s.EndStreams)
	nudge(s)
	deadline := time.Now().Add(10 * time.Second)
	for len(s.Questions().Pending(1)) < manyWindows {
		if time.Now().After(deadline) {
			mm.mmu.Lock()
			t.Fatalf("%d of %d cards (%d read(s) refused, at most %d at once)", len(s.Questions().Pending(1)), manyWindows, mm.refused, mm.maxInflight)
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i := 1; i <= manyWindows; i++ {
		k := askq.Key{Owner: 1, Source: askSourceMirror, Scope: "ws", Session: fmt.Sprintf("sess-%d", i), ToolUseID: manyTool(i)}
		if e, _ := s.Questions().Find(k); e.State != askq.Pending {
			t.Fatalf("window @%d's question: %+v", i, e)
		}
	}
	mm.mmu.Lock()
	defer mm.mmu.Unlock()
	if mm.maxInflight > askScanPerHost || mm.refused != 0 {
		t.Fatalf("at most %d scan reads ran at once (want ≤ %d); %d refused", mm.maxInflight, askScanPerHost, mm.refused)
	}
}

// TestNudgesWhileItsQuestionIsOpenUseNoAttempts: while the window's session
// has an open question (its watch covers it) every nudge skips the read
// without counting it — so many nudges then the question answered in the
// terminal, the window still waiting, and the next nudge still reads it.
func TestNudgesWhileItsQuestionIsOpenUseNoAttempts(t *testing.T) {
	sm, s, ts := scanServer(t, scanPrompt, gateCall(t))
	sm.setWindow(mirror.StatusBusy, nil) // not read: the hook registers it
	register(t, ts)
	region(t, s, ts, `data-ask-state="pending"`)
	waitWatches(t, sm, 1) // the question's own watch
	sm.setWindow(mirror.StatusWaiting, nil)
	for range askScanAttempts + 2 {
		nudge(s)
		time.Sleep(20 * time.Millisecond)
	}
	scanSettled(t, s)
	if n := sm.watchCount(); n != 1 {
		t.Fatalf("%d transcript reads: the scan read a session its watch covers", n)
	}
	sm.feed(gateTool, gateQs(t), map[string]string{"Which color do you prefer?": "Green", "Which pets do you have?": "Dog"})
	region(t, s, ts, `data-ask-state="answered-terminal"`)
	nudge(s)              // (resolving it nudges too)
	waitWatches(t, sm, 2) // the scan's read: the window still waits
}
