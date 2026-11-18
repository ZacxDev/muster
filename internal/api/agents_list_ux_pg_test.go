package api

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE AGENTS LIST'S UX BATCH on a real Postgres: marking a task complete from
// an agent's page without JavaScript lands home, and each entry says what it
// is waiting on (the sidebar row's data-nav-wait, where the retired list
// card's data-wait went), from the attention queue's one read.

// TestMarkCompleteWithoutJavaScriptLandsOnTheAgentsList: the no-JS form path
// of the agent page's Mark-complete (ui.TaskCompletePath). Operator ask:
// "after marking a task complete, it should just redirect back out of the
// agent instead of to another page". A plain form POST completes the task and
// answers 303 → /agents. Red before POST /tasks/{id}/complete existed (the
// route answered 405: the form had no method and no action at all).
func TestMarkCompleteWithoutJavaScriptLandsOnTheAgentsList(t *testing.T) {
	f := newAttnFixture(t)
	n, err := f.notes.Create(asUser(1), notes.Note{Title: "ux complete", Body: "ux complete", Status: notes.StatusReadyForReview})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM notes WHERE id = $1`, n.ID)
	})
	path := "/tasks/" + strconv.FormatInt(n.ID, 10) + "/complete"
	if path != ui.TaskCompletePath(n.ID) {
		t.Fatalf("the form's action is %q, the route is %q", ui.TaskCompletePath(n.ID), path)
	}
	// A browser without JavaScript: no HX-Request, a form body, its own origin.
	hdr := map[string]string{"Cookie": sessionCookieFor(f.srv), "Origin": "http://example.com"}
	body, ct := form(url.Values{})
	w := f.call(t, http.MethodPost, path, body, merge(hdr, ct))
	// Home (ui.HomePath): the Agents list page it used to land on is retired.
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("POST %s = %d → %q, want 303 → / (%s)", path, w.Code, w.Header().Get("Location"), w.Body)
	}
	if got, _ := f.notes.Get(asUser(1), n.ID); got.Status != notes.StatusComplete {
		t.Fatalf("the task is %s after the POST, want complete", got.Status)
	}
	// Refused cross-origin, and the task is not touched by a refused write.
	n2, err := f.notes.Create(asUser(1), notes.Note{Title: "ux complete 2", Body: "ux complete 2", Status: notes.StatusReadyForReview})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM notes WHERE id = $1`, n2.ID)
	})
	before, _ := f.notes.Get(asUser(1), n2.ID)
	body, ct = form(url.Values{})
	w = f.call(t, http.MethodPost, ui.TaskCompletePath(n2.ID), body, merge(map[string]string{"Cookie": sessionCookieFor(f.srv), "Origin": "http://evil.example"}, ct))
	if w.Code == http.StatusSeeOther {
		t.Fatalf("a cross-origin POST was accepted: %d", w.Code)
	}
	if got, _ := f.notes.Get(asUser(1), n2.ID); got.Status != before.Status || got.Status == notes.StatusComplete {
		t.Fatalf("a refused cross-origin POST changed the task to %s", got.Status)
	}
}

// navRowTags is each sidebar row's opening <a …> tag in a GET ui.NavAgentsPath
// answer, by its id ("nav-agent-<id>", "nav-mirror-<hash>").
func navRowTags(body string) map[string]string {
	out := map[string]string{}
	for _, m := range regexp.MustCompile(`<a id="(nav-[^"]+)"[^>]*>`).FindAllStringSubmatch(body, -1) {
		out[m[1]] = m[0]
	}
	return out
}

// TestTheCardsCarryWhatTheyAreWaitingOn: an agent that asked over MCP
// (attention_ask) and a mirrored Claude window stopped at a prompt (status
// waiting) both carry data-nav-wait on their sidebar row — where the retired
// Agents list's data-wait="you" moved, what the switcher's Waiting view holds —
// and a row waiting on nothing carries none. Red before the chips: no card
// carried the attribute.
func TestTheCardsCarryWhatTheyAreWaitingOn(t *testing.T) {
	f := newAttnFixture(t)
	f.ask(t, f.opTok, "Ship it tonight?")
	rows := navRowTags(f.page(t, ui.NavAgentsPath))
	op, op2 := rows[agentNavID(f.op.ID)], rows[agentNavID(f.op2.ID)]
	if op == "" || op2 == "" {
		t.Fatalf("instrument check FAILED: the two agents' rows are not in the sidebar: %v", rows)
	}
	if !strings.Contains(op, ` data-nav-wait=""`) {
		t.Errorf("the asking agent's row does not carry data-nav-wait: %s", op)
	}
	if strings.Contains(op2, "data-nav-wait") {
		t.Errorf("an agent waiting on nothing carries data-nav-wait: %s", op2)
	}
	var waiting, idle int
	for id, tag := range rows {
		if !strings.HasPrefix(id, "nav-mirror-") {
			continue
		}
		switch {
		case strings.Contains(tag, ` data-nav-wait=""`):
			waiting++
		case !strings.Contains(tag, "data-nav-wait"):
			idle++
		}
	}
	// attnWindows: @1 waiting at a prompt, @2 idle.
	if waiting != 1 || idle != 1 {
		t.Errorf("mirrored rows: %d waiting on an ask and %d on nothing, want 1 and 1: %v", waiting, idle, rows)
	}
}
