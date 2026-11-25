package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/gotrue"
	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
)

// THE ENROLMENT GATE, ROUND 2 (passkey_enrol.go, passkey_removal.go): what
// "a passkey from before the rule" means, the record writers, the ticket's
// single use, and the removal of a phone or a notification target. Every
// expectation is a literal; each test is red at the previous head, where a
// passkey no record names was trusted, the ticket was spent after GoTrue
// registered, the records had no lock and removals needed only the session.

const (
	proofUnknownRefusal = "muster has no record of how that passkey was added, so it cannot confirm this. Confirm on your phone instead."
	removalRefusal      = "Removing a phone or a notification target needs a confirmation first"
)

// proveWith runs the enrolment proof with credID as the session (at, rt).
func (r *enrolRig) proveWith(t *testing.T, at, rt, credID string) *httptest.ResponseRecorder {
	t.Helper()
	o := postJSON(r.edge, enrolProofOptions, map[string]any{}, withCookies(at, rt))
	ch, _ := jsonBody(t, o)["challenge_id"].(string)
	if o.Code != http.StatusOK || ch == "" {
		t.Fatalf("proof options: %d %s", o.Code, o.Body)
	}
	return postJSON(r.edge, enrolProofVerify, map[string]any{"challenge_id": ch, "credential": gotruetest.Assertion(credID, gotruetest.FlagUPUV)},
		withCookies(at, rt))
}

// standingOf is the standing muster gives an assertion of credID.
func (r *enrolRig) standingOf(t *testing.T, credID string) passkeyStanding {
	t.Helper()
	c, err := r.fake.Verifier().Verify(r.at)
	if err != nil {
		t.Fatal(err)
	}
	st, err := r.s.assertionStanding(context.Background(), gotrueCreds{access: r.at, claims: c}, gotruetest.Assertion(credID, gotruetest.FlagUPUV))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// setCutover moves passkey_rules.vouching_since to at for this test.
func (r *enrolRig) setCutover(t *testing.T, at time.Time) {
	t.Helper()
	var was time.Time
	sys := authz.System(context.Background())
	if err := r.pool.QueryRow(sys, `SELECT vouching_since FROM passkey_rules WHERE singleton`).Scan(&was); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(sys, `UPDATE passkey_rules SET vouching_since = $1 WHERE singleton`, at); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = r.pool.Exec(authz.System(context.Background()), `UPDATE passkey_rules SET vouching_since = $1 WHERE singleton`, was)
	})
}

// TestARegistrationWhoseRawIdLiesIsRefused: 🔴 F1(a). GoTrue stores a
// credential under the id its SIGNED authenticator data names and never
// compares the rawId the client sends; a recorded rawId could name nothing.
// An add whose rawId differs from the signed id is refused before GoTrue
// registers anything; the honest add records the signed id.
func TestARegistrationWhoseRawIdLiesIsRefused(t *testing.T) {
	r := newEnrolRig(t, false)
	o := r.options()
	ch, _ := jsonBody(t, o)["challenge_id"].(string)
	w := postJSON(r.edge, enrolVerify, map[string]any{"challenge_id": ch,
		"credential": gotruetest.AttestationClaiming("bogus-claimed-id", "real-signed-id", gotruetest.FlagUPUV)}, withCookies(r.at, r.rt))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("an add whose rawId lies: %d %s, want 400", w.Code, w.Body)
	}
	if n := r.passkeys(); n != 0 {
		t.Fatalf("an add whose rawId lies registered %d passkey(s)", n)
	}
	if w := r.add(t, "honest-cred"); w.Code != http.StatusOK {
		t.Fatalf("control: an honest add: %d %s", w.Code, w.Body)
	}
	if rec := r.record(t, "honest-cred"); rec == nil {
		t.Fatal("control: the honest add recorded no credential id it can be found by")
	}
}

// TestAPasskeyMusterNeverRecordedDecidesNoCard: 🔴 F1(b). A passkey with no
// record — added straight at GoTrue's own endpoint, or one whose record was
// lost — is trusted only when GoTrue created it before muster started
// recording (passkey_rules.vouching_since). Newer, it proves no enrolment
// and its standing is unknown; older, it keeps working.
func TestAPasskeyMusterNeverRecordedDecidesNoCard(t *testing.T) {
	r := newEnrolRig(t, false)
	// Added straight at GoTrue with the session's own token (no muster add).
	ch := r.fake.NewChallenge(r.subject())
	if _, err := r.s.gt.Client.PasskeyRegisterVerify(context.Background(), "192.0.2.10", r.at, ch, gotruetest.Attestation("direct-cred", gotruetest.FlagUPUV)); err != nil {
		t.Fatalf("the direct registration: %v", err)
	}
	if st := r.standingOf(t, "direct-cred"); st != standingUnknown {
		t.Fatalf("a passkey added outside muster, after the rule: standing %d, want unknown", st)
	}
	if w := r.proveWith(t, r.at, r.rt, "direct-cred"); w.Code != http.StatusForbidden || errorOf(w) != proofUnknownRefusal {
		t.Fatalf("its enrolment proof: %d %s, want 403 %q", w.Code, w.Body, proofUnknownRefusal)
	}
	// Control: the same passkey, had GoTrue made it before the rule.
	r.setCutover(t, time.Now().Add(time.Hour))
	if st := r.standingOf(t, "direct-cred"); st != standingVouched {
		t.Fatalf("control: a passkey older than the rule: standing %d, want vouched", st)
	}
}

// TestAPasskeyWhoseRecordNamesAnotherIdIsNotTrusted: a record whose
// credential id names nothing the account's passkeys assert — what an add
// whose rawId lied left behind before the signed id was read — makes the real
// credential one no record names, and no passkey of the account is left
// unnamed: its standing is unknown, never "older than the rule".
func TestAPasskeyWhoseRecordNamesAnotherIdIsNotTrusted(t *testing.T) {
	r := newEnrolRig(t, false)
	r.fake.AddPasskey(r.subject(), "real-cred")
	pid := r.fake.Passkeys(r.subject())[0].ID
	r.s.setPasskeyFlag(context.Background(), r.subject(), pid, map[string]any{passkeyCredIDKey: gotrue.CredentialID([]byte("bogus-cred")), "vouched": false})
	r.setCutover(t, time.Now().Add(time.Hour)) // even with every passkey older than the rule
	if st := r.standingOf(t, "real-cred"); st != standingUnknown {
		t.Fatalf("a credential no record names, every record naming another: standing %d, want unknown", st)
	}
}

// TestAPasskeyWhoseRecordAndCleanUpBothFailDecidesNoCard: F4. The record write
// fails, and so does the clean-up that removes the passkey: it stays, with no
// record, newer than the rule — proving nothing, deciding no card.
func TestAPasskeyWhoseRecordAndCleanUpBothFailDecidesNoCard(t *testing.T) {
	r := newEnrolRig(t, false)
	o := r.options()
	ch, _ := jsonBody(t, o)["challenge_id"].(string)
	r.fake.FailAdminUserGet(5)
	r.fake.FailDeletePasskey(1)
	w := postJSON(r.edge, enrolVerify, map[string]any{"challenge_id": ch, "credential": gotruetest.Attestation("stuck-cred", gotruetest.FlagUPUV)},
		withCookies(r.at, r.rt))
	r.fake.FailAdminUserGet(0)
	if w.Code == http.StatusOK {
		t.Fatalf("an unrecorded add answered 200: %s", w.Body)
	}
	if n := r.passkeys(); n != 1 {
		t.Fatalf("fixture: the clean-up was meant to fail and leave the passkey (%d)", n)
	}
	if st := r.standingOf(t, "stuck-cred"); st != standingUnknown {
		t.Fatalf("an unrecorded passkey's standing: %d, want unknown", st)
	}
	if w := r.proveWith(t, r.at, r.rt, "stuck-cred"); w.Code != http.StatusForbidden {
		t.Fatalf("its enrolment proof: %d %s, want 403", w.Code, w.Body)
	}
}

// TestATicketIsTheSessionsAndLastsTenMinutes: F5. A ticket granted to one
// browser session adds nothing in another of the same person; one past ten
// minutes adds nothing at all.
func TestATicketIsTheSessionsAndLastsTenMinutes(t *testing.T) {
	r := newEnrolRig(t, false)
	r.fake.AddPasskey(r.subject(), "owner-cred")
	recordPasskey(t, r.s, r.fake, r.subject(), "owner-cred", true)
	if w := r.proveWith(t, r.at, r.rt, "owner-cred"); w.Code != http.StatusOK {
		t.Fatalf("the proof in session A: %d %s", w.Code, w.Body)
	}
	bat, brt := r.login() // session B, the same person
	if w := postJSON(r.edge, enrolOptions, map[string]any{}, withCookies(bat, brt)); w.Code != http.StatusForbidden {
		t.Fatalf("session B with session A's ticket: %d %s, want 403 proof_required", w.Code, w.Body)
	}
	// Past ten minutes: the ticket's grant moved back.
	c, _ := r.fake.Verifier().Verify(r.at)
	key := enrolKey(r.uid, gotrueCreds{claims: c})
	r.s.passkeyEnrol.mu.Lock()
	tk := r.s.passkeyEnrol.tickets[key]
	tk.granted = tk.granted.Add(-11 * time.Minute)
	r.s.passkeyEnrol.tickets[key] = tk
	r.s.passkeyEnrol.mu.Unlock()
	proofOf(t, r.options())
}

// TestTwoAddsRacingOnOneTicketAddOnePasskey: F2. The ticket is taken at the
// check, so two verifies on one ticket — the second landing while the first
// is at GoTrue — add exactly one passkey. RED BEFORE: the ticket was only
// looked at until GoTrue had registered, so both added one.
func TestTwoAddsRacingOnOneTicketAddOnePasskey(t *testing.T) {
	r := newEnrolRig(t, false)
	r.fake.AddPasskey(r.subject(), "owner-cred")
	recordPasskey(t, r.s, r.fake, r.subject(), "owner-cred", true)
	if w := r.proveWith(t, r.at, r.rt, "owner-cred"); w.Code != http.StatusOK {
		t.Fatalf("the proof: %d %s", w.Code, w.Body)
	}
	var chs []string
	for i := 0; i < 2; i++ {
		o := r.options()
		ch, _ := jsonBody(t, o)["challenge_id"].(string)
		if o.Code != http.StatusOK || ch == "" {
			t.Fatalf("options %d: %d %s", i, o.Code, o.Body)
		}
		chs = append(chs, ch)
	}
	// The second verify lands while the first is at GoTrue (the fake's hook),
	// as two taps sent together do.
	codes := make([]int, 2)
	verify := func(i int) int {
		return postJSON(r.edge, enrolVerify, map[string]any{"challenge_id": chs[i],
			"credential": gotruetest.Attestation("race-cred-"+strconv.Itoa(i), gotruetest.FlagUPUV)}, withCookies(r.at, r.rt)).Code
	}
	r.fake.BeforeRegister(func() { codes[1] = verify(1) })
	codes[0] = verify(0)
	ok := 0
	for _, c := range codes {
		if c == http.StatusOK {
			ok++
		}
	}
	if ok != 1 || r.passkeys() != 2 {
		t.Fatalf("two adds on one ticket: codes %v, %d passkeys; want exactly one added (2 in all)", codes, r.passkeys())
	}
}

// TestConcurrentRecordWritesLoseNothing: F2. The records are one value read,
// changed and written back; writers racing on one account must each land.
func TestConcurrentRecordWritesLoseNothing(t *testing.T) {
	r := newEnrolRig(t, false)
	sub := r.subject()
	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "pk-" + strconv.Itoa(i)
			rec := map[string]any{passkeyCredIDKey: gotrue.CredentialID([]byte(id)), "vouched": true}
			if i%2 == 0 {
				r.s.setPasskeyFlag(context.Background(), sub, id, rec)
			} else if err := r.s.recordAddedPasskey(context.Background(), sub, id, rec); err != nil {
				t.Errorf("record %s: %v", id, err)
			}
		}(i)
	}
	wg.Wait()
	recs, err := r.s.passkeyRecords(context.Background(), sub)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != n {
		t.Fatalf("%d of %d concurrent record writes landed", len(recs), n)
	}
}

// TestTheEnrolmentCardIsThePhonesAlone: F5. The browser's card door refuses
// the "Add a passkey" card by name — its options mint no prompt.
func TestTheEnrolmentCardIsThePhonesAlone(t *testing.T) {
	r := newEnrolRig(t, true)
	r.fake.AddPasskey(r.subject(), "owner-cred")
	recordPasskey(t, r.s, r.fake, r.subject(), "owner-cred", true)
	w := postJSON(r.edge, enrolProofPhone, map[string]any{}, withCookies(r.at, r.rt))
	var out struct {
		Card int64 `json:"card"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Card == 0 {
		t.Fatalf("the phone proof: %d %s", w.Code, w.Body)
	}
	o := postJSON(r.edge, "/attention/asks/"+strconv.FormatInt(out.Card, 10)+"/stepup/passkey/options", map[string]any{}, withCookies(r.at, r.rt))
	if o.Code != http.StatusForbidden || errorOf(o) != "This card is confirmed only on your paired phone." {
		t.Fatalf("a passkey's options for the enrolment card: %d %s, want 403", o.Code, o.Body)
	}
}

// TestRemovingAPhoneNeedsAProofWhereTheAccountCanGiveOne: 🟡 F3. Removing a
// paired phone or a notification target can no longer silence the "passkey
// added" notice: on an account with a step-up phone it needs a ticket (here
// from the phone's Confirm), and the phone stays until then. On an account
// with neither a passkey nor a step-up phone it is removed as before — after
// every push device is told.
func TestRemovingAPhoneNeedsAProofWhereTheAccountCanGiveOne(t *testing.T) {
	r := newEnrolRig(t, true)
	remove := func() *httptest.ResponseRecorder {
		return do(r.edge, http.MethodPost, "/account/devices/"+strconv.FormatInt(r.dev, 10)+"/remove", nil, withCookies(r.at, r.rt))
	}
	if w := remove(); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), removalRefusal) {
		t.Fatalf("a password-only removal of the step-up phone: %d, want 403 naming the proof", w.Code)
	}
	if c := r.config(r.tok); c != http.StatusOK {
		t.Fatalf("the phone after a refused removal: %d, want still paired", c)
	}
	// The phone's Confirm grants the ticket; the removal then goes through.
	w := postJSON(r.edge, enrolProofPhone, map[string]any{}, withCookies(r.at, r.rt))
	var out struct {
		Card int64 `json:"card"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	confirm := hostDecisionConfirm
	nonce, msg := r.challenge(t, r.tok, actionAskAnswer, out.Card, &confirm)
	if code, body := r.act(t, r.tok, "answer", out.Card, map[string]string{"nonce": nonce, "signature": sign(t, r.key, msg), "answer": confirm}); code != http.StatusOK {
		t.Fatalf("the phone's Confirm: %d %s", code, body)
	}
	if w := remove(); w.Code != http.StatusSeeOther {
		t.Fatalf("the removal with a ticket: %d %s", w.Code, w.Body)
	}
	if c := r.config(r.tok); c != http.StatusUnauthorized {
		t.Fatalf("the removed phone: %d, want 401", c)
	}

	// Nothing to prove with: removed as before, every push device told first.
	o := newEnrolRig(t, false)
	o.pusher.mu.Lock()
	before := len(o.pusher.sent)
	o.pusher.mu.Unlock()
	if w := do(o.edge, http.MethodPost, "/account/devices/"+strconv.FormatInt(o.dev, 10)+"/remove", nil, withCookies(o.at, o.rt)); w.Code != http.StatusSeeOther {
		t.Fatalf("removal on an account with nothing to prove with: %d %s", w.Code, w.Body)
	}
	o.pusher.mu.Lock()
	told := false
	for i, e := range o.pusher.sent {
		if i >= before && e == enrolPushEP && strings.Contains(o.pusher.bodies[i], "A phone was removed from your account") {
			told = true
		}
	}
	o.pusher.mu.Unlock()
	if !told {
		t.Fatal("the phone was removed without being told")
	}
}
