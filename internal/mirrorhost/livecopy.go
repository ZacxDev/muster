package mirrorhost

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// A LIVE COPY OF A MIRRORED CONVERSATION (mirror.Window.LiveCopy): another
// transcript of the same conversation, in the same project directory, that is
// still running somewhere else. muster warns on the card and the views,
// because a message sent to this window never reaches that one.
//
// HOW A COPY HAPPENS. `claude --resume <id>` of a conversation that is still
// open in another client (the VS Code extension, another terminal) does not
// attach to it: it starts a NEW session whose transcript begins with a copy of
// the old one. Measured read-only on real transcripts on one host (Claude Code
// 2.1.x), both pairs of copies there:
//
//   - every copied row keeps its `uuid` and its `timestamp` to the
//     millisecond; only `sessionId` is rewritten to the new session's;
//   - NO row names the session it was copied from: there is no parent-session
//     field, so which of the two is the copy is not written anywhere;
//   - a /bg job is a copy too, and says so: its rows carry
//     "sessionKind":"bg". It is a copy the person asked for, and its first
//     transcript stops when the job takes over, so it is not warned about;
//   - the untimestamped bookkeeping rows at the head (mode, permission-mode,
//     last-prompt, ai-title, …) differ between the two.
//
// So a transcript's ORIGIN is its first row that has both a uuid and a
// timestamp, and two transcripts with the same origin are one conversation.
// Rows are decoded by name (encoding/json), never by position: the schema
// varies across Claude Code versions.
//
// "STILL RUNNING ELSEWHERE" is decided by, in order:
//
//  1. Claude Code's own sessions/<pid>.json, which a running CLI writes for its
//     peers (ClaudeLocator): a LIVE process (pid and procStart both match) other
//     than this window's own CLI that names the other session → running. A
//     sessions file that names it from this window's own CLI, or from a process
//     that has exited, says it is NOT running elsewhere: the CLI does not
//     rewrite that file when its conversation changes id, so the window's own
//     CLI can name the very session it moved away from.
//  2. Nothing names it (a client that writes no sessions file): its last
//     MESSAGE (lastmsg.go's rule) is within LiveCopyWindow. Not its mtime:
//     Claude Code appends untimestamped bookkeeping rows (cost-state) to
//     transcripts it is not running — measured, 37 transcripts on one host had
//     their mtime moved within one minute with no message written.
//
// 🔴 CHEAP. A window's own transcript and each sibling's head are read ONCE
// (up to liveCopyHeadMax bytes, stopping at the origin row) and cached by path
// for as long as the file is listed: the origin is the first rows of an
// append-only file, so it never changes. A project directory is listed at most
// every liveCopyDirTTL, and its names indexed by origin once per listing.
// Sessions files are read, and a sibling's tail, only when a sibling with the
// same origin exists — on the host measured, two pairs among 705 transcripts,
// one of them a /bg job. Measured there with EVERY one of the 705 transcripts
// treated as a mirrored window: 10 ms for the first listing (every head read),
// under 1 ms for each one after.

// LiveCopyWindow is how recently a copy no sessions file speaks for must have
// had a message to count as running elsewhere. Long enough to span a person
// reading a reply before answering; short enough that a copy that was closed
// stops being warned about within minutes.
const LiveCopyWindow = 5 * time.Minute

const (
	// liveCopyHeadMax is how much of a transcript's head is read for its
	// origin. The origin row is among the first few rows; a first prompt
	// longer than this leaves the transcript with no origin (no warning).
	liveCopyHeadMax = 256 << 10
	// liveCopyDirTTL is how long a project directory's listing is reused. A
	// new copy is noticed within it.
	liveCopyDirTTL = 15 * time.Second
)

// transcriptOrigin is a transcript's first row with both a uuid and a
// timestamp.
type transcriptOrigin struct {
	UUID      string
	Timestamp string
	// Background: that row carries sessionKind "bg" (a /bg job's copy).
	Background bool
}

// originRow is the fields of a transcript row readTranscriptOrigin reads.
type originRow struct {
	UUID        string `json:"uuid"`
	Timestamp   string `json:"timestamp"`
	SessionKind string `json:"sessionKind"`
}

// readTranscriptOrigin is the origin of the transcript r, read from its first
// liveCopyHeadMax bytes; ok=false when none is there. A row that is not a
// JSON object is skipped, as is one longer than the head.
func readTranscriptOrigin(r io.Reader) (transcriptOrigin, bool) {
	br := bufio.NewReader(io.LimitReader(r, liveCopyHeadMax))
	for {
		line, err := br.ReadBytes('\n')
		// Only a whole line: one cut by the limit (or still being written)
		// is not a row yet.
		if len(line) > 0 && line[len(line)-1] == '\n' {
			var row originRow
			if json.Unmarshal(line, &row) == nil && row.UUID != "" && row.Timestamp != "" {
				return transcriptOrigin{UUID: row.UUID, Timestamp: row.Timestamp, Background: row.SessionKind == "bg"}, true
			}
		}
		if err != nil {
			return transcriptOrigin{}, false
		}
	}
}

// LiveCopies finds each Claude window's live copy (ClaudePane.LiveCopy). Set
// on Tmux it runs on every listing; its caches make that cheap. Safe for
// concurrent use; the zero value is ready.
type LiveCopies struct {
	mu sync.Mutex
	// heads is each transcript's head by path (origin).
	heads map[string]liveCopyHead
	// dirs is each project directory's transcript names, and when listed.
	dirs map[string]liveCopyDir
	// lastMsg reads a sibling's last message (lastmsg.go's rule).
	lastMsg lastMessageReader
	// reads counts the heads actually read (tests: the cache).
	reads int
}

// liveCopyHead is a transcript's origin, ok=false when its head had none.
type liveCopyHead struct {
	origin transcriptOrigin
	ok     bool
}

type liveCopyDir struct {
	at    time.Time
	names []string
	// byOrigin is names by their origin (originKey), built on first use
	// after each listing: what lets a window find its copies without walking
	// the whole directory every listing. nil until built.
	byOrigin map[originKey][]string
}

// originKey is what two transcripts of one conversation share: their origin
// row's uuid AND timestamp.
type originKey struct{ uuid, timestamp string }

func (o transcriptOrigin) key() originKey { return originKey{o.UUID, o.Timestamp} }

// liveCopyHolder is a sessions file naming a session: its process, and
// whether that process is the one that wrote it and still running.
type liveCopyHolder struct {
	pid   int
	alive bool
}

// attach sets LiveCopy on each Claude Code window of windows (their
// ClaudePanes are this listing's own), reading processes from procs and, for
// a pane whose config dir the locator did not record, loc's.
func (l *LiveCopies) attach(windows []WindowInfo, procs map[int]procInfo, loc ClaudeLocator, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	holders := map[string]map[string][]liveCopyHolder{}
	holdersIn := func(cfg string) map[string][]liveCopyHolder {
		if h, ok := holders[cfg]; ok {
			return h
		}
		h := readLiveCopyHolders(cfg, procs)
		holders[cfg] = h
		return h
	}
	seenDirs := map[string]bool{}
	tails := map[string]bool{}
	for i := range windows {
		c := windows[i].Claude
		if c == nil || c.Runtime != "" || c.Transcript == "" {
			continue
		}
		seenDirs[filepath.Dir(c.Transcript)] = true
		cfg := c.ConfigDir
		if cfg == "" {
			cfg = loc.configDir()
		}
		c.LiveCopy = l.find(c, func() map[string][]liveCopyHolder { return holdersIn(cfg) }, tails, now)
	}
	for d := range l.dirs {
		if !seenDirs[d] {
			l.forgetDir(d)
		}
	}
	l.lastMsg.mu.Lock()
	for p := range l.lastMsg.cache {
		if !tails[p] {
			delete(l.lastMsg.cache, p)
		}
	}
	l.lastMsg.mu.Unlock()
}

// find is the session id of a transcript in c's directory with c's origin
// that is running elsewhere — the one with the newest evidence of it — or "".
func (l *LiveCopies) find(c *ClaudePane, holders func() map[string][]liveCopyHolder, tails map[string]bool, now time.Time) string {
	own, ok := l.origin(c.Transcript)
	if !ok || own.Background {
		return ""
	}
	dir, self := filepath.Dir(c.Transcript), filepath.Base(c.Transcript)
	var best string
	var bestAt time.Time
	for _, name := range l.sameOrigin(dir, own, now) {
		id := strings.TrimSuffix(name, ".jsonl")
		if name == self || id == c.Gen || !mirror.ValidClaudeSession(id) {
			continue
		}
		p := filepath.Join(dir, name)
		o, ok := l.origin(p)
		if !ok || o.Background || o.key() != own.key() {
			continue
		}
		at, running := l.running(id, p, c.PID, holders, tails, now)
		if running && (best == "" || at.After(bestAt) || (at.Equal(bestAt) && id < best)) {
			best, bestAt = id, at
		}
	}
	return best
}

// running reports whether session id (transcript p) is running somewhere
// other than the CLI ownPID, and as of when (see the file's comment).
func (l *LiveCopies) running(id, p string, ownPID int, holders func() map[string][]liveCopyHolder, tails map[string]bool, now time.Time) (time.Time, bool) {
	named := false
	for _, h := range holders()[id] {
		if h.alive && h.pid != ownPID {
			return now, true
		}
		named = true
	}
	if named {
		return time.Time{}, false
	}
	st, err := os.Stat(p)
	if err != nil || now.Sub(st.ModTime()) > LiveCopyWindow {
		return time.Time{}, false
	}
	tails[p] = true
	at := l.lastMsg.of(p)
	if at <= 0 {
		return time.Time{}, false
	}
	t := time.Unix(at, 0)
	return t, now.Sub(t) <= LiveCopyWindow
}

// sameOrigin is the names of dir's transcripts whose origin is o's.
func (l *LiveCopies) sameOrigin(dir string, o transcriptOrigin, now time.Time) []string {
	names := l.list(dir, now)
	d := l.dirs[dir]
	if d.byOrigin == nil {
		d.byOrigin = map[originKey][]string{}
		for _, name := range names {
			if h, ok := l.origin(filepath.Join(dir, name)); ok {
				d.byOrigin[h.key()] = append(d.byOrigin[h.key()], name)
			}
		}
		l.dirs[dir] = d
	}
	return d.byOrigin[o.key()]
}

// origin is the transcript at path's origin, from the cache or its head. A
// head with no origin yet is remembered as such until its directory is
// listed again (list), so a short transcript is not re-read every listing.
func (l *LiveCopies) origin(path string) (transcriptOrigin, bool) {
	if h, ok := l.heads[path]; ok {
		return h.origin, h.ok
	}
	var h liveCopyHead
	if f, err := os.Open(path); err == nil {
		l.reads++
		h.origin, h.ok = readTranscriptOrigin(f)
		f.Close()
	}
	if l.heads == nil {
		l.heads = map[string]liveCopyHead{}
	}
	l.heads[path] = h
	return h.origin, h.ok
}

// list is dir's *.jsonl names, listed again once liveCopyDirTTL has passed.
// A relisting drops the cached heads of the files that are gone, and of those
// whose origin was not found (they may have one now); a found origin is kept.
func (l *LiveCopies) list(dir string, now time.Time) []string {
	if d, ok := l.dirs[dir]; ok && now.Sub(d.at) < liveCopyDirTTL && !now.Before(d.at) {
		return d.names
	}
	entries, err := os.ReadDir(dir)
	var names []string
	present := map[string]bool{}
	if err == nil {
		for _, e := range entries {
			if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".jsonl") {
				names = append(names, e.Name())
				present[e.Name()] = true
			}
		}
	}
	for p, h := range l.heads {
		if filepath.Dir(p) == dir && (!h.ok || !present[filepath.Base(p)]) {
			delete(l.heads, p)
		}
	}
	if l.dirs == nil {
		l.dirs = map[string]liveCopyDir{}
	}
	l.dirs[dir] = liveCopyDir{at: now, names: names}
	return names
}

// forgetDir drops dir's listing and its files' cached heads.
func (l *LiveCopies) forgetDir(dir string) {
	delete(l.dirs, dir)
	for p := range l.heads {
		if filepath.Dir(p) == dir {
			delete(l.heads, p)
		}
	}
}

// readLiveCopyHolders is, per session id, every sessions file under cfg that
// names it: its pid, and whether that process is alive — in procs, with the
// procStart the file recorded when it recorded one (a sessions file outlives
// its process, and a pid can be reused).
func readLiveCopyHolders(cfg string, procs map[int]procInfo) map[string][]liveCopyHolder {
	out := map[string][]liveCopyHolder{}
	m, _ := filepath.Glob(filepath.Join(cfg, "sessions", "*.json"))
	for _, p := range m {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var sf sessionFile
		if json.Unmarshal(b, &sf) != nil || !sessionIDPattern.MatchString(sf.SessionID) || sf.PID <= 0 {
			continue
		}
		pi, ok := procs[sf.PID]
		alive := ok && (sf.ProcStart == "" || sf.ProcStart == pi.start)
		out[sf.SessionID] = append(out[sf.SessionID], liveCopyHolder{pid: sf.PID, alive: alive})
	}
	return out
}
