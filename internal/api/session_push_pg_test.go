package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/sessionstate"
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

// payloads is every notification that reached the operator's device.
func (f *stateFixture) payloads() []push.Payload {
	f.srv.WaitForPushes()
	return f.pusher.to(ep("attn-operator"))
}

// waitWithSecrets puts the fixture's window waiting on a prompt whose tool,
// last words and window name are all secret-shaped.
func (f *stateFixture) waitWithSecrets() {
	f.status(mirror.StatusWaiting, 5*time.Second)
	f.mirror.mu.Lock()
	defer f.mirror.mu.Unlock()
	f.mirror.windows[0].Label = "deploy " + pushSecret
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
// session state on, a window that starts waiting is pushed at once FROM THE
// HUB HOOK as its episode — one push, tagged by the episode, naming the tmux
// session and window index and host, and carrying none of the session's text
// (no tool, no command, no window name, no last words). The state pass and a
// second hook nudge send nothing more.
func TestAWaitingWindowPushesOnceAtOnceWithNoSessionText(t *testing.T) {
	f := newStateFixture(t, false)
	f.waitWithSecrets()
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	e := f.oneEpisode(t)
	want := "attention attention-episode-" + strconv.FormatInt(e.ID, 10)
	if got := f.pushes(); len(got) != 1 || got[0] != want {
		t.Fatalf("pushes after the hook: %v, want [%s]", got, want)
	}
	p := f.payloads()[0]
	if p.Title != "main:1 @ ws needs you" || p.Body != "Needs you · just now" {
		t.Fatalf("payload: title %q body %q", p.Title, p.Body)
	}
	for _, s := range []string{p.Title, p.Body, p.Data["url"]} {
		if strings.Contains(s, "PUSHSECRET") || strings.Contains(s, "deploy") || strings.Contains(s, "Bash") {
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
// and pushes a waiting stop; the hook then finds it pushed and sends nothing;
// a restarted process (a fresh hook state) sends nothing either.
func TestTheHookAndThePassPushAStopOnce(t *testing.T) {
	f := newStateFixture(t, false)
	f.status(mirror.StatusWaiting, 5*time.Second)
	f.statePass(t)
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
	if p := f.payloads()[0]; p.Title != "main:1 @ ws is stalled" || !strings.HasPrefix(p.Body, "Stalled · idle ") {
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
	if p := f.payloads()[0]; p.Title != "main:1 @ ws needs you" || strings.Contains(p.Body, "done") {
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

// TestTheRepushFloorKeepsTheSameStopQuiet (regression): a session whose
// previous episode pushed within the floor of this one opening is not pushed
// again.
func TestTheRepushFloorKeepsTheSameStopQuiet(t *testing.T) {
	f := newStateFixture(t, false)
	f.statePass(t)
	first := f.oneEpisode(t)
	// The first stop ends as gone-and-back would, at once.
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE session_episodes SET ended_at = now(), end_reason = 'gone', cleared_at = now() WHERE id = $1`, first.ID); err != nil {
		t.Fatal(err)
	}
	f.statePass(t)
	list := f.episodes(t)
	if len(list) != 2 {
		t.Fatalf("episodes: %+v", list)
	}
	if got := f.episodePushes(); len(got) != 1 {
		t.Fatalf("pushes: %v", got)
	}
	if q := f.quietOf(t, list[0].ID); q != sessionstate.QuietRepushFloor {
		t.Fatalf("quiet %q", q)
	}
}

// TestAnEpisodesTapOpensItsSession (regression): GET /attention/open/
// episode-<id> resolves the session's window when tapped; another owner's
// episode id falls back to the queue.
func TestAnEpisodesTapOpensItsSession(t *testing.T) {
	f := newStateFixture(t, false)
	f.statePass(t)
	e := f.oneEpisode(t)
	w := f.call(t, http.MethodGet, "/attention/open/episode-"+strconv.FormatInt(e.ID, 10), nil, f.operator())
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/mirror/ws/main/@1" && !strings.HasPrefix(w.Header().Get("Location"), "/mirror/") {
		t.Fatalf("tap: %d %q", w.Code, w.Header().Get("Location"))
	}
	if loc := w.Header().Get("Location"); strings.HasPrefix(loc, "/attention#") {
		t.Fatalf("tap fell back to the queue: %q", loc)
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
// session item's tag) and a live chief ask raises today's notice.
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
	f.srv.pushEpisode(octx, ep, 0) // both read ep unpushed
	f.srv.pushEpisode(octx, ep, 0)
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
