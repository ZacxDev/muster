package api

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
)

// ---------------------------------------------------------------------------
// RESUMING AN ENDED SESSION FOR ITS MAIL: docs/plugins.md#resuming-an-ended-session.
//
//	agent mail muster would route itself — a reply on a thread a session
//	   started (threadRoute), or a review of a pull request it authored
//	   (prRoute) — whose session NO window shows (routeByThread and routeByPR
//	   both declined it)
//	──▶ the gate (startMailResume): on, the sender allowed, the session live
//	   nowhere, a host that offers resumes, the durable claim (one per
//	   message; MailResumePerSessionPerHour)
//	──▶ in the background (runMailResume): per host, its limit
//	   (MailResumePerHostPerHour), then mirror's spawn-resume — the HOST finds
//	   the transcript, reads its cwd and starts `claude --resume <id>` in a new
//	   tmux window
//	──▶ wait, at most MailResumeWait, until that window can take a route
//	   (resolveRouteTarget), then routeByMuster: the ordinary route, through
//	   the send queue, fenced
//	──▶ anything else: the chief's ordinary delivery, exactly as before
//
// 🔴 THE GATE IS OFF UNLESS THE DEPLOYMENT LISTS SENDERS. An empty
// MUSTER_MAIL_RESUME_ALLOW is no resume at all; MUSTER_MAIL_RESUME_OFF=1 turns
// it off whatever the list says; and a host resumes only with -interactive and
// -resume-spawn of its own. Each refusal falls back to the behaviour without
// this feature: the chief.
//
// 🔴 THE MAIL NEVER REACHES THE HOST'S COMMAND LINE. The spawn-resume names
// the session id only; the message is pasted, as data behind the fence, by the
// send queue once the window is up and idle — the same path, preface and
// gates as every route muster makes (session_routes.go).
// ---------------------------------------------------------------------------

// MailResume is the mail resume's configuration.
type MailResume struct {
	// Allow is MUSTER_MAIL_RESUME_ALLOW: per owner, the senders (each bound to
	// one plugin, VERIFIED only) whose mail may resume an ended session. The
	// relay allowlist's grammar and matching (plugins.RelayAllowlist). Empty
	// is the feature off.
	Allow plugins.RelayAllowlist
	// Off is MUSTER_MAIL_RESUME_OFF=1: the kill switch. On, nothing is
	// resumed whatever Allow says.
	Off bool
}

const (
	// MailResumePerSessionPerHour bounds how often one session is resumed.
	MailResumePerSessionPerHour = 2
	// MailResumePerHostPerHour bounds how many windows muster has one host
	// start in an hour (the host holds its own bound as well).
	MailResumePerHostPerHour = 6
	// MailResumeWait bounds how long a resumed window has to come up and take
	// the route before the message goes to the chief.
	MailResumeWait = 90 * time.Second
	// mailResumePoll is how often the wait looks.
	mailResumePoll = time.Second
)

// mirrorResumer is the mirror plugin's resume (*mirror.Service).
type mirrorResumer interface {
	SpawnResume(ctx context.Context, host, sid string) (mirror.ResumedWindow, error)
}

// mailResumeOn reports whether the mail resume can run at all.
func (s *Server) mailResumeOn() bool {
	mr := s.ext.MailResume
	if mr == nil || mr.Off || mr.Allow.Owners() == 0 || s.ext.MailResumes == nil || s.ext.Mirror == nil || !routingOn(s.ext) {
		return false
	}
	_, ok := s.ext.Mirror.(mirrorResumer)
	return ok
}

// sessionLiveAnywhere reports whether any of the principal's hosts — online,
// or offline but remembered (its last list: the session may still run there)
// — lists a window running Claude session sid, in its active pane or another.
func (s *Server) sessionLiveAnywhere(ctx context.Context, sid string) bool {
	for _, h := range s.ext.Mirror.Hosts(ctx) {
		for _, w := range h.Windows {
			if w.ClaudeSession == sid {
				return true
			}
			for _, o := range w.OtherClaude {
				if o == sid {
					return true
				}
			}
		}
	}
	return false
}

// resumeHosts is where to ask for a resume of mr's session: its recorded host
// alone when there is one, else every online host that offers resumes (sorted;
// each holds the transcript or refuses as not_here).
func (s *Server) resumeHosts(ctx context.Context, mr musterRoute) []string {
	var out []string
	for _, h := range s.ext.Mirror.Hosts(ctx) {
		if !h.Online || !h.ResumeSpawn {
			continue
		}
		if mr.host != "" && h.Host != mr.host {
			continue
		}
		out = append(out, h.Host)
	}
	sort.Strings(out)
	return out
}

// mailResumeKey is one owner's session, for the in-flight join.
type mailResumeKey struct {
	owner int64
	sid   string
}

// startMailResume takes over a NEW message m of plugin p that routeByThread
// and routeByPR both declined, when the gate lets its session be resumed:
// true once a background resume owns m (it routes m or hands it to the chief
// itself); false, with nothing started, for the caller's ordinary delivery.
func (s *Server) startMailResume(ctx context.Context, p plugins.Plugin, m plugins.Message) bool {
	if !s.mailResumeOn() {
		return false
	}
	mr, ok := s.threadRoute(ctx, p, m)
	if !ok {
		mr, ok = s.prRoute(ctx, p, m)
	}
	if !ok {
		return false
	}
	sid := mr.origin.Session
	who := "message " + strconv.FormatInt(m.ID, 10) + " from " + strconv.Quote(m.Sender) + " for session " + sid
	if !s.ext.MailResume.Allow.Allows(p.OwnerID, p.Name, m.Sender, m.SenderVerified) {
		s.logger.Printf("mail resume: %s: the sender may not resume a session; to the chief", who)
		return false
	}
	if !mirror.ValidClaudeSession(sid) {
		return false
	}
	if s.sessionLiveAnywhere(ctx, sid) {
		s.logger.Printf("mail resume: %s: the session is live on a host already (it cannot take the route now); to the chief", who)
		return false
	}
	hosts := s.resumeHosts(ctx, mr)
	if len(hosts) == 0 {
		where := "no online host offers resumes (-resume-spawn)"
		if mr.host != "" {
			where = "its host " + mr.host + " is offline or does not offer resumes (-resume-spawn)"
		}
		s.logger.Printf("mail resume: %s: %s; to the chief", who, where)
		return false
	}
	id, err := s.ext.MailResumes.ClaimMailResume(ctx, plugins.MailResumeClaim{MessageID: m.ID, ClaudeSession: sid,
		Sender: m.Sender, Via: mr.origin.Via}, MailResumePerSessionPerHour)
	if err != nil {
		s.logger.Printf("mail resume: %s: not resumed: %v; to the chief", who, err)
		return false
	}
	s.logger.Printf("mail resume: %s: RESUMING via %s (claim %d), asking %v", who, mr.origin.String(), id, hosts)
	bg, cancel := s.streamContext(ctx)
	s.pluginInFlight.Add(1)
	safeGo(s.logger, "mail resume", func() {
		defer s.pluginInFlight.Done()
		defer cancel()
		if !s.runMailResume(bg, p, m, mr, id, hosts, who) {
			s.deliverPluginMessageCtx(ctx, p, m)
		}
	})
	return true
}

// runMailResume resumes mr's session for m (claim id) and routes m to it:
// true once routed; false — every way it can fail, recorded on the claim — for
// the chief's ordinary delivery.
func (s *Server) runMailResume(ctx context.Context, p plugins.Plugin, m plugins.Message, mr musterRoute, id int64, hosts []string, who string) bool {
	owner := p.OwnerID
	key := mailResumeKey{owner, mr.origin.Session}
	// One resume per session at a time: a message for a session another
	// message is resuming NOW waits for that window instead of starting one.
	s.mailResumeMu.Lock()
	if s.mailResumeFlights == nil {
		s.mailResumeFlights = map[mailResumeKey]bool{}
	}
	joined := s.mailResumeFlights[key]
	s.mailResumeFlights[key] = true
	s.mailResumeMu.Unlock()
	finish := func(state, window, detail string) {
		if err := s.ext.MailResumes.FinishMailResume(context.WithoutCancel(ctx), id, state, window, detail); err != nil {
			s.logger.Printf("mail resume: %s: record %s: %v", who, state, err)
		}
		s.logger.Printf("mail resume: %s: %s %s", who, state, detail)
	}
	window := ""
	if joined {
		// Another message is resuming this session now: wait for its window.
		s.logger.Printf("mail resume: %s: joins the resume already under way", who)
	} else {
		defer func() {
			s.mailResumeMu.Lock()
			delete(s.mailResumeFlights, key)
			s.mailResumeMu.Unlock()
		}()
		w, err := s.spawnResume(ctx, mr.origin.Session, id, hosts, who)
		if err != nil {
			finish(plugins.ResumeRefused, "", err.Error())
			return false
		}
		window = w.Window
		_ = s.ext.MailResumes.FinishMailResume(context.WithoutCancel(ctx), id, plugins.ResumeSpawned, window, "")
	}
	if s.awaitMailResume(ctx, p, m, mr) {
		finish(plugins.ResumeDelivered, window, "")
		return true
	}
	if ctx.Err() != nil {
		finish(plugins.ResumeTimedOut, window, "muster is shutting down")
		return false
	}
	finish(plugins.ResumeTimedOut, window, "no window took the route within "+s.mailResumeWaitFor().String())
	return false
}

// spawnResume asks hosts, in order, to resume sid: each under its own limit
// (reserved on claim id first), stopping at the first that starts a window or
// refuses for any reason but not_here.
func (s *Server) spawnResume(ctx context.Context, sid string, id int64, hosts []string, who string) (mirror.ResumedWindow, error) {
	rs := s.ext.Mirror.(mirrorResumer)
	var why []string
	for _, h := range hosts {
		if err := s.ext.MailResumes.ReserveMailResumeHost(ctx, id, h, MailResumePerHostPerHour); err != nil {
			why = append(why, h+": "+err.Error())
			continue
		}
		w, err := rs.SpawnResume(ctx, h, sid)
		if err == nil {
			s.logger.Printf("mail resume: %s: host %s started tmux %s:%s", who, h, w.Session, w.Window)
			return w, nil
		}
		if rerr := s.ext.MailResumes.ReleaseMailResumeHost(context.WithoutCancel(ctx), id); rerr != nil {
			s.logger.Printf("mail resume: %s: release host %s: %v", who, h, rerr)
		}
		if mirror.ResumeRefusal(err) != mirror.ResumeRefusalNotHere {
			return mirror.ResumedWindow{}, fmt.Errorf("host %s: %w", h, err)
		}
		why = append(why, h+": "+err.Error())
	}
	return mirror.ResumedWindow{}, fmt.Errorf("no host resumed it: %v", why)
}

// awaitMailResume waits, at most mailResumeWaitFor, until mr's session can take
// a route, then makes it (routeByMuster). true once routed.
func (s *Server) awaitMailResume(ctx context.Context, p plugins.Plugin, m plugins.Message, mr musterRoute) bool {
	sub := focus.Subject{Kind: focus.KindMirror, Key: mr.origin.Session}
	deadline := time.NewTimer(s.mailResumeWaitFor())
	defer deadline.Stop()
	tick := time.NewTicker(s.mailResumePollFor())
	defer tick.Stop()
	for {
		if _, refused := s.resolveRouteTarget(ctx, sub); refused == "" {
			return s.routeByMuster(ctx, p, m, mr.origin, mr.target, mr.what)
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-tick.C:
		}
	}
}

func (s *Server) mailResumeWaitFor() time.Duration {
	if s.mailResumeWait > 0 {
		return s.mailResumeWait
	}
	return MailResumeWait
}

func (s *Server) mailResumePollFor() time.Duration {
	if s.mailResumePoll > 0 {
		return s.mailResumePoll
	}
	return mailResumePoll
}

// mailResumeState is the Server's in-flight resumes (embedded).
type mailResumeState struct {
	mailResumeMu      sync.Mutex
	mailResumeFlights map[mailResumeKey]bool
	// mailResumeWait and mailResumePoll override MailResumeWait and
	// mailResumePoll (a test shortens them); zero is the default.
	mailResumeWait, mailResumePoll time.Duration
}
