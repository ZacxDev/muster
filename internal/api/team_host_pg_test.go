package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/users"
)

// A TEAM HOST'S MCP DOOR (team sharing 2b.2) over a real Postgres: a
// muster-mirror connected with a TEAM-owned token. Its POST /mirror/mcp acts
// as the team acting principal, and is served teamHostTools only.

// newTeamHostFixture is the mirror door fixture with the real users store (a
// team reads as a team), a team managed by the fixture's member, and a team
// host token for "ws" — the fake hub lists "ws" for the team as well.
func newTeamHostFixture(t *testing.T) (f *attnFixture, team int64, teamTok, userTok string) {
	t.Helper()
	f, userTok, _, _ = newMirrorMCPFixture(t)
	f.srv.ext.Users = users.NewPG(f.pool)
	team = newTeam(t, f.pool, f.member)
	teamTok = mirrorTok('t')
	f.tokens[teamTok] = mirror.Token{ID: 41, OwnerID: team, Host: "ws"}
	f.mirror.mu.Lock()
	f.mirror.teamHost = team
	f.mirror.mu.Unlock()
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM attention_asks WHERE owner_id = $1`, team)
	})
	return f, team, teamTok, userTok
}

// listedTools is a tools/list's names, as listed.
func listedTools(t *testing.T, f *attnFixture, hdr map[string]string) []string {
	t.Helper()
	code, r, body := f.mirrorRPC(t, hdr, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if code != 200 || r == nil || r.Error != nil {
		t.Fatalf("tools/list: %d %s", code, body)
	}
	var res struct {
		Tools []struct{ Name string } `json:"tools"`
	}
	_ = json.Unmarshal(r.Result, &res)
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	return names
}

// TestATeamHostIsListedOnlyItsKeptTools: a team token's tools/list is the
// kept tools this deployment offers (and muster_help) — the same token's
// door, with a person's token, lists the owner-authority and estate tools
// too (the control).
//
// 🔴 RED at the base (93e002f6): admit refused the team token (403).
func TestATeamHostIsListedOnlyItsKeptTools(t *testing.T) {
	f, _, teamTok, userTok := newTeamHostFixture(t)
	got := strings.Join(listedTools(t, f, mirrorAs(teamTok, focusSID)), " ")
	if want := "attention_ask attention_wait attention_cancel tasks_list task_get task_comment task_set_status muster_help"; got != want {
		t.Fatalf("a team host's tools = %s\nwant %s", got, want)
	}
	if control := strings.Join(listedTools(t, f, mirrorAs(userTok, focusSID)), " "); !strings.Contains(control, attentionAnswerTool) || !strings.Contains(control, agentsListTool) {
		t.Fatalf("CONTROL: a person's host lists %s; want attention_answer and agents_list among them", control)
	}
}

// rowsFingerprint is a count of every row the refused tools could write or
// change: asks (and the answered/cancelled ones), tasks and comments, agents,
// drafts, audit rows and mirror tokens.
func rowsFingerprint(t *testing.T, f *attnFixture) string {
	t.Helper()
	var parts []string
	for _, q := range []string{
		`SELECT count(*) FROM attention_asks`,
		`SELECT count(*) FROM attention_asks WHERE status <> 'open'`,
		`SELECT count(*) FROM notes`,
		`SELECT count(*) FROM agents`,
		`SELECT count(*) FROM audit_log`,
		`SELECT count(*) FROM plugin_reply_drafts`,
		`SELECT count(*) FROM plugin_reply_drafts WHERE status <> 'pending'`,
	} {
		var n int
		if err := f.pool.QueryRow(authz.System(context.Background()), q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		parts = append(parts, strconv.Itoa(n))
	}
	return strings.Join(parts, "/")
}

// TestATeamHostRefusesEachRefusedToolAtTheCall: EVERY tool teamHostTools
// refuses, called by name from a session the team host lists, answers the
// teamHostTools refusal as a tool error — listed or not on this deployment —
// and no row changes (no answer, no card, no draft decided, no agent touched).
// The CONTROL is a kept tool (attention_ask), which runs as the team.
func TestATeamHostRefusesEachRefusedToolAtTheCall(t *testing.T) {
	f, team, teamTok, _ := newTeamHostFixture(t)
	var refusedNames []string
	for n, r := range teamHostTools {
		if !r.keep {
			refusedNames = append(refusedNames, n)
		}
	}
	sort.Strings(refusedNames)
	if len(refusedNames) < 25 {
		t.Fatalf("instrument check FAILED: %d refused tools", len(refusedNames))
	}
	before := rowsFingerprint(t, f)
	args := map[string]any{"id": 1, "answer": "x", "question": "q?", "approve": true, "name": "x", "body": "x", "text": "x", "slug": "x", "decision": "approve"}
	for _, n := range refusedNames {
		_, r, body := f.mirrorRPC(t, mirrorAs(teamTok, focusSID), mcpToolCall(n, args))
		if r == nil || !strings.Contains(string(r.Result), `"isError":true`) || !strings.Contains(body, teamHostToolRefusal(n)) {
			t.Errorf("%s from a team host: %s; want the teamHostTools refusal", n, body)
		}
	}
	if after := rowsFingerprint(t, f); after != before {
		t.Fatalf("a refused tool changed rows: %s -> %s", before, after)
	}
	// CONTROL: a kept tool runs, as the team.
	_, r, body := f.mirrorRPC(t, mirrorAs(teamTok, focusSID), mcpToolCall(attentionAskTool, map[string]any{"question": "Ship build-01?", "options": []string{"yes", "no"}}))
	if text, isErr := toolText(t, r); r == nil || isErr {
		t.Fatalf("CONTROL: attention_ask from a team host: %s", body)
	} else {
		var a struct{ ID int64 }
		_ = json.Unmarshal([]byte(text), &a)
		var owner int64
		if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT owner_id FROM attention_asks WHERE id = $1`, a.ID).Scan(&owner); err != nil || owner != team {
			t.Fatalf("CONTROL: the team host's ask %d is owned by %d (%v); want the team %d", a.ID, owner, err, team)
		}
	}
}

// TestATeamHostCannotAnswerATeamAskThatIsNotItsOwn (the phase 4.7 seam,
// agreed with that session): 2b.2 adds NO answer path for team asks. A team
// host's session cannot answer, wait on or cancel any team-owned ask that is
// not its own question — a System card shaped like a dispatch approval card,
// a team agent's ask, and another session's ask on the same host alike:
// attention_answer is refused by teamHostTools, and attention_wait /
// attention_cancel are bound to the session's own asks (ownAsk). Every one
// stays open and unanswered. The CONTROL is the session's own ask, which it
// cancels.
func TestATeamHostCannotAnswerATeamAskThatIsNotItsOwn(t *testing.T) {
	f, team, teamTok, _ := newTeamHostFixture(t)
	asTeam := authz.WithPrincipal(context.Background(), teamActingPrincipal(team, authz.ViaAttention))
	worker := f.agentFor(t, team, "teamhost-worker-"+strconv.FormatInt(team, 10), "", "teamhost-worker-tok-"+strconv.FormatInt(team, 10))
	var ids []int64
	for _, n := range []attention.New{
		{Asker: attention.Asker{System: true, Label: "dispatch"}, Question: "Start a worker on build-01?", Options: []string{"approve", "deny"}},
		{Asker: attention.Asker{AgentID: worker.ID}, Question: "Which branch?", Options: []string{"main", "release"}},
		{Asker: attention.Asker{MirrorHost: "ws", MirrorSession: secondSID}, Question: "Another session's question?", Options: []string{"a", "b"}},
	} {
		a, err := f.attn.Create(asTeam, n)
		if err != nil {
			t.Fatalf("file %q as the team: %v", n.Question, err)
		}
		ids = append(ids, a.ID)
	}
	for _, id := range ids {
		_, r, body := f.mirrorRPC(t, mirrorAs(teamTok, focusSID), mcpToolCall(attentionAnswerTool, map[string]any{"id": id, "answer": "approve"}))
		if r == nil || !strings.Contains(body, teamHostToolRefusal(attentionAnswerTool)) {
			t.Fatalf("attention_answer on team ask %d from a team host: %s; want the teamHostTools refusal", id, body)
		}
		for _, tool := range []string{attentionWaitTool, attentionCancelTool} {
			_, r, body := f.mirrorRPC(t, mirrorAs(teamTok, focusSID), mcpToolCall(tool, map[string]any{"id": id, "wait_seconds": 0}))
			if text, isErr := toolText(t, r); r == nil || !isErr || !strings.Contains(text, "is not one of yours") {
				t.Fatalf("%s on team ask %d from a team host: %s; want it refused as not the session's own", tool, id, body)
			}
		}
		a, err := f.attn.Get(asTeam, id)
		if err != nil || a.Status != attention.Open || a.Answer != "" {
			t.Fatalf("team ask %d after the team host's tries: %+v, %v; want it open and unanswered", id, a, err)
		}
	}
	// CONTROL: the session's own ask is its to cancel.
	_, r, body := f.mirrorRPC(t, mirrorAs(teamTok, focusSID), mcpToolCall(attentionAskTool, map[string]any{"question": "Mine?", "options": []string{"y", "n"}}))
	text, isErr := toolText(t, r)
	var own struct{ ID int64 }
	if isErr || json.Unmarshal([]byte(text), &own) != nil || own.ID == 0 {
		t.Fatalf("CONTROL: attention_ask: %s", body)
	}
	_, r, body = f.mirrorRPC(t, mirrorAs(teamTok, focusSID), mcpToolCall(attentionCancelTool, map[string]any{"id": own.ID}))
	if _, isErr := toolText(t, r); isErr {
		t.Fatalf("CONTROL: the session could not cancel its own ask: %s", body)
	}
}

// TestATeamTokenIsNotServedRemotely: a remote-marked call (X-Muster-Client:
// remote) with a TEAM token is refused 401, and no row is read. The host is
// one this hub has NEVER seen ("far-team"): a remote call from a known host is
// refused earlier, as a tool error, so only an unknown host reaches this
// refusal. The call names a session that CREATED a team task (task_get on
// it): without the refusal it would be served unbound, as the team. The
// CONTROL is the same call with a person's token for an unknown host of
// theirs, which is served.
func TestATeamTokenIsNotServedRemotely(t *testing.T) {
	f, team, _, _ := newTeamHostFixture(t)
	farTeam := mirrorTok('u')
	f.tokens[farTeam] = mirror.Token{ID: 43, OwnerID: team, Host: "far-team"}
	farMine := mirrorTok('v')
	f.tokens[farMine] = mirror.Token{ID: 44, OwnerID: 1, Host: "far"}
	create := func(ctx context.Context, title string) int64 {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(`{"title":"`+title+`","body":"made by a remote session"}`))
		req.Header.Set("X-Muster-Session-Id", remoteSID)
		rec := httptest.NewRecorder()
		f.srv.handleAPITaskCreate(rec, req.WithContext(ctx))
		var created struct{ ID int64 }
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &created) != nil || created.ID == 0 {
			t.Fatalf("task create: %d %s", rec.Code, rec.Body)
		}
		t.Cleanup(func() {
			_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM notes WHERE id = $1`, created.ID)
		})
		return created.ID
	}
	teamTask := create(authz.WithPrincipal(context.Background(), teamActingPrincipal(team, authz.ViaAgent)), "team-remote-task")
	myTask := create(asUser(1), "my-remote-task")

	// CONTROL: a person's remote call from an unknown host is served.
	_, r, body := f.mirrorRPC(t, remoteAs(farMine, remoteSID), mcpToolCall(taskGetTool, map[string]any{"id": myTask}))
	if text, isErr := toolText(t, r); r == nil || isErr || !strings.Contains(text, "my-remote-task") {
		t.Fatalf("CONTROL: a person's remote task_get: %s", body)
	}
	code, _, body := f.mirrorRPC(t, remoteAs(farTeam, remoteSID), mcpToolCall(taskGetTool, map[string]any{"id": teamTask}))
	if code != http.StatusUnauthorized || !strings.Contains(body, "a team host token is not served remotely") {
		t.Fatalf("a team token's remote task_get: %d %s; want 401, a team host token is not served remotely", code, body)
	}
	if strings.Contains(body, "team-remote-task") {
		t.Fatalf("the refused remote call read the team's task: %s", body)
	}
}
