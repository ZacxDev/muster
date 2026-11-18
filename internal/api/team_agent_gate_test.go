package api

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
)

// TestTheManageRuleAdmitsAManagerOnly: refuseTeamAgentRoute itself — the
// route gate admit calls, with no handler behind it — on every route whose
// rule is teamAgentManage (start, stop, delete, credential rotation) of a team
// agent: a MANAGER of the team passes (false, nothing written), and an
// OPERATOR and a VIEWER are refused 403 with TeamAgentManagerOnly. CONTROL:
// the same operator passes the chat rule (POST /agents/{name}/send), so the
// operator's refusal is the manage rule's, not a fixture that refuses
// everyone. A gate loosened to "operator and up" goes red on the operator leg.
func TestTheManageRuleAdmitsAManagerOnly(t *testing.T) {
	worker := agents.Agent{ID: 77, Name: "team-gate-worker", OwnerID: tsTeam, Kind: agents.KindClaudeCode}
	s := &Server{logger: log.New(io.Discard, "", 0), ext: Extensions{Agents: &tsNamedAgents{tsAgents{rows: []agents.Agent{worker}}}}}
	as := func(role authz.TeamRole) context.Context {
		return authz.WithPrincipal(context.Background(), authz.Principal{UserID: tsOperator, Role: authz.RoleMember, Via: authz.ViaSession, Teams: tsIn(role)})
	}
	var manage []string
	for pattern, rule := range teamAgentRoutes {
		if rule == teamAgentManage {
			manage = append(manage, pattern)
		}
	}
	if len(manage) != 4 {
		t.Fatalf("fixture: %d routes carry teamAgentManage (%v); want the 4 lifecycle routes", len(manage), manage)
	}
	gate := func(pattern string, ctx context.Context) (bool, *httptest.ResponseRecorder) {
		r := httptest.NewRequest("POST", "/", nil).WithContext(ctx)
		r.Pattern = pattern
		if strings.Contains(pattern, "{name}") {
			r.SetPathValue("name", worker.Name)
		} else {
			r.SetPathValue("id", strconv.FormatInt(worker.ID, 10))
		}
		rec := httptest.NewRecorder()
		return s.refuseTeamAgentRoute(rec, r), rec
	}
	for _, pattern := range manage {
		if refused, rec := gate(pattern, as(authz.TeamManager)); refused || rec.Code != http.StatusOK {
			t.Errorf("%s, a manager: refused %v (%d); want admitted", pattern, refused, rec.Code)
		}
		for _, role := range []authz.TeamRole{authz.TeamOperator, authz.TeamViewer} {
			refused, rec := gate(pattern, as(role))
			if !refused || rec.Code != http.StatusForbidden || rec.Body.String() != TeamAgentManagerOnly+"\n" {
				t.Errorf("%s, a %s: refused %v, %d %q; want 403 %q", pattern, role, refused, rec.Code, rec.Body.String(), TeamAgentManagerOnly)
			}
		}
	}
	if refused, _ := gate("POST /agents/{name}/send", as(authz.TeamOperator)); refused {
		t.Error("control: an operator is refused the chat route too; the manage leg would prove nothing")
	}
}

// tsNamedAgents is tsAgents that also finds a row by name.
type tsNamedAgents struct{ tsAgents }

func (s *tsNamedAgents) GetByName(_ context.Context, name string) (agents.Agent, error) {
	for _, a := range s.rows {
		if a.Name == name {
			return a, nil
		}
	}
	return agents.Agent{}, pgx.ErrNoRows
}
