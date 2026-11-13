package api

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sessionstate"
)

// #635 audit rounds 2 and 3: the host-drop push of a held wait is suppressed
// only on POSITIVE evidence the session is dead (its window last listed
// holding no agent of it), never on an omission; a Claude pane the host could
// not match to an existing transcript is present, a NEW transcript-less one
// is not; the waiting hook's re-show; the service worker's silent mapping.

// noteFor is the fixture's window as a Claude Code pane with no rendered view
// for reason code (mirror.AgentNote*; "" for an older host's words alone).
func noteFor(w mirror.Window, code string) mirror.Window {
	w = shellIn(w, "claude")
	w.AgentNote, w.AgentNoteCode = "Claude Code is running here, but it has no rendered view.", code
	return w
}

// TestAHeldWaitIsPushedOnADropUnlessListedDead (regression, audit rounds 2
// and 3): a waiting stop held in its grace; then one, two or three connected
// passes; then its host drops for minutes, before the threshold could end the
// stop. PUSHED when the window was still listed waiting (control) or merely
// LEFT OUT of the last listing (an omission is no evidence); NOT pushed when
// the last listing showed the window holding no agent of the conversation —
// the shell left after a Ctrl-C (1, 2 or 3 passes), or a new transcript-less
// Claude started in it. RED before the round-3 fix: "omitted once" was
// silenced by the count gate. RED before the round-2 fix: "shell" at 1 and 2
// were pushed.
func TestAHeldWaitIsPushedOnADropUnlessListedDead(t *testing.T) {
	for _, c := range []struct {
		name   string
		passes int
		list   func(full []mirror.Window) []mirror.Window
		pushed bool
	}{
		{"still waiting (control)", 1, func(full []mirror.Window) []mirror.Window { return full }, true},
		{"omitted once", 1, func([]mirror.Window) []mirror.Window { return shellOnly }, true},
		{"listed as a shell once", 1, func(full []mirror.Window) []mirror.Window {
			return []mirror.Window{shellIn(full[0], "zsh")}
		}, false},
		{"listed as a shell twice", 2, func(full []mirror.Window) []mirror.Window {
			return []mirror.Window{shellIn(full[0], "zsh")}
		}, false},
		{"listed as a shell three times", 3, func(full []mirror.Window) []mirror.Window {
			return []mirror.Window{shellIn(full[0], "zsh")}
		}, false},
		{"a new transcript-less Claude once", 1, func(full []mirror.Window) []mirror.Window {
			return []mirror.Window{noteFor(full[0], mirror.AgentNoteNotYet)}
		}, false},
		// Unknown is not evidence (audit round 4): a host that cannot read its
		// process table, and an older host's note with no code, say nothing
		// of which conversation the pane holds — the drop push stands.
		{"a no_procs note once", 1, func(full []mirror.Window) []mirror.Window {
			return []mirror.Window{noteFor(full[0], mirror.AgentNoteNoProcs)}
		}, true},
		{"a code-less note once", 1, func(full []mirror.Window) []mirror.Window {
			return []mirror.Window{noteFor(full[0], "")}
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newStateFixture(t, false)
			f.status(mirror.StatusWaiting, 5*time.Second)
			f.statePass(t)
			if e := f.oneEpisode(t); !e.Waiting || !e.PushedAt.IsZero() {
				t.Fatalf("a held wait: %+v", e)
			}
			ws := c.list(f.windowsNow())
			f.listing(true, ws)
			for i := 0; i < c.passes; i++ {
				f.advance(5 * time.Second)
				f.statePass(t)
			}
			f.listing(false, ws)
			for i := 0; i < 10; i++ {
				f.nextPass(t)
			}
			got := f.sentEpisodes()
			if c.pushed && len(got) != 1 {
				t.Fatalf("pushes %v, want the held wait pushed once on the drop", got)
			}
			if !c.pushed && len(got) != 0 {
				t.Fatalf("pushes: %v — a wait last listed dead was pushed on a host drop", got)
			}
		})
	}
}

// TestAClaudePaneWithoutItsTranscriptIsPresentOnlyIfItCanBeTheSame: a Claude
// Code pane listed with no key and its no-view reason code. A pane the host
// could not MATCH to an existing transcript (no_transcript, no_env) is
// present — nothing ends. A NEW conversation (not_yet: Ctrl-C, then a fresh
// `claude`), a host that cannot read processes (no_procs), and an older
// host's note with no code (unknown) are not this conversation: it ends gone
// at the threshold.
func TestAClaudePaneWithoutItsTranscriptIsPresentOnlyIfItCanBeTheSame(t *testing.T) {
	for _, c := range []struct {
		code    string
		present bool
	}{
		{mirror.AgentNoteNoTranscript, true},
		{mirror.AgentNoteNoEnv, true},
		{mirror.AgentNoteNotYet, false},
		{mirror.AgentNoteNoProcs, false},
		{"", false},
	} {
		t.Run("code "+strconv.Quote(c.code), func(t *testing.T) {
			f := newStateFixture(t, false)
			e := pushedStop(t, f)
			ws := f.windowsNow()
			ws[0] = noteFor(ws[0], c.code)
			f.listing(true, ws)
			if c.present {
				for i := 0; i < 2*episodeGoneMisses; i++ {
					f.nextPass(t)
				}
				f.openStill(t, e.ID, "a Claude pane the host could not match to its transcript")
				return
			}
			for i := 0; i < episodeGoneMisses; i++ {
				f.nextPass(t)
			}
			if got := f.oneOf(t, e.ID); got.Open() || got.EndReason != sessionstate.EndGone {
				t.Fatalf("a pane that is not this conversation: %+v, want ended gone", got)
			}
		})
	}
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
