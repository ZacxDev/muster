package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/mirrorpath"
)

// ---------------------------------------------------------------------------
// `muster officer item enqueue` RUN INSIDE A MIRRORED CLAUDE CODE SESSION
// RECORDS THAT SESSION AS THE ITEM'S REQUESTER (migration 0178,
// officer_items.requested_host / requested_session), so the owner's decision
// on a draft the officer files for it reaches the session that asked
// (internal/api session_wake_requests.go).
//
// 🔴 THE SESSION IS THE ONE MUSTER-MIRROR BINDS, NEVER ONE THIS COMMAND NAMES.
// The CLI's own token (a personal or hook token) names no session, and there
// is deliberately no flag or header that does. Instead the command makes the
// call the session's own MCP client makes: officer_item_enqueue on this
// machine's muster-mirror MCP endpoint (loopback only, behind this host's MCP
// secret), naming its pane and tmux server ($TMUX_PANE, $TMUX) exactly as
// Claude Code's MCP config does. muster-mirror maps that pane to the session
// it lists running there NOW and muster binds the call to it
// (handleMirrorMCP, mirrorSessionOf): the same identity source and session
// pin as every session wake. The most a caller can do is act as one of this
// host's own sessions — the reach its MCP client already has.
//
// When the session cannot be resolved — not inside a Claude Code session or
// a tmux pane, no muster-mirror MCP endpoint here, this machine mirrored to a
// different muster than the one this command talks to, the secret unreadable,
// the endpoint unreachable, or the tool refusing the call — the command
// enqueues through the admin API as it always has, records NO session, and
// says why on stderr. A refusal there is not a new path around anything: it
// is the request the same token could always make.
//
// On a machine the step-up gate covers, the enqueue is a confirmation card on
// the owner's phone (the tool's own gate, as from the session's MCP client):
// the command prints the card's words and queues nothing itself.
// ---------------------------------------------------------------------------

const (
	officerItemEnqueueTool = "officer_item_enqueue"
	// envMirrorMCPSecret is the secret a session's shell exports for its MCP
	// client (doctor's registration fix); else the secret file is read.
	envMirrorMCPSecret = "MUSTER_MIRROR_MCP_SECRET"
	envMirrorURL       = "MUSTER_MIRROR_URL"
	// mirrorPaneHeader / mirrorTmuxHeader are how a caller names its pane and
	// tmux server to muster-mirror (internal/mirrorhost MCPPaneHeader,
	// MCPTmuxHeader; TestTheSessionDoorHeadersAreMirrorHosts pins the two).
	mirrorPaneHeader = "X-Muster-Pane"
	mirrorTmuxHeader = "X-Muster-Tmux"
	// sessionDoorMaxBody bounds the endpoint's answer read here.
	sessionDoorMaxBody = 1 << 20
)

// sessionDoor is this machine's muster-mirror MCP endpoint, as the calling
// session's own MCP client reaches it.
type sessionDoor struct {
	url, secret, pane, tmux string
}

// sessionDoorFor is the endpoint through which this process's session would
// enqueue on the muster at apiURL, or why there is none.
func (a *app) sessionDoorFor(apiURL string) (sessionDoor, string) {
	if sessionIDFrom(a.getenv) == "" {
		return sessionDoor{}, "this command is not running inside a Claude Code session"
	}
	pane, tmux := strings.TrimSpace(a.getenv("TMUX_PANE")), strings.TrimSpace(a.getenv("TMUX"))
	if pane == "" || tmux == "" {
		return sessionDoor{}, "it is not running inside a tmux pane, so muster-mirror cannot tell which session it is"
	}
	env := expandHome(defaultMirrorEnv, a.homeDir)
	vals, err := parseEnvFile(env)
	if err != nil || len(vals) == 0 {
		return sessionDoor{}, "this machine has no muster-mirror settings (" + defaultMirrorEnv + ")"
	}
	listen := strings.TrimSpace(vals[envMirrorMCPListen])
	if listen == "" {
		return sessionDoor{}, "muster-mirror serves no MCP endpoint on this machine (" + envMirrorMCPListen + " is not set in " + defaultMirrorEnv + ")"
	}
	if !loopbackListen(listen) {
		return sessionDoor{}, envMirrorMCPListen + "=" + listen + " is not a loopback address"
	}
	if m := strings.TrimSpace(vals[envMirrorURL]); !sameServer(m, apiURL) {
		return sessionDoor{}, "this machine's sessions are mirrored to " + orNone(m) + ", and this command talks to " + apiURL
	}
	secret := strings.TrimSpace(a.getenv(envMirrorMCPSecret))
	if secret == "" {
		path := expandHome(strings.TrimSpace(vals[envMirrorMCPSecretFile]), a.homeDir)
		if path == "" {
			path = mirrorpath.DefaultMCPSecretFile()
		}
		raw, err := os.ReadFile(path)
		if err != nil || strings.TrimSpace(string(raw)) == "" {
			return sessionDoor{}, "muster-mirror's MCP secret could not be read (" + path + ")"
		}
		secret = strings.TrimSpace(string(raw))
	}
	return sessionDoor{url: "http://" + listen + "/mcp", secret: secret, pane: pane, tmux: tmux}, ""
}

func orNone(s string) string {
	if s == "" {
		return "no muster (" + envMirrorURL + " is not set)"
	}
	return s
}

// loopbackListen reports whether listen (host:port) is a loopback address:
// the secret is never sent anywhere else.
func loopbackListen(listen string) bool {
	h, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// sameServer reports whether URLs a and b name one muster: the same scheme,
// host and port (the scheme's default when absent), and path.
func sameServer(a, b string) bool {
	norm := func(raw string) (string, bool) {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Scheme == "" || u.Host == "" {
			return "", false
		}
		port := u.Port()
		if port == "" {
			port = map[string]string{"http": "80", "https": "443"}[strings.ToLower(u.Scheme)]
		}
		return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Hostname()) + ":" + port + strings.TrimRight(u.Path, "/"), true
	}
	na, okA := norm(a)
	nb, okB := norm(b)
	return okA && okB && na == nb
}

// errSessionDoor is a call the endpoint did not answer as a tool result.
var errSessionDoor = errors.New("no tool result")

// callSessionDoor is one tools/call of tool with args on door: the tool's
// text and whether it is a tool error, or err when there was no tool result.
func (a *app) callSessionDoor(ctx context.Context, d sessionDoor, tool string, args map[string]any) (string, bool, error) {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	if err != nil {
		return "", false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(body))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+d.secret)
	req.Header.Set(mirrorPaneHeader, d.pane)
	req.Header.Set(mirrorTmuxHeader, d.tmux)
	resp, err := a.newHTTP(a.flagTimeout).Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, sessionDoorMaxBody))
	if err != nil {
		return "", false, err
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("%w: HTTP %d", errSessionDoor, resp.StatusCode)
	}
	if strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		raw = lastEventData(raw)
	}
	var out struct {
		Result *struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", false, fmt.Errorf("%w: %v", errSessionDoor, err)
	}
	if out.Error != nil {
		return "", false, fmt.Errorf("%w: %s", errSessionDoor, out.Error.Message)
	}
	if out.Result == nil || len(out.Result.Content) == 0 {
		return "", false, errSessionDoor
	}
	return out.Result.Content[0].Text, out.Result.IsError, nil
}

// lastEventData is the last JSON-RPC message an event stream carried.
func lastEventData(raw []byte) []byte {
	var last []byte
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), sessionDoorMaxBody)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "data:"); ok && strings.TrimSpace(v) != "" {
			last = []byte(strings.TrimSpace(v))
		}
	}
	return last
}

// enqueueAsSession is `officer item enqueue`'s request through the calling
// session's own MCP door (see the header). done false: there is no session to
// record, the reason already on stderr, and the caller enqueues through the
// admin API.
func (a *app) enqueueAsSession(ctx context.Context, officer string, body map[string]any) (bool, error) {
	c, err := a.api()
	if err != nil {
		return false, err
	}
	d, why := a.sessionDoorFor(c.cfg.apiURL)
	if why == "" {
		args := map[string]any{"officer": officer}
		for k, v := range body {
			if s, ok := v.(string); ok && s == "" {
				continue
			}
			if t, ok := v.(time.Time); ok {
				v = t.UTC().Format(time.RFC3339)
			}
			args[k] = v
		}
		text, isErr, err := a.callSessionDoor(ctx, d, officerItemEnqueueTool, args)
		switch {
		case err != nil:
			why = "this machine's muster-mirror MCP endpoint did not answer (" + err.Error() + ")"
		case isErr:
			why = officerItemEnqueueTool + " refused it from this session: " + text
		default:
			return true, a.sessionEnqueued(text)
		}
	}
	fmt.Fprintf(a.stderr, "note: no session recorded on this item — %s; enqueued with this command's token instead, so no session hears how the owner decides its drafts\n", why)
	return false, nil
}

// stepUpAwaiting is a step-up card's status in the gate's answer
// (internal/api hostStepUpGate).
const stepUpAwaiting = "awaiting_owner_confirmation"

// sessionEnqueued prints the tool's answer: the item ({id, created,
// requested_by_this_session}), or the step-up card the machine's gate filed
// instead.
func (a *app) sessionEnqueued(text string) error {
	var got struct {
		ID     int64  `json:"id"`
		Mine   *bool  `json:"requested_by_this_session"`
		Status string `json:"status"`
		Card   int64  `json:"card"`
	}
	if json.Unmarshal([]byte(text), &got) != nil {
		out, _ := json.Marshal(map[string]any{"answer": text})
		return a.emit(out)
	}
	switch {
	case got.Status == stepUpAwaiting:
		fmt.Fprintf(a.stderr, "note: this machine's step-up gate made the enqueue card #%d on your owner's phone; once it is confirmed, the item is queued as this session's\n", got.Card)
	case got.Mine != nil && !*got.Mine:
		fmt.Fprintf(a.stderr, "note: item %d was already live with another session recorded on it; this session is not told how its drafts are decided\n", got.ID)
	}
	return a.emit([]byte(text))
}
