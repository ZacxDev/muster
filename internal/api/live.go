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
// closes the connection, and the write's own context runs pingWriteGrace
// longer, so it is never the one to close it first. A write that lands in time
// stops the timer.
func writePing(ctx context.Context, c *websocket.Conn, bound time.Duration) error {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bound+pingWriteGrace)
	defer cancel()
	fired := make(chan struct{})
	stall := time.AfterFunc(bound, func() {
		defer close(fired)
		stallFiring()
		notePageWrite(c, context.DeadlineExceeded)
		_ = c.CloseNow()
	})
	beatWriting()
	err := c.Write(wctx, websocket.MessageText, []byte(ui.LivePingFrame))
	if !stall.Stop() {
		// The timer fired, so its callback has STARTED — not necessarily
		// finished. It is waited for: a beat returning before the note
		// landed could be forgotten first (stop's forgetPage) and the note
		// left in stalledPages for good, or land after the handler had
		// classified the end (audit #299 round 3). Noted and closed now; the
		// beat ends.
		<-fired
		if err == nil {
			err = context.DeadlineExceeded
		}
		return err
	}
	notePageWrite(c, err)
	return err
}

// pingWriteGrace is how much longer than its bound a beat's write may run, so
// that the bound's own timer — not the write's context — ends a stalled one.
const pingWriteGrace = time.Second

// Test seams around a beat's stall timer, no-ops in production: stallFiring
// runs as the timer's callback begins, beatWriting just before the beat's
// write. A test holds them to put the callback and the write in a chosen order.
var (
	stallFiring = func() {}
	beatWriting = func() {}
)
