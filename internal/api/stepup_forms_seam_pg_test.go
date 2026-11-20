package api

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/auth"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/stepup"
)

// THE STEP-UP SEAM, BY BEHAVIOUR: a write the server refuses as stale
// ("Confirm it's you") must be reachable from a page that offers the confirm —
// its form marked data-step-up-form, the page drawing the dialog and the form
// script. stepUpChannels (stepup_ledger_test.go) lists every step-up gate and
// the test that proves its page; these are the pages this round widened the
// fix to (Round 0 of #717, F1): the team connectors page, a team's end and
// dispatch cards on the Asks list, a team agent's page and its terminal.

// isoBrowserAs is a browser request of user's session signed in at signedIn
// (the team app world's browserAs, for any isoWorld); a GET's 303 is followed
// once (an agent's page lands on its current session).
func isoBrowserAs(w *isoWorld, method, path string, uid int64, signedIn time.Time) *httptest.ResponseRecorder {
	rec := isoBrowserOnce(w, method, path, uid, signedIn)
	if loc := rec.Header().Get("Location"); method == http.MethodGet && rec.Code == http.StatusSeeOther && loc != "" {
		return isoBrowserOnce(w, method, loc, uid, signedIn)
	}
	return rec
}

func isoBrowserOnce(w *isoWorld, method, path string, uid int64, signedIn time.Time) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader("")).WithContext(context.Background())
	r.Host = "muster.test"
	r.Header.Set("Cookie", sessionCookieName+"="+auth.SignSession(w.srv.auth.uiSessionSecret(), signedIn, sessionTTL))
	r.Header.Set("Origin", "http://muster.test")
	r.Header.Set(isoUserHeader, strconv.FormatInt(uid, 10))
	r.Header.Set("Sec-Fetch-Dest", "document")
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, r)
	return rec
}

// formTag is the opening <form> tag posting to action in body, "" for none.
func formTag(body, action string) string {
	for _, tag := range formTagRE.FindAllString(body, -1) {
		if strings.Contains(tag, `action="`+action+`"`) {
			return tag
		}
	}
	return ""
}

// confirmsInPlace checks the page draws the dialog and the form script, and
// that the form posting to action is marked data-step-up-form — with why as
// its own sentence when why is set.
func confirmsInPlace(t *testing.T, who, body, action, why string) {
	t.Helper()
	if !strings.Contains(body, `id="term-confirm"`) || !strings.Contains(body, "window.musterStepUpForms") {
		t.Errorf("%s: the page draws no confirm dialog or form script", who)
	}
	tag := formTag(body, action)
	switch {
	case tag == "":
		t.Errorf("%s: CONTROL: no form posts to %s", who, action)
	case !strings.Contains(tag, "data-step-up-form"):
		t.Errorf("%s: the gated form %s is not marked data-step-up-form: %s", who, action, tag)
	case why != "" && !strings.Contains(tag, `data-step-up-why="`+html.EscapeString(why)+`"`):
		t.Errorf("%s: the gated form %s does not carry its sentence %q: %s", who, action, why, tag)
	}
}

// TestTheTeamConnectorsPageConfirmsInPlace: a stale MANAGER's connectors page
// — a pending request's Approve and a standing grant's Revoke, both step-up
// gated — confirms in place. Deny is not gated, and is not marked.
//
// 🔴 RED at ea84f2ab: no dialog, neither form marked (the refusal said
// "Confirm it's you" with nothing to confirm with).
func TestTheTeamConnectorsPageConfirmsInPlace(t *testing.T) {
	tw := newTeamAppWorld(t, true)
	_, sg := tw.approvedOn(t, "tconf-standing", teamAppRepo)
	ref, tok := tw.teamWorkerOn(t, "tconf-pending", teamAppOther) // not the approved repository: that one is approved on asking
	id, _ := tw.fileGitHub(t, ref, tok)
	team := strconv.FormatInt(tw.team, 10)
	page := tw.browserAs(http.MethodGet, "/teams/"+team+"/connectors", tw.w.a.uid, time.Now().Add(-2*stepup.Window()))
	if page.Code != http.StatusOK {
		t.Fatalf("A's (manager, stale) connectors page: %d", page.Code)
	}
	body := page.Body.String()
	confirmsInPlace(t, "approve", body, "/teams/"+team+"/connectors/"+strconv.FormatInt(id, 10)+"/approve", "")
	confirmsInPlace(t, "standing revoke", body, "/teams/"+team+"/connectors/standing/"+strconv.FormatInt(sg, 10)+"/revoke", TeamConnectorsRevokeWhy())
	if tag := formTag(body, "/teams/"+team+"/connectors/"+strconv.FormatInt(id, 10)+"/deny"); tag == "" || strings.Contains(tag, "data-step-up-form") {
		t.Errorf("the Deny form (not step-up gated) is %q; want drawn and unmarked", tag)
	}
	if !strings.Contains(body, html.EscapeString(TeamConnectorsApproveWhy())) {
		t.Errorf("the page's dialog does not say the approve gate %q", TeamConnectorsApproveWhy())
	}
}

// TestATeamsCardsConfirmInPlaceForAnOperator: on the Asks page, a team's END
// card and a team's DISPATCH card are answered by an operator or a manager
// with a fresh sign-in: B's (operator) answer forms for both are marked
// data-step-up-form with their own sentence, and the page draws the dialog
// with the plugin store OFF (the dialog used to come only with the drafts
// tab). V's (viewer) copies are watch-only, with no form at all.
//
// 🔴 RED at ea84f2ab: neither form marked, and no dialog without plugins
// (assertions.log 21:49:32: a stale operator's End it answered into
// TeamEndCardStepUp with no way to confirm).
func TestATeamsCardsConfirmInPlaceForAnOperator(t *testing.T) {
	td := newTeamDispatchWorld(t)
	m := &endCardMirror{Mirror: td.w.srv.ext.Mirror, team: td.team, own: td.w.a.uid}
	td.w.srv.ext.Mirror = m
	if td.w.srv.ext.Focus == nil {
		td.w.srv.ext.Focus = focus.NewPG(td.w.pool)
	}
	t.Cleanup(func() {
		_, _ = td.w.pool.Exec(authz.System(context.Background()), `DELETE FROM chief_proposals WHERE owner_id = $1`, td.team)
	})
	_, clear := td.typed(t, td.w.b.uid, true, "end the build-01 session")
	var end struct {
		Card int64 `json:"card_id"`
	}
	if text := td.w.srv.toolEndSessionPropose(td.teamCtx, td.chief, `{"session":"`+endCardKey+`"}`); json.Unmarshal([]byte(text), &end) != nil || end.Card <= 0 {
		t.Fatalf("the team chief's end card: %s", truncate(text, 300))
	}
	clear() // B's fresh turn is spent on the end card: the dispatch below falls back to a card
	td.typed(t, td.w.b.uid, false, "start a Claude agent on the team task")
	in := td.onTask("k-seam", true)
	in.Prompt = "Do the team's work."
	dtext := td.dispatch(in)
	disp := heldCard(dtext)
	if disp <= 0 {
		t.Fatalf("no team dispatch card: %s", truncate(dtext, 400))
	}

	// The page with the plugin store OFF: the dialog must not come only with
	// the drafts tab.
	td.w.srv.ext.Plugins = nil
	stale := time.Now().Add(-2 * stepup.Window())
	page := isoBrowserAs(td.w, http.MethodGet, attentionPath, td.w.b.uid, stale)
	if page.Code != http.StatusOK {
		t.Fatalf("B's Asks page: %d", page.Code)
	}
	body := page.Body.String()
	confirmsInPlace(t, "B's end card", body, attentionPath+"/asks/"+strconv.FormatInt(end.Card, 10)+"/answer", TeamEndCardWhy())
	confirmsInPlace(t, "B's dispatch card", body, attentionPath+"/asks/"+strconv.FormatInt(disp, 10)+"/answer", TeamDispatchCardWhy())

	v := isoBrowserAs(td.w, http.MethodGet, attentionPath, td.v.uid, time.Now()).Body.String()
	for _, card := range []int64{end.Card, disp} {
		if formTag(v, attentionPath+"/asks/"+strconv.FormatInt(card, 10)+"/answer") != "" {
			t.Errorf("V's (viewer) card %d draws an answer form", card)
		}
		if !strings.Contains(askCardHTML(v, card), "data-attention-watch-only") {
			t.Errorf("V's (viewer) card %d is not drawn watch-only", card)
		}
	}
}

// TestATeamAgentsPagesConfirmInPlaceForAnOperator: the team chief's terminal
// page, read by B (operator), gates the WHOLE attach (data-step-up-attach), so
// a stale operator is asked before it attaches instead of a socket refused
// with no way to confirm. V (viewer) does not get it: there is nothing for a
// viewer to confirm.
//
// The chief's chat page draws no step-up form for B any more, and so no
// dialog: the team cards that asked with one were its asks block's, and the
// page has no asks block (attention_asker.go) — they are answered on the Asks
// page, which draws their dialog (TestATeamsCardsConfirmInPlaceForAnOperator).
//
// 🔴 RED at ea84f2ab: no dialog on the terminal page for B.
func TestATeamAgentsPagesConfirmInPlaceForAnOperator(t *testing.T) {
	td := newTeamDispatchWorld(t)
	stale := time.Now().Add(-2 * stepup.Window())
	agentPage := "/agents/" + td.chief.Name
	for who, uid := range map[string]int64{"B (operator)": td.w.b.uid, "V (viewer)": td.v.uid} {
		rec := isoBrowserAs(td.w, http.MethodGet, agentPage, uid, stale)
		if body := rec.Body.String(); rec.Code != http.StatusOK || strings.Contains(body, "data-step-up-form") || strings.Contains(body, `id="term-confirm"`) {
			t.Errorf("%s's page of the team chief: %d, a step-up form %v, a confirm dialog %v", who, rec.Code,
				strings.Contains(body, "data-step-up-form"), strings.Contains(body, `id="term-confirm"`))
		}
	}
	term := agentPage + "/terminal"
	rec := isoBrowserAs(td.w, http.MethodGet, term, td.w.b.uid, stale)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `data-step-up-attach=""`) || !strings.Contains(body, `data-fresh-for="0"`) ||
		!strings.Contains(body, `id="term-confirm"`) {
		t.Errorf("B's (operator, stale) team agent terminal: %d, attach gated %v, stale %v, dialog %v", rec.Code,
			strings.Contains(body, `data-step-up-attach=""`), strings.Contains(body, `data-fresh-for="0"`), strings.Contains(body, `id="term-confirm"`))
	}
	if body := isoBrowserAs(td.w, http.MethodGet, term, td.v.uid, stale).Body.String(); strings.Contains(body, `data-step-up-attach=""`) {
		t.Errorf("V's (viewer) team agent terminal is drawn as a gated attach")
	}
}
