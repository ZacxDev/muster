package ui

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/ZacxDev/muster/internal/notes"
)

// --- F1: two unsynchronised writers to #task-{id}, and the version that orders
// --- them ---------------------------------------------------------------------
//
// 🔴 THE HAZARD, MEASURED. #task-{id} is written by more than one request and
// none of them wait for each other:
//
//	195ms REQ  POST /agents
//	233ms REQ  GET /ui/tasks/1/card      <- SSE-triggered, BEFORE the POST returns
//	240ms RESP 200 /ui/tasks/1/card  status=open        <- read before the commit
//	278ms RESP 200 /agents           status=in_progress <- authoritative
//
// Both are morph:outerHTML into the same node, 38ms apart, and until data-card-rev
// existed the LAST ARRIVAL won regardless of which had read the store last.
// Reverse the two and the page shows `open` for a task the store has as
// `in_progress`, with nothing to correct it: taskDetailLive re-fetches only on the
// NEXT SSE event or a focus/visibility resync. That is the cause of the flaky e2e
// spec "Dispatch from a task page actually dispatches".
//
// The fix is a VERSION on the card, not a delay anywhere. These tests hold the
// RENDER half (the attribute is emitted, on the swap target, from the store's own
// updated_at). The BEHAVIOURAL half — a stale response forced to arrive last, and
// the card still showing the authoritative status — is
// e2e/tests/tasks.spec.ts "a stale card response that arrives LAST does not win",
// because only a browser can order two real responses.

// staleCardGuardName is the identifier resyncScript gives its htmx:beforeSwap
// handler. It exists so a document-level check can ask "is THIS guard here?"
// rather than "is SOME beforeSwap listener here?" — several scripts in the app
// attach one, and the weaker question answers yes on a page that has lost it.
const staleCardGuardName = "musterRefuseStaleCardSwap"

// cardRevOf parses a rendered card and returns the data-card-rev on the element
// whose id is the SWAP TARGET. It is deliberately not a substring search: the
// attribute only does its job if it sits on the node htmx replaces.
func cardRevOf(t *testing.T, markup, wantID string) (string, bool) {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(markup))
	if err != nil {
		t.Fatalf("parse card: %v", err)
	}
	var found string
	var ok bool
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			id, rev, hasRev := "", "", false
			for _, a := range n.Attr {
				switch a.Key {
				case "id":
					id = a.Val
				case "data-card-rev":
					rev, hasRev = a.Val, true
				}
			}
			if id == wantID && hasRev {
				found, ok = rev, true
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return found, ok
}

// TestTaskCardCarriesTheStoreRevisionOnItsSwapTarget pins the render half of the
// F1 fix: the card the server sends back for a #task-{id} swap states WHICH
// version of the task it was rendered from, on the element that gets swapped.
//
// The value is notes.updated_at in nanoseconds because the STORE maintains it —
// SetStatus, UpdateNote, AddTags, RemoveTags and AddComment each write
// `updated_at=now()` in the same statement as the change — so it moves forward
// with the task's own data and cannot be stamped by a render that read stale
// rows. TestUpdatedAtIsAMonotonicCardVersion (internal/notes, PG-gated) is the
// other end of that claim: it asks Postgres whether updated_at really advances
// on every card-affecting write, and never goes backwards.
//
// Both shapes are checked. The board card and the detail card are one renderer
// with one predicate between them, but they are also the two things that morph
// into #task-{id}, so a rev on only one of them orders only half the writers.
func TestTaskCardCarriesTheStoreRevisionOnItsSwapTarget(t *testing.T) {
	// Two DISTINCT instants, neither of them the zero time and neither a round
	// number, so an assertion cannot pass by matching a constant the renderer
	// might hardcode.
	older := time.Date(2026, 8, 26, 12, 0, 0, 123456789, time.UTC)
	newer := older.Add(1741 * time.Millisecond)

	for _, shape := range []struct {
		name   string
		detail bool
	}{{"board", false}, {"detail", true}} {
		t.Run(shape.name, func(t *testing.T) {
			var revs []string
			for _, when := range []time.Time{older, newer} {
				n := notes.Note{ID: 4242, Title: "a task", Body: "b", Status: string(notes.StatusOpen), UpdatedAt: when}
				out := renderString(t, noteCard(TaskCardView{Note: n, Detail: shape.detail}))
				rev, ok := cardRevOf(t, out, "task-4242")
				if !ok {
					t.Fatalf("the %s card carries no data-card-rev on #task-4242 — the element htmx swaps.\n"+
						"Without it nothing orders the card's writers: the SSE re-fetch and the mutation "+
						"response both morph:outerHTML into this node and the LAST ARRIVAL wins, which is "+
						"how a dispatch leaves `open` on screen for a task the store has as `in_progress`.\n%s",
						shape.name, out)
				}
				want := strconv.FormatInt(when.UnixNano(), 10)
				if rev != want {
					t.Errorf("the %s card's data-card-rev = %q for updated_at %s, want %q (updated_at in ns).\n"+
						"The rev must be the STORE's version of the task. A render-time clock, a constant, "+
						"or anything the renderer chooses would rank a stale read above a fresh one exactly "+
						"as often as it ranked it below.", shape.name, rev, when, want)
				}
				revs = append(revs, rev)
			}
			if revs[0] == revs[1] {
				t.Errorf("two cards rendered from updated_at %s and %s carry the SAME rev %q — the rev does "+
					"not move with the data, so it can never distinguish a stale card from a fresh one.",
					older, newer, revs[0])
			}
		})
	}
}

// TestResyncScriptRefusesAnOlderCardRevision is a PRESENCE ledger for the client
// half, and says so: it asserts the mechanism is wired, not that it works. A Go
// test cannot run this JS, and the swap it cancels only exists inside htmx.
//
// 🔴 THE BEHAVIOURAL GUARD IS THE E2E ONE — e2e/tests/tasks.spec.ts, "a stale
// card response that arrives LAST does not win": it keeps the page's own
// pre-dispatch card (a real server response, rendered while the task was still
// open), dispatches, waits for the authoritative in_progress to be on screen,
// and only then replays the stale response into #task-{id} with the same
// morph:outerHTML swap. Do not read the test below as coverage of the
// comparison; it would pass against a listener whose comparison were inverted.
//
// What it does buy is that the listener cannot be deleted, or moved off the
// document that needs it, in silence — and the three parts it names are the
// three that make the guard a guard rather than a no-op.
func TestResyncScriptRefusesAnOlderCardRevision(t *testing.T) {
	js := mustStripJSComments(t, jsSource(t, resyncScript()))
	for _, want := range []struct {
		frag, why string
	}{
		{staleCardGuardName, "the guard's identifier is what the document-level seam check matches on"},
		{"htmx:beforeSwap", "the swap must be inspected BEFORE it lands — there is no other point at which it can be refused"},
		{"data-card-rev", "the incoming card's version has to be read out of the response, not inferred"},
		{"shouldSwap", "reading the version and not cancelling the swap is a no-op"},
	} {
		if !strings.Contains(js, want.frag) {
			t.Errorf("resyncScript() no longer contains %q: %s.\n"+
				"resyncScript is loaded by BOTH documents that render a task card (the shell and "+
				"/tasks/{id}); losing this listener restores the last-arrival-wins race that made "+
				"the dispatch e2e spec flaky.", want.frag, want.why)
		}
	}
}

// TestEveryDocumentThatRendersATaskCardCarriesTheStaleGuard is the SEAM: the
// guard is only a guard on documents that actually render #task-{id}.
//
// It is asserted as a relationship rather than a list — for each standalone
// document, "renders a task card" and "carries the stale-card guard" must agree.
// A new page that renders cards without resyncScript would otherwise reintroduce
// the race on that page alone, with every existing test still green.
func TestEveryDocumentThatRendersATaskCardCarriesTheStaleGuard(t *testing.T) {
	// 🔴 THE ATTRIBUTE AS RENDERED (`data-card-rev="`), not the bare name: the
	// bare name also appears inside resyncScript's own source, so it would report
	// "this page renders a card" for every page that merely carries the guard.
	guardMark := `data-card-rev="`
	// 🔴 NOT "htmx:beforeSwap" — measured. appScript() and taskModalScript() each
	// attach one too, and both ship on every document, so that substring is
	// present whether or not THIS guard is. Dropping resyncScript() from the task
	// detail page SURVIVED against it. The named function is the only thing that
	// identifies the guard itself.
	scriptMark := staleCardGuardName

	// POSITIVE CONTROLS for both probes before either is believed — and taken from
	// the RENDERERS, not from any document. Controlling on a document would make
	// the control and the only document the loop can currently fire on the SAME
	// page, so the loop could never speak without the control speaking first.
	if card := renderString(t, noteCard(sampleTaskDetailView())); !strings.Contains(card, guardMark) {
		t.Fatalf("CONTROL: a rendered task card does not contain %q, so the \"renders a card\" probe "+
			"below is wired to nothing", guardMark)
	}
	if js := renderString(t, resyncScript()); !strings.Contains(js, scriptMark) {
		t.Fatalf("CONTROL: resyncScript() does not contain %q, so the \"carries the guard\" probe below "+
			"is wired to nothing", scriptMark)
	}
	// NEGATIVE control: a document with neither must report neither, or the two
	// probes are matching something every page has.
	bare := renderString(t, TaskNotFoundPage("613"))
	if strings.Contains(bare, guardMark) {
		t.Fatalf("CONTROL: the 404 page renders no task card yet contains %q — the card probe matches "+
			"something other than a card", guardMark)
	}

	// 🔴 A SECOND PROBE THE REV-OMISSION CANNOT ERASE.
	//
	// guardMark is the rev ATTRIBUTE, and noteCard now omits that attribute for a
	// note with a zero UpdatedAt. So "renders a card" silently narrowed to
	// "renders a card whose note has a non-zero updated_at", and a document
	// rendering #task-{id} without a rev became INVISIBLE to the loop below —
	// exempted from the very check it needs. That is not hypothetical: it happened
	// to the TaskDetailPage renderEmpty fixture the moment the omission landed
	// (2 documents matching -> 1), and nothing went red, because no test pinned
	// the count. Found by the audit of PR #681.
	//
	// wrapperMark matches the card WRAPPER, which is rendered unconditionally, so
	// the two probes must agree on every document. A card with a wrapper and no rev
	// is the coverage hole; a rev with no wrapper means guardMark is matching
	// something that is not a card.
	// 🔴 `<article id="task-`, NOT the bare `id="task-`. MEASURED: the bare form
	// also matches `id="task-modal"` and `id="task-modal-body"`, which ship on
	// EVERY document, so it reported "renders a task card" for 12 pages that
	// render none. The 12, measured: the app shell (/) and the agent detail page
	// (each populated + empty state), and shell:{tasks,repos,agents,suggestions}
	// (each populated + empty state). The 404 page is NOT among them — it carries
	// no modal, which is exactly why the negative control below could not catch
	// this on its own. A card is an <article>; the modal is a <div>.
	wrapperMark := `<article id="task-`
	if card := renderString(t, noteCard(sampleTaskDetailView())); !strings.Contains(card, wrapperMark) {
		t.Fatalf("CONTROL: a rendered task card does not contain %q, so the wrapper probe is wired to "+
			"nothing and the agreement check below is vacuous", wrapperMark)
	}
	if strings.Contains(bare, wrapperMark) {
		t.Fatalf("CONTROL: the 404 page renders no task card yet contains %q — the wrapper probe matches "+
			"something other than a card", wrapperMark)
	}

	for _, d := range allDocuments() {
		out := renderString(t, d.node)
		rendersCard := strings.Contains(out, guardMark)
		hasGuard := strings.Contains(out, scriptMark)
		// 🔴 COUNT, NOT Contains. `strings.Contains` on both sides answers "does
		// this document have AT LEAST ONE wrapper / AT LEAST ONE rev", which agrees
		// on a document rendering two cards where only one carries a rev — the
		// exact "usual cause" this check's own message names. MEASURED on a node
		// holding two noteCards, one with UpdatedAt and one bare: 2 wrappers,
		// 1 rev, and the boolean form stayed SILENT. No sweep document renders a
		// card LIST today, which is why the boolean version passed everything, and
		// a list is the natural shape for the next one.
		wrappers := strings.Count(out, wrapperMark)
		revs := strings.Count(out, guardMark)
		if rendersCard && !hasGuard {
			t.Errorf("%s renders a task card but does NOT carry the stale-card listener. On that page the "+
				"SSE re-fetch and the mutation response race for #task-{id} with nothing ordering them, "+
				"so whichever arrives last wins — including a card read before the write it is reacting "+
				"to had committed.", d.name)
		}
		if wrappers != revs {
			t.Errorf("%s renders %d task-card wrapper(s) (%q) but %d rev attribute(s) (%q) — the two "+
				"disagree. A wrapper with NO rev is a card this seam check cannot see, so it is silently "+
				"exempt from the listener requirement above and the last-arrival-wins race is "+
				"reintroduced for that card with every test green. The usual cause is a fixture built as "+
				"notes.Note{ID: N} with no UpdatedAt: noteCard omits data-card-rev for a zero timestamp, "+
				"and no note the store can produce has one (the column is NOT NULL).",
				d.name, wrappers, wrapperMark, revs, guardMark)
		}
	}
}
