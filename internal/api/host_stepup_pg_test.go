package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/devices"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers"
)

// THE STEP-UP GATE OVER POSTGRES (host_stepup.go): owner-authority tools
// called through the REAL mirror door (POST /mirror/mcp), the caller named by
// the header a process on the machine sets — here the owner's own window,
// which is exactly what an officer impersonating it would send.

// declareHostOfficerOn turns the officer runtime on in srv with one host
// officer of owner on host (an HQ-backed fixture officer: the gate reads the
// roster only).
func declareHostOfficerOn(t *testing.T, srv *Server, owner int64, host string) {
	t.Helper()
	if srv.ext.Agents == nil {
		t.Fatal("fixture: no agents store")
	}
	o := officers.Officer{ID: "marketing:stepup", HQURL: "https://hq.example.invalid/api/v1", MaxInstances: 1, IdleAfter: time.Hour,
		PollInterval: time.Minute, Owner: owner, Runtime: officers.RuntimeHost, Host: host, Repo: hostOffRepo}.WithToken("fixture-stepup-token")
	srv.officerRT = nil
	if _, err := srv.EnableOfficers(OfficersConfig{Officers: []officers.Officer{o}, HostSpawns: officers.NewMemHostSpawnStore()}); err != nil {
		t.Fatal(err)
	}
}

// cardOf reads the card id out of a gated tool's answer.
func stepUpCardOf(t *testing.T, text string) int64 {
	t.Helper()
	var out struct {
		Status string `json:"status"`
		Card   int64  `json:"card"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.Status != "awaiting_owner_confirmation" || out.Card == 0 {
		t.Fatalf("not a step-up card: %s", text)
	}
	return out.Card
}

// asDevice is owner's paired phone's principal (what the signed-action route
// answers under once the signature is verified).
func asDevice(owner int64) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: owner, Role: authz.RoleAdmin, Via: authz.ViaDevice})
}

// TestAnImpersonatedConnectorDecisionNeedsThePhone: on a machine running a host
// officer, connector_request_decide and attention_answer through the mirror
// door — naming the owner's own window — decide nothing: each files a card.
// The browser and the MCP door cannot answer the card; the owner's phone can,
// and then the decision runs once, as the session that asked. Refuse runs
// nothing. The same call from the owner's OTHER machine (no host officer)
// decides at once.
func TestAnImpersonatedConnectorDecisionNeedsThePhone(t *testing.T) {
	f := newDecideFixture(t)
	declareHostOfficerOn(t, f.srv, 1, "ws")
	id := f.request(t, f.opTok, "svc", "to read the widget API")

	text, isErr := f.decide(t, f.ws, focusSID, id, "approve")
	if isErr {
		t.Fatalf("decide on the host officer's machine: %s", text)
	}
	card := stepUpCardOf(t, text)
	if g := f.grantRow(t, id); g.status != "pending" {
		t.Fatalf("the grant after an ungated-looking decide: %+v, want still pending", g)
	}
	// The browser cannot answer the card; nor can an MCP door.
	if _, err := f.srv.answerAskFrom(asUser(1), card, hostDecisionConfirm, attention.AnswerSource{}); !errors.Is(err, errHostDecisionNeedsPhone) {
		t.Fatalf("a browser answer of the card: %v", err)
	}
	if text, _ := f.answerAs(t, f.laptop, laptopSID, card, hostDecisionConfirm); !strings.Contains(text, "paired phone") {
		t.Fatalf("an MCP answer of the card from another machine: %s", text)
	}
	if g := f.grantRow(t, id); g.status != "pending" {
		t.Fatalf("the grant after refused answers: %+v", g)
	}
	// The phone: the decision runs, once, as the session that asked.
	if _, err := f.srv.answerAskFrom(asDevice(1), card, hostDecisionConfirm, attention.AnswerSource{}); err != nil {
		t.Fatalf("the phone's confirm: %v", err)
	}
	if g := f.grantRow(t, id); g.status != "approved" || g.via != "mcp" || g.host != "ws" || g.session != focusSID {
		t.Fatalf("the grant after the phone's confirm: %+v", g)
	}
	if _, err := f.srv.answerAskFrom(asDevice(1), card, hostDecisionConfirm, attention.AnswerSource{}); err == nil {
		t.Fatal("the card was answered twice")
	}

	// attention_answer: the same gate; Refuse runs nothing.
	ask := f.askAs(t, attention.Asker{AgentID: f.op.ID, Label: f.op.Name}, "Ship the widget?")
	text, _ = f.answerAs(t, f.ws, focusSID, ask.ID, "yes")
	card2 := stepUpCardOf(t, text)
	if _, err := f.srv.answerAskFrom(asDevice(1), card2, hostDecisionRefuse, attention.AnswerSource{}); err != nil {
		t.Fatalf("the phone's refuse: %v", err)
	}
	if got := f.askStatus(t, ask.ID); got.Status != attention.Open {
		t.Fatalf("a refused card answered the ask: %+v", got)
	}
	text, _ = f.answerAs(t, f.ws, focusSID, ask.ID, "yes")
	card3 := stepUpCardOf(t, text)
	if _, err := f.srv.answerAskFrom(asDevice(1), card3, hostDecisionConfirm, attention.AnswerSource{}); err != nil {
		t.Fatalf("the phone's confirm: %v", err)
	}
	if got := f.askStatus(t, ask.ID); got.Status != attention.Answered || got.Answer != "yes" {
		t.Fatalf("the ask after the confirm: %+v", got)
	}

	// The owner's other machine runs no host officer: unaffected. (Another
	// connector: an approval of svc is shared, so a second svc request would
	// be approved by the first decision.)
	if _, err := f.store.CreateConnector(f.ctx, connectors.NewConnector{Name: "svc2", Hosts: []string{"api.example.com"},
		Scheme: connectors.SchemeBearer, Secret: connTestSecret}); err != nil {
		t.Fatal(err)
	}
	id2 := f.request(t, f.op2Tok, "svc2", "control")
	if text, isErr := f.decide(t, f.laptop, laptopSID, id2, "approve"); isErr || !strings.Contains(text, `"status":"approved"`) {
		t.Fatalf("decide from a machine with no host officer: %s", text)
	}
}

// TestAnImpersonatedDraftApprovalNeedsThePhone: plugin_draft_decide through the
// mirror door of a host officer's machine files a card and leaves the draft
// pending; the phone's confirm approves exactly that draft; the Asks page in
// the browser decides as before, ungated.
func TestAnImpersonatedDraftApprovalNeedsThePhone(t *testing.T) {
	f := newDraftToolFixture(t)
	f.srv.ext.Attention = attention.NewPG(f.pool)
	body := "Thanks — it ships Friday."
	// The drafts are made before the officer is declared: route_reply is an
	// owner-authority tool too, and would be gated.
	id, _ := f.routedDraft(t, "x-stepup-1", body)
	id2, _ := f.routedDraft(t, "x-stepup-2", "Another reply.")
	declareHostOfficerOn(t, f.srv, f.owner, "tower")
	text, isErr := f.decide(t, f.tok, otherRouteSID, id, "approve", sha(body))
	if isErr {
		t.Fatalf("decide: %s", text)
	}
	card := stepUpCardOf(t, text)
	if r := f.row(t, id); r.status != "pending" {
		t.Fatalf("the draft after the gated decide: %+v", r)
	}
	devCtx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.owner, Role: authz.RoleAdmin, Via: authz.ViaDevice})
	if _, err := f.srv.answerAskFrom(devCtx, card, hostDecisionConfirm, attention.AnswerSource{}); err != nil {
		t.Fatalf("the phone's confirm: %v", err)
	}
	if r := f.row(t, id); r.status != "approved" {
		t.Fatalf("the draft after the phone's confirm: %+v", r)
	}
	// The browser's own decision is unchanged.
	if w := f.browser(t, id2, "approve"); w.Code >= 400 {
		t.Fatalf("the Asks page's approve: %d %s", w.Code, w.Body)
	}
	if r := f.row(t, id2); r.status != "approved" {
		t.Fatalf("the draft after the browser's approve: %+v", r)
	}
}

// TestAStepUpForOneDecisionCannotBeReplayedForAnother: two cards, A and B. The
// phone's signed confirm of A — challenge bound to A's id and the exact answer
// — presented for B is refused, and B's decision does not run; A's runs.
func TestAStepUpForOneDecisionCannotBeReplayedForAnother(t *testing.T) {
	a := newActionRig(t)
	hm := newHostOfficerMirrorFor(a.uid)
	// The owner's own (untagged) window the impersonating call names.
	hm.list(hostOffHost, mirror.Window{ID: "@1", Session: "main", Claude: true, ClaudeSession: hostOffSess})
	a.s.ext.Mirror = hm
	a.s.ext.Agents = agents.NewPG(a.pool)
	declareHostOfficerOn(t, a.s, a.uid, hostOffHost)
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: a.uid, Role: authz.RoleAdmin, Via: authz.ViaMirrorHost})
	c := toolCaller{asker: attention.Asker{MirrorHost: hostOffHost, MirrorSession: hostOffSess, Label: "main:1"}, mirrorWindow: "@1"}
	x, y := a.askFor(t, a.uid, "Ship X?"), a.askFor(t, a.uid, "Ship Y?")
	gate := func(id int64) int64 {
		out, ok := a.s.hostStepUpGate(ctx, hostOffHost, c, attentionAnswerTool, `{"id":`+strconv.FormatInt(id, 10)+`,"answer":"yes"}`)
		if !ok {
			t.Fatal("not gated")
		}
		return stepUpCardOf(t, out)
	}
	cardA, cardB := gate(x), gate(y)
	confirm := hostDecisionConfirm
	nonce, msg := a.challenge(t, a.tok, actionAskAnswer, cardA, &confirm)
	body := map[string]string{"nonce": nonce, "signature": sign(t, a.key, msg), "answer": confirm}
	if code, _ := a.act(t, a.tok, "answer", cardB, body); code == http.StatusOK {
		t.Fatal("A's signed confirm was accepted for B")
	}
	if got := a.status(t, a.uid, y); got.Status != attention.Open {
		t.Fatalf("B's decision ran on A's signature: %+v", got)
	}
	// The challenge was spent by the refused attempt: A needs a new one.
	nonce, msg = a.challenge(t, a.tok, actionAskAnswer, cardA, &confirm)
	if code, rc := a.act(t, a.tok, "answer", cardA, map[string]string{"nonce": nonce, "signature": sign(t, a.key, msg), "answer": confirm}); code != http.StatusOK {
		t.Fatalf("A's own signed confirm: %d %s", code, rc)
	}
	if got := a.status(t, a.uid, x); got.Status != attention.Answered || got.Answer != "yes" {
		t.Fatalf("A's decision after its confirm: %+v", got)
	}
	if got := a.status(t, a.uid, y); got.Status != attention.Open {
		t.Fatalf("B's decision ran with A's: %+v", got)
	}
	if got := a.status(t, a.uid, cardB); got.Status != attention.Open {
		t.Fatalf("card B was answered: %+v", got)
	}
	_ = devices.KeyBiometric
}

// TestADismissedStepUpCardRunsNothingOnALaterPhoneConfirm: a card dismissed
// from the browser (the Asks dismiss route's one call, cancelAsk) or from the
// paired phone's own dismiss action is closed, and the decision it held is
// dropped: a phone Confirm signed after that is refused as not open (409),
// and the call the card held never runs — the ask it would have answered
// stays open.
func TestADismissedStepUpCardRunsNothingOnALaterPhoneConfirm(t *testing.T) {
	a := newActionRig(t)
	hm := newHostOfficerMirrorFor(a.uid)
	hm.list(hostOffHost, mirror.Window{ID: "@1", Session: "main", Claude: true, ClaudeSession: hostOffSess})
	a.s.ext.Mirror = hm
	a.s.ext.Agents = agents.NewPG(a.pool)
	declareHostOfficerOn(t, a.s, a.uid, hostOffHost)
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: a.uid, Role: authz.RoleAdmin, Via: authz.ViaMirrorHost})
	c := toolCaller{asker: attention.Asker{MirrorHost: hostOffHost, MirrorSession: hostOffSess, Label: "main:1"}, mirrorWindow: "@1"}
	gate := func(id int64) int64 {
		out, ok := a.s.hostStepUpGate(ctx, hostOffHost, c, attentionAnswerTool, `{"id":`+strconv.FormatInt(id, 10)+`,"answer":"yes"}`)
		if !ok {
			t.Fatal("not gated")
		}
		return stepUpCardOf(t, out)
	}
	confirm := hostDecisionConfirm
	phoneConfirm := func(card int64) (int, string) {
		nonce, msg := a.challenge(t, a.tok, actionAskAnswer, card, &confirm)
		return a.act(t, a.tok, "answer", card, map[string]string{"nonce": nonce, "signature": sign(t, a.key, msg), "answer": confirm})
	}

	for _, door := range []string{"browser", "phone"} {
		t.Run(door, func(t *testing.T) {
			x := a.askFor(t, a.uid, "Ship it from the "+door+"?")
			card := gate(x)
			switch door {
			case "browser":
				if _, err := a.s.cancelAsk(asUser(a.uid), card); err != nil {
					t.Fatalf("dismiss the card: %v", err)
				}
			case "phone":
				nonce, msg := a.challenge(t, a.tok, actionAskDismiss, card, nil)
				if code, body := a.act(t, a.tok, "cancel", card, map[string]string{"nonce": nonce, "signature": sign(t, a.key, msg)}); code != http.StatusOK {
					t.Fatalf("the phone's dismiss: %d %s", code, body)
				}
			}
			if got := a.status(t, a.uid, card); got.Status != attention.Cancelled {
				t.Fatalf("the card after its dismissal: %+v", got)
			}
			code, body := phoneConfirm(card)
			if code != http.StatusConflict || !strings.Contains(body, actionCodeNotOpen) {
				t.Fatalf("a phone confirm of a dismissed card: %d %s, want 409 %s", code, body, actionCodeNotOpen)
			}
			if got := a.status(t, a.uid, x); got.Status != attention.Open || got.Answer != "" {
				t.Fatalf("the dismissed card's decision ran: the ask it held is %+v, want still open", got)
			}
			if _, ok := a.s.hostDecisions.peek(card); ok {
				t.Fatal("the dismissed card's decision is still pending")
			}
		})
	}
}

// flakyAttention is the real attention store with its Get or Answer made to
// fail, or a dismissal slipped in before an Answer — what a database blip, or
// a browser Dismiss racing the phone's Confirm, does to a step-up card.
type flakyAttention struct {
	attention.Store
	mu           sync.Mutex
	getErr       error   // the next Get fails with it (once)
	answerErr    error   // the next Answer fails with it (once)
	beforeAnswer func()  // runs (once) before the next Answer
	answered     []int64 // every id an Answer reached the store for
}

func (f *flakyAttention) Get(ctx context.Context, id int64) (attention.Ask, error) {
	f.mu.Lock()
	err := f.getErr
	f.getErr = nil
	f.mu.Unlock()
	if err != nil {
		return attention.Ask{}, err
	}
	return f.Store.Get(ctx, id)
}

func (f *flakyAttention) Answer(ctx context.Context, id int64, answer string) (attention.Ask, error) {
	f.mu.Lock()
	err, before := f.answerErr, f.beforeAnswer
	f.answerErr, f.beforeAnswer = nil, nil
	f.answered = append(f.answered, id)
	f.mu.Unlock()
	if before != nil {
		before()
	}
	if err != nil {
		return attention.Ask{}, err
	}
	return f.Store.Answer(ctx, id, answer)
}

// stepUpRaceRig is one owner's step-up gate over Postgres, its attention store
// the flaky one: card files a card holding an attention_answer of a new ask
// (returned with it), and confirm is a signed phone Confirm of a card.
type stepUpRaceRig struct {
	*actionRig
	flaky *flakyAttention
}

func newStepUpRaceRig(t *testing.T) *stepUpRaceRig {
	a := newActionRig(t)
	hm := newHostOfficerMirrorFor(a.uid)
	hm.list(hostOffHost, mirror.Window{ID: "@1", Session: "main", Claude: true, ClaudeSession: hostOffSess})
	a.s.ext.Mirror = hm
	a.s.ext.Agents = agents.NewPG(a.pool)
	declareHostOfficerOn(t, a.s, a.uid, hostOffHost)
	f := &flakyAttention{Store: a.attn}
	a.s.ext.Attention = f
	return &stepUpRaceRig{actionRig: a, flaky: f}
}

func (r *stepUpRaceRig) card(t *testing.T, q string) (ask, card int64) {
	t.Helper()
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: r.uid, Role: authz.RoleAdmin, Via: authz.ViaMirrorHost})
	c := toolCaller{asker: attention.Asker{MirrorHost: hostOffHost, MirrorSession: hostOffSess, Label: "main:1"}, mirrorWindow: "@1"}
	ask = r.askFor(t, r.uid, q)
	out, ok := r.s.hostStepUpGate(ctx, hostOffHost, c, attentionAnswerTool, `{"id":`+strconv.FormatInt(ask, 10)+`,"answer":"yes"}`)
	if !ok {
		t.Fatal("not gated")
	}
	return ask, stepUpCardOf(t, out)
}

func (r *stepUpRaceRig) confirm(t *testing.T, card int64) (int, string) {
	t.Helper()
	confirm := hostDecisionConfirm
	nonce, msg := r.challenge(t, r.tok, actionAskAnswer, card, &confirm)
	return r.act(t, r.tok, "answer", card, map[string]string{"nonce": nonce, "signature": sign(t, r.key, msg), "answer": confirm})
}

// TestAStepUpCardsDecisionSurvivesAFailedRead: a Confirm whose read of the
// card FAILS (not "not open", not "not found": the database) says nothing
// about the card, so it drops nothing — the decision stays pending, the card
// and the ask it holds stay open, and the owner's next Confirm runs it.
// RED before: `err != nil || card.Status != Open` dropped the decision of an
// OPEN card on any read error, and the next Confirm was "lost".
func TestAStepUpCardsDecisionSurvivesAFailedRead(t *testing.T) {
	r := newStepUpRaceRig(t)
	x, card := r.card(t, "Ship it once the read recovers?")
	r.flaky.mu.Lock()
	r.flaky.getErr = errors.New("conn reset by peer")
	r.flaky.mu.Unlock()
	handled, _, err := r.s.hostDecisionForAnswer(asDevice(r.uid), card, hostDecisionConfirm)
	if !handled || err == nil || errors.Is(err, attention.ErrNotOpen) || !strings.Contains(err.Error(), "conn reset by peer") {
		t.Errorf("a Confirm whose card read failed: handled %v, %v; want the read's own error, never not-open", handled, err)
	}
	if _, ok := r.s.hostDecisions.peek(card); !ok {
		t.Fatal("a failed read of an OPEN card dropped its decision")
	}
	if got := r.status(t, r.uid, card); got.Status != attention.Open {
		t.Fatalf("the card after the failed read: %+v, want open", got)
	}
	if got := r.status(t, r.uid, x); got.Status != attention.Open {
		t.Fatalf("the held call ran on a failed read: %+v", got)
	}
	if code, body := r.confirm(t, card); code != http.StatusOK {
		t.Fatalf("the next Confirm: %d %s, want 200", code, body)
	}
	if got := r.status(t, r.uid, x); got.Status != attention.Answered || got.Answer != "yes" {
		t.Fatalf("the next Confirm did not run the held call: %+v", got)
	}
	if got := r.status(t, r.uid, card); got.Status != attention.Answered || got.Answer != hostDecisionConfirm {
		t.Fatalf("the card after the Confirm: %+v", got)
	}
}

// TestAStepUpCardIsClaimedBeforeItsDecisionRuns: the card is answered (the
// store's open-only update) BEFORE its call runs. A browser Dismiss that lands
// between the phone's read and that claim wins: the phone is told not open
// AND nothing ran. One that lands after the claim is itself refused as not
// open, and the call ran with the card saying Confirm. An Answer that fails
// for another reason runs nothing and leaves the decision pending.
// RED before: the call ran first and the answer was recorded after it, so a
// Dismiss in between left the phone a 409 for a call that HAD run, and a
// failed record ran it anyway.
func TestAStepUpCardIsClaimedBeforeItsDecisionRuns(t *testing.T) {
	r := newStepUpRaceRig(t)

	x, card := r.card(t, "Ship it, dismissed mid-confirm?")
	var dismissErr error
	r.flaky.mu.Lock()
	r.flaky.beforeAnswer = func() { _, dismissErr = r.s.cancelAsk(asUser(r.uid), card) }
	r.flaky.mu.Unlock()
	code, body := r.confirm(t, card)
	if dismissErr != nil {
		t.Fatalf("control: the racing dismissal: %v", dismissErr)
	}
	if code != http.StatusConflict || body != actionCodeNotOpen {
		t.Errorf("a Confirm the dismissal beat: %d %s, want 409 %s", code, body, actionCodeNotOpen)
	}
	if got := r.status(t, r.uid, x); got.Status != attention.Open || got.Answer != "" {
		t.Errorf("the phone was told not open, but the held call RAN: %+v", got)
	}
	if _, ok := r.s.hostDecisions.peek(card); ok {
		t.Error("the dismissed card's decision is still pending")
	}

	x, card = r.card(t, "Ship it, dismissed after the claim?")
	if code, body := r.confirm(t, card); code != http.StatusOK {
		t.Fatalf("a Confirm: %d %s", code, body)
	}
	if _, err := r.s.cancelAsk(asUser(r.uid), card); !errors.Is(err, attention.ErrNotOpen) {
		t.Errorf("a dismissal after the claim: %v, want not open", err)
	}
	if got := r.status(t, r.uid, x); got.Status != attention.Answered || got.Answer != "yes" {
		t.Errorf("the confirmed call: %+v", got)
	}
	if got := r.status(t, r.uid, card); got.Status != attention.Answered || got.Answer != hostDecisionConfirm {
		t.Errorf("the claimed card: %+v", got)
	}

	x, card = r.card(t, "Ship it, its answer unrecorded?")
	r.flaky.mu.Lock()
	r.flaky.answerErr = errors.New("conn reset by peer")
	r.flaky.mu.Unlock()
	if code, _ := r.confirm(t, card); code == http.StatusOK {
		t.Errorf("a Confirm whose answer was not recorded: %d", code)
	}
	if got := r.status(t, r.uid, x); got.Status != attention.Open {
		t.Errorf("an unrecorded Confirm ran the held call: %+v", got)
	}
	if _, ok := r.s.hostDecisions.peek(card); !ok {
		t.Fatal("an unrecorded Confirm dropped the decision")
	}
	if code, body := r.confirm(t, card); code != http.StatusOK {
		t.Fatalf("the next Confirm: %d %s", code, body)
	}
	if got := r.status(t, r.uid, x); got.Status != attention.Answered || got.Answer != "yes" {
		t.Errorf("the next Confirm did not run the held call: %+v", got)
	}
}

// TestARetriedConfirmOfAClosedCardSaysItIsClosed: once a closed card's
// Confirm dropped its decision, a second Confirm of it is refused as CLOSED
// (not open), never "lost when muster restarted" — and withdraws nothing.
// RED before: the retry fell into the lost-card branch.
func TestARetriedConfirmOfAClosedCardSaysItIsClosed(t *testing.T) {
	r := newStepUpRaceRig(t)
	x, card := r.card(t, "Ship it, confirmed twice?")
	if _, err := r.s.cancelAsk(asUser(r.uid), card); err != nil {
		t.Fatal(err)
	}
	if code, body := r.confirm(t, card); code != http.StatusConflict || body != actionCodeNotOpen {
		t.Fatalf("control: the first Confirm of the dismissed card: %d %s", code, body)
	}
	if _, ok := r.s.hostDecisions.peek(card); ok {
		t.Fatal("control: the decision was not dropped")
	}
	_, _, err := r.s.hostDecisionForAnswer(asDevice(r.uid), card, hostDecisionConfirm)
	if !errors.Is(err, errHostDecisionClosed) || errors.Is(err, errHostDecisionLost) {
		t.Errorf("the retried Confirm: %v, want %v", err, errHostDecisionClosed)
	}
	if code, body := r.confirm(t, card); code != http.StatusConflict || body != actionCodeNotOpen {
		t.Errorf("the retried phone Confirm: %d %s, want 409 %s", code, body, actionCodeNotOpen)
	}
	if got := r.status(t, r.uid, x); got.Status != attention.Open {
		t.Errorf("a closed card's call ran: %+v", got)
	}
}
