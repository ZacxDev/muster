package ui

import (
	"html"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/notes"
)

// 🔴 FRAMING: task threads are a NEW feature, so these are INVARIANT GUARDS, not
// regression tests.
//
// 🔴 AND THE SEAM THEY EXIST FOR: every api-tier test reaches this renderer
// THROUGH a store, so it can only ever hand it link rows the store would produce.
// A renderer bug that only shows on a state the store rarely produces — a REAPED
// session, say — is structurally invisible there. These tests hand the renderer
// each state directly.

func threadNote(links ...notes.SessionLink) notes.Note {
	return notes.Note{
		ID: 51, Title: "a threaded task", Body: "investigate", Status: notes.StatusOpen,
		Sessions: links,
	}
}

// renderThreadCard renders the card in its DETAIL shape.
//
// The session THREAD (taskSessionsSection — the per-session rows, the expander
// and its telemetry) lives only there; the board card carries just the
// taskSessionsChip COUNT. Every test in this file is about the thread, so they
// all need the shape that renders it. The chip's own board-card guard is
// TestTaskSessionsChip* below.
func renderThreadCard(t *testing.T, n notes.Note) string {
	t.Helper()
	var b strings.Builder
	if err := renderDetailCard(&b, n); err != nil {
		t.Fatalf("render: %v", err)
	}
	return b.String()
}

// renderThreadCardUnescaped is renderThreadCard with HTML entities decoded, for
// assertions about JS that lives inside an ATTRIBUTE (gomponents escapes the
// quotes there). Asserting the escaped bytes instead would pin the escaping
// scheme rather than the behaviour, and would break on a cosmetic change that
// leaves the page working.
func renderThreadCardUnescaped(t *testing.T, n notes.Note) string {
	t.Helper()
	return html.UnescapeString(renderThreadCard(t, n))
}

// chipText returns the text of the collapsed card's sessions CHIP, or "" when
// there is none.
//
// 🔴 THIS EXTRACTION IS THE POINT, not plumbing. The first version of this test
// asserted `strings.Contains(card, "👥 3 sessions")` over the WHOLE card — and a
// mutant that hardcoded the chip's count to 1 SURVIVED, because the expanded
// thread's own summary renders the identical string a few hundred bytes further
// down. The assertion was satisfied by a different element spelling the same
// words: the textbook spelled-guard walk. Scope the read to the element under
// test, always.
func chipText(t *testing.T, n notes.Note) string {
	t.Helper()
	page := renderThreadCard(t, n)
	const marker = `<span data-task-sessions-chip=""`
	i := strings.Index(page, marker)
	if i < 0 {
		return ""
	}
	rest := page[i:]
	open := strings.Index(rest, ">")
	end := strings.Index(rest, "</span>")
	if open < 0 || end < 0 || end < open {
		t.Fatalf("could not parse the sessions chip out of:\n%s", rest[:200])
	}
	return rest[open+1 : end]
}

// TestTaskSessionsChipCountsAndDisappears: the collapsed card carries ONE count
// chip, and a threadless task renders nothing at all — so every pre-0023 card is
// unchanged.
func TestTaskSessionsChipCountsAndDisappears(t *testing.T) {
	// Absent for zero.
	if got := chipText(t, threadNote()); got != "" {
		t.Fatalf("a task with no thread rendered a sessions chip %q", got)
	}
	// Present, and SINGULAR, for one.
	if got := chipText(t, threadNote(
		notes.SessionLink{SessionID: "aaaa1111", Role: notes.RoleCreated, DetailAvailable: true},
	)); got != "👥 1 session" {
		t.Fatalf("chip = %q for one link, want `👥 1 session`", got)
	}
	// Three: a count that is neither 0 nor 1 nor the number of any other thing on
	// the card (it has 0 comments and 0 attachments), so a chip wired to the wrong
	// collection cannot coincidentally agree — and, unlike the earlier whole-card
	// assertion, it is read from the chip ITSELF.
	if got := chipText(t, threadNote(
		notes.SessionLink{SessionID: "aaaa1111", Role: notes.RoleCreated, DetailAvailable: true},
		notes.SessionLink{SessionID: "bbbb2222", Role: notes.RoleWorked, DetailAvailable: true},
		notes.SessionLink{SessionID: "cccc3333", Role: notes.RoleRead, DetailAvailable: false},
	)); got != "👥 3 sessions" {
		t.Fatalf("chip = %q for three links, want `👥 3 sessions`", got)
	}
	// Exactly ONE chip: the collapsed card gets a single count and nothing else, so
	// a duplicate would be the third dense metadata layer this design refuses.
	page := renderThreadCard(t, threadNote(
		notes.SessionLink{SessionID: "aaaa1111", Role: notes.RoleCreated, DetailAvailable: true},
	))
	if got := strings.Count(page, "data-task-sessions-chip"); got != 1 {
		t.Fatalf("card carries %d sessions chips, want exactly 1", got)
	}
}

// TestTaskSessionRowsRenderAllThreeStates is the load-bearing renderer guard.
//
// 🔴 THREE STATES, NOT TWO — and the test used to say "three" while checking two,
// which is how the renderer got away with implementing two. The three are:
//
//	DetailAvailable                 → a live link to /suggestions/{id}
//	!Available && DetailSeen        → "transcript expired"     (the 14-day sweep)
//	!Available && !DetailSeen       → "no transcript recorded" (never posted)
//
// The second and third are the pair that matters. DetailAvailable is ONE bit and
// cannot separate them, so every absence rendered as "transcript expired · kept for
// 14 days" — a sentence that is FALSE for a session whose transcript was never
// written, and that was the common case: cc_sessions has exactly one producer (the
// Stop hook's POST /api/suggest), that hook was ~96% dead from 2026-06-14, and 81 of
// 83 links had no cc_sessions row. The wrong label also disguised the outage as
// ordinary retention, which is why this is a correctness test and not a copy nit.
//
// 🔴 EVERY STATE ASSERTS BOTH DIRECTIONS — its own attribute AND label present, the
// other two ABSENT. A one-directional check ("reaped rows carry data-session-reaped")
// is satisfied by a renderer that marks EVERYTHING reaped, which is precisely the
// pre-change behaviour. The fixtures are pairwise distinguishable on the only field
// that selects the branch (DetailAvailable × DetailSeen), so no case can pass by
// landing in another's branch.
func TestTaskSessionRowsRenderAllThreeStates(t *testing.T) {
	const (
		reapedAttr     = "data-session-reaped"
		unrecordedAttr = "data-session-unrecorded"
		reapedText     = "transcript expired"
		unrecordedText = "no transcript recorded"
	)

	t.Run("no sessions renders no thread block", func(t *testing.T) {
		html := renderThreadCard(t, threadNote())
		if strings.Contains(html, "data-task-sessions") {
			t.Fatalf("a threadless task rendered a thread block:\n%s", html)
		}
	})

	t.Run("available session links to its transcript", func(t *testing.T) {
		html := renderThreadCard(t, threadNote(notes.SessionLink{
			SessionID: "live-session-id", Role: notes.RoleWorked,
			Project: "muster", Cwd: "/w/c", Host: "build-host-1",
			LastSeenAt: time.Now().Add(-2 * time.Hour), DetailAvailable: true, DetailSeen: true,
		}))
		if !strings.Contains(html, `href="/suggestions/live-session-id"`) {
			t.Fatalf("an available session did not link to its transcript page:\n%s", html)
		}
		for _, no := range []string{reapedAttr, unrecordedAttr, reapedText, unrecordedText} {
			if strings.Contains(html, no) {
				t.Fatalf("an available session rendered %q:\n%s", no, html)
			}
		}
		// role · project · cwd · host all reach the row — the denormalised context
		// is the whole reason the columns exist.
		for _, want := range []string{"worked", "muster", "/w/c", "build-host-1"} {
			if !strings.Contains(html, want) {
				t.Fatalf("session row is missing %q:\n%s", want, html)
			}
		}
	})

	t.Run("reaped session renders a dead row, not an absence", func(t *testing.T) {
		html := renderThreadCard(t, threadNote(notes.SessionLink{
			SessionID: "reaped-session-id", Role: notes.RoleWorked,
			Project: "muster", LastSeenAt: time.Now().Add(-30 * 24 * time.Hour),
			// The transcript WAS observed once (DetailSeen) and has since been swept
			// (!DetailAvailable) — the only combination that means "expired".
			DetailAvailable: false, DetailSeen: true,
		}))
		if !strings.Contains(html, reapedAttr) {
			t.Fatalf("a reaped session did not render its own state:\n%s", html)
		}
		if strings.Contains(html, unrecordedAttr) {
			t.Fatalf("a reaped session was labelled never-recorded — the two states are not interchangeable:\n%s", html)
		}
		if !strings.Contains(html, "reaped-session-id"[:8]) {
			t.Fatalf("a reaped session's id is not on the card:\n%s", html)
		}
		if strings.Contains(html, `href="/suggestions/reaped-session-id"`) {
			t.Fatalf("a reaped session still offers a link to a page that 404s:\n%s", html)
		}
		// The REASON must be stated: a row with a missing link and no explanation
		// reads as a rendering bug.
		if !strings.Contains(html, reapedText) || !strings.Contains(html, "14 days") {
			t.Fatalf("the reaped row does not state the retention reason:\n%s", html)
		}
		if strings.Contains(html, unrecordedText) {
			t.Fatalf("the reaped row also carries the never-recorded label:\n%s", html)
		}
	})

	t.Run("never-recorded session says so, and does NOT claim it expired", func(t *testing.T) {
		// A session MINUTES old whose Stop hook never posted. Under the pre-0024
		// renderer this rendered "transcript expired — kept for 14 days", which is a
		// false statement about a link created moments ago and is the single most
		// visible symptom of the dead hook.
		html := renderThreadCard(t, threadNote(notes.SessionLink{
			SessionID: "never-posted-session", Role: notes.RoleWorked,
			Project: "muster", LastSeenAt: time.Now().Add(-3 * time.Minute),
			DetailAvailable: false, DetailSeen: false,
		}))
		if !strings.Contains(html, unrecordedAttr) {
			t.Fatalf("a never-recorded session has no state of its own:\n%s", html)
		}
		if strings.Contains(html, reapedAttr) {
			t.Fatalf("a never-recorded session was marked REAPED — the row claims a 14-day retention window it was never in:\n%s", html)
		}
		if !strings.Contains(html, unrecordedText) {
			t.Fatalf("the never-recorded row states no reason at all:\n%s", html)
		}
		if strings.Contains(html, reapedText) || strings.Contains(html, "14 days") {
			t.Fatalf("the never-recorded row still claims the transcript expired:\n%s", html)
		}
		// The reason names the actual cause, so a reader can act on it rather than
		// wait 14 days for a transcript that is never coming.
		if !strings.Contains(html, "Stop hook") {
			t.Fatalf("the never-recorded row does not say WHY there is no transcript:\n%s", html)
		}
		if strings.Contains(html, `href="/suggestions/never-posted-session"`) {
			t.Fatalf("a never-recorded session offers a link to a page that 404s:\n%s", html)
		}
	})

	t.Run("a mixed thread renders ALL THREE states side by side", func(t *testing.T) {
		html := renderThreadCard(t, threadNote(
			notes.SessionLink{SessionID: "alive-one", Role: notes.RoleCreated, DetailAvailable: true, DetailSeen: true},
			notes.SessionLink{SessionID: "gone-one", Role: notes.RoleWorked, DetailAvailable: false, DetailSeen: true},
			notes.SessionLink{SessionID: "never-one", Role: notes.RoleRead, DetailAvailable: false, DetailSeen: false},
		))
		if !strings.Contains(html, `href="/suggestions/alive-one"`) {
			t.Fatalf("the live row lost its link in a mixed thread:\n%s", html)
		}
		if got := strings.Count(html, reapedAttr); got != 1 {
			t.Fatalf("mixed thread rendered %d reaped rows, want exactly 1:\n%s", got, html)
		}
		if got := strings.Count(html, unrecordedAttr); got != 1 {
			t.Fatalf("mixed thread rendered %d never-recorded rows, want exactly 1:\n%s", got, html)
		}
		// The counts above are what catches a renderer that resolves every absence
		// to one branch: it would produce 2 of one and 0 of the other while still
		// rendering three rows.
		if got := strings.Count(html, `data-task-session=""`); got != 3 {
			t.Fatalf("mixed thread rendered %d rows, want 3:\n%s", got, html)
		}
	})
}

// TestTaskSessionsExpandEmitsTelemetry pins the cgTrack site on the thread's OWN
// expand control. It is deliberately not on the CARD's summary: that fires for
// every card expand and would measure card usage, not this feature.
func TestTaskSessionsExpandEmitsTelemetry(t *testing.T) {
	page := renderThreadCardUnescaped(t, threadNote(
		notes.SessionLink{SessionID: "aaaa1111", Role: notes.RoleCreated, DetailAvailable: true},
		notes.SessionLink{SessionID: "bbbb2222", Role: notes.RoleRead, DetailAvailable: true},
	))
	if !strings.Contains(page, "window.cgTrack('task.sessions.expanded'") {
		t.Fatalf("no cgTrack('task.sessions.expanded') site on the thread expander:\n%s", page)
	}
	// It must carry the id/count, otherwise the event cannot distinguish a
	// one-session thread from a twenty-session one.
	if !strings.Contains(page, `task_id:"51"`) || !strings.Contains(page, "sessions:2") {
		t.Fatalf("the telemetry call does not carry task_id + session count:\n%s", page)
	}
	// And the control must be attached to the sessions <details>, not the card's.
	if !strings.Contains(page, "data-task-sessions-toggle") {
		t.Fatalf("the thread has no dedicated expand control:\n%s", page)
	}
	// 🔴 The event must fire on `toggle`, GUARDED by `this.open`. The thread now
	// renders `open`, so the FIRST interaction is a COLLAPSE — and the previous
	// wiring (hx-on:click on the <summary>) would report that collapse as
	// 'task.sessions.expanded'. Both halves are pinned because either alone is
	// walkable: `toggle` without the guard fires on every close, and the guard
	// without `toggle` reads a pre-transition `open` on the click path.
	if !strings.Contains(page, "hx-on:toggle=") {
		t.Errorf("the thread telemetry is not on the <details> `toggle` event; on the pre-#654 click "+
			"path the first interaction on a default-open thread reports a COLLAPSE as an expand:\n%s", page)
	}
	if !strings.Contains(page, "if(this.open)") {
		t.Errorf("the thread telemetry is not guarded by `this.open`, so 'task.sessions.expanded' also "+
			"fires when the user CLOSES the thread:\n%s", page)
	}
	if strings.Contains(page, `data-task-sessions-toggle="" class`) && strings.Contains(page, "hx-on:click=\"try{window.cgTrack('task.sessions.expanded'") {
		t.Errorf("the old hx-on:click telemetry site is back on the <summary>:\n%s", page)
	}
}

// TestTaskSessionsThreadIsOpenByDefault pins the thread OPEN on the detail page.
//
// 🔴 THIS IS A REGRESSION GUARD WITH A REAL INCIDENT BEHIND IT, not an invariant
// guard like the rest of this file: shipping it CLOSED (the pre-#654 board-card
// default, kept by inertia once #654 made the body detail-only) produced muster
// task 486 — a filed, triaged "the Open session chip is painted but unclickable"
// bug that was neither. A closed <details> hands a prober a stale
// getBoundingClientRect over the comments block, which reads exactly like an
// overlap. Cost: one bug report, one triage session.
//
// It asserts the ATTRIBUTE, not any wording: `open` is the whole mechanism, and a
// reword of the summary must not be able to satisfy or break this.
func TestTaskSessionsThreadIsOpenByDefault(t *testing.T) {
	page := renderThreadCard(t, threadNote(
		notes.SessionLink{SessionID: "aaaa1111", Role: notes.RoleCreated, DetailAvailable: true},
	))
	i := strings.Index(page, `<details data-task-sessions=""`)
	if i < 0 {
		t.Fatalf("positive control failed: the detail card rendered no sessions <details> at all, so "+
			"this test cannot observe whether it is open:\n%s", page)
	}
	// Scope the read to the opening tag. `strings.Contains(page, "open")` over the
	// whole card would match the task's own `open` STATUS and pass with the
	// attribute deleted — the spelled-guard walk this file's chipText comment
	// documents.
	end := strings.Index(page[i:], ">")
	if end < 0 {
		t.Fatalf("unterminated <details> tag:\n%s", page)
	}
	tag := page[i : i+end+1]
	if !strings.Contains(tag, " open") {
		t.Errorf("the session thread is COLLAPSED by default on the detail page. Its opening tag is %q.\n"+
			"The #357 rationale (\"does not lengthen every expanded card by N rows\") died with #654 — the "+
			"section renders only under g.If(v.Detail, …), so there is one card on the page. See muster "+
			"task 486 for what collapsing it cost.", tag)
	}
}

// TestTaskSessionRowEscapesHostileContext: every value on a thread row is
// producer-supplied text arriving over a hook-token header (session id, host) or
// copied from cc_sessions (project, cwd). g.Text escapes, but a future refactor to
// g.Raw would not — so pin it.
func TestTaskSessionRowEscapesHostileContext(t *testing.T) {
	html := renderThreadCard(t, threadNote(notes.SessionLink{
		SessionID: `<img src=x onerror=alert(1)>`, Role: notes.RoleWorked,
		Project: `</span><script>alert(2)</script>`, DetailAvailable: false,
	}))
	if strings.Contains(html, "<script>") || strings.Contains(html, "onerror=") {
		t.Fatalf("a thread row rendered unescaped producer text:\n%s", html)
	}
}

// TestSessionRoleLabelFallsBackToTheRawRole: an unrecognised role must still be
// visible. Hiding it would make a future role look like a rendering bug.
func TestSessionRoleLabelFallsBackToTheRawRole(t *testing.T) {
	for role, want := range map[string]string{
		notes.RoleCreated: "created",
		notes.RoleWorked:  "worked",
		notes.RoleRead:    "read",
		"reviewed":        "reviewed",
	} {
		if got := sessionRoleLabel(role); got != want {
			t.Errorf("sessionRoleLabel(%q) = %q, want %q", role, got, want)
		}
	}
}
