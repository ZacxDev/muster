package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/devices"
	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
)

// ROUND 3 OF THE ENROLMENT GATE (pairing_gate.go, passkey_removal.go,
// passkeys.go, admin.go): pairing a phone that confirms requests, removing a
// passkey, the admin pages on the admin's own account, a passkey whose age
// GoTrue does not give, and a removal of nothing. Literal expectations; red at
// the previous head where a password paired any phone, a passkey was deleted
// with the session alone, and the admin pages reached the admin's own account.

const pairProofRefusal = "To pair a phone that confirms requests, first confirm on Account, Passkeys with a passkey you have or your paired phone, then show the pairing code again."

// mintCode asks for a pairing code as the browser (at, rt): the code and
// whether it may pair a step-up key.
func (r *enrolRig) mintCode(t *testing.T) (string, bool) {
	t.Helper()
	w := do(r.edge, "POST", accountDevicesPairPath, nil, withCookies(r.at, r.rt))
	var out struct {
		Code   string `json:"code"`
		StepUp *bool  `json:"stepup_allowed"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Code == "" {
		t.Fatalf("pairing code: %d %s", w.Code, w.Body)
	}
	return out.Code, out.StepUp != nil && *out.StepUp
}

// redeemKeyed is a phone redeeming code with a fresh biometric StrongBox key.
func (r *enrolRig) redeemKeyed(t *testing.T, code string) *httpResult {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	body, _ := json.Marshal(map[string]string{"code": code, "name": "another phone", "app_version": "0.2.0",
		"stepup_pubkey": base64.StdEncoding.EncodeToString(der), "stepup_key_type": devices.KeyBiometric, "stepup_key_level": devices.LevelStrongBox})
	w := r.call(r.edge, "POST", devicePairPath, "", string(body))
	return &httpResult{code: w.Code, body: w.Body.String()}
}

type httpResult struct {
	code int
	body string
}

// phoneConfirmsProof files the phone proof and has the rig's step-up phone
// confirm it: this session then holds a ticket.
func (r *enrolRig) phoneConfirmsProof(t *testing.T) {
	t.Helper()
	w := postJSON(r.edge, enrolProofPhone, map[string]any{}, withCookies(r.at, r.rt))
	var out struct {
		Card int64 `json:"card"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Card == 0 {
		t.Fatalf("the phone proof: %d %s", w.Code, w.Body)
	}
	confirm := hostDecisionConfirm
	nonce, msg := r.challenge(t, r.tok, actionAskAnswer, out.Card, &confirm)
	if code, body := r.act(t, r.tok, "answer", out.Card, map[string]string{"nonce": nonce, "signature": sign(t, r.key, msg), "answer": confirm}); code != http.StatusOK {
		t.Fatalf("the phone's Confirm: %d %s", code, body)
	}
}

// pushedTitle counts the pushes to enrolPushEP whose body names title.
func (r *enrolRig) pushedTitle(title string) int {
	r.pusher.mu.Lock()
	defer r.pusher.mu.Unlock()
	n := 0
	for i, e := range r.pusher.sent {
		if e == enrolPushEP && strings.Contains(r.pusher.bodies[i], title) {
			n++
		}
	}
	return n
}

func (r *enrolRig) stepUpPhones(t *testing.T) int {
	t.Helper()
	list, err := r.s.ext.Devices.List(asUser(r.uid))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, d := range list {
		if d.HasStepUpKey {
			n++
		}
	}
	return n
}

// TestAPasswordAloneCannotPairAPhoneThatConfirms: 🔴 a phone with a step-up
// key answers cards. On an account that already has one (or a passkey), a
// pairing code minted with the password alone cannot pair another: the code
// says so, the redeem is refused by name and the code is not used up. With a
// ticket (the phone's Confirm) the next code can, once.
func TestAPasswordAloneCannotPairAPhoneThatConfirms(t *testing.T) {
	r := newEnrolRig(t, true)
	code, allowed := r.mintCode(t)
	if allowed {
		t.Fatal("a code minted with the password alone says it may pair a step-up key")
	}
	if res := r.redeemKeyed(t, code); res.code != http.StatusForbidden || !strings.Contains(res.body, pairProofRefusal) {
		t.Fatalf("a password-only code redeemed with a step-up key: %d %s, want 403", res.code, res.body)
	}
	if n := r.stepUpPhones(t); n != 1 {
		t.Fatalf("step-up phones after a refused pairing: %d, want 1", n)
	}
	// With the proof.
	r.phoneConfirmsProof(t)
	code, allowed = r.mintCode(t)
	if !allowed {
		t.Fatal("a code minted with a ticket may not pair a step-up key")
	}
	if res := r.redeemKeyed(t, code); res.code != http.StatusCreated {
		t.Fatalf("a proven code redeemed with a step-up key: %d %s", res.code, res.body)
	}
	// The ticket was spent by the mint.
	if _, again := r.mintCode(t); again {
		t.Fatal("one proof minted two codes that may pair a step-up key")
	}
	// The same rule on an account whose only factor is a passkey.
	o := newEnrolRig(t, false)
	o.fake.AddPasskey(o.subject(), "owner-cred")
	recordPasskey(t, o.s, o.fake, o.subject(), "owner-cred", true)
	code, allowed = o.mintCode(t)
	if res := o.redeemKeyed(t, code); allowed || res.code != http.StatusForbidden {
		t.Fatalf("an account with a passkey: allowed %v, redeem %d %s; want a refusal", allowed, res.code, res.body)
	}
}

// TestTheFirstPhoneOnAnEmptyAccountPairsWithItsKey: an account with neither a
// passkey nor a step-up phone pairs its first one with the password (its
// first factor), and every device already registered is told.
func TestTheFirstPhoneOnAnEmptyAccountPairsWithItsKey(t *testing.T) {
	r := newEnrolRig(t, false) // a read-only phone, registered for push
	code, allowed := r.mintCode(t)
	if !allowed {
		t.Fatal("the first step-up phone of an empty account may not pair")
	}
	if res := r.redeemKeyed(t, code); res.code != http.StatusCreated {
		t.Fatalf("the first step-up phone: %d %s", res.code, res.body)
	}
	if n := r.pushedTitle("A phone was paired to your account"); n != 1 {
		t.Fatalf("\"phone paired\" pushes to the existing phone: %d, want 1", n)
	}
}

// TestRemovingAPasskeyNeedsAProof: deleting one of the account's passkeys
// takes the proof (the passkey being removed will do) and is announced.
func TestRemovingAPasskeyNeedsAProof(t *testing.T) {
	r := newEnrolRig(t, false)
	r.fake.AddPasskey(r.subject(), "owner-cred")
	recordPasskey(t, r.s, r.fake, r.subject(), "owner-cred", true)
	pid := r.fake.Passkeys(r.subject())[0].ID
	del := func() int {
		return do(r.edge, http.MethodPost, accountPasskeysPath+"/"+pid+"/delete", nil, withCookies(r.at, r.rt)).Code
	}
	if c := del(); c != http.StatusForbidden {
		t.Fatalf("a password-only removal of a passkey: %d, want 403", c)
	}
	if n := r.passkeys(); n != 1 {
		t.Fatal("a password-only removal removed the passkey")
	}
	if w := r.proveWith(t, r.at, r.rt, "owner-cred"); w.Code != http.StatusOK {
		t.Fatalf("the proof with the passkey being removed: %d %s", w.Code, w.Body)
	}
	if c := del(); c != http.StatusSeeOther {
		t.Fatalf("the removal with the proof: %d, want 303", c)
	}
	if n := r.passkeys(); n != 0 {
		t.Fatal("the proven removal left the passkey")
	}
	if n := r.pushedTitle("A passkey was removed from your account"); n != 1 {
		t.Fatalf("\"passkey removed\" pushes: %d, want 1", n)
	}
}

// TestARemovalOfNothingSpendsNothing: removing a phone or a notification
// target that is not there answers 404 before the gate: the ticket is kept and
// nobody is told anything.
func TestARemovalOfNothingSpendsNothing(t *testing.T) {
	r := newEnrolRig(t, true)
	r.phoneConfirmsProof(t)
	for _, path := range []string{"/account/devices/987654321/remove", "/account/push/987654321/remove"} {
		if w := do(r.edge, http.MethodPost, path, nil, withCookies(r.at, r.rt)); w.Code != http.StatusNotFound {
			t.Fatalf("%s: %d, want 404", path, w.Code)
		}
	}
	c, _ := r.fake.Verifier().Verify(r.at)
	if _, held := r.s.passkeyEnrol.held(enrolKey(r.uid, gotrueCreds{claims: c}), r.s.stepUpNow()); !held {
		t.Fatal("a removal of nothing spent the ticket")
	}
	if n := r.pushedTitle("removed from your account"); n != 0 {
		t.Fatalf("a removal of nothing announced a removal (%d)", n)
	}
}

// TestAnAdminCannotResetOrStripTheirOwnAccount: the admin pages refuse the
// admin's own account (the account pages, with their proof, do that); another
// account still works (the control).
func TestAnAdminCannotResetOrStripTheirOwnAccount(t *testing.T) {
	w := newPasskeyWorld(t)
	c, err := w.fake.Verifier().Verify(w.adminAT)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := w.store.BySub(context.Background(), c.Subject)
	if err != nil {
		t.Fatal(err)
	}
	self := strconv.FormatInt(admin.ID, 10)
	for _, path := range []string{"/admin/users/" + self + "/reset-link", "/admin/users/" + self + "/passkeys/delete"} {
		rec := do(w.edge, http.MethodPost, path, url.Values{}, w.asAdmin())
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Not for your own account") {
			t.Fatalf("%s on the admin's own account: %d, want 403", path, rec.Code)
		}
	}
	other, _, _, _ := w.joinMember("pk-admin-other@invite.test")
	if rec := do(w.edge, http.MethodPost, "/admin/users/"+strconv.FormatInt(other, 10)+"/reset-link", url.Values{}, w.asAdmin()); rec.Code != http.StatusOK {
		t.Fatalf("control: a reset link for another account: %d", rec.Code)
	}
	_ = gotruetest.FlagUPUV
}
