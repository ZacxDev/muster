package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/sessionstate"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE EPISODE PUSH end to end on a real Postgres (session_push.go): one
// text-free push per stop, claimed; a waiting window pushed at once from the
// hub hook; an idle stop on the chief's decision (a sent nudge quiet, a
// "leave" quiet only when nothing was asked, shadow never quiet); the tap
// route; the Stalled / waiting rows; and with MUSTER_SESSION_STATE off,
// today's pushes unchanged.
//
// 🔴 These tests use only names that exist at the base too (the fixture, the
// episodes table, the hub hook, the pass, the tap route), so each REGRESSION
// test is shown red against the base by running it there.

// pushSecret is shaped like a secret and must never reach a notification.
const pushSecret = "sk-live-PUSHSECRET0123456789abcdef"

// episodePushes is the operator's episode pushes so far, "type tag" lines.
func (f *stateFixture) episodePushes() []string {
	var out []string
	for _, p := range f.pushes() {
		if strings.Contains(p, "attention-episode-") {
			out = append(out, p)
		}
	}
	return out
}

// pastGrace moves the clock past a waiting window's grace (60 s) and runs a
// pass: the pass that sends a held waiting push.
func (f *stateFixture) pastGrace(t *testing.T) {
	t.Helper()
	f.advance(EpisodeWaitingGrace + time.Second)
	f.statePass(t)
}

// noGrace sets the test seam Server.waitingGrace to 0: a waiting window is
// pushed at once, from the hub hook, as before the grace.
func (f *stateFixture) noGrace() { f.srv.waitingGrace = 0 }

// payloads is every notification that reached the operator's device.
func (f *stateFixture) payloads() []push.Payload {
	f.srv.WaitForPushes()
	return f.pusher.to(ep("attn-operator"))
}

// waitWithSecrets puts the fixture's window waiting on a prompt whose tool,
// summary and last words are all secret-shaped. Its label stays the host's
// codename for it ("Gold:1"): a label is not session text, and the push names
// the window by it (episodeWho).
func (f *stateFixture) waitWithSecrets() {
	f.status(mirror.StatusWaiting, 5*time.Second)
	f.mirror.mu.Lock()
	defer f.mirror.mu.Unlock()
	f.mirror.windows[0].Pending = &mirror.Pending{Tool: "Bash " + pushSecret, Summary: "run " + pushSecret, Last: "about to use " + pushSecret}
}

func (f *stateFixture) live() {
	f.cfg.Live[authz.OperatorUserID] = true
}

// quietOf is episode id's quiet reason, read straight from the table.
func (f *stateFixture) quietOf(t *testing.T, id int64) string {
	t.Helper()
	var q string
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT quiet FROM session_episodes WHERE id = $1`, id).Scan(&q); err != nil {
		t.Fatal(err)
	}
	return q
}

// TestAWaitingWindowPushesOnceAtOnceWithNoSessionText (regression): with
// session state on and NO waiting grace (the test seam Server.waitingGrace
// at 0), a window that starts waiting is pushed at once FROM THE HUB HOOK as its
// episode — one push, tagged by the episode, naming the window
// as every attention surface does (its label, then its tmux session, window
// index and host), and carrying none of the session's text (no tool, no
// command, no summary, no last words). The state pass and a second hook nudge
// send nothing more.
func TestAWaitingWindowPushesOnceAtOnceWithNoSessionText(t *testing.T) {
	f := newStateFixture(t, false)
	f.noGrace()
	f.waitWithSecrets()
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	e := f.oneEpisode(t)
	want := "attention attention-episode-" + strconv.FormatInt(e.ID, 10)
	if got := f.pushes(); len(got) != 1 || got[0] != want {
		t.Fatalf("pushes after the hook: %v, want [%s]", got, want)
	}
	p := f.payloads()[0]
	if p.Title != "Gold:1 · tmux main:1 @ ws needs you" || p.Body != "Needs you · just now" {
		t.Fatalf("payload: title %q body %q", p.Title, p.Body)
	}
	for _, s := range []string{p.Title, p.Body, p.Data["url"]} {
		if strings.Contains(s, "PUSHSECRET") || strings.Contains(s, "Bash") || strings.Contains(s, "about to use") {
			t.Fatalf("session text in the push: %q", s)
		}
	}
	if p.Data["url"] != "/attention/open/episode-"+strconv.FormatInt(e.ID, 10) {
		t.Fatalf("url %q", p.Data["url"])
	}
	f.statePass(t)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	if got := f.pushes(); len(got) != 1 {
		t.Fatalf("pushes after a pass and a second nudge: %v", got)
	}
}

// TestTheHookAndThePassPushAStopOnce (regression): the claim. The pass opens
// a waiting stop (held for its grace) and pushes it once the grace has passed;
// the hook then finds it pushed and sends nothing; a restarted process (a
// fresh hook state) sends nothing either.
func TestTheHookAndThePassPushAStopOnce(t *testing.T) {
	f := newStateFixture(t, false)
	f.status(mirror.StatusWaiting, 5*time.Second)
	f.statePass(t)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.pastGrace(t)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.srv.attention.mu.Lock()
	f.srv.attention.episodeWaiting = nil // a restart: the process forgot what it saw
	f.srv.attention.mu.Unlock()
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.statePass(t)
	if got := f.episodePushes(); len(got) != 1 {
		t.Fatalf("pushes: %v", got)
	}
}

// TestAStallWithNoChiefPushesOnce (regression): MUSTER_CHIEF_NUDGES off, so
// nobody judges the stop: it is pushed at once, as stalled, once.
func TestAStallWithNoChiefPushesOnce(t *testing.T) {
	f := newStateFixture(t, false)
	f.statePass(t)
	f.statePass(t)
	e := f.oneEpisode(t)
	if got := f.episodePushes(); len(got) != 1 {
		t.Fatalf("pushes: %v", got)
	}
	if p := f.payloads()[0]; p.Title != "Gold:1 · tmux main:1 @ ws is stalled" || !strings.HasPrefix(p.Body, "Stalled · idle ") {
		t.Fatalf("payload: %q / %q (episode %+v)", p.Title, p.Body, e)
	}
}

// TestShadowModeNeverQuietsAStop (regression): the owner in shadow mode — a
// chief judges nothing live — so the stop pushes, whatever the chief would
// have said.
func TestShadowModeNeverQuietsAStop(t *testing.T) {
	f := newStateFixture(t, true)
	j := withJev(t, f.nudgeFixture)
	j.set(200, 0.02, 0.03, 0.95) // it asked nothing: only shadow mode can keep it pushed
	f.gw.set(`{"decision":"leave","reason":"nothing to do"}`)
	f.pass(t)
	f.statePass(t)
	if got := f.episodePushes(); len(got) != 1 {
		t.Fatalf("pushes: %v", got)
	}
}

// TestALiveLeaveOnAStopThatAskedNothingIsQuiet (regression): the chief, live,
// says leave it and the classifier says the session asked nothing: a quiet
// row — no push, its reason recorded.
func TestALiveLeaveOnAStopThatAskedNothingIsQuiet(t *testing.T) {
	f := newStateFixture(t, true)
	f.live()
	j := withJev(t, f.nudgeFixture)
	j.set(200, 0.02, 0.03, 0.95)
	f.gw.set(`{"decision":"leave","reason":"it is done"}`)
	f.statePass(t)
	f.pass(t)
	f.statePass(t)
	e := f.oneEpisode(t)
	if got := f.episodePushes(); len(got) != 0 {
		t.Fatalf("pushes: %v", got)
	}
	if q := f.quietOf(t, e.ID); q != sessionstate.QuietLeft {
		t.Fatalf("quiet %q", q)
	}
}

// TestALiveLeaveOnAStopThatAskedPushes (regression; operator decision 12,
// "Push it anyway"): the classifier says the session asked, so the chief's
// leave cannot quiet it — and the push carries none of the chief's words
// (no notice is raised).
func TestALiveLeaveOnAStopThatAskedPushes(t *testing.T) {
	f := newStateFixture(t, true)
	f.live()
	j := withJev(t, f.nudgeFixture)
	j.set(200, 0.91, 0.05, 0.04)
	f.gw.set(`{"decision":"leave","reason":"it is done"}`)
	f.statePass(t)
	f.pass(t)
	f.statePass(t)
	if got := f.pushes(); len(got) != 1 || !strings.Contains(got[0], "attention-episode-") {
		t.Fatalf("pushes: %v", got)
	}
	if n := f.askRows(t); n != 0 {
		t.Fatalf("%d notice(s) raised", n)
	}
	if p := f.payloads()[0]; p.Title != "Gold:1 · tmux main:1 @ ws needs you" || strings.Contains(p.Body, "done") {
		t.Fatalf("payload: %q / %q", p.Title, p.Body)
	}
}

// TestAQueuedNudgeHoldsThePushAndASentOneKeepsItQuiet: a live nudge queued
// for the window holds the stop's push (guard: nothing pushes while it waits);
// once its paste is SENT the stop is quiet (nudged) for good.
func TestAQueuedNudgeHoldsThePushAndASentOneKeepsItQuiet(t *testing.T) {
	f := newStateFixture(t, true)
	f.live()
	f.statePass(t)
	f.pass(t)
	f.statePass(t)
	if got := f.episodePushes(); len(got) != 0 {
		t.Fatalf("pushes while the nudge is queued: %v", got)
	}
	d := f.one(t)
	if d.Outcome != "queued" {
		t.Fatalf("decision %+v", d)
	}
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE chief_nudges SET outcome = 'sent' WHERE id = $1`, d.ID); err != nil {
		t.Fatal(err)
	}
	f.advance(episodeNudgeHold + time.Minute)
	f.statePass(t)
	e := f.oneEpisode(t)
	if got := f.episodePushes(); len(got) != 0 {
		t.Fatalf("pushes after the nudge was sent: %v", got)
	}
	if q := f.quietOf(t, e.ID); q != sessionstate.QuietNudged {
		t.Fatalf("quiet %q", q)
	}
}

// TestAFailedNudgePushes (regression): a live nudge whose send failed never
// counts as handled — the stop pushes.
func TestAFailedNudgePushes(t *testing.T) {
	f := newStateFixture(t, true)
	f.live()
	f.statePass(t)
	f.pass(t)
	d := f.one(t)
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE chief_nudges SET outcome = 'failed' WHERE id = $1`, d.ID); err != nil {
		t.Fatal(err)
	}
	f.statePass(t)
	if got := f.episodePushes(); len(got) != 1 {
		t.Fatalf("pushes: %v", got)
	}
}

// TestAQueuedNudgeNotSentInTimePushes (regression): the hold is bounded.
func TestAQueuedNudgeNotSentInTimePushes(t *testing.T) {
	f := newStateFixture(t, true)
	f.live()
	f.statePass(t)
	f.pass(t)
	f.advance(episodeNudgeHold + time.Minute)
	f.statePass(t)
	if got := f.episodePushes(); len(got) != 1 {
		t.Fatalf("pushes: %v", got)
	}
}

// TestAStopTheChiefNeverJudgesPushesAfterItsBound (regression): live, but no
// decision arrives (the chief's sweep never ran): held, then pushed past
// episodeChiefWait.
func TestAStopTheChiefNeverJudgesPushesAfterItsBound(t *testing.T) {
	f := newStateFixture(t, true)
	f.live()
	f.statePass(t)
	if got := f.episodePushes(); len(got) != 0 {
		t.Fatalf("pushes before the bound: %v", got)
	}
	f.advance(episodeChiefWait + time.Minute)
	f.statePass(t)
	if got := f.episodePushes(); len(got) != 1 {
		t.Fatalf("pushes after the bound: %v", got)
	}
}

// TestAnEndedStopClosesItsNotification (regression): sustained work ends the
// episode, and its notification is closed; a restart (an ended, pushed,
// uncleared row) is closed by the next pass.
func TestAnEndedStopClosesItsNotification(t *testing.T) {
	f := newStateFixture(t, false)
	f.statePass(t)
	e := f.oneEpisode(t)
	tag := "attention-episode-" + strconv.FormatInt(e.ID, 10)
	f.status(mirror.StatusBusy, 0)
	f.statePass(t)
	f.advance(sessionstate.EndAfter + time.Second)
	f.statePass(t)
	got := f.pushes()
	if len(got) != 2 || got[0] != "attention "+tag || got[1] != "resolved "+tag {
		t.Fatalf("pushes: %v", got)
	}
	// A restart: the row ended, pushed, never cleared.
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE session_episodes SET cleared_at = NULL WHERE id = $1`, e.ID); err != nil {
		t.Fatal(err)
	}
	f.statePass(t)
	if got := f.pushes(); len(got) != 3 || got[2] != "resolved "+tag {
		t.Fatalf("pushes after the restart: %v", got)
	}
}

// TestTheRepushFloorDelaysButNeverDropsAPush (regression): a session whose
// previous episode pushed under the floor ago is HELD, not dropped — a stop
// the previous one's notification no longer covers (it ended and was closed)
// is pushed once the floor has passed, if it is still open. Probed over an
// hour of passes.
func TestTheRepushFloorDelaysButNeverDropsAPush(t *testing.T) {
	f := newStateFixture(t, false)
	f.statePass(t)
	first := f.oneEpisode(t)
	// The first stop ends at once, as gone-and-back would, its notification
	// closed — and the session stops AGAIN on a new streak (it ran in
	// between): a NEW stop inside the floor. (Back on the SAME streak it would
	// be the same stop, which carries the first push and is never pushed again
	// — sessionstate.resumedReasons, session_gone_resume_pg_test.go.)
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE session_episodes SET ended_at = now(), end_reason = 'gone', cleared_at = now() WHERE id = $1`, first.ID); err != nil {
		t.Fatal(err)
	}
	f.statusSince(mirror.StatusIdle, nudgeSince+60_000, 20*time.Minute)
	f.statePass(t)
	list := f.episodes(t)
	if len(list) != 2 {
		t.Fatalf("episodes: %+v", list)
	}
	if got := f.episodePushes(); len(got) != 1 {
		t.Fatalf("pushes inside the floor: %v", got)
	}
	if q := f.quietOf(t, list[0].ID); q != sessionstate.QuietRepushFloor {
		t.Fatalf("quiet %q", q)
	}
	pushed := 0
	for i := 0; i < 120; i++ { // an hour of passes, 30 s apart
		f.advance(30 * time.Second)
		f.statePass(t)
		pushed = 0
		for _, p := range f.episodePushes() {
			if p == "attention attention-episode-"+strconv.FormatInt(list[0].ID, 10) {
				pushed++
			}
		}
		if pushed > 0 {
			break
		}
	}
	if pushed != 1 {
		t.Fatalf("the held stop pushed %d time(s); pushes %v", pushed, f.episodePushes())
	}
	if q := f.quietOf(t, list[0].ID); q != "" {
		t.Fatalf("quiet after the push %q", q)
	}
}

// TestAnEpisodesTapOpensItsSession (regression): GET /attention/open/
// episode-<id> resolves the session's window when tapped; an id that is no
// episode falls back to the queue.
func TestAnEpisodesTapOpensItsSession(t *testing.T) {
	f := newStateFixture(t, false)
	f.statePass(t)
	e := f.oneEpisode(t)
	w := f.call(t, http.MethodGet, "/attention/open/episode-"+strconv.FormatInt(e.ID, 10), nil, f.operator())
	if want := (ui.MirrorRef{Host: "ws", Session: "main", Window: "@1"}).ViewPath(); w.Code != http.StatusSeeOther || w.Header().Get("Location") != want {
		t.Fatalf("tap: %d %q, want %q", w.Code, w.Header().Get("Location"), want)
	}
	w = f.call(t, http.MethodGet, "/attention/open/episode-999999999", nil, f.operator())
	if loc := w.Header().Get("Location"); w.Code != http.StatusSeeOther || loc != "/attention#episode-999999999" {
		t.Fatalf("missing episode: %d %q", w.Code, loc)
	}
}

// TestTheAsksPageDrawsStalledAndWaitingRows (regression): the section's row —
// where, state, a link through the tap route, and no answer box; a quiet row
// says why.
func TestTheAsksPageDrawsStalledAndWaitingRows(t *testing.T) {
	f := newStateFixture(t, false)
	f.statePass(t)
	e := f.oneEpisode(t)
	body := f.page(t, "/attention")
	i := strings.Index(body, `data-testid="attention-episodes"`)
	if i < 0 {
		t.Fatalf("no section:\n%s", body)
	}
	sec := body[i:]
	sec = sec[:strings.Index(sec, "</section>")]
	for _, want := range []string{`id="episode-` + strconv.FormatInt(e.ID, 10) + `"`, "Stalled", "main:1 @ ws", `href="/attention/open/episode-`} {
		if !strings.Contains(sec, want) {
			t.Fatalf("section lacks %q:\n%s", want, sec)
		}
	}
	if strings.Contains(sec, "<textarea") || strings.Contains(sec, "<form") {
		t.Fatalf("an answer box in the section:\n%s", sec)
	}
}

// TestSessionStateOffKeepsTodaysPushes (INVARIANT GUARD — green at the base
// too): MUSTER_SESSION_STATE off, a waiting window pushes as today (its
// session item's tag; since the follow-up to #463 once its grace has passed) and a live
// chief ask raises today's notice.
func TestSessionStateOffKeepsTodaysPushes(t *testing.T) {
	f := newNudgeFixture(t, true)
	f.mirror.mu.Lock()
	f.mirror.windows[0].Status = mirror.StatusWaiting
	f.mirror.mu.Unlock()
	f.state(func(s *mirror.AgentState) { s.Status = mirror.StatusWaiting })
	f.srv.AttentionMirrorChanged(authz.OperatorUserID) // the host's first sight: seeded
	f.mirror.mu.Lock()
	f.mirror.windows[0].Status = mirror.StatusIdle
	f.mirror.mu.Unlock()
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.mirror.mu.Lock()
	f.mirror.windows[0].Status = mirror.StatusWaiting
	f.mirror.mu.Unlock()
	f.srv.attention.mu.Lock()
	f.srv.attention.pushedAt = map[string]time.Time{}
	f.srv.attention.mu.Unlock()
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	pastWaitingGrace(f.srv)
	got := f.pushes()
	sessionPushes := 0
	for _, p := range got {
		if strings.HasPrefix(p, "attention attention-session-") {
			sessionPushes++
		}
		if strings.Contains(p, "episode-") {
			t.Fatalf("an episode push with session state off: %v", got)
		}
	}
	if sessionPushes == 0 {
		t.Fatalf("waiting pushes: %v", got)
	}
	f.mirror.mu.Lock()
	f.mirror.windows[0].Status = mirror.StatusIdle
	f.mirror.mu.Unlock()
	f.idle(20 * time.Minute)
	f.gw.set(nudgeAskReply)
	f.pass(t)
	if n := f.askRows(t); n != 1 {
		t.Fatalf("notices: %d", n)
	}
}

// TestALiveLeaveWhenTheClassifierFailedPushes (regression): the chief, live,
// says leave it, and the classifier FAILED — so the session counts as having
// asked (sessionstate.Asked), and a failure cannot quiet it: the stop pushes.
func TestALiveLeaveWhenTheClassifierFailedPushes(t *testing.T) {
	f := newStateFixture(t, true)
	f.live()
	j := withJev(t, f.nudgeFixture)
	j.set(502, 0, 0, 0)
	f.gw.set(`{"decision":"leave","reason":"it is done"}`)
	f.statePass(t)
	f.pass(t)
	if d := f.one(t); d.Outcome != "left" {
		t.Fatalf("decision %+v", d)
	}
	f.statePass(t)
	if got := f.episodePushes(); len(got) != 1 {
		t.Fatalf("pushes: %v", got)
	}
}

// TestTwoProcessesThatReadAStopUnpushedPushItOnce (regression): the claim.
// Two replicas (or the hook and a pass) each read the same open episode before
// either pushed it, and each tries: exactly one push is sent.
func TestTwoProcessesThatReadAStopUnpushedPushItOnce(t *testing.T) {
	f := newStateFixture(t, false)
	f.status(mirror.StatusWaiting, 5*time.Second)
	octx := f.srv.attentionPrincipal(context.Background(), authz.OperatorUserID)
	ep, claimed, err := f.eps.Open(octx, sessionstate.Episode{Kind: sessionstate.KindMirror, Key: nudgeSID, State: sessionstate.StateNeedsYou,
		Waiting: true, Asked: true, Host: "ws", TmuxSession: "main", Window: "@1", Index: 1})
	if err != nil || !claimed {
		t.Fatalf("open: %v %v", claimed, err)
	}
	f.srv.pushEpisode(octx, ep, 0, nil) // both read ep unpushed
	f.srv.pushEpisode(octx, ep, 0, nil)
	if got := f.episodePushes(); len(got) != 1 {
		t.Fatalf("pushes: %v", got)
	}
}

// TestAShadowOwnersStopPushesWithoutWaiting (regression): in shadow mode no
// chief judges the stop live, so its push does not wait for a decision.
func TestAShadowOwnersStopPushesWithoutWaiting(t *testing.T) {
	f := newStateFixture(t, true)
	f.statePass(t)
	if got := f.episodePushes(); len(got) != 1 {
		t.Fatalf("pushes: %v", got)
	}
}

// foreignEpisodes is the store with every Get answering another owner's row:
// what a read with row-level security rolled back could return.
type foreignEpisodes struct{ *sessionstate.PG }

func (s foreignEpisodes) Get(ctx context.Context, id int64) (sessionstate.Episode, bool, error) {
	e, ok, err := s.PG.Get(ctx, id)
	e.Owner += 1000
	return e, ok, err
}

// TestATapNeverOpensAnotherOwnersEpisode (regression): the explicit owner
// check on the tap route holds without row-level security's.
func TestATapNeverOpensAnotherOwnersEpisode(t *testing.T) {
	f := newStateFixture(t, false)
	f.statePass(t)
	e := f.oneEpisode(t)
	f.srv.ext.Episodes = foreignEpisodes{f.eps}
	key := "episode-" + strconv.FormatInt(e.ID, 10)
	w := f.call(t, http.MethodGet, "/attention/open/"+key, nil, f.operator())
	if loc := w.Header().Get("Location"); loc != "/attention#"+key {
		t.Fatalf("tap: %d %q", w.Code, loc)
	}
}

// seenIdleThenWaiting has the hub hook see the fixture's host first with its
// window idle (seeding the host, as a running muster has), then waiting.
func (f *stateFixture) seenIdleThenWaiting() {
	f.status(mirror.StatusIdle, time.Minute)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.status(mirror.StatusWaiting, time.Second)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
}

// TestAWaitingWindowWithNoSessionKeyKeepsTodaysPush (regression; audit F1):
// a window session state cannot key (a host that sends no Claude session id)
// keeps today's summary-only push with the flag on — never silence; since
// since the follow-up to #463 once its grace has passed — and opens no episode.
func TestAWaitingWindowWithNoSessionKeyKeepsTodaysPush(t *testing.T) {
	f := newStateFixture(t, false)
	f.mirror.mu.Lock()
	f.mirror.windows[0].ClaudeSession = ""
	f.mirror.mu.Unlock()
	f.seenIdleThenWaiting()
	f.statePass(t)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.advance(EpisodeWaitingGrace + time.Second)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	got := f.pushes()
	if len(got) != 1 || !strings.HasPrefix(got[0], "attention attention-session-") {
		t.Fatalf("pushes: %v", got)
	}
	if list := f.episodes(t); len(list) != 0 {
		t.Fatalf("episodes: %+v", list)
	}
	// …and it stays a list item on the page: no episode row stands in for it.
	if body := f.page(t, "/attention"); !strings.Contains(body, `data-kind="session"`) {
		t.Fatalf("the keyless window is not on the list:\n%s", body)
	}
}

// TestAWindowThatGainsItsKeyMidWaitPushesOnce (regression; round 2): a
// keyless waiting window gets today's push (once its grace has passed); its
// Claude session id then arrives while it still waits. The stop is pushed
// once — not again as an episode, by the hook or by the pass.
func TestAWindowThatGainsItsKeyMidWaitPushesOnce(t *testing.T) {
	f := newStateFixture(t, false)
	f.mirror.mu.Lock()
	f.mirror.windows[0].ClaudeSession = ""
	f.mirror.mu.Unlock()
	f.seenIdleThenWaiting()
	f.advance(EpisodeWaitingGrace + time.Second)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.mirror.mu.Lock()
	f.mirror.windows[0].ClaudeSession = nudgeSID
	f.mirror.mu.Unlock()
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.statePass(t)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.statePass(t)
	if got := f.pushes(); len(got) != 1 || !strings.HasPrefix(got[0], "attention attention-session-") {
		t.Fatalf("pushes: %v", got)
	}
}

// TestOneStopIsOnePushAcrossTheHookAndThePass (regression; round 2, the
// shared rule): the hook (episodeCover) and the state pass (stall facts) key
// windows by ONE rule, so for each kind of window the stop is pushed exactly
// once across both — never twice, never not at all.
func TestOneStopIsOnePushAcrossTheHookAndThePass(t *testing.T) {
	// Four rounds, 30 s apart: past the 60 s waiting grace, so a held push is
	// sent inside the run.
	run := func(t *testing.T, f *stateFixture) {
		for i := 0; i < 4; i++ {
			f.statePass(t)
			f.srv.AttentionMirrorChanged(authz.OperatorUserID)
			f.advance(30 * time.Second)
		}
	}
	t.Run("a host offline, then back", func(t *testing.T) {
		// Its window is first seen waiting while the host is offline (inside
		// the grace that keeps its items): nothing pushes until it is back and
		// reporting — then the stop pushes once.
		f := newStateFixture(t, false)
		f.mirror.mu.Lock()
		f.mirror.online, f.mirror.offlineSince = false, f.clock.Add(-time.Minute)
		f.mirror.mu.Unlock()
		f.status(mirror.StatusWaiting, time.Second)
		f.srv.AttentionMirrorChanged(authz.OperatorUserID)
		run(t, f)
		if got := f.pushes(); len(got) != 0 {
			t.Fatalf("pushes while offline: %v", got)
		}
		f.mirror.mu.Lock()
		f.mirror.online = true
		f.mirror.mu.Unlock()
		f.srv.AttentionMirrorChanged(authz.OperatorUserID)
		run(t, f)
		// (A "resolved" for the diff's never-shown seeded item may follow, as
		// today when a seeded window stops waiting: it closes nothing.)
		if got := alerts(f.pushes()); len(got) != 1 || !strings.HasPrefix(got[0], "attention attention-episode-") {
			t.Fatalf("pushes once back: %v", f.pushes())
		}
	})
	t.Run("a keyed window on a healthy host", func(t *testing.T) {
		f := newStateFixture(t, false)
		f.seenIdleThenWaiting()
		run(t, f)
		if got := f.pushes(); len(got) != 1 || !strings.HasPrefix(got[0], "attention attention-episode-") {
			t.Fatalf("pushes: %v", got)
		}
	})
}

// TestAKeyedWaitingWindowPushesOnlyItsEpisode (regression; audit F1's control
// and F3): a host seen first while idle, so today's diff WOULD push the
// window — with a key, it is the episode's push alone, never both.
func TestAKeyedWaitingWindowPushesOnlyItsEpisode(t *testing.T) {
	f := newStateFixture(t, false)
	f.seenIdleThenWaiting()
	f.statePass(t)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.pastGrace(t)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	e := f.oneEpisode(t)
	if got := f.pushes(); len(got) != 1 || got[0] != "attention attention-episode-"+strconv.FormatInt(e.ID, 10) {
		t.Fatalf("pushes: %v", got)
	}
}

// TestAWaitThatNamesItsAskLaterPushesOnce (regression; audit F8): a host
// reports the window waiting before the ask it waits on is known (here: the
// held AskUserQuestion registers a moment later). Exactly one push either way:
//   - the stop's episode pushed already (no grace): the now-answerable ask
//     does not push a second time;
//   - the stop's episode is still HELD in its waiting grace: it is ONE stop
//     either way, and the grace passing pushes it once — as the ASK, its
//     question in full, under the episode's tag (operator decision after #463:
//     an answerable ask is an episode too; until then the ask pushed AT ONCE
//     from the waiting diff, and this case asserted that push).
func TestAWaitThatNamesItsAskLaterPushesOnce(t *testing.T) {
	ask := func(t *testing.T, f *stateFixture) {
		t.Helper()
		if _, err := f.srv.Questions().Open(askq.Key{Owner: 1, Source: askSourceMirror, Scope: "ws", Session: nudgeSID, ToolUseID: "toolu_push_flip"},
			[]askq.Question{{Question: "Which database?", Options: []askq.Option{{Label: "postgres"}, {Label: "sqlite"}}}}, askq.NewID()); err != nil {
			t.Fatal(err)
		}
		f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	}
	t.Run("the episode pushed already", func(t *testing.T) {
		f := newStateFixture(t, false)
		f.noGrace()
		f.seenIdleThenWaiting()
		ask(t, f)
		if got := f.pushes(); len(got) != 1 || !strings.HasPrefix(got[0], "attention attention-episode-") {
			t.Fatalf("pushes: %v", got)
		}
	})
	t.Run("the episode still held", func(t *testing.T) {
		f := newStateFixture(t, false)
		f.seenIdleThenWaiting()
		if got := f.pushes(); len(got) != 0 {
			t.Fatalf("pushes inside the grace: %v", got)
		}
		ask(t, f)
		if got := f.pushes(); len(got) != 0 {
			t.Fatalf("pushes when the ask registered: %v, want none inside the grace", got)
		}
		f.pastGrace(t)
		f.srv.AttentionMirrorChanged(authz.OperatorUserID)
		if got := f.pushes(); len(got) != 1 || !strings.HasPrefix(got[0], "attention attention-episode-") {
			t.Fatalf("pushes: %v, want the stop's one push", got)
		}
		if p := f.payloads()[0]; !strings.HasPrefix(p.Body, "Which database?") {
			t.Fatalf("body %q, want the ask's question", p.Body)
		}
	})
}

// TestTheAsksListMovesAWaitingWindowIntoTheSectionLive (regression; audit F4
// A06 and F5): with the flag on a keyed waiting window is a section row, not
// also a "Session waiting" item — and the list's live refresh carries the
// section, so an open page shows a window that just started waiting.
func TestTheAsksListMovesAWaitingWindowIntoTheSectionLive(t *testing.T) {
	f := newStateFixture(t, false)
	f.seenIdleThenWaiting()
	e := f.oneEpisode(t)
	for _, path := range []string{"/attention", "/attention/items"} {
		body := f.page(t, path)
		if strings.Contains(body, `data-kind="session"`) {
			t.Fatalf("%s: the window is a list item too:\n%s", path, body)
		}
		if !strings.Contains(body, `id="episode-`+strconv.FormatInt(e.ID, 10)+`"`) {
			t.Fatalf("%s: no row:\n%s", path, body)
		}
		if strings.Contains(body, `data-testid="attention-empty"`) {
			t.Fatalf("%s: says nothing is waiting beside a row:\n%s", path, body)
		}
	}
}

// TestAQuietRowIsDrawnMutedWithItsReason (regression; audit F4 A12): a left
// stop's row is muted and says why.
func TestAQuietRowIsDrawnMutedWithItsReason(t *testing.T) {
	f := newStateFixture(t, true)
	f.live()
	j := withJev(t, f.nudgeFixture)
	j.set(200, 0.02, 0.03, 0.95)
	f.gw.set(`{"decision":"leave","reason":"it is done"}`)
	f.statePass(t)
	f.pass(t)
	f.statePass(t)
	e := f.oneEpisode(t)
	body := f.page(t, "/attention")
	i := strings.Index(body, `id="episode-`+strconv.FormatInt(e.ID, 10)+`"`)
	if i < 0 {
		t.Fatalf("no row:\n%s", body)
	}
	row := body[i : i+strings.Index(body[i:], "</li>")]
	if !strings.Contains(row, "opacity-60") || !strings.Contains(row, `data-attention-episode-quiet="left"`) {
		t.Fatalf("row: %s", row)
	}
}

// TestATapOnAnotherOwnersRealEpisodeFallsBack (regression; audit F4): an
// episode of ANOTHER user's, tapped by the operator, opens the queue — under
// real row-level security, not a stub.
func TestATapOnAnotherOwnersRealEpisodeFallsBack(t *testing.T) {
	f := newStateFixture(t, false)
	ep, claimed, err := f.eps.Open(asUser(f.member), sessionstate.Episode{Kind: sessionstate.KindMirror, Key: "member-session-key",
		State: sessionstate.StateStalled, Asked: true, Host: "ws", TmuxSession: "main", Window: "@1", Index: 1})
	if err != nil || !claimed {
		t.Fatalf("open: %v %v", claimed, err)
	}
	key := "episode-" + strconv.FormatInt(ep.ID, 10)
	w := f.call(t, http.MethodGet, "/attention/open/"+key, nil, f.operator())
	if loc := w.Header().Get("Location"); w.Code != http.StatusSeeOther || loc != "/attention#"+key {
		t.Fatalf("tap: %d %q", w.Code, loc)
	}
}

// alerts is pushes without the "resolved" ones: what alerted the owner.
func alerts(pushes []string) []string {
	var out []string
	for _, p := range pushes {
		if strings.HasPrefix(p, "attention ") {
			out = append(out, p)
		}
	}
	return out
}
