package api

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/prroute"
)

// ---------------------------------------------------------------------------
// PULL-REQUEST → SESSION ROUTING (migration 0055):
// docs/plugins.md#a-pull-request-review-mail.
//
//	the scan (RunPRScan) ──▶ each online mirrored Claude Code session's
//	   transcript, read through the host's own watch from where the last scan
//	   stopped ──▶ prroute.Extractor ──▶ session_prs: who CREATED which pull
//	   request (gh pr create), who PUSHED which branch (git push)
//	a review mail (routeByPR) from a listed, VERIFIED agent-mail sender ──▶ its
//	   last ```json block (prroute.ParseMail) ──▶ the owner's most recent
//	   author ──▶ routeByMuster (the chief's own route, made by muster) — or,
//	   with no online window for that session, the chief's ordinary delivery
//	   with the session named in its turn (prAuthorHint)
//
// 🔴 THE CURSOR IS WHAT MAKES THE SCAN COMPLETE, NOT ITS FREQUENCY. Unlike the
// focus refresh (a bounded backfill each time), a scan RESUMES the watch at
// the byte offset the previous scan of that session ended at (mirror.Msg's
// Gen/Offset, the browser's own reconnect), so every event the transcript
// gains while a window shows the session is read exactly once, however long
// between scans. A scan that stops early (its quiet wait, its timeout) loses
// nothing: the cursor is only ever the end of what was read.
//
// ⚠ WHAT IT CAN MISS, EXACTLY:
//   - a session's FIRST scan, and every scan after this process restarts (the
//     cursors live in memory), read the host's bounded backfill —
//     mirror.MaxBackfillEvents events from the last mirror.BackfillWindow
//     bytes — so authorship older than that window is never seen;
//   - events written after the last scan while NO online window shows the
//     session, if it never comes back (a window /clear-ed or closed): they are
//     read when a window shows that session again, resumed at the cursor;
//   - a call whose result lands after a restart (the extractor's pending calls
//     are in memory too);
//   - SUBAGENTS: a `gh pr create` or `git push` run by a subagent is in the
//     subagent's own transcript, which the parent's stream does not carry
//     (cctranscript drops sidechain records), and the subagent tail muster can
//     ask a host for (mirror.SubagentTail) carries call targets but no
//     results — no proof of authorship. Such a pull request is not recorded.
//   - what prroute's extractor cannot see (its header: capped tool output,
//     wrapped commands).
// ---------------------------------------------------------------------------

// PRMail is pull-request → session routing's configuration.
type PRMail struct {
	// Senders is MUSTER_PR_MAIL_SENDERS: whose verified agent mail is read as
	// a pull-request review. Empty is the feature off.
	Senders plugins.PRMailSenders
}

// routedByPR: a route by pull request (routeByPR).
const routedByPR = "muster, because this session created or pushed the pull request the message is about"

// The scan's bounds.
const (
	// PRScanInterval is how often every owner's online sessions are scanned.
	PRScanInterval = time.Minute
	// prRescanAfter: a session whose window reports no new activity is still
	// scanned this often (the activity the list reports is a card's, bucketed).
	prRescanAfter = 10 * time.Minute
	// prScanTimeout bounds one session's read; prScanQuiet is how long the
	// stream must stay quiet for a resumed read to count as caught up (the
	// host polls the transcript every 300 ms).
	prScanTimeout = 15 * time.Second
	prScanQuiet   = 1500 * time.Millisecond
	// prRetention: a row last seen this long ago is pruned (hourly).
	prRetention  = 30 * 24 * time.Hour
	prPruneEvery = time.Hour
	// prForgetAfter: a session no online window has shown for this long loses
	// its cursor (a later window resumes from a fresh backfill).
	prForgetAfter = 24 * time.Hour
)

// prCursor is one session's scan state.
type prCursor struct {
	// offset is where the next read resumes (0: none yet — a backfill).
	offset int64
	x      prroute.Extractor
	// active and scanned are the window's activity at, and the time of, the
	// last completed scan; shown is when a window last showed the session.
	active, scanned, shown time.Time
}

// prScanState is the scan's process state.
type prScanState struct {
	mu sync.Mutex
	// now is the scan's clock; nil is time.Now (a test sets it).
	now     func() time.Time
	cursors map[int64]map[string]*prCursor
	pruned  map[int64]time.Time
}

func (st *prScanState) clock() time.Time {
	if st.now != nil {
		return st.now()
	}
	return time.Now()
}

// cursor is owner's cursor for session sid, created on first use.
func (st *prScanState) cursor(owner int64, sid string) *prCursor {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.cursors == nil {
		st.cursors = map[int64]map[string]*prCursor{}
	}
	if st.cursors[owner] == nil {
		st.cursors[owner] = map[string]*prCursor{}
	}
	c := st.cursors[owner][sid]
	if c == nil {
		c = &prCursor{}
		st.cursors[owner][sid] = c
	}
	return c
}

// forget drops owner's cursors no window has shown since before.
func (st *prScanState) forget(owner int64, before time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for sid, c := range st.cursors[owner] {
		if c.shown.Before(before) {
			delete(st.cursors[owner], sid)
		}
	}
}

// prRouteOn reports whether pull-request routing is on: a sender list, its
// store, the mirror it reads, and session routing (the route it makes).
func (s *Server) prRouteOn() bool {
	return s.ext.PRMail != nil && len(s.ext.PRMail.Senders) > 0 && s.ext.PRSessions != nil && s.ext.Mirror != nil && routingOn(s.ext)
}

// prMailRef is the pull request message m of plugin p reviews: only with
// routing on, only from a listed sender the plugin VERIFIED, and only when its
// body's last json block names one (prroute.ParseMail).
func (s *Server) prMailRef(p plugins.Plugin, m plugins.Message) (prroute.Ref, bool) {
	if !s.prRouteOn() || !s.ext.PRMail.Senders.Allows(p.Name, m.Sender, m.SenderVerified) {
		return prroute.Ref{}, false
	}
	return prroute.ParseMail(m.Body)
}

// prName is ref as the route and its log name it: "<owner>/<repo>#<n>".
func prName(ref prroute.Ref) string { return ref.Repo + "#" + strconv.FormatInt(ref.PR, 10) }

// routeByPR routes a NEW message m of plugin p — a review of a pull request
// one of the owner's sessions authored — to that session, as routeByThread
// routes a reply (routeByMuster). It reports whether m is taken care of; false
// leaves m to the ordinary delivery to the chief, whose turn then names the
// author when there is one (prAuthorHint).
func (s *Server) routeByPR(ctx context.Context, p plugins.Plugin, m plugins.Message) bool {
	if s.pluginDeliverer == nil {
		return false
	}
	ref, ok := s.prMailRef(p, m)
	if !ok {
		return false
	}
	hit, found, err := s.ext.PRSessions.ResolvePRSession(ctx, ref)
	switch {
	case err != nil:
		s.logger.Printf("prroute: message %d: resolve %s: %v; to the chief", m.ID, prName(ref), err)
		return false
	case !found:
		s.logger.Printf("prroute: message %d: no session of the owner authored %s; to the chief", m.ID, prName(ref))
		return false
	}
	origin := plugins.RouteOrigin{Via: plugins.RouteViaPR, Session: hit.ClaudeSession, PR: prName(ref)}
	if s.routeByMuster(ctx, p, m, origin, "the session that authored pull request "+prName(ref), "a review of pull request "+prName(ref)) {
		s.logger.Printf("prroute: message %d: %s routed to its author %s (%s)", m.ID, prName(ref), hit.ClaudeSession, hit.Kind)
		return true
	}
	s.logger.Printf("prroute: message %d: %s's author %s cannot take it now; to the chief, naming it", m.ID, prName(ref), hit.ClaudeSession)
	return false
}

// prAuthorHint is the author block for message m, delivered to the chief —
// which happens when routeByPR could not route it: the session that authored
// its pull request, with what muster saw it do and whether a window shows it
// now. nil when m is not a review muster reads, or no author is recorded. ctx
// acts as m's owner.
func (s *Server) prAuthorHint(ctx context.Context, m plugins.Message) *plugins.AuthorHint {
	if !s.prRouteOn() {
		return nil
	}
	p, err := s.ext.Plugins.Plugin(ctx, m.PluginID)
	if err != nil {
		return nil
	}
	ref, ok := s.prMailRef(p, m)
	if !ok {
		return nil
	}
	hit, found, err := s.ext.PRSessions.ResolvePRSession(ctx, ref)
	if err != nil || !found {
		return nil
	}
	a := &plugins.AuthorHint{ID: focus.MirrorSubject(hit.ClaudeSession).String(), Host: hit.Host, Kind: string(hit.Kind),
		Repo: hit.Repo, PR: hit.PR, HeadRef: hit.HeadRef, LastSeen: hit.LastSeen}
	if e, ok := s.mirrorEntryForSession(ctx, hit.ClaudeSession); ok {
		a.Online, a.Host, a.Name = true, e.Host, e.DisplayTitle()
	}
	if s.ext.Focus != nil {
		if f, has, err := s.ext.Focus.Get(ctx, focus.MirrorSubject(hit.ClaudeSession)); err == nil && has && f.Has() {
			a.Name = f.Title
		}
	}
	return a
}

// --- the scan -----------------------------------------------------------------

// RunPRScan runs PRScanPass every interval (<= 0: PRScanInterval) until ctx
// ends. It does nothing with pull-request routing off. Started by
// cmd/muster-server.
func (s *Server) RunPRScan(ctx context.Context, interval time.Duration) {
	if !s.prRouteOn() {
		return
	}
	if interval <= 0 {
		interval = PRScanInterval
	}
	s.logger.Printf("prroute: scanning every online mirrored Claude Code session's transcript every %s for the pull requests it "+
		"creates and the branches it pushes; a review mail from %d listed sender(s) goes to its author (MUSTER_PR_MAIL_SENDERS)",
		interval, len(s.ext.PRMail.Senders))
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.PRScanPass(ctx); err != nil && ctx.Err() == nil {
			s.logger.Printf("prroute: scan pass: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// PRScanPass scans every owner's online mirrored sessions once, each owner as
// itself (focusPrincipal: the same act as the focus refresh, reading one
// owner's sessions), and prunes each owner's old rows hourly.
func (s *Server) PRScanPass(ctx context.Context) error {
	if !s.prRouteOn() {
		return nil
	}
	owners, err := s.ext.PRSessions.PRSessionOwners(ctx)
	if err != nil {
		return err
	}
	for _, o := range owners {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		octx := s.focusPrincipal(ctx, o)
		s.prScanOwner(octx, o)
		s.prPrune(octx, o)
	}
	return nil
}

// prPrune deletes owner's rows past prRetention, at most every prPruneEvery.
func (s *Server) prPrune(ctx context.Context, owner int64) {
	st := &s.prScan
	now := st.clock()
	st.mu.Lock()
	if st.pruned == nil {
		st.pruned = map[int64]time.Time{}
	}
	due := now.Sub(st.pruned[owner]) >= prPruneEvery
	if due {
		st.pruned[owner] = now
	}
	st.mu.Unlock()
	if !due {
		return
	}
	if n, err := s.ext.PRSessions.PrunePRSessions(ctx, now.Add(-prRetention)); err != nil {
		s.logger.Printf("prroute: owner %d: prune: %v", owner, err)
	} else if n > 0 {
		s.logger.Printf("prroute: owner %d: pruned %d authorship row(s) unseen for %s", owner, n, prRetention)
	}
}

// prScanOwner scans owner's online mirrored Claude Code sessions — each once,
// in its most recently active window — that may have moved since their last
// scan; ctx acts as owner.
func (s *Server) prScanOwner(ctx context.Context, owner int64) {
	if !s.prRouteOn() {
		return
	}
	now := s.prScan.clock()
	seen := map[string]bool{}
	for _, e := range s.mirrorClaudeEntries(ctx) {
		sid := e.ClaudeSession
		if seen[sid] || ctx.Err() != nil {
			continue
		}
		seen[sid] = true
		c := s.prScan.cursor(owner, sid)
		c.shown = now
		if !c.scanned.IsZero() && e.ActiveAt().Equal(c.active) && now.Sub(c.scanned) < prRescanAfter {
			continue
		}
		moved, quiet, ok := s.prScanSession(ctx, e, c)
		if !ok {
			continue
		}
		c.active, c.scanned = e.ActiveAt(), now
		for _, batch := range []struct {
			sigs []prroute.Signal
			bump bool
		}{{moved, true}, {quiet, false}} {
			if len(batch.sigs) == 0 {
				continue
			}
			if err := s.ext.PRSessions.RecordPRSessions(ctx, sid, e.Host, batch.sigs, batch.bump); err != nil {
				s.logger.Printf("prroute: session %s@%s: record: %v", sid, e.Host, err)
				continue
			}
			s.logger.Printf("prroute: session %s@%s: %d authorship signal(s) recorded", sid, e.Host, len(batch.sigs))
		}
	}
	s.prScan.forget(owner, now.Add(-prForgetAfter))
}

// prScanSession reads e's transcript from c's cursor — or, with none, the
// host's backfill — feeding c's extractor, and advances the cursor to the end
// of what it read. moved are the signals read from new events (which move a
// known row's last_seen), quiet those read from a backfill (which may re-read
// events an earlier scan, or process, already recorded, so they never do).
// ok is false when nothing of the session's conversation was read.
//
// 🔴 THE STREAM MUST BE CONVERSATION e.ClaudeSession (mirrorTail's rule): a
// batch naming another transcript (a /clear or restart since the window was
// listed) ends the read, and nothing of it is fed or recorded.
func (s *Server) prScanSession(ctx context.Context, e mirror.SessionView, c *prCursor) (moved, quiet []prroute.Signal, ok bool) {
	sid := e.ClaudeSession
	ctx, cancel := context.WithTimeout(ctx, prScanTimeout)
	defer cancel()
	gen, off := "", int64(0)
	if c.offset > 0 {
		gen, off = sid, c.offset
	}
	var (
		mu         sync.Mutex
		end        int64
		read       bool
		backfill   bool
		otherConvo bool
	)
	got := make(chan struct{}, 1)
	done := make(chan error, 1)
	safeGo(s.logger, "prroute: read a mirrored transcript", func() {
		done <- s.ext.Mirror.WatchWindow(ctx, e.Host, e.Session, e.Window, gen, off, func(m mirror.Msg) error {
			mu.Lock()
			if m.Gen != sid {
				otherConvo = true
				mu.Unlock()
				cancel()
				return nil
			}
			read = true
			if m.Reset {
				// A backfill: the host could not resume (or there was nothing
				// to resume), and starts the transcript's window over.
				backfill = true
				c.x.Reset()
			}
			if sigs := c.x.Feed(m.Events); len(sigs) > 0 {
				if backfill {
					quiet = append(quiet, sigs...)
				} else {
					moved = append(moved, sigs...)
				}
			}
			if m.Offset > 0 {
				end = m.Offset
			}
			index := m.PromptIndex
			mu.Unlock()
			if index {
				// The prompt index follows a backfill: it is complete.
				cancel()
				return nil
			}
			select {
			case got <- struct{}{}:
			default:
			}
			return nil
		})
	})
	quietWait := time.NewTimer(prScanTimeout)
	defer quietWait.Stop()
	var err error
loop:
	for {
		select {
		case err = <-done:
			break loop
		case <-got:
			quietWait.Reset(prScanQuiet)
		case <-quietWait.C:
			cancel()
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if otherConvo && !read {
		s.logger.Printf("prroute: %s@%s %s holds another conversation now, not %s; nothing read", e.Session, e.Host, e.Window, sid)
		return nil, nil, false
	}
	if !read {
		if err != nil && ctx.Err() == nil {
			s.logger.Printf("prroute: session %s@%s: read: %v", sid, e.Host, err)
		}
		return nil, nil, false
	}
	if end > 0 {
		c.offset = end
	}
	return moved, quiet, true
}
