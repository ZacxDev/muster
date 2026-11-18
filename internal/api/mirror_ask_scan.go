package api

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
)

// ---------------------------------------------------------------------------
// THE OPEN-QUESTION SCAN: muster rebuilds the AskUserQuestion cards it lost,
// from the transcripts it already streams — an operator decision (PR #454):
// server-side, no wire change, no host change, so it works with every host
// that runs today.
//
// 🔴 THE TRANSCRIPT IS THE SOURCE OF TRUTH FOR WHAT IS OPEN, NOT MUSTER'S
// MEMORY. The question store (internal/askq) is in memory and the hook that
// registers a question (`muster-mirror ask`) runs ONCE, when the call is made,
// so a muster restart (every deploy) forgot every open question and its window
// fell back to the transcript's no-card row — though the picker was still
// there to be answered. Claude Code writes an AskUserQuestion's tool_use when
// its picker opens and its tool_result only once it is answered or dismissed
// (measured on 2.1.289), so while its session WAITS the call is readable.
//
// So for every Claude window a host lists as WAITING, muster reads the
// transcript's tail through the host (mirrorRead → Mirror.WatchWindow, the
// rendered view's own stream) and, when the newest unanswered call after the
// last prompt is an AskUserQuestion (cctranscript.OpenAskOf), registers it
// under the hook's exact key (askq.Store.Restore): an open question is left
// as it is (one card, the hook's id), an expired one is reopened (a session
// that looked gone), an answered, dismissed or superseded one never is.
//
// WHEN: on the hub's change and waiting-set nudges (attentionMirrorChanged) —
// a host connecting (after a muster restart, or its own reconnect) and a
// window starting to wait.
//
// 🔴 BOUNDED: one read per waiting window per waiting spell. A window is read
// until one COMPLETE read of it (the backfill and the prompt index after it)
// succeeds in its current waiting spell — then not again until it stops being
// a candidate (stops waiting, its conversation changes, it leaves the listing,
// its host drops) and is one anew; never twice at once; never while its
// session already has an open question (its watch covers it); and not when
// the host already says it waits on another tool (Window.Pending).
//
// 🔴 A HOST'S READS SHARE ITS WATCH SLOTS: every read is a WatchWindow, and a
// host serves at most mirror.MaxWatchesPerHost (8) at once — the rendered
// views' and the question watches' too. A host connecting with many waiting
// windows would otherwise open a read for each AT ONCE, refuse the rest
// (ErrTooManyWatches), and starve a view being opened. So at most
// askScanPerHost (2) of one host's reads run at a time, the rest queued,
// leaving 6 slots for everything else.
//
// 🔴 A READ THAT FAILED IS RETRIED ON A TIMER — refused (ErrTooManyWatches,
// the host busy), failed, or PARTIAL (cut by the quiet timer or the deadline
// before the prompt index: its tail may lack a newer result, so nothing is
// restored from it): after askScanRetry, doubling to askScanRetryMax, at most
// askScanAttempts reads in all; a nudge meanwhile does not cut it short. The
// retry is cancelled when the window stops being a candidate (its spell
// ends, the host goes). A read still in flight when its window's spell ended
// counts for nothing: it restores nothing and marks nothing read.
// ---------------------------------------------------------------------------

const (
	// askScanPerHost bounds one host's concurrent scan reads.
	askScanPerHost = 2
	// askScanRetry is the first retry's delay; it doubles, to askScanRetryMax.
	askScanRetry    = 2 * time.Second
	askScanRetryMax = 30 * time.Second
	// askScanAttempts bounds the reads of one window in one waiting spell.
	askScanAttempts = 6
)

// askScanKey is one waiting window, in one conversation, of owner's host.
type askScanKey struct {
	owner                 int64
	host, session, window string
	claude                string
}

// askScanHost is one host of one owner (its reads share its watch slots).
type askScanHost struct {
	owner int64
	host  string
}

// askScanSpell is one candidate's current waiting spell.
type askScanSpell struct {
	id uint64 // unique per spell: a read of an ended spell counts for nothing
	e  mirror.SessionView
	// done: a complete read of this spell succeeded; queued: waiting for a
	// slot of its host; attempts: reads started.
	done, queued bool
	attempts     int
	timer        *time.Timer // a pending retry
}

// askScans is the scan's bookkeeping.
type askScans struct {
	mu sync.Mutex
	// spells is the current candidates; spellSeq numbers them.
	spells   map[askScanKey]*askScanSpell
	spellSeq uint64
	// busy is the windows being read now (of any spell); running is each
	// host's reads now.
	busy    map[askScanKey]bool
	running map[askScanHost]int
	// retry replaces askScanRetry (tests; 0 = default).
	retry time.Duration
}

// askScanCandidate reports whether w is a window the scan reads: a Claude
// window, in a valid Claude session, whose session waits on the person — and
// whose host, when it says what the session waits on, does not name another
// tool.
func askScanCandidate(w mirror.Window) bool {
	if !w.Claude || w.Status != mirror.StatusWaiting || !mirror.ValidClaudeSession(w.ClaudeSession) {
		return false
	}
	return w.Pending == nil || w.Pending.Tool == "" || w.Pending.Tool == askq.ToolName
}

// askScanRead is one read to start.
type askScanRead struct {
	k     askScanKey
	spell uint64
	e     mirror.SessionView
}

// scanOpenAsks queues a read of every candidate window of owner's ONLINE
// hosts in hs (one read of the hub, the caller's) not read in its waiting
// spell yet, forgets every one that is no longer a candidate (and its retry),
// and starts what its hosts' slots allow. It returns at once.
func (s *Server) scanOpenAsks(owner int64, hs []mirror.HostView) {
	if s.asks == nil || owner <= 0 || s.ext.Mirror == nil {
		return
	}
	if _, ok := s.ext.Mirror.(mirrorAsker); !ok {
		return // no ask routes: a card could not be answered or shown
	}
	now := map[askScanKey]mirror.SessionView{}
	for _, h := range hs {
		if !h.Online {
			continue
		}
		for _, w := range h.Windows {
			if askScanCandidate(w) {
				now[askScanKey{owner, h.Host, w.Session, w.ID, w.ClaudeSession}] = mirror.SessionView{Host: h.Host, Session: w.Session, Window: w.ID}
			}
		}
	}
	sc := &s.askScan
	sc.mu.Lock()
	if sc.spells == nil {
		sc.spells, sc.busy, sc.running = map[askScanKey]*askScanSpell{}, map[askScanKey]bool{}, map[askScanHost]int{}
	}
	for k, sp := range sc.spells {
		if _, still := now[k]; k.owner == owner && !still {
			if sp.timer != nil {
				sp.timer.Stop()
			}
			delete(sc.spells, k)
		}
	}
	hosts := map[askScanHost]bool{}
	for k, e := range now {
		sp := sc.spells[k]
		if sp == nil {
			sc.spellSeq++
			sp = &askScanSpell{id: sc.spellSeq}
			sc.spells[k] = sp
		}
		sp.e = e
		if !sp.done && sp.timer == nil && sp.attempts < askScanAttempts {
			sp.queued = true
		}
		hosts[askScanHost{owner, k.host}] = true
	}
	var start []askScanRead
	for h := range hosts {
		start = append(start, s.askScanPumpLocked(h)...)
	}
	sc.mu.Unlock()
	s.askScanStart(start)
}

// askScanPumpLocked takes the reads h's free slots allow off the queue.
func (s *Server) askScanPumpLocked(h askScanHost) []askScanRead {
	sc := &s.askScan
	var out []askScanRead
	for k, sp := range sc.spells {
		if sc.running[h] >= askScanPerHost {
			break
		}
		if k.owner != h.owner || k.host != h.host || !sp.queued || sc.busy[k] {
			continue
		}
		sp.queued = false
		sp.attempts++
		sc.busy[k] = true
		sc.running[h]++
		out = append(out, askScanRead{k, sp.id, sp.e})
	}
	return out
}

func (s *Server) askScanStart(reads []askScanRead) {
	for _, r := range reads {
		safeGo(s.logger, "mirror: read a waiting window for an open question", func() { s.scanOpenAsk(r) })
	}
}

// retryAfterLocked is the delay before the retry that follows attempts reads.
func (sc *askScans) retryAfterLocked(attempts int) time.Duration {
	d := askScanRetry
	if sc.retry > 0 {
		d = sc.retry
	}
	for i := 1; i < attempts && d < askScanRetryMax; i++ {
		d *= 2
	}
	return min(d, askScanRetryMax)
}

// askScanEnd records r's outcome — complete (ok), failed (retry), or neither
// (skipped: its session has an open question) — frees its slot and starts
// what that allows. It reports whether r's spell is still the window's.
func (s *Server) askScanEnd(r askScanRead, ok, retry bool) bool {
	sc := &s.askScan
	h := askScanHost{r.k.owner, r.k.host}
	sc.mu.Lock()
	delete(sc.busy, r.k)
	if sc.running[h]--; sc.running[h] <= 0 {
		delete(sc.running, h)
	}
	sp := sc.spells[r.k]
	current := sp != nil && sp.id == r.spell
	switch {
	case !current:
	case ok:
		sp.done = true
	case !retry:
		sp.attempts-- // skipped, not read: the next nudge looks again
	case sp.attempts < askScanAttempts:
		k, id := r.k, r.spell
		sp.timer = time.AfterFunc(sc.retryAfterLocked(sp.attempts), func() { s.askScanRetry(k, id) })
	default:
		s.logger.Printf("mirror: %s/%s%s not read for an open question after %d attempts; it is read again once it waits anew",
			r.k.host, r.k.session, r.k.window, sp.attempts)
	}
	start := s.askScanPumpLocked(h)
	sc.mu.Unlock()
	s.askScanStart(start)
	return current
}

// askScanRetry queues k's read again, if its spell id is still the window's.
func (s *Server) askScanRetry(k askScanKey, id uint64) {
	if s.streams.context().Err() != nil {
		return // muster is shutting down
	}
	sc := &s.askScan
	sc.mu.Lock()
	sp := sc.spells[k]
	if sp == nil || sp.id != id {
		sc.mu.Unlock()
		return
	}
	sp.timer, sp.queued = nil, true
	start := s.askScanPumpLocked(askScanHost{k.owner, k.host})
	sc.mu.Unlock()
	s.askScanStart(start)
}

// scanOpenAsk reads r's window's transcript tail and restores the question it
// shows open, if any — unless the read was partial or its spell has ended.
func (s *Server) scanOpenAsk(r askScanRead) {
	k := r.k
	// Its watch already covers a session with an open question.
	if len(s.asks.OpenIn(k.owner, askSourceMirror, k.host, k.claude)) > 0 {
		s.askScanEnd(r, false, false)
		return
	}
	evs, idx, err := s.mirrorRead(s.askPrincipal(s.streams.context(), k.owner), r.e, k.claude)
	// Complete: the backfill and the prompt index after it. A read cut by
	// the quiet timer or the deadline returns no error but no index.
	ok := err == nil && idx.Got
	if !s.askScanEnd(r, ok, !ok) || !ok {
		return
	}
	a := cctranscript.OpenAskOf(evs)
	if a == nil || !toolUseIDPattern.MatchString(a.ToolUseID) {
		return
	}
	// The questions as the hook's registration would have them: re-checked
	// against askq's bounds (the stream's caps are not askq's).
	raw, err := json.Marshal(a.Questions)
	if err != nil {
		return
	}
	qs, err := askq.ParseQuestions(raw)
	if err != nil {
		return
	}
	key := askq.Key{Owner: k.owner, Source: askSourceMirror, Scope: k.host, Session: k.claude, ToolUseID: a.ToolUseID}
	got, changed, err := s.asks.Restore(key, qs)
	if err != nil {
		s.logger.Printf("mirror: an open AskUserQuestion in %s/%s%s (session %s) not registered: %v", k.host, k.session, k.window, k.claude, err)
		return
	}
	if changed {
		s.logger.Printf("mirror: %s/%s%s is asking an AskUserQuestion (%d question(s)) in session %s, read open from its transcript: registered (%s)",
			k.host, k.session, k.window, len(qs), k.claude, got.ID)
	}
	if got.State.Open() {
		s.watchAsks(k.owner, k.host, k.claude)
	}
}
