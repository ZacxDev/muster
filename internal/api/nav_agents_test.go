package api

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ui"
)

// HOME OPENS THE CHIEF'S CHAT (nav_agents.go redirectHome), and the sidebar's
// agent list is served by GET ui.NavAgentsPath.

// homeAgents is an agent store holding rows, each agent's latest session
// (latest) and, when set, a List that fails.
type homeAgents struct {
	agents.Store
	rows    []agents.Agent
	latest  map[int64]int64
	listErr error
}

func (s *homeAgents) List(context.Context) ([]agents.Agent, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return append([]agents.Agent(nil), s.rows...), nil
}

func (s *homeAgents) LastMessageByAgentIDs(context.Context, []int64) (map[int64]time.Time, error) {
	return map[int64]time.Time{}, nil
}

func (s *homeAgents) LatestSessionIDs(_ context.Context, ids []int64) (map[int64]int64, error) {
	out := map[int64]int64{}
	for _, id := range ids {
		if sid, ok := s.latest[id]; ok {
			out[id] = sid
		}
	}
	return out, nil
}

// The fixture's agents: ids, slugs and session ids pairwise distinct, none 1,
// so no answer can come out right by a zero value or a first row. The chief is
// NOT the first row and NOT the lowest id among the rows, so "the first agent"
// and "the chief" are different answers.
const (
	homeWorkerID  int64 = 17
	homeChiefID   int64 = 23
	homeChiefSess int64 = 91
)

func homeRows(withChief bool) []agents.Agent {
	rows := []agents.Agent{{ID: homeWorkerID, Name: "brave-otter", DisplayName: "worker", Status: agents.StatusRunning}}
	if withChief {
		rows = append(rows, agents.Agent{ID: homeChiefID, Name: "zesty-stoat", DisplayName: " Chief ", Status: agents.StatusRunning})
	}
	return rows
}

// homeServer is a server over store (nil: no agent store at all).
func homeServer(t *testing.T, store agents.Store) (*Server, http.Handler) {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Agents: store, SessionLiveness: stubLiveness{}})
	return s, s.Handler()
}

func homeGet(s *Server, h http.Handler, path string, hdr map[string]string) *httptest.ResponseRecorder {
	r := operatorRequest(s, http.MethodGet, path)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// chiefHref is the chief's page as the fixture's list would link it.
var chiefHref = ui.AgentSessionHref("zesty-stoat", homeChiefSess)

// TestHomeOpensTheChiefsChat: GET / sends a browser to the chief agent's page
// on its latest session (302), an htmx request the same place by HX-Redirect,
// and — with no chief to open — to the Agents list. `/?shell=1` (the service
// worker's offline copy) and /tasks still serve the shell.
//
// Red on the base tree: GET / served the Tasks shell (200) in every case.
func TestHomeOpensTheChiefsChat(t *testing.T) {
	withChief := &homeAgents{rows: homeRows(true), latest: map[int64]int64{homeChiefID: homeChiefSess, homeWorkerID: 55}}
	cases := []struct {
		name  string
		store agents.Store
		want  string
	}{
		{"a chief", withChief, chiefHref},
		{"no agent labelled chief", &homeAgents{rows: homeRows(false), latest: map[int64]int64{homeWorkerID: 55}}, "/agents"},
		{"the agent list cannot be read", &homeAgents{rows: homeRows(true), listErr: errors.New("db down")}, "/agents"},
		{"no agent store", nil, "/agents"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, h := homeServer(t, tc.store)
			rec := homeGet(s, h, "/", nil)
			if rec.Code != http.StatusFound || rec.Header().Get("Location") != tc.want {
				t.Fatalf("GET / = %d → %q, want 302 → %q", rec.Code, rec.Header().Get("Location"), tc.want)
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("GET /: Cache-Control = %q, want no-store (home follows who the chief is now)", cc)
			}
			// A boosted request (the header's wordmark): no 3xx, which htmx
			// would follow inside its XHR — HX-Redirect, and no body to swap.
			hx := homeGet(s, h, "/", map[string]string{"HX-Request": "true", "HX-Boosted": "true"})
			if hx.Code != http.StatusOK || hx.Header().Get("HX-Redirect") != tc.want || hx.Body.Len() != 0 {
				t.Errorf("htmx GET / = %d, HX-Redirect %q, %d body bytes; want 200, %q, none",
					hx.Code, hx.Header().Get("HX-Redirect"), hx.Body.Len(), tc.want)
			}
		})
	}

	// The shell is still served where it was meant to be: its offline copy
	// (fetched with redirect:'manual', which refuses any redirect) and the
	// board's own address — WITH a chief present, so a redirect was possible.
	s, h := homeServer(t, withChief)
	for _, path := range []string{"/?" + offlineShellParam + "=1", "/tasks"} {
		rec := homeGet(s, h, path, nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="panel-tasks"`) {
			t.Errorf("GET %s = %d (Location %q), want 200 and the Tasks shell", path, rec.Code, rec.Header().Get("Location"))
		}
	}
}

// TestTheNavListMarksThePageItWasAskedFrom: GET ui.NavAgentsPath lists the
// viewer's agents, the chief first, and marks the row of the page htmx names
// in HX-Current-URL — and none for a page that is no session.
func TestTheNavListMarksThePageItWasAskedFrom(t *testing.T) {
	s, h := homeServer(t, &homeAgents{rows: homeRows(true), latest: map[int64]int64{homeChiefID: homeChiefSess, homeWorkerID: 55}})
	for _, tc := range []struct{ from, want string }{
		{"http://muster.example.test/agents/brave-otter?session=55", "brave-otter"},
		{"http://muster.example.test/agents/zesty-stoat/terminal", "zesty-stoat"},
		{"http://muster.example.test/tasks", ""},
		{"", ""},
	} {
		rec := homeGet(s, h, ui.NavAgentsPath, map[string]string{"HX-Request": "true", "HX-Current-URL": tc.from})
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s from %q = %d: %s", ui.NavAgentsPath, tc.from, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		// The chief is the first row (the fixture lists it second).
		first := strings.Index(body, `href="/agents/`)
		if first < 0 || !strings.HasPrefix(body[first:], `href="/agents/zesty-stoat`) {
			t.Errorf("from %q: the first row is not the chief: %.300s", tc.from, body)
		}
		var got []string
		for _, name := range []string{"brave-otter", "zesty-stoat"} {
			i := strings.Index(body, `href="/agents/`+name)
			if i < 0 {
				t.Fatalf("from %q: no row for %s", tc.from, name)
			}
			tag := body[i : i+strings.Index(body[i:], ">")]
			if strings.Contains(tag, "data-nav-current") {
				got = append(got, name)
			}
		}
		if want := tc.want; (want == "" && len(got) != 0) || (want != "" && (len(got) != 1 || got[0] != want)) {
			t.Errorf("from %q: current rows %v, want %q", tc.from, got, want)
		}
	}
	// No agent store: an empty list, not an error.
	s, h = homeServer(t, nil)
	if rec := homeGet(s, h, ui.NavAgentsPath, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "data-nav-agents-empty") {
		t.Errorf("no agent store: GET %s = %d %q, want 200 and the empty list", ui.NavAgentsPath, rec.Code, rec.Body.String())
	}
}
