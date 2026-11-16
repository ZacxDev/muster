package api

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/plugins"
)

// A TEAM'S ASK, AS A VIEWER OF THE TEAM READS IT ON THE ASKS LIST
// (docs/mirroring.md, team sharing 2b.4: "their question cards are drawn
// unanswerable"). Found on v0.232.0 by a live browser check: a viewer's copy
// of the team's end card drew "End it" and "Not now" as tappable buttons (and
// Dismiss, and a typed answer), every one of which the server then refused.

// askCardHTML is the <li> of ask id in an Asks list body, "" when absent.
func askCardHTML(body string, id int64) string {
	open := `<li id="` + attentionAskKey(id) + `"`
	i := strings.Index(body, open)
	if i < 0 {
		return ""
	}
	j := strings.Index(body[i:], "</li>")
	if j < 0 {
		return body[i:]
	}
	return body[i : i+j]
}

// asksListAs is the Asks list's live region (GET /attention/items) as ctx's
// browser session draws it.
func asksListAs(t *testing.T, s *Server, ctx context.Context) string {
	t.Helper()
	r := httptest.NewRequest("GET", "/attention/items", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	s.handleAttentionItems(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /attention/items: %d", rec.Code)
	}
	return rec.Body.String()
}

// TestATeamViewerDrawsTheTeamsEndCardUnanswerable: the team chief files the
// team's end card on B's (operator) fresh request. On the Asks list:
//
//   - V (a VIEWER) sees the card — its question — with NO option button, no
//     answer form and no Dismiss, and TeamAskViewerNote in their place;
//   - B (an OPERATOR) sees End it and Not now as buttons (the control: the
//     card V reads is the same card, drawn answerable for an operator).
//
// 🔴 RED at ff1295f8: V's card drew End it, Not now, Dismiss and the box.
//
// Then, by the server (INVARIANT GUARDS — the server already refused these
// before this change; pinned so the drawing never becomes the only gate):
// V's tap on Not now is refused TeamEndCardOperatorOnly and declines nothing
// (2b.4: an operator's Not now declines the team's card, a viewer's must
// not), and V's Dismiss cancels nothing.
func TestATeamViewerDrawsTheTeamsEndCardUnanswerable(t *testing.T) {
	td := newTeamDispatchWorld(t)
	m := &endCardMirror{Mirror: td.w.srv.ext.Mirror, team: td.team, own: td.w.a.uid}
	td.w.srv.ext.Mirror = m
	if td.w.srv.ext.Focus == nil {
		td.w.srv.ext.Focus = focus.NewPG(td.w.pool)
	}
	t.Cleanup(func() {
		_, _ = td.w.pool.Exec(authz.System(context.Background()), `DELETE FROM chief_proposals WHERE owner_id = $1`, td.team)
	})
	td.typed(t, td.w.b.uid, true, "end the build-01 session")
	text := td.w.srv.toolEndSessionPropose(td.teamCtx, td.chief, `{"session":"`+endCardKey+`"}`)
	var out struct {
		ID   int64 `json:"proposal_id"`
		Card int64 `json:"card_id"`
	}
	if json.Unmarshal([]byte(text), &out) != nil || out.ID <= 0 || out.Card <= 0 {
		t.Fatalf("the team chief's end_session_propose: %s; want a card", truncate(text, 400))
	}

	vCtx := td.personCtx(td.v.uid, authz.ViaSession, true)
	viewer := askCardHTML(asksListAs(t, td.w.srv, vCtx), out.Card)
	if viewer == "" {
		t.Fatalf("V (viewer) does not see the team's end card on the Asks list at all")
	}
	if !strings.Contains(viewer, `data-attention-watch-only`) || !strings.Contains(viewer, html.EscapeString(TeamAskViewerNote)) {
		t.Errorf("V's card does not say why it cannot be answered (%q):\n%s", TeamAskViewerNote, viewer)
	}
	for _, mark := range []string{`data-attention-option`, `data-attention-answer`, `data-attention-dismiss`, `/answer"`, `/cancel"`,
		`value="` + endSessionLabel + `"`, `value="` + proposalDeclineLabel + `"`} {
		if strings.Contains(viewer, mark) {
			t.Errorf("V's (viewer) card draws %s — something to tap that the server refuses:\n%s", mark, viewer)
		}
	}

	op := askCardHTML(asksListAs(t, td.w.srv, td.personCtx(td.w.b.uid, authz.ViaSession, true)), out.Card)
	for _, mark := range []string{`value="` + endSessionLabel + `"`, `value="` + proposalDeclineLabel + `"`, `data-attention-dismiss`} {
		if !strings.Contains(op, mark) {
			t.Errorf("CONTROL: B's (operator) card lacks %s:\n%s", mark, op)
		}
	}
	if strings.Contains(op, `data-attention-watch-only`) {
		t.Errorf("CONTROL: B's (operator) card is drawn watch-only")
	}

	// The server, whatever is drawn.
	if _, err := td.w.srv.answerAskFrom(withOptionTap(vCtx), out.Card, proposalDeclineLabel, attention.AnswerSource{}); err == nil ||
		!strings.Contains(err.Error(), TeamEndCardOperatorOnly) {
		t.Errorf("V tapping Not now: %v; want %q", err, TeamEndCardOperatorOnly)
	}
	if _, err := td.w.srv.cancelAsk(vCtx, out.Card); err == nil {
		t.Errorf("V's Dismiss of the team's card was accepted")
	}
	a, err := td.w.srv.ext.Attention.Get(td.teamCtx, out.Card)
	if err != nil || a.Status != attention.Open {
		t.Errorf("after V's Not now and Dismiss the card is %q (%v); want open", a.Status, err)
	}
	x, err := plugins.NewPG(td.w.pool).ProposalByID(td.teamCtx, out.ID)
	if err != nil || x.Decided != plugins.ProposalOpen {
		t.Errorf("after V's Not now the team's proposal is %+v (%v); want still open", x.Decided, err)
	}
}
