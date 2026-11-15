package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
)

// TestATeamHostCannotAnswerARealTeamDispatchCard is the 2b.2 / 4.7 seam on
// the merged tree. A REAL team dispatch card (#682: the team chief's
// approval card, with its hold) is open. #682's teamCardRefusal lets a TEAM
// principal through unchecked (it guards persons: the browser, a fresh
// operator), so the team host door's tool-list filter (teamHostTools) is the
// one thing between a team host's session and that card. From a session the
// team host lists, attention_answer is refused by teamHostTools, and
// attention_wait / attention_cancel are refused as not the session's own;
// the card stays open.
//
// The CONTROL pins why that filter matters: the same answer under the team
// host door's own principal (the team acting principal at ViaMirrorHost),
// straight into answerAskFrom with no tool list in the way, is NOT refused by
// the card's gate — it reaches the answer.
func TestATeamHostCannotAnswerARealTeamDispatchCard(t *testing.T) {
	td := newTeamDispatchWorld(t)
	td.typed(t, td.w.b.uid, false, "start a Claude agent on the team task")
	in := td.onTask("k-seam", true)
	in.Prompt = "Do the team's work."
	ask := heldCard(td.dispatch(in))
	if ask <= 0 {
		t.Fatal("fixture: no team dispatch card was filed")
	}
	w := td.w
	teamTok := mirrorTok('s')
	w.srv.ext.MirrorTokens = fakeMirrorTokens{teamTok: mirror.Token{ID: 61, OwnerID: td.team, Host: "ws"}}
	fm := &fakeMirror{online: true, interactive: true, statusReport: true, windows: attnWindows(mirror.StatusWaiting), teamHost: td.team}
	w.srv.ext.Mirror = fm
	call := func(tool string, args any) string {
		t.Helper()
		r := httptest.NewRequest("POST", "/mirror/mcp", strings.NewReader(mcpToolCall(tool, args)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+teamTok)
		r.Header.Set(mirrorSessionHeader, focusSID)
		rec := httptest.NewRecorder()
		w.h.ServeHTTP(rec, r)
		if rec.Code != 200 {
			t.Fatalf("%s from the team host: HTTP %d %s", tool, rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	if body := call(attentionAnswerTool, map[string]any{"id": ask, "answer": approveLabel}); !strings.Contains(body, teamHostToolRefusal(attentionAnswerTool)) {
		t.Fatalf("attention_answer on the team's dispatch card from a team host: %s; want the teamHostTools refusal", body)
	}
	for _, tool := range []string{attentionWaitTool, attentionCancelTool} {
		var res struct {
			Result struct {
				Content []struct{ Text string } `json:"content"`
				IsError bool                    `json:"isError"`
			} `json:"result"`
		}
		body := call(tool, map[string]any{"id": ask, "wait_seconds": 0})
		if json.Unmarshal([]byte(body), &res) != nil || !res.Result.IsError || len(res.Result.Content) == 0 ||
			!strings.Contains(res.Result.Content[0].Text, "is not one of yours") {
			t.Fatalf("%s on the team's dispatch card from a team host: %s; want it refused as not the session's own", tool, body)
		}
	}
	a, err := w.srv.ext.Attention.Get(td.teamCtx, ask)
	if err != nil || a.Status != attention.Open {
		t.Fatalf("after the team host's tries the card is %q (%v); want it still open", a.Status, err)
	}
	// CONTROL: the card's own gate does not stop the team host door's
	// principal — only the tool list does.
	hostCtx := authz.WithPrincipal(context.Background(), teamActingPrincipal(td.team, authz.ViaMirrorHost))
	if refused, err := w.srv.teamCardRefusal(hostCtx, ask); refused || err != nil {
		t.Fatalf("CONTROL: teamCardRefusal refused the team host door's principal (%v, %v); the tool list would not be the one guard", refused, err)
	}
}
