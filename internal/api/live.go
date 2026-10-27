package api

import (
	"context"
	"log"
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
func socketHeartbeat(ctx context.Context, logger *log.Logger, c *websocket.Conn) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	// Both read here, on the handler's goroutine: the beat may outlive the
	// handler by a moment (stop does not wait for it), and must not read them
	// then.
	every, bound := liveHeartbeatEvery(), terminalWriteTimeout
	safeGo(logger, "live heartbeat", func() {
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
	return cancel
}

// writePing writes one heartbeat to c within bound, noting a timed-out write
// (notePageWrite), on a context that ctx's end does not cancel.
func writePing(ctx context.Context, c *websocket.Conn, bound time.Duration) error {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bound)
	defer cancel()
	err := c.Write(wctx, websocket.MessageText, []byte(ui.LivePingFrame))
	notePageWrite(c, err)
	return err
}
