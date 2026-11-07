package api

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/asklog"
	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/sessionstate"
)

// ---------------------------------------------------------------------------
// THE ASK LOG'S CAPTURE POINTS (internal/asklog; docs/asks.md). Phase 1 of
// ask auto-approval: TELEMETRY ONLY. Nothing here answers, proposes or
// decides anything; it records what was asked, how it was answered and by
// whom.
//
//   - plain_text, mirrored Claude Code: the hub's STATUS hook
//     (AskLogMirrorChanged, mirror.Service.SetStatusNotify) sees a window
//     that names its Claude session go from busy to idle, and the last
//     assistant message is read from its transcript. It is resolved when the
//     window next leaves idle (or turns idle again): the first user message
//     after the question is the answer, with its provenance; a newer
//     assistant message with no user message between is moved_on; the window
//     gone from an online host is gone.
//     🔴 WHY EVERY TURN END, AND NOT THE EPISODES: a plain-text question the
//     owner answers before the idle threshold opens NO session episode (the
//     design's recon: an episode opens only once a session is past
//     MUSTER_CHIEF_NUDGE_IDLE, or waits on a prompt), so a log built on
//     episodes would miss exactly the quick yes this feature exists to count.
//     And the hub's WAITING hook (attentionMirrorChanged) does not fire for
//     busy → idle at all, so this listens on a hook of its own.
//   - plain_text, dispatched agent: persistTurn (every turn's one door)
//     calls askLogAgentTurn; the next turn's end resolves it from the
//     conversation.
//   - ask_user_question: the question store's change hook (wireAsks), which
//     fires when handleMirrorAskOpen's registration is accepted and whenever
//     the entry changes state (the watcher's Resolve after AnswerWindow typed
//     the answer, or the terminal answered it).
//   - attention_ask: askLoggingAttention wraps the attention store, so every
//     path that creates an ask or moves it out of open is seen at the store.
//   - opencode_question / opencode_permission: the same status hook sees the
//     window's named ask arrive and leave; handleAttentionOpenCodeAnswer
//     records the answer it relayed.
//
// 🔴 CAPTURE NEVER BLOCKS OR FAILS THE FLOW: every write goes through the
// asklog.Recorder (off the caller's goroutine, in order) and every failure is
// a log line and muster_ask_capture_errors_total, never an error to the agent
// or the owner. The hooks themselves only compare in-memory state.
// ---------------------------------------------------------------------------

// AskLog is the ask log's configuration (MUSTER_ASK_LOG; Extensions.AskLog).
type AskLog struct {
	// Store is the log (asklog.PG).
	Store asklog.Store
	// TextTTL is how long question and answer text is kept
	// (MUSTER_ASK_LOG_TEXT_TTL); 0 is asklog.DefaultTextTTL.
	TextTTL time.Duration
	// JevAtCapture runs the stall classifier on a kept plain-text question at
	// capture (MUSTER_ASK_LOG_CLASSIFY), reusing the stall sweep's answer for
	// the streak when it has one. Off: the jev columns stay empty.
	JevAtCapture bool
}

// AskLogSweepInterval is how often the retention sweep runs.
const AskLogSweepInterval = 24 * time.Hour

// askLogAgentMessages is how many of a conversation's newest messages the
// agent capture reads to find a reply.
const askLogAgentMessages = 50

// askLogger is the server's ask log: its store, its recorder and what the
// capture points remember between calls.
type askLogger struct {
	cfg AskLog
	rec *asklog.Recorder

	mu sync.Mutex
	// seen is each mirrored window's last observed status (and the opencode
	// ask it waited on): what the status hook diffs against. Read and written
	// by the hook only.
	seen map[askWinKey]askSeen
	// plain is each window's open plain-text ask, and agent each agent
	// conversation's: written by the captures (on the recorder's one worker).
	plain map[askWinKey]askOpen
	agent map[askAgentKey]askOpen
	// turns is each window's last captured turn (askLogTurnQuestion's key).
	turns map[askWinKey]string
	// readHook replaces the mirrored transcript read (tests).
	readHook func(ctx context.Context, c chiefnudge.Session) (askRead, error)
}

type askWinKey struct {
	owner                 int64
	host, session, window string
}

type askAgentKey struct{ owner, agent, session int64 }

type askSeen struct {
	status, prompt string
}

// askOpen is an open plain-text ask: its row, the raw assistant text it was
// raised on (what the reply is looked for after), when, and for an agent the
// newest message id when it was asked.
type askOpen struct {
	id       int64
	question string
	asked    time.Time
	anchor   int64
}

// askRead is a mirrored window's transcript as the capture reads it.
type askRead struct {
	Msgs []chiefnudge.Msg
	Cwd  string
}

func newAskLogger(cfg AskLog, s *Server) *askLogger {
	if cfg.TextTTL <= 0 {
		cfg.TextTTL = asklog.DefaultTextTTL
	}
	return &askLogger{cfg: cfg, rec: asklog.NewRecorder(s.logger), seen: map[askWinKey]askSeen{},
		plain: map[askWinKey]askOpen{}, agent: map[askAgentKey]askOpen{}, turns: map[askWinKey]string{}}
}

// askLogOn reports whether the ask log is wired.
func (s *Server) askLogOn() bool { return s.askLog != nil && s.askLog.cfg.Store != nil }

// askLogFlush waits for every queued capture (tests, and a clean shutdown).
func (s *Server) askLogFlush(ctx context.Context) error {
	if !s.askLogOn() {
		return nil
	}
	return s.askLog.rec.Flush(ctx)
}

// askLogDo queues a capture of kind as owner.
func (s *Server) askLogDo(owner int64, kind string, fn func(context.Context) error) {
	if !s.askLogOn() || owner <= 0 {
		return
	}
	s.askLog.rec.Do(s.attentionPrincipal(context.Background(), owner), kind, fn)
}

// askLogOpen writes a and counts it when it is new.
func (s *Server) askLogOpen(ctx context.Context, a asklog.Ask) (int64, error) {
	id, created, err := s.askLog.cfg.Store.Open(ctx, a)
	if err != nil {
		return 0, asklog.AtStage(asklog.StageWrite, err)
	}
	if created {
		metrics.AsksTotal.WithLabelValues(a.Kind).Inc()
	}
	return id, nil
}

// askLogEnded observes how long an ask waited, once it has ended.
func askLogEnded(a asklog.Ask, r asklog.Resolution) {
	by := "none"
	if asklog.Answered(r.Outcome) && (r.AnsweredBy == asklog.ByOwner || r.AnsweredBy == asklog.ByChief) {
		by = r.AnsweredBy
	}
	end := r.At
	if end.IsZero() {
		end = time.Now()
	}
	if d := end.Sub(a.AskedAt); d >= 0 {
		metrics.AskAnswerSeconds.WithLabelValues(a.Kind, by).Observe(d.Seconds())
	}
}

// askLogResolve records r on row id.
func (s *Server) askLogResolve(ctx context.Context, id int64, r asklog.Resolution) error {
	a, ok, err := s.askLog.cfg.Store.Resolve(ctx, id, r)
	if err != nil {
		return asklog.AtStage(asklog.StageWrite, err)
	}
	if ok {
		askLogEnded(a, r)
	}
	return nil
}

// askLogResolveRef records r on the row of kinds for ref.
func (s *Server) askLogResolveRef(ctx context.Context, kinds []string, ref string, r asklog.Resolution) error {
	a, ok, err := s.askLog.cfg.Store.ResolveRef(ctx, kinds, ref, r)
	if err != nil {
		return asklog.AtStage(asklog.StageWrite, err)
	}
	if ok {
		askLogEnded(a, r)
	}
	return nil
}

// askLogEpisode is the open session episode of (kind, key), 0 for none.
func (s *Server) askLogEpisode(ctx context.Context, kind, key string) int64 {
	if s.ext.Episodes == nil || key == "" {
		return 0
	}
	ep, ok, err := s.ext.Episodes.OpenFor(ctx, kind, key)
	if err != nil || !ok {
		return 0
	}
	return ep.ID
}

// askLogRepo is the one repository (owner/name) claudeSession authored pull
// requests in over session_prs' retention, "" when none or several.
func (s *Server) askLogRepo(ctx context.Context, claudeSession string) string {
	if s.ext.PRSessions == nil || claudeSession == "" {
		return ""
	}
	prs, err := s.ext.PRSessions.SessionPRs(ctx, []string{claudeSession}, s.now().Add(-30*24*time.Hour))
	if err != nil {
		return ""
	}
	repo := ""
	for _, p := range prs[claudeSession] {
		r, _, _ := strings.Cut(p, "#")
		if repo != "" && r != repo {
			return ""
		}
		repo = r
	}
	return repo
}

// askLogJevOf is r as a row keeps it.
func askLogJevOf(r chiefnudge.Result) asklog.Jev {
	return asklog.Jev{Label: r.Label, NeedsInput: r.Scores["needs_input"], AsksPermission: r.Scores["asks_permission"],
		Neither: r.Scores["neither"], AskScore: r.AskScore, Model: r.Model, Err: r.Err}
}

// askLogClassify is the stall classifier's answer for c's streak, when the
// capture classifies (AskLog.JevAtCapture): the streak's answer the stall sweep or
// session state already has, else ONE call through classifyStall (the
// classifier's one door; fleet_collector_test.go's ledger lists this caller).
// The zero Jev when classification is off or no classifier is configured.
func (s *Server) askLogClassify(ctx context.Context, owner int64, c chiefnudge.Session, text string) (asklog.Jev, error) {
	if !s.askLog.cfg.JevAtCapture {
		return asklog.Jev{}, nil
	}
	now := s.now()
	if r, ok := s.nudgeRetry.cachedJev(nudgeRetryKey(owner, c), now); ok {
		return askLogJevOf(r), nil
	}
	if r, ok := s.nudgeJevFor(ctx, c); ok {
		return askLogJevOf(r), nil
	}
	if s.ext.ChiefNudge == nil || s.ext.ChiefNudge.Classifier == nil {
		return asklog.Jev{}, nil
	}
	r := s.classifyStall(ctx, s.ext.ChiefNudge.Classifier, owner, text)
	if r.Err != "" {
		return askLogJevOf(r), asklog.AtStage(asklog.StageClassify, errors.New(r.Err))
	}
	return askLogJevOf(r), nil
}

// askLogPrefilter is asklog.Prefilter, counted.
func askLogPrefilter(text string) bool {
	if asklog.Prefilter(text) {
		metrics.AskPlainTextPrefilter.WithLabelValues("kept").Inc()
		return true
	}
	metrics.AskPlainTextPrefilter.WithLabelValues("dropped").Inc()
	return false
}

// ---------------------------------------------------------------------------
// Mirrored windows: plain text (Claude Code) and opencode asks.
// ---------------------------------------------------------------------------

// AskLogMirrorChanged is the hub's status hook (mirror.Service.SetStatusNotify;
// cmd/muster-server): owner's windows' status moved. It diffs each window
// against what it last saw and queues the captures a change calls for:
//
//   - a Claude window naming its session, busy → idle: a turn ended (read its
//     last assistant message for a question);
//   - such a window leaving idle (busy, waiting): the owner may have replied
//     (resolve its open question);
//   - an opencode window's named ask appearing, changing or leaving;
//   - a window gone from an online host: its open asks end.
//
// A window seen for the first time records its status and captures nothing
// (a restart must not log every idle window's last question again) — except
// an opencode window's named ask, whose row is keyed by its prompt id and so
// cannot be logged twice.
func (s *Server) AskLogMirrorChanged(owner int64) {
	if s == nil || !s.askLogOn() || s.ext.Mirror == nil || owner <= 0 {
		return
	}
	at := s.now()
	hs := s.ext.Mirror.Hosts(s.attentionPrincipal(context.Background(), owner))
	l := s.askLog
	l.mu.Lock()
	defer l.mu.Unlock()
	listed := map[askWinKey]bool{}
	online := map[string]bool{}
	for _, h := range hs {
		if !h.Online {
			continue // a host that dropped decides nothing: its windows may come back
		}
		online[h.Host] = true
		for _, w := range h.Windows {
			k := askWinKey{owner: owner, host: h.Host, session: w.Session, window: w.ID}
			listed[k] = true
			prev, had := l.seen[k]
			cur := askSeen{status: w.Status}
			isOpenCode := w.AgentRuntime() == mirror.RuntimeOpenCode && w.AgentSession != ""
			if isOpenCode && w.Status == mirror.StatusWaiting && w.Pending != nil && w.Pending.PromptID != "" &&
				(w.Pending.Kind == mirror.PendingQuestion || w.Pending.Kind == mirror.PendingPermission) {
				cur.prompt = w.Pending.PromptID
			}
			l.seen[k] = cur
			if prev.prompt != "" && prev.prompt != cur.prompt {
				s.askLogOpenCodeLeft(owner, prev.prompt, at)
			}
			if cur.prompt != "" && cur.prompt != prev.prompt {
				s.askLogOpenCodeAsk(owner, h.Host, w, at)
			}
			if !had || !(w.Claude && w.ClaudeSession != "" && w.Runtime == "") {
				continue
			}
			switch {
			case prev.status == mirror.StatusBusy && w.Status == mirror.StatusIdle:
				s.askLogTurnEnd(owner, k, w, at)
			case prev.status == mirror.StatusIdle && w.Status != mirror.StatusIdle && w.Status != "":
				s.askLogTurnStart(owner, k, w, at)
			}
		}
	}
	for k, prev := range l.seen {
		if k.owner != owner || listed[k] || !online[k.host] {
			continue
		}
		delete(l.seen, k)
		if prev.prompt != "" {
			s.askLogOpenCodeLeft(owner, prev.prompt, at)
		}
		s.askLogWindowGone(owner, k, at)
	}
}

// askLogSession is window w of host as the stall sweep keys it.
func askLogSession(host string, w mirror.Window) chiefnudge.Session {
	c := chiefnudge.Session{Kind: chiefnudge.KindMirror, Key: w.ClaudeSession, Host: host, TmuxSession: w.Session, Window: w.ID,
		Index: w.Index, Status: w.Status}
	if w.StatusSince > 0 {
		c.StreakKey = "since:" + strconv.FormatInt(w.StatusSince, 10)
	}
	return c
}

// askLogRead is c's transcript and working directory (the test hook
// replaces it).
func (s *Server) askLogRead(ctx context.Context, c chiefnudge.Session) (askRead, error) {
	if s.askLog.readHook != nil {
		return s.askLog.readHook(ctx, c)
	}
	e := mirror.SessionView{Host: c.Host, Session: c.TmuxSession, Window: c.Window, ClaudeSession: c.Key}
	evs, _, err := s.mirrorRead(ctx, e, mirror.SessionIDOf(c.Key))
	if err != nil && (len(evs) == 0 || errors.Is(err, errNoSession)) {
		return askRead{}, err
	}
	out := askRead{Msgs: nudgeMirrorMessages(evs, s.now())}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Cwd != "" {
			out.Cwd = evs[i].Cwd
			break
		}
	}
	return out, nil
}

// askLogTurnEnd queues the capture of window k's turn end: the turn's last
// assistant message, kept when it passes the pre-filter. A question still open
// on the window is resolved first from the same read.
//
// 🔴 THE READ HAPPENS AFTER THE TURN ENDED, AND THE OWNER MAY ALREADY HAVE
// REPLIED — the quick yes this log exists to count. So the turn's message is
// not "the last message" but the last assistant message before any trailing
// user messages (askLogTurnQuestion), and a reply already in the read resolves
// the new row at once. Each turn is captured once: it is keyed by the prompt
// that started it and its text (askLogger.turns), so a later read that finds
// the same turn writes nothing.
func (s *Server) askLogTurnEnd(owner int64, k askWinKey, w mirror.Window, at time.Time) {
	c := askLogSession(k.host, w)
	s.askLogDo(owner, asklog.KindPlainText, func(ctx context.Context) error {
		read, err := s.askLogRead(ctx, c)
		if err != nil {
			return asklog.AtStage(asklog.StageRead, err)
		}
		if err := s.askLogReplyIn(ctx, k, read.Msgs, at); err != nil {
			return err
		}
		last, key, ok := askLogTurnQuestion(read.Msgs)
		if !ok {
			return nil
		}
		s.askLog.mu.Lock()
		seen := s.askLog.turns[k] == key
		s.askLog.turns[k] = key
		s.askLog.mu.Unlock()
		if seen || !askLogPrefilter(last.Text) {
			return nil
		}
		jev, jerr := s.askLogClassify(ctx, owner, c, last.Text)
		id, err := s.askLogOpen(ctx, asklog.Ask{Kind: asklog.KindPlainText, Runtime: asklog.RuntimeClaudeCode, Host: k.host,
			TmuxSession: k.session, Window: k.window, ClaudeSession: w.ClaudeSession, Cwd: read.Cwd,
			Repo: s.askLogRepo(ctx, w.ClaudeSession), EpisodeID: s.askLogEpisode(ctx, sessionstate.KindMirror, w.ClaudeSession),
			Question: last.Text, Jev: jev, AskedAt: at})
		if err != nil {
			return err
		}
		s.askLog.mu.Lock()
		s.askLog.plain[k] = askOpen{id: id, question: last.Text, asked: at}
		s.askLog.mu.Unlock()
		// A reply the owner gave before this read.
		if err := s.askLogReplyIn(ctx, k, read.Msgs, at); err != nil {
			return err
		}
		return jerr
	})
}

// askLogTurnQuestion is the turn-end message of msgs: the last non-empty
// assistant message, looking past any user messages after it (replies that
// arrived before the read). key names that turn: the id of the prompt that
// started it, and the message's text. ok false when there is none.
func askLogTurnQuestion(msgs []chiefnudge.Msg) (m chiefnudge.Msg, key string, ok bool) {
	i := len(msgs) - 1
	for i >= 0 && msgs[i].Role == "user" {
		i--
	}
	for i >= 0 && msgs[i].Role != "assistant" && msgs[i].Role != "user" {
		i--
	}
	if i < 0 || msgs[i].Role != "assistant" || strings.TrimSpace(msgs[i].Text) == "" {
		return chiefnudge.Msg{}, "", false
	}
	prompt := ""
	for j := i - 1; j >= 0; j-- {
		if msgs[j].Role == "user" {
			prompt = msgs[j].ID + "@" + strconv.Itoa(j)
			if msgs[j].ID != "" {
				prompt = msgs[j].ID
			}
			break
		}
	}
	return msgs[i], prompt + "\x00" + msgs[i].Text, true
}

// askLogTurnStart queues the resolution of window k's open question: the
// window left idle, so the owner (or something) may have replied.
func (s *Server) askLogTurnStart(owner int64, k askWinKey, w mirror.Window, at time.Time) {
	c := askLogSession(k.host, w)
	s.askLogDo(owner, asklog.KindPlainText, func(ctx context.Context) error {
		s.askLog.mu.Lock()
		_, open := s.askLog.plain[k]
		s.askLog.mu.Unlock()
		if !open {
			return nil
		}
		read, err := s.askLogRead(ctx, c)
		if err != nil {
			return asklog.AtStage(asklog.StageRead, err)
		}
		return s.askLogReplyIn(ctx, k, read.Msgs, at)
	})
}

// askLogReplyIn resolves window k's open question from msgs, when they say how
// it went: the first user message after the question is its answer; an
// assistant message after it with no user message between means the session
// moved on. Neither (the question is the newest message, or it is not in the
// read at all): it stays open.
func (s *Server) askLogReplyIn(ctx context.Context, k askWinKey, msgs []chiefnudge.Msg, at time.Time) error {
	s.askLog.mu.Lock()
	open, ok := s.askLog.plain[k]
	s.askLog.mu.Unlock()
	if !ok {
		return nil
	}
	q := strings.TrimSpace(open.question)
	idx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" && strings.TrimSpace(msgs[i].Text) == q {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	var r asklog.Resolution
	for _, m := range msgs[idx+1:] {
		if m.Role == "user" && strings.TrimSpace(m.Text) != "" {
			// The reply's own time, unless the host's clock puts it before
			// the question (then: when muster read it).
			when := m.At
			if when.IsZero() || when.Before(open.asked) {
				when = at
			}
			r = asklog.Resolution{Outcome: asklog.OutcomeAnswered, Answer: m.Text, AnsweredBy: asklog.ByOwner,
				Provenance: s.askLogProvenance(ctx, k, m.Text), At: when}
			break
		}
		if m.Role == "assistant" && strings.TrimSpace(m.Text) != "" {
			r = asklog.Resolution{Outcome: asklog.OutcomeMovedOn, At: at}
			break
		}
	}
	if r.Outcome == "" {
		return nil
	}
	s.askLog.mu.Lock()
	delete(s.askLog.plain, k)
	s.askLog.mu.Unlock()
	return s.askLogResolve(ctx, open.id, r)
}

// askLogWindowGone ends window k's open question: the window is gone from an
// online host before any reply was read.
func (s *Server) askLogWindowGone(owner int64, k askWinKey, at time.Time) {
	s.askLogDo(owner, asklog.KindPlainText, func(ctx context.Context) error {
		s.askLog.mu.Lock()
		open, ok := s.askLog.plain[k]
		delete(s.askLog.plain, k)
		s.askLog.mu.Unlock()
		if !ok {
			return nil
		}
		return s.askLogResolve(ctx, open.id, asklog.Resolution{Outcome: asklog.OutcomeGone, At: at})
	})
}

// sentBodyReader is the send queue's read of what it pasted into a window
// (sendqueue.PGStore.SentBodies); a queue without it proves nothing.
type sentBodyReader interface {
	SentBodies(ctx context.Context, t sendqueue.Target, since time.Time, limit int) ([]sendqueue.SentBody, error)
}

// askLogProvenance is what muster can prove of reply, a user message in window
// k's transcript (asks.answer_provenance):
//
//   - the body of a message its send queue pasted there: muster_send for the
//     owner's own, route for a routed message, nudge for a chief nudge or
//     relayed answer — matched by the ROW that carried it, never by wording;
//   - else text that starts like a routed message or a nudge: route, nudge;
//   - else pane: muster did not put it there, and a pane message has no
//     author it can prove (the owner typing, or another agent's send-keys).
func (s *Server) askLogProvenance(ctx context.Context, k askWinKey, reply string) string {
	t := strings.TrimSpace(reply)
	if q, ok := s.ext.SendQueue.(sentBodyReader); ok && s.ext.SendQueue != nil {
		sent, err := q.SentBodies(ctx, sendqueue.Target{Host: k.host, Session: k.session, Window: k.window}, s.now().Add(-7*24*time.Hour), 200)
		if err == nil {
			if p := askLogProvenanceOf(t, sent); p != "" {
				return p
			}
		}
	}
	return askLogTextProvenance(t)
}

// askLogProvenanceOf is reply's provenance when one of sent carried it, ""
// when none did.
func askLogProvenanceOf(reply string, sent []sendqueue.SentBody) string {
	for _, b := range sent {
		if strings.TrimSpace(b.Body) != reply {
			continue
		}
		switch {
		case b.RouteID != "":
			return asklog.ProvRoute
		case b.ChiefNudgeID > 0:
			return asklog.ProvNudge
		default:
			return asklog.ProvMusterSend
		}
	}
	return ""
}

// askLogTextProvenance is the provenance of a reply no queued row carried:
// route or nudge by the prefix muster writes on those, else pane.
func askLogTextProvenance(reply string) string {
	switch {
	case strings.HasPrefix(reply, RoutedPrefacePrefix), strings.HasPrefix(reply, "<<<"):
		return asklog.ProvRoute
	case strings.HasPrefix(reply, chiefnudge.NudgeLabel):
		return asklog.ProvNudge
	}
	return asklog.ProvPane
}

// askLogOpenCodeKind is the kind of an opencode ask of pending kind pk.
func askLogOpenCodeKind(pk string) string {
	if pk == mirror.PendingQuestion {
		return asklog.KindOpenCodeQuestion
	}
	return asklog.KindOpenCodePermission
}

// askLogOpenCodeAsk queues the capture of window w's named opencode ask.
func (s *Server) askLogOpenCodeAsk(owner int64, host string, w mirror.Window, at time.Time) {
	p := *w.Pending
	kind := askLogOpenCodeKind(p.Kind)
	q := p.Summary
	var opts []string
	if p.Kind == mirror.PendingQuestion {
		opts = p.OptionList()
	} else {
		if p.Detail != "" {
			q += "\n\n" + p.Detail
		}
		opts = strings.Fields(p.Choices)
	}
	key := w.SessionKey()
	s.askLogDo(owner, kind, func(ctx context.Context) error {
		raw, err := json.Marshal(opts)
		if err != nil {
			return err
		}
		if opts == nil {
			raw = nil
		}
		_, err = s.askLogOpen(ctx, asklog.Ask{Kind: kind, Runtime: asklog.RuntimeOpenCode, Host: host, TmuxSession: w.Session,
			Window: w.ID, ClaudeSession: key, SourceRef: p.PromptID, Question: q, Options: raw, AskedAt: at,
			EpisodeID: s.askLogEpisode(ctx, sessionstate.KindMirror, key)})
		return err
	})
}

// askLogOpenCodeLeft queues the end of opencode ask prompt: it left the host's
// list with no answer recorded from here (moved_on). An answer relayed from
// muster replaces that end (askLogOpenCodeAnswered; asklog's soft outcomes).
func (s *Server) askLogOpenCodeLeft(owner int64, prompt string, at time.Time) {
	s.askLogDo(owner, asklog.KindOpenCodeQuestion, func(ctx context.Context) error {
		return s.askLogResolveRef(ctx, asklog.OpenCodeKinds, prompt, asklog.Resolution{Outcome: asklog.OutcomeMovedOn, At: at})
	})
}

// askLogOpenCodeAnswered records the answer handleAttentionOpenCodeAnswer
// relayed to opencode ask prompt.
func (s *Server) askLogOpenCodeAnswered(owner int64, prompt, choice string) {
	at := s.now()
	s.askLogDo(owner, asklog.KindOpenCodeQuestion, func(ctx context.Context) error {
		return s.askLogResolveRef(ctx, asklog.OpenCodeKinds, prompt, asklog.Resolution{Outcome: asklog.OutcomeAnswered,
			Answer: choice, AnsweredBy: asklog.ByOwner, At: at})
	})
}

// ---------------------------------------------------------------------------
// Dispatched agents: plain text at a turn's end.
// ---------------------------------------------------------------------------

// askLogAgentTurn is persistTurn's capture: the turn's parts were just stored
// in conversation session of agent a. It first resolves the conversation's
// open question (the first user message after it is the answer; none means the
// agent moved on), then logs this turn's last text when it passes the
// pre-filter.
func (s *Server) askLogAgentTurn(ctx context.Context, a agents.Agent, session agents.ChatSession, parts []agents.TurnPart) {
	if !s.askLogOn() || a.ID <= 0 || session.ID <= 0 || s.ext.Agents == nil {
		return
	}
	owner := a.OwnerID
	if owner <= 0 {
		owner = requestOwner(ctx)
	}
	text := ""
	for i := len(parts) - 1; i >= 0; i-- {
		if (parts[i].Kind == "" || parts[i].Kind == "text") && strings.TrimSpace(parts[i].Content) != "" {
			text = parts[i].Content
			break
		}
	}
	if strings.HasPrefix(text, AgentTurnFailedPrefix) {
		text = "" // muster's stand-in for a failed turn, not the agent's words
	}
	at := s.now()
	k := askAgentKey{owner: owner, agent: a.ID, session: session.ID}
	c := chiefnudge.Session{Kind: chiefnudge.KindAgent, Key: chiefnudge.AgentKey(a.ID, session.ID), AgentID: a.ID, ChatSessionID: session.ID}
	s.askLogDo(owner, asklog.KindPlainText, func(ctx context.Context) error {
		msgs, err := s.ext.Agents.ListRecentChatMessages(ctx, session.ID, askLogAgentMessages)
		if err != nil {
			return asklog.AtStage(asklog.StageRead, err)
		}
		var newest int64
		for _, m := range msgs {
			newest = max(newest, m.ID)
		}
		s.askLog.mu.Lock()
		open, has := s.askLog.agent[k]
		delete(s.askLog.agent, k)
		s.askLog.mu.Unlock()
		if has {
			r := asklog.Resolution{Outcome: asklog.OutcomeMovedOn, At: at}
			for _, m := range msgs {
				if m.ID <= open.anchor || m.Role != "user" || (m.Kind != "" && m.Kind != "text") || strings.TrimSpace(m.Content) == "" {
					continue
				}
				r = asklog.Resolution{Outcome: asklog.OutcomeAnswered, Answer: m.Content, AnsweredBy: asklog.ByOwner,
					Provenance: askLogAgentProvenance(m), At: m.CreatedAt}
				break
			}
			if err := s.askLogResolve(ctx, open.id, r); err != nil {
				return err
			}
		}
		if text == "" || !askLogPrefilter(text) {
			return nil
		}
		jev, jerr := s.askLogClassify(ctx, owner, c, text)
		id, err := s.askLogOpen(ctx, asklog.Ask{Kind: asklog.KindPlainText, Runtime: asklog.RuntimeAgent, AgentID: a.ID,
			ChatSessionID: session.ID, Repo: a.Repo, EpisodeID: s.askLogEpisode(ctx, sessionstate.KindAgent, c.Key),
			Question: text, Jev: jev, AskedAt: at})
		if err != nil {
			return err
		}
		s.askLog.mu.Lock()
		s.askLog.agent[k] = askOpen{id: id, question: text, asked: at, anchor: newest}
		s.askLog.mu.Unlock()
		return jerr
	})
}

// askLogAgentProvenance is what muster can prove of m, a user message in an
// agent's conversation: typed into muster's chat by the owner (TypedBy, one
// writer: runEnteredTurn) is muster_send; a nudge or a routed message by the
// prefix muster writes on it; anything else (a delivered answer, a task
// event, a plugin message) has no author muster can prove: pane.
func askLogAgentProvenance(m agents.ChatMessage) string {
	if m.TypedBy > 0 {
		return asklog.ProvMusterSend
	}
	return askLogTextProvenance(strings.TrimSpace(m.Content))
}

// ---------------------------------------------------------------------------
// AskUserQuestion: the question store's change hook.
// ---------------------------------------------------------------------------

// askLogQuestion records entry e of the question store as it opens and ends.
// Called from the store's change hook (wireAsks) for a mirror host's question.
func (s *Server) askLogQuestion(e askq.Entry) {
	if !s.askLogOn() {
		return
	}
	owner := e.Key.Owner
	switch e.State {
	case askq.Pending:
		at := e.Created
		s.askLogDo(owner, asklog.KindAskUserQuestion, func(ctx context.Context) error {
			type opt struct {
				Header      string   `json:"header,omitempty"`
				Question    string   `json:"question"`
				Options     []string `json:"options"`
				MultiSelect bool     `json:"multiSelect"`
			}
			var qs []string
			opts := make([]opt, 0, len(e.Questions))
			for _, q := range e.Questions {
				qs = append(qs, q.Question)
				o := opt{Header: q.Header, Question: q.Question, MultiSelect: q.MultiSelect, Options: []string{}}
				for _, x := range q.Options {
					o.Options = append(o.Options, x.Label)
				}
				opts = append(opts, o)
			}
			raw, err := json.Marshal(opts)
			if err != nil {
				return err
			}
			a := asklog.Ask{Kind: asklog.KindAskUserQuestion, Runtime: asklog.RuntimeClaudeCode, Host: e.Key.Scope,
				ClaudeSession: e.Key.Session, SourceRef: e.ID, Question: strings.Join(qs, "\n\n"), Options: raw, AskedAt: at,
				Repo: s.askLogRepo(ctx, e.Key.Session), EpisodeID: s.askLogEpisode(ctx, sessionstate.KindMirror, e.Key.Session)}
			a.TmuxSession, a.Window = s.askLogWindowOf(ctx, e.Key.Scope, e.Key.Session)
			_, err = s.askLogOpen(ctx, a)
			return err
		})
	case askq.Answered, askq.Declined, askq.Expired, askq.Superseded:
		r := asklog.Resolution{At: e.Resolved}
		switch e.State {
		case askq.Answered:
			// Whoever's answer the CLI took — typed from muster or given in
			// the terminal — the owner's.
			r.Outcome, r.AnsweredBy, r.Answer = asklog.OutcomeAnswered, asklog.ByOwner, askqAnswerText(e)
		case askq.Declined:
			r.Outcome, r.AnsweredBy = asklog.OutcomeDeclined, asklog.ByOwner
		case askq.Expired:
			r.Outcome = asklog.OutcomeExpired
		default:
			r.Outcome = asklog.OutcomeSuperseded
		}
		s.askLogDo(owner, asklog.KindAskUserQuestion, func(ctx context.Context) error {
			return s.askLogResolveRef(ctx, []string{asklog.KindAskUserQuestion}, e.ID, r)
		})
	}
}

// askqAnswerText is e's recorded answers, one line per question in the order
// asked: "<question> → <answer>".
func askqAnswerText(e askq.Entry) string {
	var lines []string
	for _, q := range e.Questions {
		if a, ok := e.Answers[q.Question]; ok {
			lines = append(lines, q.Question+" → "+a)
		}
	}
	if len(lines) == 0 {
		for q, a := range e.Answers {
			lines = append(lines, q+" → "+a)
		}
	}
	return strings.Join(lines, "\n")
}

// askLogWindowOf is the tmux session and window on host that show Claude
// session cs, "" when none is listed.
func (s *Server) askLogWindowOf(ctx context.Context, host, cs string) (session, window string) {
	if s.ext.Mirror == nil || cs == "" {
		return "", ""
	}
	for _, h := range s.ext.Mirror.Hosts(ctx) {
		if h.Host != host {
			continue
		}
		for _, w := range h.Windows {
			if w.ClaudeSession == cs || w.SessionKey() == cs {
				return w.Session, w.ID
			}
		}
	}
	return "", ""
}

// ---------------------------------------------------------------------------
// attention_ask: the store, wrapped.
// ---------------------------------------------------------------------------

// askLoggingAttention is the attention store with the ask log's capture: every
// ask created (an ask, never a notice; never muster's own system asks — the
// log is what AGENTS ask) and every path that moves one out of open — the
// queue's answer, an answer from a session, the chief's, a cancel, an answer
// in the session, the expiry sweep — is recorded from the rows the store
// returns. Each call returns exactly what the store returned: the capture is
// queued, never awaited, and its failure is only counted.
type askLoggingAttention struct {
	attention.Store
	s *Server
}

// askLogAttentionKind reports whether a is an ask the log records.
func askLogAttentionKind(a attention.Ask) bool {
	return (a.Kind == "" || a.Kind == attention.KindAsk) && !a.Asker.System
}

func (w askLoggingAttention) Create(ctx context.Context, n attention.New) (attention.Ask, error) {
	a, err := w.Store.Create(ctx, n)
	if err == nil && askLogAttentionKind(a) {
		w.s.askLogAttentionOpened(a)
	}
	return a, err
}

func (w askLoggingAttention) Answer(ctx context.Context, id int64, answer string) (attention.Ask, error) {
	a, err := w.Store.Answer(ctx, id, answer)
	w.settled(a, err)
	return a, err
}

func (w askLoggingAttention) AnswerFromSession(ctx context.Context, id int64, answer, host, session string) (attention.Ask, error) {
	a, err := w.Store.AnswerFromSession(ctx, id, answer, host, session)
	w.settled(a, err)
	return a, err
}

func (w askLoggingAttention) AnswerByChief(ctx context.Context, id int64, answer string) (attention.Ask, error) {
	a, err := w.Store.AnswerByChief(ctx, id, answer)
	w.settled(a, err)
	return a, err
}

func (w askLoggingAttention) Cancel(ctx context.Context, id int64) (attention.Ask, error) {
	a, err := w.Store.Cancel(ctx, id)
	w.settled(a, err)
	return a, err
}

func (w askLoggingAttention) AnswerInSession(ctx context.Context, asker attention.Asker, message string, queueID int64) ([]attention.Ask, error) {
	as, err := w.Store.AnswerInSession(ctx, asker, message, queueID)
	for _, a := range as {
		w.settled(a, err)
	}
	return as, err
}

func (w askLoggingAttention) ExpireDue(ctx context.Context) ([]attention.Ask, error) {
	as, err := w.Store.ExpireDue(ctx)
	for _, a := range as {
		w.settled(a, err)
	}
	return as, err
}

func (w askLoggingAttention) settled(a attention.Ask, err error) {
	if err != nil || a.ID <= 0 || a.Status == attention.Open || !askLogAttentionKind(a) {
		return
	}
	w.s.askLogAttentionSettled(a)
}

// askLogAttentionOpened queues the capture of attention ask a.
func (s *Server) askLogAttentionOpened(a attention.Ask) {
	s.askLogDo(a.Owner, asklog.KindAttention, func(ctx context.Context) error {
		row := asklog.Ask{Kind: asklog.KindAttention, Runtime: asklog.RuntimeAgent, SourceRef: strconv.FormatInt(a.ID, 10),
			Question: a.Question, AskedAt: a.CreatedAt}
		if len(a.Options) > 0 {
			raw, err := json.Marshal(a.Options)
			if err != nil {
				return err
			}
			row.Options = raw
		}
		switch {
		case a.Asker.AgentID > 0:
			row.AgentID = a.Asker.AgentID
			if ag, err := s.ext.Agents.Get(ctx, a.Asker.AgentID); err == nil {
				row.Repo = ag.Repo
			}
		case a.Asker.MirrorSession != "":
			row.Runtime = asklog.RuntimeClaudeCode
			if strings.HasPrefix(a.Asker.MirrorSession, mirror.RuntimeOpenCode+":") {
				row.Runtime = asklog.RuntimeOpenCode
			}
			row.Host, row.ClaudeSession = a.Asker.MirrorHost, a.Asker.MirrorSession
			row.TmuxSession, row.Window = s.askLogWindowOf(ctx, a.Asker.MirrorHost, a.Asker.MirrorSession)
			row.Repo = s.askLogRepo(ctx, a.Asker.MirrorSession)
			row.EpisodeID = s.askLogEpisode(ctx, sessionstate.KindMirror, a.Asker.MirrorSession)
		}
		_, err := s.askLogOpen(ctx, row)
		return err
	})
}

// askLogAttentionSettled queues the end of attention ask a: answered by the
// owner (or, for a question to the chief, by the chief), answered in the
// session, cancelled or expired.
func (s *Server) askLogAttentionSettled(a attention.Ask) {
	r := asklog.Resolution{At: a.ResolvedAt}
	switch a.Status {
	case attention.Answered:
		r.Outcome, r.Answer, r.AnsweredBy = asklog.OutcomeAnswered, a.Answer, asklog.ByOwner
		if a.AnsweredBy == attention.ByChief {
			r.AnsweredBy = asklog.ByChief
		}
	case attention.AnsweredInSession:
		r.Outcome, r.Answer, r.AnsweredBy = asklog.OutcomeAnsweredInSession, a.Answer, asklog.ByOwner
	case attention.Cancelled:
		r.Outcome = asklog.OutcomeCancelled
	case attention.Expired:
		r.Outcome = asklog.OutcomeExpired
	default:
		return
	}
	s.askLogDo(a.Owner, asklog.KindAttention, func(ctx context.Context) error {
		return s.askLogResolveRef(ctx, []string{asklog.KindAttention}, strconv.FormatInt(a.ID, 10), r)
	})
}

// ---------------------------------------------------------------------------
// Retention.
// ---------------------------------------------------------------------------

// RunAskLogSweep runs the ask log's retention sweep every interval until ctx
// ends (once at start): question and answer text older than the text TTL is
// cleared, rows older than asklog.RowTTL deleted.
func (s *Server) RunAskLogSweep(ctx context.Context, interval time.Duration) {
	if !s.askLogOn() {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		s.askLogPurge(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// askLogPurge is one retention sweep.
func (s *Server) askLogPurge(ctx context.Context) {
	purged, deleted, err := s.askLog.cfg.Store.Purge(ctx, s.now(), s.askLog.cfg.TextTTL, asklog.RowTTL)
	if err != nil {
		s.logger.Printf("ask log: retention sweep: %v", err)
		return
	}
	if purged > 0 || deleted > 0 {
		s.logger.Printf("ask log: retention sweep cleared the text of %d ask(s) and deleted %d", purged, deleted)
	}
}
