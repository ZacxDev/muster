package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/gotrue"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/users"
)

// ---------------------------------------------------------------------------
// ADDING A PASSKEY NEEDS MORE THAN THE PASSWORD (operator decision;
// docs/auth.md, "Adding a passkey").
//
// A passkey can decide a step-up card in the browser (stepup_web_confirm.go),
// which before needed the paired phone. So if a password and a fresh sign-in
// were enough to ADD one, a password alone would reach card approval: sign
// in, enrol your own authenticator, approve. 🔴 THE INVARIANT: A PASSWORD
// ALONE NEVER REACHES CARD APPROVAL.
//
// So adding a passkey (POST /account/passkeys/options and …/verify, which
// still need a sign-in from the last stepup.CredentialMaxAge) also needs, for
// this browser session, an ENROLMENT TICKET granted by one of:
//
//   - an assertion of a passkey the account ALREADY has (…/proof/options,
//     …/proof/verify: the passkey step-up's own core, verifySessionPasskey);
//   - a confirmation on the paired phone: a step-up card "Add a passkey to
//     your account?" (…/proof/phone), answered Confirm with the phone's
//     signed action — the same proof as every other card. Only the phone
//     decides it (hostDecision.phoneOnly): never a passkey in the browser.
//
// The ticket is single use, lives enrolTicketTTL, and belongs to the GoTrue
// session that asked (a ticket granted for one browser adds nothing in
// another).
//
// 🔴 AN ACCOUNT WITH NEITHER (no passkey, no paired phone with a step-up key)
// has nothing to prove with, so its first passkey is added with the password
// as before — and is recorded as NOT able to decide a step-up card
// (passkeyVouchedKey false). It signs in and confirms "Confirm it's you" like
// any passkey; only the card decision refuses it. Every passkey records
// whether it may: one added with a ticket inherits the proof's standing (an
// assertion of a passkey that may not decide cards vouches for nothing; the
// phone always vouches), and a passkey whose record predates this rule (no
// passkeyVouchedKey) keeps working as it did, card decisions included
// (operator decision: existing passkeys keep working).
//
// 🔴 THE RECORD IS WRITTEN OR THE PASSKEY IS REMOVED. A passkey whose record
// could not be written would read as one that predates the rule — able to
// decide cards. So the add records it (its credential id and passkeyVouchedKey)
// before it answers, and a write that fails deletes the passkey just added and
// refuses.
//
// Every passkey added pushes the owner "A passkey was added to your account"
// — a security notice, sent whatever the notification switches say (an
// attacker holding the password could turn them off first).
// ---------------------------------------------------------------------------

// passkeyVouchedKey is, in a passkey's record (passkeyMetaKey), whether it
// may decide a step-up card in the browser: true when it was added with a
// proof beyond the password, false when with the password alone. Absent: the
// passkey was added before muster recorded it.
const passkeyVouchedKey = "vouched"

// enrolTicketTTL is how long an enrolment ticket is honoured: a registration
// is one GoTrue challenge (five minutes) away, with room for the person to
// reach for the button again.
const enrolTicketTTL = 10 * time.Minute

// enrolProofTarget is the target a passkey assertion for an enrolment is
// minted for in stepUpCardWeb (a card's target is its id, never 0): so a
// prompt made to add a passkey can decide no card, and a card's prompt adds
// no passkey.
const enrolProofTarget int64 = 0

// The enrolment refusals, as the passkeys page shows them.
const (
	PasskeyProofRequired   = "proof_required"
	PasskeyProofNeeded     = "To add a passkey, first confirm with a passkey you already have, or on your paired phone."
	PasskeyEnrolUnchecked  = "muster could not check how to confirm adding a passkey. Try again."
	PasskeyEnrolNoPhone    = "No phone with a confirmation key is paired to this account."
	PasskeyEnrolRecordFail = "The passkey could not be recorded, so it was not kept. Try again."
	PasskeyAddedTitle      = "A passkey was added to your account"
	PasskeyAddedBody       = "If this was not you, remove it under Account, Passkeys, and change your password."
)

// enrolTicket is one session's permission to add a passkey.
type enrolTicket struct {
	vouched bool
	proof   string // "passkey" or "phone"
	granted time.Time
}

// passkeyEnrolState is the enrolment tickets, by session (enrolKey), and the
// open phone card each session filed. In memory, as hostDecisions is: a
// restart loses a ticket, and the person proves again.
type passkeyEnrolState struct {
	mu      sync.Mutex
	tickets map[string]enrolTicket
	cards   map[string]int64
}

func (e *passkeyEnrolState) grant(key string, t enrolTicket) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.tickets == nil {
		e.tickets = map[string]enrolTicket{}
	}
	e.tickets[key] = t
	delete(e.cards, key)
}

// held is key's ticket, unspent and unexpired.
func (e *passkeyEnrolState) held(key string, now time.Time) (enrolTicket, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok := e.tickets[key]
	if !ok || now.Sub(t.granted) > enrolTicketTTL {
		delete(e.tickets, key)
		return enrolTicket{}, false
	}
	return t, true
}

// spend removes key's ticket (a passkey was added with it).
func (e *passkeyEnrolState) spend(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.tickets, key)
}

func (e *passkeyEnrolState) card(key string) (int64, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	id, ok := e.cards[key]
	return id, ok
}

func (e *passkeyEnrolState) setCard(key string, id int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cards == nil {
		e.cards = map[string]int64{}
	}
	e.cards[key] = id
}

// enrolKey is the browser session a ticket belongs to: the user and GoTrue's
// session id (the user alone for a token that names none).
func enrolKey(uid int64, c gotrueCreds) string {
	return strconv.FormatInt(uid, 10) + "/" + c.claims.SessionID
}

// enrolNeed is what adding a passkey needs of this session now.
type enrolNeed struct {
	// ticket: a ticket is held; add with its standing.
	ticket *enrolTicket
	// open: the account has no passkey and no step-up phone — the password
	// adds a passkey that may NOT decide cards.
	open bool
	// passkey, phone: the proofs this account can give (with no ticket).
	passkey, phone bool
}

// stepUpPhonePaired reports whether ctx's person has a paired phone with a
// step-up key (one that can confirm a card).
func (s *Server) stepUpPhonePaired(ctx context.Context) (bool, error) {
	if s.ext.Devices == nil {
		return false, nil
	}
	list, err := s.ext.Devices.List(ctx)
	if err != nil {
		return false, err
	}
	for _, d := range list {
		if d.HasStepUpKey {
			return true, nil
		}
	}
	return false, nil
}

// passkeyEnrolNeed is THE enrolment gate's one read: what adding a passkey
// needs of the session in creds. An error is a refusal (fail closed).
func (s *Server) passkeyEnrolNeed(ctx context.Context, creds gotrueCreds, ip string) (enrolNeed, error) {
	uid := authz.MustFrom(ctx).UserID
	if t, ok := s.passkeyEnrol.held(enrolKey(uid, creds), s.stepUpNow()); ok {
		return enrolNeed{ticket: &t}, nil
	}
	list, err := s.accountPasskeys(ctx, ip, creds)
	if err != nil {
		return enrolNeed{}, err
	}
	phone, err := s.stepUpPhonePaired(ctx)
	if err != nil {
		return enrolNeed{}, err
	}
	n := enrolNeed{passkey: len(list) > 0, phone: phone}
	n.open = !n.passkey && !n.phone
	return n, nil
}

// refuseEnrolNeed answers (true) when n gives no way to add a passkey now:
// 403 proof_required, with the proofs this account can give.
func (s *Server) refuseEnrolNeed(w http.ResponseWriter, n enrolNeed) bool {
	if n.ticket != nil || n.open {
		return false
	}
	s.writeJSON(w, http.StatusForbidden, map[string]any{"error": PasskeyProofNeeded, "code": PasskeyProofRequired,
		"proof": map[string]bool{"passkey": n.passkey, "phone": n.phone}})
	return true
}

// accountPasskeys is the passkeys of the account in creds, read through
// GoTrue's ADMIN API by its subject — never with the session's own access
// token, which may be a second from expiry when a page draws a card (GoTrue
// then refuses it, and a refusal here is a card with no Confirm or an
// enrolment refused). The gate's reads are muster's own, about the person the
// session already proved to be.
func (s *Server) accountPasskeys(ctx context.Context, ip string, creds gotrueCreds) ([]gotrue.Passkey, error) {
	return s.gt.Client.AdminPasskeys(ctx, ip, creds.claims.Subject)
}

// passkeyRecords is every recorded passkey record of sub (passkeyMetaKey),
// by passkey id, or the read's error.
func (s *Server) passkeyRecords(ctx context.Context, sub string) (map[string]map[string]any, error) {
	md, err := s.gt.Client.AdminAppMetadata(ctx, "muster-server", sub)
	if err != nil {
		return nil, err
	}
	raw, _ := md[passkeyMetaKey].(map[string]any)
	out := map[string]map[string]any{}
	for id, v := range raw {
		if m, ok := v.(map[string]any); ok {
			out[id] = m
		}
	}
	return out, nil
}

// passkeyMayDecideCards is the one rule for whether a passkey with record rec
// (found: it has one) may decide a step-up card: a record that says so, or one
// that predates the rule (no passkeyVouchedKey, or no record at all).
func passkeyMayDecideCards(rec map[string]any, found bool) bool {
	if !found {
		return true
	}
	v, ok := rec[passkeyVouchedKey].(bool)
	return !ok || v
}

// assertionMayDecideCards reports whether the passkey that made assertion
// credential may decide a step-up card: its record, found by its credential
// id. A record that cannot be read refuses (an error).
func (s *Server) assertionMayDecideCards(ctx context.Context, sub string, credential json.RawMessage) (bool, error) {
	credID, _, err := gotrue.AssertionIdentity(credential)
	if err != nil {
		return false, err
	}
	recs, err := s.passkeyRecords(ctx, sub)
	if err != nil {
		return false, err
	}
	for _, rec := range recs {
		if recordedCredID(rec) == credID {
			return passkeyMayDecideCards(rec, true), nil
		}
	}
	return passkeyMayDecideCards(nil, false), nil
}

// accountMayDecideCards reports whether any of list (sub's passkeys) may
// decide a step-up card.
func (s *Server) accountMayDecideCards(ctx context.Context, sub string, list []gotrue.Passkey) (bool, error) {
	if len(list) == 0 {
		return false, nil
	}
	recs, err := s.passkeyRecords(ctx, sub)
	if err != nil {
		return false, err
	}
	for _, p := range list {
		rec, found := recs[p.ID]
		if passkeyMayDecideCards(rec, found) {
			return true, nil
		}
	}
	return false, nil
}

// recordAddedPasskey writes passkey id's record — its flags, its credential
// id and passkeyVouchedKey — and reports whether it was written. 🔴 THE ADD
// FAILS WITHOUT IT: a passkey with no record reads as one that predates the
// rule.
func (s *Server) recordAddedPasskey(ctx context.Context, sub, id string, rec map[string]any) error {
	if recordedCredID(rec) == "" {
		return errors.New("no credential id")
	}
	if _, ok := rec[passkeyVouchedKey].(bool); !ok {
		return errors.New("no vouched flag")
	}
	s.stepUpCards.forget(sub)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	all, err := s.passkeyRecords(ctx, sub)
	if err != nil {
		return err
	}
	all[id] = rec
	return s.gt.Client.AdminSetAppMetadata(ctx, "muster-server", sub, passkeyMetaKey, all)
}

// notifyPasskeyAdded pushes owner the security notice of a passkey added.
// Not behind a notification switch: the person holding only the password
// could turn a switch off first.
func (s *Server) notifyPasskeyAdded(ctx context.Context, owner int64) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	rep := s.sendPush(ctx, owner, push.Payload{Type: "notify", Title: PasskeyAddedTitle, Body: PasskeyAddedBody,
		Tag: "muster-passkey-added", Data: map[string]string{"url": accountPasskeysPath}})
	s.logger.Printf("passkeys: told user %d a passkey was added (%d of %d device(s))", owner, rep.Delivered, rep.Devices)
}

// --- the proofs -------------------------------------------------------------

// handlePasskeyProofOptions is POST /account/passkeys/proof/options: a sign-in
// challenge (user verification required) for a passkey the account already
// has, minted for this person's enrolment (enrolProofTarget).
func (s *Server) handlePasskeyProofOptions(w http.ResponseWriter, r *http.Request) {
	creds, refused := s.stepUpPasskeyRefusal(w, r)
	if refused {
		return
	}
	ip := clientIP(r)
	list, err := s.accountPasskeys(r.Context(), ip, creds)
	if err != nil {
		s.writeGoTrueRefusal(w, "the passkey list (enrolment proof)", err, PasskeysOffRefusal)
		return
	}
	if len(list) == 0 {
		s.writeJSON(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	o, err := s.gt.Client.PasskeySignInOptions(r.Context(), ip)
	if err != nil {
		s.writeGoTrueRefusal(w, "enrolment proof options", err, PasskeysOffRefusal)
		return
	}
	opts, err := gotrue.RequireUserVerification(o.Options, false)
	if err != nil {
		s.writeGoTrueRefusal(w, "enrolment proof options", err, PasskeysOffRefusal)
		return
	}
	s.stepUpCards.mint(o.ChallengeID, authz.MustFrom(r.Context()).UserID, enrolProofTarget, s.stepUpNow())
	s.writeJSON(w, http.StatusOK, map[string]any{"available": true, "challenge_id": o.ChallengeID, "options": json.RawMessage(opts)})
}

// handlePasskeyProofVerify is POST /account/passkeys/proof/verify: an
// assertion of one of the account's passkeys, on a challenge minted for this
// person's enrolment, grants this session a ticket — able to decide cards
// only when the asserting passkey may (assertionMayDecideCards).
func (s *Server) handlePasskeyProofVerify(w http.ResponseWriter, r *http.Request) {
	creds, refused := s.stepUpPasskeyRefusal(w, r)
	if refused {
		return
	}
	var body struct {
		ChallengeID string          `json:"challenge_id"`
		Credential  json.RawMessage `json:"credential"`
	}
	if !readPasskeyBody(w, r, &body) || body.ChallengeID == "" {
		s.stepUpRefuseUnspent(w, http.StatusBadRequest, PasskeyUnreadable)
		return
	}
	ctx := r.Context()
	me := authz.MustFrom(ctx).UserID
	minted, ok := s.stepUpCards.spend(body.ChallengeID, s.stepUpNow())
	if !ok || minted.user != me || minted.card != enrolProofTarget {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": StepUpCardOtherPrompt})
		return
	}
	s.verifySessionPasskey(w, r, creds, body.ChallengeID, body.Credential, "passkey enrolment proof", false, func(pk sessionPasskey) signinOutcome {
		pk.end()
		vouched, err := s.assertionMayDecideCards(ctx, creds.claims.Subject, body.Credential)
		if err != nil {
			s.logger.Printf("passkeys: read user %d's passkey records for an enrolment proof: %v", me, err)
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": PasskeyEnrolUnchecked})
			return outcomeSucceeded
		}
		s.passkeyEnrol.grant(enrolKey(me, creds), enrolTicket{vouched: vouched, proof: "passkey", granted: s.stepUpNow()})
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return outcomeSucceeded
	})
}

// handlePasskeyProofPhone is POST /account/passkeys/proof/phone: files (or
// finds this session's open) step-up card "Add a passkey to your account?",
// which only the paired phone answers; its Confirm grants this session a
// ticket that may decide cards.
func (s *Server) handlePasskeyProofPhone(w http.ResponseWriter, r *http.Request) {
	creds, ok := s.passkeyAccountCreds(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	p := authz.MustFrom(ctx)
	phone, err := s.stepUpPhonePaired(ctx)
	if err != nil {
		s.logger.Printf("passkeys: read user %d's devices: %v", p.UserID, err)
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": PasskeyEnrolUnchecked})
		return
	}
	if !phone || s.ext.Attention == nil {
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": PasskeyEnrolNoPhone})
		return
	}
	key := enrolKey(p.UserID, creds)
	if id, ok := s.passkeyEnrol.card(key); ok {
		if _, pending := s.hostDecisions.peek(id); pending {
			s.writeJSON(w, http.StatusOK, map[string]any{"card": id})
			return
		}
	}
	card := &attention.StepUpCard{Tool: passkeyAddCardTool, Title: "Add a passkey to your account?",
		Why: "A browser signed in to your account asks to add a passkey. Confirm only if that is you, now.",
		Details: "Requested from " + clientIP(r) + ", " + firstNonBlank(r.UserAgent(), "an unnamed browser") +
			". Confirming lets that browser session add one passkey in the next ten minutes; refusing adds nothing."}
	ask, err := s.ext.Attention.Create(s.attentionPrincipal(ctx, p.UserID), attention.New{
		Asker:   attention.Asker{System: true, Label: hostDecisionLabel},
		StepUp:  card,
		Options: []string{hostDecisionConfirm, hostDecisionRefuse},
	})
	if err != nil {
		s.logger.Printf("passkeys: file user %d's enrolment card: %v", p.UserID, err)
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "The confirmation could not be sent to your phone. Try again."})
		return
	}
	s.hostDecisions.put(ask.ID, hostDecision{owner: p.UserID, tool: passkeyAddCardTool, question: ask.Question, phoneOnly: true,
		confirm: func(context.Context) string {
			s.passkeyEnrol.grant(key, enrolTicket{vouched: true, proof: "phone", granted: s.stepUpNow()})
			return "granted"
		}})
	s.passkeyEnrol.setCard(key, ask.ID)
	s.askFiled(ask)
	s.logger.Printf("passkeys: user %d asked to add a passkey; card %d waits for the paired phone", p.UserID, ask.ID)
	s.writeJSON(w, http.StatusOK, map[string]any{"card": ask.ID})
}

// passkeyAddCardTool is the enrolment card's tool name: muster's own, never a
// mirror door tool (hostStepUpTools does not list it, so it wakes no session).
const passkeyAddCardTool = "passkey_add"

// auditPasskeyAdded writes the add's audit row: which passkey, how it was
// allowed, and whether it may decide cards.
func (s *Server) auditPasskeyAdded(ctx context.Context, uid int64, id, ip, proof string, vouched bool) {
	if s.gt.Accounts == nil {
		return
	}
	if err := s.gt.Accounts.Audit(ctx, users.Actor{UserID: uid, Via: authz.ViaSession}, "user.passkey_added",
		users.UserObject(uid), map[string]any{"passkey_id": id, "ip": ip, "proof": proof, "decides_cards": vouched}); err != nil {
		s.logger.Printf("passkeys: audit user %d's added passkey: %v", uid, err)
	}
}
