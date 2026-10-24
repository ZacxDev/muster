package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/suggest"
)

// ---------------------------------------------------------------------------
// SUGGESTED REPLIES' server half (internal/suggest): when a set is generated,
// for which sessions, and the routes the composer reads.
//
// 🔴 ONLY WHILE A RENDERED VIEW IS OPEN. The trigger lives in the rendered
// view's own socket (handleMirrorViewWS feeds a suggestTracker every batch it
// relays): a session nobody is looking at never generates. The socket already
// carries the transcript, so a generation reads nothing from the host that the
// view did not already ask for.
//
// 🔴 AT TURN END ONLY. The tracker fires when the host's busy marker falls
// (a turn ended while the view was open), or when the backfill of a view opened
// on an idle session completes (the prompt index, which follows it). Then the
// window must be IDLE by its own status — not busy, not WAITING (a permission
// prompt or a question: a paste there would answer the prompt), not unknown —
// and the final reply must hand control back (suggest.HandsBack).
//
// 🔴 THE EXCLUSIONS (suggestBlocked), checked at generation AND again whenever
// the composer reads the set — so a set made while idle is not served once the
// session is busy or waiting again:
//   - MUSTER_SUGGEST=off, the owner's toggle, the auto-pause;
//   - a window that is not a Claude Code pane with a session id: Hermes, a
//     plain terminal (the Terminal view has no tracker at all);
//   - a host that is offline, has no rendered view, or is not -interactive
//     (the composer is disabled there);
//   - a status other than idle;
//   - an OPEN question card (askq) for the session;
//   - an OPEN attention ask raised by the session — whether or not this device
//     hid it ("Hide until next ask" is a per-device display choice; hidden is
//     not answered, and the server never sees it, so every open ask counts).
//
// 🔴 NEVER SENT. Nothing here pastes; GET /mirror/view/suggest only returns
// text, and the composer only fills.
// ---------------------------------------------------------------------------

// The trigger's bounds.
const (
	// suggestTrackEntries is how many conversation entries a tracker keeps
	// (suggest.Build reads only the newest few, and R).
	suggestTrackEntries = 64
	// suggestTrackAsks is how many prompt-index asks a tracker keeps.
	suggestTrackAsks = suggest.MaxAsks
	// suggestIdleWait bounds how long a turn end waits for the window's status
	// to read idle: the transcript's busy marker and the window list are two
	// channels from the host and need not land together.
	suggestIdleWait = 5 * time.Second
	// suggestIdlePoll is how often it looks.
	suggestIdlePoll = 250 * time.Millisecond
	// suggestTimeout bounds one generation end to end.
	suggestTimeout = 40 * time.Second
)

// suggestTracker is one rendered view socket's running copy of what a
// suggestion needs: the newest entries, the operator's asks, and whether a
// turn is running. It is fed every batch the socket relays (observe).
type suggestTracker struct {
	mu      sync.Mutex
	entries []suggest.Entry
	asks    []string
	askBase int
	// indexed: the host sent a prompt index, so asks come from it; without
	// one (an older host) the tail's own prompts stand in.
	indexed bool
	busy    bool
	// started: the tracker has seen a batch (so a busy marker it sees after
	// is a transition, not the stream's first word).
	started bool
}

// observe folds m in and reports whether it ends a turn: the busy marker fell,
// or the backfill of an idle session completed (its prompt index).
func (t *suggestTracker) observe(m mirror.Msg) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if m.Reset {
		t.entries, t.asks, t.askBase, t.indexed = nil, nil, 0, false
	}
	for _, ev := range m.Events {
		en, ok := suggestEntryOf(ev)
		if !ok {
			continue
		}
		t.entries = append(t.entries, en)
		if en.Role == suggest.RoleUser && t.indexed {
			t.asks = append(t.asks, promptAsk(en.Text))
		}
	}
	if len(t.entries) > suggestTrackEntries {
		t.entries = append([]suggest.Entry(nil), t.entries[len(t.entries)-suggestTrackEntries:]...)
	}
	if m.PromptIndex {
		t.indexed = true
		t.asks = t.asks[:0]
		for _, p := range m.Prompts {
			t.asks = append(t.asks, p.Text)
		}
		t.askBase = m.PromptsMore
	}
	if len(t.asks) > suggestTrackAsks {
		drop := len(t.asks) - suggestTrackAsks
		t.askBase += drop
		t.asks = append([]string(nil), t.asks[drop:]...)
	}
	wasBusy, started := t.busy, t.started
	t.busy, t.started = m.Busy, true
	switch {
	case m.PromptIndex && !m.Busy:
		return true
	case started && wasBusy && !m.Busy:
		return true
	}
	return false
}

// input is the tracker's state as a suggest.Input (focus not filled).
func (t *suggestTracker) input() suggest.Input {
	t.mu.Lock()
	defer t.mu.Unlock()
	in := suggest.Input{Tail: append([]suggest.Entry(nil), t.entries...)}
	if t.indexed {
		in.Asks, in.AskBase = append([]string(nil), t.asks...), t.askBase
		return in
	}
	for _, e := range t.entries {
		if e.Role == suggest.RoleUser {
			in.Asks = append(in.Asks, promptAsk(e.Text))
		}
	}
	return in
}

// suggestEntryOf is one transcript event as a suggestion's entry. The SAME
// redaction rule as focus (focusEntryOf): a prompt and a reply as written, a
// tool call as its name and file path only (focus.ToolLine, from its input —
// never ev.Target, which for Bash is the command line), and NOTHING else: no
// tool output, no thinking, no notices — and, unlike focus, no errors.
func suggestEntryOf(ev cctranscript.Event) (suggest.Entry, bool) {
	switch ev.Kind {
	case cctranscript.KindPrompt:
		return suggest.Entry{Role: suggest.RoleUser, Text: ev.Text}, true
	case cctranscript.KindText:
		return suggest.Entry{Role: suggest.RoleAssistant, Text: ev.Text}, true
	case cctranscript.KindToolCall:
		return suggest.Entry{Role: suggest.RoleTool, Text: focus.ToolLine(ev.Tool, ev.Input)}, true
	}
	return suggest.Entry{}, false
}

// promptAsk is a prompt as an ask: its first non-blank line (the prompt
// index's rule, cctranscript.Prompts).
func promptAsk(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// The exclusion reasons (suggestBlocked). Fixed words: they reach the
// composer's JSON, and carry nothing from the session.
const (
	suggestBlockedOff        = "suggestions are off"
	suggestBlockedUserOff    = "you turned suggestions off"
	suggestBlockedPaused     = "suggestions are paused: few were used"
	suggestBlockedNotClaude  = "not a Claude Code session"
	suggestBlockedNoView     = "no rendered view"
	suggestBlockedReadOnly   = "the host is read-only"
	suggestBlockedNotIdle    = "the session is not idle"
	suggestBlockedQuestion   = "a question is open"
	suggestBlockedAttention  = "an attention ask is open"
	suggestBlockedNoSuggests = ""
)

// suggestBlocked is why (owner's) window e of host h may not show or generate
// suggestions now, "" when it may. It reads the window's status, the question
// store and the attention queue — never the transcript.
func (s *Server) suggestBlocked(ctx context.Context, h mirror.HostView, e mirror.SessionView) string {
	eng := s.ext.Suggest
	switch eng.Enabled(ctx, requestOwner(ctx)) {
	case suggest.OutcomeOff:
		return suggestBlockedOff
	case suggest.OutcomeUserOff:
		return suggestBlockedUserOff
	case suggest.OutcomePaused:
		return suggestBlockedPaused
	}
	if !e.Claude || e.ClaudeSession == "" || e.HermesView() || e.Runtime != "" {
		return suggestBlockedNotClaude
	}
	if !e.HasRenderedView() {
		return suggestBlockedNoView
	}
	if !h.Interactive {
		return suggestBlockedReadOnly
	}
	if st := s.ext.Mirror.AgentState(ctx, h.Host, e.Session, e.Window); st.Unknown != nil || st.Status != mirror.StatusIdle {
		return suggestBlockedNotIdle
	}
	if s.asks != nil && len(s.asks.OpenIn(requestOwner(ctx), askSourceMirror, h.Host, e.ClaudeSession)) > 0 {
		return suggestBlockedQuestion
	}
	if s.ext.Attention != nil {
		open, err := s.ext.Attention.Open(ctx)
		if err != nil {
			// Fail closed: an ask we cannot rule out blocks.
			return suggestBlockedAttention
		}
		for _, a := range open {
			if a.Asker.MirrorHost == h.Host && a.Asker.MirrorSession == e.ClaudeSession {
				return suggestBlockedAttention
			}
		}
	}
	return suggestBlockedNoSuggests
}

// suggestTurn is the WORK of one turn end on (host, session, window): wait
// for the window to read idle, check the exclusions and the pre-gate, build
// the input and ask the engine. It reports whether a set was newly generated
// or served from the cache (so the socket tells the page to fetch it).
func (s *Server) suggestTurn(ctx context.Context, host, session, window string, t *suggestTracker) bool {
	if s.ext.Suggest == nil || s.ext.Mirror == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, suggestTimeout)
	defer cancel()
	in := t.input()
	reply, _, ok := suggest.FinalReply(in.Tail)
	if !ok || !suggest.HandsBack(reply) {
		return false
	}
	h, e, ok := s.suggestWindow(ctx, host, session, window)
	if !ok {
		return false
	}
	// The status may trail the stream's busy marker: wait (bounded) for idle,
	// and give up the moment it is anything that blocks for another reason.
	deadline := time.Now().Add(s.suggestIdleWait())
	for {
		why := s.suggestBlocked(ctx, h, e)
		if why == "" {
			break
		}
		if why != suggestBlockedNotIdle || time.Now().After(deadline) {
			return false
		}
		if st := s.ext.Mirror.AgentState(ctx, h.Host, e.Session, e.Window); st.Status == mirror.StatusWaiting || st.Status == mirror.StatusBusy {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(suggestIdlePoll):
		}
	}
	if s.ext.Focus != nil {
		if f, has, err := s.ext.Focus.Get(ctx, focus.MirrorSubject(e.ClaudeSession)); err == nil && has {
			in.FocusTitle, in.FocusDescription = f.Title, f.Description
		}
	}
	b, ok := suggest.Build(in)
	if !ok {
		return false
	}
	turn := s.ext.Mirror.AgentState(ctx, h.Host, e.Session, e.Window).Since
	set, out, err := s.ext.Suggest.Suggest(ctx, requestOwner(ctx), suggestSubject(h.Host, e), b, turn)
	if err != nil && !errors.Is(err, suggest.ErrCapped) {
		s.logger.Printf("suggest: %s@%s %s: %s", e.Session, h.Host, e.Window, suggest.Reason(err))
	}
	switch out {
	case suggest.OutcomeGenerated, suggest.OutcomeCached:
		return len(set.Suggestions) > 0
	}
	return false
}

func (s *Server) suggestIdleWait() time.Duration {
	if s.suggestIdleWaitFor > 0 {
		return s.suggestIdleWaitFor
	}
	return suggestIdleWait
}

// suggestWindow is the listed entry (host, session, window) of ctx's
// principal, as mirrorEntry resolves it, and whether there is one.
func (s *Server) suggestWindow(ctx context.Context, host, session, window string) (mirror.HostView, mirror.SessionView, bool) {
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	h, e, listed, _, ok := s.mirrorEntry(r, host, session, window)
	return h, e, ok && listed
}

// suggestSubject is the cache key of e's conversation: per host and Claude
// session, so a conversation moved to another window keeps its set.
func suggestSubject(host string, e mirror.SessionView) string {
	return host + "\x00" + e.ClaudeSession
}

// suggestOnTurnEnd runs suggestTurn for a view socket and, when it yields a
// set, calls tell (which writes {"type":"suggest"} to the socket). One at a
// time per socket: a turn end during a running generation waits for it.
func (s *Server) suggestOnTurnEnd(ctx context.Context, host, session, window string, t *suggestTracker, gate chan struct{}, tell func()) {
	if s.ext.Suggest == nil {
		return
	}
	safeGo(s.logger, "suggest: a turn ended in a rendered view", func() {
		select {
		case gate <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-gate }()
		if s.suggestTurn(ctx, host, session, window, t) && ctx.Err() == nil {
			tell()
		}
	})
}

// suggestView is GET /mirror/view/suggest's answer.
type suggestView struct {
	// AI is always true: the composer labels every chip as AI-generated.
	AI          bool                 `json:"ai"`
	Label       string               `json:"label"`
	Suggestions []suggest.Suggestion `json:"suggestions"`
	// Fingerprint names the set (the composer's dismissal is per set).
	Fingerprint string `json:"fingerprint,omitempty"`
	// Reason is why there are none, in fixed words; "" when there are some.
	Reason string `json:"reason,omitempty"`
}

const suggestLabel = "AI-suggested replies"

// registerSuggestRoutes registers the composer's three routes. Each answers
// 404 when the server has no engine (no OpenRouter wiring at all);
// MUSTER_SUGGEST=off keeps the engine and answers "off".
func (s *Server) registerSuggestRoutes(mux Mux) {
	mux.HandleFunc("GET /mirror/view/suggest", s.requireSession(s.handleMirrorSuggest))
	mux.HandleFunc("POST /mirror/view/suggest/event", s.requireOperatorWrite(s.handleMirrorSuggestEvent))
	mux.HandleFunc("POST /account/suggest", s.requireOperatorWrite(s.handleAccountSuggest))
}

// handleMirrorSuggest is GET /mirror/view/suggest?host=&session=[&window=]:
// the window's current set, if the exclusions allow one NOW and it answers
// the window's current turn. It never generates.
func (s *Server) handleMirrorSuggest(w http.ResponseWriter, r *http.Request) {
	if s.ext.Suggest == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	h, e, listed, _, ok := s.mirrorEntry(r, q.Get("host"), q.Get("session"), q.Get("window"))
	if !ok || !listed {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	v := suggestView{AI: true, Label: suggestLabel, Suggestions: []suggest.Suggestion{}}
	if why := s.suggestBlocked(r.Context(), h, e); why != "" {
		v.Reason = why
		s.writeJSON(w, http.StatusOK, v)
		return
	}
	set, has := s.ext.Suggest.Latest(requestOwner(r.Context()), suggestSubject(h.Host, e))
	// 🔴 THE SET MUST ANSWER THIS TURN: a set is served only while the
	// window's status is still the one it was generated in (StatusSince), so a
	// turn that ran and ended since — with no view open to regenerate — never
	// shows the previous turn's chips.
	if has && set.Turn != 0 && set.Turn == s.ext.Mirror.AgentState(r.Context(), h.Host, e.Session, e.Window).Since {
		v.Suggestions, v.Fingerprint = set.Suggestions, set.Fingerprint
		if len(set.Suggestions) == 0 {
			v.Reason = set.Problem
		}
	}
	s.writeJSON(w, http.StatusOK, v)
}

// handleMirrorSuggestEvent is POST /mirror/view/suggest/event (host, session,
// window, event=shown|filled): the composer's count of a set it showed or a
// chip it filled — the auto-pause's input. Counts only: no text is accepted.
func (s *Server) handleMirrorSuggestEvent(w http.ResponseWriter, r *http.Request) {
	if s.ext.Suggest == nil {
		http.NotFound(w, r)
		return
	}
	if _, _, listed, _, ok := s.mirrorEntry(r, r.PostFormValue("host"), r.PostFormValue("session"), r.PostFormValue("window")); !ok || !listed {
		http.NotFound(w, r)
		return
	}
	ev := suggest.Event(r.PostFormValue("event"))
	if ev != suggest.EventShown && ev != suggest.EventFilled {
		http.Error(w, "event is shown or filled", http.StatusBadRequest)
		return
	}
	s.ext.Suggest.Record(requestOwner(r.Context()), ev)
	w.WriteHeader(http.StatusNoContent)
}

// handleAccountSuggest is POST /account/suggest (on=1|0): the caller's own
// toggle. Off also drops their cached sets; on also clears an auto-pause.
func (s *Server) handleAccountSuggest(w http.ResponseWriter, r *http.Request) {
	if s.ext.Suggest == nil {
		http.NotFound(w, r)
		return
	}
	eng := s.ext.Suggest
	if eng.Prefs == nil {
		http.Error(w, "suggestion preferences need a database", http.StatusServiceUnavailable)
		return
	}
	var disabled bool
	switch r.PostFormValue("on") {
	case "1":
	case "0":
		disabled = true
	default:
		http.Error(w, "on is 1 or 0", http.StatusBadRequest)
		return
	}
	owner := requestOwner(r.Context())
	if owner <= 0 {
		http.Error(w, "no user", http.StatusForbidden)
		return
	}
	if err := eng.Prefs.SetDisabled(r.Context(), owner, disabled); err != nil {
		s.logger.Printf("suggest: owner %d: save the toggle: %v", owner, err)
		http.Error(w, "could not save the setting", http.StatusInternalServerError)
		return
	}
	if disabled {
		eng.Forget(owner)
	} else {
		eng.Resume(owner)
	}
	w.WriteHeader(http.StatusNoContent)
}

// RunSuggestSweep drops suggestion text past its TTL until ctx ends. Started
// by cmd/muster-server; nothing without an engine.
func (s *Server) RunSuggestSweep(ctx context.Context) {
	if s.ext.Suggest == nil {
		return
	}
	s.ext.Suggest.Run(ctx)
}
