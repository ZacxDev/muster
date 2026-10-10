package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// terminal is the input side: how a prompt reaches the interactive session.
// The production implementation is tmux (tmux.go); tests substitute a fake.
type terminal interface {
	// Paste delivers text as ONE bracketed paste and submits it.
	Paste(ctx context.Context, text string) error
	// Enter presses Enter once more (a submit the TUI may have missed).
	Enter(ctx context.Context) error
	// Alive reports whether the session exists.
	Alive(ctx context.Context) error
}

// hookEvent is the subset of a Claude Code hook payload ccd reads. Field names are
// the CLI's (see testdata/hook_*.json, recorded from the pinned CLI).
type hookEvent struct {
	Event          string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	PromptID       string `json:"prompt_id"`
	Prompt         string `json:"prompt"`
	Source         string `json:"source"`
	// StopFailure only.
	Error                string `json:"error"`
	LastAssistantMessage string `json:"last_assistant_message"`
}

type pendingTurn struct {
	promptID  string
	submitted chan hookEvent // UserPromptSubmit
	done      chan hookEvent // Stop or StopFailure
}

type serverConfig struct {
	Bearer        string
	ConfigDir     string        // CLAUDE_CONFIG_DIR: transcripts must live under <it>/projects
	SubmitTimeout time.Duration // paste → UserPromptSubmit
	TurnTimeout   time.Duration // UserPromptSubmit → Stop/StopFailure
	// TranscriptGrace is how long to wait after Stop for the turn_duration record,
	// which the CLI writes only after its Stop hooks have run.
	TranscriptGrace time.Duration
}

type server struct {
	cfg  serverConfig
	term terminal
	auth *authTracker

	slot chan struct{} // capacity 1: one ccd-driven turn at a time

	mu             sync.Mutex
	sessionStarted bool
	sessionID      string
	busy           bool // a prompt was submitted (by anyone) and has not stopped
	pending        *pendingTurn
}

func newServer(cfg serverConfig, term terminal, auth *authTracker) *server {
	if cfg.SubmitTimeout == 0 {
		cfg.SubmitTimeout = 20 * time.Second
	}
	if cfg.TurnTimeout == 0 {
		cfg.TurnTimeout = 30 * time.Minute
	}
	if cfg.TranscriptGrace == 0 {
		cfg.TranscriptGrace = 3 * time.Second
	}
	return &server{cfg: cfg, term: term, auth: auth, slot: make(chan struct{}, 1)}
}

// gatewayHandler is the NETWORK-facing mux. It deliberately does not serve /hook:
// a hook is a claim about what happened inside the session, and only a process in
// the pod may make it (see hookHandler).
func (s *server) gatewayHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/responses", s.handleResponses)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	return mux
}

// hookHandler is served ONLY on a loopback listener (see main.go). The hooks the
// CLI runs are `ccd hook <Event>` commands that POST their stdin here.
//
// ⚠ IT IS UNAUTHENTICATED, AND THAT IS A STATEMENT ABOUT WHO CAN REACH IT, NOT AN
// OVERSIGHT: anything that can connect to 127.0.0.1 in the pod can already type
// into the tmux session, which is strictly more than a forged Stop can do.
func (s *server) hookHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hook/{event}", s.handleHook)
	return mux
}

func (s *server) handleHook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read", http.StatusBadRequest)
		return
	}
	var ev hookEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "payload is not JSON", http.StatusBadRequest)
		return
	}
	if ev.Event == "" {
		ev.Event = r.PathValue("event")
	}
	s.onHook(ev)
	w.WriteHeader(http.StatusNoContent)
}

// onHook updates session state from one hook. It never blocks: a hook command that
// waited on ccd would stall the TUI that ran it.
func (s *server) onHook(ev hookEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch strings.ToLower(ev.Event) {
	case "sessionstart":
		// SessionStart only fires once the CLI is past its first-run screens AND the
		// workspace trust dialog (hooks do not run in an untrusted workspace), so it
		// is the readiness signal that does not depend on reading the screen.
		s.sessionStarted, s.sessionID, s.busy = true, ev.SessionID, false
	case "userpromptsubmit":
		s.busy = true
		if p := s.pending; p != nil && p.promptID == "" {
			p.promptID = ev.PromptID
			select {
			case p.submitted <- ev:
			default:
			}
		}
	case "stop", "stopfailure":
		s.busy = false
		if p := s.pending; p != nil && p.promptID != "" && (ev.PromptID == "" || ev.PromptID == p.promptID) {
			select {
			case p.done <- ev:
			default:
			}
		}
		if strings.EqualFold(ev.Event, "stopfailure") && ev.Error != "" {
			s.auth.observeTurn(classifyAPIError(apiError{Code: ev.Error, Message: ev.LastAssistantMessage}))
		}
	}
}

// --- /v1/responses ---------------------------------------------------------

// The request is the OpenAI-Responses shape muster's client sends
// (internal/agents.responsesRequest). `model`, `instructions` and `tools` are
// accepted and IGNORED: the session's model comes from the CLI's own settings,
// its instructions from CLAUDE.md, and Claude Code cannot take function tools —
// a toolless answer is what muster's chat and kickoff paths ask for anyway.
type responsesRequest struct {
	Model  string          `json:"model"`
	Input  json.RawMessage `json:"input"`
	Stream *bool           `json:"stream"`
}

type inputItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// promptFrom extracts the LAST user message's text from `input`, which may be a
// bare string or an array of items whose content is a string or an array of
// {type: input_text|text, text} parts.
func promptFrom(raw json.RawMessage) (string, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var items []inputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return "", errors.New("`input` is neither a string nor an array of items")
	}
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		if it.Role != "user" || (it.Type != "" && it.Type != "message") {
			continue
		}
		if json.Unmarshal(it.Content, &s) == nil {
			return s, nil
		}
		var parts []contentBlock
		if json.Unmarshal(it.Content, &parts) != nil {
			return "", errors.New("user message content is neither a string nor an array of parts")
		}
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "input_text" || p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String(), nil
	}
	return "", errors.New("`input` carries no user message")
}

// neutralizeInputMode keeps a chat message a message.
//
// 🔴 A PROMPT WHOSE FIRST BYTE IS `!` OR `/` IS NOT SENT TO THE MODEL AT ALL —
// EVEN AS A BRACKETED PASTE. Measured on the pinned CLI: a pasted "! pasted bang
// first" ran `pasted bang first` in bash inside the pod (the TUI's shell mode,
// which no permission prompt and no approval hook sees), and a pasted "/ …" ran
// a local slash command. Neither fires UserPromptSubmit, so ccd would also have
// waited out its submit timeout after the command had already run. `#`, `&`,
// `@`, `>` and `?` were measured too and are plain text.
//
// One leading space disarms both (measured: " ! …" and " /help …" reached the
// model as text, the space kept verbatim in the transcript). That one byte is the
// only change ccd makes to a prompt besides folding CRLF.
func neutralizeInputMode(prompt string) string {
	if strings.HasPrefix(prompt, "!") || strings.HasPrefix(prompt, "/") {
		return " " + prompt
	}
	return prompt
}

func (s *server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if !bearerOK(r, s.cfg.Bearer) {
		writeFailure(w, &failure{Status: http.StatusUnauthorized, Type: failUnauthorized,
			Message: "missing or wrong bearer"})
		return
	}
	var req responsesRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeFailure(w, &failure{Status: http.StatusBadRequest, Type: failBadRequest, Message: "body: " + err.Error()})
		return
	}
	prompt, err := promptFrom(req.Input)
	if err != nil {
		writeFailure(w, &failure{Status: http.StatusBadRequest, Type: failBadRequest, Message: err.Error()})
		return
	}
	// CRLF → LF before tmux sees it: tmux turns every LF into CR on paste and the
	// TUI turns CR back into a newline, so a CRLF would arrive as TWO newlines.
	prompt = strings.ReplaceAll(prompt, "\r\n", "\n")
	if strings.TrimSpace(prompt) == "" {
		writeFailure(w, &failure{Status: http.StatusBadRequest, Type: failBadRequest, Message: "empty prompt"})
		return
	}
	prompt = neutralizeInputMode(prompt)

	res, f := s.runTurn(r.Context(), prompt)
	if f != nil {
		log.Printf("ccd: turn failed: %s (code=%q upstream=%d)", f.Error(), f.Code, f.Upstream)
		writeFailure(w, f)
		return
	}
	stream := req.Stream == nil || *req.Stream
	writeResponse(w, res.promptID, res.Segments, stream)
}

type turnOutcome struct {
	turnResult
	promptID string
}

// runTurn drives one prompt through the session and returns its reply or a
// failure. It holds the single turn slot for its whole duration.
func (s *server) runTurn(ctx context.Context, prompt string) (turnOutcome, *failure) {
	select {
	case s.slot <- struct{}{}:
		defer func() { <-s.slot }()
	default:
		return turnOutcome{}, &failure{Status: http.StatusConflict, Type: failBusy,
			Message: "another turn is already being driven through this session"}
	}

	p := &pendingTurn{submitted: make(chan hookEvent, 1), done: make(chan hookEvent, 1)}
	s.mu.Lock()
	switch {
	case !s.sessionStarted:
		s.mu.Unlock()
		return turnOutcome{}, &failure{Status: http.StatusServiceUnavailable, Type: failNotReady,
			Message: "the session has not started (no SessionStart hook yet): the TUI may be on a " +
				"first-run or trust screen — attach to the pane to see it"}
	case s.busy:
		s.mu.Unlock()
		return turnOutcome{}, &failure{Status: http.StatusConflict, Type: failBusy,
			Message: "the session is mid-turn (a prompt was submitted in the terminal)"}
	}
	s.pending = p
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.pending = nil
		s.mu.Unlock()
	}()

	if err := s.term.Paste(ctx, prompt); err != nil {
		return turnOutcome{}, &failure{Status: http.StatusBadGateway, Type: failTerminal, Message: err.Error()}
	}

	// Wait for the CLI to accept the prompt. One extra Enter half-way through
	// covers an Enter that landed while the TUI was still ingesting the paste; an
	// extra Enter on an empty input line is a no-op in the TUI.
	var submitted hookEvent
	submitTimer := time.NewTimer(s.cfg.SubmitTimeout)
	defer submitTimer.Stop()
	retry := time.NewTimer(s.cfg.SubmitTimeout / 2)
	defer retry.Stop()
waitSubmit:
	for {
		select {
		case submitted = <-p.submitted:
			break waitSubmit
		case <-retry.C:
			_ = s.term.Enter(ctx)
		case <-submitTimer.C:
			return turnOutcome{}, &failure{Status: http.StatusGatewayTimeout, Type: failNotSubmitted,
				Message: fmt.Sprintf("no UserPromptSubmit within %s of the paste: the prompt is sitting "+
					"unsent in the input box or a dialog has focus — attach to the pane", s.cfg.SubmitTimeout)}
		case <-ctx.Done():
			return turnOutcome{}, &failure{Status: http.StatusGatewayTimeout, Type: failTurnTimeout,
				Message: "caller went away before the prompt was submitted"}
		}
	}

	var stop hookEvent
	turnTimer := time.NewTimer(s.cfg.TurnTimeout)
	defer turnTimer.Stop()
	select {
	case stop = <-p.done:
	case <-turnTimer.C:
		return turnOutcome{}, &failure{Status: http.StatusGatewayTimeout, Type: failTurnTimeout,
			Message: fmt.Sprintf("no Stop/StopFailure within %s; the turn is still running in the session", s.cfg.TurnTimeout)}
	case <-ctx.Done():
		// ⚠ The turn KEEPS RUNNING in the TUI: ccd does not interrupt work the
		// operator may be watching. The session stays busy until its Stop arrives.
		return turnOutcome{}, &failure{Status: http.StatusGatewayTimeout, Type: failTurnTimeout,
			Message: "caller went away mid-turn; the turn is still running in the session"}
	}

	path := stop.TranscriptPath
	if path == "" {
		path = submitted.TranscriptPath
	}
	res, err := s.readTurn(path, submitted.PromptID)
	if err != nil {
		return turnOutcome{}, &failure{Status: http.StatusBadGateway, Type: failTranscript, Message: err.Error()}
	}
	out := turnOutcome{turnResult: res, promptID: submitted.PromptID}

	// Failure precedence: the transcript's own error record, then the hook's.
	// Either one makes the turn a failure EVEN IF some text was also produced
	// before it — a partial answer delivered as a success hides the failure.
	if res.Err != nil {
		f := classifyAPIError(*res.Err)
		if f.Code == "" && stop.Error != "" {
			f.Code = stop.Error
		}
		s.auth.observeTurn(f)
		return turnOutcome{}, f
	}
	if strings.EqualFold(stop.Event, "StopFailure") {
		f := classifyAPIError(apiError{Code: stop.Error, Message: stop.LastAssistantMessage})
		s.auth.observeTurn(f)
		return turnOutcome{}, f
	}
	if strings.TrimSpace(res.Text()) == "" {
		return turnOutcome{}, &failure{Status: http.StatusBadGateway, Type: failEmptyReply,
			Message: fmt.Sprintf("the turn ended (Stop) with no assistant text (%d tool calls, transcript "+
				"complete=%v)", res.ToolUses, res.Complete)}
	}
	s.auth.observeTurn(nil)
	return out, nil
}

// readTurn reads the turn for promptID, re-reading briefly until the CLI has
// written the turn_duration record that follows its Stop hooks.
func (s *server) readTurn(path, promptID string) (turnResult, error) {
	if path == "" || promptID == "" {
		return turnResult{}, errors.New("the hooks carried no transcript path or prompt id")
	}
	if err := s.underProjects(path); err != nil {
		return turnResult{}, err
	}
	deadline := time.Now().Add(s.cfg.TranscriptGrace)
	for {
		res, err := readTurnFile(path, promptID)
		if err == nil && res.Found && res.Complete {
			return res, nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return res, err
			}
			if !res.Found {
				return res, fmt.Errorf("prompt %s is not in %s", promptID, path)
			}
			return res, nil // no turn_duration yet: the records before it are complete
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func readTurnFile(path, promptID string) (turnResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return turnResult{}, err
	}
	defer f.Close()
	return extractTurn(f, promptID)
}

// underProjects refuses a transcript path outside <CLAUDE_CONFIG_DIR>/projects.
func (s *server) underProjects(path string) error {
	root := filepath.Join(s.cfg.ConfigDir, "projects")
	rel, err := filepath.Rel(root, filepath.Clean(path))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return fmt.Errorf("transcript path %q is not under %s", path, root)
	}
	return nil
}

// --- /healthz ---------------------------------------------------------------

type health struct {
	OK             bool   `json:"ok"`
	SessionStarted bool   `json:"session_started"`
	Busy           bool   `json:"busy"`
	Terminal       string `json:"terminal"`
	authSnapshot
}

// handleHealthz answers 200 only when the session is at (or past) its prompt,
// tmux is alive, AND the model credential is known to work. Each check is one
// the others cannot see: a running tmux says nothing about the screen it shows,
// and `claude auth status` reports logged-in for a token the API rejects.
func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	h := health{Terminal: "ok", authSnapshot: s.auth.snapshot()}
	if err := s.term.Alive(ctx); err != nil {
		h.Terminal = err.Error()
	}
	s.mu.Lock()
	h.SessionStarted, h.Busy = s.sessionStarted, s.busy
	s.mu.Unlock()
	h.OK = h.SessionStarted && h.Terminal == "ok" && h.Auth == authOK
	w.Header().Set("Content-Type", "application/json")
	if !h.OK {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(h)
}
