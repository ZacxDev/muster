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
// 🔴 A FAILED BEAT ENDS ONLY THE BEAT, NEVER THE STREAM. Why a stream ended is
// classified by its own handler (streamEndReason: shutdown, timeout,
// client_left), and a heartbeat that cancelled it would put a reason of its own
// in that place. A beat that TIMED OUT is noted like any write to the page
// (notePageWrite), so a page alive and not reading is still a timeout.
//
// 🔴 A BEAT IS NEVER WRITTEN ON THE STREAM'S CONTEXT. coder/websocket closes
// the connection when a write's context ends mid-write, and a shutdown ends the
// stream's context BEFORE its handler tells the page why (shutdownFrame): a beat
// in flight at that moment would close the socket first. The beat's write has a
// context of its own (the stream's values, its own timeout), and ctx only says
// when to stop beating.
//
// 🔴 STOP OWNS THE FORGET. A timed-out beat leaves c in the process-wide
// stalledPages, and only forgetPage removes it; a handler that never forgot a
// page (the agent chat's) leaked one entry per such socket (audit #299). So
// stop() — and nothing earlier — forgets c, once the beat has finished (a beat
// in flight could note c again after an earlier forget). It must therefore run
// AFTER the handler has classified its end (streamEndReason reads the note):
// defer it in the HANDLER, where it runs after the handler's body.
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
	stall := time.AfterFunc(bound, func() {
		notePageWrite(c, context.DeadlineExceeded)
		_ = c.CloseNow()
	})
	err := c.Write(wctx, websocket.MessageText, []byte(ui.LivePingFrame))
	if !stall.Stop() {
		// The timer fired: noted and closed already. The beat ends.
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
