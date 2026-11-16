package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/users"
)

// A TEAM HOST'S MCP DOOR (team sharing 2b.2) over a real Postgres: a
// muster-mirror connected with a TEAM-owned token. Its POST /mirror/mcp acts
// as the team acting principal, and is served teamHostTools only. This file
// names no 2b.2 symbol, so it compiles (and is red) at the base.

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
	if want := "attention_ask attention_wait attention_cancel tasks_list task_get task_comment task_set_status track_task muster_help"; got != want {
		t.Fatalf("a team host's tools = %s\nwant %s", got, want)
	}
	if control := strings.Join(listedTools(t, f, mirrorAs(userTok, focusSID)), " "); !strings.Contains(control, attentionAnswerTool) || !strings.Contains(control, agentsListTool) {
		t.Fatalf("CONTROL: a person's host lists %s; want attention_answer and agents_list among them", control)
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
