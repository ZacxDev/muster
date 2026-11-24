package api

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/gotrue"
	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/internal/users"
)

// ---------------------------------------------------------------------------
// A STEP-UP CARD DECIDED IN THE BROWSER, WITH A FRESH PASSKEY (operator
// decision; docs/sending-and-step-up.md, "Confirming in the browser").
//
// A confirmation card (host_stepup.go) used to be answered only from the
// owner's paired phone. Its card in muster — the ask page and the Asks feed —
// now also offers Confirm and Refuse, each of which runs a PASSKEY CEREMONY
// FOR THAT ONE DECISION:
//
//	POST /attention/asks/{id}/stepup/passkey/options
//	  → {available:false} for an account with no passkey (no challenge
//	    minted), else {available, challenge_id, options}: a GoTrue sign-in
//	    challenge with user verification REQUIRED, which muster records as
//	    minted for this person and this card (stepUpCardWeb.mint).
//	POST /attention/asks/{id}/stepup/passkey/verify
//	  {challenge_id, credential, answer: "Confirm"|"Refuse", return}
//	  → the card decided exactly as the phone's signed action decides it.
//
// 🔴 A RECENT SIGN-IN IS NOT ENOUGH. The proof is the assertion made for this
// request, never the session's freshness: a session that signed in a minute
// ago still has to run the ceremony.
//
// 🔴 THE PROOF IS BOUND TO ONE CARD, TWICE. (1) The challenge is spent by the
// verify that presents it (single use) and is refused unless it was minted
// for the SAME person and the SAME card the verify names — so an assertion
// made on card A's prompt cannot decide card B, and a replay finds nothing.
// (2) The decision then runs with a proof on its context that names that card
// (withStepUpPasskeyDecision), which hostDecisionForAnswer accepts for that
// card alone: a held call running under it (an attention_answer of another
// card) proves nothing for any other.
//
// 🔴 ONE CORE. The ceremony is verifySessionPasskey, the passkey step-up's own
// (the UV guard, GoTrue's single-use challenge, the session's-own-user
// binding, the sign-in limiter); the decision is answerAskFrom →
// hostDecisionForAnswer, the phone's own — owner-scoped, a closed card runs
// nothing, the card claimed before its call runs, so a phone and a browser
// racing decide it once and the other is told it was already decided. The
// session GoTrue minted for the ceremony is signed out at once: the ceremony
// decides the card and makes no session fresh.
//
// A TEAM'S QUESTION is held to the phone door's own rule (teamPhoneAskRefusal:
// an operator or a manager of the team, on a fresh read), before any
// challenge is minted and again before the decision.
// ---------------------------------------------------------------------------

// stepUpCardPasskeyPath is card id's passkey decision route prefix (the page's
// script posts to <prefix>/options and <prefix>/verify). Registered literally
// in registerAttentionRoutes.
func stepUpCardPasskeyPath(id int64) string { return askPagePath(id) + "/stepup/passkey" }

// stepUpCardChallengeTTL is how long a challenge minted for a card decision is
// honoured: GoTrue's own passkey challenge lifetime.
const stepUpCardChallengeTTL = 5 * time.Minute

// stepUpCardChallengesPerUser bounds the challenges kept for one person (the
// oldest goes first): a prompt opened on several cards, or reopened, never
// grows the set without bound.
const stepUpCardChallengesPerUser = 8

// stepUpPasskeyCacheFor is how long whether an account has a passkey is kept
// for drawing its cards (a live refresh of the Asks feed asks again often);
// adding or removing one forgets it at once (setPasskeyFlag).
const stepUpPasskeyCacheFor = 30 * time.Second

// stepUpCardChallenge is what one minted challenge was minted for.
type stepUpCardChallenge struct {
	user, card int64
	minted     time.Time
}

// stepUpCardWeb is the browser door's in-memory state: the challenges minted
// for card decisions, by GoTrue challenge id, and whether an account has a
// passkey, by GoTrue subject. In memory, as hostDecisions is: muster runs one
// replica, and a restart loses only challenges nobody can use any more.
type stepUpCardWeb struct {
	mu         sync.Mutex
	challenges map[string]stepUpCardChallenge
	has        map[string]passkeyHeld
}

// passkeyHeld is one cached answer of whether an account has a passkey.
type passkeyHeld struct {
	has bool
	at  time.Time
}

// mint records challenge ch as minted for user's decision of card.
func (c *stepUpCardWeb) mint(ch string, user, card int64, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.challenges == nil {
		c.challenges = map[string]stepUpCardChallenge{}
	}
	oldest, n := "", 0
	for k, v := range c.challenges {
		if now.Sub(v.minted) > stepUpCardChallengeTTL {
			delete(c.challenges, k)
			continue
		}
		if v.user == user {
			n++
			if oldest == "" || v.minted.Before(c.challenges[oldest].minted) {
				oldest = k
			}
		}
	}
	if n >= stepUpCardChallengesPerUser && oldest != "" {
		delete(c.challenges, oldest)
	}
	c.challenges[ch] = stepUpCardChallenge{user: user, card: card, minted: now}
}

// spend removes challenge ch and returns what it was minted for: single use,
// so whatever the verify then decides, the same challenge is never honoured
// again. false for one never minted here, spent already, or expired.
func (c *stepUpCardWeb) spend(ch string, now time.Time) (stepUpCardChallenge, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.challenges[ch]
	delete(c.challenges, ch)
	if !ok || now.Sub(v.minted) > stepUpCardChallengeTTL {
		return stepUpCardChallenge{}, false
	}
	return v, true
}

func (c *stepUpCardWeb) cachedHas(sub string, now time.Time) (bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h, ok := c.has[sub]
	if !ok || now.Sub(h.at) > stepUpPasskeyCacheFor {
		return false, false
	}
	return h.has, true
}

func (c *stepUpCardWeb) rememberHas(sub string, has bool, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.has == nil {
		c.has = map[string]passkeyHeld{}
	}
	c.has[sub] = passkeyHeld{has: has, at: now}
}

// forget drops what is cached about sub's passkeys (one was added or
// removed).
func (c *stepUpCardWeb) forget(sub string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.has, sub)
}

// stepUpPasskeyDecisionKey carries, on a decision's context, the card a passkey
// ceremony was made for and when it verified — set only by
// handleStepUpCardPasskeyVerify, after verifySessionPasskey verified the
// assertion and the challenge's binding to that card held.
type stepUpPasskeyDecisionKey struct{}

type stepUpPasskeyDecision struct {
	card int64
	at   time.Time
}

func withStepUpPasskeyDecision(ctx context.Context, card int64, at time.Time) context.Context {
	return context.WithValue(ctx, stepUpPasskeyDecisionKey{}, stepUpPasskeyDecision{card: card, at: at})
}

// stepUpPasskeyDecisionAt is when ctx's passkey decision verified, whatever
// card it was for (the session wake's sign-in time: wakeDecisionSignIn).
func stepUpPasskeyDecisionAt(ctx context.Context) (time.Time, bool) {
	d, ok := ctx.Value(stepUpPasskeyDecisionKey{}).(stepUpPasskeyDecision)
	return d.at, ok && !d.at.IsZero()
}

// The doors that may decide a step-up card (stepUpDoor).
const (
	stepUpDoorPhone   = "phone"
	stepUpDoorPasskey = "passkey"
)

// stepUpDoor is the door ctx decides card id through — the paired phone's
// signed action, or a passkey ceremony made for THIS card — and "" for any
// other (a browser without one, an MCP answer, a passkey proof for another
// card).
func stepUpDoor(ctx context.Context, p authz.Principal, id int64) string {
	if p.Via == authz.ViaDevice {
		return stepUpDoorPhone
	}
	if d, ok := ctx.Value(stepUpPasskeyDecisionKey{}).(stepUpPasskeyDecision); ok && d.card == id && !d.at.IsZero() {
		return stepUpDoorPasskey
	}
	return ""
}

// stepUpDoorWords is the door in the server log's words.
func stepUpDoorWords(door string) string {
	if door == stepUpDoorPasskey {
		return "in the browser with a passkey"
	}
	return "on the owner's phone"
}

// stepUpAuditStore is the users store's audit write.
type stepUpAuditStore interface {
	Audit(ctx context.Context, by users.Actor, action, object string, detail map[string]any) error
}

// stepUpDecisionAuditAction is the audit_log action of a decided step-up card.
const stepUpDecisionAuditAction = "stepup.decided"

// auditStepUpDecision writes the one audit_log row of a decided card: which
// card, the tool it held, the answer, and the door that decided it. A failed
// write is logged.
func (s *Server) auditStepUpDecision(ctx context.Context, p authz.Principal, id int64, tool, answer, door string) {
	a, ok := s.ext.Users.(stepUpAuditStore)
	if !ok {
		return
	}
	if err := a.Audit(ctx, users.Actor{UserID: p.UserID, Via: p.Via}, stepUpDecisionAuditAction, "ask:"+strconv.FormatInt(id, 10),
		map[string]any{"tool": tool, "answer": answer, "door": door}); err != nil {
		s.logger.Printf("step-up: audit card %d's decision: %v", id, err)
	}
}

// The browser door's refusals, as the page's script shows them.
const (
	StepUpCardOtherPrompt = "That passkey prompt was for another card, or has expired. Nothing was decided: try again."
	StepUpCardDecided     = "This card was already decided. Nothing more was run."
	StepUpCardClosed      = "This card is closed, so its request was dropped and nothing ran."
	StepUpCardLost        = "This card's request was lost when muster restarted: nothing ran, and the session must ask again."
	StepUpCardNotACard    = "That is not a confirmation card."
)

// stepUpCardRefusal is why ctx's principal may not decide card id in the
// browser now — "" when it may — with its status and the phone door's refusal
// code where one applies: not theirs (row-level security reads another
// owner's ask as missing), not a step-up card, a team's question they do not
// operate, settled, or its decision lost.
func (s *Server) stepUpCardRefusal(ctx context.Context, id int64) (string, int) {
	if s.ext.Attention == nil {
		return "No such question.", http.StatusNotFound
	}
	a, err := s.ext.Attention.Get(ctx, id)
	switch {
	case errors.Is(err, attention.ErrNotFound):
		return "No such question.", http.StatusNotFound
	case err != nil:
		s.logger.Printf("step-up: read card %d for a passkey decision: %v", id, err)
		return "That could not be checked. Try again.", http.StatusInternalServerError
	case !isStepUpCard(a):
		return StepUpCardNotACard, http.StatusBadRequest
	}
	if err := s.teamPhoneAskRefusal(ctx, id); err != nil {
		return TeamAskPhoneNotOperator, http.StatusForbidden
	}
	switch {
	case stepUpCardDecided(a):
		return StepUpCardDecided, http.StatusConflict
	case a.Status != attention.Open:
		return StepUpCardClosed, http.StatusConflict
	}
	d, ok := s.hostDecisions.peek(id)
	switch {
	case !ok && s.hostDecisions.isDeciding(id):
		return StepUpCardDecided, http.StatusConflict
	case !ok:
		return StepUpCardLost, http.StatusGone
	case d.owner != requestOwner(ctx):
		return "No such question.", http.StatusNotFound
	}
	return "", 0
}

// handleStepUpCardPasskeyOptions is POST /attention/asks/{id}/stepup/passkey/options.
func (s *Server) handleStepUpCardPasskeyOptions(w http.ResponseWriter, r *http.Request) {
	creds, refused := s.stepUpPasskeyRefusal(w, r)
	if refused {
		return
	}
	id, ok := askIDParam(r)
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "No such question."})
		return
	}
	if why, status := s.stepUpCardRefusal(r.Context(), id); why != "" {
		s.writeJSON(w, status, map[string]any{"error": why})
		return
	}
	ip := clientIP(r)
	list, err := s.gt.Client.Passkeys(r.Context(), ip, creds.access)
	if err != nil {
		s.writeGoTrueRefusal(w, "the passkey list (card decision)", err, PasskeysOffRefusal)
		return
	}
	s.stepUpCards.rememberHas(creds.claims.Subject, len(list) > 0, s.stepUpNow())
	if len(list) == 0 {
		// No passkey, no challenge (as the step-up confirm's options).
		s.writeJSON(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	o, err := s.gt.Client.PasskeySignInOptions(r.Context(), ip)
	if err != nil {
		s.writeGoTrueRefusal(w, "card decision options", err, PasskeysOffRefusal)
		return
	}
	opts, err := gotrue.RequireUserVerification(o.Options, false)
	if err != nil {
		s.writeGoTrueRefusal(w, "card decision options", err, PasskeysOffRefusal)
		return
	}
	s.stepUpCards.mint(o.ChallengeID, authz.MustFrom(r.Context()).UserID, id, s.stepUpNow())
	s.writeJSON(w, http.StatusOK, map[string]any{"available": true, "challenge_id": o.ChallengeID, "options": json.RawMessage(opts)})
}

// handleStepUpCardPasskeyVerify is POST /attention/asks/{id}/stepup/passkey/verify.
func (s *Server) handleStepUpCardPasskeyVerify(w http.ResponseWriter, r *http.Request) {
	creds, refused := s.stepUpPasskeyRefusal(w, r)
	if refused {
		return
	}
	id, ok := askIDParam(r)
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "No such question."})
		return
	}
	ctx := r.Context()
	me := authz.MustFrom(ctx).UserID
	// The card first: one that is not this person's to decide now is refused
	// before anything else is read.
	if why, status := s.stepUpCardRefusal(ctx, id); why != "" {
		s.writeJSON(w, status, map[string]any{"error": why})
		return
	}
	var body struct {
		ChallengeID string          `json:"challenge_id"`
		Credential  json.RawMessage `json:"credential"`
		Answer      string          `json:"answer"`
		Return      string          `json:"return"`
	}
	if !readPasskeyBody(w, r, &body) || body.ChallengeID == "" {
		s.stepUpRefuseUnspent(w, http.StatusBadRequest, PasskeyUnreadable)
		return
	}
	if body.Answer != hostDecisionConfirm && body.Answer != hostDecisionRefuse {
		s.stepUpRefuseUnspent(w, http.StatusBadRequest, "Answer "+hostDecisionConfirm+" or "+hostDecisionRefuse+".")
		return
	}
	// 🔴 THE CARD BINDING: the challenge is spent here, whatever follows, and
	// must have been minted for this person's decision of THIS card.
	minted, ok := s.stepUpCards.spend(body.ChallengeID, s.stepUpNow())
	if !ok || minted.user != me || minted.card != id {
		s.logger.Printf("step-up: refused a passkey decision of card %d by user %d: its challenge was not minted for that card", id, me)
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": StepUpCardOtherPrompt})
		return
	}
	s.verifySessionPasskey(w, r, creds, body.ChallengeID, body.Credential, "passkey decision", func(pk sessionPasskey) signinOutcome {
		// The ceremony decides this card; it makes no session fresh.
		pk.end()
		dctx := withStepUpPasskeyDecision(ctx, id, pk.at)
		a, err := s.answerAskFrom(dctx, id, body.Answer, attention.AnswerSource{})
		if err != nil {
			why, status := stepUpCardDecisionRefusal(err)
			if status == http.StatusInternalServerError {
				s.logger.Printf("step-up: a passkey decision of card %d: %v", id, err)
			}
			s.writeJSON(w, status, map[string]any{"error": why})
			// The passkey was this person's: the refusal is the card's, not a
			// failed sign-in.
			return outcomeSucceeded
		}
		next := askPagePath(id)
		if body.Return == attentionPath {
			next = settledLink(id)
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": a.ID, "status": a.Status, "answer": a.Answer, "next": next})
		return outcomeSucceeded
	})
}

// stepUpCardDecisionRefusal is a refused decision's words and status.
func stepUpCardDecisionRefusal(err error) (string, int) {
	switch {
	case errors.Is(err, errHostDecisionDecided):
		return StepUpCardDecided, http.StatusConflict
	case errors.Is(err, errHostDecisionClosed), errors.Is(err, attention.ErrNotOpen):
		return StepUpCardClosed, http.StatusConflict
	case errors.Is(err, errHostDecisionLost):
		return StepUpCardLost, http.StatusGone
	case errors.Is(err, errTeamAskNotOperator):
		return TeamAskPhoneNotOperator, http.StatusForbidden
	case errors.Is(err, attention.ErrNotFound):
		return "No such question.", http.StatusNotFound
	case errors.Is(err, attention.ErrInvalid):
		return attentionErrText(err, attention.Ask{}), http.StatusBadRequest
	}
	return "That could not be decided. Try again.", http.StatusInternalServerError
}

// --- drawing it ----------------------------------------------------------------

// stepUpWebOffer is what a step-up card's line offers in the browser.
type stepUpWebOffer int

const (
	// stepUpWebPhoneOnly: the phone line alone (no passkeys on this server,
	// the account's could not be read, or the card's decision is not one this
	// reader can make).
	stepUpWebPhoneOnly stepUpWebOffer = iota
	// stepUpWebNoPasskey: the phone line, and where a passkey is added.
	stepUpWebNoPasskey
	// stepUpWebPasskey: Confirm and Refuse, each behind a passkey ceremony.
	stepUpWebPasskey
)

// stepUpWebAccount is what ctx's reader's ACCOUNT allows on any card: a
// passkey decision when this server serves passkeys and the account has one,
// the hint when it has none, the phone line alone otherwise. One read per
// render, cached briefly (stepUpPasskeyCacheFor).
func (s *Server) stepUpWebAccount(ctx context.Context) stepUpWebOffer {
	creds, ok := gotrueCredsFrom(ctx)
	if !ok || !s.passkeysServed(ctx) {
		return stepUpWebPhoneOnly
	}
	now := s.stepUpNow()
	if has, ok := s.stepUpCards.cachedHas(creds.claims.Subject, now); ok {
		if has {
			return stepUpWebPasskey
		}
		return stepUpWebNoPasskey
	}
	// muster's own read while drawing a page, not a request of the client's:
	// named as the server, as its other reads of GoTrue are.
	list, err := s.gt.Client.Passkeys(ctx, "muster-server", creds.access)
	if err != nil {
		s.logger.Printf("step-up: read the passkeys of user %d for their cards: %v", requestOwner(ctx), err)
		return stepUpWebPhoneOnly
	}
	s.stepUpCards.rememberHas(creds.claims.Subject, len(list) > 0, now)
	if len(list) > 0 {
		return stepUpWebPasskey
	}
	return stepUpWebNoPasskey
}

// stepUpWebFor is what card id's line offers ctx's reader, given what their
// account allows: a passkey decision only for an open card whose decision is
// pending for THIS reader (a lost one is the phone's to refuse too).
func (s *Server) stepUpWebFor(ctx context.Context, account stepUpWebOffer, id int64) stepUpWebOffer {
	if account != stepUpWebPasskey {
		return account
	}
	if d, ok := s.hostDecisions.peek(id); !ok || d.owner != requestOwner(ctx) {
		return stepUpWebPhoneOnly
	}
	return stepUpWebPasskey
}

// The browser door's words on a card.
const (
	askStepUpPasskeyLine = "Confirm here with your passkey, or on your phone."
	askStepUpPasskeyHint = "To confirm here instead, add a passkey under "
)

// askStepUpWebHTML is a step-up card's line in place of its options and
// answer box, on the ask page and the feed's card alike (askStepUpPhoneHTML
// for the phone alone): with a passkey decision, the line and Confirm and
// Refuse, each running a passkey ceremony for that card (stepUpCardScript);
// with no passkey on the account, the phone line and where one is added.
// back is the page the decision returns to (attentionPath or the card's
// page); size is the line's text size class.
func askStepUpWebHTML(id int64, offer stepUpWebOffer, back, size string) string {
	switch offer {
	case stepUpWebNoPasskey:
		return askStepUpPhoneHTML(size) + `<p class="break-words text-xs text-fg2" data-ask-stepup-passkey-hint>` + askStepUpPasskeyHint +
			`<a href="` + accountPasskeysPath + `" class="font-semibold text-accent underline">Account → Passkeys</a>.</p>`
	case stepUpWebPasskey:
	default:
		return askStepUpPhoneHTML(size)
	}
	esc := html.EscapeString
	n := strconv.FormatInt(id, 10)
	return `<div class="flex min-w-0 flex-col gap-2" data-ask-stepup-web data-card="` + n + `" data-path="` + esc(stepUpCardPasskeyPath(id)) +
		`" data-return="` + esc(back) + `">` +
		`<p class="break-words ` + size + ` text-fg2" data-ask-stepup-phone data-phone-line="` + esc(askStepUpPhoneLine) + `">` + askStepUpPasskeyLine + `</p>` +
		`<div class="flex flex-wrap items-center gap-2" data-stepup-buttons>` +
		`<button type="button" class="` + ui.AsksPrimaryButtonBase + `" data-stepup-decide="` + hostDecisionConfirm + `">` + hostDecisionConfirm + `</button>` +
		`<button type="button" class="` + ui.AsksSecondaryButtonClass + `" data-stepup-decide="` + hostDecisionRefuse + `">` + hostDecisionRefuse + `</button>` +
		`</div><p role="alert" class="break-words text-sm text-st-error-fg" hidden data-stepup-error></p></div>`
}

// stepUpCardScript runs a step-up card's Confirm and Refuse: the card's
// options are fetched when the reader reaches for a button (pointerdown,
// focus), so the tap reaches navigator.credentials without a round trip in
// between (ui.PasskeyPrefetchJS says why); the tap runs the passkey prompt,
// posts the assertion with the answer, and on success loads the page the
// server names (the card, settled). A browser that cannot run WebAuthn gets
// the phone line back and no buttons. Delegated on the document, so a card a
// live refresh draws later is covered.
const stepUpCardScript = `<script>
(function () {
` + ui.PasskeyJS + ui.PasskeyPrefetchJS + `
  var pre = {}, busy = {};
  function boxOf(el) { return el && el.closest ? el.closest('[data-ask-stepup-web]') : null; }
  function say(b, msg) { var e = b.querySelector('[data-stepup-error]'); if (!e) return; e.textContent = msg || ''; e.hidden = !msg; }
  function phoneOnly(b) {
    var line = b.querySelector('[data-ask-stepup-phone]'), btns = b.querySelector('[data-stepup-buttons]');
    if (line) line.textContent = line.getAttribute('data-phone-line') || line.textContent;
    if (btns) btns.hidden = true;
  }
  function options(b, fresh) {
    var id = b.getAttribute('data-card'), p = pre[id];
    if (!fresh && p && mpkFresh(p)) return p;
    p = pre[id] = mpkPrefetch(b.getAttribute('data-path') + '/options');
    return p;
  }
  function reach(ev) {
    var b = boxOf(ev.target);
    if (!b || mpkMissing() || busy[b.getAttribute('data-card')]) return;
    options(b, false);
  }
  document.addEventListener('pointerdown', reach, true);
  document.addEventListener('focusin', reach, true);
  document.addEventListener('click', async function (ev) {
    var btn = ev.target && ev.target.closest ? ev.target.closest('[data-stepup-decide]') : null, b = boxOf(btn);
    if (!btn || !b) return;
    ev.preventDefault();
    var id = b.getAttribute('data-card');
    if (busy[id]) return;
    if (mpkMissing()) { phoneOnly(b); say(b, 'This browser cannot use a passkey here. Confirm on your phone.'); return; }
    busy[id] = true;
    b.setAttribute('aria-busy', 'true');
    say(b, '');
    function done(msg) { busy[id] = false; b.removeAttribute('aria-busy'); say(b, msg); }
    var p = options(b, false), o;
    // A challenge is used once: the next tap fetches another.
    delete pre[id];
    try { o = await p.promise; } catch (e) { done('muster could not be reached. Try again.'); return; }
    if (!o.ok) { done((o.data && o.data.error) || 'The passkey prompt could not be started. Try again.'); return; }
    if (!o.data.available || !o.data.challenge_id) { phoneOnly(b); done('This account has no passkey. Add one under Account, Passkeys, or confirm on your phone.'); return; }
    var cred;
    try { cred = await navigator.credentials.get({ publicKey: mpkRequestOptions(o.data.options) }); }
    catch (e) { done(mpkCancelled(e) ? 'The passkey was cancelled. Nothing was decided.' : 'The passkey could not be used (' + (e && e.name || 'error') + '). Nothing was decided.'); return; }
    var v;
    try {
      v = await mpkPost(b.getAttribute('data-path') + '/verify', { challenge_id: o.data.challenge_id, credential: mpkCredJSON(cred),
        answer: btn.getAttribute('data-stepup-decide'), 'return': b.getAttribute('data-return') });
    } catch (e) { done('muster could not be reached. Nothing was decided: try again.'); return; }
    if (v.ok) { window.location.assign((v.data && v.data.next) || window.location.href); return; }
    done((v.data && v.data.error) || 'That could not be decided. Try again.');
  });
})();
</script>
`
