package clankmailplugin

// musterclient.go — muster's plugin door (docs/plugins.md, "The plugin
// author's contract") and the one non-plugin route this plugin may call,
// POST /api/plugin-threads, with a separate credential.

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

// MusterClient is the plugin door with an mpl_ token.
type MusterClient struct {
	Base  string // MUSTER_URL, no trailing slash
	Token string // the plugin token
	HTTP  *http.Client
	UA    string
}

// MusterError is a non-2xx answer from muster: the status only (muster's error
// prose can quote what we sent).
type MusterError struct{ Status int }

func (e *MusterError) Error() string { return fmt.Sprintf("muster HTTP %d", e.Status) }

func (m *MusterClient) do(ctx context.Context, method, path, token string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		// <, > and & as themselves: muster reads a bounded request, and a body
		// of 16 KiB must fit it however it is spelled.
		enc.SetEscapeHTML(false)
		if err := enc.Encode(body); err != nil {
			return 0, nil, err
		}
		rd = &buf
	}
	req, err := http.NewRequestWithContext(ctx, method, m.Base+path, rd)
	if err != nil {
		return 0, nil, errors.New("bad muster request")
	}
	req.GetBody = nil
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", m.UA)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return 0, nil, errTransport
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, errTransport
	}
	return resp.StatusCode, b, nil
}

// musterMessage is the POST /plugin/v1/messages body.
type musterMessage struct {
	ExternalID string `json:"external_id"`
	Sender     string `json:"sender"`
	Subject    string `json:"subject,omitempty"`
	Body       string `json:"body"`
	ReceivedAt string `json:"received_at"`
	// SenderVerified is ALWAYS sent, never omitted: true only for a message
	// whose signature verified and whose signer is a pinned peer whose chain
	// verified (relay.go). muster cannot check it and stores it immutably.
	SenderVerified bool `json:"sender_verified"`
	// Thread is the DERIVED root (an m_ id), omitted when it could not be
	// derived. Never the hub's unsigned `thread`.
	Thread string `json:"thread,omitempty"`
}

func (m *MusterClient) postMessage(ctx context.Context, msg musterMessage) error {
	st, _, err := m.do(ctx, http.MethodPost, "/plugin/v1/messages", m.Token, msg)
	if err != nil {
		return err
	}
	if st >= 200 && st < 300 {
		return nil
	}
	return &MusterError{st}
}

// permanentRefusal: muster judged the CONTENT invalid; the same bytes can never
// be accepted. 401/403/404/429/5xx are not: a revoked token, the door switched
// off or a full queue must hold the cursor, never drop mail.
func permanentRefusal(err error) bool {
	var me *MusterError
	if !errors.As(err, &me) {
		return false
	}
	return me.Status == http.StatusBadRequest || me.Status == http.StatusRequestEntityTooLarge ||
		me.Status == http.StatusUnprocessableEntity
}

// draft is a claimed approved reply. draft_id may be a number or a string.
type draft struct {
	DraftID           json.RawMessage `json:"draft_id"`
	MessageExternalID string          `json:"message_external_id"`
	Body              string          `json:"body"`
}

func (d draft) id() string {
	s := strings.TrimSpace(string(d.DraftID))
	var str string
	if json.Unmarshal(d.DraftID, &str) == nil {
		return str
	}
	return s
}

// claim long-polls for one approved reply; nil on 204.
func (m *MusterClient) claim(ctx context.Context, wait int) (*draft, error) {
	st, b, err := m.do(ctx, http.MethodPost, "/plugin/v1/drafts/claim", m.Token, map[string]int{"wait_seconds": wait})
	if err != nil {
		return nil, err
	}
	if st == http.StatusNoContent {
		return nil, nil
	}
	if st != http.StatusOK {
		return nil, &MusterError{st}
	}
	var d draft
	if json.Unmarshal(b, &d) != nil || d.id() == "" || d.id() == "null" {
		return nil, errors.New("claim: malformed muster answer")
	}
	return &d, nil
}

type draftResult struct {
	OK          bool   `json:"ok"`
	ExternalRef string `json:"external_ref,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

// result reports a draft's outcome. final=true on 2xx and on 409 (muster holds
// another outcome, or the lease lapsed: retrying cannot change it).
func (m *MusterClient) result(ctx context.Context, id string, r draftResult) (final bool, err error) {
	st, _, err := m.do(ctx, http.MethodPost, "/plugin/v1/drafts/"+url.PathEscape(id)+"/result", m.Token, r)
	if err != nil {
		return false, err
	}
	if st >= 200 && st < 300 {
		return true, nil
	}
	if st == http.StatusConflict {
		return true, &MusterError{st}
	}
	return false, &MusterError{st}
}

// bindThread is POST /api/plugin-threads {thread, claude_session} with a
// PERSONAL or hook token (not the plugin token: muster refuses mpl_ off
// /plugin/v1/*). created=false on 200 (already bound: the first bind wins).
func (m *MusterClient) bindThread(ctx context.Context, apiToken, thread, session string) (created bool, err error) {
	st, _, err := m.do(ctx, http.MethodPost, "/api/plugin-threads", apiToken,
		map[string]string{"thread": thread, "claude_session": session})
	if err != nil {
		return false, err
	}
	switch st {
	case http.StatusCreated:
		return true, nil
	case http.StatusOK:
		return false, nil
	}
	return false, &MusterError{st}
}
