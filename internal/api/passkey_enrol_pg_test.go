package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/devices"
	"github.com/ZacxDev/muster/internal/gotrue"
	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
)

// ADDING A PASSKEY NEEDS MORE THAN THE PASSWORD (passkey_enrol.go), over
// Postgres, the GoTrue session tier (the fake GoTrue) and the browser
// listener. 🔴 THE INVARIANT UNDER TEST: A PASSWORD ALONE NEVER REACHES CARD
// APPROVAL — not by adding a passkey where the account can prove more, and
// not by deciding a card with a passkey the password alone added.
//
// Every expectation is a literal (the routes, the codes, the record's key),
// so the file runs against the commit before the gate too: there a password
// adds any passkey, and every gated test below fails. That is the red half of
// the PR's matrix.

const (
	enrolOptions      = "/account/passkeys/options"
	enrolVerify       = "/account/passkeys/verify"
	enrolProofOptions = "/account/passkeys/proof/options"
	enrolProofVerify  = "/account/passkeys/proof/verify"
	enrolProofPhone   = "/account/passkeys/proof/phone"
	enrolPushEP       = "https://" + devNtfyHost + "/upEnrol1?up=1"
	unvouchedRefusal  = "That passkey was added with your password alone, so it cannot approve a card. Confirm on your phone, or add a passkey confirmed on your phone."
)

// enrolRig is one owner signed in (at, rt) with passkeys on, and a phone
// registered for push at enrolPushEP: keyed (a step-up phone: phone true) or
// read-only (no step-up key: phone false).
type enrolRig struct {
	*actionRig
	at, rt string
}

func newEnrolRig(t *testing.T, phone bool) *enrolRig {
	t.Helper()
	a := &actionRig{}
	a.deviceRig = newDeviceRig(t, func(ext *Extensions, pool *pgxpool.Pool) {
		a.attn = attention.NewPG(pool)
		ext.Attention = a.attn
	})
	t.Cleanup(func() {
		_, _ = a.pool.Exec(authz.System(context.Background()), `DELETE FROM attention_asks WHERE owner_id = $1`, a.uid)
	})
	a.fake.EnablePasskeys()
	var tok string
	if phone {
		a.key, tok, a.dev = a.pairKeyedAs(t, "keyed phone", devices.KeyBiometric)
		a.tok = tok
	} else {
		at, rt := a.login()
		tok, a.dev = a.pair(at, rt, "read-only phone")
	}
	reg := `{"endpoint":"` + enrolPushEP + `","p256dh":"BPk","auth":"au"}`
	if w := a.call(a.edge, "PUT", devicePushPath, tok, reg); w.Code != http.StatusOK {
		t.Fatalf("push register: %d %s", w.Code, w.Body.String())
	}
	at, rt := a.login()
	return &enrolRig{actionRig: a, at: at, rt: rt}
}

func (r *enrolRig) options() *httptest.ResponseRecorder {
	return postJSON(r.edge, enrolOptions, map[string]any{}, withCookies(r.at, r.rt))
}

// add is the whole add of a passkey for credID: options, then verify. The
// options' answer when it is not 200, else the verify's.
func (r *enrolRig) add(t *testing.T, credID string) *httptest.ResponseRecorder {
	t.Helper()
	o := r.options()
	if o.Code != http.StatusOK {
		return o
	}
	ch, _ := jsonBody(t, o)["challenge_id"].(string)
	return postJSON(r.edge, enrolVerify, map[string]any{"challenge_id": ch, "credential": gotruetest.Attestation(credID, gotruetest.FlagUPUV)},
		withCookies(r.at, r.rt))
}

// record is the passkey record muster wrote for credID (its credential id as
// recorded), nil for none.
func (r *enrolRig) record(t *testing.T, credID string) map[string]any {
	t.Helper()
	recs, err := r.s.passkeyRecords(context.Background(), r.subject())
	if err != nil {
		t.Fatal(err)
	}
	want, _, err := gotrue.AssertionIdentity(gotruetest.Assertion(credID, gotruetest.FlagUPUV))
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		if recordedCredID(rec) == want {
			return rec
		}
	}
	return nil
}

// recordPasskey writes the record muster's add would have written for the
// fake passkey credID of sub: its credential id and vouched. (A passkey the
// fake's AddPasskey made has none: one GoTrue made outside muster's add.)
func recordPasskey(t *testing.T, s *Server, fake *gotruetest.Fake, sub, credID string, vouched bool) {
	t.Helper()
	for _, p := range fake.Passkeys(sub) {
		if p.CredentialID == credID {
			s.setPasskeyFlag(context.Background(), sub, p.ID, map[string]any{passkeyCredIDKey: gotrue.CredentialID([]byte(credID)), "vouched": vouched})
			return
		}
	}
	t.Fatalf("no fake passkey %q", credID)
}

// grantEnrolTicket grants the session whose access token is at an enrolment
// ticket, as a proof would (passkey_enrol.go), for tests of something else.
func grantEnrolTicket(t *testing.T, s *Server, fake *gotruetest.Fake, uid int64, at string) {
	t.Helper()
	c, err := fake.Verifier().Verify(at)
	if err != nil {
		t.Fatal(err)
	}
	s.passkeyEnrol.grant(enrolKey(uid, gotrueCreds{access: at, claims: c}), enrolTicket{vouched: true, proof: "phone", granted: s.stepUpNow()})
}

// enablePasskeys turns passkeys on in fake and drops srv's cached GoTrue
// settings: a rig that paired a phone first (the pairing gate reads them)
// would otherwise keep "off" for passkeySettingsTTL.
func enablePasskeys(srv *Server, fake *gotruetest.Fake) {
	fake.EnablePasskeys()
	srv.gt.pk.mu.Lock()
	srv.gt.pk.at = time.Time{}
	srv.gt.pk.mu.Unlock()
}

func (r *enrolRig) passkeys() int { return len(r.fake.Passkeys(r.subject())) }

// pushedAdded counts the "A passkey was added" pushes that reached the phone.
func (r *enrolRig) pushedAdded() int {
	r.pusher.mu.Lock()
	defer r.pusher.mu.Unlock()
	n := 0
	for i, e := range r.pusher.sent {
		if e == enrolPushEP && strings.Contains(r.pusher.bodies[i], "A passkey was added to your account") {
			n++
		}
	}
	return n
}

func proofOf(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	b := jsonBody(t, w)
	if w.Code != http.StatusForbidden || b["code"] != "proof_required" {
		t.Fatalf("want 403 proof_required, got %d %s", w.Code, w.Body)
	}
	p, _ := b["proof"].(map[string]any)
	return p
}

// TestAPasswordAloneCannotAddAPasskeyWhereThePhoneCanConfirm: 🔴 the
// enrolment bypass. An account with a step-up phone, signed in a moment ago
// with its password: the options answer proof_required (phone), the add
// itself is refused, and no passkey is added.
func TestAPasswordAloneCannotAddAPasskeyWhereThePhoneCanConfirm(t *testing.T) {
	r := newEnrolRig(t, true)
	if p := proofOf(t, r.options()); p["phone"] != true || p["passkey"] != false {
		t.Fatalf("the proofs offered: %v, want the phone only", p)
	}
	// The verify too, with a registration challenge minted straight at GoTrue.
	ch := r.fake.NewChallenge(r.subject())
	w := postJSON(r.edge, enrolVerify, map[string]any{"challenge_id": ch, "credential": gotruetest.Attestation("attacker-cred", gotruetest.FlagUPUV)},
		withCookies(r.at, r.rt))
	proofOf(t, w)
	if n := r.passkeys(); n != 0 {
		t.Fatalf("a password alone added %d passkey(s)", n)
	}
}

// TestAPasswordAloneCannotAddASecondPasskey: an account that has a passkey
// (and no phone) — the password does not add another.
func TestAPasswordAloneCannotAddASecondPasskey(t *testing.T) {
	r := newEnrolRig(t, false)
	r.fake.AddPasskey(r.subject(), "owner-cred")
	if p := proofOf(t, r.add(t, "attacker-cred")); p["passkey"] != true || p["phone"] != false {
		t.Fatalf("the proofs offered: %v, want a passkey only", p)
	}
	if n := r.passkeys(); n != 1 {
		t.Fatalf("passkeys after a password-only add: %d, want 1", n)
	}
}

// TestAnExistingPasskeyLetsYouAddAnother: the proof is an assertion of a
// passkey the account has, on a challenge minted for the enrolment. The add
// then succeeds once (the ticket is spent), is recorded as able to decide
// cards, and the owner's phone is told.
func TestAnExistingPasskeyLetsYouAddAnother(t *testing.T) {
	r := newEnrolRig(t, false)
	r.fake.AddPasskey(r.subject(), "owner-cred")
	recordPasskey(t, r.s, r.fake, r.subject(), "owner-cred", true)
	o := postJSON(r.edge, enrolProofOptions, map[string]any{}, withCookies(r.at, r.rt))
	ch, _ := jsonBody(t, o)["challenge_id"].(string)
	if o.Code != http.StatusOK || ch == "" {
		t.Fatalf("proof options: %d %s", o.Code, o.Body)
	}
	// A card's prompt is not an enrolment's: a challenge minted here cannot
	// be presented as one minted for a card, and vice versa (stepUpCardWeb).
	if w := postJSON(r.edge, enrolProofVerify, map[string]any{"challenge_id": ch, "credential": gotruetest.Assertion("owner-cred", gotruetest.FlagUPUV)},
		withCookies(r.at, r.rt)); w.Code != http.StatusOK {
		t.Fatalf("the proof: %d %s", w.Code, w.Body)
	}
	if w := r.add(t, "second-cred"); w.Code != http.StatusOK {
		t.Fatalf("the add after the proof: %d %s", w.Code, w.Body)
	}
	if rec := r.record(t, "second-cred"); rec == nil || rec["vouched"] != true {
		t.Fatalf("the added passkey's record: %v, want vouched true", rec)
	}
	if n := r.pushedAdded(); n != 1 {
		t.Fatalf("the owner was pushed %d \"passkey added\" notice(s), want 1", n)
	}
	// The ticket was spent: a third needs a proof again.
	proofOf(t, r.add(t, "third-cred"))
	if n := r.passkeys(); n != 2 {
		t.Fatalf("passkeys: %d, want 2", n)
	}
}

// TestAPhoneConfirmedEnrolmentAddsAPasskeyThatDecidesCards: the phone proof is
// a step-up card the browser cannot decide (no passkey door, not even a
// passkey's), answered Confirm by the phone's signed action. Then this
// session adds one passkey, recorded as able to decide cards, and the owner
// is pushed the notice.
func TestAPhoneConfirmedEnrolmentAddsAPasskeyThatDecidesCards(t *testing.T) {
	r := newEnrolRig(t, true)
	w := postJSON(r.edge, enrolProofPhone, map[string]any{}, withCookies(r.at, r.rt))
	var out struct {
		Card int64 `json:"card"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Card == 0 {
		t.Fatalf("the phone proof: %d %s", w.Code, w.Body)
	}
	card := r.status(t, r.uid, out.Card)
	if card.StepUp == nil || card.StepUp.Title != "Add a passkey to your account?" || card.Status != attention.Open {
		t.Fatalf("the enrolment card: %+v", card)
	}
	// The browser cannot answer it: not the plain answer, not a passkey.
	if _, err := r.s.answerAskFrom(withStepUpPasskeyDecision(asUser(r.uid), out.Card, r.fake.Now()), out.Card, hostDecisionConfirm, attention.AnswerSource{}); err == nil {
		t.Fatal("a passkey proof answered the enrolment card")
	}
	proofOf(t, r.options())
	// The phone: its signed Confirm of the card.
	confirm := hostDecisionConfirm
	nonce, msg := r.challenge(t, r.tok, actionAskAnswer, out.Card, &confirm)
	if code, body := r.act(t, r.tok, "answer", out.Card, map[string]string{"nonce": nonce, "signature": sign(t, r.key, msg), "answer": confirm}); code != http.StatusOK {
		t.Fatalf("the phone's Confirm: %d %s", code, body)
	}
	if w := r.add(t, "phone-vouched-cred"); w.Code != http.StatusOK {
		t.Fatalf("the add after the phone's Confirm: %d %s", w.Code, w.Body)
	}
	if rec := r.record(t, "phone-vouched-cred"); rec == nil || rec["vouched"] != true {
		t.Fatalf("the added passkey's record: %v, want vouched true", rec)
	}
	if n := r.pushedAdded(); n != 1 {
		t.Fatalf("\"passkey added\" pushes: %d, want 1", n)
	}
}

// TestAFirstPasskeyAddedWithThePasswordCannotDecideCards: an account with
// neither a passkey nor a step-up phone has nothing to prove with, so the
// password adds its first passkey — recorded as NOT able to decide a card —
// and the owner is pushed the notice.
func TestAFirstPasskeyAddedWithThePasswordCannotDecideCards(t *testing.T) {
	r := newEnrolRig(t, false)
	if w := r.add(t, "first-cred"); w.Code != http.StatusOK {
		t.Fatalf("the first passkey: %d %s", w.Code, w.Body)
	}
	if rec := r.record(t, "first-cred"); rec == nil || rec["vouched"] != false {
		t.Fatalf("a password-only passkey's record: %v, want vouched false", rec)
	}
	if n := r.pushedAdded(); n != 1 {
		t.Fatalf("\"passkey added\" pushes: %d, want 1", n)
	}
	// A passkey proven with THAT one vouches for nothing either: the second
	// inherits its standing.
	o := postJSON(r.edge, enrolProofOptions, map[string]any{}, withCookies(r.at, r.rt))
	ch, _ := jsonBody(t, o)["challenge_id"].(string)
	if w := postJSON(r.edge, enrolProofVerify, map[string]any{"challenge_id": ch, "credential": gotruetest.Assertion("first-cred", gotruetest.FlagUPUV)},
		withCookies(r.at, r.rt)); w.Code != http.StatusOK {
		t.Fatalf("the proof with the password-only passkey: %d %s", w.Code, w.Body)
	}
	if w := r.add(t, "second-cred"); w.Code != http.StatusOK {
		t.Fatalf("the second passkey: %d %s", w.Code, w.Body)
	}
	if rec := r.record(t, "second-cred"); rec == nil || rec["vouched"] != false {
		t.Fatalf("a passkey proven by a password-only one: %v, want vouched false", rec)
	}
}

// TestAPasskeyThatCannotBeRecordedIsNotKept: the record is what says whether
// a passkey may decide cards, and a passkey with none would read as one that
// predates the rule. So a record write that fails removes the passkey and
// refuses.
func TestAPasskeyThatCannotBeRecordedIsNotKept(t *testing.T) {
	r := newEnrolRig(t, false)
	o := r.options()
	ch, _ := jsonBody(t, o)["challenge_id"].(string)
	r.fake.FailAdminUserGet(5)
	w := postJSON(r.edge, enrolVerify, map[string]any{"challenge_id": ch, "credential": gotruetest.Attestation("lost-cred", gotruetest.FlagUPUV)},
		withCookies(r.at, r.rt))
	r.fake.FailAdminUserGet(0)
	if w.Code == http.StatusOK {
		t.Fatalf("an unrecorded add answered 200: %s", w.Body)
	}
	if n := r.passkeys(); n != 0 {
		t.Fatalf("an unrecorded passkey was kept (%d)", n)
	}
}

// TestAPasskeyAddedWithThePasswordAloneDecidesNoCard: 🔴 the other half of
// the invariant. The owner's passkey recorded as added with the password
// alone (vouched false): the card's options offer no prompt, its verify is
// refused by name and the call never runs, the page draws the phone line and
// why. Control: the same passkey recorded as vouched decides it.
func TestAPasskeyAddedWithThePasswordAloneDecidesNoCard(t *testing.T) {
	r := newWebRig(t)
	x, card := r.card(t, "Ship it, with a password-only passkey?")
	credID, _, err := gotrue.AssertionIdentity(gotruetest.Assertion("owner-cred", gotruetest.FlagUPUV))
	if err != nil {
		t.Fatal(err)
	}
	pid := r.fake.Passkeys(r.subject())[0].ID
	setVouched := func(v bool) {
		t.Helper()
		if err := r.s.gt.Client.AdminSetAppMetadata(context.Background(), "muster-server", r.subject(), passkeyMetaKey,
			map[string]any{pid: map[string]any{passkeyCredIDKey: credID, "vouched": v}}); err != nil {
			t.Fatal(err)
		}
		r.s.stepUpCards.forget(r.subject())
	}
	_ = credID
	setVouched(false)
	if o := r.options(r.at, r.rt, card); o.Code != http.StatusOK || jsonBody(t, o)["available"] != false {
		t.Fatalf("a card's options for a password-only passkey: %d %s, want available:false", o.Code, o.Body)
	}
	page := do(r.edge, http.MethodGet, "/attention/asks/"+strconv.FormatInt(card, 10), nil, withCookies(r.at, r.rt), asNavigation).Body.String()
	if strings.Contains(page, `data-stepup-decide="Confirm"`) || !strings.Contains(page, "added with your password alone") {
		t.Fatal("the card offers a password-only passkey a Confirm, or does not say why not")
	}
	// Presented anyway, with a challenge minted while it was vouched.
	setVouched(true)
	ch := r.challengeFor(t, card)
	setVouched(false)
	w := r.verify(r.at, r.rt, card, ch, "owner-cred", gotruetest.FlagUPUV, "Confirm")
	if w.Code != http.StatusForbidden || errorOf(w) != unvouchedRefusal {
		t.Fatalf("a password-only passkey's Confirm: %d %s, want 403 %q", w.Code, w.Body, unvouchedRefusal)
	}
	if got := r.status(t, r.uid, x); got.Status != attention.Open {
		t.Fatalf("a password-only passkey ran the call: %+v", got)
	}
	// Control.
	setVouched(true)
	if w := r.decide(t, card, "Confirm"); w.Code != http.StatusOK {
		t.Fatalf("control: a vouched passkey: %d %s", w.Code, w.Body)
	}
	if got := r.status(t, r.uid, x); got.Status != attention.Answered {
		t.Fatalf("control: the call after a vouched passkey: %+v", got)
	}
}
