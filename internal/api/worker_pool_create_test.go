package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestAPoolWorkerIsVisibleWhereAgentsAreListed: a create with worker_pool set
// (POST /api/agents, which replaced the dispatch form) answers the new agent's
// page, and that page carries the pool chip; GET /api/agents carries
// workerPool. A second agent with no pool is the control: its page has no
// chip, its workerPool is "".
func TestAPoolWorkerIsVisibleWhereAgentsAreListed(t *testing.T) {
	s, h, store := kindsServer(t, stubKinds{pick: "work"})
	s.ext.Agents = &pageablePreflight{store}
	h = s.Handler()
	rec := postDispatch(t, s, h, url.Values{"kind": {"claude-code"}, "worker_pool": {"reviewers"}})
	if !dispatched(rec, store) {
		t.Fatalf("dispatch: %d %s", rec.Code, rec.Body)
	}
	pooled := createdHref(t, rec)
	if rec := postDispatch(t, s, h, url.Values{"kind": {"claude-code"}}); !dispatched(rec, store) {
		t.Fatalf("control dispatch: %d %s", rec.Code, rec.Body)
	}
	plain := "/agents/" + store.rows()[1].Name
	for _, c := range []struct {
		page string
		want int
	}{{pooled, 1}, {plain, 0}} {
		body := followAgentPage(t, s, h, c.page)
		if n := strings.Count(body, `data-worker-pool="reviewers"`); n != c.want || strings.Count(body, `data-worker-pool=`) != c.want {
			t.Fatalf("%s carries %d pool chips, want %d:\n%s", c.page, n, c.want, body)
		}
	}

	var rows []struct {
		Name       string `json:"name"`
		WorkerPool string `json:"workerPool"`
	}
	if err := json.Unmarshal(getOwed(t, s, h, "/api/agents").Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	pools := map[string]int{}
	for _, r := range rows {
		pools[r.WorkerPool]++
	}
	if len(rows) != 2 || pools["reviewers"] != 1 || pools[""] != 1 {
		t.Fatalf("GET /api/agents pools = %v over %d rows, want one reviewers and one none", pools, len(rows))
	}
}

// pageablePreflight is the dispatch fixture's store answering what an agent's
// page reads too: each created row by name, with one session (its id + 1000)
// and no messages.
type pageablePreflight struct{ *preflightStore }

func (p *pageablePreflight) byName(name string) (agents.Agent, bool) {
	for _, a := range p.rows() {
		if a.Name == name {
			return a, true
		}
	}
	return agents.Agent{}, false
}

func (p *pageablePreflight) GetByName(_ context.Context, name string) (agents.Agent, error) {
	if a, ok := p.byName(name); ok {
		return a, nil
	}
	return agents.Agent{}, pgx.ErrNoRows
}

func (p *pageablePreflight) Get(_ context.Context, id int64) (agents.Agent, error) {
	for _, a := range p.rows() {
		if a.ID == id {
			return a, nil
		}
	}
	return agents.Agent{}, pgx.ErrNoRows
}

func (p *pageablePreflight) LatestOrCreateSession(_ context.Context, agentID int64, _ string) (agents.ChatSession, error) {
	return agents.ChatSession{ID: agentID + 1000, AgentID: agentID, SessionKey: "pool-key"}, nil
}

func (p *pageablePreflight) GetSession(_ context.Context, id int64) (agents.ChatSession, error) {
	return agents.ChatSession{ID: id, AgentID: id - 1000, SessionKey: "pool-key"}, nil
}

func (p *pageablePreflight) ListSessions(ctx context.Context, agentID int64) ([]agents.ChatSession, error) {
	sess, _ := p.LatestOrCreateSession(ctx, agentID, "")
	return []agents.ChatSession{sess}, nil
}

func (p *pageablePreflight) MarkSessionRead(context.Context, int64) error { return nil }

func (p *pageablePreflight) ListRecentChatMessages(context.Context, int64, int) ([]agents.ChatMessage, error) {
	return nil, nil
}

// followAgentPage GETs an agent page as the signed-in operator, following the
// bare URL's one 303 to its session, and returns the page.
func followAgentPage(t *testing.T, s *Server, h http.Handler, path string) string {
	t.Helper()
	for range 2 {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		admit(s, req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		switch rec.Code {
		case http.StatusOK:
			return rec.Body.String()
		case http.StatusSeeOther:
			path = rec.Header().Get("Location")
		default:
			t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body)
		}
	}
	t.Fatalf("GET %s: more than one redirect", path)
	return ""
}
