package api

import (
	"context"
	"strconv"
	"sync"

	"github.com/ZacxDev/muster/internal/push"
)

// ---------------------------------------------------------------------------
// ORDER FOR A TAG THAT IS WITHDRAWN (push_tags.go; docs/push.md "When a
// notification is withdrawn").
//
// goNotify sends each notification on its own goroutine, with no order per
// tag, and a device closes a `resolved` tag only if it is showing NOW: a
// withdrawal that lands before the alert (or the quiet replacement) it
// follows closes nothing, the earlier one then shows, and nothing withdraws
// it again — the waiting thing is gone, so nothing will. Two guards, both for
// the fleet push, plugin pairing and connector requests:
//
//   - IN THIS PROCESS, ONE SEND PER (owner, tag) AT A TIME (goNotifyOrdered):
//     each is handed to the devices — or given up — before the next starts.
//   - ACROSS PROCESSES (two replicas) AND AS A BACKSTOP, A RE-CHECK: a
//     withdrawal (or a replacement) is checked again after
//     pushBroadcastTimeout — the bound on one fan-out, so whatever was in
//     flight anywhere has landed — and sent again if the thing is still gone
//     (recheckLater, the episodes' clearEpisodeLater's scheduler).
// ---------------------------------------------------------------------------

// orderedPush is one send waiting its turn under its (owner, tag).
type orderedPush struct {
	owner int64
	event string
	n     RouterNotification
}

// pushOrder is the per-(owner, tag) queues of goNotifyOrdered.
type pushOrder struct {
	mu   sync.Mutex
	busy map[string][]orderedPush
}

// goNotifyOrdered is goNotify, in order with every other goNotifyOrdered of
// the same owner and tag: it starts only once the one before it has been
// handed to the devices (or refused). It never blocks the caller.
func (s *Server) goNotifyOrdered(owner int64, event string, n RouterNotification) {
	key := strconv.FormatInt(owner, 10) + "\x00" + n.Tag
	o := &s.pushOrder
	o.mu.Lock()
	if o.busy == nil {
		o.busy = map[string][]orderedPush{}
	}
	if q, running := o.busy[key]; running {
		o.busy[key] = append(q, orderedPush{owner, event, n})
		o.mu.Unlock()
		return
	}
	o.busy[key] = nil // running, nothing queued behind it
	o.mu.Unlock()
	next := orderedPush{owner, event, n}
	// Held until the queue drains, so WaitForPushes waits for every send
	// queued behind this one too.
	s.pushInFlight.Add(1)
	safeGoDone(s.logger, "notify in order tag="+n.Tag, &s.pushInFlight, func() {
		done := false
		defer func() {
			if !done { // a panic: free the tag rather than queue behind it forever
				o.mu.Lock()
				delete(o.busy, key)
				o.mu.Unlock()
			}
		}()
		for {
			var wg sync.WaitGroup
			m := next.n
			m.wait = &wg
			s.goNotify(next.owner, next.event, m, nil)
			wg.Wait()
			o.mu.Lock()
			q := o.busy[key]
			if len(q) == 0 {
				delete(o.busy, key)
				o.mu.Unlock()
				done = true
				return
			}
			next, o.busy[key] = q[0], q[1:]
			o.mu.Unlock()
		}
	})
}

// withdrawSettled withdraws owner's notification tag (id its payload id), in
// order with the tag's other sends, and re-checks it after one fan-out: still
// gone (gone answers on octx's principal), the withdrawal is sent again. key
// names the re-check (one pending per key).
func (s *Server) withdrawSettled(octx context.Context, owner int64, id, tag string, gone func(rctx context.Context) bool) {
	n := RouterNotification{Type: push.TypeResolved, ID: id, Tag: tag}
	s.goNotifyOrdered(owner, push.EventResolved, n)
	s.recheckLater(octx, "withdraw-"+strconv.FormatInt(owner, 10)+"-"+tag, pushBroadcastTimeout, func(rctx context.Context) {
		if gone == nil || gone(rctx) {
			s.goNotifyOrdered(owner, push.EventResolved, n)
		}
	})
}
