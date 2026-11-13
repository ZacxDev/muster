package api

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sessionstate"
)

// A STOP BACK FROM 'gone' ON THE SAME STREAK IS NOT PUSHED AGAIN
// (sessionstate.Resumed, PG.Open; session_push.go's header). A host drops,
// reconnects, and its listing omits its idle windows for a pass — each ends
// gone — then lists them again, unchanged. Before the resume rule each opened
// a new episode on its unchanged streak and was pushed again: a burst of the
// same stops. A session that ran in between, a genuinely new window, and a
// stop that was never pushed before it went are all still pushed.
//
// These tests use only names that exist at the base too, so each is run there
// to show which are red before the fix.

// sentEpisodes is the episode pushes SENT so far (not their clears), as
// "attention attention-episode-<id>" lines.
func (f *stateFixture) sentEpisodes() []string {
	var out []string
	for _, p := range f.pushes() {
		if strings.HasPrefix(p, "attention attention-episode-") {
			out = append(out, p)
		}
	}
	return out
}

// listing sets the fixture host's online state and window list.
func (f *stateFixture) listing(online bool, ws []mirror.Window) {
	f.mirror.mu.Lock()
	defer f.mirror.mu.Unlock()
	f.mirror.online = online
	f.mirror.windows = ws
}

// windowsNow is a copy of the fixture host's window list.
func (f *stateFixture) windowsNow() []mirror.Window {
	f.mirror.mu.Lock()
	defer f.mirror.mu.Unlock()
	return append([]mirror.Window(nil), f.mirror.windows...)
}

// shellOnly is a reporting host's listing that no longer holds the session's
// window: what ends its episode 'gone'.
var shellOnly = []mirror.Window{{Session: "main", ID: "@2", Index: 2, Label: "shell"}}

// dropAndGone runs a pass with the host offline (nothing ends), then one with
// it back but listing without the window (its episode ends gone), and checks
// that episode ended gone.
func (f *stateFixture) dropAndGone(t *testing.T, ep sessionstate.Episode) {
	t.Helper()
	f.advance(20 * time.Second)
	f.listing(false, f.windowsNow())
	f.statePass(t)
	if e := f.oneOf(t, ep.ID); !e.Open() {
		t.Fatalf("an offline host ended the episode: %+v", e)
	}
	f.advance(10 * time.Second)
	f.listing(true, shellOnly)
	f.statePass(t)
	if e := f.oneOf(t, ep.ID); e.Open() || e.EndReason != sessionstate.EndGone {
		t.Fatalf("the window left the reporting host's listing: %+v, want ended gone", e)
	}
}

// oneOf is episode id as the episodes read lists it.
func (f *stateFixture) oneOf(t *testing.T, id int64) sessionstate.Episode {
	t.Helper()
	for _, e := range f.episodes(t) {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("no episode %d", id)
	return sessionstate.Episode{}
}

func episodeTag(id int64) string { return "attention attention-episode-" + strconv.FormatInt(id, 10) }

// TestAReconnectWithUnchangedStreaksPushesNothingNew (regression): a stalled
// window is pushed; its host drops and reconnects with a listing that omits
// the window (its episode ends gone), then lists it again with the SAME
// streak. The new episode carries the first one's push and nothing more is
// sent — not at once, not after the repush floor, not an hour later.
func TestAReconnectWithUnchangedStreaksPushesNothingNew(t *testing.T) {
	f := newStateFixture(t, false)
	f.statePass(t)
	first := f.oneEpisode(t)
	if got := f.sentEpisodes(); len(got) != 1 || got[0] != episodeTag(first.ID) {
		t.Fatalf("the stop's push: %v", got)
	}
	full := f.windowsNow()
	f.dropAndGone(t, first)

	f.advance(30 * time.Second)
	f.listing(true, full)
	f.statePass(t)
	list := f.episodes(t)
	if len(list) != 2 || !list[0].Open() || list[0].Seq != first.Seq+1 || list[0].StreakKey != first.StreakKey {
		t.Fatalf("the window back on its streak: %+v, want a second open episode on streak %q", list, first.StreakKey)
	}
	for _, d := range []time.Duration{episodeRepushFloor + time.Second, episodeChiefWait, time.Hour} {
		f.advance(d)
		f.statePass(t)
	}
	if got := f.sentEpisodes(); len(got) != 1 {
		t.Fatalf("pushes: %v, want only the first stop's — the same stop back from gone was pushed again", got)
	}
	if back := f.oneOf(t, list[0].ID); !back.Open() || back.PushedAt.IsZero() {
		t.Fatalf("the stop back from gone: %+v, want open and counted as pushed", back)
	}
}

// TestAReconnectAfterTheSessionWorkedPushes: the same drop, but the session
// ran in between (a new status time: a new streak). That is a new stop, and
// it is pushed.
func TestAReconnectAfterTheSessionWorkedPushes(t *testing.T) {
	f := newStateFixture(t, false)
	f.statePass(t)
	first := f.oneEpisode(t)
	full := f.windowsNow()
	f.dropAndGone(t, first)

	f.advance(episodeRepushFloor + time.Second)
	f.listing(true, full)
	f.statusSince(mirror.StatusIdle, nudgeSince+600_000, 20*time.Minute)
	f.statePass(t)
	list := f.episodes(t)
	if len(list) != 2 || list[0].StreakKey == first.StreakKey {
		t.Fatalf("episodes: %+v, want a second one on a new streak", list)
	}
	if got := f.sentEpisodes(); len(got) != 2 || got[1] != episodeTag(list[0].ID) {
		t.Fatalf("pushes: %v, want the new stop pushed", got)
	}
}

// TestAClosedWindowAndANewOnePushes: the window is genuinely closed (gone) and
// a new window holds a NEW conversation — a new session key — on the very
// same status time. It is a new stop, and it is pushed.
func TestAClosedWindowAndANewOnePushes(t *testing.T) {
	f := newStateFixture(t, false)
	f.statePass(t)
	first := f.oneEpisode(t)
	full := f.windowsNow()
	f.dropAndGone(t, first)

	f.advance(episodeRepushFloor + time.Second)
	fresh := full[0]
	fresh.ID, fresh.Index, fresh.Label = "@7", 7, "Gold:7"
	fresh.ClaudeSession = "5c1d9e0a-2b3f-4a6d-8e7c-1f0a9b8c7d6e"
	f.listing(true, append([]mirror.Window{fresh}, shellOnly...))
	f.statePass(t)
	list := f.episodes(t)
	if len(list) != 2 || list[0].Key != fresh.ClaudeSession || list[0].StreakKey != first.StreakKey {
		t.Fatalf("episodes: %+v, want the new window's own episode on the same streak", list)
	}
	if got := f.sentEpisodes(); len(got) != 2 || got[1] != episodeTag(list[0].ID) {
		t.Fatalf("pushes: %v, want the new window pushed", got)
	}
}

// TestAHeldStopAcrossADropStillPushesOnce: a waiting window's stop is HELD in
// its grace (not pushed) when its host drops and its listing omits the window
// (ended gone, unpushed). Listed back, still waiting on the same streak, its
// new episode carries no push — the hold delays, it never silences — and it is
// pushed exactly once after its grace.
func TestAHeldStopAcrossADropStillPushesOnce(t *testing.T) {
	f := newStateFixture(t, false)
	f.status(mirror.StatusWaiting, 5*time.Second)
	f.statePass(t)
	first := f.oneEpisode(t)
	if !first.Waiting || !first.PushedAt.IsZero() || len(f.sentEpisodes()) != 0 {
		t.Fatalf("a held wait: %+v, pushes %v", first, f.sentEpisodes())
	}
	full := f.windowsNow()
	f.dropAndGone(t, first)
	if got := f.sentEpisodes(); len(got) != 0 {
		t.Fatalf("pushes during the drop: %v", got)
	}

	f.advance(10 * time.Second)
	f.listing(true, full)
	f.statePass(t)
	list := f.episodes(t)
	if len(list) != 2 || !list[0].Open() || !list[0].PushedAt.IsZero() || list[0].StreakKey != first.StreakKey {
		t.Fatalf("the held stop back: %+v, want an open, unpushed episode on streak %q", list, first.StreakKey)
	}
	f.pastGrace(t)
	f.advance(time.Hour)
	f.statePass(t)
	if got := f.sentEpisodes(); len(got) != 1 || got[0] != episodeTag(list[0].ID) {
		t.Fatalf("pushes: %v, want the held stop pushed once, as its new episode", got)
	}
}
