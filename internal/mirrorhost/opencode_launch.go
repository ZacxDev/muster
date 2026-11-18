package mirrorhost

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/opencodehost"
	"github.com/ZacxDev/muster/internal/procstat"
)

// LAUNCHER-BOUND OPENCODE PANES (docs/design/opencode-first-class.md): a pane
// whose TUI `muster-mirror opencode` started has a record naming its session
// (opencodehost.LaunchRecord). This host binds the pane to that session —
// SourceLauncher, AgentPane.Send — when the record names this daemon's tmux
// server and the pane, the TUI the walk found (its pid AND its start time),
// this host's opencode server, and a root session that server has. Only a
// bound pane is SENT to; any other opencode pane keeps the title match and is
// view-only.
//
// 🔴 THE PANE TITLE NEVER DECIDES A BOUND PANE'S SESSION (operator decision).
// A title that contradicts the record — the TUI switched to another session —
// is a WARNING on the window (its note), and the send still goes to the
// recorded session, which is the one the rendered view shows: what the person
// reads in muster and where the message lands cannot diverge.

// SourceLauncher: an opencode pane bound by its launcher record.
const SourceLauncher = "launcher"

// TitleMismatchWarning is the bound window's note while its pane's title
// shows another session than the recorded one (title, as the pane shows it).
func TitleMismatchWarning(title string) string {
	return "This pane's title shows a different opencode session (" + title +
		") than the one muster sends to; messages still go to the session shown here."
}

// fallbackTitle is upstream's fallback session title
// (packages/util/src/session-title-fallback.ts at 6ec5682).
var fallbackTitle = regexp.MustCompile(`^(New session|Child session) - \d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)

// SetLaunches has s bind panes by the launcher records under dir for the tmux
// server at socket (this daemon's). Without it, no pane is bound.
func (s *OpenCodeSource) SetLaunches(dir, socket string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.launchDir, s.launchSocket = dir, socket
}

// launches is the records' directory and socket, "" when unset.
func (s *OpenCodeSource) launches() (dir, socket string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.launchDir, s.launchSocket
}

// bindLaunch binds ap (its TUI's process started at start) by its pane's
// launcher record, against sessions (the server's root sessions): true when it
// did. A record whose process is not the TUI's is stale and is removed.
func (s *OpenCodeSource) bindLaunch(ap *AgentPane, start, paneTitle string, titleOK bool, sessions []opencodehost.Session) bool {
	dir, socket := s.launches()
	if dir == "" || socket == "" || start == "" {
		return false
	}
	rec, err := opencodehost.ReadLaunch(dir, socket, ap.Pane)
	if err != nil {
		if !errors.Is(err, opencodehost.ErrNoLaunch) {
			s.logger.Printf("muster-mirror: opencode: the launcher record of pane %s cannot be read: %v", ap.Pane, err)
		}
		return false
	}
	if rec.PID != ap.PID || rec.PIDStart != start {
		// Another TUI runs in the pane now (or it was restarted without the
		// launcher): the record is stale.
		_ = opencodehost.RemoveLaunch(dir, rec)
		return false
	}
	if s.creds == nil || rec.Server != s.creds.URL() {
		return false
	}
	var sess *opencodehost.Session
	for i := range sessions {
		if sessions[i].ID == rec.Session && sessions[i].ParentID == "" {
			sess = &sessions[i]
		}
	}
	if sess == nil {
		return false
	}
	ap.Gen, ap.Source, ap.Send = rec.Session, SourceLauncher, true
	if titleOK && !titleConsistent(paneTitle, *sess) {
		ap.OpenCode.Warning = TitleMismatchWarning(strings.TrimPrefix(paneTitle, opencodehost.TitlePrefix))
	}
	return true
}

// titleConsistent reports whether a pane titled t can be showing session s:
// its "OC | <title>" (a cut title a prefix), the home screen's "OpenCode"
// while s has no real title, or no title at all (the TUI's titles turned off).
func titleConsistent(t string, s opencodehost.Session) bool {
	switch {
	case t == "" || t == "OpenCode" && (s.Title == "" || fallbackTitle.MatchString(s.Title)):
		return true
	case !strings.HasPrefix(t, opencodehost.TitlePrefix):
		return false
	}
	return opencodehost.MatchTitle(t, []opencodehost.Session{s}).Session == s.ID
}

// launchSweepEvery bounds how often stale records are swept.
const launchSweepEvery = time.Minute

// sweepLaunches removes, at most every launchSweepEvery, each of this
// daemon's records whose process is gone or is another process now (its
// start time differs): the TUI exited, and its pane holds a shell or nothing.
func (s *OpenCodeSource) sweepLaunches(proc string) {
	dir, socket := s.launches()
	if dir == "" || socket == "" {
		return
	}
	s.mu.Lock()
	if s.now().Sub(s.sweptAt) < launchSweepEvery {
		s.mu.Unlock()
		return
	}
	s.sweptAt = s.now()
	s.mu.Unlock()
	p, err := opencodehost.LaunchPath(dir, socket, "%0")
	if err != nil {
		return
	}
	ents, err := os.ReadDir(filepath.Dir(p))
	if err != nil {
		return
	}
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		rec, err := opencodehost.ReadLaunch(dir, socket, "%"+name)
		if err != nil {
			continue
		}
		if st, ok := procStart(proc, rec.PID); !ok || st != rec.PIDStart {
			_ = opencodehost.RemoveLaunch(dir, rec)
		}
	}
}

// procStart is /proc/<pid>/stat's start time under proc (procstat.Parse,
// the one parser of it), false when the process is gone.
func procStart(proc string, pid int) (string, bool) {
	b, err := os.ReadFile(filepath.Join(proc, strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", false
	}
	st, ok := procstat.Parse(b)
	return st.Start, ok
}
