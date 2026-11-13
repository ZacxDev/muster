package api

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
)

// #635 audit round 2: the host-drop push of a held wait is gated on the pass
// never having found its window missing; a Claude pane the host could not
// match to its transcript is present; the waiting hook's re-show; the service
// worker's silent mapping.

// TestAHeldWaitSeenMissingIsNotPushedOnADrop (regression, audit round 2): a
// waiting stop held in its grace; its Claude is Ctrl-C'd (the shell stays) and
// a connected pass or two see the window missing; then the host drops for
// minutes, before the threshold could end the stop. Nothing is pushed — at 1,
// 2 or 3 missing passes. CONTROL: a wait still listed waiting when its host
// drops IS pushed (the drop rule, unchanged). RED before the fix at 1 and 2:
// the drop pushed "needs you" for a dead session.
func TestAHeldWaitSeenMissingIsNotPushedOnADrop(t *testing.T) {
	for _, misses := range []int{0, 1, 2, 3} {
		t.Run("missed "+strconv.Itoa(misses), func(t *testing.T) {
			f := newStateFixture(t, false)
			f.status(mirror.StatusWaiting, 5*time.Second)
			f.statePass(t)
			if e := f.oneEpisode(t); !e.Waiting || !e.PushedAt.IsZero() {
				t.Fatalf("a held wait: %+v", e)
			}
			ws := f.windowsNow()
			if misses > 0 {
				ws[0] = shellIn(ws[0], "zsh")
				f.listing(true, ws)
				for i := 0; i < misses; i++ {
					f.advance(5 * time.Second)
					f.statePass(t)
				}
			}
			f.listing(false, ws)
			for i := 0; i < 10; i++ {
				f.nextPass(t)
			}
			got := f.sentEpisodes()
			if misses == 0 {
				if len(got) != 1 {
					t.Fatalf("CONTROL: pushes %v, want the held wait pushed once on the drop", got)
				}
				return
			}
			if len(got) != 0 {
				t.Fatalf("pushes: %v — a wait seen missing on %d pass(es) was pushed on a host drop", got, misses)
			}
		})
	}
}

// TestAClaudePaneWithoutItsTranscriptIsPresent: a running Claude Code pane the
// host could not match to its transcript is listed as no runtime, with no
// key, carrying the host's no-view note (Window.AgentNote). Present, not
// missing: nothing ends — where a plain shell (no note) ends gone.
func TestAClaudePaneWithoutItsTranscriptIsPresent(t *testing.T) {
	f := newStateFixture(t, false)
	e := pushedStop(t, f)
	ws := f.windowsNow()
	ws[0] = shellIn(ws[0], "claude")
	ws[0].AgentNote = "Claude Code is running here, but its transcript was not found"
	f.listing(true, ws)
	for i := 0; i < 2*episodeGoneMisses; i++ {
		f.nextPass(t)
	}
	f.openStill(t, e.ID, "a Claude pane the host could not match to its transcript")
}

// TestTheWaitingHookReshowsAStopBackFromGone: a waiting stop is pushed, goes
// missing past the threshold (gone), and comes back still waiting on its
// streak — seen FIRST by the hub's change hook, which opens its episode. The
// hook shows the notification again, silently, once; the next pass (finding
// the episode open) shows nothing more and nothing alerts again.
func TestTheWaitingHookReshowsAStopBackFromGone(t *testing.T) {
	f := newStateFixture(t, false)
	f.waitWithSecrets()
	f.statePass(t)
	f.pastGrace(t)
	first := f.oneEpisode(t)
	if got := f.sentEpisodes(); len(got) != 1 || got[0] != episodeTag(first.ID) {
		t.Fatalf("the waiting stop's push: %v", got)
	}
	full := f.windowsNow()
	f.listing(true, shellOnly)
	for i := 0; i < episodeGoneMisses; i++ {
		f.nextPass(t)
	}
	f.listing(true, full)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	list := f.episodes(t)
	if len(list) != 2 || !list[0].Open() || list[0].ResumedFrom != first.ID {
		t.Fatalf("the hook's episode: %+v, want a second, open one resumed from %d", list, first.ID)
	}
	f.nextPass(t)
	if got := f.reshows(); len(got) != 1 || got[0] != "attention-episode-"+itoa(list[0].ID) {
		t.Fatalf("silent re-shows: %v, want one, under episode %d's tag", got, list[0].ID)
	}
	if got := f.sentEpisodes(); len(got) != 1 {
		t.Fatalf("alerting pushes: %v, want only the first", got)
	}
}

// TestTheServiceWorkerShowsASilentPayloadSilently pins the browser half of the
// quiet re-show: the worker the server serves maps the payload's silent flag
// onto the notification's options — `silent` from payload.silent, and no
// re-alert under the tag — in the options it shows every attention push with.
func TestTheServiceWorkerShowsASilentPayloadSilently(t *testing.T) {
	_, h := pwaServer(t, nil)
	r := get(h, "/sw.js", nil, false)
	js := r.Body.String()
	start := strings.Index(js, "const options = {")
	if start < 0 {
		t.Fatalf("GET /sw.js (%d): no options object", r.Code)
	}
	end := strings.Index(js[start:], "};")
	if end < 0 {
		t.Fatal("sw.js: the options object does not close")
	}
	opts := js[start : start+end]
	for _, want := range []string{"silent: payload.silent === true,", "renotify: false,"} {
		if !strings.Contains(opts, want) {
			t.Errorf("sw.js's notification options lack %q:\n%s", want, opts)
		}
	}
	if !strings.Contains(js[start+end:], "self.registration.showNotification(title, options)") {
		t.Error("sw.js no longer shows the push with those options")
	}
}
