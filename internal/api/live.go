package api

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/ui"
)

// liveHeartbeatEvery is how often a live socket or stream sends its heartbeat:
// ui.LiveHeartbeat (MUSTER_LIVE_HEARTBEAT). A function variable so a test can
// beat faster without touching the process-wide setting.
var liveHeartbeatEvery = ui.LiveHeartbeat

// socketHeartbeat writes ui.LivePingFrame to c every liveHeartbeatEvery until
// ctx ends, stop is called or a write fails. It is how a page tells a socket
// that is alive from one that only LOOKS open (ui/live.go): every socket a page
// watches for staleness gets one.
//
// 🔴 A BEAT NEVER CANCELS THE STREAM AND NEVER NAMES ITS END. Why a stream
// ended is classified by its own handler (streamEndReason: shutdown, timeout,
// client_left). A beat that FAILS (the page gone) just stops beating. A beat
// not written within its bound — the page alive and not reading, or the
// socket's write lock held that long by another writer — is a stalled page:
// writePing notes it (notePageWrite) and THEN closes the connection, so the
// handler sees its page end and finds the note, and calls it a timeout.
//
// 🔴 A BEAT IS NEVER WRITTEN ON THE STREAM'S CONTEXT. coder/websocket closes
// the connection when a write's context ends mid-write, and a shutdown ends the
// stream's context BEFORE its handler tells the page why (shutdownFrame): a beat
// in flight at that moment would close the socket first. The beat's write has a
// context of its own (the stream's values, its own timeout), and ctx only says
// when to stop beating.
//
// 🔴 STOP OWNS THE FORGET. A stalled beat leaves c in the process-wide
// stalledPages, and only forgetPage removes it; a handler that never forgot a
// page (the agent chat's) leaked one entry per such socket (audit #299). So
// the beat's goroutine forgets c when it ends, but not before stop() has been
// called: the note must outlive the handler's classification (streamEndReason
// reads it), so stop is deferred in the HANDLER, where it runs after the
// handler's body. Nothing can note c after that forget: the goroutine's last
// beat has returned, and writePing returns only once a fired stall timer's
// callback has finished noting.
func socketHeartbeat(ctx context.Context, logger *log.Logger, c *websocket.Conn) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	stopped := make(chan struct{})
	// Both read here, on the handler's goroutine: the beat may outlive the
	// handler by a moment (stop does not wait for it), and must not read them
	// then.
	every, bound := liveHeartbeatEvery(), terminalWriteTimeout
	safeGo(logger, "live heartbeat", func() {
		// Whenever the beat ends — stopped, its context gone, or a failed
		// write — the note is forgotten only once stop has been called.
		defer func() { <-stopped; forgetPage(c) }()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := writePing(ctx, c, bound); err != nil {
					return
				}
			}
		}
	})
	var once sync.Once
	return func() { once.Do(func() { cancel(); close(stopped) }) }
}

// writePing writes one heartbeat to c within bound, on a context that ctx's
// end does not cancel. A beat that has not been written by then is a stalled
// page: it is noted (notePageWrite) and the connection closed, IN THAT ORDER.
//
// 🔴 THE NOTE MUST COME BEFORE THE CLOSE. coder/websocket closes the
// connection itself when a write's context expires — from a goroutine of its
// own, before Write returns — and the handler, seeing its page gone, may
// classify the end before a note written after Write returned: an idle page
// that stopped reading (the beat its only writer) was counted client_left, not
// timeout (audit #299 round 2: 1 in 60 single sockets, most runs of 12). So the
// beat keeps the deadline itself: a timer at bound notes the stall and then
// closes the connection (armStall, shared with every page write but the
// terminal keepalive's ping), and the write's own context runs stallGrace
// longer, so it is never the one to close it first. A write that lands in time
// stops the timer.
func writePing(ctx context.Context, c *websocket.Conn, bound time.Duration) error {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bound+stallGrace)
	defer cancel()
	seams := writeSeamsOf(ctx)
	disarm := armStall(seams, c, time.Now().Add(bound))
	seams.beatWriting()
	err := c.Write(wctx, websocket.MessageText, []byte(ui.LivePingFrame))
	if disarm() {
		// The timer fired, and disarm has waited for its callback to finish:
		// a beat returning before the note landed could be forgotten first
		// (stop's forgetPage) and the note left in stalledPages for good, or
		// land after the handler had classified the end (audit #299 round 3).
		// Noted and closed now; the beat ends.
		return stalledWriteErr(err)
	}
	notePageWrite(seams, c, err)
	return err
}

// writeSeams are the test seams around one write to a page — a beat
// (writePing) or any other page write (pageWrite) — and its stall timer
// (armStall). They ride on the CONTEXT of the write they hook (withWriteSeams),
// and production never sets them: writeSeamsOf then returns nil, whose methods
// are no-ops.
//
// 🔴 NEVER A PACKAGE VARIABLE. A beat's goroutine outlives the handler that
// started it by a moment (socketHeartbeat's stop does not wait for it), and
// nothing makes a handler end with the test that opened its socket. A
// seam held in a package variable was read by a beat one test had left behind
// while the next test assigned it: a data race, and a hook firing on a write
// that was never the test's (TestAStallCallbackFinishesBeforeTheBeatReturns,
// on CI). On the write's own context, a seam reaches exactly the writes made
// on that context — a test's, never another test's.
type writeSeams struct {
	stallFiring  func() // the stall timer's callback begins (armStall)
	beatWrite    func() // just before a beat's write (writePing)
	writeStarts  func() // just before a page write (pageWrite)
	writeReturns func() // a page write returned, before its outcome is noted
	stallNoted   func() // a stall was noted (notePageWrite)
}

type writeSeamsKey struct{}

// withWriteSeams returns ctx carrying s for every write made on it.
func withWriteSeams(ctx context.Context, s *writeSeams) context.Context {
	return context.WithValue(ctx, writeSeamsKey{}, s)
}

// writeSeamsOf is the seams ctx carries, nil (all no-ops) when it carries none.
func writeSeamsOf(ctx context.Context) *writeSeams {
	s, _ := ctx.Value(writeSeamsKey{}).(*writeSeams)
	return s
}

func runSeam(f func()) {
	if f != nil {
		f()
	}
}

func (s *writeSeams) fireStall() {
	if s != nil {
		runSeam(s.stallFiring)
	}
}

func (s *writeSeams) beatWriting() {
	if s != nil {
		runSeam(s.beatWrite)
	}
}

func (s *writeSeams) writeStarting() {
	if s != nil {
		runSeam(s.writeStarts)
	}
}

func (s *writeSeams) writeReturned() {
	if s != nil {
		runSeam(s.writeReturns)
	}
}

func (s *writeSeams) noteStall() {
	if s != nil {
		runSeam(s.stallNoted)
	}
}
