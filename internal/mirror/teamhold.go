package mirror

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"
)

// ---------------------------------------------------------------------------
// A TEAM HOST'S CONNECTION IS PINNED (team sharing phase 2b.2;
// docs/design/team-sharing.md, "Closing the displacement path cheaply").
//
// A team host token is portable, and a person who leaves the team may keep a
// copy (the rotate mark's case: users/team_hosts.go). Under the personal rule
// — a newer connection for the same (owner, host) replaces the live one
// (ServeConn) — that copy could connect from any machine and silently take
// the host's place: every member's step-up'd sends and pastes would go to it.
//
// So for a TEAM-OWNED token (Token.Team) a second connection for a (team,
// host) that is LIVE is HELD, not swapped in:
//
//   - it is refused at once with ErrHeldForManager ("pending a manager's
//     confirmation"), before it is registered or listed anywhere, so it is
//     sent nothing and shows members nothing; the live host keeps serving;
//   - EACH held attempt is recorded by the address it came from (the TCP
//     peer, Service.peerAddr), with when it was first and last seen, and
//     /teams/{id}/hosts lists them all on a "replace the connected host?"
//     card;
//   - 🔴 A MANAGER'S CONFIRM NAMES ONE ATTEMPT, by its address
//     (ConfirmReplacement). It arms a one-shot replacement for
//     ReplaceConfirmWindow that admits ONLY a reconnect from that address:
//     the chosen machine's next attempt — muster-mirror retries at most a
//     minute apart — is taken, and the live connection is cancelled the way
//     a replacement is today. Another address presenting the same token in
//     that window is still held. There is no confirm that names no attempt,
//     so a retained token retrying fast cannot win the race a blanket
//     confirm would open (#685 round 0).
//
// ⚠ AN ADDRESS IS THE TCP PEER, NOT X-Forwarded-For (which any client
// writes). Behind a proxy chain every machine shares the proxy's address, so
// two machines there are ONE attempt on the card and the confirm cannot tell
// them apart: the card says so (HeldReplacement.Addr is the proxy's). The
// binding is exact only where hosts reach muster directly.
//
// A connection arriving while the host is OFFLINE (not live) is taken at
// once, as today, so a restart of the real host is unaffected: a clean
// restart closes the old connection first, and a half-open one is ended by
// the keepalive, after which the retry is taken. A PERSONAL host keeps the
// newer-replaces rule. This closes the silent take-over by a retained token;
// it does not close the offline window, which only rotation closes.
// ---------------------------------------------------------------------------

// ErrHeldForManager is the welcome's refusal of a held team host connection.
var ErrHeldForManager = errors.New("mirror: this team host is already connected; this connection is pending a manager's confirmation on the team's hosts page")

// ReplaceConfirmWindow is how long a manager's confirm waits for the chosen
// machine's next attempt. muster-mirror's backoff tops out at a minute (with
// jitter at most a minute), so three cover several attempts.
const ReplaceConfirmWindow = 3 * time.Minute

// heldRetention is how long a held attempt stays on the manager's card after
// its last attempt.
const heldRetention = 15 * time.Minute

// maxHeldAttempts bounds the distinct addresses recorded per (team, host): a
// token spraying from many addresses cannot grow the hub without bound. Past
// it, the oldest-seen attempt is forgotten.
const maxHeldAttempts = 16

// HeldReplacement is one held second connection, as the manager's card shows
// it: one machine, by the address it connected from.
type HeldReplacement struct {
	Host string
	// Addr is the attempt's TCP peer address (see the file comment's ⚠).
	Addr string
	// First and Last are the first and the latest held attempt from Addr.
	First, Last time.Time
	Attempts    int
	// Confirmed: a manager chose THIS attempt, and its next attempt is taken.
	Confirmed bool
}

// heldAttempt is one address's held attempts, under Hub.mu.
type heldAttempt struct {
	first, last time.Time
	attempts    int
}

// held is one (team, host)'s hold state, under Hub.mu.
type held struct {
	attempts map[string]*heldAttempt
	// confirmedAddr and confirmedUntil: a manager's confirm of ONE attempt,
	// good until then (zero: none).
	confirmedAddr  string
	confirmedUntil time.Time
}

// admitPinned decides a TEAM host connection's arrival for key from addr:
// true when it may proceed — the host is offline, or a manager's confirm of
// THIS address is armed (and is now spent) — and false when it is held (the
// attempt is recorded under addr).
func (h *Hub) admitPinned(key hostKey, addr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	hd := h.held[key]
	if hd != nil && hd.confirmedAddr == addr && now.Before(hd.confirmedUntil) {
		delete(h.held, key) // the confirm is one-shot, and the other attempts go with the host they wanted
		return true
	}
	if h.hosts[key] == nil {
		delete(h.held, key) // offline: taken at once, and nothing is left to confirm
		return true
	}
	if h.held == nil {
		h.held = map[hostKey]*held{}
	}
	if hd == nil {
		hd = &held{attempts: map[string]*heldAttempt{}}
		h.held[key] = hd
	}
	a := hd.attempts[addr]
	if a == nil || now.Sub(a.last) > heldRetention {
		if a == nil && len(hd.attempts) >= maxHeldAttempts {
			oldest := ""
			for k, v := range hd.attempts {
				if k != hd.confirmedAddr && (oldest == "" || v.last.Before(hd.attempts[oldest].last)) {
					oldest = k
				}
			}
			delete(hd.attempts, oldest)
		}
		a = &heldAttempt{first: now}
		hd.attempts[addr] = a
	}
	a.last = now
	a.attempts++
	return false
}

// HeldReplacements is owner's held second connections, one per (host,
// address), for the manager's card. A hold whose live host has gone is gone
// too, and an attempt not seen for heldRetention is forgotten.
func (h *Hub) HeldReplacements(owner int64) []HeldReplacement {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	var out []HeldReplacement
	for k, hd := range h.held {
		if k.owner != owner {
			continue
		}
		if h.hosts[k] == nil {
			delete(h.held, k)
			continue
		}
		confirmed := now.Before(hd.confirmedUntil)
		for addr, a := range hd.attempts {
			if now.Sub(a.last) > heldRetention && !(confirmed && addr == hd.confirmedAddr) {
				delete(hd.attempts, addr)
				continue
			}
			out = append(out, HeldReplacement{Host: k.host, Addr: addr, First: a.first, Last: a.last, Attempts: a.attempts,
				Confirmed: confirmed && addr == hd.confirmedAddr})
		}
		if len(hd.attempts) == 0 {
			delete(h.held, k)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].Addr < out[j].Addr
	})
	return out
}

// ConfirmReplacement arms the replacement of owner's live host by the held
// attempt from addr: that address's next attempt within ReplaceConfirmWindow
// is taken, and no other address's. False when no attempt from addr is held
// for host (or the live host has gone, so the next attempt is taken anyway).
// A later confirm of another address replaces this one.
func (h *Hub) ConfirmReplacement(owner int64, host, addr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := hostKey{owner, host}
	hd := h.held[k]
	if hd == nil || h.hosts[k] == nil || addr == "" || hd.attempts[addr] == nil {
		return false
	}
	hd.confirmedAddr, hd.confirmedUntil = addr, h.now().Add(ReplaceConfirmWindow)
	return true
}

// heldFrom reports whether owner's host has a held attempt from addr.
func (h *Hub) heldFrom(owner int64, host, addr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	hd := h.held[hostKey{owner, host}]
	return hd != nil && hd.attempts[addr] != nil
}

// HeldReplacements is the viewing principal's held team host connections
// (Hub.HeldReplacements). /teams/{id}/hosts calls it as the team acting
// principal (a manager, after a fresh role read).
func (s *Service) HeldReplacements(ctx context.Context) []HeldReplacement {
	o, err := owner(ctx)
	if err != nil {
		return nil
	}
	return s.hub.HeldReplacements(o)
}

// ConfirmReplacement is Hub.ConfirmReplacement for the viewing principal's
// host and the chosen attempt's address.
func (s *Service) ConfirmReplacement(ctx context.Context, host, addr string) bool {
	o, err := owner(ctx)
	if err != nil {
		return false
	}
	return s.hub.ConfirmReplacement(o, host, addr)
}

// HeldHere reports whether the machine making r — the viewing principal's
// host, by the token's (owner, host) — is a HELD second connection rather
// than the connected one: an attempt from r's address is held, and the live
// connection came from another address. `muster doctor`'s this-host check
// (GET /mirror/host) asks it, since a held machine shares the live one's
// token. ⚠ Two machines behind one address cannot be told apart (the file
// comment); then this answers false.
func (s *Service) HeldHere(ctx context.Context, host string, r *http.Request) bool {
	o, err := owner(ctx)
	if err != nil {
		return false
	}
	addr := s.peerAddr(r)
	if !s.hub.heldFrom(o, host, addr) {
		return false
	}
	s.connMu.Lock()
	c, live := s.connTok[hostKey{o, host}]
	s.connMu.Unlock()
	return live && c.addr != addr
}

// SetPeerAddr overrides how a request's peer address is read (tests: one
// process cannot dial from several addresses). nil restores clientAddr.
func (s *Service) SetPeerAddr(fn func(*http.Request) string) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	s.peerFn = fn
}

// peerAddr is r's peer address as the hold records it: clientAddr, unless a
// test set SetPeerAddr.
func (s *Service) peerAddr(r *http.Request) string {
	s.connMu.Lock()
	fn := s.peerFn
	s.connMu.Unlock()
	if fn != nil {
		return fn(r)
	}
	return clientAddr(r)
}
