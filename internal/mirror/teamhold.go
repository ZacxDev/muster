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
//   - it is refused at once with ErrHeldForManager ("pending a manager's
//     confirmation"), before it is registered or listed anywhere, so it is
//     sent nothing and shows members nothing; the live host keeps serving;
//   - EACH held attempt is recorded — by its ATTEMPT CODE when it sent one
//     (internal/attemptcode), else by the address it came from (the TCP peer,
//     Service.peerAddr) — with when it was first and last seen, and
//     /teams/{id}/hosts lists them all on a "replace the connected host?"
//     card;
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
//     (#685 round 1, Y1). The card offers no per-row button for a coded
//     attempt: the code is the only way to name it.
//   - The code is compared in constant time, only its hash is kept
//     (internal/attemptcode), and a team's wrong codes are rate-limited
//     (confirmFailLimit per confirmFailWindow) so it cannot be guessed.
//
// 🔴 THE FALLBACK: AN OLD muster-mirror SENDS NO CODE. Its attempt is recorded
// by address and confirmed by address (ConfirmReplacement), #685's rule: an
// address is the TCP peer, NOT X-Forwarded-For (which any client writes), and
// in the supported deployments it tells no machines apart — behind a proxy
// chain or a Kubernetes NodePort every machine shares one address (measured on
// a NodePort deployment). So a confirm for an attempt whose address is the
// LIVE connection's is REFUSED (ErrIndistinguishable), and the card labels a
// codeless attempt "cannot be told apart from other machines (old
// muster-mirror)". An address confirm admits only a CODELESS reconnect from
// that address, and a code confirm only a reconnect with that code: neither
// lets the other kind in.
//
// 🔴 THE HOLD GUARDS THE TUNNEL ONLY. A held machine, or any kept copy of the
// token, can still call /mirror/mcp and /mirror/ask: those doors bind a call
// to a session the LIVE host lists, and the session id is the caller's claim
// (internal/api mirror_mcp.go), so it can act as any session the live host
// lists, within teamHostTools. The rotate mark is the answer to a kept token.
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

// heldAnnounceEvery is the most often ONE attempt's holds are logged and
// nudge the team's views: a machine retrying in a tight loop is counted, not
// announced on every attempt.
const heldAnnounceEvery = 30 * time.Second

// confirmFailLimit wrong codes per confirmFailWindow, per team, and a team's
// further code confirms are refused (ErrConfirmRateLimited) until the oldest
// ages out. A code is 60 bits (internal/attemptcode): five guesses per ten minutes
// never find one.
const (
	confirmFailLimit  = 5
	confirmFailWindow = 10 * time.Minute
)

// ErrIndistinguishable: the chosen held attempt came from the LIVE
// connection's own address and sent no code, so a confirm could not tell the
// two machines apart (the file comment).
var ErrIndistinguishable = errors.New("mirror: this machine can't be told apart from the live host by address; stop the live host first, or replace the token")

// ErrNothingHeld: no codeless attempt from that address is held for that
// host (or the host is no longer connected, and its next connection is taken
// anyway).
var ErrNothingHeld = errors.New("mirror: no held attempt from that address")

// ErrNoSuchCode: no machine held for the team shows that code.
var ErrNoSuchCode = errors.New("mirror: no held machine shows that code")

// ErrConfirmRateLimited: the team typed too many wrong codes lately.
var ErrConfirmRateLimited = errors.New("mirror: too many wrong codes for this team; wait a few minutes")

// maxHeldAttempts bounds the distinct attempts recorded per (team, host): a
// token spraying from many addresses, or with many codes, cannot grow the hub
// without bound. Past it, the oldest-seen attempt is forgotten.
const maxHeldAttempts = 16

// HeldReplacement is one held second connection, as the manager's card shows
// it: one machine, by its code (never shown: the manager types it) or, for an
// old muster-mirror, by the address it connected from.
type HeldReplacement struct {
	Host string
	// Addr is the attempt's TCP peer address (see the file comment's ⚠).
	Addr string
	// Coded: the attempt sent an attempt code; it is confirmed only by typing
	// that code (ConfirmCode). False: an old muster-mirror, confirmed by Addr.
	Coded bool
	// First and Last are the first and the latest held attempt.
	First, Last time.Time
	Attempts    int
	// Confirmed: a manager chose THIS attempt, and its next attempt is taken.
	Confirmed bool
	// SameAsLive: a codeless attempt from the live connection's own address,
	// so it cannot be confirmed (ErrIndistinguishable). Set by
	// Service.HeldReplacements.
	SameAsLive bool
}

// heldAttempt is one machine's held attempts, under Hub.mu: one code, or one
// address's codeless attempts.
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

// admits reports whether a reconnect from addr presenting code (nil: none)
// is the machine a confirm of a chose: a coded attempt admits only its own
// code, compared in constant time; a codeless one only a codeless reconnect
// from its address.
func (a *heldAttempt) admits(addr string, code *[32]byte) bool {
	if a.coded {
		return code != nil && subtle.ConstantTimeCompare(code[:], a.code[:]) == 1
	}
	return code == nil && addr == a.addr
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
	// confirmed and confirmedUntil: a manager's confirm of ONE attempt (its
	// attemptKey), good until then (zero: none).
	confirmed      string
	confirmedUntil time.Time
}

// admitPinned decides a TEAM host connection's arrival for key from addr,
// presenting code (nil: an old muster-mirror sent none): true when it may
// proceed — the host is offline, or a manager's confirm of THIS attempt is
// armed (and is now spent) — and false when it is held (the attempt is
// recorded). announce reports, for a held attempt, whether to log it and
// nudge the views now: at most once per heldAnnounceEvery per attempt.
func (h *Hub) admitPinned(key hostKey, addr string, code *[32]byte) (admit, announce bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	hd := h.held[key]
	if hd != nil && now.Before(hd.confirmedUntil) {
		if c := hd.attempts[hd.confirmed]; c != nil && c.admits(addr, code) {
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
		a = &heldAttempt{addr: addr, first: now}
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

// ConfirmReplacement is the FALLBACK confirm, for an old muster-mirror that
// sent no code: it arms the replacement of owner's live host by the codeless
// held attempt from addr — that address's next CODELESS attempt within
// ReplaceConfirmWindow is taken, and nothing else. False when no codeless
// attempt from addr is held for host (or the live host has gone, so the next
// attempt is taken anyway). A coded attempt is never confirmed here: its code
// is the only way to name it. A later confirm replaces this one.
func (h *Hub) ConfirmReplacement(owner int64, host, addr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := hostKey{owner, host}
	hd := h.held[k]
	ak := attemptKey(addr, nil)
	if hd == nil || h.hosts[k] == nil || addr == "" || hd.attempts[ak] == nil {
		return false
	}
	hd.confirmed, hd.confirmedUntil = ak, h.now().Add(ReplaceConfirmWindow)
	return true
}

// ConfirmCode is a manager's confirm by the code the moved machine shows: it
// arms the replacement of owner's live host by the held attempt presenting
// that code — only a reconnect with that code, within ReplaceConfirmWindow,
// is taken. It answers the host, and the code's fingerprint for the audit row
// ("" for a malformed code). ErrNoSuchCode when no held machine of owner's
// shows the code; ErrConfirmRateLimited, comparing nothing, once owner has
// typed confirmFailLimit wrong codes in confirmFailWindow. Every wrong code
// counts against owner.
func (h *Hub) ConfirmCode(owner int64, code string) (host, fingerprint string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	if h.confirmLimited(owner, now) {
		return "", "", ErrConfirmRateLimited
	}
	want, wellFormed := attemptcode.Hash(code)
	if wellFormed {
		fingerprint = attemptcode.Fingerprint(want)
	}
	var hit *held
	hitKey, hitAttempt, matches := hostKey{}, "", 0
	for k, hd := range h.held {
		if k.owner != owner || h.hosts[k] == nil {
			continue
		}
		for ak, a := range hd.attempts {
			// 🔴 IN CONSTANT TIME, every coded attempt compared: how far a
			// wrong code got says nothing about a right one.
			eq := subtle.ConstantTimeCompare(want[:], a.code[:])
			if eq == 1 && a.coded && wellFormed && now.Sub(a.last) <= heldRetention {
				hit, hitKey, hitAttempt = hd, k, ak
				matches++
			}
		}
	}
	if matches != 1 {
		h.confirmFailed(owner, now)
		return "", fingerprint, ErrNoSuchCode
	}
	hit.confirmed, hit.confirmedUntil = hitAttempt, now.Add(ReplaceConfirmWindow)
	return hitKey.host, fingerprint, nil
}

// confirmLimited reports whether owner has typed confirmFailLimit wrong codes
// within confirmFailWindow of now, forgetting the older ones. Under h.mu.
func (h *Hub) confirmLimited(owner int64, now time.Time) bool {
	recent := h.confirmFails[owner][:0]
	for _, t := range h.confirmFails[owner] {
		if now.Sub(t) < confirmFailWindow {
			recent = append(recent, t)
		}
	}
	if len(recent) == 0 {
		delete(h.confirmFails, owner)
		return false
	}
	h.confirmFails[owner] = recent
	return len(recent) >= confirmFailLimit
}

// confirmFailed counts one wrong code against owner. Under h.mu.
func (h *Hub) confirmFailed(owner int64, now time.Time) {
	if h.confirmFails == nil {
		h.confirmFails = map[int64][]time.Time{}
	}
	h.confirmFails[owner] = append(h.confirmFails[owner], now)
}

// heldFrom reports whether owner's host has a held attempt from addr, coded
// or not.
func (h *Hub) heldFrom(owner int64, host, addr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	hd := h.held[hostKey{owner, host}]
	if hd == nil {
		return false
	}
	for _, a := range hd.attempts {
		if a.addr == addr {
			return true
		}
	}
	return false
}

// heldCode reports whether owner's host has a held attempt presenting code.
func (h *Hub) heldCode(owner int64, host string, code [32]byte) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	hd := h.held[hostKey{owner, host}]
	if hd == nil {
		return false
	}
	found := 0
	for _, a := range hd.attempts {
		found |= subtle.ConstantTimeCompare(code[:], a.code[:]) & boolInt(a.coded)
	}
	return found == 1
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// attemptCodeOf is r's attempt code's hash (attemptcode.Header), or nil when
// it sends none — or a malformed one, which is treated as none: such an
// attempt is told apart by address only, and the card says so.
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
	out := s.hub.HeldReplacements(o)
	for i := range out {
		if out[i].Coded {
			continue
		}
		if live, ok := s.liveAddr(o, out[i].Host); ok && live == out[i].Addr {
			out[i].SameAsLive = true
		}
	}
	return out
}

// ConfirmReplacement is the FALLBACK confirm (Hub.ConfirmReplacement) for the
// viewing principal's host and a codeless attempt's address: nil when armed,
// ErrIndistinguishable for an attempt from the live connection's own address
// (nothing is armed), ErrNothingHeld when no codeless attempt from addr is
// held.
func (s *Service) ConfirmReplacement(ctx context.Context, host, addr string) error {
	o, err := owner(ctx)
	if err != nil {
		return ErrNothingHeld
	}
	if live, ok := s.liveAddr(o, host); ok && live == addr {
		return ErrIndistinguishable
	}
	if !s.hub.ConfirmReplacement(o, host, addr) {
		return ErrNothingHeld
	}
	return nil
}

// ConfirmCode is Hub.ConfirmCode for the viewing principal (the team): the
// host the code's machine is held for, and the code's fingerprint, or
// ErrNoSuchCode / ErrConfirmRateLimited.
func (s *Service) ConfirmCode(ctx context.Context, code string) (host, fingerprint string, err error) {
	o, err := owner(ctx)
	if err != nil {
		return "", "", ErrNoSuchCode
	}
	return s.hub.ConfirmCode(o, code)
}

// liveAddr is the address owner's host's live connection came from.
func (s *Service) liveAddr(owner int64, host string) (string, bool) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	c, ok := s.connTok[hostKey{owner, host}]
	return c.addr, ok
}

// HeldHere answers, for the machine making r — the viewing principal's host,
// by the token's (owner, host) — `muster doctor`'s this-host question, which
// the token alone cannot (a held machine shares the live one's token):
//   - r presents this machine's attempt code (doctor reads it where
//     muster-mirror leaves it): held exactly when an attempt with that code is
//     held, whatever the addresses, and never unsure;
//   - otherwise, by address (an old doctor or muster-mirror): held when an
//     attempt from r's address is held and the live connection came from
//     ANOTHER address; unsure when the live connection came from the SAME
//     address (a proxy or NodePort: the file comment) — this muster cannot
//     tell whether this machine is the live one or the held one.
func (s *Service) HeldHere(ctx context.Context, host string, r *http.Request) (held, unsure bool) {
	o, err := owner(ctx)
	if err != nil {
		return false, false
	}
	if code := attemptCodeOf(r); code != nil {
		return s.hub.heldCode(o, host, *code), false
	}
	addr := s.peerAddr(r)
	if !s.hub.heldFrom(o, host, addr) {
		return false, false
	}
	live, ok := s.liveAddr(o, host)
	if !ok {
		return false, false
	}
	return live != addr, live == addr
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
