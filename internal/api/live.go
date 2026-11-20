package api

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/ui"
)

// socketHeartbeat writes ui.LivePingFrame to c every heartbeat (ctx's
// socketSettings: ui.LiveHeartbeat, MUSTER_LIVE_HEARTBEAT, in production) until
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
	// Both read here, from ctx's socketSettings — never a package variable a
	// test could reassign while this beat, outliving its test, still ran.
	set := socketSettingsOf(ctx)
	every, bound := set.heartbeatEvery(), set.writeTimeout()
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
	seams := socketSettingsOf(ctx)
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

// socketSettings are the timings of a page's socket and the test seams around
// its writes. They ride on the CONTEXT of the socket or write they apply to
// (withSocketSettings), set once before it opens and never changed after;
// production sets none, and socketSettingsOf then returns nil, whose timings
// are the production defaults (terminalWriteTimeout, pageWriteGrace,
// terminalPingInterval, ui.LiveHeartbeat) and whose seams are no-ops. A zero
// timing is its default too.
//
// 🔴 NEVER A PACKAGE VARIABLE A TEST REASSIGNS. A beat's goroutine outlives
// the handler that started it by a moment (socketHeartbeat's stop does not
// wait for it), and nothing makes a handler end with the test that opened its
// socket. A seam or timing held in a package variable was read by a socket
// one test had left behind while the next test assigned it: a data race
// (TestAStallCallbackFinishesBeforeTheBeatReturns on CI; the write timeout,
// reassigned in a test's cleanup, against socketHeartbeat's read), and a hook
// or bound applied to a socket that was never the test's. On the context, a
// setting reaches exactly the sockets and writes made on it.
type socketSettings struct {
	every time.Duration // the heartbeat (socketHeartbeat, the /events stream)
	bound time.Duration // one write to the page (pageWriteContext, the beat, the keepalive)
	grace time.Duration // a page write past its stream's cancellation (pageWriteContext)
	ping  time.Duration // the terminal keepalive's interval (bridgeTerminal)

	stallFiring  func() // the stall timer's callback begins (armStall)
	beatWrite    func() // just before a beat's write (writePing)
	writeStarts  func() // just before a page write (pageWrite)
	writeReturns func() // a page write returned, before its outcome is noted
	stallNoted   func() // a stall was noted (notePageWrite)
}

type socketSettingsKey struct{}

// withSocketSettings returns ctx carrying s for every socket and write on it.
func withSocketSettings(ctx context.Context, s *socketSettings) context.Context {
	return context.WithValue(ctx, socketSettingsKey{}, s)
}

// socketSettingsOf is the settings ctx carries, nil (production's) when none.
func socketSettingsOf(ctx context.Context) *socketSettings {
	s, _ := ctx.Value(socketSettingsKey{}).(*socketSettings)
	return s
}

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func (s *socketSettings) heartbeatEvery() time.Duration {
	if s == nil || s.every <= 0 {
		return ui.LiveHeartbeat()
	}
	return s.every
}

func (s *socketSettings) writeTimeout() time.Duration {
	if s == nil {
		return terminalWriteTimeout
	}
	return orDefault(s.bound, terminalWriteTimeout)
}

func (s *socketSettings) writeGrace() time.Duration {
	if s == nil {
		return pageWriteGrace
	}
	return orDefault(s.grace, pageWriteGrace)
}

func (s *socketSettings) pingInterval() time.Duration {
	if s == nil {
		return terminalPingInterval
	}
	return orDefault(s.ping, terminalPingInterval)
}

func runSeam(f func()) {
	if f != nil {
		f()
	}
}

func (s *socketSettings) fireStall() {
	if s != nil {
		runSeam(s.stallFiring)
	}
}

func (s *socketSettings) beatWriting() {
	if s != nil {
		runSeam(s.beatWrite)
	}
}

func (s *socketSettings) writeStarting() {
	if s != nil {
		runSeam(s.writeStarts)
	}
}

func (s *socketSettings) writeReturned() {
	if s != nil {
		runSeam(s.writeReturns)
	}
}

func (s *socketSettings) noteStall() {
	if s != nil {
		runSeam(s.stallNoted)
	}
}
