package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// TWO PROBES OF THE OPERATOR'S ASK (2026-11-22: "rejections as well as any
// response to an ask [go] back to the asking session"), written against
// nothing but what muster had before this change — the mirror door, an
// officer's draft tools, the Asks page's Reject, the step-up gate, the phone's
// answer, the send queue's dispatcher — and the session-wake fixtures of
// session_wake_probe_pg_test.go. Each decision must reach the session that
// ASKED for the work as one paste:
//
//   - a session enqueues an officer item through its own MCP door, the officer
//     files a draft for it, the owner rejects it with a reason (the measured
//     case: officer item 223 → draft 476 rejected → nothing reached the
//     session);
//   - a session's owner-authority call files a step-up card, the owner
//     confirms it on their phone.
//
// At a build without request wakes both are red with "took 0 pastes".

// officerWakeWorld is an outbound world (an officer, its bound plugin, the
// owner user 1) whose owner's host "tower" mirrors here, with one Claude Code
// window (@4) running routeSID, and session wakes on.
type officerWakeWorld struct {
	*outboundWorld
	host    *wakeMirror
	queue   *sendqueue.PGStore
	hostTok string
}

const officerWakeHostTok = "mmt_" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" + "w"

func newOfficerWakeWorld(t *testing.T) *officerWakeWorld {
	t.Helper()
	w := newOutboundWorld(t, true)
	host := &wakeMirror{routeMirror: &routeMirror{interactive: true, statusReport: true}, owner: 1, sid: routeSID,
		since: time.Now().Add(-time.Hour).UnixMilli()}
	q := sendqueue.NewPG(w.f.pool)
	w.srv.ext.SendQueue = q
	w.srv.ext.MirrorTokens = fakeMirrorTokens{officerWakeHostTok: mirror.Token{ID: 91, OwnerID: 1, Host: "tower"}}
	wakesOn(w.srv, w.f.pool, host, attention.NewPG(w.f.pool), pages.NewPG(w.f.pool))
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM session_wakes WHERE owner_id = 1`, `DELETE FROM send_queue WHERE owner_id = 1`} {
			_, _ = w.f.pool.Exec(sys, q)
		}
	})
	return &officerWakeWorld{outboundWorld: w, host: host, queue: q, hostTok: officerWakeHostTok}
}

// mirrorCall is one tools/call through the REAL mirror door, as host tower's
// muster-mirror forwards it for the session in window @4 (sid).
func (w *officerWakeWorld) mirrorCall(sid, tool string, args any) string {
	w.t.Helper()
	rec := w.do("POST", "/mirror/mcp", mcpToolCall(tool, args), func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+w.hostTok)
		r.Header.Set(mirrorSessionHeader, sid)
	})
	var out struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || len(out.Result.Content) == 0 {
		return "transport: " + strconv.Itoa(rec.Code) + " " + rec.Body.String()
	}
	if out.Result.IsError {
		return "tool error: " + out.Result.Content[0].Text
	}
	return out.Result.Content[0].Text
}

// draftForItem has the officer's instance claim item and file body for it,
// checked sendable: the draft's id.
func (w *officerWakeWorld) draftForItem(item int64, body string) int64 {
	w.t.Helper()
	if out := w.srv.dispatchAgentTool(w.ctx, w.agent, officers.ToolClaim, `{"item_id":"`+strconv.FormatInt(item, 10)+`"}`); isToolErr(out) {
		w.t.Fatalf("claim: %s", out)
	}
	b, _ := json.Marshal(body)
	id, out := w.postDraft(`{"plugin":"` + outboundPluginName + `","venue":"example.forum","target":{"kind":"post"},"body":` + string(b) +
		`,"item_id":"` + strconv.FormatInt(item, 10) + `"}`)
	if id == 0 {
		w.t.Fatalf("file: %s", out)
	}
	if rec := w.check(w.outboundWorld.tok, id, true, ""); rec.Code != http.StatusOK {
		w.t.Fatalf("check: %d %s", rec.Code, rec.Body.String())
	}
	return id
}

// sessionEnqueues is the session's enqueue of an owner plan for the officer,
// through its own MCP door (officer_item_enqueue). The item's id.
func (w *officerWakeWorld) sessionEnqueues(subject string) int64 {
	w.t.Helper()
	out := w.mirrorCall(routeSID, "officer_item_enqueue", map[string]any{"officer": builtinOfficerID, "kind": "owner.plan", "subject_key": subject})
	var got struct {
		ID int64 `json:"id"`
	}
	if json.Unmarshal([]byte(out), &got) != nil || got.ID <= 0 {
		w.t.Fatalf("officer_item_enqueue from the session's door: %s", out)
	}
	return got.ID
}

// tick runs the deliveries muster started, one attention sweep's wake steps
// and one send-queue pass, twice.
func (w *officerWakeWorld) tick() {
	w.t.Helper()
	for range 2 {
		w.srv.WaitForAttentionDeliveries()
		probeSweep(w.srv)
		w.srv.WaitForAttentionDeliveries()
		if _, err := w.srv.newSendQueueDispatcher().Tick(context.Background()); err != nil {
			w.t.Fatal(err)
		}
	}
}

func TestWakeProbeARejectedOfficerDraftReachesTheSessionThatEnqueuedItsItem(t *testing.T) {
	w := newOfficerWakeWorld(t)
	item := w.sessionEnqueues("plan-probe")
	id := w.draftForItem(item, "A post the owner turns down.")
	if rec := w.rejectForm(id, "not this week, try Tuesday"); rec.Code != http.StatusSeeOther {
		t.Fatalf("reject: %d %s", rec.Code, rec.Body.String())
	}
	w.tick()
	probeOne(t, w.host, "not this week, try Tuesday")
	if strings.Contains(w.host.taken()[0].text, "A post the owner turns down.") {
		t.Fatalf("the draft's body reached the session: %s", w.host.taken()[0].text)
	}
}

func TestWakeProbeAConfirmedStepUpCardReachesTheSessionThatRaisedIt(t *testing.T) {
	f, m, att, _ := probeFixture(t)
	declareHostOfficerOn(t, f.srv, f.owner, "tower")
	// The ask the session's attention_answer answers: a session's on the
	// owner's OTHER host (no session answers an ask raised on its own host),
	// which no window here shows — its own wake waits, and the window takes
	// nothing for it.
	x, err := att.Create(probeOwner(f), attention.New{Asker: attention.Asker{MirrorHost: "laptop", MirrorSession: "0ther-session-of-the-owner"}, Question: "Go?"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.owner, Role: authz.RoleMember, Via: authz.ViaMirrorHost})
	c := toolCaller{asker: attention.Asker{MirrorHost: "tower", MirrorSession: routeSID, Label: "work:1"}, mirrorWindow: "@4", mirrorTmuxSession: "work"}
	out, gated := f.srv.hostStepUpGate(ctx, "tower", c, attentionAnswerTool, `{"id":`+itoa(x.ID)+`,"answer":"probe-yes"}`)
	if !gated {
		t.Fatal("fixture: the step-up gate does not cover tower")
	}
	card := stepUpCardOf(t, out)
	if _, err := f.srv.answerAskFrom(asDevice(f.owner), card, hostDecisionConfirm, attention.AnswerSource{}); err != nil {
		t.Fatalf("the phone's Confirm: %v", err)
	}
	if got, _ := att.Get(probeOwner(f), x.ID); got.Status != attention.Answered {
		t.Fatalf("fixture: the confirmed call did not run (ask %d is %s)", x.ID, got.Status)
	}
	probeTick(t, f)
	probeOne(t, m, "card #"+itoa(card))
}
