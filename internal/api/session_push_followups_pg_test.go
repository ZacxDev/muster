package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
)

// The audit round 1 follow-ups to the "every prompt's push waits 60 s" change
// (PR #466). The invariant each pins: a push is delayed, never silenced; a
// prompt answered inside its OWN grace never buzzes; nothing is pushed twice.
// 🔴 Every test here uses only names that exist at the round-1 head (92d7cf01)
// too, so the regression ones are shown red there by running them.

// toolQ2 is the fixture window's SECOND AskUserQuestion's tool_use id.
const toolQ2 = "toolu_grace_q2"

var graceQuestion2 = []askq.Question{{Question: "Which cache?", Options: []askq.Option{{Label: "redis"}, {Label: "memcached"}}}}

// asksNext is the fixture window asking a second question after the first
// was answered: the window waits again (a new status time, so a new streak),
// the hook registers the question, and the hub is nudged.
func (f *stateFixture) asksNext(t *testing.T) {
	t.Helper()
	f.statusSince(mirror.StatusWaiting, nudgeSince+1_200_000, 0)
	k := questionKey()
	k.ToolUseID = toolQ2
	if _, err := f.srv.Questions().Open(k, graceQuestion2, askq.NewID()); err != nil {
		t.Fatal(err)
	}
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
}

// opening runs step and stamps every episode it OPENED (by id: one that
// already existed keeps its own opening, which is what F1's bug timed the next
// question from) with the fixture's time. 🔴 An episode's opened_at is the
// DATABASE's now(), not the fixture's clock — so these tests run their clock
// BEHIND the real one (fromNowBack), each opening then lands no later than the
// fixture's time, and the stamp writes what a live process opening it then
// would have.
func (f *stateFixture) opening(t *testing.T, step func(*testing.T)) {
	t.Helper()
	var last int64
	sys := authz.System(context.Background())
	if err := f.pool.QueryRow(sys, `SELECT coalesce(max(id), 0) FROM session_episodes`).Scan(&last); err != nil {
		t.Fatal(err)
	}
	step(t)
	if _, err := f.pool.Exec(sys, `UPDATE session_episodes SET opened_at = $1 WHERE owner_id = $2 AND id > $3`,
		f.srv.now(), authz.OperatorUserID, last); err != nil {
		t.Fatal(err)
	}
}

// fromNowBack sets the fixture's clock back by d from the real time, and
// returns it: the test's 0 s.
func (f *stateFixture) fromNowBack(d time.Duration) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock = time.Now().Add(-d)
	return f.clock
}

// answeredNext is the owner answering the second question at the terminal.
func (f *stateFixture) answeredNext(t *testing.T) {
	t.Helper()
	if _, ok := f.srv.Questions().Resolve(authz.OperatorUserID, askSourceMirror, "ws", nudgeSID, toolQ2, map[string]string{"Which cache?": "redis"}); !ok {
		t.Fatal("the second question was not open to resolve")
	}
	f.statusSince(mirror.StatusBusy, nudgeSince+1_800_000, 0)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
}

// passAt moves the fixture's clock to at after start and runs a state pass
// and a hub nudge, as a live process does around then.
func (f *stateFixture) passAt(t *testing.T, start time.Time, at time.Duration) {
	t.Helper()
	f.mu.Lock()
	f.clock = start.Add(at)
	f.mu.Unlock()
	f.statePass(t)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
}

// TestAQuestionAfterAnAnsweredOneGetsItsOwnGrace (regression, audit round 1
// F1): a question asked right after one the owner answered is a NEW stop, timed
// from when IT was asked. Red at 92d7cf01: no pass ran between the answer and
// the next question, so the next question joined the answered one's episode and
// was timed from ITS opening — pushed at once, or early, or at all though it was
// answered inside its own grace.
func TestAQuestionAfterAnAnsweredOneGetsItsOwnGrace(t *testing.T) {
	t.Run("Q1 answered at 55 s, Q2 at 62 s, still open: pushed only past 122 s", func(t *testing.T) {
		f := newStateFixture(t, false)
		start := f.fromNowBack(2 * time.Minute)
		f.opening(t, f.asks)
		f.mu.Lock()
		f.clock = start.Add(55 * time.Second)
		f.mu.Unlock()
		f.answered(t)
		f.mu.Lock()
		f.clock = start.Add(62 * time.Second)
		f.mu.Unlock()
		f.opening(t, f.asksNext)
		for _, at := range []time.Duration{62, 90, 120} {
			f.passAt(t, start, at*time.Second)
			if got := f.buzzes(); len(got) != 0 {
				t.Fatalf("pushed by %d s, inside the second question's grace: %v", at, got)
			}
		}
		f.passAt(t, start, 123*time.Second)
		if got := f.buzzes(); len(got) != 1 {
			t.Fatalf("pushes past the second question's grace: %v, want its one", got)
		}
		var bodies []string
		for _, p := range f.payloads() {
			if p.Title != "" {
				bodies = append(bodies, p.Body)
			}
		}
		if len(bodies) != 1 || bodies[0] != "Which cache?\nOptions: redis · memcached" {
			t.Fatalf("bodies %q, want the second question's alone", bodies)
		}
		for _, at := range []time.Duration{150, 180} {
			f.passAt(t, start, at*time.Second)
		}
		if got := f.buzzes(); len(got) != 1 {
			t.Fatalf("pushes after more passes: %v", got)
		}
	})
	t.Run("Q1 answered at 55 s, Q2 at 62 s, answered at 100 s: never pushed", func(t *testing.T) {
		f := newStateFixture(t, false)
		start := f.fromNowBack(2 * time.Minute)
		f.opening(t, f.asks)
		f.mu.Lock()
		f.clock = start.Add(55 * time.Second)
		f.mu.Unlock()
		f.answered(t)
		f.mu.Lock()
		f.clock = start.Add(62 * time.Second)
		f.mu.Unlock()
		f.opening(t, f.asksNext)
		f.passAt(t, start, 90*time.Second)
		f.mu.Lock()
		f.clock = start.Add(100 * time.Second)
		f.mu.Unlock()
		f.answeredNext(t)
		for _, at := range []time.Duration{120, 150, 180, 240} {
			f.passAt(t, start, at*time.Second)
		}
		if got := f.buzzes(); len(got) != 0 {
			t.Fatalf("pushes: %v, want none — each question was answered inside its own grace", got)
		}
	})
	t.Run("Q1 answered at 50 s, Q2 at 58 s, answered at 75 s: never pushed", func(t *testing.T) {
		f := newStateFixture(t, false)
		start := f.fromNowBack(2 * time.Minute)
		f.opening(t, f.asks)
		f.mu.Lock()
		f.clock = start.Add(50 * time.Second)
		f.mu.Unlock()
		f.answered(t)
		f.mu.Lock()
		f.clock = start.Add(58 * time.Second)
		f.mu.Unlock()
		f.opening(t, f.asksNext)
		f.passAt(t, start, 61*time.Second)
		f.passAt(t, start, 70*time.Second)
		f.mu.Lock()
		f.clock = start.Add(75 * time.Second)
		f.mu.Unlock()
		f.answeredNext(t)
		for _, at := range []time.Duration{90, 120, 150, 200} {
			f.passAt(t, start, at*time.Second)
		}
		if got := f.buzzes(); len(got) != 0 {
			t.Fatalf("pushes: %v, want none — Q2 was answered 17 s after it was asked", got)
		}
	})
}

// TestAWindowThatBrieflyListsNoStatusKeepsItsStop (invariant guard, for the
// F1 fix's own limit): the hook ends a stop only on a window its host lists in
// a KNOWN status other than waiting. A window whose host lists it with no
// status for a moment, then waiting again, is the same stop — pushed once its
// grace from the ORIGINAL opening has passed, not timed afresh from the gap.
func TestAWindowThatBrieflyListsNoStatusKeepsItsStop(t *testing.T) {
	f := newStateFixture(t, false)
	f.seenIdleThenWaiting()
	f.advance(20 * time.Second)
	f.mirror.mu.Lock()
	f.mirror.windows[0].Status = ""
	f.mirror.mu.Unlock()
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.advance(10 * time.Second)
	f.status(mirror.StatusWaiting, time.Second)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.advance(31 * time.Second)
	f.statePass(t)
	if got := f.buzzes(); len(got) != 1 || !strings.HasPrefix(got[0], "attention attention-episode-") {
		t.Fatalf("pushes 61 s after the stop opened: %v, want its episode's one", got)
	}
	if n := len(f.episodes(t)); n != 1 {
		t.Fatalf("episodes: %d, want the one stop", n)
	}
}

// waitingNow puts the fixture's window waiting from this instant, by its
// host's own time.
func (f *stateFixture) waitingNow() {
	f.statusSince(mirror.StatusWaiting, f.srv.now().UnixMilli(), 0)
}

// TestASlowRestartPushesAWaitNoProcessPushedOnce (regression, audit round 1
// F2): a wait the waiting diff holds (a window session state cannot key, or
// session state off) starts at 0; muster restarts at 40 s, before its push was
// due; the restarted process first sees the host at 70 s, past the grace.
// Still waiting: pushed exactly once. Answered before that first sight:
// never. Red at 92d7cf01: the first sight seeded every wait older than its
// grace, so this one — never pushed by anyone — was silent.
func TestASlowRestartPushesAWaitNoProcessPushedOnce(t *testing.T) {
	for _, mode := range []string{"keyless window", "session state off"} {
		for _, answered := range []bool{false, true} {
			name := mode + ", still waiting"
			if answered {
				name = mode + ", answered before the first sight"
			}
			t.Run(name, func(t *testing.T) {
				f := newStateFixture(t, false)
				if mode == "keyless window" {
					f.keyless()
				} else {
					f.srv.ext.Episodes = nil
				}
				f.srv.AttentionMirrorChanged(authz.OperatorUserID) // the host seen idle
				f.waitingNow()
				f.srv.AttentionMirrorChanged(authz.OperatorUserID)
				f.advance(40 * time.Second)
				f.srv.recheckOfflineHosts()
				if got := f.buzzes(); len(got) != 0 {
					t.Fatalf("pushed inside the grace: %v", got)
				}
				f.restart()
				if answered {
					f.advance(10 * time.Second)
					f.statusSince(mirror.StatusBusy, f.srv.now().UnixMilli(), 0)
					f.advance(20 * time.Second)
				} else {
					f.advance(30 * time.Second)
				}
				f.srv.AttentionMirrorChanged(authz.OperatorUserID) // the restarted process's first sight, at 70 s
				for i := 0; i < 3; i++ {
					f.advance(30 * time.Second)
					f.srv.recheckOfflineHosts()
					f.srv.AttentionMirrorChanged(authz.OperatorUserID)
				}
				want := 1
				if answered {
					want = 0
				}
				got := f.buzzes()
				if len(got) != want {
					t.Fatalf("pushes: %v, want %d", got, want)
				}
				if want == 1 && !strings.HasPrefix(got[0], "attention attention-session-") {
					t.Fatalf("push %q, want the waiting diff's", got[0])
				}
			})
		}
	}
}

// TestAHeldWaitThatLosesItsSessionKeyStillPushesOnce (regression, audit round
// 1 F3): a covered window's wait is held as an episode; 20 s in, its host stops
// naming its Claude session, so it leaves the cover for the waiting diff. Its
// held stop was never pushed, so the diff takes it as a new wait and pushes it
// once its grace has passed — and only once. Red at 92d7cf01: the window could
// no longer name its episode, so it was taken as already pushed and seeded.
func TestAHeldWaitThatLosesItsSessionKeyStillPushesOnce(t *testing.T) {
	f := newStateFixture(t, false)
	f.seenIdleThenWaiting()
	if n := len(f.episodes(t)); n != 1 {
		t.Fatalf("episodes: %d, want its held one", n)
	}
	f.advance(20 * time.Second)
	f.keyless()
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.advance(30 * time.Second)
	f.statePass(t)
	f.srv.recheckOfflineHosts()
	if got := f.buzzes(); len(got) != 0 {
		t.Fatalf("pushed inside its grace: %v", got)
	}
	f.advance(31 * time.Second)
	f.statePass(t)
	f.srv.recheckOfflineHosts()
	got := f.buzzes()
	if len(got) != 1 || !strings.HasPrefix(got[0], "attention attention-session-") {
		t.Fatalf("pushes past its grace: %v, want the diff's one", got)
	}
	for i := 0; i < 3; i++ {
		f.advance(30 * time.Second)
		f.statePass(t)
		f.srv.recheckOfflineHosts()
		f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	}
	if got := f.buzzes(); len(got) != 1 {
		t.Fatalf("pushes after more passes: %v", got)
	}
}

// TestAFirstSightNeverHoldsPastTheGraceOnAHostClockAhead (invariant guard,
// audit round 1 F4: the future-Since clamp): a host whose clock runs 5 minutes
// ahead lists a wait that "started" in the future. Its first sight holds it
// for the grace from NOW — pushed once 61 s later — never until 60 s past the
// host's future time.
func TestAFirstSightNeverHoldsPastTheGraceOnAHostClockAhead(t *testing.T) {
	f := newStateFixture(t, false)
	f.keyless()
	f.statusSince(mirror.StatusWaiting, f.srv.now().Add(5*time.Minute).UnixMilli(), 0)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID) // the host's first sight
	f.advance(30 * time.Second)
	f.srv.recheckOfflineHosts()
	if got := f.buzzes(); len(got) != 0 {
		t.Fatalf("pushed inside the grace: %v", got)
	}
	f.advance(31 * time.Second)
	f.srv.recheckOfflineHosts()
	if got := f.buzzes(); len(got) != 1 {
		t.Fatalf("pushes 61 s after the first sight: %v, want one", got)
	}
}

// TestAWaitThatStopsInsideItsGraceAndWaitsAgainIsTimedAfresh (invariant guard,
// audit round 1 F4: a held entry is dropped when its wait stops): a keyless
// wait starts at 0, is answered at 20 s and waits again at 50 s. The second wait
// is held for its own grace, from 50 s — not pushed at 75 s off the first one's
// start — and pushed once past 110 s.
func TestAWaitThatStopsInsideItsGraceAndWaitsAgainIsTimedAfresh(t *testing.T) {
	f := newStateFixture(t, false)
	f.keyless()
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.waitingNow()
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.advance(20 * time.Second)
	f.statusSince(mirror.StatusBusy, f.srv.now().UnixMilli(), 0)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.advance(30 * time.Second)
	f.waitingNow()
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.advance(25 * time.Second)
	f.srv.recheckOfflineHosts()
	if got := f.buzzes(); len(got) != 0 {
		t.Fatalf("pushed at 75 s, 25 s into the second wait: %v", got)
	}
	f.advance(36 * time.Second)
	f.srv.recheckOfflineHosts()
	if got := f.buzzes(); len(got) != 1 {
		t.Fatalf("pushes at 111 s: %v, want the second wait's one", got)
	}
}

// TestAnEpisodesAskPushIsItsOwnWindowsQuestion (invariant guard, audit round 1
// F4: episodeAsk matches the ask's window to the episode's key): two windows
// wait on two different questions. Each one's episode notification carries
// ITS window's question, never the other's.
func TestAnEpisodesAskPushIsItsOwnWindowsQuestion(t *testing.T) {
	const sid2 = "7c1e9b40-2a5d-4f8e-b3c6-1d0e9f8a7b65"
	f := newStateFixture(t, false)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.mirror.mu.Lock()
	f.mirror.windows[0].Status = mirror.StatusWaiting
	f.mirror.windows = append(f.mirror.windows, mirror.Window{Session: "main", ID: "@2", Index: 2, Label: "Blue:2", Claude: true,
		ClaudeSession: sid2, Status: mirror.StatusWaiting, StatusSince: nudgeSince})
	f.mirror.mu.Unlock()
	k2 := questionKey()
	k2.Session, k2.ToolUseID = sid2, toolQ2
	for _, q := range []struct {
		k  askq.Key
		qs []askq.Question
	}{{questionKey(), graceQuestion}, {k2, graceQuestion2}} {
		if _, err := f.srv.Questions().Open(q.k, q.qs, askq.NewID()); err != nil {
			t.Fatal(err)
		}
	}
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	eps := f.episodes(t)
	if len(eps) != 2 {
		t.Fatalf("episodes: %+v, want one per window", eps)
	}
	want := map[string]string{nudgeSID: "Which database?\nOptions: postgres · sqlite", sid2: "Which cache?\nOptions: redis · memcached"}
	octx := f.srv.attentionPrincipal(t.Context(), authz.OperatorUserID)
	for _, e := range eps {
		n := f.srv.episodeNotification(octx, e, 0, nil)
		if w, ok := want[e.Key]; !ok || n.Body != w {
			t.Fatalf("episode %d (%s, window %s): body %q, want %q", e.ID, e.Key, e.Window, n.Body, w)
		}
	}
}

// TestAnUnrelayableOpenCodeQuestionKeepsItsWhyCard (regression, audit round 1
// F5): an opencode question muster cannot relay is a plain wait for its PUSH,
// but on the Asks list it keeps its question card, which says why it cannot be
// answered here — with no second "Stalled / waiting" row for its stop. Red at
// 92d7cf01: the card was dropped for a text-free row.
func TestAnUnrelayableOpenCodeQuestionKeepsItsWhyCard(t *testing.T) {
	f := newStateFixture(t, false)
	f.openCodeQuestionWindow()
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	if n := len(f.episodes(t)); n != 1 {
		t.Fatalf("episodes: %d, want its held one", n)
	}
	body := f.page(t, "/attention")
	if !strings.Contains(body, `data-kind="question"`) || !strings.Contains(body, "data-opencode-note") ||
		!strings.Contains(body, "answer it in the terminal") {
		t.Fatalf("the question's card, saying why, is not on the Asks list:\n%s", body)
	}
	if strings.Contains(body, `data-testid="attention-episodes"`) {
		t.Fatalf("a second \"Stalled / waiting\" row beside its card:\n%s", body)
	}
}
