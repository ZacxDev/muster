package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/authz"
)

// TestATeamHostsQuestionIsWatchedAsTheTeam (team sharing 2b.2): an
// AskUserQuestion on a TEAM host (the hook's token is a team's: owner =
// askTeamOwner) opens ONE question under the team — none under anyone else —
// and its watcher reads the team host's window AS THE TEAM (the team acting
// principal through authz.ViaAsk), so the terminal's answer resolves it.
//
// 🔴 RED at the base (93e002f6): ViaAsk was not team-acting, so askPrincipal
// panicked in WithPrincipal; the watch goroutine died (recovered), the
// window was never read and the question never resolved.
func TestATeamHostsQuestionIsWatchedAsTheTeam(t *testing.T) {
	am, s, ts := askServer(t, true)
	s.ext.Users = teamUsers{team: askTeamOwner}
	am.fakeMirror.mu.Lock()
	am.fakeMirror.teamHost = askTeamOwner
	am.fakeMirror.mu.Unlock()
	h := askq.Hook{Base: ts.URL, Path: "/mirror/ask", Auth: func(r *http.Request) { r.Header.Set("Authorization", "Bearer mmt_team") },
		Client: &http.Client{Timeout: 3 * time.Second}}
	if !h.Run(context.Background(), gatePayload(t)) {
		t.Fatal("the team host's registration was refused")
	}
	if got := s.Questions().Pending(askTeamOwner); len(got) != 1 {
		t.Fatalf("the team has %d open questions; want 1", len(got))
	}
	if got := s.Questions().Pending(1); len(got) != 0 {
		t.Fatalf("user 1 has %d open questions from the team host; want none", len(got))
	}
	want := authz.Principal{UserID: askTeamOwner, Team: true, Role: authz.RoleMember, Via: authz.ViaAsk}
	deadline := time.Now().Add(5 * time.Second)
	for {
		am.amu.Lock()
		seen := append([]authz.Principal(nil), am.watchedAs...)
		am.amu.Unlock()
		if len(seen) > 0 {
			if seen[0] != want {
				t.Fatalf("the watcher read the team host's window as %+v; want the team acting principal %+v", seen[0], want)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the team host's question was never watched: the watcher did not read its window")
		}
		time.Sleep(10 * time.Millisecond)
	}
	am.feed(gateTool, gateQs(t), map[string]string{"Which color do you prefer?": "Green", "Which pets do you have?": "Dog"})
	deadline = time.Now().Add(5 * time.Second)
	for len(s.Questions().Pending(askTeamOwner)) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the terminal's answer did not resolve the team host's question")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
