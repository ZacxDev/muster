package mirror

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// RESUMING AN ENDED CLAUDE CODE SESSION ON ITS HOST (spawn-resume): the server
// half. internal/api's mail resume (mail_resume.go) asks a host to start a new
// tmux window running `claude --resume <session id>` so that agent mail for a
// session whose window has closed can still reach it, through the ordinary
// send queue, once the window is listed.
//
// 🔴 THE REQUEST CARRIES A SESSION ID AND NOTHING ELSE. No text, no path, no
// command: the host finds the transcript itself, reads the cwd from it, and
// builds the argv from its own configuration. The mail that caused the resume
// is never on this wire — it reaches the window only through the send queue,
// fenced, after the window is up.
//
// 🔴 OFF ON BOTH HALVES BY DEFAULT. A host offers it only with -interactive AND
// -resume-spawn (Msg.ResumeSpawn), and muster never sends it to a host that did
// not advertise it. muster's own gate (MUSTER_MAIL_RESUME_ALLOW, its kill
// switch and its rate limits) decides whether to ask at all.
//
// NO VERSION BUMP, BACKWARD-COMPATIBLE BOTH WAYS, like PaneScroll: an older
// muster ignores the field and never asks; an older host never sets it, so
// muster never sends it a spawn-resume.

const (
	// TypeSpawnResume (muster → host): start a window resuming Claude Code
	// session Msg.Gen. ID is the request id (the stream id space).
	TypeSpawnResume = "spawn-resume"
	// TypeResumeSpawned (host → muster): its outcome. Session and Window name
	// the new window; Error and Refusal say why not.
	TypeResumeSpawned = "resume-spawned"
)

// The kinds of refusal a host answers a spawn-resume with (Msg.Refusal).
const (
	// ResumeRefusalUnsupported: the host does not resume (no -resume-spawn, or
	// read-only).
	ResumeRefusalUnsupported = "unsupported"
	// ResumeRefusalNotHere: the host holds no transcript of that session. The
	// one refusal after which another host may be asked.
	ResumeRefusalNotHere = "not_here"
	// ResumeRefusalLive: the session is running on this host already.
	ResumeRefusalLive = "live"
	// ResumeRefusalNoCwd: the transcript names no usable working directory, or
	// it no longer exists.
	ResumeRefusalNoCwd = "no_cwd"
	// ResumeRefusalInvalid: not a session id this host accepts.
	ResumeRefusalInvalid = "invalid"
	// ResumeRefusalLimited: the host's own limit (a session resumed moments
	// ago, or too many this hour).
	ResumeRefusalLimited = "limited"
)

var (
	// ErrResumeUnsupported: the host did not advertise Msg.ResumeSpawn.
	ErrResumeUnsupported = errors.New("mirror: this host's muster-mirror does not resume sessions (it needs -interactive and -resume-spawn)")
	// ErrResumeRefused wraps the host's own reason; ResumeRefusal reads its kind.
	ErrResumeRefused = errors.New("mirror: the host refused to resume the session")
	// ErrResumeTimeout: the host did not answer in time.
	ErrResumeTimeout = errors.New("mirror: the host did not answer the resume in time")
)

// resumeTimeout bounds the wait for the host's answer. The host's work is a
// glob, a read of a transcript's head and one tmux command.
const resumeTimeout = 15 * time.Second

// ResumedWindow is the window a host started for a resume.
type ResumedWindow struct {
	Session string
	Window  string
}

// ResumeRefusedError is ErrResumeRefused with the host's refusal: Kind is a
// ResumeRefusal* ("refused" for one the host did not classify), Why its prose.
type ResumeRefusedError struct {
	Kind, Why string
}

func (r *ResumeRefusedError) Error() string {
	return ErrResumeRefused.Error() + " (" + r.Kind + "): " + r.Why
}
func (r *ResumeRefusedError) Unwrap() error { return ErrResumeRefused }

// ResumeRefusal is the kind of a host's refusal (ResumeRefusal*), "" when err is
// not one.
func ResumeRefusal(err error) string {
	var r *ResumeRefusedError
	if errors.As(err, &r) {
		return r.Kind
	}
	return ""
}

type resumeAnswer struct {
	w   ResumedWindow
	err error
}

// SpawnResume asks owner's host to start a window resuming Claude Code session
// sid, and waits for its answer.
func (h *Hub) SpawnResume(ctx context.Context, owner int64, host, sid string) (w ResumedWindow, err error) {
	ctx, done := hubOp(ctx, "spawn-resume")
	defer func() { done(err) }()
	if !ValidClaudeSession(sid) {
		return ResumedWindow{}, fmt.Errorf("mirror: %q is not a Claude session id", sid)
	}
	h.mu.Lock()
	hc := h.hosts[hostKey{owner, host}]
	h.mu.Unlock()
	if hc == nil {
		return ResumedWindow{}, fmt.Errorf("%w (host %q)", ErrHostOffline, host)
	}
	if !hc.resumeSpawn || !hc.interactive {
		return ResumedWindow{}, fmt.Errorf("%w (host %q)", ErrResumeUnsupported, host)
	}
	hc.mu.Lock()
	hc.next++
	id := hc.next
	ch := make(chan resumeAnswer, 1)
	hc.resumes[id] = ch
	hc.mu.Unlock()
	defer func() {
		hc.mu.Lock()
		delete(hc.resumes, id)
		hc.mu.Unlock()
	}()
	if err := writeJSON(ctx, hc.ws, Msg{Type: TypeSpawnResume, ID: id, Gen: sid}); err != nil {
		return ResumedWindow{}, fmt.Errorf("mirror: send the resume to the host: %w", err)
	}
	t := time.NewTimer(resumeTimeout)
	defer t.Stop()
	select {
	case a := <-ch:
		return a.w, a.err
	case <-ctx.Done():
		return ResumedWindow{}, ctx.Err()
	case <-t.C:
		return ResumedWindow{}, ErrResumeTimeout
	}
}

// resumeSpawned hands the host's answer to the resume waiting on m.ID.
func (hc *hostConn) resumeSpawned(m Msg) {
	hc.mu.Lock()
	ch := hc.resumes[m.ID]
	delete(hc.resumes, m.ID)
	hc.mu.Unlock()
	if ch == nil {
		return
	}
	if m.Error != "" {
		kind := m.Refusal
		if kind == "" {
			kind = "refused"
		}
		ch <- resumeAnswer{err: &ResumeRefusedError{Kind: kind, Why: m.Error}}
		return
	}
	if ValidSession(m.Session) != nil || ValidWindowID(m.Window) != nil {
		ch <- resumeAnswer{err: &ResumeRefusedError{Kind: "refused", Why: "the host named no valid window"}}
		return
	}
	ch <- resumeAnswer{w: ResumedWindow{Session: m.Session, Window: m.Window}}
}

// SpawnResume asks one of the viewing principal's hosts to resume Claude Code
// session sid in a new window. See Hub.SpawnResume.
func (s *Service) SpawnResume(ctx context.Context, host, sid string) (ResumedWindow, error) {
	o, err := owner(ctx)
	if err != nil {
		return ResumedWindow{}, err
	}
	return s.hub.SpawnResume(ctx, o, host, sid)
}
