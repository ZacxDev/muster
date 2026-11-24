package api

import (
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
	"github.com/ZacxDev/muster/internal/users"
)

// A STEP-UP CARD DECIDED IN THE BROWSER WITH A FRESH PASSKEY
// (stepup_web_confirm.go), over Postgres, the real GoTrue session tier (the
// fake GoTrue, which does no cryptography: the WebAuthn signature itself is
// exercised by e2e/tests/mirror-stepup.spec.ts, "a step-up card confirmed in
// the browser with a passkey"), the browser listener and
// the step-up gate of a host officer's machine.
//
// 🔴 EVERY EXPECTATION IS A LITERAL — the routes, the refusals, the log line's
// shape — never a constant read back from the code under test, so the file
// compiles and RUNS against the commit before the feature, where each test
// fails (the routes are 404, the phone line is the only line, no log line is
// written). That is the red half of the PR's matrix.

const (
	webCardOtherPrompt = "That passkey prompt was for another card, or has expired. Nothing was decided: try again."
	webCardDecided     = "This card was already decided. Nothing more was run."
	webCardClosed      = "This card is closed, so its request was dropped and nothing ran."
	webCardNotOperator = "this is a team's question: only an operator or a manager of the team answers it"
	webPasskeyLine     = "Confirm here with your passkey, or on your phone."
	webPhoneLine       = "Confirm this on your phone."
)

func webCardPath(card int64, leaf string) string {
	return "/attention/asks/" + strconv.FormatInt(card, 10) + "/stepup/passkey/" + leaf
}

// webRig is the step-up race rig (a host officer's machine, the flaky
// attention store) with passkeys on in its GoTrue, the owner holding
// "owner-cred", and the owner signed in in a browser (at, rt).
type webRig struct {
	*stepUpRaceRig
	at, rt string
}

func newWebRig(t *testing.T) *webRig {
	t.Helper()
	r := newStepUpRaceRig(t)
	r.fake.EnablePasskeys()
	r.fake.AddPasskey(r.subject(), "owner-cred")
	at, rt := r.login()
	return &webRig{stepUpRaceRig: r, at: at, rt: rt}
}

// options asks for a card decision's challenge as the browser (at, rt).
func (r *webRig) options(at, rt string, card int64) *httptest.ResponseRecorder {
	return postJSON(r.edge, webCardPath(card, "options"), map[string]any{}, withCookies(at, rt))
}

// challengeFor is a minted challenge's id, failing the test on any refusal.
func (r *webRig) challengeFor(t *testing.T, card int64) string {
	t.Helper()
	o := r.options(r.at, r.rt, card)
	if o.Code != http.StatusOK {
		t.Fatalf("options of card %d: %d %s", card, o.Code, o.Body)
	}
	body := jsonBody(t, o)
	ch, _ := body["challenge_id"].(string)
	if body["available"] != true || ch == "" {
		t.Fatalf("options of card %d offered no challenge: %s", card, o.Body)
	}
	opts, _ := body["options"].(map[string]any)
	if opts["userVerification"] != "required" {
		t.Fatalf("the card's options do not REQUIRE user verification: %v", opts["userVerification"])
	}
	return ch
}

// verify presents an assertion for credID (flags) on challenge ch at card's
// verify route, answering answer.
func (r *webRig) verify(at, rt string, card int64, ch, credID string, flags byte, answer string) *httptest.ResponseRecorder {
	body := map[string]any{"challenge_id": ch, "answer": answer, "return": "/attention/asks/" + strconv.FormatInt(card, 10)}
	if credID != "" {
		body["credential"] = gotruetest.Assertion(credID, flags)
	}
	return postJSON(r.edge, webCardPath(card, "verify"), body, withCookies(at, rt))
}

// decide is the whole browser decision of card: options, then the owner's
// user-verified passkey.
func (r *webRig) decide(t *testing.T, card int64, answer string) *httptest.ResponseRecorder {
	t.Helper()
	return r.verify(r.at, r.rt, card, r.challengeFor(t, card), "owner-cred", gotruetest.FlagUPUV, answer)
}

// answersTo counts the times the held call — an attention_answer of ask id,
// a session's answer — reached the store: how many times it RAN.
func (r *webRig) answersTo(id int64) int {
	r.flaky.mu.Lock()
	defer r.flaky.mu.Unlock()
	n := 0
	for _, a := range append(append([]int64{}, r.flaky.answered...), r.flaky.fromSession...) {
		if a == id {
			n++
		}
	}
	return n
}

// auditDoor is the door named by card's stepup.decided audit rows, "" for none
// (and the test fails on more than one).
func (r *webRig) auditDoor(t *testing.T, card int64) string {
	t.Helper()
	rows, err := r.pool.Query(authz.System(context.Background()), `SELECT detail->>'door' FROM audit_log WHERE user_id = $1 AND action = 'stepup.decided' AND object = $2`,
		r.uid, "ask:"+strconv.FormatInt(card, 10))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var doors []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		doors = append(doors, d)
	}
	if len(doors) > 1 {
		t.Fatalf("card %d has %d decision audit rows: %v", card, len(doors), doors)
	}
	if len(doors) == 0 {
		return ""
	}
	return doors[0]
}

// TestAStepUpCardIsConfirmedInTheBrowserWithAPasskey: Confirm on the card's
// page, behind a passkey ceremony made for it, runs the held call ONCE,
// exactly as asked — the same outcome as the phone's Confirm — answers the
// card "Confirm", audits the door as the passkey, and says where to go next. A
// second Confirm, from the browser or the phone, is refused as already decided
// and runs nothing more.
func TestAStepUpCardIsConfirmedInTheBrowserWithAPasskey(t *testing.T) {
	r := newWebRig(t)
	x, card := r.card(t, "Ship it from the browser?")
	w := r.decide(t, card, "Confirm")
	if w.Code != http.StatusOK {
		t.Fatalf("the browser's Confirm: %d %s", w.Code, w.Body)
	}
	if next := jsonBody(t, w)["next"]; next != "/attention/asks/"+strconv.FormatInt(card, 10) {
		t.Fatalf("the Confirm's next page: %v", next)
	}
	if got := r.status(t, r.uid, x); got.Status != attention.Answered || got.Answer != "yes" {
		t.Fatalf("the held call after the browser's Confirm: %+v, want the ask answered yes", got)
	}
	if got := r.status(t, r.uid, card); got.Status != attention.Answered || got.Answer != "Confirm" {
		t.Fatalf("the card after the browser's Confirm: %+v", got)
	}
	if door := r.auditDoor(t, card); door != "passkey" {
		t.Fatalf("the decision's audit door = %q, want passkey", door)
	}
	if n := r.answersTo(x); n != 1 {
		t.Fatalf("the held call answered its ask %d times, want once", n)
	}
	// Again, from the browser: already decided, nothing more runs.
	if again := r.options(r.at, r.rt, card); again.Code != http.StatusConflict || errorOf(again) != webCardDecided {
		t.Fatalf("a second browser Confirm's prompt: %d %s, want 409 %q", again.Code, again.Body, webCardDecided)
	}
	// And from the phone.
	if code, body := r.confirm(t, card); code != http.StatusConflict || body != "not_open" {
		t.Fatalf("a phone Confirm after the browser's: %d %s, want 409 not_open", code, body)
	}
	if n := r.answersTo(x); n != 1 {
		t.Fatalf("after the retries the held call answered its ask %d times, want once", n)
	}
	// The ceremony decides the card and makes no session fresh: every session
	// GoTrue minted for it was signed out.
	if n := r.fake.CountCalls(http.MethodPost, "/logout"); n < 1 {
		t.Fatalf("GoTrue /logout calls = %d: the ceremony's session was not ended", n)
	}
}

// TestAStepUpCardRefusedInTheBrowserRunsNothing: Refuse behind the passkey
// answers the card "Refuse" and the held call never runs.
func TestAStepUpCardRefusedInTheBrowserRunsNothing(t *testing.T) {
	r := newWebRig(t)
	x, card := r.card(t, "Ship it, refused in the browser?")
	if w := r.decide(t, card, "Refuse"); w.Code != http.StatusOK {
		t.Fatalf("the browser's Refuse: %d %s", w.Code, w.Body)
	}
	if got := r.status(t, r.uid, card); got.Status != attention.Answered || got.Answer != "Refuse" {
		t.Fatalf("the card after the Refuse: %+v", got)
	}
	if got := r.status(t, r.uid, x); got.Status != attention.Open || got.Answer != "" {
		t.Fatalf("the refused call ran: its ask is %+v", got)
	}
	if n := r.answersTo(x); n != 0 {
		t.Fatalf("a refused card's call reached the store %d times", n)
	}
	if door := r.auditDoor(t, card); door != "passkey" {
		t.Fatalf("the refusal's audit door = %q, want passkey", door)
	}
}

// TestAPasskeyPromptForOneCardCannotDecideAnother: 🔴 the card binding. A
// challenge minted for card A, presented — with a perfectly good assertion —
// at card B's verify, is refused and decides neither; it is spent by that
// attempt, so A cannot use it either; A's own fresh prompt decides A. In
// process too: a passkey proof made for A, on the context of an answer to B,
// is no door for B.
func TestAPasskeyPromptForOneCardCannotDecideAnother(t *testing.T) {
	r := newWebRig(t)
	xa, cardA := r.card(t, "Ship A?")
	xb, cardB := r.card(t, "Ship B?")
	chA := r.challengeFor(t, cardA)
	w := r.verify(r.at, r.rt, cardB, chA, "owner-cred", gotruetest.FlagUPUV, "Confirm")
	if w.Code != http.StatusForbidden || errorOf(w) != webCardOtherPrompt {
		t.Fatalf("A's prompt at B: %d %s, want 403 %q", w.Code, w.Body, webCardOtherPrompt)
	}
	for _, id := range []int64{xa, xb, cardA, cardB} {
		if got := r.status(t, r.uid, id); got.Status != attention.Open {
			t.Fatalf("ask %d after A's prompt was presented at B: %+v, want open", id, got)
		}
	}
	if w := r.verify(r.at, r.rt, cardA, chA, "owner-cred", gotruetest.FlagUPUV, "Confirm"); w.Code != http.StatusForbidden || errorOf(w) != webCardOtherPrompt {
		t.Fatalf("A's spent prompt replayed at A: %d %s, want 403 %q", w.Code, w.Body, webCardOtherPrompt)
	}
	if got := r.status(t, r.uid, xa); got.Status != attention.Open {
		t.Fatalf("a replayed prompt ran A's call: %+v", got)
	}
	// In process: a proof for A on an answer to B.
	proofA := withStepUpPasskeyDecision(asUser(r.uid), cardA, time.Now())
	if _, err := r.s.answerAskFrom(proofA, cardB, hostDecisionConfirm, attention.AnswerSource{}); err == nil || !strings.Contains(err.Error(), "paired phone") {
		t.Fatalf("a passkey proof for card A answering card B: %v, want refused as needing the phone or a passkey for B", err)
	}
	if got := r.status(t, r.uid, xb); got.Status != attention.Open {
		t.Fatalf("A's proof ran B's call: %+v", got)
	}
	// Control: A's own fresh prompt decides A, and B is untouched.
	if w := r.decide(t, cardA, "Confirm"); w.Code != http.StatusOK {
		t.Fatalf("control: A's own prompt: %d %s", w.Code, w.Body)
	}
	if got := r.status(t, r.uid, xa); got.Status != attention.Answered || got.Answer != "yes" {
		t.Fatalf("control: A's call after its own prompt: %+v", got)
	}
	if got := r.status(t, r.uid, cardB); got.Status != attention.Open {
		t.Fatalf("B after A was decided: %+v, want open", got)
	}
}

// TestABrowserDecisionNeedsAPasskeyAssertion: no ceremony, no decision. The
// card's ordinary answer form — on a session that signed in a moment ago — is
// still refused; a verify with no assertion, one without user verification,
// and another account's passkey are each refused; the card and its call stay
// as they were throughout. The control decides.
func TestABrowserDecisionNeedsAPasskeyAssertion(t *testing.T) {
	r := newWebRig(t)
	x, card := r.card(t, "Ship it, with no passkey?")
	other := r.fake.AddUser("someone-else@example.com", "someone else's password")
	r.fake.AddPasskey(other, "other-cred")
	open := func(what string) {
		t.Helper()
		if got := r.status(t, r.uid, card); got.Status != attention.Open {
			t.Fatalf("%s: the card is %+v, want open", what, got)
		}
		if got := r.status(t, r.uid, x); got.Status != attention.Open {
			t.Fatalf("%s: the held call ran: %+v", what, got)
		}
	}
	// A fresh sign-in is not enough: the plain answer form.
	form := do(r.edge, http.MethodPost, "/attention/asks/"+strconv.FormatInt(card, 10)+"/answer",
		url.Values{"option": {"Confirm"}, "return": {"/attention/asks/" + strconv.FormatInt(card, 10)}}, withCookies(r.at, r.rt))
	if form.Code >= 500 {
		t.Fatalf("the answer form: %d", form.Code)
	}
	open("the plain answer form on a fresh session")
	cases := []struct {
		name, cred string
		flags      byte
		code       int
	}{
		{"no assertion", "", 0, http.StatusBadRequest},
		{"no user verification", "owner-cred", gotruetest.FlagUP, http.StatusUnauthorized},
		{"another account's passkey", "other-cred", gotruetest.FlagUPUV, http.StatusForbidden},
	}
	for _, tc := range cases {
		w := r.verify(r.at, r.rt, card, r.challengeFor(t, card), tc.cred, tc.flags, "Confirm")
		if w.Code != tc.code {
			t.Fatalf("%s: %d %s, want %d", tc.name, w.Code, w.Body, tc.code)
		}
		open(tc.name)
	}
	if _, ok := r.s.hostDecisions.peek(card); !ok {
		t.Fatal("a refused browser decision dropped the card's pending decision")
	}
	// Control: the owner's own user-verified passkey decides it.
	w := r.verify(r.at, r.rt, card, r.challengeFor(t, card), "owner-cred", gotruetest.FlagUPUV, "Confirm")
	if w.Code != http.StatusOK {
		t.Fatalf("control: the owner's passkey: %d %s", w.Code, w.Body)
	}
	if got := r.status(t, r.uid, x); got.Status != attention.Answered {
		t.Fatalf("control: the call after the owner's passkey: %+v", got)
	}
}

// loginAs adds a muster user with a GoTrue login and signs them in on the
// rig's browser listener: their id and cookies.
func (r *webRig) loginAs(t *testing.T, name string) (int64, string, string) {
	t.Helper()
	email := name + strconv.FormatInt(time.Now().UnixNano(), 36) + "@example.com"
	pw := name + "'s password"
	sub := r.fake.AddUser(email, pw)
	var uid int64
	if err := r.pool.QueryRow(authz.System(context.Background()), `INSERT INTO users (role, display_name, gotrue_sub, email) VALUES ('member', $1, $2, $3) RETURNING id`,
		name, sub, email).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		_, _ = r.pool.Exec(bg, `DELETE FROM audit_log WHERE user_id = $1`, uid)
		_, _ = r.pool.Exec(bg, `DELETE FROM users WHERE id = $1`, uid)
	})
	r.fake.AddPasskey(sub, name+"-cred")
	w := do(r.edge, "POST", "/login", url.Values{"email": {email}, "password": {pw}})
	ck := setCookies(w)
	if w.Code != http.StatusSeeOther || ck[accessCookieName] == nil {
		t.Fatalf("sign-in of %s: %d", name, w.Code)
	}
	return uid, ck[accessCookieName].Value, ck[refreshCookieName].Value
}

// TestOnlyTheOwnerDecidesAStepUpCardInTheBrowser: another person — with a
// passkey of their own — cannot mint a prompt for the owner's card, nor
// present one; and a TEAM's card is held to the phone door's operator rule: a
// viewer of the team is refused by name, before any challenge is minted.
func TestOnlyTheOwnerDecidesAStepUpCardInTheBrowser(t *testing.T) {
	r := newWebRig(t)
	x, card := r.card(t, "Ship it, decided by someone else?")
	_, oat, ort := r.loginAs(t, "stranger")
	if w := r.options(oat, ort, card); w.Code != http.StatusNotFound {
		t.Fatalf("another person's options for the owner's card: %d %s, want 404", w.Code, w.Body)
	}
	if w := r.verify(oat, ort, card, r.challengeFor(t, card), "stranger-cred", gotruetest.FlagUPUV, "Confirm"); w.Code == http.StatusOK {
		t.Fatalf("another person presented the owner's prompt: %d %s", w.Code, w.Body)
	}
	if got := r.status(t, r.uid, x); got.Status != attention.Open {
		t.Fatalf("another person ran the owner's call: %+v", got)
	}

	// A team's card, the owner a viewer of the team.
	sys := authz.System(context.Background())
	us := users.NewPG(r.pool)
	admin := users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}
	tm, err := us.CreateTeam(sys, admin, "stepup-web-"+strconv.FormatInt(time.Now().UnixNano(), 36), authz.OperatorUserID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM attention_asks WHERE owner_id = $1`, `DELETE FROM team_members WHERE team_id = $1`,
			`DELETE FROM audit_log WHERE team_id = $1 OR object = 'team:' || $1::text`, `DELETE FROM users WHERE id = $1`} {
			_, _ = r.pool.Exec(bg, q, tm.ID)
		}
	})
	if err := us.SetTeamMember(sys, admin, tm.ID, r.uid, authz.TeamViewer); err != nil {
		t.Fatal(err)
	}
	asTeam := authz.WithPrincipal(context.Background(), authz.Principal{UserID: tm.ID, Team: true, Role: authz.RoleMember, Via: authz.ViaAttention})
	tc, err := r.attn.Create(asTeam, attention.New{Asker: attention.Asker{System: true, Label: hostDecisionLabel},
		StepUp: &attention.StepUpCard{Tool: attentionAnswerTool, Title: "Answer the team's ask?", Why: "A fixture card of the team's.",
			Details: "attention_answer {\"id\":1,\"answer\":\"yes\"}"},
		Options: []string{hostDecisionConfirm, hostDecisionRefuse}})
	if err != nil {
		t.Fatal(err)
	}
	r.s.hostDecisions.put(tc.ID, hostDecision{owner: tm.ID, host: hostOffHost, tool: attentionAnswerTool, args: `{"id":1,"answer":"yes"}`})
	// The owner's session was signed in before the membership: a new one.
	at, rt := r.login()
	before := r.fake.CountCalls(http.MethodPost, "/passkeys/authentication/options")
	if w := r.options(at, rt, tc.ID); w.Code != http.StatusForbidden || errorOf(w) != webCardNotOperator {
		t.Fatalf("a team viewer's options for the team's card: %d %s, want 403 %q", w.Code, w.Body, webCardNotOperator)
	}
	if n := r.fake.CountCalls(http.MethodPost, "/passkeys/authentication/options"); n != before {
		t.Fatal("a team viewer's refused options minted a challenge")
	}
	if got, err := r.attn.Get(asTeam, tc.ID); err != nil || got.Status != attention.Open {
		t.Fatalf("the team's card after a viewer's attempt: %+v %v", got, err)
	}
	if _, ok := r.s.hostDecisions.peek(tc.ID); !ok {
		t.Fatal("a viewer's refused attempt dropped the team card's decision")
	}
}

// TestAClosedStepUpCardCannotBeDecidedInTheBrowser: a card dismissed — even
// after its prompt was minted — or expired is refused as closed, and its call
// never runs.
func TestAClosedStepUpCardCannotBeDecidedInTheBrowser(t *testing.T) {
	r := newWebRig(t)
	// Dismissed after its prompt was minted.
	x, card := r.card(t, "Ship it, dismissed mid-prompt?")
	ch := r.challengeFor(t, card)
	if _, err := r.s.cancelAsk(asUser(r.uid), card); err != nil {
		t.Fatal(err)
	}
	w := r.verify(r.at, r.rt, card, ch, "owner-cred", gotruetest.FlagUPUV, "Confirm")
	if w.Code != http.StatusConflict || errorOf(w) != webCardClosed {
		t.Fatalf("a dismissed card's Confirm: %d %s, want 409 %q", w.Code, w.Body, webCardClosed)
	}
	if got := r.status(t, r.uid, x); got.Status != attention.Open {
		t.Fatalf("a dismissed card's call ran: %+v", got)
	}
	if w := r.options(r.at, r.rt, card); w.Code != http.StatusConflict || errorOf(w) != webCardClosed {
		t.Fatalf("a dismissed card's options: %d %s, want 409 %q", w.Code, w.Body, webCardClosed)
	}
	// Expired.
	x2, card2 := r.card(t, "Ship it, expired?")
	ch2 := r.challengeFor(t, card2)
	if _, err := r.pool.Exec(authz.System(context.Background()), `UPDATE attention_asks SET status = 'expired', resolved_at = now() WHERE id = $1`, card2); err != nil {
		t.Fatal(err)
	}
	if w := r.verify(r.at, r.rt, card2, ch2, "owner-cred", gotruetest.FlagUPUV, "Confirm"); w.Code != http.StatusConflict || errorOf(w) != webCardClosed {
		t.Fatalf("an expired card's Confirm: %d %s, want 409 %q", w.Code, w.Body, webCardClosed)
	}
	if got := r.status(t, r.uid, x2); got.Status != attention.Open {
		t.Fatalf("an expired card's call ran: %+v", got)
	}
}

// TestAPhoneAndABrowserRacingDecideACardOnce: the phone's Confirm and the
// browser's land on one card together. Whichever claims it first runs the
// call, once; the other — reaching the card while the winner is recording its
// answer — is told it was already decided, and neither withdraws the card as
// lost. 🔴 RED BEFORE: the loser found no pending decision and an OPEN card,
// withdrew it as lost by a restart, and the winner's claim then failed —
// nothing ran at all.
func TestAPhoneAndABrowserRacingDecideACardOnce(t *testing.T) {
	t.Run("browser first", func(t *testing.T) {
		r := newWebRig(t)
		x, card := r.card(t, "Ship it, browser first?")
		ch := r.challengeFor(t, card)
		var phoneCode int
		var phoneBody string
		r.flaky.mu.Lock()
		r.flaky.beforeAnswer = func() { phoneCode, phoneBody = r.confirm(t, card) }
		r.flaky.mu.Unlock()
		w := r.verify(r.at, r.rt, card, ch, "owner-cred", gotruetest.FlagUPUV, "Confirm")
		if w.Code != http.StatusOK {
			t.Fatalf("the browser's Confirm: %d %s", w.Code, w.Body)
		}
		if phoneCode != http.StatusConflict || phoneBody != "not_open" {
			t.Fatalf("the racing phone Confirm: %d %s, want 409 not_open", phoneCode, phoneBody)
		}
		if got := r.status(t, r.uid, x); got.Status != attention.Answered || r.answersTo(x) != 1 {
			t.Fatalf("the call: %+v, answered %d times; want once", got, r.answersTo(x))
		}
		if door := r.auditDoor(t, card); door != "passkey" {
			t.Fatalf("the winning door = %q, want passkey", door)
		}
	})
	t.Run("phone first", func(t *testing.T) {
		r := newWebRig(t)
		x, card := r.card(t, "Ship it, phone first?")
		ch := r.challengeFor(t, card)
		var web *httptest.ResponseRecorder
		r.flaky.mu.Lock()
		r.flaky.beforeAnswer = func() { web = r.verify(r.at, r.rt, card, ch, "owner-cred", gotruetest.FlagUPUV, "Confirm") }
		r.flaky.mu.Unlock()
		if code, body := r.confirm(t, card); code != http.StatusOK {
			t.Fatalf("the phone's Confirm: %d %s", code, body)
		}
		if web == nil || web.Code != http.StatusConflict || errorOf(web) != webCardDecided {
			code, body := 0, ""
			if web != nil {
				code, body = web.Code, web.Body.String()
			}
			t.Fatalf("the racing browser Confirm: %d %s, want 409 %q", code, body, webCardDecided)
		}
		if got := r.status(t, r.uid, x); got.Status != attention.Answered || r.answersTo(x) != 1 {
			t.Fatalf("the call: %+v, answered %d times; want once", got, r.answersTo(x))
		}
		if door := r.auditDoor(t, card); door != "phone" {
			t.Fatalf("the winning door = %q, want phone", door)
		}
	})
	t.Run("together", func(t *testing.T) {
		r := newWebRig(t)
		for round := 0; round < 4; round++ {
			x, card := r.card(t, "Ship it, together "+strconv.Itoa(round)+"?")
			ch := r.challengeFor(t, card)
			var wg sync.WaitGroup
			wg.Add(2)
			var web *httptest.ResponseRecorder
			var phone int
			go func() {
				defer wg.Done()
				web = r.verify(r.at, r.rt, card, ch, "owner-cred", gotruetest.FlagUPUV, "Confirm")
			}()
			// The phone's signed Confirm, its challenge taken first (no
			// t.Fatal from the racing goroutine).
			confirm := hostDecisionConfirm
			nonce, msg := r.challenge(t, r.tok, actionAskAnswer, card, &confirm)
			signed := map[string]string{"nonce": nonce, "signature": sign(t, r.key, msg), "answer": confirm}
			go func() { defer wg.Done(); phone, _ = r.act(t, r.tok, "answer", card, signed) }()
			wg.Wait()
			wins := 0
			if web.Code == http.StatusOK {
				wins++
			}
			if phone == http.StatusOK {
				wins++
			}
			if wins != 1 || r.answersTo(x) != 1 {
				t.Fatalf("round %d: web %d, phone %d, the call answered %d times; want exactly one winner and one run", round, web.Code, phone, r.answersTo(x))
			}
		}
	})
}

// TestASecondConfirmWhileTheFirstIsRecordedNeverWithdrawsTheCard: two
// Confirms of one card from the phone (a double tap, two devices), the second
// landing while the first is recording its claim. The first runs the call,
// once; the second is told not open. 🔴 RED BEFORE: the second found no
// pending decision and the card still open, withdrew it as LOST by a restart,
// and the first's claim then failed — the owner confirmed and NOTHING ran.
func TestASecondConfirmWhileTheFirstIsRecordedNeverWithdrawsTheCard(t *testing.T) {
	r := newStepUpRaceRig(t)
	x, card := r.card(t, "Ship it, tapped twice?")
	var second int
	var secondCode string
	r.flaky.mu.Lock()
	r.flaky.beforeAnswer = func() { second, secondCode = r.confirm(t, card) }
	r.flaky.mu.Unlock()
	if code, body := r.confirm(t, card); code != http.StatusOK {
		t.Fatalf("the first Confirm: %d %s, want 200 (the second must not withdraw the card under it)", code, body)
	}
	if second != http.StatusConflict || secondCode != "not_open" {
		t.Fatalf("the second Confirm: %d %s, want 409 not_open", second, secondCode)
	}
	if got := r.status(t, r.uid, x); got.Status != attention.Answered || got.Answer != "yes" {
		t.Fatalf("the held call after a double Confirm: %+v, want answered yes", got)
	}
	if got := r.status(t, r.uid, card); got.Status != attention.Answered || got.Answer != "Confirm" {
		t.Fatalf("the card after a double Confirm: %+v, want answered Confirm (never withdrawn)", got)
	}
}

// TestTheBrowserOffersAPasskeyDecisionOnlyWhereOneCanWork: the card's page
// and the Asks feed draw Confirm and Refuse only for an account with a
// passkey and a card whose decision is pending; an account with none sees the
// phone line and where a passkey is added, and its options mint no challenge;
// a card whose decision a restart lost draws the phone line alone.
func TestTheBrowserOffersAPasskeyDecisionOnlyWhereOneCanWork(t *testing.T) {
	base := newStepUpRaceRig(t)
	base.fake.EnablePasskeys() // passkeys served, none on this account yet
	at, rt := base.login()
	r := &webRig{stepUpRaceRig: base, at: at, rt: rt}
	_, card := r.card(t, "Ship it, drawn?")
	page := func(path string) string {
		t.Helper()
		w := do(r.edge, http.MethodGet, path, nil, withCookies(r.at, r.rt), asNavigation)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", path, w.Code)
		}
		return w.Body.String()
	}
	askPage := "/attention/asks/" + strconv.FormatInt(card, 10)
	confirmBtn := `data-stepup-decide="Confirm"`

	// No passkey: the phone line and where one is added; no Confirm; and the
	// options mint no challenge.
	for _, path := range []string{askPage, "/attention"} {
		body := page(path)
		if strings.Contains(body, confirmBtn) || !strings.Contains(body, webPhoneLine) || !strings.Contains(body, `href="/account/passkeys"`) {
			t.Fatalf("%s for an account with no passkey: want the phone line, where a passkey is added, and no Confirm", path)
		}
	}
	before := r.fake.CountCalls(http.MethodPost, "/passkeys/authentication/options")
	if o := r.options(r.at, r.rt, card); o.Code != http.StatusOK || jsonBody(t, o)["available"] != false {
		t.Fatalf("options for an account with no passkey: %d %s, want available:false", o.Code, o.Body)
	}
	if n := r.fake.CountCalls(http.MethodPost, "/passkeys/authentication/options"); n != before {
		t.Fatal("options for an account with no passkey minted a challenge")
	}

	// A passkey added (muster forgets what it cached on an add: setPasskeyFlag).
	r.fake.AddPasskey(r.subject(), "owner-cred")
	r.s.stepUpCards.forget(r.subject())
	for _, path := range []string{askPage, "/attention"} {
		body := page(path)
		if !strings.Contains(body, webPasskeyLine) || !strings.Contains(body, confirmBtn) || !strings.Contains(body, `data-stepup-decide="Refuse"`) ||
			!strings.Contains(body, `data-path="/attention/asks/`+strconv.FormatInt(card, 10)+`/stepup/passkey"`) {
			t.Fatalf("%s for an account with a passkey draws no passkey Confirm/Refuse", path)
		}
	}
	// Rendering minted nothing: a challenge is fetched only when a button is
	// reached for.
	if n := r.fake.CountCalls(http.MethodPost, "/passkeys/authentication/options"); n != before {
		t.Fatalf("rendering the card minted %d challenge(s)", n-before)
	}

	// A lost decision (a restart): the phone line alone, even with a passkey.
	r.s.hostDecisions.take(card)
	if body := page(askPage); strings.Contains(body, confirmBtn) || !strings.Contains(body, webPhoneLine) {
		t.Fatal("a card whose decision was lost draws a passkey Confirm")
	}
}

// TestEveryPhoneRequestIsLoggedWithItsOutcome: one log line per challenge and
// per signed action, whatever its outcome — the refusal code, the action, the
// ask, the device, the owner and the latency — and none of the request's
// secrets: never the token, the nonce, the signed message, the signature or
// the answer's text.
func TestEveryPhoneRequestIsLoggedWithItsOutcome(t *testing.T) {
	a := newActionRig(t)
	logs := &lockedBuffer{}
	a.s.logger = log.New(logs, "", 0)
	id := a.askFor(t, a.uid, "Ship it, logged?")
	closed := a.askFor(t, a.uid, "Ship the closed one?")
	if _, err := a.attn.Cancel(asUser(a.uid), closed); err != nil {
		t.Fatal(err)
	}
	answer := "a very private answer, never in a log"
	nonce, msg := a.challenge(t, a.tok, actionAskAnswer, id, &answer)
	sig := sign(t, a.key, msg)
	if c, code := a.act(t, a.tok, "answer", id, map[string]string{"nonce": nonce, "signature": sig, "answer": answer}); c != http.StatusOK {
		t.Fatalf("the signed answer: %d %s", c, code)
	}
	nonce2, msg2 := a.challenge(t, a.tok, actionAskAnswer, closed, &answer)
	if c, code := a.act(t, a.tok, "answer", closed, map[string]string{"nonce": nonce2, "signature": sign(t, a.key, msg2), "answer": answer}); code != "not_open" {
		t.Fatalf("control: the closed ask's answer: %d %s", c, code)
	}
	nonce3, _ := a.challenge(t, a.tok, actionAskDismiss, id, nil)
	if _, code := a.act(t, a.tok, "cancel", id, map[string]string{"nonce": nonce3, "signature": sig}); code != "bad_signature" {
		t.Fatalf("control: a wrong signature: %s", code)
	}
	if w := a.call(a.edge, "POST", deviceChallengePath, a.tok, `{"action":"ask_answer","target":"nope"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("control: a bad challenge: %d", w.Code)
	}
	if w := a.call(a.edge, "POST", deviceChallengePath, "mdv_not-a-real-token", `{"action":"ask_answer","target":"1"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("control: a bad token: %d", w.Code)
	}
	out := logs.String()
	dev, owner := "device="+strconv.FormatInt(a.dev, 10), "owner="+strconv.FormatInt(a.uid, 10)
	ask, closedAsk := "ask="+strconv.FormatInt(id, 10), "ask="+strconv.FormatInt(closed, 10)
	want := [][]string{
		{"devices: challenge outcome=ok action=ask_answer", ask, dev, owner, "status=200"},
		{"devices: signed action outcome=ok action=ask_answer", ask, dev, owner, "status=200"},
		{"devices: challenge outcome=ok action=ask_answer", closedAsk, dev, owner},
		{"devices: signed action outcome=not_open action=ask_answer", closedAsk, dev, owner, "status=409"},
		{"devices: challenge outcome=ok action=ask_dismiss", ask, dev, owner},
		{"devices: signed action outcome=bad_signature action=ask_dismiss", ask, dev, owner, "status=401"},
		{"devices: challenge outcome=bad_request action=ask_answer", "ask=0", dev, owner, "status=400"},
		{"devices: challenge outcome=bad_token action=-", "ask=0", "device=0", "owner=0", "status=401"},
	}
	lines := strings.Split(out, "\n")
	for _, parts := range want {
		found := false
		for _, l := range lines {
			if !strings.HasPrefix(l, parts[0]+" ") || !strings.Contains(l, " in=") || !strings.HasSuffix(l, "ms") {
				continue
			}
			ok := true
			for _, p := range parts[1:] {
				if !strings.Contains(l, " "+p) {
					ok = false
				}
			}
			if ok {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no log line %v in:\n%s", parts, out)
		}
	}
	if n := strings.Count(out, "devices: challenge outcome="); n != 5 {
		t.Errorf("%d challenge lines, want 5 (one per request):\n%s", n, out)
	}
	if n := strings.Count(out, "devices: signed action outcome="); n != 3 {
		t.Errorf("%d signed action lines, want 3 (one per request):\n%s", n, out)
	}
	for _, secret := range []string{a.tok, nonce, nonce2, nonce3, sig, msg, answer, "very private"} {
		if strings.Contains(out, secret) {
			t.Errorf("the log carries a secret %q:\n%s", secret, out)
		}
	}
}
