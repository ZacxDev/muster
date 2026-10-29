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
// card that draws a Dispatch button when the server can dispatch. Every method
// but the three the board and the dispatch sheet read panics (the embedded nil
// interface), so a read this test did not anticipate fails loudly.
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
// install ships (MUSTER_AGENT_PROVISIONER unset, so `none`).
func dispatchOfferServer(t *testing.T, prov Provisioner) http.Handler {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(os.Stderr, "", 0))
	s.UseExtensions(Extensions{Notes: offerNotes{}, Provisioner: prov})
	h := s.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		admit(s, r)
		h.ServeHTTP(w, r)
	})
}

func offerGet(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s answered %d, want 200: %s", path, rec.Code, truncate(rec.Body.String(), 300))
	}
	return rec.Body.String()
}

// TestTheUIOffersDispatchOnlyWhenTheServerCanDispatch: a server with no agent
// provisioner answers POST /agents with requireLifecycleProvisioner's 503, so
// no surface it draws may offer a dispatch — not the sidebar's "+", not the
// onboarding page's button, not a task card's Dispatch button — and the dispatch sheet, if reached anyway,
// explains what is missing instead of drawing a form that can only fail. With
// a provisioner wired, all three are offered as before.
//
// Every surface is read through the real handler chain, so the assertion is on
// what a browser receives, and both shapes are asserted, so a gate that hid
// the offer everywhere would fail the wired half.
//
// Red at origin/main: the unwired server drew the "+", the card's Dispatch and
// the dispatch form, the shipped defect on a docker compose install.
func TestTheUIOffersDispatchOnlyWhenTheServerCanDispatch(t *testing.T) {
	plus := `id="` + ui.NavDeployID + `"`
	cardDispatch := `hx-get="/ui/agents/new?`
	sheetForm := `hx-post="/agents"`
	sheetExplains := `data-dispatch-unavailable`

	for _, tc := range []struct {
		name  string
		prov  Provisioner
		offer bool
	}{
		{"no provisioner (compose default)", nil, false},
		{"provisioner wired", stubProvisioner{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := dispatchOfferServer(t, tc.prov)

			// The "+" beside the sidebar's Agents label, and the onboarding page's
			// button, on GET /agents (home has nothing to open on this server, so
			// it draws the onboarding page).
			doc := offerGet(t, h, "/agents")
			if got := strings.Contains(doc, plus); got != tc.offer {
				t.Errorf("GET /agents draws the sidebar's dispatch + = %t, want %t", got, tc.offer)
			}
			if got := strings.Contains(doc, `id="`+ui.HomeDispatchID+`"`); got != tc.offer {
				t.Errorf("GET /agents draws the onboarding page's dispatch button = %t, want %t", got, tc.offer)
			}
			// Where the "+" is absent the sidebar says why (and links the guide);
			// where it is drawn, nothing does.
			if got := strings.Contains(doc, `data-dispatch-off-hint`); got == tc.offer {
				t.Errorf("GET /agents draws the dispatch-not-set-up hint = %t, want %t", got, !tc.offer)
			}
			// POSITIVE CONTROL: the sidebar's agent list the "+" heads is drawn in
			// both shapes, so an absent "+" is the gate and not a document
			// missing its sidebar.
			if !strings.Contains(doc, `id="`+ui.NavAgentsListID+`"`) {
				t.Fatalf("GET /agents drew no sidebar agent list at all; the + assertion above is vacuous")
			}

			// A task card's Dispatch button, on the board partial.
			board := offerGet(t, h, "/ui/tasks")
			if got := strings.Contains(board, cardDispatch); got != tc.offer {
				t.Errorf("GET /ui/tasks draws the card's Dispatch = %t, want %t", got, tc.offer)
			}
			// POSITIVE CONTROL: the card itself is on the board in both shapes.
			if !strings.Contains(board, offerTask.Title) {
				t.Fatalf("GET /ui/tasks drew no card for the task; the Dispatch assertion above is vacuous")
			}

			// The same card on the task's own page, built by the single-card
			// builder (taskCardView) that every card re-render also uses.
			detail := offerGet(t, h, "/tasks/4242")
			if got := strings.Contains(detail, cardDispatch); got != tc.offer {
				t.Errorf("GET /tasks/4242 draws the card's Dispatch = %t, want %t", got, tc.offer)
			}
			if !strings.Contains(detail, offerTask.Title) {
				t.Fatalf("GET /tasks/4242 drew no card for the task; the Dispatch assertion above is vacuous")
			}

			// The dispatch sheet: the task-less one ("+") and the task-scoped one
			// (a card's Dispatch). A stale page can still request either.
			for _, path := range []string{"/ui/agents/new", "/ui/agents/new?note=4242"} {
				sheet := offerGet(t, h, path)
				if got := strings.Contains(sheet, sheetForm); got != tc.offer {
					t.Errorf("GET %s draws the dispatch form = %t, want %t", path, got, tc.offer)
				}
				if got := strings.Contains(sheet, sheetExplains); got == tc.offer {
					t.Errorf("GET %s draws the not-configured explanation = %t, want %t", path, got, !tc.offer)
				}
			}
		})
	}
}
