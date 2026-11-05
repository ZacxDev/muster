package api

import (
	"encoding/json"
	"sync"

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
// once (askScans.done) until it stops being a candidate — stops waiting, its
// conversation changes, it leaves the listing, its host drops — and then
// again the next time it is one; never twice at once (askScans.busy); never
// while its session already has an open question (its watch covers it); and
// not when the host already says it waits on another tool (Window.Pending).
// A read that failed is not counted, so the next nudge tries again.
// ---------------------------------------------------------------------------

// askScanKey is one waiting window, in one conversation, of owner's host.
type askScanKey struct {
	owner                 int64
	host, session, window string
	claude                string
}

// askScans is the scan's bookkeeping.
type askScans struct {
	mu sync.Mutex
	// done is the candidates read in their current waiting spell; busy those
	// being read now.
	done, busy map[askScanKey]bool
	// reads counts the reads started (tests).
	reads int
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

// scanOpenAsks starts a read of every candidate window of owner's ONLINE hosts
// in hs (one read of the hub, the caller's) not read in its waiting spell yet,
// and forgets every one that is no longer a candidate. It returns at once.
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
	var start []askScanKey
	s.askScan.mu.Lock()
	if s.askScan.done == nil {
		s.askScan.done, s.askScan.busy = map[askScanKey]bool{}, map[askScanKey]bool{}
	}
	for k := range s.askScan.done {
		if _, still := now[k]; k.owner == owner && !still {
			delete(s.askScan.done, k)
		}
	}
	for k := range now {
		if !s.askScan.done[k] && !s.askScan.busy[k] {
			start = append(start, k)
		}
	}
	s.askScan.mu.Unlock()
	for _, k := range start {
		// Its watch already covers a session with an open question.
		if len(s.asks.OpenIn(owner, askSourceMirror, k.host, k.claude)) > 0 {
			continue
		}
		s.askScan.mu.Lock()
		if s.askScan.busy[k] || s.askScan.done[k] {
			s.askScan.mu.Unlock()
			continue
		}
		s.askScan.busy[k] = true
		s.askScan.reads++
		s.askScan.mu.Unlock()
		e := now[k]
		safeGo(s.logger, "mirror: read a waiting window for an open question", func() { s.scanOpenAsk(k, e) })
	}
}

// scanOpenAsk reads k's window's transcript tail and restores the question it
// shows open, if any.
func (s *Server) scanOpenAsk(k askScanKey, e mirror.SessionView) {
	evs, _, err := s.mirrorRead(s.askPrincipal(s.streams.context(), k.owner), e, k.claude)
	s.askScan.mu.Lock()
	delete(s.askScan.busy, k)
	if err == nil && len(evs) > 0 {
		s.askScan.done[k] = true
	}
	s.askScan.mu.Unlock()
	if err != nil {
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
