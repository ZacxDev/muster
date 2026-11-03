package clankmailplugin

// held.go — a pinned peer's messages that cannot be judged finally yet.
//
// WHY. A peer is `unverified` until its first chain check: a fresh or lost
// STATE_DIR, a relabel (New), a boot where the hub's handle route fails. The
// hub can still hold that key on our allow list from before, so its mail keeps
// reaching our `inbox` queue meanwhile, and the inbox poll runs concurrently
// with the peer check. "Not checked yet" says nothing about the message:
// judging it then (peer_disabled) and moving the cursor past it lost it for
// good. `disabled` is no verdict on a message either: a hub that serves a
// forged handle disables the peer, and the peer's authentic mail must survive
// that until a human re-checks the chain. And a pinned peer's message that the
// hub put in `requests` (it was submitted before our allow call reached the
// hub, or while the peer was disabled and so off the allow list) is that
// peer's authentic mail too.
//
// WHAT. Such a message is HELD: its id, signer, queue and the hub's unsigned
// thread (compared, never sent) are appended to state.held in the same write
// that moves the cursor past it. Nothing of its text is kept. Every inbox poll
// first walks state.held in order. An entry whose peer is still not
// deliverable stays. Otherwise the message is fetched again (authenticated GET
// /v1/objects/{id}: a message whose `to` holds our key is visible to us),
// passes the whole §4 pipeline again with its id recomputed, and is judged and
// settled exactly like a queue item (relay.go), its record written in the same
// state write that drops the entry. So exactly-once is the inbox's rule: a
// crash between muster's 2xx and that write re-posts, and muster dedupes on
// external_id.
//
// HELD WHILE the peer is `unverified` or `disabled`, ONE rule for both queues:
// a pinned peer's message is never judged finally on its peer's status.
// `unverified` is released by the first chain check; `disabled` is sticky
// (peersync.go) and released only by a human's `peer verify`. Before this
// rule an inbox message from a disabled peer was final while a later requests
// message from it was held, so `peer verify` delivered the second and never
// the first: a thread with a hole. A disabled peer's held entries are reported
// per peer by `show`, /health and `peer verify` (Summary, VerifyPeer).
//
// ORDER is per peer: while a peer has a held entry, its later messages are
// held behind it, so they reach muster in the order they were read. Other
// peers are NOT blocked: a peer stuck unverified (the hub's handle route down
// for days) holds only its own mail. The cost of that is that a stuck peer's
// held entries accumulate in state.json (ids only), and each inbox poll looks
// them up without touching the network until the peer's status changes.

import (
	"context"
	"sort"
)

// HeldItem is one held message: who and where, never what it says.
type HeldItem struct {
	ID     string `json:"id"`
	From   string `json:"from"`
	Queue  string `json:"queue"`            // "inbox" or "requests"
	Thread string `json:"thread,omitempty"` // the hub's unsigned claim, only compared
	At     int64  `json:"at"`
}

// stillHeld: a message from key cannot be judged finally now.
func (p *Plugin) stillHeld(key string) bool {
	_, status, ok := p.peerFor(key)
	if !ok {
		return false // no longer pinned: judged (not_peer, or listed as a stranger's request)
	}
	return status == peerUnverified || status == peerDisabled
}

// mustHold: v (an authentic item, from either queue) is held rather than settled.
func (p *Plugin) mustHold(v verdict) bool {
	switch v.outcome {
	case outUnverified, outPeerDisabled:
		return true
	case "", outMusterRefused: // the peer is ok: held only behind its own earlier held mail
		return p.heldFrom(v.obj.From)
	}
	return false
}

func (p *Plugin) isHeld(id string) (held bool) {
	p.st.view(func(s *State) {
		for _, h := range s.Held {
			if h.ID == id {
				held = true
				return
			}
		}
	})
	return held
}

func (p *Plugin) heldFrom(key string) (held bool) {
	p.st.view(func(s *State) {
		for _, h := range s.Held {
			if h.From == key {
				held = true
				return
			}
		}
	})
	return held
}

func dropHeld(s *State, id string) {
	out := s.Held[:0]
	for _, h := range s.Held {
		if h.ID != id {
			out = append(out, h)
		}
	}
	s.Held = out
}

// hold appends v's message to state.held, in the same write as move.
func (p *Plugin) hold(v verdict, queue, hubThread string, move func(*State)) error {
	if err := p.st.update(func(s *State) error {
		s.Held = append(s.Held, &HeldItem{ID: v.obj.ID, From: v.obj.From, Queue: queue, Thread: hubThread, At: p.now().Unix()})
		move(s)
		return nil
	}); err != nil {
		return errTransient{err}
	}
	p.log.Info("held: a pinned peer's message waits for the peer's chain to verify (a disabled peer: for `peer verify`)", "message", v.obj.ID,
		"peer", v.peer.Label, "queue", queue, "why", v.outcome)
	return nil
}

// retryHeld walks state.held in order and settles every entry whose peer is
// now deliverable (or no longer pinned). A transient failure stops the walk
// with that entry still held.
func (p *Plugin) retryHeld(ctx context.Context) error {
	var held []HeldItem
	p.st.view(func(s *State) {
		for _, h := range s.Held {
			held = append(held, *h)
		}
	})
	blocked := map[string]bool{} // peers with an earlier entry still held: keep their order
	for _, h := range held {
		if blocked[h.From] {
			continue
		}
		if p.stillHeld(h.From) {
			blocked[h.From] = true
			continue
		}
		drop := func(s *State) { dropHeld(s, h.ID) }
		if p.alreadyDone(h.ID) {
			if err := p.st.update(func(s *State) error { drop(s); return nil }); err != nil {
				return errTransient{err}
			}
			continue
		}
		tok, found, err := p.hub.Object(ctx, h.ID)
		if err != nil {
			return errTransient{err}
		}
		if !found {
			p.log.Warn("held message: the hub no longer serves it; not delivered", "message", h.ID)
			if err := p.st.update(func(s *State) error {
				drop(s)
				s.Messages[h.ID] = &MsgRecord{Dir: "in", From: h.From, Outcome: outGone, At: p.now().Unix()}
				return nil
			}); err != nil {
				return errTransient{err}
			}
			continue
		}
		v := p.judge(tok)
		if v.outcome == "rejected" || v.obj.ID != h.ID {
			p.log.Warn("held message: the hub served bytes that do not verify as it; not delivered", "message", h.ID)
			if err := p.st.update(func(s *State) error { drop(s); s.Rejected++; return nil }); err != nil {
				return errTransient{err}
			}
			continue
		}
		switch {
		case h.Queue == queueRequests && v.outcome == outNotPeer:
			// The key is no longer pinned: a stranger's request now, listed only.
			if err := p.recordRequest(v.obj, drop); err != nil {
				return err
			}
			continue
		case v.outcome == outUnverified || v.outcome == outPeerDisabled:
			blocked[h.From] = true // the status changed back meanwhile
			continue
		}
		if err := p.settle(ctx, v, h.Thread, drop); err != nil {
			return err
		}
	}
	return nil
}

// adoptPinnedRequests moves every listed request whose signer is now a pinned
// peer into state.held (oldest first), so it is delivered once that peer
// verifies, like any requests item read from a pinned key. Run from New.
func adoptPinnedRequests(s *State, pinned map[string]PeerConf, now int64) {
	var ids []string
	for id, r := range s.Requests {
		if _, ok := pinned[r.From]; ok {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := s.Requests[ids[i]], s.Requests[ids[j]]
		return a.TS < b.TS || a.TS == b.TS && ids[i] < ids[j]
	})
	inHeld := map[string]bool{}
	for _, h := range s.Held {
		inHeld[h.ID] = true
	}
	for _, id := range ids {
		if _, done := s.Messages[id]; !done && !inHeld[id] {
			s.Held = append(s.Held, &HeldItem{ID: id, From: s.Requests[id].From, Queue: queueRequests, At: now})
		}
		delete(s.Requests, id)
	}
}

// heldFor is key's held entries, in order (what `peer verify` releases).
func (p *Plugin) heldFor(key string) []HeldItem {
	var out []HeldItem
	p.st.view(func(s *State) {
		for _, h := range s.Held {
			if h.From == key {
				out = append(out, *h)
			}
		}
	})
	return out
}
