package mirror

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/ZacxDev/muster/internal/attemptcode"
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
//   - it is refused at once, before it is registered or listed anywhere, so it
//     is sent nothing and shows members nothing; the live host keeps serving;
//   - EACH held attempt is recorded, by its ATTEMPT CODE's hash
//     (internal/attemptcode), with the address it came from (for the card
//     only) and when it was first and last seen, and /teams/{id}/hosts lists
//     them all on a "replace the connected host?" card;
//   - 🔴 A MANAGER'S CONFIRM NAMES ONE ATTEMPT, BY THE CODE ITS MACHINE SHOWS
//     (ConfirmCode): the manager types the code muster-mirror printed on the
//     machine they moved (its log, `muster doctor`). It arms a one-shot
//     replacement for ReplaceConfirmWindow that admits ONLY a reconnect
//     presenting that code: the chosen machine's next attempt — muster-mirror
//     retries at most a minute apart, with the same code, since the code is
//     per process — is taken, and the live connection is cancelled the way a
//     replacement is today. Any other connection in that window is still
//     held, whatever its address: a kept copy of the token, on another
//     machine, draws its own code, which the manager never types. This holds
//     behind a proxy or a NodePort, where every machine shares one address
//     (#685 round 1, Y1). The card offers no per-row button: the code is the
//     only way to name an attempt.
//   - 🔴 AN ATTEMPT WITH NO CODE (a muster-mirror older than the code, or a
//     malformed header) cannot be confirmed at all (ErrHeldNoCode): an address
//     tells no machines apart in the supported deployments (Y1), so there is
//     nothing to name it by. The card says so: upgrade muster-mirror on that
//     machine, or stop the live host (its next connection is then taken at
//     once, as offline).
//
// 🔴 THE HOLD GUARDS THE TUNNEL ONLY. A held machine, or any kept copy of the
// token, can still call /mirror/mcp and /mirror/ask: those doors bind a call
// to a session the LIVE host lists, and the session id is the caller's claim
// (internal/api mirror_mcp.go), so it can act as any session the live host
// lists, within teamHostTools. The rotate mark is the answer to a kept token.
//
// A connection arriving while the host is OFFLINE (not live) is taken at
// once, as today, code or no code, so a restart of the real host is
// unaffected: a clean restart closes the old connection first, and a
// half-open one is ended by the keepalive, after which the retry is taken. A
// PERSONAL host keeps the newer-replaces rule. This closes the silent
// take-over by a retained token; it does not close the offline window, which
// only rotation closes.
// ---------------------------------------------------------------------------

// ErrHeldForManager is the welcome's refusal of a held team host connection
// that presents an attempt code.
var ErrHeldForManager = errors.New("mirror: this team host is already connected; this connection is pending a manager's confirmation on the team's hosts page")

// ErrHeldNoCode is the welcome's refusal of a held team host connection that
// presents NO attempt code: no manager can confirm it.
var ErrHeldNoCode = errors.New("mirror: this team host is already connected, and this muster-mirror sends no attempt code, so no manager can confirm it: upgrade muster-mirror on this machine, or stop the live host")

// ReplaceConfirmWindow is how long a manager's confirm waits for the chosen
// machine's next attempt. muster-mirror's backoff tops out at a minute (with
// jitter at most a minute), so three cover several attempts.
const ReplaceConfirmWindow = 3 * time.Minute

// heldRetention is how long a held attempt stays on the manager's card after
// its last attempt.
const heldRetention = 15 * time.Minute

// heldAnnounceEvery is the most often ONE attempt's holds are logged and
// nudge the team's views: a machine retrying in a tight loop is counted, not
// announced on every attempt.
const heldAnnounceEvery = 30 * time.Second

// ErrNoSuchCode: no machine held for the team shows that code.
var ErrNoSuchCode = errors.New("mirror: no held machine shows that code")

// maxHeldAttempts bounds the distinct attempts recorded per (team, host): a
// token spraying codes (or, with no code, addresses) cannot grow the hub
// without bound. Past it, the oldest-seen attempt is forgotten.
const maxHeldAttempts = 16

// HeldReplacement is one held second connection, as the manager's card shows
// it: one machine, by when it was seen and where from — never by its code,
// which the manager types.
type HeldReplacement struct {
	Host string
	// Addr is the attempt's TCP peer address, shown for context only: it
	// names nothing (the file comment).
	Addr string
	// Coded: the attempt sent an attempt code, and is confirmed by typing it
	// (ConfirmCode). False: no code, so it cannot be confirmed.
	Coded bool
	// First and Last are the first and the latest held attempt.
	First, Last time.Time
	Attempts    int
	// Confirmed: a manager chose THIS attempt, and its next attempt is taken.
	Confirmed bool
}

// heldAttempt is one machine's held attempts, under Hub.mu: one code, or one
// address's codeless attempts (for the card only).
type heldAttempt struct {
	addr string
	// coded and code: the attempt code's HASH (attemptcode.Hash), never the
	// code.
	coded bool
	code  [32]byte
	first time.Time
	last  time.Time
	// attempts counts them; announced is when this attempt's hold was last
	// logged and nudged.
	attempts  int
	announced time.Time
}

// admits reports whether a reconnect presenting code (nil: none) is the
// machine a confirm of a chose: only a coded attempt is ever armed, and it
// admits only its own code.
func (a *heldAttempt) admits(code *[32]byte) bool {
	return a.coded && code != nil && subtle.ConstantTimeCompare(code[:], a.code[:]) == 1
}

// attemptKey is where an attempt is recorded in held.attempts: by the code's
// hash, or by address for a codeless one.
func attemptKey(addr string, code *[32]byte) string {
	if code != nil {
		return "code:" + hex.EncodeToString(code[:])
	}
	return "addr:" + addr
}

// held is one (team, host)'s hold state, under Hub.mu.
type held struct {
	attempts map[string]*heldAttempt
	// confirmed and confirmedUntil: a manager's confirm of ONE coded attempt
	// (its attemptKey), good until then (zero: none).
	confirmed      string
	confirmedUntil time.Time
}

// admitPinned decides a TEAM host connection's arrival for key from addr,
// presenting code (nil: none): true when it may proceed — the host is
// offline, or a manager's confirm of THIS code is armed (and is now spent) —
// and false when it is held (the attempt is recorded). announce reports, for
// a held attempt, whether to log it and nudge the views now: at most once per
// heldAnnounceEvery per attempt.
func (h *Hub) admitPinned(key hostKey, addr string, code *[32]byte) (admit, announce bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	hd := h.held[key]
	if hd != nil && now.Before(hd.confirmedUntil) {
		if c := hd.attempts[hd.confirmed]; c != nil && c.admits(code) {
			delete(h.held, key) // the confirm is one-shot, and the other attempts go with the host they wanted
			return true, false
		}
	}
	if h.hosts[key] == nil {
		delete(h.held, key) // offline: taken at once, and nothing is left to confirm
		return true, false
	}
	if h.held == nil {
		h.held = map[hostKey]*held{}
	}
	if hd == nil {
		hd = &held{attempts: map[string]*heldAttempt{}}
		h.held[key] = hd
	}
	ak := attemptKey(addr, code)
	a := hd.attempts[ak]
	if a == nil || now.Sub(a.last) > heldRetention {
		if a == nil && len(hd.attempts) >= maxHeldAttempts {
			oldest := ""
			for k, v := range hd.attempts {
				if k != hd.confirmed && (oldest == "" || v.last.Before(hd.attempts[oldest].last)) {
					oldest = k
				}
			}
			delete(hd.attempts, oldest)
		}
		a = &heldAttempt{first: now}
		if code != nil {
			a.coded, a.code = true, *code
		}
		hd.attempts[ak] = a
	}
	a.addr = addr
	a.last = now
	a.attempts++
	if a.announced.IsZero() || now.Sub(a.announced) >= heldAnnounceEvery {
		a.announced = now
		return false, true
	}
	return false, false
}

// HeldReplacements is owner's held second connections, one per attempt, for
// the manager's card. A hold whose live host has gone is gone too, and an
// attempt not seen for heldRetention is forgotten.
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
		for ak, a := range hd.attempts {
			if now.Sub(a.last) > heldRetention && !(confirmed && ak == hd.confirmed) {
				delete(hd.attempts, ak)
				continue
			}
			out = append(out, HeldReplacement{Host: k.host, Addr: a.addr, Coded: a.coded, First: a.first, Last: a.last, Attempts: a.attempts,
				Confirmed: confirmed && ak == hd.confirmed})
		}
		if len(hd.attempts) == 0 {
			delete(h.held, k)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		if out[i].Addr != out[j].Addr {
			return out[i].Addr < out[j].Addr
		}
		return out[i].First.Before(out[j].First)
	})
	return out
}

// ConfirmCode is a manager's confirm by the code the moved machine shows: it
// arms the replacement of owner's live host by the held attempt presenting
// that code — only a reconnect with that code, within ReplaceConfirmWindow,
// is taken. It answers the host, and the code's fingerprint for the audit row
// ("" for a malformed code). ErrNoSuchCode when no held machine of owner's
// shows the code — including one whose attempt was forgotten (heldRetention,
// or maxHeldAttempts' eviction).
func (h *Hub) ConfirmCode(owner int64, code string) (host, fingerprint string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	want, wellFormed := attemptcode.Hash(code)
	if !wellFormed {
		return "", "", ErrNoSuchCode
	}
	fingerprint = attemptcode.Fingerprint(want)
	for k, hd := range h.held {
		if k.owner != owner || h.hosts[k] == nil {
			continue
		}
		for ak, a := range hd.attempts {
			if a.coded && subtle.ConstantTimeCompare(want[:], a.code[:]) == 1 && now.Sub(a.last) <= heldRetention {
				hd.confirmed, hd.confirmedUntil = ak, now.Add(ReplaceConfirmWindow)
				return k.host, fingerprint, nil
			}
		}
	}
	return "", fingerprint, ErrNoSuchCode
}

// heldCode reports whether owner's host has a held attempt presenting code.
func (h *Hub) heldCode(owner int64, host string, code [32]byte) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	hd := h.held[hostKey{owner, host}]
	return hd != nil && hd.attempts[attemptKey("", &code)] != nil
}

// attemptCodeOf is r's attempt code's hash (attemptcode.Header), or nil when
// it sends none — or a malformed one, which is treated as none.
func attemptCodeOf(r *http.Request) *[32]byte {
	v := r.Header.Get(attemptcode.Header)
	if v == "" {
		return nil
	}
	h, ok := attemptcode.Hash(v)
	if !ok {
		return nil
	}
	return &h
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

// ConfirmCode is Hub.ConfirmCode for the viewing principal (the team): the
// host the code's machine is held for, and the code's fingerprint, or
// ErrNoSuchCode.
func (s *Service) ConfirmCode(ctx context.Context, code string) (host, fingerprint string, err error) {
	o, err := owner(ctx)
	if err != nil {
		return "", "", ErrNoSuchCode
	}
	return s.hub.ConfirmCode(o, code)
}

// HeldHere answers, for the machine making r — the viewing principal's host,
// by the token's (owner, host) — `muster doctor`'s this-host question, which
// the token alone cannot (a held machine shares the live one's token): held
// exactly when r presents an attempt code (doctor reads it where muster-mirror
// leaves it) and an attempt with that code is held. With no code there is
// nothing to tell this machine by, and it is not reported held.
func (s *Service) HeldHere(ctx context.Context, host string, r *http.Request) bool {
	o, err := owner(ctx)
	if err != nil {
		return false
	}
	code := attemptCodeOf(r)
	return code != nil && s.hub.heldCode(o, host, *code)
}
