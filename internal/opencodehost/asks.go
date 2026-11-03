package opencodehost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// WHAT AN OPENCODE SESSION ASKS THE PERSON (upstream at 6ec5682;
// docs/design/opencode.md, "Asks"): two kinds, each announced on the event
// stream and listed per session, each answered by one POST.
//
//   - A PERMISSION REQUEST (packages/schema/src/permission.ts): a tool call
//     the session's ruleset says to ask about. `permission.asked` carries the
//     Permission.Request ({id "per…", sessionID, action, resources, save?,
//     metadata?, source?, message?}); `permission.replied` {sessionID,
//     requestID, reply} settles it. Listed by GET
//     /api/session/{id}/permission (pending only); answered by POST
//     /api/session/{id}/permission/{requestID}/reply {"decision": once |
//     always | reject} → 204 (404 once it is settled).
//   - A FORM (packages/schema/src/form.ts) — what the QUESTION TOOL raises
//     (packages/core/src/tool/plugin/question.ts: metadata.kind "question",
//     one field per question, key "q<i>", title = header, description = the
//     question, type "string" or "multiselect", options {value, label}, custom
//     true). `form.created` carries {form: Form.Info} — 🔴 its sessionID is
//     NESTED under data.form, not at data.sessionID like every other event's;
//     `form.replied` {id, sessionID, answer} and `form.cancelled` {id,
//     sessionID} settle it. Listed by GET /api/session/{id}/form (pending
//     only); answered by POST /api/session/{id}/form/{formID}/reply
//     {"answer": {"q0": "<label>"}} → 204 (409 once settled, 400 for an answer
//     its field cannot take).
//
// Measured against a real `opencode serve` (v2.0.32+6ec5682) in a scratch
// home: the event shapes above are the frames it sent (testdata in
// opencodetest), and the replies' statuses are what it answered.

// AskKind is what an Ask asks for.
type AskKind string

const (
	// AskPermission is a permission request.
	AskPermission AskKind = "permission"
	// AskQuestion is a form the question tool raised.
	AskQuestion AskKind = "question"
	// AskForm is any other form (an MCP elicitation, a plugin's): shown, never
	// answered from muster.
	AskForm AskKind = "form"
)

// PermissionReply is a permission request's answer as upstream spells it.
type PermissionReply string

const (
	PermissionOnce   PermissionReply = "once"
	PermissionAlways PermissionReply = "always"
	PermissionReject PermissionReply = "reject"
)

// Field is one question of a form, as much of it as muster reads.
type Field struct {
	Key         string `json:"key"`
	Type        string `json:"type"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Options     []struct {
		Value string `json:"value"`
		Label string `json:"label"`
	} `json:"options,omitempty"`
	Custom bool `json:"custom,omitempty"`
}

// Ask is one pending permission request or form of a session.
type Ask struct {
	Kind AskKind
	// ID is the request's ("per…") or form's ("frm_…") id; Session the session
	// that asks (a subagent's child session, possibly).
	ID, Session string
	// A permission request's action ("bash", "edit", …) and resources (the
	// command, the paths).
	Action    string
	Resources []string
	// A form's title and fields.
	Title  string
	Fields []Field
}

// permissionWire is Permission.Request as the wire carries it.
type permissionWire struct {
	ID        string   `json:"id"`
	SessionID string   `json:"sessionID"`
	Action    string   `json:"action"`
	Resources []string `json:"resources"`
}

func (p permissionWire) ask() (Ask, bool) {
	if !ValidRequestID(p.ID) || !ValidSessionID(p.SessionID) {
		return Ask{}, false
	}
	return Ask{Kind: AskPermission, ID: p.ID, Session: p.SessionID, Action: p.Action, Resources: p.Resources}, true
}

// formWire is Form.Info as the wire carries it.
type formWire struct {
	ID        string  `json:"id"`
	SessionID string  `json:"sessionID"`
	Title     string  `json:"title"`
	Fields    []Field `json:"fields"`
	Metadata  struct {
		Kind string `json:"kind"`
	} `json:"metadata"`
}

func (f formWire) ask() (Ask, bool) {
	// A form owned by "global" (an MCP elicitation with no session) is no
	// session's: no window can show it.
	if !ValidFormID(f.ID) || !ValidSessionID(f.SessionID) {
		return Ask{}, false
	}
	k := AskForm
	if f.Metadata.Kind == "question" {
		k = AskQuestion
	}
	return Ask{Kind: k, ID: f.ID, Session: f.SessionID, Title: f.Title, Fields: f.Fields}, true
}

// AskEvent is what an event says about an ask: one raised (Raised), or one
// settled (Settled names it, by its id) — answered, rejected or cancelled,
// in the terminal or anywhere else. ok false: the event is about no ask.
func (e Event) AskEvent() (raised *Ask, settled string, session string, ok bool) {
	switch e.Type {
	case "permission.asked":
		var p permissionWire
		if json.Unmarshal(e.Data, &p) != nil {
			return nil, "", "", false
		}
		a, ok := p.ask()
		if !ok {
			return nil, "", "", false
		}
		return &a, "", a.Session, true
	case "form.created":
		var d struct {
			Form formWire `json:"form"`
		}
		if json.Unmarshal(e.Data, &d) != nil {
			return nil, "", "", false
		}
		a, ok := d.Form.ask()
		if !ok {
			return nil, "", "", false
		}
		return &a, "", a.Session, true
	case "permission.replied":
		var d struct {
			SessionID string `json:"sessionID"`
			RequestID string `json:"requestID"`
		}
		if json.Unmarshal(e.Data, &d) != nil || !ValidRequestID(d.RequestID) {
			return nil, "", "", false
		}
		return nil, d.RequestID, d.SessionID, true
	case "form.replied", "form.cancelled":
		var d struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionID"`
		}
		if json.Unmarshal(e.Data, &d) != nil || !ValidFormID(d.ID) {
			return nil, "", "", false
		}
		return nil, d.ID, d.SessionID, true
	}
	return nil, "", "", false
}

// ParentOf is the parent a session.created event names for its session: ""
// for a root session, or any other event.
func (e Event) ParentOf() (session, parent string) {
	if e.Type != "session.created" {
		return "", ""
	}
	var d struct {
		SessionID string `json:"sessionID"`
		ParentID  string `json:"parentID"`
	}
	_ = json.Unmarshal(e.Data, &d)
	if !ValidSessionID(d.SessionID) || (d.ParentID != "" && !ValidSessionID(d.ParentID)) {
		return "", ""
	}
	return d.SessionID, d.ParentID
}

// idOK reports whether id is prefix and up to 125 more bytes of
// [0-9A-Za-z_-]: an opencode id this client puts in a path.
func idOK(id, prefix string) bool {
	if !strings.HasPrefix(id, prefix) || len(id) > 128 || len(id) == len(prefix) {
		return false
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// ValidRequestID reports whether id is a permission request id ("per…",
// Permission.ID's pattern "^per").
func ValidRequestID(id string) bool { return idOK(id, "per") }

// ValidFormID reports whether id is a form id ("frm_…", Form.ID's pattern).
func ValidFormID(id string) bool { return idOK(id, "frm_") }

// PendingAsks is session id's pending permission requests and forms (GET
// /api/session/{id}/permission and /form), requests first.
func (c *Client) PendingAsks(ctx context.Context, id string) ([]Ask, error) {
	if !ValidSessionID(id) {
		return nil, fmt.Errorf("%q is not an opencode session id", id)
	}
	var perms struct {
		Data []permissionWire `json:"data"`
	}
	if err := c.get(ctx, "/api/session/"+url.PathEscape(id)+"/permission", nil, &perms); err != nil {
		return nil, err
	}
	var forms struct {
		Data []formWire `json:"data"`
	}
	if err := c.get(ctx, "/api/session/"+url.PathEscape(id)+"/form", nil, &forms); err != nil {
		return nil, err
	}
	var out []Ask
	for _, p := range perms.Data {
		if a, ok := p.ask(); ok {
			out = append(out, a)
		}
	}
	for _, f := range forms.Data {
		if a, ok := f.ask(); ok {
			out = append(out, a)
		}
	}
	return out, nil
}

// ErrAskSettled: the server says the ask is no longer pending (404 for a
// permission request, 404 or 409 for a form): it was answered, rejected or
// cancelled elsewhere. Nothing was answered.
var ErrAskSettled = errors.New("opencode: the ask is no longer pending")

// ErrAnswerRefused: the server refused the answer itself (400): the field
// cannot take it. Nothing was answered.
var ErrAnswerRefused = errors.New("opencode refused the answer")

// ReplyPermission answers permission request requestID of session with
// decision: once or reject. "always" widens the session's rules past this
// one call, so muster never sends it — refused here, before a byte is sent.
func (c *Client) ReplyPermission(ctx context.Context, session, requestID string, decision PermissionReply) error {
	if decision != PermissionOnce && decision != PermissionReject {
		return fmt.Errorf("%w: muster answers a permission request with once or reject only", ErrAnswerRefused)
	}
	if !ValidSessionID(session) || !ValidRequestID(requestID) {
		return fmt.Errorf("%w: not an opencode session and request id", ErrAnswerRefused)
	}
	return c.reply(ctx, "/api/session/"+url.PathEscape(session)+"/permission/"+url.PathEscape(requestID)+"/reply",
		map[string]any{"decision": string(decision)})
}

// ReplyForm answers form formID of session with answer: each field's key to
// its value (a label, or labels for a multiselect).
func (c *Client) ReplyForm(ctx context.Context, session, formID string, answer map[string]any) error {
	if !ValidSessionID(session) || !ValidFormID(formID) || len(answer) == 0 {
		return fmt.Errorf("%w: not an opencode session, form id and answer", ErrAnswerRefused)
	}
	return c.reply(ctx, "/api/session/"+url.PathEscape(session)+"/form/"+url.PathEscape(formID)+"/reply",
		map[string]any{"answer": answer})
}

// reply POSTs body as JSON to path and expects 204. 🔴 THE ONLY WRITE TO A
// SESSION THIS PACKAGE MAKES, and only ReplyPermission and ReplyForm call it
// (ledger_test.go pins both).
func (c *Client) reply(ctx context.Context, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := c.request(ctx, http.MethodPost, path, nil, false)
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
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusNotFound, http.StatusConflict:
		return fmt.Errorf("%w (HTTP %d)", ErrAskSettled, resp.StatusCode)
	case http.StatusBadRequest:
		return fmt.Errorf("%w (HTTP 400: %s)", ErrAnswerRefused, strings.TrimSpace(string(msg)))
	}
	return fmt.Errorf("opencode POST %s: HTTP %d", req.URL.Path, resp.StatusCode)
}
