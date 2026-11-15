package mirror

import (
	"context"
	"errors"
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
//   - the hold is recorded (HeldReplacements) for /teams/{id}/hosts, which
//     shows the manager a "replace the connected host?" card;
//   - a manager's confirm (ConfirmReplacement) arms a one-shot replacement
//     for ReplaceConfirmWindow: the held host's next attempt — muster-mirror
//     retries at most a minute apart — is taken, and the live connection is
//     cancelled the way a replacement is today.
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

// ReplaceConfirmWindow is how long a manager's confirm waits for the held
// host's next attempt. muster-mirror's backoff tops out at a minute (with
// jitter at most a minute), so three cover several attempts.
const ReplaceConfirmWindow = 3 * time.Minute

// heldRetention is how long a held attempt stays on the manager's card after
// the last one.
const heldRetention = 15 * time.Minute

// HeldReplacement is one held second connection, as the manager's card shows
// it.
type HeldReplacement struct {
	Host string
	// First and Last are the first and the latest held attempt.
	First, Last time.Time
	Attempts    int
	// Confirmed: a manager confirmed it, and its next attempt is taken.
	Confirmed bool
}

// held is one key's hold state, under Hub.mu.
type held struct {
	first, last time.Time
	attempts    int
	// confirmedUntil: a manager's confirm, good until then (zero: none).
	confirmedUntil time.Time
}

// admitPinned decides a TEAM host connection's arrival for key: false when it
// is held (a live connection exists and no manager's confirm is armed; the
// attempt is recorded), true when it may proceed — the host is offline, or a
// confirm was armed (and is now spent).
func (h *Hub) admitPinned(key hostKey) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	hd := h.held[key]
	if hd != nil && !hd.confirmedUntil.IsZero() && now.Before(hd.confirmedUntil) {
		delete(h.held, key) // the confirm is one-shot
		return true
	}
	if h.hosts[key] == nil {
		delete(h.held, key) // offline: taken at once, and nothing is left to confirm
		return true
	}
	if h.held == nil {
		h.held = map[hostKey]*held{}
	}
	if hd == nil || now.Sub(hd.last) > heldRetention {
		hd = &held{first: now}
		h.held[key] = hd
	}
	hd.last = now
	hd.attempts++
	return false
}

// HeldReplacements is owner's held second connections, by host, for the
// manager's card. A hold whose live host has gone is gone too.
func (h *Hub) HeldReplacements(owner int64) []HeldReplacement {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	var out []HeldReplacement
	for k, hd := range h.held {
		if k.owner != owner {
			continue
		}
		if h.hosts[k] == nil || now.Sub(hd.last) > heldRetention && !now.Before(hd.confirmedUntil) {
			delete(h.held, k)
			continue
		}
		out = append(out, HeldReplacement{Host: k.host, First: hd.first, Last: hd.last, Attempts: hd.attempts,
			Confirmed: now.Before(hd.confirmedUntil)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// ConfirmReplacement arms the replacement of owner's live host by its held
// second connection: the held host's next attempt within ReplaceConfirmWindow
// is taken. False when nothing is held for host (or the live host has gone,
// so the next attempt is taken anyway).
func (h *Hub) ConfirmReplacement(owner int64, host string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := hostKey{owner, host}
	hd := h.held[k]
	if hd == nil || h.hosts[k] == nil {
		return false
	}
	hd.confirmedUntil = h.now().Add(ReplaceConfirmWindow)
	return true
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
// host.
func (s *Service) ConfirmReplacement(ctx context.Context, host string) bool {
	o, err := owner(ctx)
	if err != nil {
		return false
	}
	return s.hub.ConfirmReplacement(o, host)
}
