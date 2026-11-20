package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/push"
)

// THE ONE-TAP CONFIRM OF A STEP-UP CARD, FROM ITS NOTIFICATION (host_stepup.go,
// device_actions.go; docs/mirroring.md "A machine that starts agents is
// step-up gated"): the card's push offers Confirm and Refuse as the signed
// ask_answer actions, the challenge for one hands the phone the card's own
// text to show in its step-up prompt, and only a signature by the paired
// phone's step-up key decides it.

// stepUpCardRig is an action rig on a machine the step-up gate covers (a host
// officer declared on it), whose owner's paired phone is sent payload v2.
type stepUpCardRig struct {
	*actionRig
	pusher *v2Pusher
	ctx    context.Context
	caller toolCaller
}

func newStepUpCardRig(t *testing.T) *stepUpCardRig {
	t.Helper()
	a := newActionRig(t)
	hm := newHostOfficerMirrorFor(a.uid)
	hm.list(hostOffHost, mirror.Window{ID: "@1", Session: "main", Claude: true, ClaudeSession: hostOffSess})
	a.s.ext.Mirror = hm
	a.s.ext.Agents = agents.NewPG(a.pool)
	declareHostOfficerOn(t, a.s, a.uid, hostOffHost)
	store, pusher := androidPush{&memPush{}}, &v2Pusher{}
	a.s.ext.Push = push.NewService(store, pusher, push.VAPIDConfig{PublicKey: "k"}, nil)
	claim(t, store, a.uid, ep("stepup-card-"+strconv.FormatInt(time.Now().UnixNano(), 36)))
	return &stepUpCardRig{actionRig: a, pusher: pusher,
		ctx:    authz.WithPrincipal(context.Background(), authz.Principal{UserID: a.uid, Role: authz.RoleAdmin, Via: authz.ViaMirrorHost}),
		caller: toolCaller{asker: attention.Asker{MirrorHost: hostOffHost, MirrorSession: hostOffSess, Label: "main:1"}, mirrorWindow: "@1"}}
}

// card files a step-up card for attention_answer of ask id with answer, as
// the machine's door would: its id.
func (r *stepUpCardRig) card(t *testing.T, id int64, answer string) int64 {
	t.Helper()
	out, ok := r.s.hostStepUpGate(r.ctx, hostOffHost, r.caller, attentionAnswerTool, `{"id":`+strconv.FormatInt(id, 10)+`,"answer":"`+answer+`"}`)
	if !ok {
		t.Fatal("not gated: the fixture's machine is not covered by the step-up gate")
	}
	return stepUpCardOf(t, out)
}

// challengeConfirms asks the phone's challenge for action on ask id (with
// text's hash, when given) and returns its nonce, message and `confirms`
// (whether the field was present at all, and its text).
func (r *stepUpCardRig) challengeConfirms(t *testing.T, tok, action string, id int64, text *string) (nonce, msg string, present bool, confirms string) {
	t.Helper()
	body := map[string]string{"action": action, "target": strconv.FormatInt(id, 10)}
	if text != nil {
		sum := sha256.Sum256([]byte(*text))
		body["text_sha256"] = hex.EncodeToString(sum[:])
	}
	b, _ := json.Marshal(body)
	w := r.call(r.edge, "POST", deviceChallengePath, tok, string(b))
	var raw map[string]json.RawMessage
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &raw) != nil {
		t.Fatalf("challenge %s %d: %d %s", action, id, w.Code, w.Body.String())
	}
	_ = json.Unmarshal(raw["nonce"], &nonce)
	_ = json.Unmarshal(raw["message"], &msg)
	if c, ok := raw["confirms"]; ok {
		present = true
		_ = json.Unmarshal(c, &confirms)
	}
	return nonce, msg, present, confirms
}

// TestAStepUpCardIsConfirmedFromItsNotification: the card's push carries
// Confirm and Refuse as signed ask_answer actions on the card; the phone's
// challenge for either hands back the card's own text (`confirms`), and an
// ordinary ask's challenge carries none; the phone's signature then runs the
// held decision once, through the real device route.
func TestAStepUpCardIsConfirmedFromItsNotification(t *testing.T) {
	r := newStepUpCardRig(t)
	x := r.askFor(t, r.uid, "Ship X?")
	card := r.card(t, x, "yes")
	r.s.WaitForPushes()

	// The push: the card's two answers, each a signed ask_answer on the card,
	// by index into its options.
	p := pushFor(t, r.pusher, card)
	if p.Ask == nil || p.Ask.ID != card || strings.Join(p.Ask.Options, "|") != hostDecisionConfirm+"|"+hostDecisionRefuse {
		t.Fatalf("the card's push chips: %+v; want the card's id and %q, %q", p.Ask, hostDecisionConfirm, hostDecisionRefuse)
	}
	answers := map[int]bool{}
	for _, ac := range p.Actions {
		if ac.ID == push.ActionAskAnswer && ac.Target == strconv.FormatInt(card, 10) && ac.Option != nil {
			answers[*ac.Option] = true
		}
	}
	if !answers[0] || !answers[1] {
		t.Fatalf("the card's push actions %+v; want ask_answer option 0 (Confirm) and option 1 (Refuse) on card %d", p.Actions, card)
	}

	// The challenge: the card's text, exactly as filed, for Confirm and for
	// Refuse alike.
	filed := r.status(t, r.uid, card)
	question := filed.Question
	if filed.StepUp == nil || filed.StepUp.Tool != attentionAnswerTool || !strings.Contains(filed.StepUp.Details, hostOffHost) {
		t.Fatalf("fixture: the card %+v does not name its tool and host", filed.StepUp)
	}
	refuse := hostDecisionRefuse
	if _, _, present, got := r.challengeConfirms(t, r.tok, actionAskAnswer, card, &refuse); !present || got != question {
		t.Fatalf("the Refuse challenge's confirms: present %v, %q; want the card's question %q", present, got, question)
	}
	confirm := hostDecisionConfirm
	nonce, msg, present, got := r.challengeConfirms(t, r.tok, actionAskAnswer, card, &confirm)
	if !present || got != question {
		t.Fatalf("the Confirm challenge's confirms: present %v, %q; want the card's question %q", present, got, question)
	}
	// Control: an ordinary ask's challenge says nothing of a card.
	yes := "yes"
	y := r.askFor(t, r.uid, "Ship Y?")
	if _, _, present, got := r.challengeConfirms(t, r.tok, actionAskAnswer, y, &yes); present {
		t.Fatalf("an ordinary ask's challenge carries confirms %q", got)
	}

	// The tap: the signed Confirm runs the held decision once.
	if code, rc := r.act(t, r.tok, "answer", card, map[string]string{"nonce": nonce, "signature": sign(t, r.key, msg), "answer": confirm}); code != http.StatusOK {
		t.Fatalf("the phone's signed Confirm: %d %s", code, rc)
	}
	if got := r.status(t, r.uid, x); got.Status != attention.Answered || got.Answer != "yes" {
		t.Fatalf("the held decision after the phone's Confirm: %+v; want answered yes", got)
	}
	if got := r.status(t, r.uid, card).Status; got != attention.Answered {
		t.Fatalf("the card after its Confirm: %s; want answered", got)
	}
	// Once a card is decided, its challenge carries no text any more.
	if _, _, present, _ := r.challengeConfirms(t, r.tok, actionAskAnswer, card, &confirm); present {
		t.Fatal("a decided card's challenge still carries confirms")
	}
}

// TestAStepUpCardIsRefusedFromItsNotification: Refuse, signed, runs nothing
// and closes the card.
func TestAStepUpCardIsRefusedFromItsNotification(t *testing.T) {
	r := newStepUpCardRig(t)
	x := r.askFor(t, r.uid, "Ship X?")
	card := r.card(t, x, "yes")
	refuse := hostDecisionRefuse
	nonce, msg, _, _ := r.challengeConfirms(t, r.tok, actionAskAnswer, card, &refuse)
	if code, rc := r.act(t, r.tok, "answer", card, map[string]string{"nonce": nonce, "signature": sign(t, r.key, msg), "answer": refuse}); code != http.StatusOK {
		t.Fatalf("the phone's signed Refuse: %d %s", code, rc)
	}
	if got := r.status(t, r.uid, x); got.Status != attention.Open {
		t.Fatalf("the held decision after a Refuse: %+v; want it never run (open)", got)
	}
	if got := r.status(t, r.uid, card); got.Status != attention.Answered || got.Answer != hostDecisionRefuse {
		t.Fatalf("the card after its Refuse: %+v", got)
	}
}

// TestAStepUpCardIsDecidedOnlyByThePairedPhonesKey: the negative cases. A
// signature by any key but the paired phone's step-up key, a phone paired
// read-only, the owner's signed-in browser (the Attention page's own answer
// route) and the host's door (the principal a host token acts under) each
// decide nothing, and the card stays open for the phone. Positive controls:
// the same browser route answers an ordinary ask, and the phone's own key then
// decides the card.
func TestAStepUpCardIsDecidedOnlyByThePairedPhonesKey(t *testing.T) {
	r := newStepUpCardRig(t)
	x := r.askFor(t, r.uid, "Ship X?")
	card := r.card(t, x, "yes")
	confirm := hostDecisionConfirm
	stillOpen := func(what string) {
		t.Helper()
		if got := r.status(t, r.uid, x); got.Status != attention.Open {
			t.Fatalf("%s ran the held decision: %+v", what, got)
		}
		if got := r.status(t, r.uid, card); got.Status != attention.Open {
			t.Fatalf("%s closed the card: %+v", what, got)
		}
	}

	// 1. Another key: the challenge is the phone's own, the signature is not.
	stranger, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	nonce, msg, _, _ := r.challengeConfirms(t, r.tok, actionAskAnswer, card, &confirm)
	if code, rc := r.act(t, r.tok, "answer", card, map[string]string{"nonce": nonce, "signature": sign(t, stranger, msg), "answer": confirm}); code != http.StatusUnauthorized || rc != actionCodeBadSignature {
		t.Fatalf("a Confirm signed by another key: %d %s; want 401 %s", code, rc, actionCodeBadSignature)
	}
	stillOpen("a Confirm signed by another key")

	// 2. A phone paired read-only (no step-up key): no challenge at all.
	at, rt := r.login()
	ro, _ := r.pair(at, rt, "read-only phone")
	sum := sha256.Sum256([]byte(confirm))
	b, _ := json.Marshal(map[string]string{"action": actionAskAnswer, "target": strconv.FormatInt(card, 10), "text_sha256": hex.EncodeToString(sum[:])})
	if w := r.call(r.edge, "POST", deviceChallengePath, ro, string(b)); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), actionCodeNoKey) {
		t.Fatalf("a read-only phone's challenge for the card: %d %s; want 403 %s", w.Code, w.Body.String(), actionCodeNoKey)
	}
	stillOpen("a read-only phone")

	// 3. The owner's signed-in browser: the Attention page's answer route.
	// Positive control first: the same route answers an ordinary ask.
	y := r.askFor(t, r.uid, "Ship Y?")
	browser := func(id int64, answer string) {
		do(r.edge, "POST", attentionPath+"/asks/"+strconv.FormatInt(id, 10)+"/answer", url.Values{"answer": {answer}}, withCookies(at, rt))
	}
	browser(y, "yes")
	if got := r.status(t, r.uid, y); got.Status != attention.Answered {
		t.Fatalf("control: the browser's answer route did not answer an ordinary ask (%+v): the instrument cannot see a browser answer", got)
	}
	browser(card, confirm)
	stillOpen("the browser's Confirm")
	if _, err := r.s.answerAskFrom(asUser(r.uid), card, confirm, attention.AnswerSource{}); err == nil || !strings.Contains(err.Error(), "paired phone") {
		t.Fatalf("a signed-in session's Confirm: %v; want refused for the paired phone", err)
	}
	stillOpen("a signed-in session's Confirm")

	// 4. The host's door: the principal a host token's calls act under.
	if _, err := r.s.answerAskFrom(r.ctx, card, confirm, attention.AnswerSource{Host: hostOffHost, Session: hostOffSess}); err == nil || !strings.Contains(err.Error(), "paired phone") {
		t.Fatalf("the host door's Confirm: %v; want refused for the paired phone", err)
	}
	stillOpen("the host door's Confirm")

	// The phone's own key decides it (a fresh challenge: every refused one
	// above was spent).
	nonce, msg, _, _ = r.challengeConfirms(t, r.tok, actionAskAnswer, card, &confirm)
	if code, rc := r.act(t, r.tok, "answer", card, map[string]string{"nonce": nonce, "signature": sign(t, r.key, msg), "answer": confirm}); code != http.StatusOK {
		t.Fatalf("the phone's own signed Confirm: %d %s", code, rc)
	}
	if got := r.status(t, r.uid, x); got.Status != attention.Answered || got.Answer != "yes" {
		t.Fatalf("the held decision after the phone's Confirm: %+v", got)
	}
}

// TestAStepUpCardsTextIsOnlyItsOwnersPhones: stepUpCardConfirms hands the
// card's text only to a principal of the card's owner; another user's phone
// (and a card that is not pending) gets none.
func TestAStepUpCardsTextIsOnlyItsOwnersPhones(t *testing.T) {
	r := newStepUpCardRig(t)
	x := r.askFor(t, r.uid, "Ship X?")
	card := r.card(t, x, "yes")
	target := strconv.FormatInt(card, 10)
	if got := r.s.stepUpCardConfirms(asDevice(r.uid), target); got == "" {
		t.Fatal("control: the owner's phone gets no card text")
	}
	if got := r.s.stepUpCardConfirms(asDevice(r.uid+1000), target); got != "" {
		t.Fatalf("another user's phone got the card's text: %q", got)
	}
	if got := r.s.stepUpCardConfirms(asDevice(r.uid), strconv.FormatInt(x, 10)); got != "" {
		t.Fatalf("an ordinary ask got card text: %q", got)
	}
}
