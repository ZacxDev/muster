package opencodehost

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SENDING TO A SESSION (docs/design/opencode-first-class.md): the API send,
// and the launcher's session create. Shapes are upstream's at 6ec5682, and
// MEASURED against a real `opencode serve` (v2.0.32+6ec5682) in a scratch
// home: opencodetest/testdata/prompt-v2.0.32.{json,sse}.
//
//   - POST /api/session/{id}/prompt {id, text, delivery} answers 200
//     {data: {id, sessionID, payload: {text}, delivery}} on ADMISSION (the
//     turn has not run yet). The SAME id again on the same session answers
//     200 with the record stored FIRST — measured: a second post carrying
//     different text came back with the first text, and the session holds one
//     user message — so a retry of a send under its id lands at most once.
//     The same id on ANOTHER session answers 409; an unknown session 404.
//   - delivery "queue": a busy session holds it until its turn ends. muster
//     never steers a message into a running turn.
//   - POST /api/session {id, location: {directory}} creates a session with the
//     caller's id, which the TUI then opens (`opencode -s <id>`).

// Errors of a send. Each is the cause of what the host replies, so muster
// says which happened.
var (
	// ErrSessionNotFound: the server has no such session (404).
	ErrSessionNotFound = errors.New("opencode has no such session (404)")
	// ErrPromptConflict: the message id names another session's message (409).
	ErrPromptConflict = errors.New("opencode refused the message id: it names a message of another session (409)")
	// ErrPromptRefused: the server refused the message (400).
	ErrPromptRefused = errors.New("opencode refused the message (400)")
	// ErrNotAdmitted: a 200 that does not name the message sent.
	ErrNotAdmitted = errors.New("opencode answered without admitting the message")
)

// Admitted is a prompt the server admitted (Session.Inbox.User).
type Admitted struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	Delivery  string `json:"delivery"`
	Payload   struct {
		Text string `json:"text"`
	} `json:"payload"`
}

// Prompt posts text to session under messageID, delivery "queue", and returns
// the admitted record: the one stored first, when messageID was posted before.
// 🔴 ONE OF THE PACKAGE'S WRITES (ledger_test.go): only the host's gated send
// calls it.
func (c *Client) Prompt(ctx context.Context, session, messageID, text string) (Admitted, error) {
	if !ValidSessionID(session) || !ValidMessageID(messageID) {
		return Admitted{}, fmt.Errorf("%w: not an opencode session and message id", ErrPromptRefused)
	}
	var out struct {
		Data Admitted `json:"data"`
	}
	body := map[string]any{"id": messageID, "text": text, "delivery": "queue"}
	if err := c.prompt(ctx, "/api/session/"+url.PathEscape(session)+"/prompt", body, &out); err != nil {
		return Admitted{}, err
	}
	if out.Data.ID != messageID || out.Data.SessionID != session {
		return Admitted{}, fmt.Errorf("%w (id %q, session %q)", ErrNotAdmitted, out.Data.ID, out.Data.SessionID)
	}
	return out.Data, nil
}

// prompt is the prompt POST.
func (c *Client) prompt(ctx context.Context, path string, body, out any) error {
	req, err := c.request(ctx, http.MethodPost, path, nil, false)
	if err != nil {
		return err
	}
	return c.postJSON(req, body, out)
}

// ModelRef is opencode's Model.Ref (packages/schema/src/model.ts at
// 6ec5682): a model by its provider and its id within that provider.
type ModelRef struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
}

// CreateSession creates a root session with id in directory (an absolute
// path), on model when it is not nil (the session's first turn runs on it:
// measured on v2.0.32+6ec5682, an OpenRouter model's assistant messages name
// it). 🔴 ONE OF THE PACKAGE'S WRITES (ledger_test.go). Its callers are the
// launcher (`muster-mirror opencode`, no model) and the host spawn
// (internal/mirrorhost, with one), and no other: a caller ledger pins the two.
// It runs nothing. 🔴 opencode does NOT check the model (measured: an unknown
// one is created all the same), so a caller checks it against Models first.
func (c *Client) CreateSession(ctx context.Context, id, directory string, model *ModelRef) (Session, error) {
	if !ValidSessionID(id) || !strings.HasPrefix(directory, "/") {
		return Session{}, fmt.Errorf("%w: not an opencode session id and an absolute directory", ErrPromptRefused)
	}
	if model != nil && (model.ID == "" || model.ProviderID == "") {
		return Session{}, fmt.Errorf("%w: a model names its provider and its id", ErrPromptRefused)
	}
	var out struct {
		Data Session `json:"data"`
	}
	body := map[string]any{"id": id, "location": map[string]string{"directory": directory}}
	if model != nil {
		body["model"] = *model
	}
	if err := c.createSession(ctx, body, &out); err != nil {
		return Session{}, err
	}
	if out.Data.ID != id {
		return Session{}, fmt.Errorf("opencode created session %q, not %q", out.Data.ID, id)
	}
	return out.Data, nil
}

func (c *Client) createSession(ctx context.Context, body, out any) error {
	req, err := c.request(ctx, http.MethodPost, "/api/session", nil, false)
	if err != nil {
		return err
	}
	return c.postJSON(req, body, out)
}

// RemoveSession deletes session id (DELETE /api/session/{id}, session.remove):
// measured, 204, then 404 for the same id again, which is reported as
// ErrSessionNotFound. 🔴 ONE OF THE PACKAGE'S WRITES (ledger_test.go): only
// the host spawn calls it, for the session IT created when the spawn fails
// before its first prompt — nothing has run in it.
func (c *Client) RemoveSession(ctx context.Context, id string) error {
	if !ValidSessionID(id) {
		return fmt.Errorf("%q is not an opencode session id", id)
	}
	return c.removeSession(ctx, "/api/session/"+url.PathEscape(id))
}

func (c *Client) removeSession(ctx context.Context, path string) error {
	req, err := c.request(ctx, http.MethodDelete, path, nil, false)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	c.observed(req, resp, err)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusNotFound:
		return ErrSessionNotFound
	}
	return fmt.Errorf("opencode DELETE %s: HTTP %d", req.URL.Path, resp.StatusCode)
}

// Model is one model the server lists (GET /api/model, Model.Info), as much
// of it as muster reads.
type Model struct {
	ID         string `json:"id"`
	ModelID    string `json:"modelID,omitempty"`
	ProviderID string `json:"providerID"`
	Status     string `json:"status,omitempty"`
	Enabled    *bool  `json:"enabled,omitempty"`
	// Limit is the model's token limits (Model.Info.limit): Context, its
	// context window, is the readout's denominator (usage.go).
	Limit struct {
		Context int64 `json:"context"`
	} `json:"limit"`
}

// Models is GET /api/model: every model the server can run now. ⚠ Upstream
// calls the answer a snapshot that "may precede initial plugin settlement";
// measured on v2.0.32+6ec5682 (three restarts), the first answer after a
// restart already listed every OpenRouter model, and a server with no
// OpenRouter credential lists none at all.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	var out struct {
		Data []Model `json:"data"`
	}
	if err := c.get(ctx, "/api/model", nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// postJSON sends body as req's JSON body and decodes a 200's {data} into out.
// It builds no request (ledger_test.go: only Client.request does).
func (c *Client) postJSON(req *http.Request, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req.Body, req.ContentLength = io.NopCloser(bytes.NewReader(b)), int64(len(b))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	c.observed(req, resp, err)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxJSON+1))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusNotFound:
		return ErrSessionNotFound
	case http.StatusConflict:
		return ErrPromptConflict
	case http.StatusBadRequest:
		return fmt.Errorf("%w: %s", ErrPromptRefused, strings.TrimSpace(string(msg[:min(len(msg), 512)])))
	default:
		return fmt.Errorf("opencode POST %s: HTTP %d", req.URL.Path, resp.StatusCode)
	}
	if len(msg) > maxJSON {
		return fmt.Errorf("opencode POST %s: the response is larger than %d bytes", req.URL.Path, maxJSON)
	}
	if err := json.Unmarshal(msg, out); err != nil {
		return fmt.Errorf("opencode POST %s: %w", req.URL.Path, err)
	}
	return nil
}

// GetSession is GET /api/session/{id}: ErrSessionNotFound when there is none.
func (c *Client) GetSession(ctx context.Context, id string) (Session, error) {
	if !ValidSessionID(id) {
		return Session{}, fmt.Errorf("%q is not an opencode session id", id)
	}
	var out struct {
		Data Session `json:"data"`
	}
	if err := c.get(ctx, "/api/session/"+url.PathEscape(id), nil, &out); err != nil {
		if se := (*StatusError)(nil); errors.As(err, &se) && se.Code == http.StatusNotFound {
			return Session{}, ErrSessionNotFound
		}
		return Session{}, err
	}
	return out.Data, nil
}

// Command is one slash command the server lists (GET /api/command).
type Command struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// Commands is the server's slash commands for directory. 🔴 The list depends
// on it (measured: without one the server answered none; with the session's,
// its built-ins).
func (c *Client) Commands(ctx context.Context, directory string) ([]Command, error) {
	var out struct {
		Data []Command `json:"data"`
	}
	var q url.Values
	if directory != "" {
		q = url.Values{"location[directory]": {directory}}
	}
	if err := c.get(ctx, "/api/command", q, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// ValidMessageID reports whether id is a message id muster posts: "msg_" and
// up to 64 more bytes of [0-9A-Za-z] (upstream's pattern is "^msg_").
func ValidMessageID(id string) bool {
	rest, ok := strings.CutPrefix(id, "msg_")
	if !ok || rest == "" || len(rest) > 64 {
		return false
	}
	for _, r := range rest {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

// idChars is upstream's identifier alphabet (packages/schema/src/identifier.ts).
const idChars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// NewMessageID is a fresh message id in upstream's format: "msg_", 12 hex
// digits of the (time-ascending) millisecond counter, 14 random characters.
func NewMessageID(now time.Time) string {
	return "msg_" + newID(uint64(now.UnixMilli())*0x1000+1)
}

// NewSessionID is a fresh session id in upstream's own format: "ses_", 12 hex
// digits of the bit-inverted (time-DESCENDING) millisecond counter, 14
// random characters — so a launcher-made session sorts among the TUI's own.
func NewSessionID(now time.Time) string {
	return "ses_" + newID(^(uint64(now.UnixMilli())*0x1000 + 1))
}

// newID is upstream's identifier body for counter v: its low 48 bits as 12
// hex digits, then 14 random characters of idChars.
func newID(v uint64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%012x", v&(1<<48-1))
	for range 14 {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(idChars))))
		if err != nil {
			panic(err)
		}
		b.WriteByte(idChars[n.Int64()])
	}
	return b.String()
}
