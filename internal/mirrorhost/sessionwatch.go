package mirrorhost

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// A Claude Code session's status (busy, idle, waiting) is read from its
// sessions file, <config dir>/sessions/<pid>.json, which the CLI rewrites
// whenever the status moves. Without a watch, the mirror learned of a move
// only at its next Refresh (5 s by default): an operator who answered a
// waiting session in its own terminal left muster showing "waiting" — and its
// notification standing — for up to that long.
//
// sessionsWatch watches each sessions directory a live CLI writes under and
// nudges the refresh goroutine when a *.json in one is created, written,
// renamed or removed, so the move reaches muster within about
// sessionsWatchEvery. The Refresh tick stays as the backstop: a host where
// the watch cannot be set up (no inotify instances left, a platform fsnotify
// does not support, a directory that does not exist yet) loses nothing it had
// before.

// ClaudeConfigDirer is a Backend that names the Claude Code config dir it
// reads by default (Tmux: its ClaudeLocator's, $CLAUDE_CONFIG_DIR else
// ~/.claude). Its sessions directory is watched even before any pane names
// its own (ClaudePane.ConfigDir).
type ClaudeConfigDirer interface {
	ClaudeConfigDir() string
}

// ClaudeConfigDir implements ClaudeConfigDirer.
func (t Tmux) ClaudeConfigDir() string { return t.Claude.configDir() }

const (
	// sessionsWatchSettle is how long a change is left to settle before the
	// refresh it asks for: a CLI that writes its file in more than one step is
	// read once it is whole, not between the steps.
	sessionsWatchSettle = 50 * time.Millisecond
	// sessionsWatchEvery is the least time between two refreshes the watch
	// asks for. A busy CLI rewrites its file many times a second; however
	// many writes arrive, they cost at most one refresh per this interval.
	sessionsWatchEvery = 250 * time.Millisecond
)

// sessionsWatch is one connection's watch on the sessions directories.
// Its methods are safe for concurrent use; run is its one goroutine.
type sessionsWatch struct {
	onChange func()
	logf     func(format string, args ...any)
	// logged is the process's "already said that" set (Client.watchLogged),
	// so a reconnecting mirror does not repeat the same line per connection.
	logged *sync.Map
	settle time.Duration
	every  time.Duration
	// dirs carries the config dirs to watch now (want): one slot, the latest
	// set replacing one not yet taken.
	dirs chan []string
}

func newSessionsWatch(onChange func(), logf func(string, ...any), logged *sync.Map) *sessionsWatch {
	if logged == nil {
		logged = &sync.Map{}
	}
	return &sessionsWatch{onChange: onChange, logf: logf, logged: logged,
		settle: sessionsWatchSettle, every: sessionsWatchEvery, dirs: make(chan []string, 1)}
}

// want sets the Claude Code config dirs whose sessions directories are
// watched: every one not watched yet is added (and one that could not be
// added before — it did not exist — is tried again, which is how a
// recreated directory is watched again within one Refresh); one no longer
// wanted is dropped. It never blocks.
func (w *sessionsWatch) want(configDirs []string) {
	seen := map[string]bool{}
	var dirs []string
	for _, d := range configDirs {
		if d == "" || !filepath.IsAbs(d) {
			continue
		}
		s := filepath.Join(filepath.Clean(d), "sessions")
		if !seen[s] {
			seen[s] = true
			dirs = append(dirs, s)
		}
	}
	slices.Sort(dirs)
	for {
		select {
		case w.dirs <- dirs:
			return
		default:
		}
		select {
		case <-w.dirs: // a set not taken yet: this one replaces it
		default:
		}
	}
}

// once logs a line the first time key is seen by this process.
func (w *sessionsWatch) once(key, format string, args ...any) {
	if _, dup := w.logged.LoadOrStore(key, true); !dup {
		w.logf(format, args...)
	}
}

// start runs the watch until ctx ends; stop cancels it and returns once its
// goroutine (and fsnotify's) has exited.
func (w *sessionsWatch) start(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.run(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

// run is the watch's goroutine. When the watcher cannot be created it logs
// once and returns: the Refresh tick is all that is left, as before.
func (w *sessionsWatch) run(ctx context.Context) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		w.once("new", "muster-mirror: cannot watch Claude Code's sessions files (%v); a session's status reaches muster at the next refresh", err)
		return
	}
	defer fw.Close()
	watched := map[string]bool{}
	th := throttle{settle: w.settle, every: w.every}
	var (
		timer *time.Timer
		fire  <-chan time.Time
	)
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	schedule := func() {
		if d, arm := th.poke(time.Now()); arm {
			timer = time.NewTimer(d)
			fire = timer.C
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case dirs := <-w.dirs:
			for d := range watched {
				if !slices.Contains(dirs, d) {
					_ = fw.Remove(d)
					delete(watched, d)
				}
			}
			for _, d := range dirs {
				if watched[d] {
					continue
				}
				if err := fw.Add(d); err != nil {
					w.once("add "+d, "muster-mirror: not watching %s (%v); status changes there reach muster at the next refresh, and the watch is retried then", d, err)
					continue
				}
				watched[d] = true
				// Files may have changed before the watch began.
				schedule()
			}
		case ev, ok := <-fw.Events:
			if !ok {
				return
			}
			if watched[ev.Name] && ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				// The directory itself went: its watch is gone with it. The
				// next want re-adds it once it exists again.
				_ = fw.Remove(ev.Name)
				delete(watched, ev.Name)
				schedule()
				continue
			}
			if filepath.Ext(ev.Name) == ".json" && watched[filepath.Dir(ev.Name)] &&
				ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) != 0 {
				schedule()
			}
		case err, ok := <-fw.Errors:
			if !ok {
				return
			}
			// An overflowed queue lost events: one refresh reads everything.
			w.once("err "+err.Error(), "muster-mirror: watching Claude Code's sessions files: %v", err)
			schedule()
		case <-fire:
			fire, timer = nil, nil
			th.fired(time.Now())
			w.onChange()
		}
	}
}

// throttle spaces the refreshes the watch asks for. Its clock is the
// caller's, so a test can drive it without sleeping.
type throttle struct {
	settle, every time.Duration
	last          time.Time
	pending       bool
}

// poke is a change at now: arm says to start a timer for d. None is armed
// while one is pending — the pending refresh reads everything written since,
// and re-arming would postpone it for as long as the writes go on — and none
// fires sooner than settle from now or every after the last.
func (t *throttle) poke(now time.Time) (d time.Duration, arm bool) {
	if t.pending {
		return 0, false
	}
	t.pending = true
	d = t.settle
	if wait := t.every - now.Sub(t.last); wait > d {
		d = wait
	}
	return d, true
}

// fired records the refresh at now.
func (t *throttle) fired(now time.Time) {
	t.pending = false
	t.last = now
}

// claudeConfigDirs is every config dir the listing's Claude panes write
// under, and the backend's default.
func claudeConfigDirs(def string, raw []WindowInfo) []string {
	var out []string
	if def != "" {
		out = append(out, def)
	}
	for _, w := range raw {
		if w.Claude != nil && w.Claude.ConfigDir != "" {
			out = append(out, w.Claude.ConfigDir)
		}
	}
	return out
}

// watchesSessions is whether this connection watches the sessions
// directories: its backend lists Claude Code panes (per window or per
// session), or names a config dir of its own.
func (h *hostSide) watchesSessions() bool {
	if _, ok := WindowListerOf(h.c.cfg.Backend); ok {
		return true
	}
	if _, ok := ClaudeOf(h.c.cfg.Backend); ok {
		return true
	}
	_, ok := capabilityOf[ClaudeConfigDirer](h.c.cfg.Backend)
	return ok
}

// defaultClaudeConfigDir is the backend's own config dir
// (ClaudeConfigDirer), "" when it names none.
func (h *hostSide) defaultClaudeConfigDir() string {
	if d, ok := capabilityOf[ClaudeConfigDirer](h.c.cfg.Backend); ok {
		return d.ClaudeConfigDir()
	}
	return ""
}
