package api

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// ---------------------------------------------------------------------------
// AN OWED KICKOFF IS REPORTED ON BOTH TIERS, FROM ONE PREDICATE, WITHOUT THE
// NOTE'S TEXT.
//
// 🔴 EVERY ASSERTION HERE GOES THROUGH THE REAL MUX. The two tiers are built by
// different code (internal/ui's agentCard via cardViewIndexed; agentJSON via
// newAgentJSON) from one store row, which is exactly the seam neither package's
// own suite can hold: internal/ui has no store and internal/agents has no HTTP
// layer. A unit test of either composer would have passed with the other tier
// silently blind — which is the state that shipped for agents.pending_note, a
// column with no reader on any tier at all.
//
// WHICH GUARD IS WHICH:
//
//	TestBothTiersReportAnOwedKickoffForTheSameRow        REGRESSION (relationship)
//	TestAnOwedKickoffIsReportedWithoutTheNotesText       MIXED — see its own doc:
//	                                                     the "this surface reports
//	                                                     the state" half is
//	                                                     regression, the "and not
//	                                                     the text" half is an
//	                                                     INVARIANT GUARD the bug
//	                                                     never violated.
//	TestOneCardComposerFeedsEverySurface                 INVARIANT GUARD
// ---------------------------------------------------------------------------

// owedStore serves a fixed list of agents to both tiers.
//
// ⚠ IT EMBEDS agents.Store, so a surface that grows a new store read panics
// rather than reading a zero value — the choice preflightStore records.
type owedStore struct {
	agents.Store
	rows []agents.Agent
}

func (s *owedStore) List(context.Context) ([]agents.Agent, error) {
	return append([]agents.Agent(nil), s.rows...), nil
}

func (s *owedStore) Get(_ context.Context, id int64) (agents.Agent, error) {
	for _, a := range s.rows {
		if a.ID == id {
			return a, nil
		}
	}
	return agents.Agent{}, os.ErrNotExist
}

func (s *owedStore) LastMessageByAgentIDs(context.Context, []int64) (map[int64]time.Time, error) {
	return nil, nil
}

// owedKickoffNote is the fixture's pending note.
//
// 🔴 IT IS A DISTINCTIVE, UNMISTAKABLE STRING AND NOT PLACEHOLDER PROSE. The leak
// assertion searches both responses for it, so a generic value ("test note") could
// plausibly appear in markup for an unrelated reason and the search would report a
// leak that is not one — or worse, a value that is a SUBSTRING of something the
// page always emits would make the test unable to distinguish the two.
//
// ⚠ IT ALSO DELIBERATELY DOES NOT LOOK LIKE REAL OPERATOR INSTRUCTIONS. This is a
// public repository with a leak gate (tests/leakscan.py); a fixture that reads
// like a real dispatch is a different hazard from the one being guarded.
const owedKickoffNote = "ZZQX-pending-note-sentinel-ZZQX"

// owedFixtureRows is the three-row fixture every test below shares.
//
// 🔴 THE THREE ROWS ARE THE THREE STATES A BADGE MUST TELL APART, and row 1 is the
// defect: a `running` agent whose first message was never delivered. Row 2 is the
// control the live board actually contains (a delivered kickoff whose note text is
// still on the row — PendingNote is never cleared), and row 3 is an agent that was
// never asked for a first turn at all.
func owedFixtureRows() []agents.Agent {
	return []agents.Agent{
		{ID: 1, Name: "owed-running", DisplayName: "owed-running", Namespace: "devpod-owed-running",
			Status: agents.StatusRunning, PendingNote: owedKickoffNote, KickedOff: false},
		{ID: 2, Name: "delivered", DisplayName: "delivered", Namespace: "devpod-delivered",
			Status: agents.StatusRunning, PendingNote: owedKickoffNote, KickedOff: true},
		{ID: 3, Name: "no-note", DisplayName: "no-note", Namespace: "devpod-no-note",
			Status: agents.StatusStopped, PendingNote: "", KickedOff: false},
	}
}

// owedServer wires both tiers over one store, with NO provisioner — so the
// displayed status is the stored one (liveStatusIndexed's nil-index fallback) and
// these tests are about the kickoff signal rather than about live reconciliation.
func owedServer(t *testing.T) (*Server, http.Handler, *owedStore) {
	t.Helper()
	store := &owedStore{rows: owedFixtureRows()}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken},
		log.New(os.Stderr, "", 0))
	s.UseExtensions(Extensions{Agents: store, SessionLiveness: stubLiveness{}})
	return s, s.Handler(), store
}

// getOwed drives one GET through the production chain with both credentials.
func getOwed(t *testing.T, s *Server, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	admit(s, req)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200. body: %s", path, rec.Code, rec.Body.String())
	}
	return rec
}

// cardsByAgent splits the rendered card list into one chunk per agent card, keyed
// by the agent's slug.
//
// 🔴 PER-CARD, BECAUSE A DOCUMENT-WIDE SUBSTRING CHECK CANNOT ATTRIBUTE. "The list
// contains a badge" is satisfied by a badge on the WRONG agent — which is exactly
// the failure a predicate wired to the wrong field produces — so every assertion
// below names the card it is about.
func cardsByAgent(t *testing.T, markup string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, a := range owedFixtureRows() {
		marker := `href="/agents/` + a.Name + `"`
		i := strings.Index(markup, marker)
		if i < 0 {
			t.Fatalf("the rendered list has no card for %q (looked for %s).\nlist:\n%s",
				a.Name, marker, markup)
		}
		rest := markup[i:]
		if j := strings.Index(rest, "</article>"); j >= 0 {
			rest = rest[:j]
		}
		out[a.Name] = rest
	}
	return out
}

// TestBothTiersReportAnOwedKickoffForTheSameRow is the relationship guard.
//
// 🔴 IT PINS THAT THE TWO TIERS AGREE, NOT THAT EACH WORKS. Either surface alone
// can be correct while the other is blind, and the blindness is the defect: a
// machine caller that cannot see an owed kickoff is the same hole one layer down
// from an operator who cannot. Asserting the HTML badge and the JSON boolean per
// agent, in one test, over one store, is what makes a divergence fail.
//
// `running` is asserted explicitly on row 1 because that pairing — a healthy
// status next to an owed turn — is the state that fooled everyone.
func TestBothTiersReportAnOwedKickoffForTheSameRow(t *testing.T) {
	s, h, _ := owedServer(t)

	cards := cardsByAgent(t, getOwed(t, s, h, "/ui/agents").Body.String())

	var wire []agentJSON
	body := getOwed(t, s, h, "/api/agents").Body.Bytes()
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("decode GET /api/agents: %v\nbody: %s", err, body)
	}
	if len(wire) != len(owedFixtureRows()) {
		t.Fatalf("GET /api/agents returned %d agents, want %d — the rest of this test "+
			"would be a claim about a list that is not the fixture", len(wire), len(owedFixtureRows()))
	}
	byName := map[string]agentJSON{}
	for _, a := range wire {
		byName[a.Name] = a
	}

	for _, want := range []struct {
		agent  string
		owed   bool
		status string
		why    string
	}{
		{"owed-running", true, agents.StatusRunning,
			"the measured defect: a live, ready, `running` agent whose first message was " +
				"never handed to a gateway. Both tiers must say so."},
		{"delivered", false, agents.StatusRunning,
			"PendingNote is never cleared after delivery, so a predicate keyed on the note " +
				"alone would flag every agent that ever started."},
		{"no-note", false, agents.StatusStopped,
			"nothing was ever asked for, so nothing is outstanding — a predicate keyed on " +
				"!KickedOff alone would badge this."},
	} {
		t.Run(want.agent, func(t *testing.T) {
			card, ok := cards[want.agent]
			if !ok {
				t.Fatalf("no card for %s", want.agent)
			}
			gotBadge := strings.Contains(card, "data-kickoff-owed")
			if gotBadge != want.owed {
				t.Errorf("the CARD for %s shows the owed-kickoff badge = %v, want %v.\n  %s\ncard:\n%s",
					want.agent, gotBadge, want.owed, want.why, card)
			}
			if !strings.Contains(card, `aria-label="Status: `+want.status+`"`) {
				t.Errorf("the card for %s does not render status %q; the badge is ADDITIVE "+
					"and the status must still be there.\ncard:\n%s", want.agent, want.status, card)
			}

			row, ok := byName[want.agent]
			if !ok {
				t.Fatalf("GET /api/agents has no entry for %s", want.agent)
			}
			if row.KickoffOwed != want.owed {
				t.Errorf("the MACHINE tier reports kickoffOwed = %v for %s, want %v.\n  %s\n"+
					"    A machine caller cannot derive this: the other conjunct is "+
					"agents.pending_note, which is json:\"-\" and reaches no surface, so "+
					"`kickedOff: false` alone cannot separate an owed turn from an agent "+
					"that was never asked to take one.", row.KickoffOwed, want.agent, want.owed, want.why)
			}
			if row.Status != want.status {
				t.Errorf("the machine tier reports status %q for %s, want %q",
					row.Status, want.agent, want.status)
			}
			// The two tiers must not merely each be right — they must AGREE.
			if gotBadge != row.KickoffOwed {
				t.Errorf("the two tiers DISAGREE about %s: card badge = %v, wire "+
					"kickoffOwed = %v. They are composed by different code from one row; "+
					"a divergence is a surface that silently says the opposite.",
					want.agent, gotBadge, row.KickoffOwed)
			}
		})
	}

	// POSITIVE CONTROL for the extraction itself: a zero badge count across the
	// whole list would be indistinguishable from a parser wired to nothing.
	owedCards := 0
	for _, c := range cards {
		if strings.Contains(c, "data-kickoff-owed") {
			owedCards++
		}
	}
	if owedCards != 1 {
		t.Errorf("exactly one of the three fixture agents is owed a kickoff; the card "+
			"extraction found %d badged cards. One is the measurement; zero would mean the "+
			"splitter matched nothing and every assertion above passed vacuously.", owedCards)
	}
	t.Logf("control: 1 badged card of 3, and 1 kickoffOwed:true of 3 on the wire")
}

// TestAnOwedKickoffIsReportedWithoutTheNotesText is the leak-shaped guard.
//
// 🔴 THE SIGNAL IS A BOOLEAN AND THE NOTE IS OPERATOR-AUTHORED INSTRUCTION TEXT.
// agents.Agent tags PendingNote `json:"-"` and that must stay true through BOTH
// tiers — a card template or a DTO field that carried the text would publish
// whatever an operator typed into the dispatch box, from a public-repo service.
// The fixture's note is a sentinel precisely so this search can be exact.
//
// ⚠ IT ALSO CHECKS THE SENTINEL IS REACHABLE AT ALL. A search for a string the
// fixture never stored is a guaranteed pass; the store is re-read and the row
// asserted to hold it, so the absence below is a measurement.
//
// 🔴 THE TWO HALVES HAVE DIFFERENT STRENGTHS, AND SAYING SO IS THE HONEST READING
// OF A GREEN. The JSON half is mutation-proven: adding a `pendingNote` field to
// agentJSON and setting it from the row kills this test on its own message. The
// HTML half CANNOT fail today for a reason stronger than this test —
// ui.AgentCardView has no note field at all, so no single edit inside internal/ui
// can leak the text. It is here as a guard against the field being ADDED, which is
// the realistic route (a card that wanted to show a preview), and it is a claim
// about the whole response body rather than about one element.
func TestAnOwedKickoffIsReportedWithoutTheNotesText(t *testing.T) {
	s, h, store := owedServer(t)

	// Reachability: the note really is on the row these responses are built from.
	row, err := store.Get(context.Background(), 1)
	if err != nil {
		t.Fatalf("fixture row 1: %v", err)
	}
	if row.PendingNote != owedKickoffNote {
		t.Fatalf("the fixture row does not hold the sentinel note, so searching the "+
			"responses for it proves nothing. got %q", row.PendingNote)
	}
	if !agents.KickoffOwed(row) {
		t.Fatal("the fixture row is not owed a kickoff, so the surfaces under test are " +
			"not even rendering the state this guard is about")
	}

	for _, path := range []string{"/ui/agents", "/api/agents", "/ui/agents/1/card"} {
		got := getOwed(t, s, h, path).Body.String()
		if strings.Contains(got, owedKickoffNote) {
			t.Errorf("%s emits the pending note's TEXT.\n"+
				"    Only the BOOLEAN may leave the process: the note is whatever the "+
				"operator typed into the dispatch box, and this service's repository is "+
				"public. See agents.Agent.PendingNote's `json:\"-\"` and tests/leakscan.py.\n"+
				"body:\n%s", path, got)
		}
		// And the tier still reports the state — otherwise "no leak" is satisfied
		// by a surface that says nothing at all.
		if !strings.Contains(got, "kickoff") && !strings.Contains(got, "kickoffOwed") {
			t.Errorf("%s mentions no kickoff signal at all, so its clean leak result is "+
				"a claim about an empty surface", path)
		}
	}
}

// TestOneCardComposerFeedsEverySurface pins the consolidation in agents.go.
//
// ⚠ INVARIANT GUARD, NOT REGRESSION COVERAGE. The owed-kickoff defect did not
// violate it — the two composers agreed at the time, because neither set the field.
// It is here because the DUPLICATE is what makes the next additive card field go
// out of sync: handleAgentsContent built the list's AgentCardView literal and
// cardViewFor built the single-card re-render's, the same eleven fields spelled
// twice. The observable of a drift is a badge that appears on the list and
// disappears when the card is re-rendered (the inline rename's Cancel, GET
// /ui/agents/{id}/card) — which reads as "the warning went away".
//
// 🔴 IT COUNTS COMPOSITE LITERALS THAT SET Status, NOT ALL OF THEM. The rename
// form legitimately builds a ui.AgentCardView with three identity fields and no
// status — it renders an edit form, not a card — so a blanket count would be red on
// correct code.
func TestOneCardComposerFeedsEverySurface(t *testing.T) {
	// Behavioural half: the list and the single-card route render the SAME badge
	// state for the same row. Two composers that drift produce two answers here.
	s, h, _ := owedServer(t)
	list := cardsByAgent(t, getOwed(t, s, h, "/ui/agents").Body.String())
	single := getOwed(t, s, h, "/ui/agents/1/card").Body.String()

	fromList := strings.Contains(list["owed-running"], "data-kickoff-owed")
	fromCard := strings.Contains(single, "data-kickoff-owed")
	if !fromList || !fromCard || fromList != fromCard {
		t.Errorf("the list and the single-card re-render disagree about the owed badge "+
			"for one row: list = %v, /ui/agents/1/card = %v (both want true).\n"+
			"    These are the two surfaces an operator sees in one session — the board, "+
			"and the card that comes back after a rename is cancelled.\ncard:\n%s",
			fromList, fromCard, single)
	}

	// Structural half: exactly one place in this package composes a card view with
	// a status. A second one is a second chance to forget a field.
	sites := cardViewLiteralSites(t)
	if len(sites) != 1 {
		t.Errorf("ui.AgentCardView is composed WITH A STATUS at %d site(s), want exactly "+
			"1 (cardViewIndexed, agents.go):\n  %s\n\n"+
			"    Every card surface must come from one composer, or an additive field is "+
			"set on one surface and dropped on the other.", len(sites), strings.Join(sites, "\n  "))
		return
	}
	if !strings.Contains(sites[0], "agents.go") {
		t.Errorf("the card composer lives at %s; cardViewIndexed in agents.go is the only "+
			"place a card view may be built", sites[0])
	}
}

// cardViewLiteralSites returns file:line for every `ui.AgentCardView{...}`
// composite literal in this package's non-test sources that sets the Status
// field.
//
// 🔴 THE Status KEY IS THE FILTER, AND IT IS WHAT KEEPS THIS GATE OFF CORRECT
// CODE. handleAgentRenameForm builds a ui.AgentCardView holding only ID/Name/
// DisplayName to render the inline rename FORM — no status, no card — so counting
// every literal would be red from day one, and a permanently-red gate is worse
// than no gate.
//
// ⚠ IT MATCHES A COMPOSITE LITERAL, so a card assembled field-by-field
// (`var v ui.AgentCardView; v.Status = …`) is invisible to it. Nothing in this
// package is written that way; the human writing it is the only control.
func cardViewLiteralSites(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var sites []string
	parsed, literals := 0, 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		parsed++
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "AgentCardView" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "ui" {
				return true
			}
			literals++
			for _, el := range lit.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Status" {
					sites = append(sites, fset.Position(lit.Pos()).String())
					return true
				}
			}
			return true
		})
	}
	if parsed < 10 {
		t.Fatalf("the scanner parsed only %d source file(s); it is wired to nothing", parsed)
	}
	// POSITIVE CONTROL: the scanner must SEE AgentCardView literals at all.
	// Without this, a renamed type or a wrong package qualifier yields zero
	// status-bearing sites and the "exactly 1" assertion fails with a message
	// about consolidation when the instrument is what broke.
	if literals == 0 {
		t.Fatalf("the scanner found NO ui.AgentCardView composite literals across %d "+
			"file(s) — it is matching the wrong shape, so its count says nothing about "+
			"consolidation", parsed)
	}
	return sites
}
