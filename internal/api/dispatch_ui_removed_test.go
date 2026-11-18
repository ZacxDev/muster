package api

import (
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/ui"
)

// offerNotes is a notes store holding one open task with no linked agent: the
// card that drew a Dispatch button when the server could dispatch, before
// docs/design/chief-dispatch.md phase 2 deleted it. Every method but the three
// the board and the task page read panics (the embedded nil interface), so a
// read this test did not anticipate fails loudly.
type offerNotes struct{ notes.Store }

var offerTask = notes.Note{ID: 4242, Title: "Offer probe task", Body: "synthetic body", Status: string(notes.StatusOpen)}

func (offerNotes) ListPage(context.Context, notes.ListFilter) (notes.Page, error) {
	return notes.Page{Notes: []notes.Note{offerTask}, Total: 1}, nil
}
func (offerNotes) TagVocabulary(context.Context) ([]notes.TagCount, error) { return nil, nil }
func (offerNotes) Get(context.Context, int64) (notes.Note, error)          { return offerTask, nil }

// dispatchOfferServer is a server with a notes store and, when prov is non-nil,
// an agent provisioner — and nothing else, so the provisioner is the only
// dependency the two shapes differ on. A nil prov is what the docker compose
// install ships (MUSTER_AGENT_PROVISIONER unset, so `none`). Every request is
// admitted as the operator's browser session.
func dispatchOfferServer(t *testing.T, prov Provisioner) (*Server, http.Handler) {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(os.Stderr, "", 0))
	s.UseExtensions(Extensions{Notes: offerNotes{}, Provisioner: prov})
	h := s.Handler()
	return s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			admit(s, r)
		}
		h.ServeHTTP(w, r)
	})
}

func offerDo(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func offerGet(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	rec := offerDo(h, http.MethodGet, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s answered %d, want 200: %s", path, rec.Code, truncate(rec.Body.String(), 300))
	}
	return rec.Body.String()
}

// TestTheDispatchUIIsGoneAndTheMachineRouteReplacesIt pins
// docs/design/chief-dispatch.md phase 2 through the real handler chain, on a
// server with no agent provisioner AND on one with a provisioner wired (the
// shape that used to draw every dispatch surface):
//
//   - GET /ui/agents/new and GET /ui/agents/notes answer 404, and POST /agents
//     405 (GET /agents, home, is still registered on that path, so the mux
//     answers Method Not Allowed with an Allow that names no POST) — each with
//     the operator's own session, so the answer is the route being gone and not
//     the session gate;
//   - no document offers a dispatch: no sidebar "+" (#sidebar-deploy), no
//     "not set up" hint, no home-page button (#home-deploy), no task-card
//     Dispatch (D8: nothing replaces them);
//   - the replacement is reachable: POST /api/agents with an agent-create token
//     creates an agent where a provisioner is wired, and answers the
//     provisioner 503 where none is.
//
// Red at the parent commit: every route answered (200, 200 and a create), and
// the wired server drew the "+", the button and the card's Dispatch.
func TestTheDispatchUIIsGoneAndTheMachineRouteReplacesIt(t *testing.T) {
	gone := []string{`id="sidebar-deploy"`, `id="home-deploy"`, `data-dispatch-off-hint`, `/ui/agents/new`,
		`hx-post="/agents"`, `value="save"`, `id="agent-modal"`}
	for _, tc := range []struct {
		name string
		prov Provisioner
	}{
		{"no provisioner (compose default)", nil},
		{"provisioner wired", stubProvisioner{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, h := dispatchOfferServer(t, tc.prov)

			for _, rt := range []struct{ method, path string }{
				{http.MethodGet, "/ui/agents/new"},
				{http.MethodGet, "/ui/agents/new?note=4242"},
				{http.MethodGet, "/ui/agents/notes"},
			} {
				if rec := offerDo(h, rt.method, rt.path); rec.Code != http.StatusNotFound {
					t.Errorf("%s %s answered %d, want 404 (the route is removed): %s", rt.method, rt.path, rec.Code, truncate(rec.Body.String(), 200))
				}
			}
			if rec := offerDo(h, http.MethodPost, "/agents"); rec.Code != http.StatusMethodNotAllowed || strings.Contains(rec.Header().Get("Allow"), "POST") {
				t.Errorf("POST /agents answered %d (Allow %q), want 405 with no POST allowed (the route is removed)", rec.Code, rec.Header().Get("Allow"))
			}
			// POSITIVE CONTROL for the 404s: the same session reaches a route
			// that stays (the repository options the task forms use).
			if rec := offerDo(h, http.MethodGet, "/ui/agents/repos"); rec.Code != http.StatusOK {
				t.Fatalf("CONTROL: GET /ui/agents/repos answered %d; the 404s above may be the session, not the route", rec.Code)
			}

			docs := map[string]string{
				// Home has nothing to open on this server, so it draws the
				// onboarding page, inside the sidebar.
				"GET /agents":     offerGet(t, h, "/agents"),
				"GET /ui/tasks":   offerGet(t, h, "/ui/tasks"),
				"GET /tasks/4242": offerGet(t, h, "/tasks/4242"),
			}
			// POSITIVE CONTROLS: the sidebar's agent list and the task card are
			// drawn, so an absence below is the surface being gone and not a
			// document missing its sidebar or its card.
			if !strings.Contains(docs["GET /agents"], `id="`+ui.NavAgentsListID+`"`) || !strings.Contains(docs["GET /agents"], `id="`+ui.HomeEmptyID+`"`) {
				t.Fatalf("GET /agents drew no sidebar agent list or no onboarding page; the absence checks are vacuous")
			}
			for _, k := range []string{"GET /ui/tasks", "GET /tasks/4242"} {
				if !strings.Contains(docs[k], offerTask.Title) {
					t.Fatalf("%s drew no card for the task; the absence checks are vacuous", k)
				}
			}
			for name, doc := range docs {
				for _, g := range gone {
					if strings.Contains(doc, g) {
						t.Errorf("%s still carries %q", name, g)
					}
				}
			}

			// The replacement, on the same deployment shape with an agent store
			// (the create path writes a row).
			var rec *httptest.ResponseRecorder
			if tc.prov == nil {
				rec = apiCreateAgent(t, s, h, map[string]any{"prompt": "probe", "start": false})
			} else {
				ps, ph := preflightServer(t, newPreflightProvisioner(""), &preflightStore{})
				rec = apiCreateAgent(t, ps, ph, map[string]any{"prompt": "probe", "start": false})
			}
			if tc.prov == nil {
				if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), ProvisionerUnwiredField) {
					t.Errorf("POST /api/agents with no provisioner: %d %s; want the provisioner 503", rec.Code, rec.Body)
				}
				return
			}
			if rec.Code != http.StatusCreated {
				t.Errorf("POST /api/agents with an agent-create token: %d %s; want 201", rec.Code, rec.Body)
			}
		})
	}
}
