package mirrorhost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/ZacxDev/muster/internal/mirror"
)

// RESUMING AN ENDED CLAUDE CODE SESSION (spawn-resume): the host half. See
// internal/mirror's resume.go for the wire and why it exists.
//
// 🔴 OFF BY DEFAULT, AND REFUSED UNLESS THIS HOST OPTED IN: -interactive AND
// -resume-spawn (Config.ResumeSpawn), whatever muster asks.
//
// 🔴 WHAT THIS HOST STARTS IS DECIDED HERE, NEVER BY THE REQUEST. The request
// names a session id and nothing else. The argv is this host's configured
// Claude binary, `--resume`, and that id: no shell, no text from any message.
// The working directory is the one the TRANSCRIPT records, read from this
// host's own ~/.claude — never a path off the wire.
//
// The checks, in order, each a refusal muster can tell apart (Msg.Refusal):
//
//  1. the id is a plain Claude session id (letters, digits, hyphens; never a
//     leading hyphen, so it cannot read as a flag);
//  2. this host holds EXACTLY ONE transcript <home>/projects/*/<id>.jsonl, a
//     regular file — none is ResumeRefusalNotHere, the one refusal after which
//     muster may ask another host;
//  3. the transcript records a cwd (an absolute, clean path) whose project slug
//     IS the directory the transcript is filed under — `claude --resume` finds
//     a session only from that directory — and the directory still exists;
//  4. the session is not live here: no running Claude Code process names it
//     (its sessions/<pid>.json, checked against the process table), and no
//     tmux pane this host lists runs it;
//  5. this host's own limits: one resume per session per resumeSessionGap, and
//     at most resumeHostPerHour an hour. muster has its own, durable ones; these
//     hold even against a muster that forgot them.
//
// Then ONE tmux command starts the window, detached, in this host's resume
// session (Config.ResumeSession, created when absent), and the host lists its
// windows at once (nudge) so muster sees it.

const (
	// DefaultResumeSession is the tmux session resumed windows are started in.
	DefaultResumeSession = "muster-resume"
	// DefaultResumeClaude is the Claude Code binary a resume runs.
	DefaultResumeClaude = "claude"
	// resumeSessionGap is how soon one session may be resumed again here.
	resumeSessionGap = 10 * time.Minute
	// resumeHostPerHour bounds the resumes this host starts in an hour.
	resumeHostPerHour = 6
	// resumeHeadBytes bounds how much of a transcript is read for its cwd.
	resumeHeadBytes = 4 << 20
)

// ResumeBackend is a Backend that can start a new window running a command.
// The tmux backend implements it.
type ResumeBackend interface {
	// NewResumeWindow starts a DETACHED window named name in session (the
	// session created when absent), with working directory dir, running argv
	// directly (no shell), and answers its tmux window id.
	NewResumeWindow(ctx context.Context, session, name, dir string, argv []string) (string, error)
}

// ResumeOf is b's ResumeBackend, looking through wrappers like ClaudeOf.
func ResumeOf(b Backend) (ResumeBackend, bool) { return capabilityOf[ResumeBackend](b) }

// resumeIDPattern is the session ids this host resumes: a Claude session id
// (mirror.ValidClaudeSession) that is also safe in a glob and on an argv —
// letters, digits and hyphens, never a leading hyphen.
var resumeIDPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z-]{0,63}$`)

// errResume is a refusal of a kind (mirror.ResumeRefusal*).
type errResume struct{ kind, why string }

func (e *errResume) Error() string { return e.why }

func refuseResume(kind, format string, a ...any) error {
	return &errResume{kind: kind, why: fmt.Sprintf(format, a...)}
}

// resumeLimits is this host's own record of what it resumed: it outlives a
// connection (Client.resumes), not the process.
type resumeLimits struct {
	mu      sync.Mutex
	last    map[string]time.Time
	started []time.Time
}

// take records a resume of sid at now, or refuses it.
func (l *resumeLimits) take(sid string, now time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last = map[string]time.Time{}
	}
	if t, ok := l.last[sid]; ok && now.Sub(t) < resumeSessionGap {
		return refuseResume(mirror.ResumeRefusalLimited, "session %s was resumed on this host %s ago", sid, now.Sub(t).Round(time.Second))
	}
	kept := l.started[:0]
	for _, t := range l.started {
		if now.Sub(t) < time.Hour {
			kept = append(kept, t)
		}
	}
	l.started = kept
	if len(l.started) >= resumeHostPerHour {
		return refuseResume(mirror.ResumeRefusalLimited, "this host already resumed %d sessions in the last hour", len(l.started))
	}
	l.last[sid] = now
	l.started = append(l.started, now)
	return nil
}

// resumeTarget is a session this host may resume: its transcript and cwd.
type resumeTarget struct {
	transcript string
	cwd        string
}

// findResumeTarget runs checks 1–3: the one transcript of sid under home and
// the cwd it records.
func findResumeTarget(home, sid string) (resumeTarget, error) {
	if !resumeIDPattern.MatchString(sid) || !mirror.ValidClaudeSession(sid) {
		return resumeTarget{}, refuseResume(mirror.ResumeRefusalInvalid, "%q is not a Claude session id this host resumes", sid)
	}
	matches, err := filepath.Glob(filepath.Join(home, "projects", "*", sid+".jsonl"))
	if err != nil {
		return resumeTarget{}, refuseResume(mirror.ResumeRefusalNotHere, "the transcripts cannot be searched: %v", err)
	}
	var files []string
	for _, m := range matches {
		if fi, err := os.Lstat(m); err == nil && fi.Mode().IsRegular() {
			files = append(files, m)
		}
	}
	switch len(files) {
	case 0:
		return resumeTarget{}, refuseResume(mirror.ResumeRefusalNotHere, "this host holds no transcript of session %s", sid)
	case 1:
	default:
		return resumeTarget{}, refuseResume(mirror.ResumeRefusalNoCwd, "this host holds %d transcripts of session %s, so which project it belongs to is ambiguous", len(files), sid)
	}
	slug := filepath.Base(filepath.Dir(files[0]))
	cwd, err := transcriptCwd(files[0], slug)
	if err != nil {
		return resumeTarget{}, err
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		return resumeTarget{}, refuseResume(mirror.ResumeRefusalNoCwd, "session %s's working directory %s no longer exists", sid, cwd)
	}
	return resumeTarget{transcript: files[0], cwd: cwd}, nil
}

// transcriptCwd is the first cwd a transcript's rows record whose project slug
// is slug (the directory the transcript is filed under), within its first
// resumeHeadBytes. Rows are untrusted JSON; only a well-formed absolute,
// clean path with no control characters is taken.
func transcriptCwd(path, slug string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", refuseResume(mirror.ResumeRefusalNoCwd, "the transcript cannot be read: %v", err)
	}
	defer f.Close()
	r := bufio.NewReaderSize(io.LimitReader(f, resumeHeadBytes), 64<<10)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var row struct {
				Cwd string `json:"cwd"`
			}
			if json.Unmarshal(line, &row) == nil && row.Cwd != "" && cleanAbsPath(row.Cwd) && ProjectSlug(row.Cwd) == slug {
				return row.Cwd, nil
			}
		}
		if err != nil {
			break
		}
	}
	return "", refuseResume(mirror.ResumeRefusalNoCwd, "the transcript records no working directory that matches its project")
}

// cleanAbsPath: absolute, already clean, no control characters.
func cleanAbsPath(p string) bool {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return false
	}
	return !strings.ContainsFunc(p, unicode.IsControl)
}

// resumeLive reports whether sid runs on this host now (check 4): a live
// Claude Code process whose sessions file names it, or a pane this host lists.
func resumeLive(ctx context.Context, loc ClaudeLocator, b Backend, sid string) (bool, error) {
	procs, _ := loc.table()
	if _, ok := loc.liveSessions(procs).byID[sid]; ok {
		return true, nil
	}
	wl, ok := WindowListerOf(b)
	if !ok {
		return false, nil
	}
	ws, err := wl.Windows(ctx)
	if err != nil {
		// Unknown is not "not live": refuse rather than risk a second CLI on
		// one conversation.
		return false, err
	}
	for _, w := range ws {
		if w.Claude != nil && w.Claude.Gen == sid {
			return true, nil
		}
		for _, o := range w.Others {
			if o.Gen == sid {
				return true, nil
			}
		}
	}
	return false, nil
}

// resumeSpawn is what this host advertises as Msg.ResumeSpawn: an
// -interactive v2 host started with -resume-spawn whose backend can start a
// window.
func (h *hostSide) resumeSpawn() bool {
	_, ok := ResumeOf(h.c.cfg.Backend)
	return ok && h.version >= 2 && h.c.cfg.Interactive && h.c.cfg.ResumeSpawn
}

// spawnResume serves a spawn-resume and answers resume-spawned.
func (h *hostSide) spawnResume(m mirror.Msg) {
	reply := func(w mirror.ResumedWindow, err error) {
		ans := mirror.Msg{Type: mirror.TypeResumeSpawned, ID: m.ID, Session: w.Session, Window: w.Window}
		if err != nil {
			ans.Session, ans.Window = "", ""
			ans.Error = err.Error()
			var e *errResume
			if errors.As(err, &e) {
				ans.Refusal = e.kind
			}
			h.c.log.Printf("muster-mirror: resume of session %q refused: %v", m.Gen, err)
		}
		_ = h.send(ans)
	}
	w, err := h.c.resume(h.ctx, m.Gen, h.resumeSpawn())
	reply(w, err)
	if err == nil {
		h.nudge()
	}
}

// resume runs the checks and starts the window. offered is whether this
// connection advertised the capability.
func (c *Client) resume(ctx context.Context, sid string, offered bool) (mirror.ResumedWindow, error) {
	rb, ok := ResumeOf(c.cfg.Backend)
	if !offered || !ok {
		return mirror.ResumedWindow{}, refuseResume(mirror.ResumeRefusalUnsupported, "this host does not resume sessions (muster-mirror needs -interactive and -resume-spawn)")
	}
	loc := c.cfg.ResumeLocator
	tgt, err := findResumeTarget(loc.configDir(), sid)
	if err != nil {
		return mirror.ResumedWindow{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	live, err := resumeLive(ctx, loc, c.cfg.Backend, sid)
	switch {
	case err != nil:
		return mirror.ResumedWindow{}, refuseResume(mirror.ResumeRefusalLive, "whether session %s is running here cannot be read: %v", sid, err)
	case live:
		return mirror.ResumedWindow{}, refuseResume(mirror.ResumeRefusalLive, "session %s is running on this host already", sid)
	}
	session := c.cfg.ResumeSession
	if session == "" {
		session = DefaultResumeSession
	}
	bin := c.cfg.ResumeClaude
	if bin == "" {
		bin = DefaultResumeClaude
	}
	if err := c.resumes.take(sid, time.Now()); err != nil {
		return mirror.ResumedWindow{}, err
	}
	name := "resume-" + sid[:min(8, len(sid))]
	id, err := rb.NewResumeWindow(ctx, session, name, tgt.cwd, []string{bin, "--resume", sid})
	if err != nil {
		return mirror.ResumedWindow{}, fmt.Errorf("the window could not be started: %w", err)
	}
	c.log.Printf("muster-mirror: RESUMED Claude session %s in tmux %s:%s (cwd %s), as muster asked", sid, session, id, tgt.cwd)
	return mirror.ResumedWindow{Session: session, Window: id}, nil
}

// NewResumeWindow implements ResumeBackend: `new-window -d` in session, or
// `new-session -d` when the session does not exist yet, each with -c dir, -n
// name and argv as tmux's own command arguments (more than one argument: tmux
// runs it directly, with no shell). -P -F answers the new window's id.
func (t Tmux) NewResumeWindow(ctx context.Context, session, name, dir string, argv []string) (string, error) {
	if err := mirror.ValidSession(session); err != nil {
		return "", err
	}
	if mirror.IsViewSession(session) {
		return "", fmt.Errorf("muster-mirror: %q is a viewer session's name", session)
	}
	if len(argv) < 2 || strings.HasPrefix(argv[0], "-") || argv[0] == "" {
		return "", errors.New("muster-mirror: a resume command needs a binary and its arguments")
	}
	if !cleanAbsPath(dir) {
		return "", fmt.Errorf("muster-mirror: %q is not an absolute, clean directory", dir)
	}
	opts := []string{"-d", "-P", "-F", "#{window_id}", "-n", name, "-c", dir}
	var out string
	var err error
	if _, herr := t.run(ctx, "has-session", "-t", "="+session); herr == nil {
		out, err = t.run(ctx, append(append([]string{"new-window", "-t", "=" + session + ":"}, opts...), argv...)...)
	} else {
		out, err = t.run(ctx, append(append([]string{"new-session", "-s", session}, opts...), argv...)...)
		if err != nil && strings.Contains(err.Error(), "duplicate session") {
			// Another resume created it in between.
			out, err = t.run(ctx, append(append([]string{"new-window", "-t", "=" + session + ":"}, opts...), argv...)...)
		}
	}
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(out)
	if err := mirror.ValidWindowID(id); err != nil {
		return "", fmt.Errorf("muster-mirror: tmux answered %q for the new window", id)
	}
	return id, nil
}
