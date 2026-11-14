package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
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
